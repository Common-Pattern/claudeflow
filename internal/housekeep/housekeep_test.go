package housekeep

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/git"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

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

type env struct {
	checkout string
	store    *state.Store
	keeper   Keeper
}

// newEnv builds an origin plus a checkout on "preview". Nothing is alive.
func newEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	run(t, root, "init", "--bare", "-b", "preview", origin)
	checkout := filepath.Join(root, "checkout")
	run(t, root, "clone", origin, checkout)
	run(t, checkout, "commit", "--allow-empty", "-m", "base")
	run(t, checkout, "push", "-u", "origin", "preview")

	st, err := state.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	cfg := config.Default()
	cfg.Repo, cfg.User = "o/n", "u"
	cfg.Branches = config.Branches{Base: "main", Integration: "preview", Prefix: "claude/"}

	e := &env{checkout: checkout, store: st}
	e.keeper = Keeper{
		Cfg: cfg, Repo: git.New(checkout), Store: st,
		Alive: func(int) bool { return false },
		Log:   func(string, ...any) {},
	}
	return e
}

// addWorktree creates a worktree on branch, optionally merging it into the
// integration branch so it counts as reclaimable.
func (e *env) addWorktree(t *testing.T, name, branch string, merged bool) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(e.checkout), name)
	run(t, e.checkout, "worktree", "add", "-b", branch, path, "preview")
	run(t, path, "commit", "--allow-empty", "-m", "work on "+branch)
	if merged {
		run(t, e.checkout, "merge", "--no-ff", "--no-edit", branch)
		run(t, e.checkout, "push", "origin", "preview")
	}
	run(t, e.checkout, "fetch", "origin")
	return path
}

func TestReclaimsAMergedWorktree(t *testing.T) {
	e := newEnv(t)
	path := e.addWorktree(t, "build-1-slot1", "claude/issue-1", true)

	rep, err := e.keeper.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Contains(rep.RemovedWorktrees, path) {
		t.Fatalf("RemovedWorktrees = %v, want it to contain %s", rep.RemovedWorktrees, path)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("worktree directory still exists")
	}
	if out := run(t, e.checkout, "branch", "--list", "claude/issue-1"); out != "" {
		t.Errorf("branch survived: %q", out)
	}
}

// The safety rule the whole package rests on.
func TestKeepsAnUnmergedWorktree(t *testing.T) {
	e := newEnv(t)
	path := e.addWorktree(t, "build-2-slot1", "claude/issue-2", false)

	rep, err := e.keeper.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.RemovedWorktrees) != 0 {
		t.Errorf("removed %v; unmerged work must be kept", rep.RemovedWorktrees)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("unmerged worktree was removed: %v", err)
	}
}

// A worktree on a branch the tool did not create is someone else's.
func TestKeepsAMergedWorktreeWithoutThePrefix(t *testing.T) {
	e := newEnv(t)
	path := e.addWorktree(t, "hand-1", "hand/experiment", true)

	rep, err := e.keeper.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.RemovedWorktrees) != 0 {
		t.Errorf("removed %v; only prefixed branches are ours", rep.RemovedWorktrees)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("someone else's worktree was removed: %v", err)
	}
}

// A branch can be merged while a run is still pushing a follow-up to it.
func TestKeepsAWorktreeWithALiveRun(t *testing.T) {
	e := newEnv(t)
	path := e.addWorktree(t, "build-3-slot2", "claude/issue-3", true)
	if err := e.store.SaveRun(state.Run{
		Kind: state.KindBuild, Ref: 3, Slot: 2, PID: 1234,
		Worktree: path, Started: time.Now(),
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	e.keeper.Alive = func(int) bool { return true }

	rep, err := e.keeper.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.RemovedWorktrees) != 0 {
		t.Errorf("removed %v while a run is live in it", rep.RemovedWorktrees)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("live worktree was removed: %v", err)
	}
}

func TestNeverRemovesTheMainCheckout(t *testing.T) {
	e := newEnv(t)
	if _, err := e.keeper.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.checkout, ".git")); err != nil {
		t.Fatalf("the main checkout was damaged: %v", err)
	}
}

// Teardown must happen before removal: afterwards the project's own teardown
// script has gone with the worktree.
func TestRunsTeardownBeforeRemoval(t *testing.T) {
	e := newEnv(t)
	path := e.addWorktree(t, "build-4-slot3", "claude/issue-4", true)

	var sawSlot int
	var existedAtTeardown bool
	e.keeper.Cfg.Hooks.EnvDown = "true"
	e.keeper.RunHook = func(_ context.Context, _ string, slot int, wt string) error {
		sawSlot = slot
		_, err := os.Stat(wt)
		existedAtTeardown = err == nil
		return nil
	}

	if _, err := e.keeper.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sawSlot != 3 {
		t.Errorf("teardown saw slot %d, want 3 recovered from the worktree name", sawSlot)
	}
	if !existedAtTeardown {
		t.Error("teardown ran after the worktree was removed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("worktree survived")
	}
}

// A teardown that fails must not strand the worktree forever.
func TestTeardownFailureStillReclaims(t *testing.T) {
	e := newEnv(t)
	e.addWorktree(t, "build-5-slot1", "claude/issue-5", true)
	e.keeper.Cfg.Hooks.EnvDown = "false"
	e.keeper.RunHook = func(context.Context, string, int, string) error {
		return context.DeadlineExceeded
	}

	rep, err := e.keeper.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.RemovedWorktrees) != 1 {
		t.Errorf("RemovedWorktrees = %v, want the worktree reclaimed despite teardown failing", rep.RemovedWorktrees)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	e := newEnv(t)
	path := e.addWorktree(t, "build-6-slot1", "claude/issue-6", true)
	e.keeper.DryRun = true

	rep, err := e.keeper.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.RemovedWorktrees) != 1 {
		t.Errorf("RemovedWorktrees = %v, want the worktree reported", rep.RemovedWorktrees)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("dry run removed the worktree")
	}
}

func TestClearsDeadRunRecords(t *testing.T) {
	e := newEnv(t)
	if err := e.store.SaveRun(state.Run{Kind: state.KindPlan, Ref: 9, PID: 4242, Started: time.Now()}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	rep, err := e.keeper.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Contains(rep.ClearedRuns, "plan-9") {
		t.Errorf("ClearedRuns = %v, want plan-9", rep.ClearedRuns)
	}
	runs, _ := e.store.Runs()
	if len(runs) != 0 {
		t.Errorf("records survived: %v", runs)
	}
}

func TestKeepsLiveRunRecords(t *testing.T) {
	e := newEnv(t)
	if err := e.store.SaveRun(state.Run{Kind: state.KindPlan, Ref: 9, PID: 4242, Started: time.Now()}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	e.keeper.Alive = func(int) bool { return true }

	rep, err := e.keeper.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.ClearedRuns) != 0 {
		t.Errorf("ClearedRuns = %v, want none", rep.ClearedRuns)
	}
}

func TestDeletesOldLogsOnly(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.keeper.Now = func() time.Time { return now }
	e.keeper.LogRetention = 14 * 24 * time.Hour

	old := filepath.Join(e.store.LogDir(), "old.log")
	recent := filepath.Join(e.store.LogDir(), "recent.log")
	for _, p := range []string{old, recent} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	stale := now.Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	rep, err := e.keeper.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.DeletedLogs != 1 {
		t.Errorf("DeletedLogs = %d, want 1", rep.DeletedLogs)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old log survived")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("recent log was deleted")
	}
}

func TestZeroRetentionKeepsEveryLog(t *testing.T) {
	e := newEnv(t)
	p := filepath.Join(e.store.LogDir(), "any.log")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	old := time.Now().Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	rep, err := e.keeper.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.DeletedLogs != 0 {
		t.Errorf("DeletedLogs = %d, want 0 with retention unset", rep.DeletedLogs)
	}
}

func TestSlotOfWorktree(t *testing.T) {
	for _, tc := range []struct {
		path string
		want int
		ok   bool
	}{
		{"/x/build-12-slot3", 3, true},
		{"/x/review-238-slot1", 1, true},
		{"/x/plan-4", 0, false},
		{"/x/nothing", 0, false},
	} {
		got, ok := slotOfWorktree(tc.path)
		if got != tc.want || ok != tc.ok {
			t.Errorf("slotOfWorktree(%q) = (%d, %v), want (%d, %v)", tc.path, got, ok, tc.want, tc.ok)
		}
	}
}
