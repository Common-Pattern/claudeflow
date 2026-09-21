package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The tests drive a real git binary against repositories built in temporary
// directories. Nothing here reaches the network or a fixed path: a fake git
// would only prove that the fake matches the expectations, and every incident
// this package exists to prevent came from what git actually does.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// newRepo returns an initialised repository with one commit on main.
func newRepo(t *testing.T) *Repo {
	t.Helper()
	requireGit(t)
	// The operator's own git config must not reach these repositories: a
	// global commit.gpgsign, merge driver or hooksPath would change what the
	// tests are measuring.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	r := New(t.TempDir())
	mustRun(t, r, "init", "-q", "-b", "main")
	mustRun(t, r, "config", "user.email", "claudeflow@example.invalid")
	mustRun(t, r, "config", "user.name", "claudeflow test")
	mustRun(t, r, "config", "commit.gpgsign", "false")
	commit(t, r, "root")
	return r
}

func mustRun(t *testing.T, r *Repo, args ...string) string {
	t.Helper()
	out, err := r.Run(context.Background(), args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func commit(t *testing.T, r *Repo, message string) string {
	t.Helper()
	mustRun(t, r, "commit", "-q", "--allow-empty", "-m", message)
	return mustRun(t, r, "rev-parse", "HEAD")
}

func commitFile(t *testing.T, r *Repo, name, content, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.Root, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	mustRun(t, r, "add", name)
	mustRun(t, r, "commit", "-q", "-m", message)
	return mustRun(t, r, "rev-parse", "HEAD")
}

// commitOn makes a commit on a branch without leaving the current one checked
// out elsewhere, by using a throwaway worktree.
func commitOnBranch(t *testing.T, r *Repo, branch, startPoint, message string) string {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "scratch-"+strings.ReplaceAll(branch, "/", "-"))
	if err := r.AddWorktree(ctx, dir, branch, startPoint); err != nil {
		t.Fatalf("AddWorktree(%s): %v", branch, err)
	}
	sha := commit(t, New(dir), message)
	if err := r.RemoveWorktree(ctx, dir); err != nil {
		t.Fatalf("RemoveWorktree(%s): %v", dir, err)
	}
	return sha
}

func TestWorktreeAddListRemove(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	path := filepath.Join(t.TempDir(), "issue-7")

	if err := r.AddWorktree(ctx, path, "claude/issue-7", "main"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	worktrees, err := r.Worktrees(ctx)
	if err != nil {
		t.Fatalf("Worktrees: %v", err)
	}
	if len(worktrees) != 2 {
		t.Fatalf("Worktrees = %d entries, want 2: %+v", len(worktrees), worktrees)
	}
	added := worktrees[1]
	if !samePath(added.Path, path) {
		t.Errorf("worktree path = %q, want %q", added.Path, path)
	}
	if added.Branch != "claude/issue-7" {
		t.Errorf("worktree branch = %q, want claude/issue-7 — the refs/heads/ prefix must be stripped", added.Branch)
	}
	if added.Detached {
		t.Error("worktree reported detached while on a branch")
	}

	if err := r.RemoveWorktree(ctx, path); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	worktrees, err = r.Worktrees(ctx)
	if err != nil {
		t.Fatalf("Worktrees after remove: %v", err)
	}
	if len(worktrees) != 1 {
		t.Fatalf("Worktrees after remove = %d entries, want 1: %+v", len(worktrees), worktrees)
	}
	// Removal must leave the branch alone: the pull request it belongs to is
	// still open, and a retry reuses it.
	exists, err := r.BranchExists(ctx, "claude/issue-7")
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if !exists {
		t.Error("RemoveWorktree deleted the branch; it must remove only the checkout")
	}
}

func TestAddWorktreeReusesExistingBranch(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	second := commit(t, r, "second")
	mustRun(t, r, "branch", "claude/existing", second)
	mustRun(t, r, "checkout", "-q", "-b", "later")
	first := commit(t, r, "later")

	path := filepath.Join(t.TempDir(), "reuse")
	if err := r.AddWorktree(ctx, path, "claude/existing", first); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	head, err := New(path).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA in worktree: %v", err)
	}
	if head != second {
		t.Errorf("worktree HEAD = %s, want the existing branch tip %s — the start point must be ignored when the branch exists", head, second)
	}
}

func TestAddWorktreeRejectsOccupiedPath(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	path := filepath.Join(t.TempDir(), "taken")
	if err := r.AddWorktree(ctx, path, "claude/one", "main"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	err := r.AddWorktree(ctx, path, "claude/two", "main")
	if !errors.Is(err, ErrWorktreeExists) {
		t.Fatalf("AddWorktree on an occupied path = %v, want ErrWorktreeExists", err)
	}
}

func TestAddWorktreeRejectsBranchCheckedOutElsewhere(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	first := filepath.Join(t.TempDir(), "first")
	if err := r.AddWorktree(ctx, first, "claude/shared", "main"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	err := r.AddWorktree(ctx, filepath.Join(t.TempDir(), "second"), "claude/shared", "main")
	if !errors.Is(err, ErrBranchCheckedOut) {
		t.Fatalf("AddWorktree on a branch already checked out = %v, want ErrBranchCheckedOut", err)
	}
}

func TestRemoveWorktreeUnknownPathSucceeds(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	if err := r.RemoveWorktree(ctx, filepath.Join(t.TempDir(), "never-existed")); err != nil {
		t.Fatalf("RemoveWorktree on an unregistered path = %v, want nil", err)
	}
}

func TestPruneWorktrees(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	path := filepath.Join(t.TempDir(), "vanishing")
	if err := r.AddWorktree(ctx, path, "claude/vanishing", "main"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove worktree directory: %v", err)
	}
	if err := r.PruneWorktrees(ctx); err != nil {
		t.Fatalf("PruneWorktrees: %v", err)
	}
	worktrees, err := r.Worktrees(ctx)
	if err != nil {
		t.Fatalf("Worktrees: %v", err)
	}
	for _, w := range worktrees {
		if samePath(w.Path, path) {
			t.Fatalf("pruned worktree %s still registered", path)
		}
	}
}

func TestParseWorktreeList(t *testing.T) {
	out := strings.Join([]string{
		"worktree /repo",
		"HEAD 1111111111111111111111111111111111111111",
		"branch refs/heads/main",
		"",
		"worktree /repo/.claudeflow/worktrees/issue-12",
		"HEAD 2222222222222222222222222222222222222222",
		"branch refs/heads/claude/issue-12",
		"",
		"worktree /repo/.claudeflow/worktrees/loose",
		"HEAD 3333333333333333333333333333333333333333",
		"detached",
		"",
		"worktree /repo/.claudeflow/worktrees/stale",
		"HEAD 4444444444444444444444444444444444444444",
		"branch refs/heads/claude/stale",
		"prunable gitdir file points to non-existent location",
		"",
	}, "\n")

	got := parseWorktreeList(out)
	want := []Worktree{
		{Path: "/repo", Branch: "main"},
		{Path: "/repo/.claudeflow/worktrees/issue-12", Branch: "claude/issue-12"},
		{Path: "/repo/.claudeflow/worktrees/loose", Detached: true},
		{Path: "/repo/.claudeflow/worktrees/stale", Branch: "claude/stale"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d worktrees, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("worktree %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseWorktreeListBareRepository(t *testing.T) {
	// A bare main worktree carries neither a branch nor a detached line.
	out := "worktree /repo.git\nbare\n\n"
	got := parseWorktreeList(out)
	if len(got) != 1 {
		t.Fatalf("parsed %d worktrees, want 1: %+v", len(got), got)
	}
	if got[0].Branch != "" || got[0].Detached {
		t.Errorf("bare worktree = %+v, want an empty branch and Detached false", got[0])
	}
}

func TestWorktreesReportsDetached(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	path := filepath.Join(t.TempDir(), "detached")
	mustRun(t, r, "worktree", "add", "--detach", path, "HEAD")

	worktrees, err := r.Worktrees(ctx)
	if err != nil {
		t.Fatalf("Worktrees: %v", err)
	}
	var found *Worktree
	for i := range worktrees {
		if samePath(worktrees[i].Path, path) {
			found = &worktrees[i]
		}
	}
	if found == nil {
		t.Fatalf("detached worktree missing from %+v", worktrees)
	}
	if !found.Detached || found.Branch != "" {
		t.Errorf("detached worktree = %+v, want Detached true and no branch", *found)
	}
}

func TestIsAncestor(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	base := mustRun(t, r, "rev-parse", "HEAD")
	tip := commit(t, r, "ahead")

	yes, err := r.IsAncestor(ctx, base, tip)
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}
	if !yes {
		t.Error("IsAncestor(base, tip) = false, want true")
	}
	no, err := r.IsAncestor(ctx, tip, base)
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}
	if no {
		t.Error("IsAncestor(tip, base) = true, want false")
	}
}

func TestIsAncestorUnknownRefErrors(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	// A missing ref must not read as "not an ancestor": that answer would send
	// a caller on to delete work it could not find.
	if _, err := r.IsAncestor(ctx, "no-such-ref", "main"); err == nil {
		t.Fatal("IsAncestor with an unknown ref returned no error")
	}
}

func TestCommitsBetween(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	base := mustRun(t, r, "rev-parse", "HEAD")
	commit(t, r, "one")
	commit(t, r, "two")

	n, err := r.CommitsBetween(ctx, base, "HEAD")
	if err != nil {
		t.Fatalf("CommitsBetween: %v", err)
	}
	if n != 2 {
		t.Errorf("CommitsBetween(base, HEAD) = %d, want 2", n)
	}
	n, err = r.CommitsBetween(ctx, "HEAD", base)
	if err != nil {
		t.Fatalf("CommitsBetween: %v", err)
	}
	if n != 0 {
		t.Errorf("CommitsBetween(HEAD, base) = %d, want 0", n)
	}
}

func TestBranchExists(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	mustRun(t, r, "branch", "claude/present")

	present, err := r.BranchExists(ctx, "claude/present")
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if !present {
		t.Error("BranchExists on an existing branch = false")
	}
	absent, err := r.BranchExists(ctx, "claude/absent")
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if absent {
		t.Error("BranchExists on a missing branch = true")
	}
}

func TestHeadSHA(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	want := commit(t, r, "head")

	got, err := r.HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if got != want {
		t.Errorf("HeadSHA = %q, want %q", got, want)
	}
}

func TestMergeConflictAbortsAndLeavesACleanIndex(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	commitFile(t, r, "shared.txt", "base\n", "base")

	mustRun(t, r, "checkout", "-q", "-b", "claude/theirs")
	commitFile(t, r, "shared.txt", "theirs\n", "theirs")
	mustRun(t, r, "checkout", "-q", "main")
	commitFile(t, r, "shared.txt", "ours\n", "ours")

	err := r.Merge(ctx, "claude/theirs")
	if !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("Merge on conflicting changes = %v, want ErrMergeConflict", err)
	}

	// A conflicted index left behind wedges the checkout for every later run,
	// which is why the abort is part of Merge rather than the caller's job.
	if status := mustRun(t, r, "status", "--porcelain"); status != "" {
		t.Errorf("working tree after a conflicted merge:\n%s\nwant it clean", status)
	}
	if _, err := r.Run(ctx, "rev-parse", "--verify", "--quiet", "MERGE_HEAD"); err == nil {
		t.Error("MERGE_HEAD still set after a conflicted merge")
	}

	// And the repository must still be usable: the next merge is the one that
	// silently failed before the abort was added.
	mustRun(t, r, "checkout", "-q", "-b", "claude/clean")
	commitFile(t, r, "other.txt", "fine\n", "unrelated")
	mustRun(t, r, "checkout", "-q", "main")
	if err := r.Merge(ctx, "claude/clean"); err != nil {
		t.Fatalf("Merge after an aborted merge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Root, "other.txt")); err != nil {
		t.Errorf("merged file missing: %v", err)
	}
}

func TestMergeUnknownRefIsNotAConflict(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	err := r.Merge(ctx, "no-such-branch")
	if err == nil {
		t.Fatal("Merge of an unknown ref returned no error")
	}
	if errors.Is(err, ErrMergeConflict) {
		t.Fatalf("Merge of an unknown ref = %v, want a plain failure rather than ErrMergeConflict", err)
	}
}

func TestFastForward(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	ahead := commitOnBranch(t, r, "claude/ahead", "main", "ahead")

	if err := r.FastForward(ctx, "claude/ahead"); err != nil {
		t.Fatalf("FastForward: %v", err)
	}
	head, err := r.HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if head != ahead {
		t.Errorf("HEAD after FastForward = %s, want %s", head, ahead)
	}
	// An already-contained ref is not a refusal, and a caller must be able to
	// tell that apart from divergence.
	if err := r.FastForward(ctx, "claude/ahead"); err != nil {
		t.Errorf("FastForward when already up to date = %v, want nil", err)
	}
}

func TestFastForwardRefusesDivergence(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	commitOnBranch(t, r, "claude/theirs", "main", "theirs")
	commit(t, r, "ours")

	err := r.FastForward(ctx, "claude/theirs")
	if !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("FastForward onto a diverged branch = %v, want ErrNotFastForward", err)
	}
}

func TestDeleteBranchAfterWorktreeRemoval(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	path := filepath.Join(t.TempDir(), "issue-3")
	if err := r.AddWorktree(ctx, path, "claude/issue-3", "main"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}

	// Deleting first is the ordering bug; it must be reported as one rather
	// than as an opaque git failure.
	err := r.DeleteBranch(ctx, "claude/issue-3")
	if !errors.Is(err, ErrBranchCheckedOut) {
		t.Fatalf("DeleteBranch while checked out = %v, want ErrBranchCheckedOut", err)
	}

	if err := r.RemoveWorktree(ctx, path); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if err := r.DeleteBranch(ctx, "claude/issue-3"); err != nil {
		t.Fatalf("DeleteBranch after removal: %v", err)
	}
	exists, err := r.BranchExists(ctx, "claude/issue-3")
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if exists {
		t.Error("branch still present after DeleteBranch")
	}
	if err := r.DeleteBranch(ctx, "claude/issue-3"); err != nil {
		t.Errorf("DeleteBranch on a missing branch = %v, want nil", err)
	}
}

func TestReclaimableWorktrees(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	dirs := t.TempDir()

	add := func(name, branch string) string {
		t.Helper()
		path := filepath.Join(dirs, name)
		if err := r.AddWorktree(ctx, path, branch, "main"); err != nil {
			t.Fatalf("AddWorktree(%s): %v", branch, err)
		}
		commit(t, New(path), "work on "+branch)
		return path
	}

	mergedPrefixed := add("merged", "claude/merged")
	add("live", "claude/live")
	add("hand", "hand/merged")
	detached := filepath.Join(dirs, "detached")
	mustRun(t, r, "worktree", "add", "--detach", detached, "HEAD")

	// Both merges land on main, so only the prefix separates the two.
	if err := r.Merge(ctx, "claude/merged"); err != nil {
		t.Fatalf("Merge claude/merged: %v", err)
	}
	if err := r.Merge(ctx, "hand/merged"); err != nil {
		t.Fatalf("Merge hand/merged: %v", err)
	}

	worktrees, err := r.Worktrees(ctx)
	if err != nil {
		t.Fatalf("Worktrees: %v", err)
	}
	got, err := ReclaimableWorktrees(ctx, r, worktrees, "claude/", "main")
	if err != nil {
		t.Fatalf("ReclaimableWorktrees: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ReclaimableWorktrees = %+v, want only the merged claude/ worktree", got)
	}
	if !samePath(got[0].Path, mergedPrefixed) {
		t.Errorf("reclaimable worktree = %q, want %q", got[0].Path, mergedPrefixed)
	}
}

func TestReclaimableWorktreesSkipsTheMainCheckout(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	mustRun(t, r, "checkout", "-q", "-b", "claude/main-checkout")
	mustRun(t, r, "branch", "-f", "integration", "HEAD")

	worktrees, err := r.Worktrees(ctx)
	if err != nil {
		t.Fatalf("Worktrees: %v", err)
	}
	got, err := ReclaimableWorktrees(ctx, r, worktrees, "claude/", "integration")
	if err != nil {
		t.Fatalf("ReclaimableWorktrees: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ReclaimableWorktrees = %+v, want none — the main checkout is never reclaimable", got)
	}
}

func TestFetchAndPush(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)

	remote := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatalf("create remote dir: %v", err)
	}
	mustRun(t, New(remote), "init", "-q", "--bare")
	mustRun(t, r, "remote", "add", "origin", remote)

	if err := r.Push(ctx, "origin", "main"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if err := r.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	head, err := r.HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if tracked := mustRun(t, r, "rev-parse", "origin/main"); tracked != head {
		t.Errorf("origin/main = %s, want %s", tracked, head)
	}
}

func TestFetchWithoutRemoteSucceeds(t *testing.T) {
	r := newRepo(t)
	if err := r.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch with no remote = %v, want nil", err)
	}
}

func TestRunReportsCommandAndStderr(t *testing.T) {
	r := newRepo(t)
	_, err := r.Run(context.Background(), "rev-parse", "--verify", "definitely-not-a-ref")
	if err == nil {
		t.Fatal("Run on a bad ref returned no error")
	}
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("Run error = %T, want *CommandError", err)
	}
	if cmdErr.ExitCode <= 0 {
		t.Errorf("ExitCode = %d, want a non-zero git exit status", cmdErr.ExitCode)
	}
	if !strings.Contains(err.Error(), "rev-parse --verify definitely-not-a-ref") {
		t.Errorf("error %q does not name the failing command", err)
	}
	if cmdErr.Stderr == "" {
		t.Error("CommandError carries no stderr")
	}
}
