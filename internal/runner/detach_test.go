package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A run must outlive the context that started it.
//
// This is the failure that made `claudeflow once` useless: the command builds a
// signal context, spawns a run, and cancels that context on return — killing
// the agent microseconds after starting it, with nothing written to the log.
func TestStartWithoutCancelSurvivesItsParentContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	p, err := Start(context.WithoutCancel(ctx), Spawn{
		Command: "sh",
		Args:    []string{"-c", "sleep 3; echo survived"},
		Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel() // the supervisor exits here

	res := p.Wait()
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0 — the run was killed with its parent context", res.ExitCode)
	}
	if res.Stdout == "" {
		t.Error("no output; the run did not survive long enough to produce any")
	}
}

// The plain form still honours cancellation, which is what Stop and the
// timeout rely on.
func TestStartWithLiveContextIsStillCancellable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, err := Start(ctx, Spawn{
		Command: "sh",
		Args:    []string{"-c", "sleep 30"},
		Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()

	done := make(chan Result, 1)
	go func() { done <- p.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("cancelling the context did not stop the run")
	}
}

// A detached run's output must reach its log file even after the process that
// started it has gone.
//
// os/exec only passes a descriptor straight through for an *os.File; given any
// other writer it copies in a goroutine owned by the starting process. When
// that process exits, the copier dies and the transcript is empty — a run that
// plainly worked, with nothing to show for it.
func TestDetachedRunStillWritesItsLog(t *testing.T) {
	log := filepath.Join(t.TempDir(), "run.log")
	ctx, cancel := context.WithCancel(context.Background())

	p, err := Start(context.WithoutCancel(ctx), Spawn{
		Command: "sh",
		Args:    []string{"-c", "sleep 1; echo to-stdout; echo to-stderr >&2"},
		Timeout: time.Minute,
		LogPath: log,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel() // the supervisor goes away before the child writes anything

	p.Wait()

	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	body := string(raw)
	if body == "" {
		t.Fatal("log is empty; the child's output was copied by a process that had exited")
	}
	for _, want := range []string{"to-stdout", "to-stderr"} {
		if !strings.Contains(body, want) {
			t.Errorf("log = %q, want it to contain %q", body, want)
		}
	}
}
