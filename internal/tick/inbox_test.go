package tick

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/inbox"
	"github.com/Common-Pattern/claudeflow/internal/state"
	"github.com/Common-Pattern/claudeflow/skills"
)

// liveRun records a running build on issue ref, backed by a real process so
// that liveness checks see it alive.
func liveRun(t *testing.T, e Engine, ref int) state.Run {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start a stand-in agent: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	r := state.Run{Kind: state.KindBuild, Ref: ref, PID: cmd.Process.Pid, Started: time.Now(), Phase: state.PhaseRunning}
	if err := e.Store.SaveRun(r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	return r
}

func pendingFiles(t *testing.T, e Engine, r state.Run) []string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(e.Store.InboxDir(r.ID()), "pending", "*.md"))
	return matches
}

func TestDeliverInboxLeavesNewCommentsForALiveRunOnce(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(7, epoch, e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	f.Say(forge.TargetIssue, 7, epoch, "the original ask, which the run read")
	r := liveRun(t, e, 7)
	_ = e.Store.MarkSeen(issueKey(7), epoch)

	e.deliverInbox(t.Context())
	if got := pendingFiles(t, e, r); len(got) != 0 {
		t.Fatalf("pending = %v; the run already read everything up to its start", got)
	}

	f.Say(forge.TargetIssue, 7, epoch.Add(time.Hour), "actually, use Postgres")
	e.deliverInbox(t.Context())
	e.deliverInbox(t.Context())
	got := pendingFiles(t, e, r)
	if len(got) != 1 {
		t.Fatalf("pending = %v, want the one new comment, once", got)
	}
	raw, _ := os.ReadFile(got[0])
	if !strings.Contains(string(raw), "actually, use Postgres") {
		t.Errorf("message = %q", raw)
	}
}

func TestDeliverInboxSkipsWhatItCannotPlace(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(7, epoch, e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	f.Say(forge.TargetIssue, 7, epoch.Add(time.Hour), "hello")

	// No seen marker: no way to tell what the run read.
	r := liveRun(t, e, 7)
	e.deliverInbox(t.Context())
	if got := pendingFiles(t, e, r); len(got) != 0 {
		t.Errorf("pending = %v with no seen marker; that would hand over the whole history", got)
	}

	// A run whose process has gone is Reap's, not the inbox's.
	_ = e.Store.MarkSeen(issueKey(8), epoch)
	dead := state.Run{Kind: state.KindBuild, Ref: 8, PID: 0, Phase: state.PhaseRunning}
	_ = e.Store.SaveRun(dead)
	f.Say(forge.TargetIssue, 8, epoch.Add(time.Hour), "hello")
	e.deliverInbox(t.Context())
	if got := pendingFiles(t, e, dead); len(got) != 0 {
		t.Errorf("pending = %v for a run with no process", got)
	}
}

// What the agent was handed must not come back as a new instruction once the
// run ends; what it never saw must.
func TestSettleInboxAdvancesOnlyPastWhatWasHandedOver(t *testing.T) {
	e, f := newEngine(t)
	f.AddIssue(7, epoch, e.Cfg.Labels.Queued, e.Cfg.Labels.Working)
	r := liveRun(t, e, 7)
	_ = e.Store.MarkSeen(issueKey(7), epoch)
	w := Watcher{Client: f, Store: e.Store, Cfg: e.Cfg}

	f.Say(forge.TargetIssue, 7, epoch.Add(time.Hour), "seen by the agent")
	e.deliverInbox(t.Context())
	if _, err := inbox.Take(e.Store.InboxDir(r.ID())); err != nil {
		t.Fatal(err)
	}
	e.settleInbox(r)
	if seen, _ := e.Store.Seen(issueKey(7)); !seen.Equal(epoch.Add(time.Hour)) {
		t.Errorf("seen = %v, want it moved up to the delivered comment", seen)
	}
	if _, err := os.Stat(e.Store.InboxDir(r.ID())); !os.IsNotExist(err) {
		t.Errorf("inbox still there after settling: %v", err)
	}
	_ = f.EditLabels(t.Context(), 7, []string{e.Cfg.Labels.Landed}, []string{e.Cfg.Labels.Working})
	if got, _ := w.Requeue(t.Context()); len(got) != 0 {
		t.Errorf("re-queued %v for a comment the agent acted on", got)
	}

	// Same again, but the comment lands after the agent's last look.
	_ = f.EditLabels(t.Context(), 7, []string{e.Cfg.Labels.Working}, []string{e.Cfg.Labels.Landed})
	f.Say(forge.TargetIssue, 7, epoch.Add(2*time.Hour), "too late for the agent")
	e.deliverInbox(t.Context())
	e.settleInbox(r)
	_ = f.EditLabels(t.Context(), 7, []string{e.Cfg.Labels.Landed}, []string{e.Cfg.Labels.Working})
	if got, _ := w.Requeue(t.Context()); len(got) != 1 {
		t.Errorf("re-queued %v, want [7]: the agent never saw that comment", got)
	}
}

func TestBuildRunsAreGivenTheInbox(t *testing.T) {
	e, _ := newEngine(t)
	args, err := e.agentArgs(Start{Kind: state.KindBuild, Ref: 7})
	if err != nil {
		t.Fatal(err)
	}
	prompt := args[slices.Index(args, "--append-system-prompt")+1]
	if !strings.Contains(prompt, "Comments that arrive while you work") {
		t.Error("the system prompt does not explain the inbox notes; the agent would ignore them as injected")
	}

	env, flags := e.inboxArgs(state.Run{Kind: state.KindBuild, Ref: 7})
	if env["CLAUDEFLOW_INBOX"] != e.Store.InboxDir("build-7") {
		t.Errorf("CLAUDEFLOW_INBOX = %q", env["CLAUDEFLOW_INBOX"])
	}
	if len(flags) != 2 || flags[0] != "--settings" {
		t.Fatalf("flags = %v, want --settings and a document", flags)
	}
	var settings struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal([]byte(flags[1]), &settings); err != nil {
		t.Fatalf("settings do not parse: %v", err)
	}
	for _, event := range []string{"PostToolUse", "Stop"} {
		m := settings.Hooks[event]
		if len(m) != 1 || len(m[0].Hooks) != 1 || !strings.HasSuffix(m[0].Hooks[0].Command, "' inbox") {
			t.Errorf("%s hooks = %+v, want one running claudeflow inbox", event, m)
		}
	}
}

func TestReviewRunsHaveNoInbox(t *testing.T) {
	e, _ := newEngine(t)
	if env, flags := e.inboxArgs(state.Run{Kind: state.KindReview, Ref: 40}); env != nil || flags != nil {
		t.Errorf("review run given an inbox: %v %v", env, flags)
	}
	instructions, err := skills.Read(skills.Review, "")
	if err != nil {
		t.Fatal(err)
	}
	if withInbox(state.KindReview, instructions) != string(instructions) {
		t.Error("review run's prompt carries the inbox note")
	}
}
