package land

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/git"
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// run executes git in dir with the operator's global config neutralised, so a
// developer's gpgsign or hooksPath cannot change what these tests measure.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// world builds an origin, a checkout on the integration branch, and a feature
// worktree branched from it.
type world struct {
	origin, checkout, worktree string
}

func newWorld(t *testing.T) world {
	t.Helper()
	needGit(t)
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	run(t, root, "init", "--bare", "-b", "preview", origin)

	checkout := filepath.Join(root, "checkout")
	run(t, root, "clone", origin, checkout)
	// Landing merges through git.Repo, which inherits the process environment
	// rather than this helper's. A machine with no git identity — a CI runner,
	// a fresh container — then fails the merge with "Committer identity
	// unknown". Configure the repository the way a real checkout is.
	// Worktrees share this config, so setting it once covers them too.
	run(t, checkout, "config", "user.email", "t@example.com")
	run(t, checkout, "config", "user.name", "t")
	run(t, checkout, "commit", "--allow-empty", "-m", "base")
	run(t, checkout, "push", "-u", "origin", "preview")

	worktree := filepath.Join(root, "wt")
	run(t, checkout, "worktree", "add", "-b", "claude/issue-1", worktree, "preview")
	run(t, worktree, "commit", "--allow-empty", "-m", "work")
	run(t, worktree, "push", "-u", "origin", "claude/issue-1")

	return world{origin: origin, checkout: checkout, worktree: worktree}
}

func cfg() config.Config {
	c := config.Default()
	c.Repo, c.User = "acme/widgets", "alice"
	c.Branches = config.Branches{Base: "main", Integration: "preview", Prefix: "claude/"}
	return c
}

func greenChecks() []forge.Check {
	return []forge.Check{
		{Name: "Unit Tests", Done: true, Passed: true},
		{Name: "Lint", Done: true, Passed: true},
	}
}

func lander(w world, f *forge.Fake) Lander {
	return Lander{Cfg: cfg(), Client: f, Repo: git.New(w.checkout), Required: []string{"Unit Tests", "Lint"}}
}

func TestLandMergesAndSyncsTheCheckout(t *testing.T) {
	w := newWorld(t)
	f := forge.NewFake()
	f.ChecksBy[7] = greenChecks()
	f.Standing = []int{238}

	res, err := lander(w, f).Land(t.Context(), 7, w.worktree)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(f.Merged) != 1 || f.Merged[0] != 7 {
		t.Errorf("Merged = %v, want [7]", f.Merged)
	}
	if res.MergedSHA == "" {
		t.Error("Result.MergedSHA is empty")
	}
}

// The merge happens on the server and touches nothing locally. Without the
// fast-forward the checkout keeps serving pre-merge code and every later check
// passes honestly against the wrong tree.
func TestLandFastForwardsTheLocalCheckout(t *testing.T) {
	w := newWorld(t)
	// Simulate the merge having landed on origin already.
	run(t, w.worktree, "push", "origin", "HEAD:preview")

	before := run(t, w.checkout, "rev-parse", "HEAD")
	f := forge.NewFake()
	f.ChecksBy[7] = greenChecks()
	f.Standing = []int{238}

	if _, err := lander(w, f).Land(t.Context(), 7, w.worktree); err != nil {
		t.Fatalf("Land: %v", err)
	}
	after := run(t, w.checkout, "rev-parse", "HEAD")
	if before == after {
		t.Error("checkout HEAD did not move; the local tree is still pre-merge")
	}
}

func TestLandRefusesWhenAChecksIsFailing(t *testing.T) {
	w := newWorld(t)
	f := forge.NewFake()
	f.ChecksBy[7] = []forge.Check{
		{Name: "Unit Tests", Done: true, Passed: false},
		{Name: "Lint", Done: true, Passed: true},
	}
	_, err := lander(w, f).Land(t.Context(), 7, w.worktree)
	if !errors.Is(err, ErrChecksNotGreen) {
		t.Fatalf("err = %v, want ErrChecksNotGreen", err)
	}
	if len(f.Merged) != 0 {
		t.Error("merged despite a failing check")
	}
}

func TestLandRefusesWhenAChecksIsStillRunning(t *testing.T) {
	w := newWorld(t)
	f := forge.NewFake()
	f.ChecksBy[7] = []forge.Check{
		{Name: "Unit Tests", Done: false},
		{Name: "Lint", Done: true, Passed: true},
	}
	_, err := lander(w, f).Land(t.Context(), 7, w.worktree)
	if !errors.Is(err, ErrChecksNotGreen) {
		t.Fatalf("err = %v, want ErrChecksNotGreen", err)
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("err = %q, want it to name the pending check", err)
	}
}

// A required check that is absent is the same answer as one that failed. This
// is what stops a merge landing before CI was ever scheduled.
func TestLandRefusesWhenARequiredCheckIsAbsent(t *testing.T) {
	w := newWorld(t)
	f := forge.NewFake()
	f.ChecksBy[7] = []forge.Check{{Name: "Vercel", Done: true, Passed: true}}

	_, err := lander(w, f).Land(t.Context(), 7, w.worktree)
	if !errors.Is(err, ErrChecksNotGreen) {
		t.Fatalf("err = %v, want ErrChecksNotGreen", err)
	}
	if len(f.Merged) != 0 {
		t.Error("merged with the required checks absent — this is the pre-CI merge bug")
	}
}

func TestLandRefusesWhenThereAreNoChecksAtAll(t *testing.T) {
	w := newWorld(t)
	f := forge.NewFake()
	l := lander(w, f)
	l.Required = nil // fall back to "nothing failed or pending"

	_, err := l.Land(t.Context(), 7, w.worktree)
	if !errors.Is(err, ErrChecksNotGreen) {
		t.Errorf("err = %v, want ErrChecksNotGreen; no checks is not evidence of anything", err)
	}
}

func TestLandResyncsAndRewaitsWhenBehind(t *testing.T) {
	w := newWorld(t)
	// Move the integration branch on, so the feature branch falls behind.
	run(t, w.checkout, "commit", "--allow-empty", "-m", "someone else landed")
	run(t, w.checkout, "push", "origin", "preview")

	f := forge.NewFake()
	f.ChecksBy[7] = greenChecks()
	f.Standing = []int{238}

	waited := 0
	l := lander(w, f)
	l.Wait = func(context.Context, int) ([]forge.Check, error) { waited++; return greenChecks(), nil }

	res, err := l.Land(t.Context(), 7, w.worktree)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if !res.Resynced {
		t.Error("Result.Resynced = false, want true")
	}
	if waited != 1 {
		t.Errorf("waited %d times, want 1 — CI must re-run against the moved base", waited)
	}
}

func TestLandReportsChecksFailingAfterResync(t *testing.T) {
	w := newWorld(t)
	run(t, w.checkout, "commit", "--allow-empty", "-m", "moved on")
	run(t, w.checkout, "push", "origin", "preview")

	f := forge.NewFake()
	f.ChecksBy[7] = greenChecks()
	l := lander(w, f)
	l.Wait = func(context.Context, int) ([]forge.Check, error) {
		return []forge.Check{
			{Name: "Unit Tests", Done: true, Passed: false},
			{Name: "Lint", Done: true, Passed: true},
		}, nil
	}

	_, err := l.Land(t.Context(), 7, w.worktree)
	if !errors.Is(err, ErrChecksAfterSync) {
		t.Fatalf("err = %v, want ErrChecksAfterSync", err)
	}
	if len(f.Merged) != 0 {
		t.Error("merged after CI failed on the resynced tree")
	}
}

func TestLandReportsAConflict(t *testing.T) {
	w := newWorld(t)
	// Both branches touch the same file differently.
	writeAndCommit(t, w.checkout, "shared.txt", "integration side")
	run(t, w.checkout, "push", "origin", "preview")
	writeAndCommit(t, w.worktree, "shared.txt", "feature side")

	f := forge.NewFake()
	f.ChecksBy[7] = greenChecks()
	l := lander(w, f)
	l.Wait = func(context.Context, int) ([]forge.Check, error) { return greenChecks(), nil }

	_, err := l.Land(t.Context(), 7, w.worktree)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if len(f.Merged) != 0 {
		t.Error("merged despite a conflict")
	}
	// The worktree must be usable afterwards, not left mid-merge.
	if status := run(t, w.worktree, "status", "--porcelain"); status != "" {
		t.Errorf("worktree left dirty after the conflict:\n%s", status)
	}
}

func TestLandOpensTheStandingPRWhenMissing(t *testing.T) {
	w := newWorld(t)
	f := forge.NewFake()
	f.ChecksBy[7] = greenChecks()
	// No standing PR.

	res, err := lander(w, f).Land(t.Context(), 7, w.worktree)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if !res.OpenedStanding {
		t.Error("OpenedStanding = false; the integration branch is left ungated")
	}
}

func TestLandLeavesAnExistingStandingPRAlone(t *testing.T) {
	w := newWorld(t)
	f := forge.NewFake()
	f.ChecksBy[7] = greenChecks()
	f.Standing = []int{238}

	res, err := lander(w, f).Land(t.Context(), 7, w.worktree)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if res.OpenedStanding {
		t.Error("opened a second standing pull request")
	}
	if res.StandingPR != 238 {
		t.Errorf("StandingPR = %d, want 238", res.StandingPR)
	}
}

func TestLandSkipsStandingPRForSingleBranchProjects(t *testing.T) {
	w := newWorld(t)
	f := forge.NewFake()
	f.ChecksBy[7] = greenChecks()

	l := lander(w, f)
	l.Cfg.Branches = config.Branches{Base: "preview", Integration: "preview"}

	res, err := l.Land(t.Context(), 7, w.worktree)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if res.OpenedStanding {
		t.Error("opened a standing pull request for a single-branch project")
	}
}

func TestLandRunsTheInstallHookAfterFastForward(t *testing.T) {
	w := newWorld(t)
	f := forge.NewFake()
	f.ChecksBy[7] = greenChecks()
	f.Standing = []int{238}

	ran := 0
	l := lander(w, f)
	l.Cfg.Hooks.Install = "true"
	l.RunHook = func(context.Context, string) error { ran++; return nil }

	if _, err := l.Land(t.Context(), 7, w.worktree); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if ran != 1 {
		t.Errorf("install hook ran %d times, want 1 — a new dependency is unlinked without it", ran)
	}
}

func TestLandReportsALocalDivergence(t *testing.T) {
	w := newWorld(t)
	// Both sides must move. A checkout that is merely *ahead* of origin still
	// fast-forwards (git reports "Already up to date" and exits 0); only a
	// genuine fork refuses.
	run(t, w.worktree, "push", "origin", "HEAD:preview")
	run(t, w.checkout, "commit", "--allow-empty", "-m", "local only")

	f := forge.NewFake()
	f.ChecksBy[7] = greenChecks()
	f.Standing = []int{238}

	_, err := lander(w, f).Land(t.Context(), 7, w.worktree)
	if !errors.Is(err, ErrLocalDiverged) {
		t.Fatalf("err = %v, want ErrLocalDiverged", err)
	}
	// The merge itself did happen; only the local sync failed.
	if len(f.Merged) != 1 {
		t.Errorf("Merged = %v, want the merge to have gone through before the sync failed", f.Merged)
	}
}

func writeAndCommit(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := writeFile(path, body); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	run(t, dir, "add", name)
	run(t, dir, "commit", "-m", "touch "+name)
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}

// GitHub closes an issue only when a pull request merges into the default
// branch. The agent's pull request merges into the integration branch, so its
// `Closes #N` does nothing — without carrying it forward the work ships and the
// issue stays open.
func TestLandCarriesClosingRefsToTheStandingPR(t *testing.T) {
	w := newWorld(t)
	f := forge.NewFake()
	f.ChecksBy[7] = greenChecks()
	f.Standing = []int{238}
	f.Bodies[7] = "## Summary\n\nfixed the thing\n\nCloses #192"
	f.Bodies[238] = "preview → main"

	res, err := lander(w, f).Land(t.Context(), 7, w.worktree)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(res.Closes) != 1 || res.Closes[0] != 192 {
		t.Errorf("Result.Closes = %v, want [192]", res.Closes)
	}
	if !strings.Contains(f.Bodies[238], "Fixes #192") {
		t.Errorf("standing body = %q, want it to close #192 on merge", f.Bodies[238])
	}
	if !strings.Contains(f.Bodies[238], "preview → main") {
		t.Error("the existing standing body was lost")
	}
}

func TestLandAccumulatesClosingRefsAcrossLandings(t *testing.T) {
	w := newWorld(t)
	f := forge.NewFake()
	f.Standing = []int{238}
	f.Bodies[238] = "preview → main"

	for _, tc := range []struct {
		pr    int
		issue int
	}{{7, 192}, {8, 193}} {
		f.ChecksBy[tc.pr] = greenChecks()
		f.Bodies[tc.pr] = fmt.Sprintf("Closes #%d", tc.issue)
		if _, err := lander(w, f).Land(t.Context(), tc.pr, w.worktree); err != nil {
			t.Fatalf("Land #%d: %v", tc.pr, err)
		}
	}
	refs := forge.ClosingRefs(f.Bodies[238])
	if len(refs) != 2 || refs[0] != 192 || refs[1] != 193 {
		t.Errorf("standing refs = %v, want [192 193]", refs)
	}
}

func TestLandWithNoClosingRefLeavesTheStandingBodyAlone(t *testing.T) {
	w := newWorld(t)
	f := forge.NewFake()
	f.ChecksBy[7] = greenChecks()
	f.Standing = []int{238}
	f.Bodies[7] = "## Summary\n\nno issue reference here"
	f.Bodies[238] = "preview → main"

	if _, err := lander(w, f).Land(t.Context(), 7, w.worktree); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if f.Bodies[238] != "preview → main" {
		t.Errorf("standing body = %q, want it untouched", f.Bodies[238])
	}
}

// A newly opened standing pull request must carry the references too.
func TestLandOpensStandingPRWithClosingRefs(t *testing.T) {
	w := newWorld(t)
	f := forge.NewFake()
	f.ChecksBy[7] = greenChecks()
	f.Bodies[7] = "Closes #192"

	res, err := lander(w, f).Land(t.Context(), 7, w.worktree)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if !res.OpenedStanding {
		t.Fatal("no standing pull request was opened")
	}
	if !strings.Contains(f.Bodies[res.StandingPR], "Fixes #192") {
		t.Errorf("new standing body = %q, want it to close #192", f.Bodies[res.StandingPR])
	}
}
