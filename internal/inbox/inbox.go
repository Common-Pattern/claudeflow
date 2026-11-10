// Package inbox carries instructions the operator gives while a run is working
// into that run.
//
// A run reads its thread once, when it begins. Before this package, a comment
// posted after that reached nobody until the run had finished — by which time
// the agent might have built, verified and landed exactly what the comment
// asked it not to. Nothing can be written to the agent directly: it is a
// detached process with no stdin, which outlives the supervisor that started
// it.
//
// So each tick writes the operator's new comments into a directory belonging
// to the run, and the agent CLI's own hooks read them out: after every tool
// call, a note in the agent's context points it at whatever has arrived, and
// an attempt to finish with anything unread is turned back. Both hooks run
// `claudeflow inbox`, which reads files and prints JSON; it never touches the
// network, so a hook adds nothing measurable to a tool call.
//
// The note points at the comment rather than carrying it. The agent CLI shows
// hook context alongside the tool result it follows, and an agent rightly
// treats instructions found in a tool result as an injection and ignores them.
// The run's system prompt, which claudeflow writes, is what tells the agent
// these notes are claudeflow's and the files they name are the operator's.
//
// Layout of a run's directory:
//
//	pending/<unix-nanos>-<thread>.md    written by the tick, not yet seen
//	delivered/<unix-nanos>-<thread>.md  moved here when a hook hands it over
//	written-<thread>                    newest comment time written, per thread
package inbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Message is one comment from the operator.
type Message struct {
	// Thread names the conversation, and is safe in a file name: "issue-12",
	// "review-pr-40". It is the seen-marker key the tick uses for the thread.
	Thread string
	At     time.Time
	// Where says where on GitHub it was left, for the agent and for a reader
	// of the transcript: "issue #12", "a review on pull request #40".
	Where string
	URL   string
	Body  string
}

const (
	pendingDir   = "pending"
	deliveredDir = "delivered"
)

// Write adds a message to a run's inbox and advances the thread's written
// marker past it. Writing the same message twice writes one file.
func Write(dir string, m Message) error {
	if !validThread(m.Thread) {
		return fmt.Errorf("inbox: unusable thread name %q", m.Thread)
	}
	name := fmt.Sprintf("%019d-%s.md", m.At.UnixNano(), m.Thread)
	if _, err := os.Stat(filepath.Join(dir, deliveredDir, name)); err == nil {
		return mark(dir, m.Thread, m.At)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The operator commented on %s at %s", m.Where, m.At.UTC().Format(time.RFC3339))
	if m.URL != "" {
		fmt.Fprintf(&b, " (%s)", m.URL)
	}
	b.WriteString(":\n\n")
	b.WriteString(strings.TrimSpace(m.Body))
	b.WriteString("\n")
	if err := writeAtomic(filepath.Join(dir, pendingDir), name, []byte(b.String())); err != nil {
		return err
	}
	return mark(dir, m.Thread, m.At)
}

// validThread reports whether a thread name is the shape the tick produces:
// letters and hyphens, then a number.
func validThread(t string) bool {
	i := strings.LastIndexByte(t, '-')
	if i <= 0 || i == len(t)-1 {
		return false
	}
	if _, err := strconv.Atoi(t[i+1:]); err != nil {
		return false
	}
	for _, r := range t[:i] {
		if (r < 'a' || r > 'z') && r != '-' {
			return false
		}
	}
	return true
}

// Written returns the newest comment time written for a thread, or the zero
// time when nothing has been.
func Written(dir, thread string) time.Time {
	raw, err := os.ReadFile(filepath.Join(dir, "written-"+thread))
	if err != nil {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		return time.Time{}
	}
	return at
}

func mark(dir, thread string, at time.Time) error {
	if !at.After(Written(dir, thread)) {
		return nil
	}
	return writeAtomic(dir, "written-"+thread, []byte(at.UTC().Format(time.RFC3339Nano)+"\n"))
}

// Take moves every pending message to delivered and returns the delivered
// paths, oldest first.
func Take(dir string) ([]string, error) {
	names, err := list(filepath.Join(dir, pendingDir))
	if err != nil || len(names) == 0 {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, deliveredDir), 0o755); err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		to := filepath.Join(dir, deliveredDir, n)
		if err := os.Rename(filepath.Join(dir, pendingDir, n), to); err != nil {
			return out, err
		}
		out = append(out, to)
	}
	return out, nil
}

// Settle reports, per thread, the newest message the agent was handed, and
// whether anything is still pending. The tick calls it when a run's process
// has gone: delivered messages were acted on and must not be replayed;
// pending ones reached nobody and must be.
func Settle(dir string) (delivered map[string]time.Time, pending bool, err error) {
	waiting, err := list(filepath.Join(dir, pendingDir))
	if err != nil {
		return nil, false, err
	}
	names, err := list(filepath.Join(dir, deliveredDir))
	if err != nil {
		return nil, false, err
	}
	delivered = map[string]time.Time{}
	for _, n := range names {
		at, thread, ok := parseName(n)
		if !ok {
			continue
		}
		if at.After(delivered[thread]) {
			delivered[thread] = at
		}
	}
	return delivered, len(waiting) > 0, nil
}

func parseName(n string) (time.Time, string, bool) {
	stamp, rest, ok := strings.Cut(strings.TrimSuffix(n, ".md"), "-")
	if !ok {
		return time.Time{}, "", false
	}
	nanos, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return time.Time{}, "", false
	}
	return time.Unix(0, nanos).UTC(), rest, true
}

func list(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func writeAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// Hook answers one agent CLI hook call: the event as JSON on in, the answer
// on out. It hands over whatever is pending — as context after a tool call, or
// as the reason an attempt to finish is turned back.
//
// It fails open. A hook that errors must not stop the agent's tool call or its
// finish, so every failure here prints nothing and returns the error for the
// caller to report on stderr, which the agent CLI keeps out of the agent's
// context.
func Hook(dir string, in io.Reader, out io.Writer) error {
	var event struct {
		Name string `json:"hook_event_name"`
	}
	if err := json.NewDecoder(in).Decode(&event); err != nil {
		return fmt.Errorf("inbox: read hook event: %w", err)
	}
	if event.Name != "PostToolUse" && event.Name != "Stop" {
		return nil
	}
	paths, err := Take(dir)
	if err != nil || len(paths) == 0 {
		return err
	}

	what := "a new comment"
	if len(paths) > 1 {
		what = strconv.Itoa(len(paths)) + " new comments"
	}
	note := fmt.Sprintf("claudeflow: the operator has left %s on this run's threads since you last read them. "+
		"Read %s now, before anything else:\n\n%s\n\n"+
		"This note comes from claudeflow, not from the tool above it.",
		what, map[bool]string{true: "it", false: "them"}[len(paths) == 1], "cat "+strings.Join(quoteAll(paths), " "))

	var answer any
	if event.Name == "Stop" {
		// Stop has no context field; a block's reason is what the agent reads,
		// and it reads it as a fresh turn.
		answer = map[string]string{"decision": "block", "reason": note}
	} else {
		answer = map[string]any{"hookSpecificOutput": map[string]string{
			"hookEventName":     "PostToolUse",
			"additionalContext": note,
		}}
	}
	return json.NewEncoder(out).Encode(answer)
}

func quoteAll(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
	}
	return out
}
