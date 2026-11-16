package tick

import (
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
