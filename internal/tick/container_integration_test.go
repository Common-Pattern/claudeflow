package tick

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/git"
	"github.com/Common-Pattern/claudeflow/internal/hostlogin"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

// The engine driving real container sandboxes: a build that pushes, parks on
// its pull request and frees its container; a fix that goes back into the
// branch as pushed, in a fresh container that the first run's leftovers do
// not reach; and landing, which works from a checkout of its own.
//
// It needs the sandbox image and runs only when CLAUDEFLOW_SANDBOX_IMAGE names
// it; see internal/sandbox/integration_test.go.
func TestIntegrationEngineWithContainerSandbox(t *testing.T) {
	image := os.Getenv("CLAUDEFLOW_SANDBOX_IMAGE")
	if image == "" {
		t.Skip("CLAUDEFLOW_SANDBOX_IMAGE not set")
	}
	dir := t.TempDir()
	sh := func(d string, args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = d
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// A writable bare repository stands in for GitHub.
	origin := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	sh(dir, "git", "init", "--quiet", "--bare", "--initial-branch=main", origin)
	sh(dir, "git", "init", "--quiet", "--initial-branch=main", seed)
	// db has a healthcheck and web waits for it to pass: nested podman runs
	// no healthchecks of its own, so this needs the sandbox to run them, and
	// the condition needs Docker Compose, through the API the sandbox serves.
	compose := `services:
  db:
    image: docker.io/library/alpine
    command: ["sleep", "3600"]
    healthcheck:
      test: ["CMD-SHELL", "true"]
      interval: 2s
  web:
    image: docker.io/library/nginx:alpine
    depends_on:
      db:
        condition: service_healthy
    ports:
      - "80"
`
	if err := os.WriteFile(filepath.Join(seed, "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(seed, "git", "add", ".")
	sh(seed, "git", "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "seed")
	sh(seed, "git", "push", "--quiet", origin, "main")
	root := filepath.Join(dir, "root")
	sh(dir, "git", "clone", "--quiet", origin, root)
	sh(root, "git", "config", "user.name", "Operator")
	sh(root, "git", "config", "user.email", "operator@example.com")

	// The agent: a script that behaves like a build the first time and a fix
	// the second, and reports what it saw as JSON lines in its transcript.
	scripts := filepath.Join(dir, "scripts")
	agent := `#!/bin/sh
set -e
curl -fsS "$CLAUDEFLOW_URL_WEB" >/dev/null && echo '{"type":"probe","web":"ok"}'
podman compose version 2>/dev/null | grep -q "Docker Compose" && echo '{"type":"probe","compose":"docker"}'
test "$(podman inspect -f '{{.State.Health.Status}}' cf-db-1)" = healthy && echo '{"type":"probe","health":"ok"}'
if [ -z "$CLAUDEFLOW_FAILING_CHECKS" ]; then
  c="$CLAUDE_CONFIG_DIR/.credentials.json"
  grep -q sk-ant-oat01-first "$c" && grep -q sk-ant-ort01-host "$c" && echo '{"type":"probe","login":"shared"}'
  grep -q user-host "$CLAUDE_CONFIG_DIR/.claude.json" && echo '{"type":"probe","account":"shared"}'
  sed -i 's/sk-ant-oat01-first/sk-ant-oat01-refreshed-in-run/' "$c"
  mkdir -p "$CLAUDE_CONFIG_DIR/projects/-work" && echo '{"session":"sandboxed"}' > "$CLAUDE_CONFIG_DIR/projects/-work/s1.jsonl"
  podman run -d --name stray docker.io/library/nginx:alpine >/dev/null
  echo one > one.txt; git add one.txt; git commit --quiet -m "build commit"
  git push --quiet -u origin HEAD
  echo '{"type":"probe","build":"pushed"}'
else
  git log --format=%s | grep -q "build commit" && echo '{"type":"probe","resumed":"ok"}'
  test -z "$(podman ps -aq --filter name=stray)" && echo '{"type":"probe","clean":"ok"}'
  echo two > two.txt; git add two.txt; git commit --quiet -m "fix commit"
  git push --quiet origin HEAD
  echo '{"type":"probe","fix":"pushed"}'
fi
`
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "agent"), []byte(agent), 0o755); err != nil {
		t.Fatal(err)
	}

	// The operator's Claude Code directory, as the agent CLI keeps it on the
	// host.
	hostClaude := filepath.Join(dir, "host-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", hostClaude)
	if err := os.MkdirAll(hostClaude, 0o700); err != nil {
		t.Fatal(err)
	}
	login := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-first","refreshToken":"sk-ant-ort01-host","expiresAt":%d}}`,
		time.Now().Add(8*time.Hour).UnixMilli())
	if err := os.WriteFile(filepath.Join(hostClaude, hostlogin.File), []byte(login), 0o600); err != nil {
		t.Fatal(err)
	}
	account := `{"oauthAccount":{"accountUuid":"acc-host"},"userID":"user-host"}`
	if err := os.WriteFile(filepath.Join(hostClaude, hostlogin.AccountFile), []byte(account), 0o600); err != nil {
		t.Fatal(err)
	}

	e, f := newEngine(t)
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	e.Cfg.Repo = "test/" + stamp
	e.Cfg.Paths.Root = root
	e.Cfg.Branches.Integration = "main"
	e.Cfg.Paths.Worktrees = filepath.Join(dir, "worktrees")
	e.Cfg.Compose.File = "compose.yaml"
	e.Cfg.Compose.Expose = map[string]int{"web": 80}
	e.Cfg.Agent.Command = filepath.Join(scripts, "agent")
	e.Cfg.Sandbox = config.Sandbox{
		Kind: config.SandboxContainer, Bin: "podman", Image: image, Remote: origin,
		Mounts: []string{origin + ":" + origin, scripts + ":" + scripts + ":ro"},
	}
	e.Repo = git.New(root)
	sb, err := NewContainerSandbox(e)
	if err != nil {
		t.Fatal(err)
	}
	cs := sb.(containerSandbox)
	cs.p.Self = buildSelf(t, dir)
	e.Sandbox = cs
	t.Cleanup(func() {
		for _, id := range []string{"build-1"} {
			_ = exec.Command("podman", "rm", "--force", cs.p.Name(id)).Run()
		}
		_ = exec.Command("podman", "volume", "rm", "--force", cs.p.Prefix+"-slot1").Run()
	})

	ctx := t.Context()
	f.AddIssue(1, time.Now(), e.Cfg.Labels.Queued)
	if err := e.Start(ctx, Start{Kind: state.KindBuild, Ref: 1, Slot: 1}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	run := soleRun(t, e)
	if run.Sandbox == "" || run.PID != 0 {
		t.Fatalf("run = %+v, want one found by its sandbox", run)
	}
	waitUntil(t, 10*time.Minute, "the build to finish", func() bool { return !e.RunAlive(soleRun(t, e)) })
	assertProbes(t, run.Log, `"login":"shared"`, `"account":"shared"`, `"web":"ok"`, `"compose":"docker"`, `"health":"ok"`, `"build":"pushed"`)
	// What the run's CLI writes there is the host's: a refreshed login, and
	// the session's transcript in the host's history.
	if got, _ := os.ReadFile(filepath.Join(hostClaude, hostlogin.File)); !strings.Contains(string(got), "refreshed-in-run") {
		t.Errorf("the host's login after the run refreshed it = %s", got)
	}
	if got, err := os.ReadFile(filepath.Join(hostClaude, "projects", "-work", "s1.jsonl")); err != nil || !strings.Contains(string(got), "sandboxed") {
		t.Errorf("the run's transcript is not in the host's history: %q, %v", got, err)
	}

	// The agent opened its pull request: reaping parks the run and removes
	// the container.
	f.PRByBranch["claude/build-1"] = 10
	if err := e.Reap(ctx); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	parked := soleRun(t, e)
	if !parked.InPhase(state.PhaseAwaitingCI) || parked.Sandbox != "" {
		t.Fatalf("run = %+v, want it parked on checks with no sandbox", parked)
	}
	if exec.Command("podman", "container", "exists", run.Sandbox).Run() == nil {
		t.Errorf("container %s still exists after the run was parked", run.Sandbox)
	}

	// A fix: a new container, the branch as pushed, none of the first run's
	// nested containers.
	parked.Slot = 1
	parked.Attempts = 1
	if err := e.startFix(ctx, parked, "Unit Tests"); err != nil {
		t.Fatalf("startFix: %v", err)
	}
	fix := soleRun(t, e)
	waitUntil(t, 10*time.Minute, "the fix to finish", func() bool { return !e.RunAlive(soleRun(t, e)) })
	assertProbes(t, fix.Log, `"web":"ok"`, `"resumed":"ok"`, `"clean":"ok"`, `"fix":"pushed"`)
	if err := e.sandbox().Teardown(ctx, fix); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	// Landing works from a checkout the supervisor makes for itself, at the
	// branch as the sandboxes pushed it.
	wt, err := e.landingWorktree(ctx, fix)
	if err != nil {
		t.Fatalf("landingWorktree: %v", err)
	}
	if got, want := sh(wt, "git", "rev-parse", "HEAD"), sh(origin, "git", "rev-parse", "claude/build-1"); got != want {
		t.Errorf("landing checkout at %s, want origin's claude/build-1 at %s", got, want)
	}
	if author := sh(wt, "git", "log", "-1", "--format=%an"); author != "Operator" {
		t.Errorf("commit author = %q, want the operator's identity from the checkout", author)
	}
}

func soleRun(t *testing.T, e Engine) state.Run {
	t.Helper()
	runs, err := e.Store.Runs()
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, %v; want exactly one", runs, err)
	}
	return runs[0]
}

func assertProbes(t *testing.T, logPath string, wants ...string) {
	t.Helper()
	raw, _ := os.ReadFile(logPath)
	for _, w := range wants {
		if !strings.Contains(string(raw), w) {
			t.Errorf("transcript lacks %s:\n%s", w, raw)
		}
	}
}

func waitUntil(t *testing.T, limit time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Second)
	}
}

func buildSelf(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(dir, "claudeflow")
	cmd := exec.Command("go", "build", "-o", out, "github.com/Common-Pattern/claudeflow/cmd/claudeflow")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build claudeflow: %v\n%s", err, b)
	}
	return out
}
