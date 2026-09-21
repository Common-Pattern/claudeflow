package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestRunRoundTrip(t *testing.T) {
	s := newStore(t)
	want := Run{
		Kind: KindBuild, Ref: 192, Slot: 3,
		PID: 4242, Branch: "claude/issue-192",
		Worktree: "/tmp/wt", Log: "/tmp/log", Started: time.Now().UTC().Truncate(time.Second),
	}
	if err := s.SaveRun(want); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	runs, err := s.Runs()
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("Runs() returned %d, want 1", len(runs))
	}
	if got := runs[0]; got != want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestRunsOrderedByStart(t *testing.T) {
	s := newStore(t)
	base := time.Now().UTC()
	for i, d := range []time.Duration{3 * time.Hour, time.Hour, 2 * time.Hour} {
		if err := s.SaveRun(Run{Kind: KindBuild, Ref: i + 1, Started: base.Add(d)}); err != nil {
			t.Fatalf("SaveRun: %v", err)
		}
	}
	runs, err := s.Runs()
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	wantRefs := []int{2, 3, 1}
	for i, want := range wantRefs {
		if runs[i].Ref != want {
			t.Errorf("runs[%d].Ref = %d, want %d (oldest first)", i, runs[i].Ref, want)
		}
	}
}

func TestRunsEmptyStore(t *testing.T) {
	runs, err := newStore(t).Runs()
	if err != nil {
		t.Fatalf("Runs on empty store: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("Runs() returned %d, want 0", len(runs))
	}
}

func TestSaveRunRejectsUnknownKind(t *testing.T) {
	if err := newStore(t).SaveRun(Run{Kind: "sideways", Ref: 1}); err == nil {
		t.Fatal("SaveRun with an unknown kind succeeded, want error")
	}
}

func TestDeleteRunIsIdempotent(t *testing.T) {
	s := newStore(t)
	if err := s.SaveRun(Run{Kind: KindPlan, Ref: 7}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	for i := range 2 {
		if err := s.DeleteRun("plan-7"); err != nil {
			t.Fatalf("DeleteRun call %d: %v", i+1, err)
		}
	}
	runs, _ := s.Runs()
	if len(runs) != 0 {
		t.Errorf("after delete, Runs() returned %d, want 0", len(runs))
	}
}

func TestCorruptRunRecordIsReported(t *testing.T) {
	s := newStore(t)
	if err := os.WriteFile(filepath.Join(s.Dir(), "runs", "build-1.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.Runs(); err == nil {
		t.Fatal("Runs() on a corrupt record succeeded, want an error rather than a silent skip")
	}
}

// The first sight of a thread must record without acting, or switching the
// system on would replay every comment ever written.
func TestNoteLatestFirstSightDoesNotFire(t *testing.T) {
	s := newStore(t)
	at := time.Date(2026, 8, 25, 10, 14, 12, 0, time.UTC)
	isNew, err := s.NoteLatest("issue-219", at)
	if err != nil {
		t.Fatalf("NoteLatest: %v", err)
	}
	if isNew {
		t.Error("first sight reported new, want it to record silently")
	}
}

func TestNoteLatestSequence(t *testing.T) {
	s := newStore(t)
	t0 := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)

	if isNew, _ := s.NoteLatest("issue-1", t0); isNew {
		t.Fatal("first sight reported new")
	}
	if isNew, _ := s.NoteLatest("issue-1", t0); isNew {
		t.Error("same timestamp reported new, want false")
	}
	if isNew, _ := s.NoteLatest("issue-1", t0.Add(-time.Hour)); isNew {
		t.Error("older timestamp reported new, want false")
	}
	isNew, err := s.NoteLatest("issue-1", t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("NoteLatest: %v", err)
	}
	if !isNew {
		t.Error("newer timestamp did not report new")
	}
	// Having fired, the marker must have advanced.
	if isNew, _ := s.NoteLatest("issue-1", t0.Add(time.Minute)); isNew {
		t.Error("marker did not advance after firing, so the same comment would fire twice")
	}
}

func TestNoteLatestZeroTimeNeverFires(t *testing.T) {
	s := newStore(t)
	isNew, err := s.NoteLatest("issue-2", time.Time{})
	if err != nil {
		t.Fatalf("NoteLatest: %v", err)
	}
	if isNew {
		t.Error("zero time reported new; a thread with no instruction comments must never fire")
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "seen", "issue-2")); err == nil {
		t.Error("zero time wrote a marker, want none")
	}
}

func TestNoteLatestCorruptMarkerTreatedAsFirstSight(t *testing.T) {
	s := newStore(t)
	if err := os.WriteFile(filepath.Join(s.Dir(), "seen", "issue-3"), []byte("not a time"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	isNew, err := s.NoteLatest("issue-3", time.Now())
	if err != nil {
		t.Fatalf("NoteLatest: %v", err)
	}
	if isNew {
		t.Error("corrupt marker fired; re-acting is the expensive direction, so it must record instead")
	}
}

func TestForgetSeenExcept(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	for _, k := range []string{"pr-238", "pr-201", "issue-9"} {
		if err := s.MarkSeen(k, now); err != nil {
			t.Fatalf("MarkSeen %s: %v", k, err)
		}
	}
	if err := s.ForgetSeenExcept("pr-", "pr-238"); err != nil {
		t.Fatalf("ForgetSeenExcept: %v", err)
	}
	for name, wantPresent := range map[string]bool{"pr-238": true, "pr-201": false, "issue-9": true} {
		_, err := os.Stat(filepath.Join(s.Dir(), "seen", name))
		if present := err == nil; present != wantPresent {
			t.Errorf("marker %s present = %v, want %v", name, present, wantPresent)
		}
	}
}

func TestFlags(t *testing.T) {
	s := newStore(t)
	if s.FlagRaised(Paused) {
		t.Error("new store reports paused")
	}
	if err := s.SetFlag(Paused, ""); err != nil {
		t.Fatalf("SetFlag: %v", err)
	}
	if !s.FlagRaised(Paused) {
		t.Error("flag not raised after SetFlag")
	}
	for i := range 2 {
		if err := s.ClearFlag(Paused); err != nil {
			t.Fatalf("ClearFlag call %d: %v", i+1, err)
		}
	}
	if s.FlagRaised(Paused) {
		t.Error("flag still raised after ClearFlag")
	}
}

func TestRateLimit(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC()

	if _, ok := s.RateLimitedUntil(now); ok {
		t.Error("new store reports rate limited")
	}
	if err := s.SetRateLimited(now.Add(time.Hour)); err != nil {
		t.Fatalf("SetRateLimited: %v", err)
	}
	until, ok := s.RateLimitedUntil(now)
	if !ok {
		t.Fatal("RateLimitedUntil reported false while a backoff is in force")
	}
	if !until.After(now) {
		t.Errorf("until = %v, want after %v", until, now)
	}
	// Once elapsed it must stop reporting, without needing a separate clear.
	if _, ok := s.RateLimitedUntil(now.Add(2 * time.Hour)); ok {
		t.Error("elapsed backoff still reports rate limited")
	}
}

func TestKindHelpers(t *testing.T) {
	for _, k := range []Kind{KindBuild, KindReview, KindPlan} {
		if !k.Valid() {
			t.Errorf("%q.Valid() = false", k)
		}
	}
	if Kind("nope").Valid() {
		t.Error("unknown kind reported valid")
	}
	if !KindBuild.HoldsSlot() || !KindReview.HoldsSlot() {
		t.Error("build and review runs must hold a slot")
	}
	if KindPlan.HoldsSlot() {
		t.Error("planning runs must not hold a slot")
	}
}

func TestRunID(t *testing.T) {
	if got := (Run{Kind: KindBuild, Ref: 42}).ID(); got != "build-42" {
		t.Errorf("ID() = %q, want build-42", got)
	}
}
