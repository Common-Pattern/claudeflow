package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests start real containers on the host's podman. They need the
// sandbox image built (podman build -t localhost/claudeflow-sandbox sandbox/)
// and run only when CLAUDEFLOW_SANDBOX_IMAGE names it:
//
//	CLAUDEFLOW_SANDBOX_IMAGE=localhost/claudeflow-sandbox go test ./internal/sandbox -run Integration -v

func integrationImage(t *testing.T) string {
	t.Helper()
	image := os.Getenv("CLAUDEFLOW_SANDBOX_IMAGE")
	if image == "" {
		t.Skip("CLAUDEFLOW_SANDBOX_IMAGE not set")
	}
	if err := exec.Command("podman", "image", "exists", image).Run(); err != nil {
		t.Skipf("image %s not present: %v", image, err)
	}
	return image
}

// BuildSelf compiles claudeflow statically, as releases are, for mounting into
// a sandbox. The test binary cannot stand in: it has no sandbox-run command.
func BuildSelf(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(dir, "claudeflow")
	cmd := exec.Command("go", "build", "-o", out, "github.com/Common-Pattern/claudeflow/cmd/claudeflow")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build claudeflow: %v\n%s", err, b)
	}
	return out
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// OriginWithStack makes a bare repository whose main branch carries a Compose
// file serving nginx on port 80, published on an ephemeral port.
func OriginWithStack(t *testing.T, dir string) string {
	t.Helper()
	origin := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	run(t, dir, "git", "init", "--quiet", "--bare", "--initial-branch=main", origin)
	run(t, dir, "git", "init", "--quiet", "--initial-branch=main", seed)
	compose := "services:\n  web:\n    image: docker.io/library/nginx:alpine\n    ports:\n      - \"80\"\n" +
		"    healthcheck:\n      test: [\"CMD\", \"wget\", \"-q\", \"-O\", \"/dev/null\", \"http://127.0.0.1/\"]\n" +
		"      interval: 2s\n      retries: 30\n"
	if err := os.WriteFile(filepath.Join(seed, "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, seed, "git", "add", ".")
	run(t, seed, "git", "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "seed")
	run(t, seed, "git", "push", "--quiet", origin, "main")
	return origin
}

// The whole container path, for real: a fresh container clones the branch,
// runs the install hook, brings a Compose stack up on the podman nested inside
// it, and starts an agent that can reach the stack, commit, push, and read a
// comment delivered to its inbox while it runs. When the agent exits, the
// container stops; removing it frees the slot.
func TestIntegrationSandboxRunsAStackAndAnAgent(t *testing.T) {
	image := integrationImage(t)
	dir := t.TempDir()
	self := BuildSelf(t, dir)
	origin := OriginWithStack(t, dir)

	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	p := Podman{
		Image:  image,
		Prefix: "cftest-" + stamp,
		Repo:   "test/" + stamp,
		Self:   self,
		Mirror: origin,
		Mounts: []string{origin + ":" + origin + ":ro"},
	}
	t.Cleanup(func() {
		_ = exec.Command("podman", "rm", "--force", p.Name("build-1")).Run()
		_ = exec.Command("podman", "volume", "rm", "--force", p.Prefix+"-slot1").Run()
	})

	// The origin is mounted read-only, so the agent pushes to a second,
	// writable bare repository standing in for GitHub.
	pushTo := filepath.Join(dir, "pushed.git")
	run(t, dir, "git", "init", "--quiet", "--bare", pushTo)
	p.Mounts = append(p.Mounts, pushTo+":"+pushTo)

	agent := `set -e
curl -fsS "$WEB" >/dev/null && echo '{"type":"probe","web":"ok"}'
test -f installed.txt && echo '{"type":"probe","install":"ok"}'
test "$(git rev-parse --abbrev-ref HEAD)" = "claude/build-1" && echo '{"type":"probe","branch":"ok"}'
echo change > change.txt
git add change.txt
git commit --quiet -m "agent change"
git push --quiet "` + pushTo + `" HEAD:refs/heads/claude/build-1
echo '{"type":"probe","push":"ok"}'
for i in $(seq 1 120); do
  if ls "$CLAUDEFLOW_INBOX"/pending/* >/dev/null 2>&1; then echo '{"type":"probe","inbox":"ok"}'; exit 0; fi
  sleep 1
done
echo '{"type":"probe","inbox":"timeout"}'
exit 3
`
	logPath := filepath.Join(dir, "logs", "build-1.log")
	inbox := filepath.Join(dir, "inbox", "build-1")
	spec := Spec{
		RunID:        "build-1",
		Remote:       origin,
		Mirror:       MirrorPath,
		Workdir:      "/home/podman/work/repo",
		Branch:       "claude/build-1",
		StartPoint:   "main",
		AuthorName:   "Sandbox Test",
		AuthorEmail:  "sandbox@example.com",
		Stack:        true,
		Install:      "echo installed > installed.txt",
		ComposeFile:  "compose.yaml",
		Expose:       map[string]int{"web": 80},
		ReadyTimeout: 5 * time.Minute,
		AgentEnv:     map[string]string{"WEB": "${CLAUDEFLOW_URL_WEB}"},
		Command:      "sh",
		Args:         []string{"-c", agent},
		Env:          map[string]string{"CLAUDEFLOW_INBOX": inbox},
		Timeout:      10 * time.Minute,
		Log:          logPath,
	}
	specPath := filepath.Join(dir, "sandbox", "build-1.json")
	if err := spec.Write(specPath); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	name, err := p.Launch(ctx, Start{RunID: "build-1", Slot: 1, Spec: specPath, Log: logPath, Inbox: inbox})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if busy, err := p.SlotBusy(ctx, 1); err != nil || !busy {
		t.Errorf("SlotBusy(1) = %v, %v; want busy while the sandbox exists", busy, err)
	}
	if busy, _ := p.SlotBusy(ctx, 2); busy {
		t.Error("SlotBusy(2) = true; nothing holds slot 2")
	}

	// Deliver a comment once the agent has pushed, the way a tick would.
	waitFor(t, 10*time.Minute, "the agent to push", func() bool {
		raw, _ := os.ReadFile(logPath)
		return strings.Contains(string(raw), `"push":"ok"`) || !mustRunning(t, p, name)
	})
	if err := os.MkdirAll(filepath.Join(inbox, "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "pending", "1-issue.md"), []byte("a comment"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Minute, "the sandbox to finish", func() bool { return !mustRunning(t, p, name) })

	raw, _ := os.ReadFile(logPath)
	log := string(raw)
	for _, want := range []string{`"web":"ok"`, `"install":"ok"`, `"branch":"ok"`, `"push":"ok"`, `"inbox":"ok"`} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %s:\n%s", want, log)
		}
	}
	exit := strings.TrimSpace(run(t, dir, "podman", "container", "inspect", "--format", "{{.State.ExitCode}}", name))
	if exit != "0" {
		t.Errorf("sandbox exit code = %s, want 0\n%s", exit, log)
	}
	got := run(t, dir, "git", "--git-dir", pushTo, "log", "--format=%an <%ae> %s", "claude/build-1", "-1")
	if !strings.Contains(got, "Sandbox Test <sandbox@example.com> agent change") {
		t.Errorf("pushed commit = %q, want the agent's commit under the configured identity", got)
	}
	t.Logf("transcript:\n%s", log)

	if err := p.Remove(ctx, name); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if running, err := p.Running(ctx, name); err != nil || running {
		t.Errorf("Running after Remove = %v, %v; want false, nil", running, err)
	}
	if busy, _ := p.SlotBusy(ctx, 1); busy {
		t.Error("slot 1 still busy after the sandbox was removed")
	}
}

func mustRunning(t *testing.T, p Podman, name string) bool {
	t.Helper()
	running, err := p.Running(context.Background(), name)
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	return running
}

func waitFor(t *testing.T, limit time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Second)
	}
}
