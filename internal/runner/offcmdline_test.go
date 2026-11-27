package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An agent's instructions on its command line can be matched by anything that
// reads process command lines, the agent itself included.
func TestPromptOffCommandLineStillReachesTheAgent(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "agent")
	// Prints its arguments, its standard input, and the file named after
	// --append-system-prompt-file.
	body := `#!/bin/sh
echo "args: $*"
echo "stdin: $(cat)"
while [ $# -gt 0 ]; do
  if [ "$1" = --append-system-prompt-file ]; then echo "system: $(cat "$2")"; fi
  shift
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "run.log")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	proc, err := Start(context.Background(), Spawn{
		Command:              script,
		Args:                 []string{"-p", "work issue 7 with vite", "--append-system-prompt", "kill vite carefully", "--verbose"},
		Dir:                  dir,
		LogPath:              log,
		PromptOffCommandLine: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res := proc.Wait(); res.ExitCode != 0 {
		t.Fatalf("exit %d", res.ExitCode)
	}
	raw, _ := os.ReadFile(log)
	out := string(raw)
	args := strings.SplitN(out, "\n", 2)[0]
	if strings.Contains(args, "vite") {
		t.Errorf("command line still carries the prompt: %q", args)
	}
	if !strings.Contains(args, "-p --append-system-prompt-file /dev/fd/3 --verbose") {
		t.Errorf("args = %q", args)
	}
	for _, want := range []string{"stdin: work issue 7 with vite", "system: kill vite carefully"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left %d file(s) behind in the temp directory", len(left))
	}
}
