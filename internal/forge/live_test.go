package forge

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// Live tests run against a real repository through a real gh, and are skipped
// unless CLAUDEFLOW_LIVE_REPO names one.
//
// They exist because every other test in this package decodes a fixture, and a
// fixture cannot notice gh changing its output shape. That is the failure this
// client is most exposed to: it reads gh's JSON, and gh's JSON is not a
// versioned contract.
//
//	CLAUDEFLOW_LIVE_REPO=owner/name \
//	CLAUDEFLOW_LIVE_USER=login \
//	CLAUDEFLOW_LIVE_ISSUE=219 \
//	CLAUDEFLOW_LIVE_PR=238 \
//	go test ./internal/forge/ -run Live -v
func liveClient(t *testing.T) (*GH, string) {
	t.Helper()
	repo := os.Getenv("CLAUDEFLOW_LIVE_REPO")
	if repo == "" {
		t.Skip("set CLAUDEFLOW_LIVE_REPO to run live tests")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh not available")
	}
	user := os.Getenv("CLAUDEFLOW_LIVE_USER")
	if user == "" {
		t.Skip("set CLAUDEFLOW_LIVE_USER to run live tests")
	}
	return NewGH(repo), user
}

func liveNumber(t *testing.T, env string) int {
	t.Helper()
	raw := os.Getenv(env)
	if raw == "" {
		t.Skipf("set %s to run this test", env)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s=%q: %v", env, raw, err)
	}
	return n
}

func TestLiveIssueComments(t *testing.T) {
	g, user := liveClient(t)
	n := liveNumber(t, "CLAUDEFLOW_LIVE_ISSUE")

	at, err := g.LatestCommentBy(t.Context(), TargetIssue, n, user)
	if err != nil {
		t.Fatalf("LatestCommentBy: %v", err)
	}
	t.Logf("issue #%d: %s last spoke at %v", n, user, at)
	if at.IsZero() {
		t.Logf("note: %s has not commented on #%d, so this proves decoding but not matching", user, n)
	}
}

// The pull request path reads two shapes — gh's own from the subcommand and
// REST's from the one endpoint with no subcommand. This is the test that
// notices if either moves.
func TestLivePRComments(t *testing.T) {
	g, user := liveClient(t)
	n := liveNumber(t, "CLAUDEFLOW_LIVE_PR")

	at, err := g.LatestCommentBy(t.Context(), TargetPR, n, user)
	if err != nil {
		t.Fatalf("LatestCommentBy: %v", err)
	}
	t.Logf("pr #%d: %s last spoke at %v", n, user, at)
}

func TestLiveOpenIssues(t *testing.T) {
	g, _ := liveClient(t)
	issues, err := g.OpenIssues(t.Context())
	if err != nil {
		t.Fatalf("OpenIssues: %v", err)
	}
	t.Logf("%d open issues", len(issues))
	for _, i := range issues {
		if i.Number == 0 {
			t.Error("an issue decoded with number 0; the JSON shape has moved")
		}
		if i.CreatedAt.IsZero() {
			t.Errorf("issue #%d decoded with no createdAt; ordering would be arbitrary", i.Number)
		}
	}
}

func TestLiveLabels(t *testing.T) {
	g, _ := liveClient(t)
	n := liveNumber(t, "CLAUDEFLOW_LIVE_ISSUE")

	labels, err := g.Labels(t.Context(), n)
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	t.Logf("issue #%d labels: %v", n, labels)
}

func TestLiveStandingPR(t *testing.T) {
	g, _ := liveClient(t)
	base, head := os.Getenv("CLAUDEFLOW_LIVE_BASE"), os.Getenv("CLAUDEFLOW_LIVE_HEAD")
	if base == "" || head == "" {
		t.Skip("set CLAUDEFLOW_LIVE_BASE and CLAUDEFLOW_LIVE_HEAD")
	}
	pr, err := g.StandingPR(t.Context(), base, head)
	switch {
	case errors.Is(err, ErrNoStandingPR):
		t.Logf("no standing %s → %s pull request", head, base)
	case errors.Is(err, ErrManyStandingPRs):
		t.Errorf("more than one standing pull request: %v", err)
	case err != nil:
		t.Fatalf("StandingPR: %v", err)
	default:
		t.Logf("standing pull request: #%d", pr)
	}
}

func TestLiveChecks(t *testing.T) {
	g, _ := liveClient(t)
	n := liveNumber(t, "CLAUDEFLOW_LIVE_PR")

	checks, err := g.Checks(t.Context(), n)
	if err != nil {
		t.Fatalf("Checks: %v", err)
	}
	t.Logf("pr #%d: %d checks, %d pending, %d failed", n, len(checks), len(Pending(checks)), len(Failed(checks)))
	for _, c := range checks {
		if c.Name == "" {
			t.Error("a check decoded with no name; the JSON shape has moved")
		}
	}
}

func TestLivePRHeadSHA(t *testing.T) {
	g, _ := liveClient(t)
	n := liveNumber(t, "CLAUDEFLOW_LIVE_PR")

	sha, err := g.PRHeadSHA(t.Context(), n)
	if err != nil {
		t.Fatalf("PRHeadSHA: %v", err)
	}
	if len(sha) != 40 {
		t.Errorf("headRefOid = %q, want a 40-character sha", sha)
	}
}
