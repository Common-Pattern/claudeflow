package scaffold

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Common-Pattern/claudeflow/internal/config"
)

// The whole point of scaffolding a commented template rather than serialising
// the struct: nothing but a test stops it naming a key the parser rejects, and
// the parser rejects unknown keys.
func TestRenderedTemplateParses(t *testing.T) {
	cfg, err := config.Parse(Render(Facts{Repo: "acme/widgets", User: "alice", Base: "main"}), t.TempDir())
	if err != nil {
		t.Fatalf("the scaffolded config does not load: %v", err)
	}
	if cfg.Repo != "acme/widgets" || cfg.User != "alice" || cfg.Branches.Base != "main" {
		t.Errorf("detected facts did not reach the file: %+v", cfg)
	}
}

// A config with placeholders in it must still load, so that `doctor` can run
// against it and say what is left to fill in.
func TestTemplateWithPlaceholdersParses(t *testing.T) {
	cfg, err := config.Parse(Render(Facts{}), t.TempDir())
	if err != nil {
		t.Fatalf("the placeholder config does not load: %v", err)
	}
	if unfilled := Unfilled(cfg.Repo, cfg.User); len(unfilled) != 2 {
		t.Errorf("Unfilled = %v, want both placeholders reported", unfilled)
	}
}

func TestUnfilledIgnoresRealValues(t *testing.T) {
	if unfilled := Unfilled("acme/widgets", "alice"); len(unfilled) != 0 {
		t.Errorf("Unfilled = %v, want nothing for a filled-in config", unfilled)
	}
}

func TestWriteRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, Name)
	if err := os.WriteFile(path, []byte("# the operator's own file\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := Write(dir, Facts{}); !errors.Is(err, ErrExists) {
		t.Fatalf("Write over an existing config = %v, want ErrExists", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "the operator's own file") {
		t.Error("Write clobbered a configuration it was told not to touch")
	}
}

func TestWriteCreates(t *testing.T) {
	dir := t.TempDir()
	path, err := Write(dir, Facts{Repo: "acme/widgets", User: "alice", Base: "trunk"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "base: trunk") {
		t.Errorf("the detected base branch is not in the file:\n%s", raw)
	}
}

// Detection asks gh first, because it answers every fact at once and knows the
// default branch. A checkout gh does not recognise still yields the repository.
func TestDetectPrefersGH(t *testing.T) {
	run := func(_ context.Context, name string, args ...string) (string, error) {
		switch {
		case name == "gh" && args[0] == "repo":
			return "acme/widgets\ntrunk\n", nil
		case name == "gh" && args[0] == "api":
			return "alice\n", nil
		}
		return "", errors.New("unexpected command")
	}
	got := Detect(context.Background(), run, t.TempDir())
	if got != (Facts{Repo: "acme/widgets", User: "alice", Base: "trunk"}) {
		t.Errorf("Detect = %+v", got)
	}
}

func TestDetectFallsBackToTheGitRemote(t *testing.T) {
	run := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "git" {
			return "git@github.com:acme/widgets.git\n", nil
		}
		if name == "gh" && args[0] == "api" {
			return "alice\n", nil
		}
		return "", errors.New("gh does not know this remote")
	}
	got := Detect(context.Background(), run, t.TempDir())
	if got.Repo != "acme/widgets" || got.User != "alice" {
		t.Errorf("Detect = %+v, want the repository read off the remote", got)
	}
}

// Nothing detectable is not an error: a file with blanks to fill in beats no
// file and a message.
func TestDetectSurvivesAHostWithNothing(t *testing.T) {
	run := func(context.Context, string, ...string) (string, error) {
		return "", errors.New("no such command")
	}
	got := Detect(context.Background(), run, t.TempDir())
	if got != (Facts{}) {
		t.Errorf("Detect = %+v, want empty facts", got)
	}
	if len(got.Missing()) != 2 {
		t.Errorf("Missing = %v, want both facts reported as missing", got.Missing())
	}
}

func TestRepoFromRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:acme/widgets.git":              "acme/widgets",
		"https://github.com/acme/widgets.git":          "acme/widgets",
		"https://user@github.example.com/acme/widgets": "acme/widgets",
		"ssh://git@github.com/acme/widgets.git":        "acme/widgets",
		"not-a-remote":                                 "",
	}
	for in, want := range cases {
		if got := repoFromRemote(in); got != want {
			t.Errorf("repoFromRemote(%q) = %q, want %q", in, got, want)
		}
	}
}
