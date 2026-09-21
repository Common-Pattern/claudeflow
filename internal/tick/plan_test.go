package tick

import (
	"testing"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/slot"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

var epoch = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func issue(n int, minutesOld int, labels ...string) forge.Issue {
	return forge.Issue{Number: n, CreatedAt: epoch.Add(time.Duration(minutesOld) * time.Minute), Labels: labels}
}

func baseInputs() Inputs {
	return Inputs{
		Now:    epoch,
		Limits: config.Limits{MaxBuilds: 2, MaxPlanning: 2},
		Alloc:  slot.Allocator{Min: 1, Max: 6},
	}
}

func kinds(starts []Start) []string {
	out := make([]string, 0, len(starts))
	for _, s := range starts {
		out = append(out, string(s.Kind))
	}
	return out
}

func TestPlanEmpty(t *testing.T) {
	if got := Plan(t.Context(), baseInputs()); len(got) != 0 {
		t.Errorf("Plan on nothing queued = %v, want none", got)
	}
}

func TestPlanStartsOldestFirst(t *testing.T) {
	in := baseInputs()
	in.QueuedBuild = []forge.Issue{issue(9, 30), issue(3, 10), issue(7, 20)}
	got := Plan(t.Context(), in)
	if len(got) != 2 {
		t.Fatalf("started %d, want 2 (the cap)", len(got))
	}
	if got[0].Ref != 3 || got[1].Ref != 7 {
		t.Errorf("refs = %d,%d, want 3,7 (oldest first)", got[0].Ref, got[1].Ref)
	}
}

func TestPlanTiesBreakByNumber(t *testing.T) {
	in := baseInputs()
	in.QueuedBuild = []forge.Issue{issue(9, 5), issue(4, 5)}
	got := Plan(t.Context(), in)
	if got[0].Ref != 4 {
		t.Errorf("first = %d, want 4 — equal timestamps break by issue number", got[0].Ref)
	}
}

func TestPlanRespectsBuildCap(t *testing.T) {
	in := baseInputs()
	in.Limits.MaxBuilds = 1
	in.QueuedBuild = []forge.Issue{issue(1, 1), issue(2, 2)}
	if got := Plan(t.Context(), in); len(got) != 1 {
		t.Errorf("started %d, want 1", len(got))
	}
}

func TestPlanCountsLiveAgainstCap(t *testing.T) {
	in := baseInputs()
	in.Live = []state.Run{{Kind: state.KindBuild, Ref: 99, Slot: 1}}
	in.QueuedBuild = []forge.Issue{issue(1, 1), issue(2, 2)}
	got := Plan(t.Context(), in)
	if len(got) != 1 {
		t.Fatalf("started %d, want 1 — one build is already live against a cap of 2", len(got))
	}
	if got[0].Slot == 1 {
		t.Error("allocated slot 1, which the live run holds")
	}
}

func TestPlanAllocatesDistinctSlots(t *testing.T) {
	in := baseInputs()
	in.QueuedBuild = []forge.Issue{issue(1, 1), issue(2, 2)}
	got := Plan(t.Context(), in)
	if len(got) != 2 {
		t.Fatalf("started %d, want 2", len(got))
	}
	if got[0].Slot == got[1].Slot {
		t.Errorf("both runs got slot %d; two environments on one slot collide", got[0].Slot)
	}
}

func TestPlanStopsWhenSlotsExhausted(t *testing.T) {
	in := baseInputs()
	in.Limits.MaxBuilds = 4
	in.Alloc = slot.Allocator{Min: 1, Max: 1}
	in.QueuedBuild = []forge.Issue{issue(1, 1), issue(2, 2), issue(3, 3)}
	if got := Plan(t.Context(), in); len(got) != 1 {
		t.Errorf("started %d, want 1 — only one slot exists", len(got))
	}
}

func TestPlanSkipsIssueAlreadyLive(t *testing.T) {
	in := baseInputs()
	in.Live = []state.Run{{Kind: state.KindBuild, Ref: 5, Slot: 2}}
	in.QueuedBuild = []forge.Issue{issue(5, 1), issue(6, 2)}
	got := Plan(t.Context(), in)
	if len(got) != 1 || got[0].Ref != 6 {
		t.Errorf("starts = %v, want only issue 6; issue 5 is already running", got)
	}
}

// Planning is budgeted apart from building, so a full build queue must not
// stop a conversation, and vice versa.
func TestPlanningIsBudgetedSeparately(t *testing.T) {
	in := baseInputs()
	in.Live = []state.Run{
		{Kind: state.KindBuild, Ref: 1, Slot: 1},
		{Kind: state.KindBuild, Ref: 2, Slot: 2},
	}
	in.QueuedBuild = []forge.Issue{issue(3, 1)}
	in.QueuedPlan = []forge.Issue{issue(4, 1)}

	got := Plan(t.Context(), in)
	if len(got) != 1 {
		t.Fatalf("starts = %v, want exactly the planning run", got)
	}
	if got[0].Kind != state.KindPlan || got[0].Ref != 4 {
		t.Errorf("start = %+v, want a plan run on issue 4", got[0])
	}
}

func TestPlanningHoldsNoSlot(t *testing.T) {
	in := baseInputs()
	in.QueuedPlan = []forge.Issue{issue(4, 1)}
	got := Plan(t.Context(), in)
	if len(got) != 1 {
		t.Fatalf("starts = %v, want 1", got)
	}
	if got[0].Slot != 0 {
		t.Errorf("plan run got slot %d, want 0 — planning reads in place", got[0].Slot)
	}
}

func TestPlanningRespectsItsOwnCap(t *testing.T) {
	in := baseInputs()
	in.Limits.MaxPlanning = 1
	in.QueuedPlan = []forge.Issue{issue(1, 1), issue(2, 2)}
	if got := Plan(t.Context(), in); len(got) != 1 {
		t.Errorf("started %d planning runs, want 1", len(got))
	}
}

func TestPlanningDoesNotConsumeSlotsFromBuilds(t *testing.T) {
	in := baseInputs()
	in.Alloc = slot.Allocator{Min: 1, Max: 1}
	in.QueuedPlan = []forge.Issue{issue(1, 1)}
	in.QueuedBuild = []forge.Issue{issue(2, 2)}
	got := Plan(t.Context(), in)
	if len(got) != 2 {
		t.Fatalf("starts = %v, want both a plan and a build", kinds(got))
	}
}

// Review comments come before new issues: the user is waiting on those, and
// work already on the integration branch that is wrong matters more than work
// that has not started.
func TestReviewGoesBeforeNewIssues(t *testing.T) {
	in := baseInputs()
	in.Limits.MaxBuilds = 1
	in.ReviewOwed = true
	in.StandingPR = 238
	in.QueuedBuild = []forge.Issue{issue(1, 1)}

	got := Plan(t.Context(), in)
	if len(got) != 1 {
		t.Fatalf("starts = %v, want 1 at a cap of 1", kinds(got))
	}
	if got[0].Kind != state.KindReview || got[0].Ref != 238 {
		t.Errorf("start = %+v, want the review run on 238", got[0])
	}
}

func TestReviewNotStartedWithoutAStandingPR(t *testing.T) {
	in := baseInputs()
	in.ReviewOwed = true
	in.StandingPR = 0
	if got := Plan(t.Context(), in); len(got) != 0 {
		t.Errorf("starts = %v, want none without a standing PR", got)
	}
}

func TestReviewNotStartedTwice(t *testing.T) {
	in := baseInputs()
	in.ReviewOwed = true
	in.StandingPR = 238
	in.Live = []state.Run{{Kind: state.KindReview, Ref: 238, Slot: 1}}
	if got := Plan(t.Context(), in); len(got) != 0 {
		t.Errorf("starts = %v, want none; a review run is already live", got)
	}
}

func TestReviewTakesASlot(t *testing.T) {
	in := baseInputs()
	in.ReviewOwed = true
	in.StandingPR = 238
	got := Plan(t.Context(), in)
	if len(got) != 1 {
		t.Fatalf("starts = %v, want 1", got)
	}
	if got[0].Slot == 0 {
		t.Error("review run got no slot; it lands code and needs an environment")
	}
}

func TestSplitQueued(t *testing.T) {
	l := config.Default().Labels
	in := []forge.Issue{
		issue(1, 1, l.Queued),
		issue(2, 2, l.Queued, l.Planning),
		issue(3, 3, l.Queued, l.Working),
		issue(4, 4, l.Landed),
		issue(5, 5, l.Queued, l.Blocked),
	}
	build, plan := SplitQueued(in, l)

	if len(build) != 2 {
		t.Fatalf("build = %v, want issues 1 and 5", refs(build))
	}
	if build[0].Number != 1 || build[1].Number != 5 {
		t.Errorf("build refs = %v, want [1 5]", refs(build))
	}
	if len(plan) != 1 || plan[0].Number != 2 {
		t.Errorf("plan refs = %v, want [2]", refs(plan))
	}
}

// An issue carrying both queued and planning is a conversation. Treating it as
// a build would have the agent write code on an issue whose whole point is
// that it is not yet understood.
func TestSplitQueuedPlanningNeverBecomesABuild(t *testing.T) {
	l := config.Default().Labels
	build, plan := SplitQueued([]forge.Issue{issue(2, 1, l.Queued, l.Planning)}, l)
	if len(build) != 0 {
		t.Errorf("build = %v, want none", refs(build))
	}
	if len(plan) != 1 {
		t.Errorf("plan = %v, want issue 2", refs(plan))
	}
}

func TestSplitQueuedIgnoresUnlabelled(t *testing.T) {
	l := config.Default().Labels
	build, plan := SplitQueued([]forge.Issue{issue(1, 1, "bug")}, l)
	if len(build) != 0 || len(plan) != 0 {
		t.Errorf("an issue without the queued label was picked up: build=%v plan=%v", refs(build), refs(plan))
	}
}

func TestDispatchBlocked(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Now()

	if err := DispatchBlocked(st, now); err != nil {
		t.Errorf("fresh store blocked: %v", err)
	}
	if err := st.SetFlag(state.Paused, ""); err != nil {
		t.Fatalf("SetFlag: %v", err)
	}
	if err := DispatchBlocked(st, now); err != ErrPaused {
		t.Errorf("err = %v, want ErrPaused", err)
	}
	if err := st.ClearFlag(state.Paused); err != nil {
		t.Fatalf("ClearFlag: %v", err)
	}
	if err := st.SetRateLimited(now.Add(time.Hour)); err != nil {
		t.Fatalf("SetRateLimited: %v", err)
	}
	if err := DispatchBlocked(st, now); err != ErrRateLimited {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
	// Once the backoff elapses it must clear itself.
	if err := DispatchBlocked(st, now.Add(2*time.Hour)); err != nil {
		t.Errorf("after backoff elapsed: %v, want nil", err)
	}
}

func refs(issues []forge.Issue) []int {
	out := make([]int, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.Number)
	}
	return out
}
