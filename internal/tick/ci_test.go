package tick

import (
	"slices"
	"testing"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

func green() []forge.Check {
	return []forge.Check{{Name: "Unit Tests", Done: true, Passed: true}}
}

func red() []forge.Check {
	return []forge.Check{
		{Name: "Unit Tests", Done: true, Passed: false},
		{Name: "Lint", Done: true, Passed: true},
	}
}

// An agent that opened a pull request has done its part. The run is parked on
// that pull request rather than resolved, and the slot is freed — nothing about
// waiting for checks needs an environment.
func TestReapParksARunThatOpenedAPullRequest(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	f.PRByBranch["claude/build-7"] = 200
	if err := e.Store.SaveRun(state.Run{
		Kind: state.KindBuild, Ref: 7, Slot: 1, PID: 1,
		Branch: "claude/build-7", Started: time.Now(),
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	if err := e.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	runs, _ := e.Store.Runs()
	if len(runs) != 1 {
		t.Fatalf("runs = %v, want the run kept", runs)
	}
	if !runs[0].InPhase(state.PhaseAwaitingCI) {
		t.Errorf("phase = %q, want awaiting-ci", runs[0].Phase)
	}
	if runs[0].PR != 200 {
		t.Errorf("PR = %d, want 200", runs[0].PR)
	}

	// Not resolved: the work is not finished, so no outcome label yet.
	labels, _ := f.Labels(t.Context(), 7)
	if slices.Contains(labels, e.Cfg.Labels.Blocked) {
		t.Errorf("labels = %v, want no outcome while checks run", labels)
	}
}

// Without a pull request the old behaviour stands: a dead run is a decision.
func TestReapResolvesARunWithNoPullRequest(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	if err := e.Store.SaveRun(state.Run{
		Kind: state.KindBuild, Ref: 7, Slot: 1, PID: 1,
		Branch: "claude/build-7", Started: time.Now(),
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	if err := e.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 7)
	if !slices.Contains(labels, e.Cfg.Labels.Blocked) {
		t.Errorf("labels = %v, want blocked", labels)
	}
	if runs, _ := e.Store.Runs(); len(runs) != 0 {
		t.Errorf("runs = %v, want the record cleared", runs)
	}
}

func awaiting(t *testing.T, e Engine, ref, pr int, attempts int) {
	t.Helper()
	if err := e.Store.SaveRun(state.Run{
		Kind: state.KindBuild, Ref: ref, Slot: 1, PR: pr,
		Phase: state.PhaseAwaitingCI, Attempts: attempts,
		Branch: "claude/build-1", Started: time.Now(),
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
}

// Pending checks are not an outcome. Nothing should happen.
func TestAwaitingCIWaitsWhilePending(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	f.ChecksBy[200] = []forge.Check{{Name: "Unit Tests", Done: false}}
	awaiting(t, e, 7, 200, 0)

	if err := e.advanceAwaitingCI(t.Context()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	runs, _ := e.Store.Runs()
	if len(runs) != 1 || !runs[0].InPhase(state.PhaseAwaitingCI) {
		t.Errorf("runs = %v, want it still waiting", runs)
	}
	if len(f.Merged) != 0 {
		t.Error("merged while checks were still running")
	}
}

// No checks at all is not green. Merging here lands before CI was scheduled.
func TestAwaitingCIDoesNotTreatNoChecksAsGreen(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	awaiting(t, e, 7, 200, 0)

	if err := e.advanceAwaitingCI(t.Context()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if len(f.Merged) != 0 {
		t.Error("merged with no checks reported")
	}
	runs, _ := e.Store.Runs()
	if len(runs) != 1 {
		t.Errorf("runs = %v, want the run kept", runs)
	}
}

// After the cap, stop sending agents. The branch is left intact for a human.
func TestAwaitingCIGivesUpAfterTheCap(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	f.ChecksBy[200] = red()
	awaiting(t, e, 7, 200, e.Cfg.Limits.FixAttempts)

	if err := e.advanceAwaitingCI(t.Context()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 7)
	if !slices.Contains(labels, e.Cfg.Labels.Blocked) {
		t.Errorf("labels = %v, want blocked after the cap", labels)
	}
	if runs, _ := e.Store.Runs(); len(runs) != 0 {
		t.Errorf("runs = %v, want the record cleared", runs)
	}
	if len(f.Comments[7]) == 0 {
		t.Error("gave up without saying why on the issue")
	}
}

// A run already being fixed must not have a second agent sent at it.
func TestAwaitingCISkipsWhenAFixIsLive(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	f.ChecksBy[200] = red()
	awaiting(t, e, 7, 200, 1)

	// A second record on the same pull request, still running.
	if err := e.Store.SaveRun(state.Run{
		Kind: state.KindReview, Ref: 99, Slot: 2, PR: 200,
		Phase: state.PhaseRunning, PID: 1, Started: time.Now(),
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	if err := e.advanceAwaitingCI(t.Context()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	runs, _ := e.Store.Runs()
	for _, r := range runs {
		if r.Ref == 7 && r.Attempts != 1 {
			t.Errorf("attempts = %d, want it untouched while a fix is live", r.Attempts)
		}
	}
}

func TestPhaseDefaultsToRunning(t *testing.T) {
	var r state.Run
	if !r.InPhase(state.PhaseRunning) {
		t.Error("a record with no phase must read as running, or old records break")
	}
	if r.InPhase(state.PhaseAwaitingCI) {
		t.Error("an empty phase must not read as awaiting-ci")
	}
}

func TestLiveFixFor(t *testing.T) {
	runs := []state.Run{
		{Ref: 1, PR: 200, Phase: state.PhaseAwaitingCI},
		{Ref: 2, PR: 201, Phase: state.PhaseRunning},
	}
	if liveFixFor(runs, 200) {
		t.Error("a waiting run counts as a live fix")
	}
	if !liveFixFor(runs, 201) {
		t.Error("a running run on that pull request was missed")
	}
}

// A waiting run holds no slot, so it must not be counted against the pool.
func TestLiveSlotRunsExcludesWaitingRuns(t *testing.T) {
	runs := []state.Run{
		{Kind: state.KindBuild, Slot: 1, Phase: state.PhaseAwaitingCI},
		{Kind: state.KindBuild, Slot: 2, Phase: state.PhaseRunning},
		{Kind: state.KindPlan, Slot: 0, Phase: state.PhaseRunning},
	}
	got := liveSlotRuns(runs)
	if len(got) != 1 || got[0].Slot != 2 {
		t.Errorf("liveSlotRuns = %v, want only the running slot-holder", got)
	}
}

// A parked run still owns its issue. Counting only live pids would dispatch a
// second run on an issue whose pull request is already open.
func TestParkedRunStillBlocksItsIssue(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued)
	awaiting(t, e, 7, 200, 0)

	live, err := e.liveRuns()
	if err != nil {
		t.Fatalf("liveRuns: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("liveRuns = %v, want the parked run counted", live)
	}
	if !liveFor(live, state.KindBuild, 7) {
		t.Error("the parked run does not block its own issue")
	}
}

// It holds no slot, though — the environment came down when it parked.
func TestParkedRunReleasesItsSlot(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	f.PRByBranch["claude/build-7"] = 200
	if err := e.Store.SaveRun(state.Run{
		Kind: state.KindBuild, Ref: 7, Slot: 3, PID: 1,
		Branch: "claude/build-7", Started: time.Now(),
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := e.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	runs, _ := e.Store.Runs()
	if runs[0].Slot != 0 {
		t.Errorf("Slot = %d, want 0 — the environment is down", runs[0].Slot)
	}
	if got := claimedSlots(runs); len(got) != 0 {
		t.Errorf("claimedSlots = %v, want none reserved", got)
	}
}

// An issue that is both landed and blocked tells a reader nothing. Resolving
// must clear the outcome a previous run left behind.
func TestLandingClearsAStaleOutcomeLabel(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working, e.Cfg.Labels.Blocked)
	f.ChecksBy[200] = green()
	f.HeadSHA[200] = "abc"
	awaiting(t, e, 7, 200, 0)

	// Landing needs a real checkout; assert the label handling directly.
	if err := e.finishRun(t.Context(), state.Run{Kind: state.KindBuild, Ref: 7},
		e.Cfg.Labels.Landed, "done"); err != nil {
		t.Fatalf("finishRun: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 7)
	if !slices.Contains(labels, e.Cfg.Labels.Landed) {
		t.Errorf("labels = %v, want landed", labels)
	}
	if slices.Contains(labels, e.Cfg.Labels.Blocked) {
		t.Errorf("labels = %v, want the stale blocked label cleared", labels)
	}
	if !slices.Contains(labels, e.Cfg.Labels.Queued) {
		t.Errorf("labels = %v, want the queued label kept", labels)
	}
}
