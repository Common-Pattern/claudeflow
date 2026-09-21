// Package git drives a repository through the git command line.
//
// It shells out rather than using a library. go-git, the obvious alternative,
// has no real `git worktree` support, and a worktree per run is the whole
// mechanism claudeflow is built on: one checkout per issue, created and
// reclaimed without disturbing the operator's own working tree. Shelling out
// also means the repository behaves under claudeflow exactly as it does under
// the operator's hands — same config, same credential helper, same hooks.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Errors a caller is expected to branch on.
var (
	// ErrMergeConflict means a merge stopped on conflicting changes. The merge
	// has been aborted by the time it is returned, so the worktree is clean.
	ErrMergeConflict = errors.New("merge conflict")
	// ErrNotFastForward means the target holds commits the given ref does not,
	// so it cannot be advanced without a merge. Fast-forwarding a ref that is
	// already up to date is not this: it succeeds.
	ErrNotFastForward = errors.New("not a fast-forward")
	// ErrWorktreeExists means a worktree is already registered at that path.
	ErrWorktreeExists = errors.New("worktree already exists")
	// ErrBranchCheckedOut means the branch is checked out in some worktree, so
	// it cannot be deleted or checked out again. A caller seeing this from
	// DeleteBranch removed the branch before the worktree that held it.
	ErrBranchCheckedOut = errors.New("branch is checked out in a worktree")
)

// Repo is a git repository on disk.
type Repo struct {
	// Root is the repository the commands run in.
	Root string
	// Bin is the git executable. Empty means "git" on PATH.
	Bin string
}

// New returns a Repo rooted at the given checkout.
func New(root string) *Repo { return &Repo{Root: root} }

func (r *Repo) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	return "git"
}

// CommandError carries everything needed to locate a failing git invocation.
type CommandError struct {
	Args     []string
	Dir      string
	ExitCode int
	Stdout   string
	Stderr   string
	Err      error
}

func (e *CommandError) Error() string {
	msg := e.Stderr
	if msg == "" {
		msg = e.Stdout
	}
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git -C %s %s: exit %d: %s", e.Dir, strings.Join(e.Args, " "), e.ExitCode, msg)
}

// Unwrap returns the underlying exec error.
func (e *CommandError) Unwrap() error { return e.Err }

// message returns everything git said, in either stream. git reports merge
// conflicts and several other refusals on stdout, so matching stderr alone
// misses them.
func (e *CommandError) message() string {
	return strings.ToLower(e.Stderr + "\n" + e.Stdout)
}

// Run executes git in the repository and returns its trimmed standard output.
// Output is returned even on failure, because git reports some refusals there.
func (r *Repo) Run(ctx context.Context, args ...string) (string, error) {
	full := args
	if r.Root != "" {
		full = append([]string{"-C", r.Root}, args...)
	}
	cmd := exec.CommandContext(ctx, r.bin(), full...)
	// An unattended run must fail rather than block forever on a credential or
	// host-key prompt that nobody is there to answer.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err == nil {
		return out, nil
	}
	code := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	}
	return out, &CommandError{
		Args:     args,
		Dir:      r.Root,
		ExitCode: code,
		Stdout:   out,
		Stderr:   strings.TrimSpace(stderr.String()),
		Err:      err,
	}
}

// exitStatus reports a command's exit code, and whether the error was a plain
// non-zero exit rather than a failure to run git at all.
func exitStatus(err error) (int, bool) {
	var cmdErr *CommandError
	if errors.As(err, &cmdErr) && cmdErr.ExitCode >= 0 {
		return cmdErr.ExitCode, true
	}
	return -1, false
}

// said reports whether git's output contains needle, in either stream.
func said(err error, needle string) bool {
	var cmdErr *CommandError
	return errors.As(err, &cmdErr) && strings.Contains(cmdErr.message(), strings.ToLower(needle))
}

// Fetch updates remote-tracking refs and drops the ones that no longer exist
// upstream. A repository with no remote is a no-op, not an error.
func (r *Repo) Fetch(ctx context.Context) error {
	if _, err := r.Run(ctx, "fetch", "--prune", "--quiet"); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	return nil
}

// HeadSHA returns the commit HEAD points at.
func (r *Repo) HeadSHA(ctx context.Context) (string, error) {
	sha, err := r.Run(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("head sha: %w", err)
	}
	return sha, nil
}

// BranchExists reports whether a local branch of that name exists.
func (r *Repo) BranchExists(ctx context.Context, branch string) (bool, error) {
	_, err := r.Run(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	if code, ok := exitStatus(err); ok && code == 1 {
		return false, nil
	}
	return false, fmt.Errorf("branch exists %s: %w", branch, err)
}

// CommitsBetween counts the commits reachable from to but not from from.
func (r *Repo) CommitsBetween(ctx context.Context, from, to string) (int, error) {
	out, err := r.Run(ctx, "rev-list", "--count", from+".."+to)
	if err != nil {
		return 0, fmt.Errorf("count commits %s..%s: %w", from, to, err)
	}
	n, convErr := strconv.Atoi(out)
	if convErr != nil {
		return 0, fmt.Errorf("count commits %s..%s: unexpected output %q: %w", from, to, out, convErr)
	}
	return n, nil
}

// IsAncestor reports whether maybeAncestor is contained in of.
func (r *Repo) IsAncestor(ctx context.Context, maybeAncestor, of string) (bool, error) {
	_, err := r.Run(ctx, "merge-base", "--is-ancestor", maybeAncestor, of)
	if err == nil {
		return true, nil
	}
	if code, ok := exitStatus(err); ok && code == 1 {
		return false, nil
	}
	return false, fmt.Errorf("is-ancestor %s %s: %w", maybeAncestor, of, err)
}

// Worktree is one entry from the repository's worktree list.
type Worktree struct {
	// Path is the checkout's absolute path.
	Path string
	// Branch is the checked-out branch, without its refs/heads/ prefix. Empty
	// for a detached or bare worktree.
	Branch string
	// Detached means the worktree is on a commit rather than a branch.
	Detached bool
}

// Worktrees returns every worktree registered with the repository, the main
// checkout first.
func (r *Repo) Worktrees(ctx context.Context) ([]Worktree, error) {
	out, err := r.Run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}
	return parseWorktreeList(out), nil
}

// parseWorktreeList reads `git worktree list --porcelain`: blank-line separated
// records, each opening with `worktree <path>` and carrying at most one of
// `branch refs/heads/<name>` or `detached`. A bare or detached worktree has no
// branch line at all, so nothing here may assume one.
func parseWorktreeList(out string) []Worktree {
	var (
		list []Worktree
		cur  Worktree
		open bool
	)
	flush := func() {
		if open && cur.Path != "" {
			list = append(list, cur)
		}
		cur, open = Worktree{}, false
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			flush()
			cur.Path, open = value, true
		case "branch":
			cur.Branch = strings.TrimPrefix(value, "refs/heads/")
		case "detached":
			cur.Detached = true
		}
	}
	flush()
	return list
}

// AddWorktree creates a worktree at path on the given branch. An existing
// branch is checked out as it stands; otherwise it is created at startPoint,
// or at HEAD when startPoint is empty.
//
// Reusing the branch rather than recreating it is what lets a run be retried
// after its worktree was reclaimed: the commits already pushed stay reachable
// and the pull request stays open.
func (r *Repo) AddWorktree(ctx context.Context, path, branch, startPoint string) error {
	worktrees, err := r.Worktrees(ctx)
	if err != nil {
		return err
	}
	for _, w := range worktrees {
		if samePath(w.Path, path) {
			return fmt.Errorf("add worktree %s: %w", path, ErrWorktreeExists)
		}
	}
	exists, err := r.BranchExists(ctx, branch)
	if err != nil {
		return err
	}
	args := []string{"worktree", "add", path, branch}
	if !exists {
		if startPoint == "" {
			startPoint = "HEAD"
		}
		args = []string{"worktree", "add", "-b", branch, path, startPoint}
	}
	if _, err := r.Run(ctx, args...); err != nil {
		if said(err, "is already used by worktree") || said(err, "already checked out") {
			return fmt.Errorf("add worktree %s on %s: %w", path, branch, ErrBranchCheckedOut)
		}
		return fmt.Errorf("add worktree %s on %s: %w", path, branch, err)
	}
	return nil
}

// RemoveWorktree deletes a worktree and its registration, discarding whatever
// is uncommitted in it. A path that is not a registered worktree is already in
// the wanted state and reports success.
//
// It deletes no branch, local or remote. Order is not a preference here: a
// branch checked out in a worktree cannot be deleted, so the worktree goes
// first and the branch is the caller's separate decision afterwards.
func (r *Repo) RemoveWorktree(ctx context.Context, path string) error {
	if _, err := r.Run(ctx, "worktree", "remove", "--force", path); err != nil {
		known, listErr := r.Worktrees(ctx)
		if listErr != nil {
			return fmt.Errorf("remove worktree %s: %w", path, err)
		}
		for _, w := range known {
			if samePath(w.Path, path) {
				return fmt.Errorf("remove worktree %s: %w", path, err)
			}
		}
		return nil
	}
	return nil
}

// PruneWorktrees drops registrations whose directories are gone.
func (r *Repo) PruneWorktrees(ctx context.Context) error {
	if _, err := r.Run(ctx, "worktree", "prune"); err != nil {
		return fmt.Errorf("prune worktrees: %w", err)
	}
	return nil
}

// DeleteBranch removes a local branch whether or not it is merged. A branch
// that does not exist reports success; one still checked out somewhere returns
// ErrBranchCheckedOut, which means its worktree was not removed first.
func (r *Repo) DeleteBranch(ctx context.Context, branch string) error {
	exists, err := r.BranchExists(ctx, branch)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if _, err := r.Run(ctx, "branch", "-D", branch); err != nil {
		if said(err, "checked out at") || said(err, "used by worktree") {
			return fmt.Errorf("delete branch %s: %w", branch, ErrBranchCheckedOut)
		}
		return fmt.Errorf("delete branch %s: %w", branch, err)
	}
	return nil
}

// FastForward advances the current branch to ref without creating a commit.
// A ref already contained in the current branch succeeds unchanged; a current
// branch holding commits ref does not returns ErrNotFastForward, which is a
// human's problem rather than something to retry.
func (r *Repo) FastForward(ctx context.Context, ref string) error {
	if _, err := r.Run(ctx, "merge", "--ff-only", ref); err != nil {
		if said(err, "not possible to fast-forward") || said(err, "non-fast-forward") {
			return fmt.Errorf("fast-forward to %s: %w", ref, ErrNotFastForward)
		}
		return fmt.Errorf("fast-forward to %s: %w", ref, err)
	}
	return nil
}

// Merge merges ref into the current branch, committing without an editor.
//
// A conflicting merge is aborted before this returns ErrMergeConflict. Leaving
// the conflicted index in place wedges the worktree for every later command —
// the next run finds a checkout mid-merge and every git operation it tries
// fails for a reason unrelated to what it was doing.
func (r *Repo) Merge(ctx context.Context, ref string) error {
	_, err := r.Run(ctx, "merge", "--no-edit", ref)
	if err == nil {
		return nil
	}
	conflicted, probeErr := r.mergeInProgress(ctx)
	if probeErr == nil && conflicted {
		if _, abortErr := r.Run(ctx, "merge", "--abort"); abortErr != nil {
			return fmt.Errorf("merge %s conflicted and could not be aborted: %w", ref, abortErr)
		}
		return fmt.Errorf("merge %s: %w", ref, ErrMergeConflict)
	}
	return fmt.Errorf("merge %s: %w", ref, err)
}

// mergeInProgress reports whether the repository is sitting mid-merge. This
// asks git for MERGE_HEAD rather than matching the word "CONFLICT" in the
// output, which is localised and printed on stdout.
func (r *Repo) mergeInProgress(ctx context.Context) (bool, error) {
	_, err := r.Run(ctx, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	if err == nil {
		return true, nil
	}
	if code, ok := exitStatus(err); ok && code == 1 {
		return false, nil
	}
	return false, err
}

// Push publishes a branch to a remote.
func (r *Repo) Push(ctx context.Context, remote, branch string) error {
	if _, err := r.Run(ctx, "push", remote, branch); err != nil {
		if said(err, "non-fast-forward") || said(err, "fetch first") {
			return fmt.Errorf("push %s to %s: %w", branch, remote, ErrNotFastForward)
		}
		return fmt.Errorf("push %s to %s: %w", branch, remote, err)
	}
	return nil
}

// ReclaimableWorktrees selects the worktrees whose checkouts may be removed.
//
// Two conditions, both required. The branch must carry prefix, so a worktree
// the operator made by hand is never touched. And the branch must already be
// contained in integration, so removing the checkout throws away nothing that
// is not also somewhere else — an unmerged branch is work in progress no
// matter how finished the run that made it looked.
//
// The main checkout is excluded whatever its branch: it is where the repository
// is administered from, and removing it is never the intent.
func ReclaimableWorktrees(ctx context.Context, r *Repo, worktrees []Worktree, prefix, integration string) ([]Worktree, error) {
	var out []Worktree
	for _, w := range worktrees {
		if w.Detached || w.Branch == "" || !strings.HasPrefix(w.Branch, prefix) {
			continue
		}
		if samePath(w.Path, r.Root) {
			continue
		}
		merged, err := r.IsAncestor(ctx, w.Branch, integration)
		if err != nil {
			return nil, err
		}
		if merged {
			out = append(out, w)
		}
	}
	return out, nil
}

// samePath compares two paths for pointing at the same directory. Symlinks are
// resolved where possible: a temporary directory is behind one on macOS, so a
// path git reports and the path a caller passes can differ character by
// character while naming the same worktree.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, aerr := filepath.EvalSymlinks(a)
	rb, berr := filepath.EvalSymlinks(b)
	if aerr != nil || berr != nil {
		return false
	}
	return filepath.Clean(ra) == filepath.Clean(rb)
}
