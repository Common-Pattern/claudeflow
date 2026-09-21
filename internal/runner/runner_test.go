package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runCmd(t *testing.T, r *Runner, command string) Result {
	t.Helper()
	res, err := r.Exec(context.Background(), command)
	if err != nil {
		t.Fatalf("Exec(%q): %v", command, err)
	}
	return res
}

// groupGone reports whether every process in pgid's group has exited. The
// group, not the leader, is what a run has to leave clean.
func groupGone(pgid int) bool {
	return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestExecExitCodeZero(t *testing.T) {
	res := runCmd(t, &Runner{}, "exit 0")
	if res.ExitCode != 0 || res.TimedOut {
		t.Errorf("got %+v, want exit 0 and no timeout", res)
	}
}

func TestExecExitCodeNonZero(t *testing.T) {
	res := runCmd(t, &Runner{}, "exit 3")
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if res.TimedOut {
		t.Error("TimedOut = true for a command that exited on its own")
	}
}

func TestExecSeparatesStreams(t *testing.T) {
	res := runCmd(t, &Runner{}, `printf 'to stdout'; printf 'to stderr' >&2`)
	if res.Stdout != "to stdout" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "to stdout")
	}
	if res.Stderr != "to stderr" {
		t.Errorf("Stderr = %q, want %q", res.Stderr, "to stderr")
	}
}

func TestExecEnvReachesCommand(t *testing.T) {
	r := &Runner{Env: Env{"CLAUDEFLOW_SLOT": "4"}}
	if res := runCmd(t, r, `printf %s "$CLAUDEFLOW_SLOT"`); res.Stdout != "4" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "4")
	}
}

func TestExecRunsInDir(t *testing.T) {
	dir := t.TempDir()
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	res := runCmd(t, &Runner{Dir: dir}, "pwd -P")
	if got := strings.TrimSpace(res.Stdout); got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}
}

func TestExecTimeoutIsNotAnExitCode(t *testing.T) {
	r := &Runner{Timeout: 200 * time.Millisecond}
	start := time.Now()
	res := runCmd(t, r, "sleep 5")
	if !res.TimedOut {
		t.Fatalf("TimedOut = false for a command that outlived its timeout: %+v", res)
	}
	if res.ExitCode != ExitTimeout {
		t.Errorf("ExitCode = %d, want ExitTimeout (%d)", res.ExitCode, ExitTimeout)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Exec returned after %v, want shortly after the timeout", elapsed)
	}
}

func TestTimeoutLeavesNoProcess(t *testing.T) {
	p, err := Start(context.Background(), Spawn{
		Command: "sh", Args: []string{"-c", "sleep 5"}, Timeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := p.PID()
	if res := p.Wait(); !res.TimedOut {
		t.Fatalf("TimedOut = false, want true: %+v", res)
	}
	waitFor(t, "the timed-out group to be gone", func() bool { return groupGone(pid) })
}

func TestLogPathAppendsAndCreatesParent(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "logs", "build-192", "run.log")
	r := &Runner{LogPath: logPath}

	runCmd(t, r, `printf 'first\n'; printf 'first stderr\n' >&2`)
	runCmd(t, r, `printf 'second\n'`)

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for _, want := range []string{"first\n", "first stderr\n", "second\n"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("log %q missing %q — a retry must not erase its predecessor", raw, want)
		}
	}
}

func TestCaptureKeepsTail(t *testing.T) {
	original := MaxCapturedBytes
	MaxCapturedBytes = 4
	t.Cleanup(func() { MaxCapturedBytes = original })

	if res := runCmd(t, &Runner{}, `printf 'abcdefghij'`); res.Stdout != "ghij" {
		t.Errorf("Stdout = %q, want the last 4 bytes %q", res.Stdout, "ghij")
	}
}

func TestStartReportsUsablePIDAndAlive(t *testing.T) {
	p, err := Start(context.Background(), Spawn{Command: "sh", Args: []string{"-c", "sleep 0.3"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p.PID() <= 0 {
		t.Fatalf("PID = %d, want a real pid", p.PID())
	}
	if !Alive(p.PID()) {
		t.Errorf("Alive(%d) = false while the process is running", p.PID())
	}
	pid := p.PID()
	if res := p.Wait(); res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if Alive(pid) {
		t.Errorf("Alive(%d) = true after the process exited and was reaped", pid)
	}
}

func TestStopKillsTheWholeGroup(t *testing.T) {
	dir := t.TempDir()
	childPID := filepath.Join(dir, "child.pid")
	p, err := Start(context.Background(), Spawn{
		Command: "sh",
		Args:    []string{"-c", "sleep 60 & echo $! > " + childPID + "; sleep 60"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var child int
	waitFor(t, "the spawned child to report its pid", func() bool {
		raw, err := os.ReadFile(childPID)
		if err != nil {
			return false
		}
		child, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && child > 0 && Alive(child)
	})

	start := time.Now()
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res := p.Wait(); res.TimedOut {
		t.Error("TimedOut = true for a process that was stopped, not timed out")
	}
	// A surviving child keeps the inherited stdout pipe open, so Wait blocks on
	// it for as long as that child runs. Stop returning promptly is the same
	// evidence as the group being gone, read from the other side.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Stop took %v, want it to return once the group is dead", elapsed)
	}

	waitFor(t, "the group to be gone", func() bool { return groupGone(p.PID()) })
	if Alive(child) {
		t.Fatalf("child %d survived Stop — an orphan holds the run's slot forever", child)
	}
}

func TestStopReportsSignalExitCode(t *testing.T) {
	p, err := Start(context.Background(), Spawn{Command: "sh", Args: []string{"-c", "sleep 60"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res := p.Wait(); res.ExitCode != 128+int(syscall.SIGTERM) {
		t.Errorf("ExitCode = %d, want %d", res.ExitCode, 128+int(syscall.SIGTERM))
	}
}

func TestStopIsTermBeforeKill(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "term.seen")
	p, err := Start(context.Background(), Spawn{
		Command: "sh",
		Args:    []string{"-c", "trap 'printf caught > " + marker + "; exit 0' TERM; sleep 60 & wait"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The trap is installed after the shell starts, so signalling immediately
	// would test nothing.
	time.Sleep(200 * time.Millisecond)
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	p.Wait()
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("SIGTERM handler never ran (%v), so SIGKILL came first", err)
	}
}

func TestContextCancelStopsTheProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, err := Start(ctx, Spawn{Command: "sh", Args: []string{"-c", "sleep 60"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := p.PID()
	cancel()
	res := p.Wait()
	if res.TimedOut {
		t.Error("TimedOut = true for a cancelled process, want false")
	}
	if !errors.Is(p.Err(), context.Canceled) {
		t.Errorf("Err() = %v, want context.Canceled", p.Err())
	}
	waitFor(t, "the cancelled group to be gone", func() bool { return groupGone(pid) })
}

func TestExecReportsCancellationAsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := (&Runner{}).Exec(ctx, "sleep 60")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Exec error = %v, want context.Canceled", err)
	}
}

func TestStartRejectsMissingCommand(t *testing.T) {
	if _, err := Start(context.Background(), Spawn{}); err == nil {
		t.Error("Start with no command succeeded, want error")
	}
	if _, err := Start(context.Background(), Spawn{Command: "claudeflow-no-such-binary"}); err == nil {
		t.Error("Start with an unresolvable command succeeded, want error")
	}
}

func TestAlive(t *testing.T) {
	for _, pid := range []int{0, -1, -12345} {
		if Alive(pid) {
			t.Errorf("Alive(%d) = true, want false", pid)
		}
	}
	if !Alive(os.Getpid()) {
		t.Error("Alive(os.Getpid()) = false")
	}
}

func TestAliveSinceRejectsAMismatchedStart(t *testing.T) {
	p, err := Start(context.Background(), Spawn{Command: "sh", Args: []string{"-c", "sleep 2"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	if !AliveSince(p.PID(), time.Now()) {
		t.Error("AliveSince with the true start time = false, want true")
	}
	if AliveSince(p.PID(), time.Now().Add(-2*time.Hour)) {
		t.Error("AliveSince with a start time two hours off = true — a recycled pid would be adopted")
	}
}

func TestHitUsageLimit(t *testing.T) {
	cases := []struct {
		transcript string
		want       bool
	}{
		{"Claude usage limit reached. Your limit will reset at 3pm.", true},
		{"claude AI usage limit reached", true},
		{"Error: rate limit reached, try again later", true},
		{"API error: Rate limit exceeded", true},
		{"429 too many requests", true},
		{"rate_limit_error", true},
		{"You have exceeded your current quota", true},
		{"quota exceeded for this organisation", true},
		{"", false},
		{"Tests failed: 3 assertions did not hold", false},
		{"error: the rate limiter config is missing", false},
		{"exit status 1", false},
	}
	for _, c := range cases {
		if got := HitUsageLimit(c.transcript); got != c.want {
			t.Errorf("HitUsageLimit(%q) = %v, want %v", c.transcript, got, c.want)
		}
	}
}

func TestUsageLimitPatternsAreExtensible(t *testing.T) {
	original := UsageLimitPatterns
	t.Cleanup(func() { UsageLimitPatterns = original })

	phrase := "Come back tomorrow"
	if HitUsageLimit(phrase) {
		t.Fatalf("%q already matches, pick another phrase for this test", phrase)
	}
	UsageLimitPatterns = append(UsageLimitPatterns, regexp.MustCompile(`(?i)come back tomorrow`))
	if !HitUsageLimit(phrase) {
		t.Error("an appended pattern did not take effect")
	}
}
