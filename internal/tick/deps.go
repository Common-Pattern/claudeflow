package tick

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Common-Pattern/claudeflow/internal/forge"
)

// holdWaiting drops the issues that are waiting on another, and logs each.
//
// An issue GitHub records as blocked by one whose work is not done yet would be
// built on a base that lacks what it needs. Starting it costs a slot and a run
// that can only stop and say so; holding it costs nothing, and it starts on the
// first tick after the last blocker is done. Nothing has to be re-labelled:
// the queued label stays on throughout, and the dependency itself is the hold.
//
// Dependencies are the user's to record. This is the only place they are
// enforced. An agent that finds an unrecorded prerequisite asks about it
// instead of acting on it.
//
// Only builds are held. A planning run writes nothing, and talking through an
// issue whose prerequisite is unfinished is often exactly what is wanted.
func (e Engine) holdWaiting(ctx context.Context, issues []forge.Issue) []forge.Issue {
	var ready []forge.Issue
	landed := map[int]bool{}
	for _, issue := range issues {
		var waiting []string
		for _, b := range issue.BlockedBy {
			if !e.blockerDone(ctx, b, landed) {
				waiting = append(waiting, e.refName(b))
			}
		}
		if len(waiting) > 0 {
			e.logf("issue #%d: waiting on %s", issue.Number, strings.Join(waiting, ", "))
			continue
		}
		ready = append(ready, issue)
	}
	return ready
}

// blockerDone reports whether a blocker's work is done: the issue is closed, or
// it is in this repository and its work has landed on the integration branch.
//
// Landed counts because closing is the operator's, and happens only when the
// integration branch reaches the base. A dependant needs the work on the branch
// it builds from, which is the integration branch, and waiting on the close
// instead would hold it for however long that merge takes.
//
// A blocker whose labels cannot be read is not done. Holding an issue a tick
// too long costs a tick; starting it early costs a run built on missing work.
func (e Engine) blockerDone(ctx context.Context, b forge.Blocker, landed map[int]bool) bool {
	if !b.Open {
		return true
	}
	if !strings.EqualFold(b.Repo, e.Cfg.Repo) {
		return false
	}
	if done, ok := landed[b.Number]; ok {
		return done
	}
	labels, err := e.Client.Labels(ctx, b.Number)
	if err != nil {
		e.logf("issue #%d: could not read its labels to see whether it has landed: %v", b.Number, err)
		return false
	}
	done := slices.Contains(labels, e.Cfg.Labels.Landed)
	landed[b.Number] = done
	return done
}

func (e Engine) refName(b forge.Blocker) string {
	if strings.EqualFold(b.Repo, e.Cfg.Repo) || b.Repo == "" {
		return fmt.Sprintf("#%d", b.Number)
	}
	return fmt.Sprintf("%s#%d", b.Repo, b.Number)
}
