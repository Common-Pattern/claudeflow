package forge

import (
	"slices"
	"strings"
	"testing"
)

// The agent speaks through the operator's credentials, so the marker is the
// only thing separating its words from theirs. Without it the system reads its
// own answer as a fresh instruction and runs again.
func TestSignAndDetect(t *testing.T) {
	signed := SignComment("here is my analysis")
	if !IsAgentComment(signed) {
		t.Fatal("a signed comment is not detected as the agent's")
	}
	if !strings.Contains(signed, "here is my analysis") {
		t.Error("signing lost the body")
	}
	if IsAgentComment("a comment from a human") {
		t.Error("an unsigned comment was read as the agent's")
	}
}

func TestSignIsIdempotent(t *testing.T) {
	once := SignComment("body")
	twice := SignComment(once)
	if once != twice {
		t.Errorf("signing twice changed the body:\n%q\n%q", once, twice)
	}
	if strings.Count(twice, AgentMarker) != 1 {
		t.Errorf("marker appears %d times, want 1", strings.Count(twice, AgentMarker))
	}
}

func TestClosingRefs(t *testing.T) {
	for _, tc := range []struct {
		body string
		want []int
	}{
		{"Closes #12", []int{12}},
		{"closes #12", []int{12}},
		{"Fixes #3 and Resolves #7", []int{3, 7}},
		{"fixed #9", []int{9}},
		{"Closed #4", []int{4}},
		{"resolve #5", []int{5}},
		{"Closes #12 and Closes #12", []int{12}},
		{"see #12", nil},
		{"mentions issue 12", nil},
		{"", nil},
		{"Fixes #2\nCloses #1", []int{1, 2}},
	} {
		got := ClosingRefs(tc.body)
		if !slices.Equal(got, tc.want) {
			t.Errorf("ClosingRefs(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// A bare "#12" is a reference, not an instruction to close.
func TestClosingRefsIgnoresPlainReferences(t *testing.T) {
	if got := ClosingRefs("related to #12, blocked by #13"); got != nil {
		t.Errorf("ClosingRefs = %v, want none", got)
	}
}

func TestWithClosingRefsAddsABlock(t *testing.T) {
	got := WithClosingRefs("## Summary\n\nsome work", []int{7})
	if !strings.Contains(got, "Fixes #7") {
		t.Errorf("body = %q, want it to close #7", got)
	}
	if !strings.Contains(got, "## Summary") {
		t.Error("the original body was lost")
	}
}

// Several landings accumulate onto one standing pull request, and each must
// preserve what the others put there.
func TestWithClosingRefsAccumulates(t *testing.T) {
	body := WithClosingRefs("base", []int{7})
	body = WithClosingRefs(body, []int{3})
	got := ClosingRefs(body)
	if !slices.Equal(got, []int{3, 7}) {
		t.Fatalf("refs = %v, want [3 7]", got)
	}
	if strings.Count(body, ClosesMarker) != 1 {
		t.Errorf("marker appears %d times, want 1", strings.Count(body, ClosesMarker))
	}
}

func TestWithClosingRefsDeduplicates(t *testing.T) {
	body := WithClosingRefs("base", []int{7})
	body = WithClosingRefs(body, []int{7})
	if n := strings.Count(body, "Fixes #7"); n != 1 {
		t.Errorf("Fixes #7 appears %d times, want 1", n)
	}
}

func TestWithClosingRefsNoRefsIsANoOp(t *testing.T) {
	if got := WithClosingRefs("unchanged", nil); got != "unchanged" {
		t.Errorf("= %q, want it untouched", got)
	}
}

// Text after the managed block must survive a later landing.
func TestWithClosingRefsPreservesTrailingContent(t *testing.T) {
	body := WithClosingRefs("intro", []int{1}) + "\ntrailing note\n"
	body = WithClosingRefs(body, []int{2})
	if !strings.Contains(body, "trailing note") {
		t.Errorf("body = %q, want the trailing note kept", body)
	}
	if !slices.Equal(ClosingRefs(body), []int{1, 2}) {
		t.Errorf("refs = %v, want [1 2]", ClosingRefs(body))
	}
}
