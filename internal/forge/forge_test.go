package forge

import (
	"errors"
	"testing"
	"time"
)

func TestIssueHasLabel(t *testing.T) {
	i := Issue{Labels: []string{"bug", "claude:working"}}
	if !i.HasLabel("claude:working") {
		t.Error("HasLabel missed a present label")
	}
	if i.HasLabel("claude") {
		t.Error(`HasLabel("claude") matched "claude:working"; it must be exact, not a prefix`)
	}
	if !i.HasAnyLabel("absent", "bug") {
		t.Error("HasAnyLabel missed a present label")
	}
	if i.HasAnyLabel("absent", "gone") {
		t.Error("HasAnyLabel matched nothing present")
	}
}

// A review carries submitted_at and a null created_at. Reading only created_at
// silently misses every review the user left, which is the whole PR channel.
func TestLatestByReadsReviewSubmittedAt(t *testing.T) {
	raw := []byte(`[
	  {"user":{"login":"sudhirj"},"created_at":null,"submitted_at":"2026-09-05T09:12:18Z"}
	]`)
	at, err := latestBy(raw, "sudhirj")
	if err != nil {
		t.Fatalf("latestBy: %v", err)
	}
	want := time.Date(2026, 9, 5, 9, 12, 18, 0, time.UTC)
	if !at.Equal(want) {
		t.Errorf("latestBy = %v, want %v", at, want)
	}
}

func TestLatestByFiltersAuthor(t *testing.T) {
	raw := []byte(`[
	  {"user":{"login":"vercel[bot]"},"created_at":"2026-09-10T00:00:00Z"},
	  {"user":{"login":"copilot-pull-request-reviewer[bot]"},"submitted_at":"2026-09-11T00:00:00Z"},
	  {"user":{"login":"sudhirj"},"created_at":"2026-09-05T00:00:00Z"}
	]`)
	at, err := latestBy(raw, "sudhirj")
	if err != nil {
		t.Fatalf("latestBy: %v", err)
	}
	want := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	if !at.Equal(want) {
		t.Errorf("latestBy = %v, want %v — bot comments must not count as instructions", at, want)
	}
}

func TestLatestByNoMatchingAuthor(t *testing.T) {
	raw := []byte(`[{"user":{"login":"someone"},"created_at":"2026-09-05T00:00:00Z"}]`)
	at, err := latestBy(raw, "sudhirj")
	if err != nil {
		t.Fatalf("latestBy: %v", err)
	}
	if !at.IsZero() {
		t.Errorf("latestBy = %v, want zero time", at)
	}
}

// gh --paginate concatenates one array per page rather than merging them.
func TestLatestByHandlesConcatenatedPages(t *testing.T) {
	raw := []byte(`[{"user":{"login":"u"},"created_at":"2026-01-01T00:00:00Z"}]` +
		`[{"user":{"login":"u"},"created_at":"2026-03-01T00:00:00Z"}]`)
	at, err := latestBy(raw, "u")
	if err != nil {
		t.Fatalf("latestBy: %v", err)
	}
	want := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if !at.Equal(want) {
		t.Errorf("latestBy = %v, want %v from the second page", at, want)
	}
}

func TestLatestByEmpty(t *testing.T) {
	at, err := latestBy([]byte(`[]`), "u")
	if err != nil {
		t.Fatalf("latestBy: %v", err)
	}
	if !at.IsZero() {
		t.Errorf("latestBy on empty = %v, want zero", at)
	}
}

func TestAllPassed(t *testing.T) {
	green := []Check{
		{Name: "Unit Tests", Passed: true, Done: true},
		{Name: "Lint", Passed: true, Done: true},
	}
	if !AllPassed(green, "Unit Tests", "Lint") {
		t.Error("AllPassed = false on two green checks")
	}
	// An absent required check must not read as passing. This is the failure
	// that lets a merge happen before CI was ever scheduled.
	if AllPassed(green, "Unit Tests", "Smoke") {
		t.Error("AllPassed = true with a required check absent")
	}
	pending := []Check{{Name: "Unit Tests", Passed: false, Done: false}}
	if AllPassed(pending, "Unit Tests") {
		t.Error("AllPassed = true on a pending check")
	}
	failed := []Check{{Name: "Unit Tests", Passed: false, Done: true}}
	if AllPassed(failed, "Unit Tests") {
		t.Error("AllPassed = true on a failed check")
	}
	if !AllPassed(green) {
		t.Error("AllPassed = false when nothing is required")
	}
}

func TestPendingAndFailed(t *testing.T) {
	checks := []Check{
		{Name: "a", Done: true, Passed: true},
		{Name: "b", Done: false},
		{Name: "c", Done: true, Passed: false},
	}
	if got := Pending(checks); len(got) != 1 || got[0] != "b" {
		t.Errorf("Pending = %v, want [b]", got)
	}
	if got := Failed(checks); len(got) != 1 || got[0] != "c" {
		t.Errorf("Failed = %v, want [c]", got)
	}
}

func TestPRNumberFromURL(t *testing.T) {
	n, err := prNumberFromURL("https://github.com/Common-Pattern/slotbooks/pull/238")
	if err != nil {
		t.Fatalf("prNumberFromURL: %v", err)
	}
	if n != 238 {
		t.Errorf("= %d, want 238", n)
	}
	if _, err := prNumberFromURL("not-a-url"); err == nil {
		t.Error("prNumberFromURL on junk succeeded, want error")
	}
}

func TestIntersect(t *testing.T) {
	got := intersect([]string{"a", "b", "c"}, []string{"b", "z"})
	if len(got) != 1 || got[0] != "b" {
		t.Errorf("intersect = %v, want [b]", got)
	}
	if got := intersect([]string{"a"}, nil); len(got) != 0 {
		t.Errorf("intersect with nothing held = %v, want empty", got)
	}
}

func TestFakeStandingPR(t *testing.T) {
	f := NewFake()
	if _, err := f.StandingPR(t.Context(), "main", "preview"); !errors.Is(err, ErrNoStandingPR) {
		t.Errorf("empty: err = %v, want ErrNoStandingPR", err)
	}
	f.Standing = []int{238}
	if n, err := f.StandingPR(t.Context(), "main", "preview"); err != nil || n != 238 {
		t.Errorf("one: (%d, %v), want (238, nil)", n, err)
	}
	f.Standing = []int{238, 300}
	_, err := f.StandingPR(t.Context(), "main", "preview")
	if !errors.Is(err, ErrManyStandingPRs) {
		t.Errorf("two: err = %v, want ErrManyStandingPRs — picking one silently is the bug", err)
	}
}

func TestFakeEditLabels(t *testing.T) {
	f := NewFake()
	f.AddIssue(1, time.Now(), "claude", "bug")
	if err := f.EditLabels(t.Context(), 1, []string{"claude:working"}, []string{"claude"}); err != nil {
		t.Fatalf("EditLabels: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 1)
	want := map[string]bool{"bug": true, "claude:working": true}
	if len(labels) != 2 {
		t.Fatalf("labels = %v, want exactly bug and claude:working", labels)
	}
	for _, l := range labels {
		if !want[l] {
			t.Errorf("unexpected label %q", l)
		}
	}
	// Removing an absent label must not fail.
	if err := f.EditLabels(t.Context(), 1, nil, []string{"nope"}); err != nil {
		t.Errorf("removing an absent label failed: %v", err)
	}
}

func TestFakeMergeChecksHead(t *testing.T) {
	f := NewFake()
	f.HeadSHA[5] = "abc123"
	if err := f.Merge(t.Context(), 5, "stale"); err == nil {
		t.Error("Merge with a stale SHA succeeded, want refusal")
	}
	if err := f.Merge(t.Context(), 5, "abc123"); err != nil {
		t.Errorf("Merge with the current SHA failed: %v", err)
	}
	if len(f.Merged) != 1 || f.Merged[0] != 5 {
		t.Errorf("Merged = %v, want [5]", f.Merged)
	}
}
