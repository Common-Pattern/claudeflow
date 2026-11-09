package transcripts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"
)

// maxBody bounds what one tool's input or output puts on the page. A run that
// reads a large file should not make the page unusable; the raw log has all of
// it.
const maxBody = 32 * 1024

// maxSummary bounds the one line that stands for a tool call.
const maxSummary = 160

// summaryKeys are the tool inputs that say what a call is doing, best first.
var summaryKeys = []string{"command", "file_path", "path", "pattern", "query", "url", "description", "prompt"}

// record is one line of the agent CLI's stream-json output. Only what the page
// shows is decoded.
type record struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Model   string `json:"model"`
	Cwd     string `json:"cwd"`
	// Timestamp is set on assistant and user messages only; the CLI stamps no
	// other event.
	Timestamp string `json:"timestamp"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Parent        string  `json:"parent_tool_use_id"`
	Result        string  `json:"result"`
	IsError       bool    `json:"is_error"`
	NumTurns      int     `json:"num_turns"`
	DurationMS    int64   `json:"duration_ms"`
	CostUSD       float64 `json:"total_cost_usd"`
	RateLimitInfo struct {
		Status string `json:"status"`
	} `json:"rate_limit_info"`
}

// block is one piece of a message: text, thinking, a tool call or its result.
type block struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Content  json.RawMessage `json:"content"`
	IsError  bool            `json:"is_error"`
}

// event is one thing shown on the page. With a Body it is a collapsible
// section headed by Label and Summary; without one it is a single line.
type event struct {
	Class   string
	Label   string
	Summary string
	Body    string
	Open    bool
	Sub     bool
	// Time is when the record was written, zero when the record carries none.
	Time time.Time
}

// eventTmpl is html/template because everything in an event is whatever a run
// printed, and the page around it is one a person has open in a browser.
//
// The time is written in UTC; the page's script rewrites it in the reader's
// zone, so the same link reads correctly wherever it is opened.
var eventTmpl = template.Must(template.New("event").Parse(
	`<div class="ev {{.Class}}{{if .Sub}} sub{{end}}"><span class="t">` +
		`{{if not .Time.IsZero}}<time datetime="{{.Time.Format "2006-01-02T15:04:05.000Z07:00"}}">{{.Time.Format "15:04:05"}}</time>{{end}}` +
		`</span><div class="b">` +
		`{{if .Body}}<details{{if .Open}} open{{end}}><summary>{{if .Label}}<b>{{.Label}}</b> {{end}}{{.Summary}}</summary><pre>{{.Body}}</pre></details>` +
		`{{else}}{{if .Label}}<b>{{.Label}}</b> {{end}}{{.Summary}}{{end}}</div></div>` + "\n"))

// render turns complete lines of a log into page fragments, and reports whether
// they include the agent's closing event.
//
// A log is mostly stream-json, with install hook output and anything the CLI
// wrote to stderr between the lines. Those are shown as they are. Events the
// page has no use for — token counts, rate limit headroom, summaries — are
// dropped, and a type this code has never seen is dropped with them, because
// the stream gains event types without notice and the raw log keeps them all.
func render(data []byte) (template.HTML, bool) {
	var out bytes.Buffer
	done := false
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec record
		if line[0] != '{' || json.Unmarshal(line, &rec) != nil || rec.Type == "" {
			write(&out, event{Class: "plain", Summary: string(line)})
			continue
		}
		if rec.Type == "result" {
			done = true
		}
		// One message can show as several events; only the first carries the
		// time, so a column of identical stamps does not bury the ones that move.
		stamp, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)
		for i, ev := range events(rec) {
			ev.Sub = rec.Parent != ""
			if i == 0 {
				ev.Time = stamp.UTC()
			}
			write(&out, ev)
		}
	}
	return template.HTML(out.String()), done
}

func write(out *bytes.Buffer, ev event) {
	// Writing to a buffer cannot fail, and the template is fixed.
	_ = eventTmpl.Execute(out, ev)
}

// events is what one record shows.
func events(rec record) []event {
	switch rec.Type {
	case "system":
		if rec.Subtype == "init" {
			return []event{{Class: "note", Summary: fmt.Sprintf("Agent started: %s in %s", rec.Model, rec.Cwd)}}
		}
	case "assistant", "user":
		return messageEvents(rec.Message.Content)
	case "rate_limit_event":
		if rec.RateLimitInfo.Status == "rejected" {
			return []event{{Class: "error", Summary: "The API refused a request on a rate limit."}}
		}
	case "result":
		return []event{closing(rec)}
	}
	return nil
}

func messageEvents(content json.RawMessage) []event {
	var blocks []block
	if err := json.Unmarshal(content, &blocks); err != nil {
		// A message handed to a subagent is a bare string, not a list of blocks.
		var text string
		if json.Unmarshal(content, &text) == nil && text != "" {
			return []event{{Class: "prompt", Label: "Prompt", Summary: clip(text), Body: bounded(text)}}
		}
		return nil
	}
	var evs []event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			evs = append(evs, event{Class: "say", Summary: b.Text})
		case "thinking":
			// The CLI usually sends these with the text withheld.
			if b.Thinking != "" {
				evs = append(evs, event{Class: "think", Label: "Thinking", Summary: clip(b.Thinking), Body: bounded(b.Thinking)})
			}
		case "tool_use":
			evs = append(evs, toolCall(b))
		case "tool_result":
			evs = append(evs, toolResult(b))
		}
	}
	return evs
}

func toolCall(b block) event {
	var input map[string]any
	_ = json.Unmarshal(b.Input, &input)

	summary := ""
	for _, key := range summaryKeys {
		if s, ok := input[key].(string); ok && s != "" {
			summary = clip(s)
			break
		}
	}

	keys := make([]string, 0, len(input))
	for k := range input {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var body strings.Builder
	for _, k := range keys {
		// Strings go through unquoted: an edit's old and new text is meant to
		// be read with its line breaks, not as one escaped line.
		if s, ok := input[k].(string); ok {
			fmt.Fprintf(&body, "%s: %s\n", k, s)
			continue
		}
		raw, _ := json.Marshal(input[k])
		fmt.Fprintf(&body, "%s: %s\n", k, raw)
	}
	return event{Class: "tool", Label: b.Name, Summary: summary, Body: bounded(body.String())}
}

func toolResult(b block) event {
	text := resultText(b.Content)
	ev := event{Class: "output", Label: "Output", Summary: clip(text), Body: bounded(text)}
	if text == "" {
		ev.Summary = "(empty)"
	}
	if b.IsError {
		ev.Class, ev.Label, ev.Open = "output error", "Error", true
	}
	return ev
}

// resultText reads a tool result, which is a string or a list of blocks.
func resultText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var blocks []block
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		} else {
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}

// closing reports how the agent finished. The closing text of a successful run
// repeats its last message, so only a failure's is shown.
func closing(rec record) event {
	took := (time.Duration(rec.DurationMS) * time.Millisecond).Round(time.Second)
	summary := fmt.Sprintf("Finished in %s, %d turns, $%.2f", took, rec.NumTurns, rec.CostUSD)
	if rec.IsError {
		return event{Class: "error", Summary: strings.TrimSpace("Failed (" + rec.Subtype + ") after " + took.String() + ". " + rec.Result)}
	}
	return event{Class: "note", Summary: summary}
}

// clip reduces text to the first line, short enough to head a section.
func clip(s string) string {
	s = strings.TrimSpace(s)
	first, _, more := strings.Cut(s, "\n")
	runes := []rune(first)
	if len(runes) > maxSummary {
		return string(runes[:maxSummary]) + "…"
	}
	if more {
		return first + " …"
	}
	return first
}

// bounded caps a section's body, cutting on a character boundary.
func bounded(s string) string {
	if len(s) <= maxBody {
		return s
	}
	return strings.ToValidUTF8(s[:maxBody], "") + fmt.Sprintf("\n… %d more bytes in the raw log", len(s)-maxBody)
}
