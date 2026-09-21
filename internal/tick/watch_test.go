package tick

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

func newWatcher(t *testing.T) (Watcher, *forge.Fake, *state.Store) {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f := forge.NewFake()
	cfg := config.Default()
	cfg.Repo, cfg.User = "acme/widgets", "alice"
	cfg.Branches = config.Branches{Base: "main", Integration: "preview", Prefix: "claude/"}
	return Watcher{Client: f, Store: st, Cfg: cfg}, f, st
}

func TestRequeueFirstSightDoesNotFire(t *testing.T) {
	w, f, _ := newWatcher(t)
	f.AddIssue(10, epoch, "claude:blocked")
	f.SetLastSpoke(forge.TargetIssue, 10, epoch.Add(time.Hour))

	got, err := w.Requeue(t.Context())
	if err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("re-queued %v on first sight; switching on would replay every old comment", got)
	}
}

func TestRequeueFiresOnANewerComment(t *testing.T) {
	w, f, _ := newWatcher(t)
	f.AddIssue(10, epoch, "claude:blocked")
	f.SetLastSpoke(forge.TargetIssue, 10, epoch.Add(time.Hour))

	if _, err := w.Requeue(t.Context()); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	f.SetLastSpoke(forge.TargetIssue, 10, epoch.Add(2*time.Hour))

	got, err := w.Requeue(t.Context())
	if err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if len(got) != 1 || got[0] != 10 {
		t.Fatalf("re-queued %v, want [10]", got)
	}

	labels, _ := f.Labels(t.Context(), 10)
	if !slices.Contains(labels, "claude") {
		t.Errorf("labels = %v, want the queued label added", labels)
	}
	if slices.Contains(labels, "claude:blocked") {
		t.Errorf("labels = %v; the resolution label must be cleared on re-queue", labels)
	}
}

func TestRequeueDoesNotFireTwiceForOneComment(t *testing.T) {
	w, f, _ := newWatcher(t)
	f.AddIssue(10, epoch, "claude:question")
	f.SetLastSpoke(forge.TargetIssue, 10, epoch)
	if _, err := w.Requeue(t.Context()); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	f.SetLastSpoke(forge.TargetIssue, 10, epoch.Add(time.Hour))
	if got, _ := w.Requeue(t.Context()); len(got) != 1 {
		t.Fatalf("re-queued %v, want [10]", got)
	}
	if got, _ := w.Requeue(t.Context()); len(got) != 0 {
		t.Errorf("re-queued %v on an unchanged thread, want none", got)
	}
}

// A live run is already reading the whole thread; re-queueing underneath it
// would start a second run on one issue.
func TestRequeueSkipsAWorkingIssueButStillAdvancesTheMarker(t *testing.T) {
	w, f, _ := newWatcher(t)
	f.AddIssue(10, epoch, "claude:working")
	f.SetLastSpoke(forge.TargetIssue, 10, epoch)
	if _, err := w.Requeue(t.Context()); err != nil {
		t.Fatalf("Requeue: %v", err)
	}

	f.SetLastSpoke(forge.TargetIssue, 10, epoch.Add(time.Hour))
	got, err := w.Requeue(t.Context())
	if err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("re-queued %v while a run is live, want none", got)
	}

	// The marker advanced, so the same comment must not fire once the run ends.
	if err := f.EditLabels(t.Context(), 10, []string{"claude:landed"}, []string{"claude:working"}); err != nil {
		t.Fatalf("EditLabels: %v", err)
	}
	if got, _ := w.Requeue(t.Context()); len(got) != 0 {
		t.Errorf("re-queued %v after the run ended; that comment was already seen", got)
	}
}

func TestRequeueIgnoresThreadsTheUserNeverPostedOn(t *testing.T) {
	w, f, _ := newWatcher(t)
	f.AddIssue(10, epoch, "claude:landed")
	// No SetLastSpoke: only bots have commented.
	if got, err := w.Requeue(t.Context()); err != nil || len(got) != 0 {
		t.Errorf("Requeue = (%v, %v), want (none, nil)", got, err)
	}
}

func TestRequeueIgnoresUntouchedIssues(t *testing.T) {
	w, f, _ := newWatcher(t)
	f.AddIssue(10, epoch, "bug")
	f.SetLastSpoke(forge.TargetIssue, 10, epoch.Add(time.Hour))
	if got, _ := w.Requeue(t.Context()); len(got) != 0 {
		t.Errorf("re-queued %v; an issue the agent never touched is not its business", got)
	}
}

func TestReviewOwedFirstSightDoesNotFire(t *testing.T) {
	w, f, _ := newWatcher(t)
	f.Standing = []int{238}
	f.SetLastSpoke(forge.TargetPR, 238, epoch)

	pr, owed, err := w.ReviewOwed(t.Context())
	if err != nil {
		t.Fatalf("ReviewOwed: %v", err)
	}
	if pr != 238 {
		t.Errorf("pr = %d, want 238", pr)
	}
	if owed {
		t.Error("owed on first sight; a PR that rolls over would replay its whole history")
	}
}

func TestReviewOwedFiresAndStaysOwed(t *testing.T) {
	w, f, _ := newWatcher(t)
	f.Standing = []int{238}
	f.SetLastSpoke(forge.TargetPR, 238, epoch)
	if _, _, err := w.ReviewOwed(t.Context()); err != nil {
		t.Fatalf("ReviewOwed: %v", err)
	}

	f.SetLastSpoke(forge.TargetPR, 238, epoch.Add(time.Hour))
	_, owed, err := w.ReviewOwed(t.Context())
	if err != nil {
		t.Fatalf("ReviewOwed: %v", err)
	}
	if !owed {
		t.Fatal("owed = false after a new comment")
	}

	// Still owed on the next tick: a tick that could not allocate a slot must
	// not drop the instruction.
	if _, owed, _ = w.ReviewOwed(t.Context()); !owed {
		t.Error("owed = false on a later tick; the pending marker did not hold")
	}

	if err := w.ClearReviewOwed(238); err != nil {
		t.Fatalf("ClearReviewOwed: %v", err)
	}
	if _, owed, _ = w.ReviewOwed(t.Context()); owed {
		t.Error("owed = true after the review run started")
	}
}

func TestReviewOwedNoStandingPR(t *testing.T) {
	w, _, _ := newWatcher(t)
	pr, owed, err := w.ReviewOwed(t.Context())
	if err != nil {
		t.Fatalf("ReviewOwed: %v", err)
	}
	if pr != 0 || owed {
		t.Errorf("= (%d, %v), want (0, false)", pr, owed)
	}
}

// Two standing pull requests is not something to resolve by picking one: the
// agent would read comments from one and land work gated by the other.
func TestReviewOwedManyStandingPRsIsAnError(t *testing.T) {
	w, f, _ := newWatcher(t)
	f.Standing = []int{238, 300}
	if _, _, err := w.ReviewOwed(t.Context()); !errors.Is(err, forge.ErrManyStandingPRs) {
		t.Errorf("err = %v, want ErrManyStandingPRs", err)
	}
}

func TestReviewOwedSingleBranchProjectHasNone(t *testing.T) {
	w, f, _ := newWatcher(t)
	w.Cfg.Branches = config.Branches{Base: "main", Integration: "main"}
	f.Standing = []int{238}
	f.SetLastSpoke(forge.TargetPR, 238, epoch.Add(time.Hour))

	pr, owed, err := w.ReviewOwed(t.Context())
	if err != nil {
		t.Fatalf("ReviewOwed: %v", err)
	}
	if pr != 0 || owed {
		t.Errorf("= (%d, %v), want (0, false) with no separate integration branch", pr, owed)
	}
}

func TestReviewOwedForgetsMarkersForRolledOverPRs(t *testing.T) {
	w, f, st := newWatcher(t)
	if err := st.MarkSeen("pr-201", epoch); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	f.Standing = []int{238}
	f.SetLastSpoke(forge.TargetPR, 238, epoch)
	if _, _, err := w.ReviewOwed(t.Context()); err != nil {
		t.Fatalf("ReviewOwed: %v", err)
	}
	// pr-201 merged long ago; its marker can never fire again.
	if isNew, _ := st.NoteLatest("pr-201", epoch.Add(time.Hour)); isNew {
		t.Error("the stale marker survived and was treated as a live thread")
	}
}

func TestRequeuePropagatesClientErrors(t *testing.T) {
	w, f, _ := newWatcher(t)
	f.Err = errors.New("gh exploded")
	if _, err := w.Requeue(t.Context()); err == nil {
		t.Fatal("Requeue succeeded with a failing client, want the error surfaced")
	}
}
