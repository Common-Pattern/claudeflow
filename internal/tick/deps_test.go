package tick

import (
	"testing"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/forge"
)

// An issue blocked by unfinished work is held, not started: a run on it would
// build on a base without what it needs. It starts once every blocker is
// closed or has landed on the integration branch.
func TestHoldWaitingHoldsUntilEveryBlockerIsDone(t *testing.T) {
	e, f := newEngine(t)
	l := e.Cfg.Labels
	f.AddIssue(1, time.Now(), l.Queued, l.Landed) // landed, still open
	f.AddIssue(2, time.Now(), l.Queued)           // open, not landed

	cases := []struct {
		name     string
		blockers []forge.Blocker
		held     bool
	}{
		{"no blockers", nil, false},
		{"closed blocker", []forge.Blocker{{Number: 9, Repo: "o/n"}}, false},
		{"landed blocker", []forge.Blocker{{Number: 1, Repo: "o/n", Open: true}}, false},
		{"unfinished blocker", []forge.Blocker{{Number: 2, Repo: "o/n", Open: true}}, true},
		{"one of two unfinished", []forge.Blocker{{Number: 1, Repo: "o/n", Open: true}, {Number: 2, Repo: "o/n", Open: true}}, true},
		// Another repository's labels mean nothing here; only its close does.
		{"open in another repo", []forge.Blocker{{Number: 1, Repo: "x/y", Open: true}}, true},
		{"closed in another repo", []forge.Blocker{{Number: 1, Repo: "x/y"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			issue := forge.Issue{Number: 50, BlockedBy: c.blockers}
			got := e.holdWaiting(t.Context(), []forge.Issue{issue})
			if held := len(got) == 0; held != c.held {
				t.Errorf("held = %v, want %v", held, c.held)
			}
		})
	}
}
