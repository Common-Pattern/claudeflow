// Package land merges a green pull request and leaves the machine in a state
// the operator can actually look at.
//
// This is a package rather than instructions in a skill because every step
// below has a failure mode that reads like success, and prose handed to an
// agent gets paraphrased. The merge happening on the server while the local
// checkout silently keeps serving pre-merge code is the one that cost the most.
package land

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/git"
)

// Outcomes a caller must be able to tell apart. The skills instruct the agent
// to report these and stop rather than work around them.
var (
	// ErrConflict means the integration branch conflicts with the work.
	ErrConflict = errors.New("integration branch conflicts with this branch")
	// ErrChecksAfterSync means CI failed once the branch was brought up to date.
	ErrChecksAfterSync = errors.New("checks failed after syncing with the integration branch")
	// ErrChecksNotGreen means a required check is missing or not passing.
	ErrChecksNotGreen = errors.New("a required check is not green")
	// ErrMergeRefused means the forge refused the merge.
	ErrMergeRefused = errors.New("merge refused")
	// ErrLocalDiverged means the local checkout cannot fast-forward.
	ErrLocalDiverged = errors.New("local checkout has commits and cannot fast-forward")
)

// Waiter blocks until a pull request's checks settle, returning the final set.
type Waiter func(ctx context.Context, pr int) ([]forge.Check, error)

// HookFunc runs a project hook by name. Landing calls the install hook after a
// fast-forward.
type HookFunc func(ctx context.Context, command string) error

// Lander merges one pull request.
type Lander struct {
	Cfg    config.Config
	Client forge.Client
	Repo   *git.Repo
	// Wait blocks on CI. Required only when a branch has to be re-synced.
	Wait Waiter
	// RunHook is optional; without it the install hook is skipped.
	RunHook HookFunc
	// Required names the checks that must be green. Empty means "whatever the
	// pull request reports, as long as none of it failed".
	Required []string
	// Log receives progress. Optional.
	Log func(format string, args ...any)
}

func (l Lander) logf(format string, args ...any) {
	if l.Log != nil {
		l.Log(format, args...)
	}
}

// Result describes what landing did.
type Result struct {
	PR             int
	MergedSHA      string
	Resynced       bool
	StandingPR     int
	OpenedStanding bool
	// Closes are the issues this landing will close, once the standing pull
	// request is merged into the base branch.
	Closes []int
}

// Land merges pr into the integration branch.
//
// Callers must serialise: two runs landing at once each pass CI against a base
// the other is about to move, and the local checkout ends up fast-forwarded to
// a tree neither of them tested. Lock in the caller, around this whole call.
func (l Lander) Land(ctx context.Context, pr int, worktree string) (Result, error) {
	res := Result{PR: pr}
	wt := git.New(worktree)

	if err := wt.Fetch(ctx); err != nil {
		return res, fmt.Errorf("fetch in worktree: %w", err)
	}

	integration := "origin/" + l.Cfg.Branches.Integration

	// CI proves the branch works against the base it was cut from. If the
	// integration branch has moved since, that proof is about a tree that no
	// longer exists.
	behind, err := wt.CommitsBetween(ctx, "HEAD", integration)
	if err != nil {
		return res, fmt.Errorf("compare with %s: %w", integration, err)
	}
	if behind > 0 {
		l.logf("branch is %d commits behind %s; merging it in", behind, integration)
		if err := wt.Merge(ctx, integration); err != nil {
			if errors.Is(err, git.ErrMergeConflict) {
				return res, ErrConflict
			}
			return res, fmt.Errorf("merge %s: %w", integration, err)
		}
		branch := branchOf(ctx, wt)
		if err := wt.Push(ctx, "origin", branch); err != nil {
			return res, fmt.Errorf("push resynced branch: %w", err)
		}
		res.Resynced = true

		if l.Wait == nil {
			return res, fmt.Errorf("branch was resynced but no CI waiter is configured")
		}
		l.logf("pushed the merge; re-waiting on checks")
		checks, err := l.Wait(ctx, pr)
		if err != nil {
			return res, fmt.Errorf("wait for checks: %w", err)
		}
		if !l.green(checks) {
			return res, fmt.Errorf("%w: %v", ErrChecksAfterSync, forge.Failed(checks))
		}
	}

	headSHA, err := wt.HeadSHA(ctx)
	if err != nil {
		return res, fmt.Errorf("read head: %w", err)
	}
	res.MergedSHA = headSHA

	checks, err := l.Client.Checks(ctx, pr)
	if err != nil {
		return res, fmt.Errorf("read checks: %w", err)
	}
	if !l.green(checks) {
		if pending := forge.Pending(checks); len(pending) > 0 {
			return res, fmt.Errorf("%w: still running: %v", ErrChecksNotGreen, pending)
		}
		return res, fmt.Errorf("%w: %v", ErrChecksNotGreen, forge.Failed(checks))
	}

	// Read the closing references before merging, while the pull request is
	// still the thing being talked about.
	var closes []int
	if body, err := l.Client.PRBody(ctx, pr); err == nil {
		closes = forge.ClosingRefs(body)
	} else {
		l.logf("could not read the pull request body: %v", err)
	}

	l.logf("merging #%d at %s", pr, short(headSHA))
	if err := l.Client.Merge(ctx, pr, headSHA); err != nil {
		return res, fmt.Errorf("%w: %v", ErrMergeRefused, err)
	}
	res.Closes = closes

	// The merge happened on the server, so nothing about it touched this
	// machine. Without the next two steps the local checkout keeps serving
	// pre-merge code, and every check the operator runs afterwards passes
	// honestly against a tree missing the thing that just landed.
	if err := l.syncLocal(ctx); err != nil {
		return res, err
	}

	res.StandingPR, res.OpenedStanding, err = l.ensureStanding(ctx, closes)
	if err != nil {
		// The merge already happened. A standing-pull-request problem is worth
		// reporting but must not read as a failed merge.
		l.logf("merged, but could not settle the standing pull request: %v", err)
	}
	return res, nil
}

func (l Lander) syncLocal(ctx context.Context) error {
	if err := l.Repo.Fetch(ctx); err != nil {
		return fmt.Errorf("fetch in checkout: %w", err)
	}
	l.logf("fast-forwarding the checkout")
	if err := l.Repo.FastForward(ctx, "origin/"+l.Cfg.Branches.Integration); err != nil {
		if errors.Is(err, git.ErrNotFastForward) {
			return ErrLocalDiverged
		}
		return fmt.Errorf("fast-forward checkout: %w", err)
	}

	// A fast-forward that brings in a new dependency or workspace package is
	// not finished at the merge: nothing links it until the install hook runs,
	// and the next process to reload dies on a missing module.
	if l.RunHook != nil && l.Cfg.Hooks.Install != "" {
		l.logf("re-running the install hook")
		if err := l.RunHook(ctx, l.Cfg.Hooks.Install); err != nil {
			l.logf("install hook reported a problem: %v", err)
		}
	}
	return nil
}

// ensureStanding keeps the integration pull request open.
//
// Where CI runs on pull requests, a push to the integration branch is gated
// only because that standing pull request makes it a push to some pull
// request's head. If it lapses, the integration branch is silently ungated.
func (l Lander) ensureStanding(ctx context.Context, closes []int) (pr int, opened bool, err error) {
	if l.Cfg.Branches.SingleBranch() {
		// With no separate integration branch the work merged straight into
		// the base branch, so GitHub has already closed the issues itself.
		return 0, false, nil
	}
	pr, err = l.Client.StandingPR(ctx, l.Cfg.Branches.Base, l.Cfg.Branches.Integration)
	switch {
	case err == nil:
		// Carry the closing references forward. GitHub closes an issue only
		// when a pull request merges into the default branch, so the `Closes
		// #N` on the branch that just merged into the integration branch does
		// nothing at all — without this the work ships and the issue stays
		// open.
		if len(closes) > 0 {
			if err := l.addClosingRefs(ctx, pr, closes); err != nil {
				l.logf("could not carry closing references onto #%d: %v", pr, err)
			}
		}
		return pr, false, nil
	case !errors.Is(err, forge.ErrNoStandingPR):
		return 0, false, err
	}

	title := fmt.Sprintf("%s → %s", l.Cfg.Branches.Integration, l.Cfg.Branches.Base)
	body := fmt.Sprintf("Opened by claudeflow on %s to keep %s gated by CI.",
		time.Now().UTC().Format(time.RFC3339), l.Cfg.Branches.Integration)
	body = forge.WithClosingRefs(body, closes)
	pr, err = l.Client.CreatePR(ctx, l.Cfg.Branches.Base, l.Cfg.Branches.Integration, title, body)
	if err != nil {
		// Identical branches is the ordinary case, not a fault: there is
		// nothing to open a pull request for until something lands.
		return 0, false, err
	}
	l.logf("opened the standing pull request #%d", pr)
	return pr, true, nil
}

func (l Lander) green(checks []forge.Check) bool {
	if len(l.Required) > 0 {
		return forge.AllPassed(checks, l.Required...)
	}
	if len(checks) == 0 {
		// No checks at all is not evidence of anything. Refusing here is what
		// stops a merge landing before CI was ever scheduled.
		return false
	}
	return len(forge.Failed(checks)) == 0 && len(forge.Pending(checks)) == 0
}

func branchOf(ctx context.Context, r *git.Repo) string {
	out, err := r.Run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// addClosingRefs merges refs into the standing pull request's closing block.
//
// Read-modify-write rather than append: several landings accumulate onto one
// pull request, and each must preserve what the others put there.
func (l Lander) addClosingRefs(ctx context.Context, pr int, refs []int) error {
	body, err := l.Client.PRBody(ctx, pr)
	if err != nil {
		return fmt.Errorf("read standing body: %w", err)
	}
	updated := forge.WithClosingRefs(body, refs)
	if updated == body {
		return nil
	}
	if err := l.Client.UpdatePRBody(ctx, pr, updated); err != nil {
		return fmt.Errorf("update standing body: %w", err)
	}
	l.logf("standing pull request #%d now closes %v on merge", pr, forge.ClosingRefs(updated))
	return nil
}
