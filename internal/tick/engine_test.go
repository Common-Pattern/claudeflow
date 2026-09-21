package tick

import (
	"slices"
	"testing"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

func newEngine(t *testing.T) (Engine, *forge.Fake) {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	cfg := config.Default()
	cfg.Repo, cfg.User = "o/n", "u"
	f := forge.NewFake()
	return Engine{Cfg: cfg, Client: f, Store: st, Log: func(string, ...any) {}}, f
}

// Start claims the label before spawning, so two ticks cannot both start on one
// issue. When the spawn then fails the claim is left with no run record, and
// Reap — which walks run records — never sees it. The issue would sit in the
// working state forever, invisible to dispatch.
func TestReapReleasesAClaimWithNoRunBehindIt(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(42, time.Now(), e.Cfg.Labels.Working)

	if err := e.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 42)
	if !slices.Contains(labels, e.Cfg.Labels.Queued) {
		t.Errorf("labels = %v, want the issue re-queued", labels)
	}
	if slices.Contains(labels, e.Cfg.Labels.Working) {
		t.Errorf("labels = %v, want the stranded claim cleared", labels)
	}
}

// An issue WITH a run record is the record path's business, not the stranded
// -claim sweep's. A record whose process is gone resolves to blocked, which is
// a decision; re-queueing it would retry a run that died for an unknown reason
// and eat a slot each time.
func TestReapResolvesAClaimWithARecordRatherThanRequeueing(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(42, time.Now(), e.Cfg.Labels.Working)
	if err := e.Store.SaveRun(state.Run{
		Kind: state.KindBuild, Ref: 42, Slot: 1, PID: 1, Started: time.Now(),
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	if err := e.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 42)
	if !slices.Contains(labels, e.Cfg.Labels.Blocked) {
		t.Errorf("labels = %v, want blocked from the run-record path", labels)
	}
	if slices.Contains(labels, e.Cfg.Labels.Queued) {
		t.Errorf("labels = %v, want it NOT re-queued — a dead run is a decision", labels)
	}
}

func TestReapIgnoresUnclaimedIssues(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(42, time.Now(), e.Cfg.Labels.Queued)

	if err := e.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 42)
	if slices.Contains(labels, e.Cfg.Labels.Working) {
		t.Errorf("labels = %v, want an unclaimed issue untouched", labels)
	}
}
