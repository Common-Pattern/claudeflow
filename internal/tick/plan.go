// Package tick decides what claudeflow should do next, and carries it out.
//
// The decision is separated from the execution on purpose. Scheduling is where
// the rules live — which work goes first, what may run at once, which slot it
// takes — and those rules are worth testing without a GitHub account, a git
// checkout or a process to supervise. Plan is a function of observed state;
// Engine is the part that observes and then acts.
package tick

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/slot"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

// Start is one run the tick decided to begin.
type Start struct {
	Kind state.Kind
	Ref  int
	// Slot is the allocated environment, or zero for kinds that hold none.
	Slot int
}

// Inputs is everything a tick observed before deciding.
type Inputs struct {
	Now time.Time

	// Live are runs whose process is still going.
	Live []state.Run

	// Queued are open issues carrying the queued label and not already
	// claimed. Planning issues are separated out because they are budgeted
	// apart and take no slot.
	QueuedBuild []forge.Issue
	QueuedPlan  []forge.Issue

	// ReviewOwed is set when the watched user has commented on the standing
	// pull request since the last review run.
	ReviewOwed bool
	// StandingPR is the pull request a review run would address.
	StandingPR int

	Limits config.Limits
	Alloc  slot.Allocator
}

// Plan decides which runs to start.
//
// Order is deliberate and is the one scheduling rule worth stating plainly:
// review comments go before new issues. The user is waiting on those, and work
// already on the integration branch that is wrong matters more than work that
// has not started.
func Plan(ctx context.Context, in Inputs) []Start {
	var starts []Start

	builds, plans := countLive(in.Live)
	claimed := claimedSlots(in.Live)

	// A review run holds a slot and counts against the build budget: it
	// implements and lands code exactly as a build does.
	if in.ReviewOwed && in.StandingPR > 0 && !liveFor(in.Live, state.KindReview, in.StandingPR) {
		if builds < in.Limits.MaxBuilds {
			if n, err := in.Alloc.Free(ctx, claimed); err == nil {
				starts = append(starts, Start{Kind: state.KindReview, Ref: in.StandingPR, Slot: n})
				claimed = append(claimed, n)
				builds++
			}
		}
	}

	// Planning next, on its own budget. A planning run holds no slot and
	// finishes in minutes, so queueing a reply behind two long builds would
	// turn a conversation into a day.
	for _, issue := range oldestFirst(in.QueuedPlan) {
		if plans >= in.Limits.MaxPlanning {
			break
		}
		if liveFor(in.Live, state.KindPlan, issue.Number) {
			continue
		}
		starts = append(starts, Start{Kind: state.KindPlan, Ref: issue.Number})
		plans++
	}

	for _, issue := range oldestFirst(in.QueuedBuild) {
		if builds >= in.Limits.MaxBuilds {
			break
		}
		if liveFor(in.Live, state.KindBuild, issue.Number) {
			continue
		}
		n, err := in.Alloc.Free(ctx, claimed)
		if err != nil {
			// Out of slots. Later issues would fail the same way, so stop
			// rather than spinning through the whole queue.
			break
		}
		starts = append(starts, Start{Kind: state.KindBuild, Ref: issue.Number, Slot: n})
		claimed = append(claimed, n)
		builds++
	}

	return starts
}

func countLive(runs []state.Run) (builds, plans int) {
	for _, r := range runs {
		if r.Kind == state.KindPlan {
			plans++
		} else {
			builds++
		}
	}
	return builds, plans
}

func claimedSlots(runs []state.Run) []int {
	var out []int
	for _, r := range runs {
		// Slot zero means none held — a planning run, or one parked on its
		// pull request with the environment already taken down.
		if r.Kind.HoldsSlot() && r.Slot != 0 {
			out = append(out, r.Slot)
		}
	}
	return out
}

func liveFor(runs []state.Run, kind state.Kind, ref int) bool {
	for _, r := range runs {
		if r.Kind == kind && r.Ref == ref {
			return true
		}
	}
	return false
}

func oldestFirst(issues []forge.Issue) []forge.Issue {
	out := make([]forge.Issue, len(issues))
	copy(out, issues)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].Number < out[j].Number
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Blocked reports why a tick must not dispatch, or nil if it may.
var (
	// ErrPaused means an operator stopped dispatch.
	ErrPaused = errors.New("dispatch paused")
	// ErrRateLimited means the agent CLI reported a usage limit recently.
	ErrRateLimited = errors.New("agent usage limit backoff in force")
)

// DispatchBlocked reports whether dispatch is currently suspended, and why.
//
// Live runs are unaffected by either condition: a pause stops new work
// starting, it does not abandon work in progress.
func DispatchBlocked(st *state.Store, now time.Time) error {
	if st.FlagRaised(state.Paused) {
		return ErrPaused
	}
	if _, limited := st.RateLimitedUntil(now); limited {
		return ErrRateLimited
	}
	return nil
}

// SplitQueued divides queued issues into planning and building, and drops any
// already claimed.
//
// An issue carrying both the queued and planning labels is a conversation, not
// a build. Getting this backwards would have the agent write code on an issue
// whose whole point was that it is not yet understood.
func SplitQueued(issues []forge.Issue, l config.Labels) (build, plan []forge.Issue) {
	for _, i := range issues {
		// Queued means: the agent's, nothing running, no outcome yet. The
		// queued label alone is not enough now that it is never removed.
		if !l.IsQueued(i.Labels) {
			continue
		}
		if i.HasLabel(l.Planning) {
			plan = append(plan, i)
		} else {
			build = append(build, i)
		}
	}
	return build, plan
}
