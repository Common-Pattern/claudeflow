package tick

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Common-Pattern/claudeflow/internal/state"
	"github.com/Common-Pattern/claudeflow/skills"
)

func TestPromptNoteReachesEveryRunKind(t *testing.T) {
	e, _ := newEngine(t)
	e.Cfg.Agent.PromptNote = "  Use the stack you were given, not scripts/lane.sh.\n"

	for _, k := range []state.Kind{state.KindBuild, state.KindReview, state.KindPlan} {
		args, err := e.agentArgs(Start{Kind: k, Ref: 7})
		if err != nil {
			t.Fatal(err)
		}
		prompt := args[slices.Index(args, "--append-system-prompt")+1]
		if !strings.Contains(prompt, "## About this project\n\nUse the stack you were given, not scripts/lane.sh.\n") {
			t.Errorf("%s run: the project's note is missing from its system prompt", k)
		}
	}

	// The note sits between the skill and the inbox note, so the inbox
	// instructions stay last, where they were written to be read.
	args, _ := e.agentArgs(Start{Kind: state.KindBuild, Ref: 7})
	prompt := args[slices.Index(args, "--append-system-prompt")+1]
	if strings.Index(prompt, "About this project") > strings.Index(prompt, "Comments that arrive while you work") {
		t.Error("the project's note comes after the inbox note")
	}
}

func TestNoPromptNoteLeavesTheSkillAlone(t *testing.T) {
	e, _ := newEngine(t)
	instructions, err := skills.Read(skills.Review, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range []string{"", " \n\t"} {
		e.Cfg.Agent.PromptNote = note
		if e.systemPrompt(state.KindReview, instructions) != string(instructions) {
			t.Errorf("note %q changed the prompt", note)
		}
	}
}

// The agent CLI's stream echoes neither prompt, so the log has them only if
// claudeflow writes them there.
func TestTheLogStartsWithWhatTheAgentWasGiven(t *testing.T) {
	e, _ := newEngine(t)
	args, err := e.agentArgs(Start{Kind: state.KindBuild, Ref: 7})
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "logs", "build-7.log")
	e.logPrompt("build-7", logPath, args)

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Type         string `json:"type"`
		Prompt       string `json:"prompt"`
		SystemPrompt string `json:"system_prompt"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("log is not one JSON line: %v\n%s", err, raw)
	}
	if rec.Type != "claudeflow_prompt" || !strings.Contains(rec.Prompt, "GitHub issue #7") ||
		rec.SystemPrompt != args[slices.Index(args, "--append-system-prompt")+1] {
		t.Errorf("log does not hold the prompts: %+v", rec)
	}
}
