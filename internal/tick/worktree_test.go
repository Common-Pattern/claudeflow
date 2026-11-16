package tick

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/git"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

// engineWithRepo is newEngine over a real repository with one commit on main.
func engineWithRepo(t *testing.T) Engine {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	e, _ := newEngine(t)
	root := t.TempDir()
	e.Repo = git.New(root)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "claudeflow@example.invalid"},
		{"config", "user.name", "claudeflow test"},
		{"commit", "-q", "--allow-empty", "-m", "root"},
	} {
		if _, err := e.Repo.Run(t.Context(), args...); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}
	e.Cfg.Paths.Root = root
	e.Cfg.Paths.Worktrees = filepath.Join(t.TempDir(), "worktrees")
	return e
}

// A run that stopped to ask a question leaves its worktree on its slot. When a
// comment re-queues it onto another slot, the branch is still checked out in
// the old worktree, and adding a new one failed with "branch is checked out in
// a worktree". The old worktree is moved to the new slot's path instead, with
// whatever it left uncommitted.
func TestRequeuedRunOnAnotherSlotMovesItsWorktree(t *testing.T) {
	e := engineWithRepo(t)
	ctx := t.Context()
	branch := e.Cfg.Branches.Prefix + "build-351"
	old := filepath.Join(e.Cfg.Paths.Worktrees, "build-351-slot2")
	if err := e.Repo.AddWorktree(ctx, old, branch, "main"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(old, "wip.txt"), []byte("draft"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The earlier run's record is gone: it finished and was reaped.

	next := filepath.Join(e.Cfg.Paths.Worktrees, "build-351-slot1")
	if err := e.ensureWorktree(ctx, next, branch); err != nil {
		t.Fatalf("ensureWorktree on a new slot: %v", err)
	}
	w, ok, err := e.Repo.WorktreeOnBranch(ctx, branch)
	if err != nil || !ok || !git.SamePath(w.Path, next) {
		t.Fatalf("branch's worktree = %v, %v, %v; want it at %s", w, ok, err, next)
	}
	if b, err := os.ReadFile(filepath.Join(next, "wip.txt")); err != nil || string(b) != "draft" {
		t.Errorf("uncommitted file = %q, %v; want it carried to the new slot", b, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old slot's worktree still present: %v", err)
	}
}

// A branch whose worktree a live run is using is not taken from under it.
func TestWorktreeHeldByALiveRunIsNotMoved(t *testing.T) {
	e := engineWithRepo(t)
	ctx := t.Context()
	branch := e.Cfg.Branches.Prefix + "build-351"
	old := filepath.Join(e.Cfg.Paths.Worktrees, "build-351-slot2")
	if err := e.Repo.AddWorktree(ctx, old, branch, "main"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if err := e.Store.SaveRun(state.Run{
		Kind: state.KindBuild, Ref: 351, Slot: 2, Worktree: old,
		Phase: state.PhaseAwaitingCI, Started: time.Now(),
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	next := filepath.Join(e.Cfg.Paths.Worktrees, "build-351-slot1")
	if err := e.ensureWorktree(ctx, next, branch); err == nil {
		t.Fatalf("ensureWorktree took the worktree of a live run")
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("live run's worktree disturbed: %v", err)
	}
}

// The ordinary first run on an issue still gets a fresh worktree.
func TestFirstRunAddsAWorktree(t *testing.T) {
	e := engineWithRepo(t)
	ctx := t.Context()
	e.Cfg.Branches.Integration = "main"
	// AddWorktree starts a new branch at origin/<integration>; stand a remote
	// up so that ref exists.
	if _, err := e.Repo.Run(ctx, "remote", "add", "origin", e.Cfg.Paths.Root); err != nil {
		t.Fatalf("remote add: %v", err)
	}
	if err := e.Repo.Fetch(ctx); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	branch := e.Cfg.Branches.Prefix + "build-9"
	path := filepath.Join(e.Cfg.Paths.Worktrees, "build-9-slot1")
	if err := e.ensureWorktree(ctx, path, branch); err != nil {
		t.Fatalf("ensureWorktree: %v", err)
	}
	if w, ok, _ := e.Repo.WorktreeOnBranch(ctx, branch); !ok || !git.SamePath(w.Path, path) {
		t.Fatalf("branch's worktree = %v, %v; want %s", w, ok, path)
	}
}
