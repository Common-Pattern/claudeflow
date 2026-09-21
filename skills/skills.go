// Package skills carries the agent instructions claudeflow ships with.
//
// They are embedded rather than installed alongside the binary because they are
// the binary's contract: they name its labels, its environment variables and
// its exit codes. A skill file that drifts out of step with the binary produces
// a run that does the wrong thing confidently, which is the worst failure this
// system has. Shipping them in the same artifact makes that drift impossible.
//
// A project that needs different instructions points config at its own
// directory; see config.Agent.SkillsDir.
package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed issue/SKILL.md review/SKILL.md planning/SKILL.md
var embedded embed.FS

// Name identifies one shipped skill.
type Name string

// The skills, one per kind of run.
const (
	// Issue implements a labelled issue and lands it.
	Issue Name = "issue"
	// Review addresses comments on the standing pull request.
	Review Name = "review"
	// Planning is conversation only: no branch, no files.
	Planning Name = "planning"
)

// All returns every shipped skill name.
func All() []Name { return []Name{Issue, Review, Planning} }

// Read returns a skill's markdown, preferring an override on disk.
//
// overrideDir may be empty. A directory that exists but lacks the skill falls
// back to the embedded copy, so a project can replace one skill without
// vendoring all three.
func Read(name Name, overrideDir string) ([]byte, error) {
	if overrideDir != "" {
		path := filepath.Join(overrideDir, string(name), "SKILL.md")
		switch raw, err := os.ReadFile(path); {
		case err == nil:
			return raw, nil
		case !os.IsNotExist(err):
			return nil, fmt.Errorf("read skill override %s: %w", path, err)
		}
	}
	raw, err := embedded.ReadFile(string(name) + "/SKILL.md")
	if err != nil {
		return nil, fmt.Errorf("read embedded skill %s: %w", name, err)
	}
	return raw, nil
}

// Install writes the skills into dir, one directory per skill, so a run can
// load them the way the agent CLI expects. Existing files are replaced.
func Install(dir, overrideDir string) error {
	for _, name := range All() {
		raw, err := Read(name, overrideDir)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, "claudeflow-"+string(name))
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("create skill dir %s: %w", target, err)
		}
		if err := os.WriteFile(filepath.Join(target, "SKILL.md"), raw, 0o644); err != nil {
			return fmt.Errorf("write skill %s: %w", name, err)
		}
	}
	return nil
}

// FS exposes the embedded files for callers that want to walk them.
func FS() fs.FS { return embedded }
