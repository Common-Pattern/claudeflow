package tick

import (
	"context"
	"fmt"
	"strconv"

	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/land"
	"github.com/Common-Pattern/claudeflow/internal/state"
	"github.com/Common-Pattern/claudeflow/skills"
)

// reviewKey is the seen-marker for the conversation on an issue's own pull
// request while it waits for its operator.
//
// It is kept apart from prKey on purpose: the standing-pull-request watcher
// prunes every "pr-" marker but the current standing number, which would erase
// these on every tick in a project that has both.
func reviewKey(pr int) string { return "review-pr-" + strconv.Itoa(pr) }

// revisionPending is raised when the operator has asked for changes that no
// revision run has started on yet. It survives a tick that finds no free slot,
// so an instruction is never dropped for want of an environment.
func revisionPending(pr int) state.Flag { return state.Flag(reviewKey(pr) + ".pending") }

// readyForReview parks a green pull request for its operator.
//
// The environment is already down and the slot released: waiting for a person
// needs neither. The run keeps its worktree and branch, because the next thing
// that happens may be a revision on them.
func (e Engine) readyForReview(ctx context.Context, r state.Run) error {
	sha, err := e.Client.PRHeadSHA(ctx, r.PR)
	if err != nil {
		return fmt.Errorf("read head of #%d: %w", r.PR, err)
	}
	if sha != r.Announced {
		body := fmt.Sprintf("#%d is green at `%s` and waiting for your review.\n\n"+
			"Merge it when it is right. To change it, comment or leave a review on the pull request, "+
			"or comment here; either starts a revision on the same branch.", r.PR, short(sha))
		if err := e.Client.Comment(ctx, r.Ref, body); err != nil {
			e.logf("%s: could not comment: %v", r.ID(), err)
		}
		r.Announced = sha
	}

	l := e.Cfg.Labels
	if err := e.Client.EditLabels(ctx, r.Ref, []string{l.Review},
		[]string{l.Working, l.Landed, l.Question, l.Blocked}); err != nil {
		e.logf("%s: could not set %s: %v", r.ID(), l.Review, err)
	}
	r.Phase = state.PhaseAwaitingReview
	e.logf("%s: #%d is green; waiting for review", r.ID(), r.PR)
	return e.Store.SaveRun(r)
}

// advanceAwaitingReview settles pull requests a human merged or closed, and
// starts a revision on any the operator has asked to change.
//
// Settling happens whether or not dispatch is allowed: a merge is finished work
// being recorded, not new work being started. Revisions wait for dispatch like
// anything else that spends an environment and a model session.
func (e Engine) advanceAwaitingReview(ctx context.Context, dispatch bool) error {
	runs, err := e.Store.Runs()
	if err != nil {
		return err
	}
	for _, r := range runs {
		if !r.InPhase(state.PhaseAwaitingReview) {
			continue
		}
		settled, err := e.settleIfClosed(ctx, r)
		if err != nil {
			e.logf("%s: %v", r.ID(), err)
			continue
		}
		if settled {
			continue
		}
		if r, err = e.noteIfBehind(ctx, r); err != nil {
			e.logf("%s: could not compare #%d with %s: %v", r.ID(), r.PR, e.Cfg.Branches.Integration, err)
		}
		owed, err := e.revisionOwed(ctx, r)
		if err != nil {
			e.logf("%s: %v", r.ID(), err)
			continue
		}
		if !owed || !dispatch {
			continue
		}
		if err := e.startRevision(ctx, r); err != nil {
			e.logf("%s: could not start a revision: %v", r.ID(), err)
		}
	}
	return nil
}

// settleIfClosed finishes a run whose pull request is no longer open, and
// reports whether it did.
func (e Engine) settleIfClosed(ctx context.Context, r state.Run) (bool, error) {
	st, err := e.Client.PRState(ctx, r.PR)
	if err != nil {
		return false, fmt.Errorf("read state of #%d: %w", r.PR, err)
	}
	switch st {
	case forge.PRMerged:
		return true, e.mergedByHuman(ctx, r)
	case forge.PRClosed:
		e.logf("%s: #%d was closed without merging", r.ID(), r.PR)
		_ = e.Store.ClearFlag(revisionPending(r.PR))
		return true, e.finishRun(ctx, r, e.Cfg.Labels.Blocked, fmt.Sprintf(
			"#%d was closed without merging. The branch is kept; comment here to start the issue again.", r.PR))
	}
	return false, nil
}

// noteIfBehind tells the operator when the integration branch has moved past
// the point a pull request under review was branched from.
//
// A pull request that was green against an older base has not been tested
// against what it will merge into, and a repository that requires branches to
// be up to date refuses the merge outright. Either way the operator should hear
// it before reaching for the merge button, not from the button. The note goes
// on the pull request, where the review happens, once per head commit: a
// revision that brings the branch up to date moves the head, and a base that
// keeps moving under an unchanged head is still the same news.
func (e Engine) noteIfBehind(ctx context.Context, r state.Run) (state.Run, error) {
	sha, err := e.Client.PRHeadSHA(ctx, r.PR)
	if err != nil {
		return r, err
	}
	if sha == "" || sha == r.BehindNoted {
		return r, nil
	}
	behind, err := e.Client.BehindBy(ctx, e.Cfg.Branches.Integration, sha)
	if err != nil || behind == 0 {
		return r, err
	}

	base := e.Cfg.Branches.Integration
	commits := "commits"
	if behind == 1 {
		commits = "commit"
	}
	body := fmt.Sprintf("`%s` has moved %d %s past this branch, so it needs updating before it merges.\n\n"+
		"Comment here to have it brought up to date: a revision merges `%s` in, resolves any conflicts, and the checks run again. "+
		"Where the merge is clean, GitHub's **Update branch** button does the same.", base, behind, commits, base)
	if err := e.Client.Comment(ctx, r.PR, body); err != nil {
		return r, err
	}
	e.logf("%s: #%d is %d %s behind %s", r.ID(), r.PR, behind, commits, base)
	r.BehindNoted = sha
	return r, e.Store.SaveRun(r)
}

// mergedByHuman records a merge the operator made and brings the checkout up
// to date, as landing would have.
func (e Engine) mergedByHuman(ctx context.Context, r state.Run) error {
	e.logf("%s: #%d was merged", r.ID(), r.PR)
	_ = e.Store.ClearFlag(revisionPending(r.PR))

	body := fmt.Sprintf("#%d was merged into `%s`.", r.PR, e.Cfg.Branches.Integration)
	if e.Repo != nil {
		unlock, err := e.lockLanding()
		if err != nil {
			return err
		}
		defer unlock()
		l := land.Lander{Cfg: e.Cfg, Client: e.Client, Repo: e.Repo, Log: e.Log}
		res, err := l.AfterMerge(ctx, r.PR)
		if err != nil {
			body += fmt.Sprintf("\n\nThe local checkout could not be brought up to date:\n\n```\n%v\n```", err)
		}
		if len(res.Closes) > 0 && res.StandingPR != 0 {
			body += fmt.Sprintf("\n\nThe standing pull request will close this issue when it merges into `%s`.", e.Cfg.Branches.Base)
		}
	}
	return e.finishRun(ctx, r, e.Cfg.Labels.Landed, body)
}

// revisionOwed reports whether the operator has spoken since the pull request
// was last announced or revised, on the pull request or on the issue.
//
// Both threads count. A pull request's review is the natural place to ask for
// a change, but the issue is where the conversation started, and a reply there
// must not vanish because the work has moved on to a pull request.
func (e Engine) revisionOwed(ctx context.Context, r state.Run) (bool, error) {
	pending := revisionPending(r.PR)
	if e.Store.FlagRaised(pending) {
		return true, nil
	}
	owed := false
	for _, thread := range []struct {
		target forge.Target
		number int
		key    string
	}{
		{forge.TargetPR, r.PR, reviewKey(r.PR)},
		{forge.TargetIssue, r.Ref, issueKey(r.Ref)},
	} {
		latest, err := e.Client.LatestCommentBy(ctx, thread.target, thread.number, e.Cfg.User)
		if err != nil {
			return false, fmt.Errorf("read comments on #%d: %w", thread.number, err)
		}
		isNew, err := e.Store.NoteLatest(thread.key, latest)
		if err != nil {
			return false, err
		}
		owed = owed || isNew
	}
	if owed {
		if err := e.Store.SetFlag(pending, ""); err != nil {
			return false, err
		}
	}
	return owed, nil
}

// startRevision sends an agent at the operator's comments, on the run's own
// branch and pull request.
//
// It spends a slot and counts against the build budget exactly as a build does.
// With neither free the pending flag stays up and the next tick tries again.
func (e Engine) startRevision(ctx context.Context, r state.Run) error {
	live, err := e.liveRuns()
	if err != nil {
		return err
	}
	if builds, _ := countLive(live); builds >= e.Cfg.Limits.MaxBuilds {
		e.logf("%s: #%d has changes requested; waiting for a build slot", r.ID(), r.PR)
		return nil
	}
	slot, err := e.allocator().Free(ctx, claimedSlots(liveSlotRuns(live)))
	if err != nil {
		e.logf("%s: #%d has changes requested but no slot is free", r.ID(), r.PR)
		return nil
	}

	// Record where both threads stand at the start, so a comment the agent is
	// about to read is not replayed as new once it finishes.
	for _, thread := range []struct {
		target forge.Target
		number int
		key    string
	}{
		{forge.TargetPR, r.PR, reviewKey(r.PR)},
		{forge.TargetIssue, r.Ref, issueKey(r.Ref)},
	} {
		if latest, err := e.Client.LatestCommentBy(ctx, thread.target, thread.number, e.Cfg.User); err == nil && !latest.IsZero() {
			_ = e.Store.MarkSeen(thread.key, latest)
		}
	}

	l := e.Cfg.Labels
	parked := r
	r.Phase = state.PhaseRunning
	r.Slot = slot
	r.Attempts = 0
	if err := e.Client.EditLabels(ctx, r.Ref, []string{l.Working}, []string{l.Review}); err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	if err := e.Store.SaveRun(r); err != nil {
		return err
	}

	prompt := fmt.Sprintf(
		"The operator has asked for changes on pull request #%d, which implements issue #%d. Address them, following the run instructions in your system prompt exactly.",
		r.PR, r.Ref)
	if err := e.resume(ctx, r, skills.Revise, prompt, fmt.Sprintf("revise-%d", r.PR), nil); err != nil {
		if lerr := e.Client.EditLabels(ctx, r.Ref, []string{l.Review}, []string{l.Working}); lerr != nil {
			e.logf("%s: could not restore %s: %v", r.ID(), l.Review, lerr)
		}
		if serr := e.Store.SaveRun(parked); serr != nil {
			return serr
		}
		return err
	}
	return e.Store.ClearFlag(revisionPending(r.PR))
}
