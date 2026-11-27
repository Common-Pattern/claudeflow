package sandbox

import (
	"slices"
	"strings"
	"testing"
)

func testPodman() Podman {
	return Podman{
		Image:  "localhost/img",
		Prefix: "claudeflow-o-n",
		Repo:   "o/n",
		Pass:   []string{"CF_TEST_PASSED", "CF_TEST_UNSET"},
		Mounts: []string{"/srv/cache:/cache:ro"},
		Self:   "/usr/local/bin/claudeflow",
		Mirror: "/repo/.git",
	}
}

// A secret reaches the container by name only: its value must never be on the
// command line, where any user of the host can read it from the process table.
func TestRunArgsPassSecretsByNameOnly(t *testing.T) {
	t.Setenv("CF_TEST_PASSED", "visible-only-in-env")
	args := testPodman().RunArgs(Start{
		RunID: "build-7", Slot: 2, Spec: "/s/build-7.json", Log: "/l/build-7.log",
		Inbox: "/i/build-7", Secrets: map[string]string{"GH_TOKEN": "ghp_secret"},
		ClaudeDir: "/s/build-7.claude", ClaudePath: "/home/op/.claude",
		Binaries: []string{"/home/op/.local/share/claude/versions/9"},
	})
	line := strings.Join(args, " ")
	if strings.Contains(line, "ghp_secret") || strings.Contains(line, "visible-only-in-env") {
		t.Fatalf("a secret value is on the command line: %s", line)
	}
	for _, want := range []string{"--env GH_TOKEN", "--env CF_TEST_PASSED", "--volume /s/build-7.claude:/home/op/.claude",
		"--volume /home/op/.local/share/claude/versions/9:/home/op/.local/share/claude/versions/9:ro"} {
		if !strings.Contains(line, want) {
			t.Errorf("args lack %q: %s", want, line)
		}
	}
	if strings.Contains(line, "CF_TEST_UNSET") {
		t.Errorf("an unset variable was passed: %s", line)
	}
}

// Everything the host reads back — the log and the inbox — is mounted at the
// path it has on the host, so the code reading it there needs no translation.
func TestRunArgsMountsAndLabels(t *testing.T) {
	args := testPodman().RunArgs(Start{
		RunID: "build-7", Slot: 2, Spec: "/s/build-7.json", Log: "/l/build-7.log", Inbox: "/i/build-7",
	})
	line := strings.Join(args, " ")
	for _, want := range []string{
		"--name claudeflow-o-n-build-7",
		"--label claudeflow.repo=o/n",
		"--label claudeflow.slot=2",
		"--userns=keep-id",
		"--pids-limit=-1",
		"--volume /l/build-7.log:/l/build-7.log",
		"--volume /i/build-7:/i/build-7",
		"--volume /s/build-7.json:/s/build-7.json:ro",
		"--volume /usr/local/bin/claudeflow:/usr/local/bin/claudeflow:ro",
		"--volume /repo/.git:" + MirrorPath + ":ro",
		"--volume claudeflow-o-n-slot2:" + storagePath,
		"--volume /srv/cache:/cache:ro",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("args lack %q:\n%s", want, line)
		}
	}
	tail := args[len(args)-4:]
	if !slices.Equal(tail, []string{"localhost/img", "/usr/local/bin/claudeflow", "sandbox-run", "/s/build-7.json"}) {
		t.Errorf("command = %v, want the image running claudeflow sandbox-run on the spec", tail)
	}
}

// A run that holds no slot runs no stack, so it gets no image storage to keep.
func TestRunArgsNoStorageWithoutASlot(t *testing.T) {
	args := testPodman().RunArgs(Start{RunID: "plan-7", Spec: "/s/p.json", Log: "/l/p.log"})
	if strings.Contains(strings.Join(args, " "), storagePath) {
		t.Errorf("a slotless run got image storage: %v", args)
	}
}
