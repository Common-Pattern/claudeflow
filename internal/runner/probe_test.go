package runner

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestProbeClaude spawns the agent CLI exactly as the engine does, so a
// failure here is the failure a real run sees. Opt-in: it costs a real call.
func TestProbeClaude(t *testing.T) {
	if os.Getenv("CLAUDEFLOW_PROBE") == "" {
		t.Skip("set CLAUDEFLOW_PROBE=1")
	}
	log := filepath.Join(t.TempDir(), "probe.log")
	p, err := Start(t.Context(), Spawn{
		Command: "claude",
		Args:    []string{"-p", "say OK and stop", "--permission-mode", "bypassPermissions", "--output-format", "text", "--model", "opus"},
		Dir:     os.Getenv("CLAUDEFLOW_PROBE_DIR"),
		Env:     Env{"CLAUDEFLOW_ISSUE": "247", "CLAUDEFLOW_BRANCH": "", "CLAUDEFLOW_WORKTREE": ""},
		Timeout: 2 * time.Minute,
		LogPath: log,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Logf("pid %d", p.PID())
	res := p.Wait()
	raw, _ := os.ReadFile(log)
	t.Logf("exit=%d timedOut=%v err=%v", res.ExitCode, res.TimedOut, p.Err())
	t.Logf("stdout=%q", res.Stdout)
	t.Logf("stderr=%q", res.Stderr)
	t.Logf("log(%d bytes)=%q", len(raw), string(raw))
	if res.ExitCode != 0 {
		t.Errorf("exit %d", res.ExitCode)
	}
}
