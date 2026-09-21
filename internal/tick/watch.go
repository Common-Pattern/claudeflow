package tick

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

// Watcher turns the configured user's comments into work.
//
// A comment from that user is an instruction. Only from that user: the agent
// comments on these same threads, and without the author filter it would spend
// every slot answering itself.
type Watcher struct {
	Client forge.Client
	Store  *state.Store
	Cfg    config.Config
}

func issueKey(n int) string { return "issue-" + strconv.Itoa(n) }
func prKey(n int) string    { return "pr-" + strconv.Itoa(n) }

// Requeue re-queues every touched issue the user has spoken on since the last
// run, and returns the issue numbers it re-queued.
//
// This is why a blocked or answered issue needs no manual re-label: replying to
// the agent's question is the re-label. It also means an issue can come back
// from any state, including one the agent considered finished.
func (w Watcher) Requeue(ctx context.Context) ([]int, error) {
	l := w.Cfg.Labels
	touched, err := w.Client.TouchedIssues(ctx, prefixOf(l.Queued), 200)
	if err != nil {
		return nil, fmt.Errorf("list touched issues: %w", err)
	}

	var requeued []int
	for _, issue := range touched {
		latest, err := w.Client.LatestCommentBy(ctx, forge.TargetIssue, issue.Number, w.Cfg.User)
		if err != nil {
			return nil, fmt.Errorf("read comments on #%d: %w", issue.Number, err)
		}
		isNew, err := w.Store.NoteLatest(issueKey(issue.Number), latest)
		if err != nil {
			return nil, err
		}
		// A live run is already reading the whole thread, so re-queueing
		// underneath it would start a second run on the same issue. The marker
		// is still advanced above, so the comment is not replayed later.
		if !isNew || issue.HasLabel(l.Working) {
			continue
		}
		if err := w.Client.EditLabels(ctx, issue.Number, []string{l.Queued}, l.Resolution()); err != nil {
			return nil, fmt.Errorf("re-queue #%d: %w", issue.Number, err)
		}
		requeued = append(requeued, issue.Number)
	}
	return requeued, nil
}

// ReviewOwed reports the standing pull request and whether a review run is due
// on it.
//
// Once owed, it stays owed until a review run is started: a pending marker is
// written so that a tick which could not allocate a slot does not lose the
// instruction.
func (w Watcher) ReviewOwed(ctx context.Context) (pr int, owed bool, err error) {
	if w.Cfg.Branches.SingleBranch() {
		// With no separate integration branch there is no standing pull
		// request to review against.
		return 0, false, nil
	}
	pr, err = w.Client.StandingPR(ctx, w.Cfg.Branches.Base, w.Cfg.Branches.Integration)
	switch {
	case errors.Is(err, forge.ErrNoStandingPR):
		return 0, false, nil
	case err != nil:
		// ErrManyStandingPRs reaches the caller deliberately. With two, the
		// agent would read comments from one while landing work gated by the
		// other, and choosing between them silently is how that stays hidden.
		return 0, false, err
	}

	pending := prKey(pr) + ".pending"
	if w.Store.FlagRaised(state.Flag(pending)) {
		owed = true
	} else {
		latest, err := w.Client.LatestCommentBy(ctx, forge.TargetPR, pr, w.Cfg.User)
		if err != nil {
			return 0, false, fmt.Errorf("read comments on PR #%d: %w", pr, err)
		}
		isNew, err := w.Store.NoteLatest(prKey(pr), latest)
		if err != nil {
			return 0, false, err
		}
		owed = isNew
	}

	if owed {
		if err := w.Store.SetFlag(state.Flag(pending), ""); err != nil {
			return 0, false, err
		}
	}

	// A pending marker for a pull request that has since merged can never fire
	// again, because only the current standing number is ever consulted.
	if err := w.Store.ForgetSeenExcept("pr-", prKey(pr)); err != nil {
		return 0, false, err
	}
	return pr, owed, nil
}

// ClearReviewOwed drops the pending marker once a review run has started.
func (w Watcher) ClearReviewOwed(pr int) error {
	return w.Store.ClearFlag(state.Flag(prKey(pr) + ".pending"))
}

// prefixOf returns the label prefix that identifies every label this system
// owns, so touched issues can be found whatever state they ended in.
func prefixOf(queued string) string { return queued }
