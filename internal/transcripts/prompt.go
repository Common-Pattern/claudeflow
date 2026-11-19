package transcripts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// promptType marks the line claudeflow writes at the head of a run's log.
const promptType = "claudeflow_prompt"

// WritePrompt appends what an agent is about to be given to its log, as one
// line in the shape of the stream around it.
//
// The agent CLI's stream echoes neither the prompt nor the appended system
// prompt, so without this a transcript shows everything the agent did and
// nothing of what it was asked. Anything else that reads the log skips a type
// it does not know.
func WritePrompt(path, prompt, systemPrompt string, at time.Time) error {
	line, err := json.Marshal(struct {
		Type         string `json:"type"`
		Timestamp    string `json:"timestamp"`
		Prompt       string `json:"prompt,omitempty"`
		SystemPrompt string `json:"system_prompt,omitempty"`
	}{promptType, at.UTC().Format(time.RFC3339Nano), prompt, systemPrompt})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("prepare log dir for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log %s: %w", path, err)
	}
	_, werr := f.Write(append(line, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}
