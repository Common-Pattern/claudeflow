package tick

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/git"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

// landingWorld is an engine over a real checkout of a bare origin, with a run
// parked on a green pull request whose branch is in its own worktree. The
// integration branch is "preview".
func landingWorld(t *testing.T, attempts int) (Engine, *git.Repo, string) {
	t.Helper()
	e := engineWithRepo(t) // also neutralises the operator's git config
	ctx := t.Context()

	origin := filepath.Join(t.TempDir(), "origin.git")
	must := func(r *git.Repo, args ...string) {
		t.Helper()
		if _, err := r.Run(ctx, args...); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}
	must(git.New(filepath.Dir(origin)), "init", "-q", "--bare", "-b", "preview", origin)
	must(e.Repo, "branch", "-M", "preview")
	must(e.Repo, "remote", "add", "origin", origin)
	must(e.Repo, "push", "-q", "-u", "origin", "preview")

	e.Cfg.Branches.Integration = "preview"
	e.Cfg.Branches.Base = "main"
	e.Cfg.RequiredChecks = nil
	// No container engine in a test: the slot allocator reads every slot as
	// unavailable, which is the "no slot free" path and starts no agent.
	e.Cfg.Compose.Bin = []string{filepath.Join(t.TempDir(), "no-such-compose")}

	branch := "claude/build-7"
	wtPath := filepath.Join(e.Cfg.Paths.Worktrees, "build-7-slot1")
	if err := e.Repo.AddWorktree(ctx, wtPath, branch, "preview"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	wt := git.New(wtPath)
	write := func(r *git.Repo, dir, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		must(r, "add", "shared.txt")
		must(r, "commit", "-q", "-m", content)
	}
	write(wt, wtPath, "feature side")
	must(wt, "push", "-q", "-u", "origin", branch)

	if err := e.Store.SaveRun(state.Run{
		Kind: state.KindBuild, Ref: 7, PR: 200, Phase: state.PhaseAwaitingCI,
		Attempts: attempts, Branch: branch, Worktree: wtPath, Started: time.Now(),
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	return e, e.Repo, wtPath
}

func forgeOf(e Engine) *forge.Fake { return e.Client.(*forge.Fake) }

// moveIntegration lands someone else's commit on preview, touching shared.txt
// when conflict is true and another file otherwise.
func moveIntegration(t *testing.T, checkout *git.Repo, conflict bool) {
	t.Helper()
	ctx := t.Context()
	name := "other.txt"
	if conflict {
		name = "shared.txt"
	}
	if err := os.WriteFile(filepath.Join(checkout.Root, name), []byte("integration side"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, args := range [][]string{{"add", name}, {"commit", "-q", "-m", "someone else landed"}, {"push", "-q", "origin", "preview"}} {
		if _, err := checkout.Run(ctx, args...); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}
}

// A green pull request that conflicts with the integration branch is not a
// failure for the operator to pick up. #347 on slotbooks was labelled blocked
// with "integration branch conflicts with this branch"; the run now stays
// parked and an agent is sent to merge and resolve, as for red checks.
func TestConflictOnLandingDoesNotBlockTheIssue(t *testing.T) {
	e, checkout, _ := landingWorld(t, 0)
	f := forgeOf(e)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	f.ChecksBy[200] = green()
	moveIntegration(t, checkout, true)

	if err := e.advanceAwaitingCI(t.Context()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 7)
	if slices.Contains(labels, e.Cfg.Labels.Blocked) {
		t.Errorf("labels = %v; a conflict blocked the issue instead of sending an agent", labels)
	}
	if len(f.Merged) != 0 {
		t.Errorf("merged %v despite the conflict", f.Merged)
	}
	runs, _ := e.Store.Runs()
	if len(runs) != 1 || runs[0].PR != 200 {
		t.Fatalf("runs = %v, want the run kept on #200 for a resolve run", runs)
	}
}

// After the attempt cap a conflict is handed to the operator, saying so.
func TestConflictOnLandingGivesUpAfterTheCap(t *testing.T) {
	e, checkout, _ := landingWorld(t, 3)
	e.Cfg.Limits.FixAttempts = 3
	f := forgeOf(e)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	f.ChecksBy[200] = green()
	moveIntegration(t, checkout, true)

	if err := e.advanceAwaitingCI(t.Context()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 7)
	if !slices.Contains(labels, e.Cfg.Labels.Blocked) {
		t.Errorf("labels = %v, want blocked after the cap", labels)
	}
	if len(f.Comments[7]) == 0 || !strings.Contains(f.Comments[7][0], "still conflicts with `preview`") {
		t.Errorf("comments = %q, want one saying it still conflicts", f.Comments[7])
	}
	if runs, _ := e.Store.Runs(); len(runs) != 0 {
		t.Errorf("runs = %v, want the record cleared", runs)
	}
}

// A branch that is merely behind is merged up and pushed, and the run goes on
// waiting for the new head's checks. It used to push the merge and then fail
// the landing with "no CI waiter is configured".
func TestBehindBranchIsResyncedAndWaitsAgain(t *testing.T) {
	e, checkout, wtPath := landingWorld(t, 0)
	f := forgeOf(e)
	f.AddIssue(7, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	f.ChecksBy[200] = green()
	moveIntegration(t, checkout, false)

	if err := e.advanceAwaitingCI(t.Context()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 7)
	if slices.Contains(labels, e.Cfg.Labels.Blocked) {
		t.Errorf("labels = %v; a clean resync blocked the issue", labels)
	}
	if len(f.Merged) != 0 {
		t.Errorf("merged %v before the resynced head had checks", f.Merged)
	}
	runs, _ := e.Store.Runs()
	if len(runs) != 1 || !runs[0].InPhase(state.PhaseAwaitingCI) {
		t.Fatalf("runs = %v, want the run still waiting on checks", runs)
	}
	wt := git.New(wtPath)
	if ok, err := wt.IsAncestor(t.Context(), "origin/preview", "origin/claude/build-7"); err != nil || !ok {
		t.Errorf("pushed branch does not contain preview: %v, %v", ok, err)
	}
}
