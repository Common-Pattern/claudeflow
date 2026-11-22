package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllSkillsAreEmbedded(t *testing.T) {
	for _, name := range All() {
		raw, err := Read(name, "")
		if err != nil {
			t.Fatalf("Read(%s): %v", name, err)
		}
		if len(raw) == 0 {
			t.Errorf("skill %s is empty", name)
		}
		if !strings.HasPrefix(string(raw), "---") {
			t.Errorf("skill %s does not open with YAML frontmatter, so the agent CLI will not index it", name)
		}
		if !strings.Contains(string(raw), "name:") {
			t.Errorf("skill %s has no name in its frontmatter", name)
		}
	}
}

// The skills name the binary's environment variables. A skill that stops
// mentioning them is a run that will not know where it is.
func TestSkillsReferenceTheContract(t *testing.T) {
	for name, required := range map[Name][]string{
		Issue:    {"CLAUDEFLOW_ISSUE", "CLAUDEFLOW_BRANCH", "CLAUDEFLOW_VERIFY"},
		Review:   {"CLAUDEFLOW_PR", "CLAUDEFLOW_USER"},
		Planning: {"CLAUDEFLOW_ISSUE", "CLAUDEFLOW_LABEL_QUESTION"},
		Fix:      {"CLAUDEFLOW_PR", "CLAUDEFLOW_FAILING_CHECKS", "CLAUDEFLOW_VERIFY"},
		Revise:   {"CLAUDEFLOW_PR", "CLAUDEFLOW_ISSUE", "CLAUDEFLOW_USER", "CLAUDEFLOW_BRANCH", "CLAUDEFLOW_VERIFY"},
		Resolve:  {"CLAUDEFLOW_PR", "CLAUDEFLOW_ISSUE", "CLAUDEFLOW_BRANCH", "CLAUDEFLOW_INTEGRATION", "CLAUDEFLOW_VERIFY"},
	} {
		raw, err := Read(name, "")
		if err != nil {
			t.Fatalf("Read(%s): %v", name, err)
		}
		for _, want := range required {
			if !strings.Contains(string(raw), want) {
				t.Errorf("skill %s does not mention %q", name, want)
			}
		}
	}
}

// The planning skill's entire point is that it writes nothing.
func TestPlanningSkillForbidsWriting(t *testing.T) {
	raw, err := Read(Planning, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	body := string(raw)
	for _, want := range []string{"no branch", "no commit"} {
		if !strings.Contains(body, want) {
			t.Errorf("planning skill does not say %q", want)
		}
	}
}

func TestReadPrefersAnOverride(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, string(Issue))
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("custom"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	raw, err := Read(Issue, dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(raw) != "custom" {
		t.Errorf("Read = %q, want the override", raw)
	}
}

// A project replacing one skill must not have to vendor all three.
func TestReadFallsBackPerSkill(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, string(Issue))
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("custom"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	raw, err := Read(Planning, dir)
	if err != nil {
		t.Fatalf("Read(Planning): %v", err)
	}
	if string(raw) == "custom" || len(raw) == 0 {
		t.Error("Planning did not fall back to the embedded copy")
	}
}

func TestInstallWritesEverySkill(t *testing.T) {
	dir := t.TempDir()
	if err := Install(dir, ""); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, name := range All() {
		path := filepath.Join(dir, "claudeflow-"+string(name), "SKILL.md")
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("missing %s: %v", path, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", path)
		}
	}
}

// Install runs on every start, including into a worktree a previous run used.
func TestInstallIsRepeatable(t *testing.T) {
	dir := t.TempDir()
	for i := range 2 {
		if err := Install(dir, ""); err != nil {
			t.Fatalf("Install call %d: %v", i+1, err)
		}
	}
}

func TestReadUnknownSkill(t *testing.T) {
	if _, err := Read(Name("nope"), ""); err == nil {
		t.Fatal("Read of an unknown skill succeeded, want error")
	}
}

// Waiting on checks is claudeflow's job now. A skill that still tells the agent
// to watch them puts back the cost the split removed — an environment and a
// model session held for a run's worth of polling.
func TestNoSkillWaitsOnChecks(t *testing.T) {
	for _, name := range All() {
		raw, err := Read(name, "")
		if err != nil {
			t.Fatalf("Read(%s): %v", name, err)
		}
		if strings.Contains(string(raw), "--watch") {
			t.Errorf("skill %s still tells the agent to watch checks", name)
		}
	}
}

// Every skill must say the run has no second turn: it is the instruction whose
// absence left a finished pull request unmerged.
func TestEverySkillSaysThereIsNoSecondTurn(t *testing.T) {
	for _, name := range []Name{Issue, Review, Fix, Resolve} {
		raw, err := Read(name, "")
		if err != nil {
			t.Fatalf("Read(%s): %v", name, err)
		}
		if !strings.Contains(string(raw), "no second turn") {
			t.Errorf("skill %s does not say the run has no second turn", name)
		}
	}
}

// An issue run must find out where the issue sits in its family: a sub-issue
// reads its parent for context, and a parent works its sub-issues in turn and
// closes each one through its pull request. Without this, a parent's run sees
// only the parent's body, and its sub-issues stay open after the work ships.
func TestIssueSkillHandlesParentsAndSubIssues(t *testing.T) {
	raw, err := Read(Issue, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"--json parent,subIssues",
		"read the parent for context",
		"one at a time",
		"for every sub-issue",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("issue skill does not say %q", want)
		}
	}
	raw, err = Read(Planning, "")
	if err != nil {
		t.Fatalf("Read(Planning): %v", err)
	}
	if !strings.Contains(string(raw), "--json parent,subIssues") {
		t.Error("planning skill does not look up the issue's parent and sub-issues")
	}
}

// Dependencies are the user's: claudeflow enforces the recorded ones, and the
// agent asks about any it finds rather than recording them itself. An agent
// that recorded its own would hold work on its own judgement, with nothing
// prompting a human to check it.
func TestIssueSkillLeavesDependenciesToTheUser(t *testing.T) {
	raw, err := Read(Issue, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	body := string(raw)
	if strings.Contains(body, "addBlockedBy") {
		t.Error("issue skill tells the agent to record dependencies itself")
	}
	if !strings.Contains(body, "Do not add, remove or change one") {
		t.Error("issue skill does not say dependencies are the user's")
	}
}

// The pull request is forgotten once it merges; the issue is what the user
// comes back to. Every skill that ends with an issue comment has to put what
// it left unfixed there, not only in the pull request body.
func TestSkillsPutUnresolvedProblemsOnTheIssue(t *testing.T) {
	for _, name := range []Name{Issue, Revise, Fix} {
		raw, err := Read(name, "")
		if err != nil {
			t.Fatalf("Read(%s): %v", name, err)
		}
		body := string(raw)
		if !strings.Contains(body, "did not fix") && !strings.Contains(body, "left alone") {
			t.Errorf("skill %s does not say where unresolved problems go", name)
		}
		if !strings.Contains(body, "not only in") && !strings.Contains(body, "the issue comment is the record") {
			t.Errorf("skill %s does not say the pull request body alone is not enough", name)
		}
	}
}
