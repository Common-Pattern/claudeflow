package tick

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/state"
)

// fakeSandbox records what the engine asks of it, and answers liveness from a
// set the test controls.
type fakeSandbox struct {
	mu        sync.Mutex
	launches  []Launch
	alive     map[string]bool // by sandbox name
	torn      []string        // run IDs torn down
	stopped   []string
	busySlots map[int]bool
	fail      error
}

func newFakeSandbox() *fakeSandbox {
	return &fakeSandbox{alive: map[string]bool{}, busySlots: map[int]bool{}}
}

func (f *fakeSandbox) Launch(_ context.Context, l Launch) (state.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches = append(f.launches, l)
	if f.fail != nil {
		return l.Run, f.fail
	}
	r := l.Run
	r.Sandbox = "sb-" + r.ID()
	r.Started = time.Now()
	f.alive[r.Sandbox] = true
	return r, nil
}

func (f *fakeSandbox) Alive(r state.Run) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive[r.Sandbox]
}

func (f *fakeSandbox) Stop(r state.Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, r.ID())
	return nil
}

func (f *fakeSandbox) Teardown(_ context.Context, r state.Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.torn = append(f.torn, r.ID())
	return nil
}

func (f *fakeSandbox) Busy(_ context.Context, slot int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busySlots[slot], nil
}

func (f *fakeSandbox) exit(r string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive["sb-"+r] = false
}

// A build is launched through the sandbox with its branch named and the
// agent's environment filled in, and the record keeps what the sandbox needs
// to find it again.
func TestStartLaunchesThroughTheSandbox(t *testing.T) {
	e, f := newEngine(t)
	sb := newFakeSandbox()
	e.Sandbox = sb
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued)

	if err := e.Start(t.Context(), Start{Kind: state.KindBuild, Ref: 7, Slot: 2}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(sb.launches) != 1 {
		t.Fatalf("launches = %d, want 1", len(sb.launches))
	}
	l := sb.launches[0]
	if l.Resume {
		t.Error("a fresh build launched as a resume")
	}
	if l.Run.Branch != "claude/build-7" || l.Run.Slot != 2 {
		t.Errorf("run = %+v, want branch claude/build-7 on slot 2", l.Run)
	}
	if l.Env["CLAUDEFLOW_BRANCH"] != "claude/build-7" || l.Env["CLAUDEFLOW_ISSUE"] != "7" {
		t.Errorf("env = %v, want the branch and issue set", l.Env)
	}
	if l.Command != e.Cfg.Agent.Command || argValue(l.Args, "-p") == "" {
		t.Errorf("command = %q %v, want the agent with a prompt", l.Command, l.Args)
	}

	runs, _ := e.Store.Runs()
	if len(runs) != 1 || runs[0].Sandbox != "sb-build-7" {
		t.Fatalf("runs = %+v, want one recording its sandbox", runs)
	}
	live, _ := e.liveRuns()
	if len(live) != 1 {
		t.Errorf("live = %v, want the sandboxed run counted live", live)
	}
}

// Reap asks the sandbox, not the process table, whether a run is over, and
// hands its environment back to the sandbox when it is.
func TestReapAsksTheSandbox(t *testing.T) {
	e, f := newEngine(t)
	sb := newFakeSandbox()
	e.Sandbox = sb
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued)
	if err := e.Start(t.Context(), Start{Kind: state.KindBuild, Ref: 7, Slot: 1}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := e.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if runs, _ := e.Store.Runs(); len(runs) != 1 || len(sb.torn) != 0 {
		t.Fatalf("a live sandbox was reaped: runs %v, torn %v", runs, sb.torn)
	}

	sb.exit("build-7")
	if err := e.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if !slices.Equal(sb.torn, []string{"build-7"}) {
		t.Errorf("torn = %v, want build-7 torn down", sb.torn)
	}
	if runs, _ := e.Store.Runs(); len(runs) != 0 {
		t.Errorf("runs = %v, want the finished run gone", runs)
	}
}

// A run that opened a pull request is parked: its environment is torn down and
// its record no longer points at a sandbox, so nothing reads it as live.
func TestParkingReleasesTheSandbox(t *testing.T) {
	e, f := newEngine(t)
	sb := newFakeSandbox()
	e.Sandbox = sb
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued)
	if err := e.Start(t.Context(), Start{Kind: state.KindBuild, Ref: 7, Slot: 1}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.PRByBranch["claude/build-7"] = 200
	sb.exit("build-7")

	if err := e.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	runs, _ := e.Store.Runs()
	if len(runs) != 1 || !runs[0].InPhase(state.PhaseAwaitingCI) {
		t.Fatalf("runs = %+v, want one parked on checks", runs)
	}
	if runs[0].Sandbox != "" || runs[0].Slot != 0 {
		t.Errorf("parked run still holds sandbox %q, slot %d", runs[0].Sandbox, runs[0].Slot)
	}
	if !slices.Equal(sb.torn, []string{"build-7"}) {
		t.Errorf("torn = %v, want the environment released on parking", sb.torn)
	}
}

// A fix goes back into the run's branch: the sandbox is told it is a resume,
// with the same branch and the failing checks in the environment.
func TestFixResumesThroughTheSandbox(t *testing.T) {
	e, _ := newEngine(t)
	sb := newFakeSandbox()
	e.Sandbox = sb
	r := state.Run{Kind: state.KindBuild, Ref: 7, Slot: 3, Branch: "claude/build-7", PR: 200, Attempts: 1}

	if err := e.startFix(t.Context(), r, "Unit Tests"); err != nil {
		t.Fatalf("startFix: %v", err)
	}
	if len(sb.launches) != 1 {
		t.Fatalf("launches = %d, want 1", len(sb.launches))
	}
	l := sb.launches[0]
	if !l.Resume || l.Run.Branch != "claude/build-7" {
		t.Errorf("launch = resume %v on %q, want a resume of claude/build-7", l.Resume, l.Run.Branch)
	}
	if l.Env["CLAUDEFLOW_FAILING_CHECKS"] != "Unit Tests" || l.Env["CLAUDEFLOW_PR"] != "200" {
		t.Errorf("env = %v, want the failing checks and the pull request", l.Env)
	}
	runs, _ := e.Store.Runs()
	if len(runs) != 1 || runs[0].Sandbox == "" || runs[0].Log == "" {
		t.Errorf("runs = %+v, want the resumed run recorded with its sandbox and log", runs)
	}
}

// Slot allocation asks the sandbox which slots are taken.
func TestAllocatorAsksTheSandbox(t *testing.T) {
	e, _ := newEngine(t)
	sb := newFakeSandbox()
	sb.busySlots[1] = true
	e.Sandbox = sb
	n, err := e.allocator().Free(t.Context(), nil)
	if err != nil {
		t.Fatalf("Free: %v", err)
	}
	if n != 2 {
		t.Errorf("Free = %d, want 2: slot 1 is busy in the sandbox", n)
	}
}
