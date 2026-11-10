package inbox

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 10, 5, 0, 0, 0, time.UTC)

func msg(thread string, at time.Time, body string) Message {
	return Message{Thread: thread, At: at, Where: "issue #12", URL: "https://example/c/1", Body: body}
}

func hook(t *testing.T, dir, event string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := Hook(dir, strings.NewReader(`{"hook_event_name":"`+event+`"}`), &out); err != nil {
		t.Fatalf("Hook(%s): %v", event, err)
	}
	if out.Len() == 0 {
		return nil
	}
	var answer map[string]any
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
		t.Fatalf("Hook(%s) printed %q: %v", event, out.String(), err)
	}
	return answer
}

func TestHookHandsOverAfterAToolCallOnce(t *testing.T) {
	dir := t.TempDir()
	if hook(t, dir, "PostToolUse") != nil {
		t.Fatal("an empty inbox produced hook output")
	}
	if err := Write(dir, msg("issue-12", t0, "Use Postgres, not SQLite.")); err != nil {
		t.Fatal(err)
	}

	answer := hook(t, dir, "PostToolUse")
	specific, _ := answer["hookSpecificOutput"].(map[string]any)
	note, _ := specific["additionalContext"].(string)
	if specific["hookEventName"] != "PostToolUse" || !strings.Contains(note, "cat '") {
		t.Fatalf("answer = %v, want context naming the file to read", answer)
	}
	path := strings.TrimSuffix(strings.SplitN(note, "cat '", 2)[1], "")
	path = path[:strings.IndexByte(path, '\'')]
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the note names %q, which does not read: %v", path, err)
	}
	if !strings.Contains(string(raw), "Use Postgres, not SQLite.") || !strings.Contains(string(raw), "issue #12") {
		t.Errorf("message file = %q", raw)
	}

	if hook(t, dir, "PostToolUse") != nil {
		t.Error("a message was handed over twice")
	}
}

// A comment that lands after the agent's last tool call must stop it from
// finishing without reading it.
func TestHookBlocksAFinishWithMailUnread(t *testing.T) {
	dir := t.TempDir()
	if hook(t, dir, "Stop") != nil {
		t.Fatal("Stop blocked with nothing pending")
	}
	_ = Write(dir, msg("issue-12", t0, "one"))
	_ = Write(dir, msg("review-pr-40", t0.Add(time.Second), "two"))

	answer := hook(t, dir, "Stop")
	reason, _ := answer["reason"].(string)
	if answer["decision"] != "block" || !strings.Contains(reason, "2 new comments") {
		t.Fatalf("answer = %v, want a block naming both", answer)
	}
	if hook(t, dir, "Stop") != nil {
		t.Error("Stop blocked again with nothing new; the agent could never finish")
	}
}

func TestHookIgnoresOtherEvents(t *testing.T) {
	dir := t.TempDir()
	_ = Write(dir, msg("issue-12", t0, "x"))
	if hook(t, dir, "SessionStart") != nil {
		t.Fatal("answered an event it does not handle")
	}
	if _, pending, _ := Settle(dir); !pending {
		t.Error("an unhandled event consumed the message")
	}
}

func TestSettleSplitsDeliveredFromPending(t *testing.T) {
	dir := t.TempDir()
	_ = Write(dir, msg("issue-12", t0, "a"))
	_ = Write(dir, msg("issue-12", t0.Add(time.Minute), "b"))
	_ = Write(dir, msg("review-pr-40", t0.Add(2*time.Minute), "c"))
	if _, err := Take(dir); err != nil {
		t.Fatal(err)
	}
	_ = Write(dir, msg("issue-12", t0.Add(time.Hour), "late"))

	delivered, pending, err := Settle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Error("the late message is not reported pending")
	}
	if !delivered["issue-12"].Equal(t0.Add(time.Minute)) || !delivered["review-pr-40"].Equal(t0.Add(2*time.Minute)) {
		t.Errorf("delivered = %v", delivered)
	}
}

func TestWriteTracksTheThreadAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	m := msg("issue-12", t0, "a")
	_ = Write(dir, m)
	if _, err := Take(dir); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, m); err != nil {
		t.Fatal(err)
	}
	if _, pending, _ := Settle(dir); pending {
		t.Error("rewriting a delivered message queued it again")
	}
	if got := Written(dir, "issue-12"); !got.Equal(t0) {
		t.Errorf("Written = %v, want %v", got, t0)
	}
	if got := Written(dir, "issue-99"); !got.IsZero() {
		t.Errorf("Written on an unknown thread = %v, want zero", got)
	}
}

func TestWriteRejectsAPathAsAThread(t *testing.T) {
	for _, bad := range []string{"", "../etc-1", "issue-", "issue-x", "Issue-1"} {
		if err := Write(t.TempDir(), msg(bad, t0, "x")); err == nil {
			t.Errorf("Write accepted thread %q", bad)
		}
	}
}
