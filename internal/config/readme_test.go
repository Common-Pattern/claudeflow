package config

import (
	"os"
	"strings"
	"testing"
)

// The README's configuration example must actually load.
//
// It drifted once: after environment hooks were replaced by a required Compose
// file, the documented example still showed `envUp`, `envDown` and `busyCheck`
// and had no `compose:` block at all — so anyone following it wrote a config
// that failed validation on the first run. Documentation that does not work is
// worse than none, and nothing else checks it.
func TestREADMEExampleLoads(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Skipf("README not readable from here: %v", err)
	}

	yaml, ok := firstYAMLBlock(string(raw))
	if !ok {
		t.Fatal("no yaml code block found in the README")
	}
	cfg, err := Parse([]byte(yaml), t.TempDir())
	if err != nil {
		t.Fatalf("the README's example does not load: %v", err)
	}

	// Spot-check that it is the real example and not some fragment.
	if cfg.Repo == "" || cfg.User == "" {
		t.Errorf("parsed example has no repo/user: %+v", cfg)
	}
	if cfg.Compose.File == "" {
		t.Error("the example omits compose.file, which is required")
	}
}

// firstYAMLBlock returns the contents of the first ```yaml fence.
func firstYAMLBlock(md string) (string, bool) {
	const open = "```yaml\n"
	i := strings.Index(md, open)
	if i < 0 {
		return "", false
	}
	rest := md[i+len(open):]
	j := strings.Index(rest, "\n```")
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}
