package tick

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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

// deadPID returns the id of a process that has exited.
//
// Tests used to write PID 1 for "a run whose process is gone", relying on its
// start time being nothing like the record's. That holds on a host that has
// been up a while and fails on one that just booted: a CI runner's init is
// seconds old, the gap falls inside the tolerance, and the run reads as alive.
// A process that really has exited is the same claim without the dependency on
// how long the machine has been running.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start a throwaway process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	return pid
}

// An issue WITH a run record is the record path's business, not the stranded
// -claim sweep's. A record whose process is gone resolves to blocked, which is
// a decision; re-queueing it would retry a run that died for an unknown reason
// and eat a slot each time.
func TestReapResolvesAClaimWithARecordRatherThanRequeueing(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(42, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	if err := e.Store.SaveRun(state.Run{
		Kind: state.KindBuild, Ref: 42, Slot: 1, PID: deadPID(t), Started: time.Now(),
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

// A start that fails after its claim resolves the claim itself, as blocked.
// Left to the stranded-claim sweep it was re-queued and re-dispatched, and a
// failure that was not transient added and removed the working label on every
// tick, forever.
func TestFailedStartSettlesAsBlocked(t *testing.T) {
	e, f := newEngine(t)
	e.Cfg.Agent.Command = filepath.Join(t.TempDir(), "no-such-agent")
	f.AddIssue(42, time.Now(), e.Cfg.Labels.Queued, e.Cfg.Labels.Planning)

	if err := e.Start(t.Context(), Start{Kind: state.KindPlan, Ref: 42}); err == nil {
		t.Fatal("Start succeeded with an agent that does not exist")
	}
	labels, _ := f.Labels(t.Context(), 42)
	if slices.Contains(labels, e.Cfg.Labels.Working) {
		t.Errorf("labels = %v, want the claim released", labels)
	}
	if !slices.Contains(labels, e.Cfg.Labels.Blocked) {
		t.Errorf("labels = %v, want blocked", labels)
	}
	if len(f.Comments[42]) != 1 || !strings.Contains(f.Comments[42][0], "no-such-agent") {
		t.Errorf("comments = %q, want one naming the failure", f.Comments[42])
	}

	// And the next tick leaves it alone rather than re-queueing it.
	if err := e.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	after, _ := f.Labels(t.Context(), 42)
	if !slices.Equal(after, labels) {
		t.Errorf("labels after Reap = %v, want unchanged %v", after, labels)
	}
	if e.Cfg.Labels.IsQueued(after) {
		t.Errorf("labels = %v, want it NOT ready to re-dispatch", after)
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

// A project points its own tooling at the run's containers through agent.env.
// Without it a test suite that refuses to guess its database — the correct
// behaviour — has nothing to be told, and the run cannot verify anything.
func TestExpandAgentEnv(t *testing.T) {
	resolved := map[string]string{
		"CLAUDEFLOW_PORT_POSTGRES": "49153",
		"CLAUDEFLOW_HOST_POSTGRES": "db.example.net",
		"CLAUDEFLOW_URL_WEB":       "http://db.example.net:49154",
	}
	got := expandAgentEnv(map[string]string{
		"DATABASE_URL": "postgresql://u:p@${CLAUDEFLOW_HOST_POSTGRES}:${CLAUDEFLOW_PORT_POSTGRES}/app_test",
		"BASE_URL":     "${CLAUDEFLOW_URL_WEB}",
		"LITERAL":      "no placeholders here",
	}, resolved)

	if want := "postgresql://u:p@db.example.net:49153/app_test"; got["DATABASE_URL"] != want {
		t.Errorf("DATABASE_URL = %q, want %q", got["DATABASE_URL"], want)
	}
	if got["BASE_URL"] != resolved["CLAUDEFLOW_URL_WEB"] {
		t.Errorf("BASE_URL = %q", got["BASE_URL"])
	}
	if got["LITERAL"] != "no placeholders here" {
		t.Errorf("LITERAL = %q, want it untouched", got["LITERAL"])
	}
}

// An unresolvable placeholder must expand to empty rather than be left as
// literal text: a DSN containing "${...}" would be dialled and fail somewhere
// far from the cause.
func TestExpandAgentEnvUnknownPlaceholder(t *testing.T) {
	got := expandAgentEnv(map[string]string{"X": "a-${NOPE}-b"}, map[string]string{})
	if got["X"] != "a--b" {
		t.Errorf("X = %q, want the placeholder emptied", got["X"])
	}
}

func TestExpandAgentEnvEmpty(t *testing.T) {
	if got := expandAgentEnv(nil, map[string]string{"A": "1"}); len(got) != 0 {
		t.Errorf("= %v, want empty", got)
	}
}
