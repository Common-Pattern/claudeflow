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
	f.AddIssue(42, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)

	if err := e.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 42)
	if !e.Cfg.Labels.IsQueued(labels) {
		t.Errorf("labels = %v, want the issue queued again", labels)
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
	f.AddIssue(42, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
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
	// The queued label stays — it marks membership. What matters is that the
	// issue does not read as ready to pick up again: a dead run is a decision.
	if e.Cfg.Labels.IsQueued(labels) {
		t.Errorf("labels = %v, want it NOT ready to re-dispatch — a dead run is a decision", labels)
	}
}

func TestReapIgnoresUnclaimedIssues(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(42, time.Now(), e.Cfg.Labels.Queued)

	if err := e.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 42)
	if !e.Cfg.Labels.IsQueued(labels) {
		t.Errorf("labels = %v, want an unclaimed issue untouched", labels)
	}
}

// Claiming must not remove the queued label: the issue does not stop being the
// agent's while a run is on it.
func TestClaimKeepsTheQueuedLabel(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(42, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Question)

	if err := e.Client.EditLabels(t.Context(), 42,
		[]string{e.Cfg.Labels.Working}, e.Cfg.Labels.Resolution()); err != nil {
		t.Fatalf("EditLabels: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 42)
	if !slices.Contains(labels, e.Cfg.Labels.Queued) {
		t.Errorf("labels = %v, want the queued label kept", labels)
	}
	if !slices.Contains(labels, e.Cfg.Labels.Working) {
		t.Errorf("labels = %v, want the working label added", labels)
	}
	if slices.Contains(labels, e.Cfg.Labels.Question) {
		t.Errorf("labels = %v, want the previous outcome cleared", labels)
	}
	if e.Cfg.Labels.IsQueued(labels) {
		t.Errorf("a claimed issue still reads as queued: %v", labels)
	}
}

// Releasing a stranded claim removes only the working label; the queued label
// was never taken away, so it does not need restoring.
func TestStrandedReleaseOnlyRemovesWorking(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(42, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)

	if err := e.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	labels, _ := f.Labels(t.Context(), 42)
	if !e.Cfg.Labels.IsQueued(labels) {
		t.Errorf("labels = %v, want the issue queued again", labels)
	}
	if n := len(labels); n != 1 {
		t.Errorf("labels = %v, want exactly the queued label", labels)
	}
}
