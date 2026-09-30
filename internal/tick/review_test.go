package tick

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

func newHumanEngine(t *testing.T) (Engine, *forge.Fake) {
	t.Helper()
	e, f := newEngine(t)
	e.Cfg.Merge = config.MergeHuman
	return e, f
}

func inReview(t *testing.T, e Engine, ref, pr int) {
	t.Helper()
	if err := e.Store.SaveRun(state.Run{
		Kind: state.KindBuild, Ref: ref, PR: pr, Phase: state.PhaseAwaitingReview,
		Branch: "claude/build-" + strconv.Itoa(ref), Announced: "abc", Started: time.Now(),
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
}

func onlyRun(t *testing.T, e Engine) state.Run {
	t.Helper()
	runs, err := e.Store.Runs()
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %v, want exactly one", runs)
	}
	return runs[0]
}

// Green is not a merge when a human merges. The run waits for them instead,
// and the issue says so.
func TestHumanMergeParksAGreenPullRequestForReview(t *testing.T) {
	e, f := newHumanEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	f.ChecksBy[200] = green()
	f.HeadSHA[200] = "abcdef1234"
	awaiting(t, e, 7, 200, 0)

	if err := e.advanceAwaitingCI(t.Context()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if len(f.Merged) != 0 {
		t.Fatalf("merged %v, want nothing merged", f.Merged)
	}
	if r := onlyRun(t, e); !r.InPhase(state.PhaseAwaitingReview) {
		t.Errorf("phase = %q, want awaiting review", r.Phase)
	}
	labels, _ := f.Labels(t.Context(), 7)
	if !slices.Contains(labels, e.Cfg.Labels.Review) || slices.Contains(labels, e.Cfg.Labels.Working) {
		t.Errorf("labels = %v, want review and not working", labels)
	}
	if len(f.Comments[7]) != 1 || !strings.Contains(f.Comments[7][0], "#200") {
		t.Errorf("comments = %v, want one saying #200 is ready", f.Comments[7])
	}
}

// A revision that changed nothing comes back green at the same commit. Saying
// "ready for review" again would be noise.
func TestHumanMergeAnnouncesEachCommitOnce(t *testing.T) {
	e, f := newHumanEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	f.HeadSHA[200] = "abc"
	r := state.Run{Kind: state.KindBuild, Ref: 7, PR: 200, Phase: state.PhaseAwaitingCI, Announced: "abc", Started: time.Now()}

	if err := e.readyForReview(t.Context(), r); err != nil {
		t.Fatalf("readyForReview: %v", err)
	}
	if len(f.Comments[7]) != 0 {
		t.Errorf("comments = %v, want none for an already announced commit", f.Comments[7])
	}
}

func TestHumanMergeFinishesWhenTheOperatorMerges(t *testing.T) {
	e, f := newHumanEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Review)
	f.States[200] = forge.PRMerged
	inReview(t, e, 7, 200)

	if err := e.advanceAwaitingReview(t.Context(), true); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if runs, _ := e.Store.Runs(); len(runs) != 0 {
		t.Errorf("runs = %v, want the run finished", runs)
	}
	labels, _ := f.Labels(t.Context(), 7)
	if !slices.Contains(labels, e.Cfg.Labels.Landed) || slices.Contains(labels, e.Cfg.Labels.Review) {
		t.Errorf("labels = %v, want landed and not review", labels)
	}
}

// Settling a merge is recording finished work, so a pause does not hold it.
func TestHumanMergeSettlesWhileDispatchIsBlocked(t *testing.T) {
	e, f := newHumanEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Review)
	f.States[200] = forge.PRMerged
	inReview(t, e, 7, 200)

	if err := e.advanceAwaitingReview(t.Context(), false); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if runs, _ := e.Store.Runs(); len(runs) != 0 {
		t.Errorf("runs = %v, want the merge settled while paused", runs)
	}
}

func TestHumanMergeBlocksWhenThePullRequestIsClosed(t *testing.T) {
	e, f := newHumanEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Review)
	f.States[200] = forge.PRClosed
	inReview(t, e, 7, 200)

	if err := e.advanceAwaitingReview(t.Context(), true); err != nil {
		t.Fatalf("advance: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 7)
	if !slices.Contains(labels, e.Cfg.Labels.Blocked) {
		t.Errorf("labels = %v, want blocked", labels)
	}
	if e.Cfg.Labels.IsQueued(labels) {
		t.Error("a closed pull request re-queued its issue; it would be rebuilt at once")
	}
}

// The operator may merge before the checks finish. Waiting on them would then
// wait on a pull request that no longer matters.
func TestHumanMergeSettlesAMergeWhileChecksAreRunning(t *testing.T) {
	e, f := newHumanEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	f.ChecksBy[200] = []forge.Check{{Name: "Unit Tests", Done: false}}
	f.States[200] = forge.PRMerged
	awaiting(t, e, 7, 200, 0)

	if err := e.advanceAwaitingCI(t.Context()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if runs, _ := e.Store.Runs(); len(runs) != 0 {
		t.Errorf("runs = %v, want the run finished", runs)
	}
}

func TestRevisionOwedOnAReviewOrAPullRequestComment(t *testing.T) {
	e, f := newHumanEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Review)
	inReview(t, e, 7, 200)
	r := onlyRun(t, e)
	_ = e.Store.MarkSeen(reviewKey(200), epoch)
	_ = e.Store.MarkSeen(issueKey(7), epoch)

	if owed, err := e.revisionOwed(t.Context(), r); err != nil || owed {
		t.Fatalf("revisionOwed = %v, %v; want nothing owed before anyone speaks", owed, err)
	}
	f.SetLastSpoke(forge.TargetPR, 200, epoch.Add(time.Hour))
	if owed, err := e.revisionOwed(t.Context(), r); err != nil || !owed {
		t.Fatalf("revisionOwed = %v, %v; want a revision after a review", owed, err)
	}
}

// A reply on the issue is as much an instruction as one on the pull request.
func TestRevisionOwedOnAnIssueComment(t *testing.T) {
	e, f := newHumanEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Review)
	inReview(t, e, 7, 200)
	_ = e.Store.MarkSeen(reviewKey(200), epoch)
	_ = e.Store.MarkSeen(issueKey(7), epoch)
	f.SetLastSpoke(forge.TargetIssue, 7, epoch.Add(time.Hour))

	if owed, err := e.revisionOwed(t.Context(), onlyRun(t, e)); err != nil || !owed {
		t.Fatalf("revisionOwed = %v, %v; want a revision after an issue comment", owed, err)
	}
}

// An instruction that arrives when no build slot is free must still be acted
// on later, not consumed by the tick that noticed it.
func TestRevisionThatCannotStartStaysOwed(t *testing.T) {
	e, f := newHumanEngine(t)
	e.Cfg.Limits.MaxBuilds = 0
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Review)
	inReview(t, e, 7, 200)
	_ = e.Store.MarkSeen(reviewKey(200), epoch)
	_ = e.Store.MarkSeen(issueKey(7), epoch)
	f.SetLastSpoke(forge.TargetPR, 200, epoch.Add(time.Hour))

	for range 2 {
		if err := e.advanceAwaitingReview(t.Context(), true); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	if !e.Store.FlagRaised(revisionPending(200)) {
		t.Error("the revision instruction was dropped")
	}
	if r := onlyRun(t, e); !r.InPhase(state.PhaseAwaitingReview) {
		t.Errorf("phase = %q, want still awaiting review", r.Phase)
	}
}

// First sight of the pull request records where its conversation stands, so
// the operator's first review after that is new rather than a baseline.
func TestAwaitCIBaselinesThePullRequestConversation(t *testing.T) {
	e, f := newHumanEngine(t)
	f.SetLastSpoke(forge.TargetPR, 200, epoch)
	r := state.Run{Kind: state.KindBuild, Ref: 7, Branch: "claude/build-7", Started: time.Now()}

	if err := e.awaitCI(t.Context(), r, 200); err != nil {
		t.Fatalf("awaitCI: %v", err)
	}
	isNew, err := e.Store.NoteLatest(reviewKey(200), epoch.Add(time.Minute))
	if err != nil || !isNew {
		t.Errorf("NoteLatest = %v, %v; want the first review after opening to be new", isNew, err)
	}
}

// Pull requests waiting on a person must not use up the build budget, or a few
// unreviewed ones would stop all new work.
func TestPlanDoesNotCountRunsAwaitingReview(t *testing.T) {
	in := baseInputs()
	in.Live = []state.Run{
		{Kind: state.KindBuild, Ref: 1, Phase: state.PhaseAwaitingReview},
		{Kind: state.KindBuild, Ref: 2, Phase: state.PhaseAwaitingReview},
	}
	in.QueuedBuild = []forge.Issue{issue(3, 0, "claude")}

	if got := Plan(t.Context(), in); len(got) != 1 || got[0].Ref != 3 {
		t.Errorf("Plan = %v, want issue 3 started", got)
	}
}

// The review pass reads the issue thread of a pull request under review. The
// re-queue watcher must leave it alone, or a reply would start a fresh build
// beside the open pull request.
func TestRequeueLeavesAnIssueUnderReviewAlone(t *testing.T) {
	w, f, st := newWatcher(t)
	w.Cfg.Merge = config.MergeHuman
	f.AddIssue(10, epoch, "claude", "claude:review")
	_ = st.MarkSeen(issueKey(10), epoch)
	f.SetLastSpoke(forge.TargetIssue, 10, epoch.Add(time.Hour))

	got, err := w.Requeue(t.Context())
	if err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Requeue = %v, want nothing re-queued", got)
	}
	if isNew, _ := st.NoteLatest(issueKey(10), epoch.Add(time.Hour)); !isNew {
		t.Error("Requeue consumed the comment the review pass needs")
	}
}

// A revision that ends after the operator merged has finished the issue, not
// died: there is no open pull request left for it, and that is success.
func TestReapSettlesARevisionWhosePullRequestWasMerged(t *testing.T) {
	e, f := newHumanEngine(t)
	e.Cfg.Compose.Bin = []string{"true"}
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	f.States[200] = forge.PRMerged
	if err := e.Store.SaveRun(state.Run{
		Kind: state.KindBuild, Ref: 7, PR: 200, Slot: 1, PID: deadPID(t),
		Branch: "claude/build-7", Started: time.Now(),
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	if err := e.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 7)
	if !slices.Contains(labels, e.Cfg.Labels.Landed) || slices.Contains(labels, e.Cfg.Labels.Blocked) {
		t.Errorf("labels = %v, want landed and not blocked", labels)
	}
	if runs, _ := e.Store.Runs(); len(runs) != 0 {
		t.Errorf("runs = %v, want the run finished", runs)
	}
}

func TestReviewNotesAPullRequestThatFellBehind(t *testing.T) {
	e, f := newHumanEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Review)
	f.HeadSHA[200] = "abc"
	f.Behind["abc"] = 3
	inReview(t, e, 7, 200)

	for range 2 {
		if err := e.advanceAwaitingReview(t.Context(), false); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	if len(f.Comments[200]) != 1 || !strings.Contains(f.Comments[200][0], "3 commits past") {
		t.Fatalf("comments on #200 = %v, want one note that it is 3 commits behind", f.Comments[200])
	}
	if len(f.Comments[7]) != 0 {
		t.Errorf("comments on the issue = %v, want the note on the pull request only", f.Comments[7])
	}
}

func TestReviewSaysNothingWhileUpToDate(t *testing.T) {
	e, f := newHumanEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Review)
	f.HeadSHA[200] = "abc"
	inReview(t, e, 7, 200)

	if err := e.advanceAwaitingReview(t.Context(), false); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if len(f.Comments[200]) != 0 {
		t.Errorf("comments = %v, want none for an up-to-date branch", f.Comments[200])
	}
}

// Updating the branch moves its head. If the base moves on again, that is news
// about the new commit and is said again.
func TestReviewNotesAgainForANewHead(t *testing.T) {
	e, f := newHumanEngine(t)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Review)
	f.HeadSHA[200] = "abc"
	f.Behind["abc"] = 1
	inReview(t, e, 7, 200)
	if err := e.advanceAwaitingReview(t.Context(), false); err != nil {
		t.Fatalf("advance: %v", err)
	}

	f.HeadSHA[200] = "def"
	f.Behind["def"] = 2
	if err := e.advanceAwaitingReview(t.Context(), false); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if len(f.Comments[200]) != 2 {
		t.Errorf("comments = %v, want one note per behind head", f.Comments[200])
	}
	if !strings.Contains(f.Comments[200][0], "1 commit past") {
		t.Errorf("first note = %q, want the singular", f.Comments[200][0])
	}
}
