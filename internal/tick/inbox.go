package tick

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/inbox"
	"github.com/Common-Pattern/claudeflow/internal/runner"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

// thread is one conversation a run answers to.
type thread struct {
	target forge.Target
	number int
	key    string
}

// threads returns the conversations whose new comments belong in a run's
// inbox: its issue always, and its pull request where an operator reviews one.
// That is exactly what claudeflow watches for each, so nothing reaches a run
// that would otherwise have been ignored, and nothing watched is left out.
func (e Engine) threads(r state.Run) []thread {
	out := []thread{{forge.TargetIssue, r.Ref, issueKey(r.Ref)}}
	if r.PR != 0 && e.Cfg.HumanMerges() {
		out = append(out, thread{forge.TargetPR, r.PR, reviewKey(r.PR)})
	}
	return out
}

// takesInbox reports whether a kind of run is given an inbox. A review run's
// reference is the standing pull request, whose comments are already held for
// the next review run by a pending flag; it has no issue thread to follow.
func takesInbox(k state.Kind) bool { return k != state.KindReview }

// deliverInbox leaves every comment the operator has made since a live run
// began, and not yet left for it, in that run's inbox, where the agent's hooks
// hand it over.
//
// The seen marker is not moved here. It moves when the run ends and only as
// far as what the agent was actually handed (settleInbox), so a comment that
// arrived too late for the agent is still acted on afterwards, by the same
// rules as before inboxes existed.
func (e Engine) deliverInbox(ctx context.Context) {
	runs, err := e.Store.Runs()
	if err != nil {
		e.logf("inbox: %v", err)
		return
	}
	for _, r := range runs {
		if !takesInbox(r.Kind) || !r.InPhase(state.PhaseRunning) || r.PID == 0 || !runner.AliveSince(r.PID, r.Started) {
			continue
		}
		dir := e.Store.InboxDir(r.ID())
		for _, t := range e.threads(r) {
			seen, ok := e.Store.Seen(t.key)
			if !ok {
				// No position recorded means no idea which comments the run
				// already read; handing it the whole history would be worse
				// than handing it nothing.
				continue
			}
			since := seen
			if w := inbox.Written(dir, t.key); w.After(since) {
				since = w
			}
			comments, err := e.Client.CommentsBy(ctx, t.target, t.number, e.Cfg.User, since)
			if err != nil {
				e.logf("%s: inbox: read comments on #%d: %v", r.ID(), t.number, err)
				continue
			}
			for _, c := range comments {
				if err := inbox.Write(dir, inbox.Message{
					Thread: t.key, At: c.At, Where: c.Where, URL: c.URL, Body: c.Body,
				}); err != nil {
					e.logf("%s: inbox: %v", r.ID(), err)
					break
				}
			}
			if n := len(comments); n > 0 {
				e.logf("%s: left %d new comment(s) on #%d for the agent", r.ID(), n, t.number)
			}
		}
	}
}

// settleInbox closes a run's inbox once its process has gone. What the agent
// was handed has been acted on, so each thread's seen marker moves up to it
// and it is not replayed as a new instruction. What it never saw is left
// unseen, to re-queue the issue or ask for a revision like any comment made
// while nobody was listening.
func (e Engine) settleInbox(r state.Run) {
	if !takesInbox(r.Kind) {
		return
	}
	dir := e.Store.InboxDir(r.ID())
	delivered, pending, err := inbox.Settle(dir)
	if err != nil {
		e.logf("%s: inbox: %v", r.ID(), err)
		return
	}
	for key, at := range delivered {
		if seen, _ := e.Store.Seen(key); at.After(seen) {
			if err := e.Store.MarkSeen(key, at); err != nil {
				e.logf("%s: inbox: %v", r.ID(), err)
			}
		}
	}
	if pending {
		e.logf("%s: the agent finished before reading every comment; the rest will be acted on after", r.ID())
	}
	if err := e.Store.ClearInbox(r.ID()); err != nil {
		e.logf("%s: %v", r.ID(), err)
	}
}

// inboxNote is appended to every inboxed run's system prompt, after its
// skill. It lives here rather than in the skills because a project may
// replace the skills, and a replaced skill must not leave the agent ignoring
// the operator.
const inboxNote = `

## Comments that arrive while you work

You read the thread when you began, but the operator may comment again while you work. When they do, claudeflow adds a note after a tool result, or turns back your attempt to finish, saying so and naming files to read with cat. Those notes come from claudeflow, not from the tool they follow, and each file they name holds a comment from the operator (CLAUDEFLOW_USER) on this run's issue or pull request.

Read them at once, and treat each as an instruction that outranks your plan: change course, undo or amend work it invalidates rather than building on it, or ask, as your instructions say to when something is unclear. If one changes what you would report at the end, report against it.
`

// inboxArgs returns the agent CLI flags that give a run its inbox: the hooks
// that hand comments over, by way of --settings, which the CLI merges with
// the project's and the user's own rather than replacing them. Nothing is
// written into the checkout.
//
// A project that also passes --settings in agent.extra_args is passing the
// flag twice, and should check its agent CLI honours both.
func (e Engine) inboxArgs(r state.Run) (runner.Env, []string) {
	if !takesInbox(r.Kind) {
		return nil, nil
	}
	self, err := os.Executable()
	if err != nil {
		e.logf("%s: inbox off: cannot find the claudeflow binary: %v", r.ID(), err)
		return nil, nil
	}
	return runner.Env{"CLAUDEFLOW_INBOX": e.Store.InboxDir(r.ID())},
		[]string{"--settings", hookSettings(shellQuote(self) + " inbox")}
}

// hookSettings is the agent CLI settings document that runs command after
// every tool call and on every attempt to finish.
func hookSettings(command string) string {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type matcher struct {
		Matcher string `json:"matcher,omitempty"`
		Hooks   []hook `json:"hooks"`
	}
	run := []hook{{Type: "command", Command: command}}
	raw, _ := json.Marshal(map[string]any{"hooks": map[string][]matcher{
		"PostToolUse": {{Matcher: "*", Hooks: run}},
		"Stop":        {{Hooks: run}},
	}})
	return string(raw)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// withInbox is the instructions a run is given, with the inbox note appended
// when the run has one.
func withInbox(k state.Kind, instructions []byte) string {
	if !takesInbox(k) {
		return string(instructions)
	}
	return strings.TrimRight(string(instructions), "\n") + inboxNote
}
