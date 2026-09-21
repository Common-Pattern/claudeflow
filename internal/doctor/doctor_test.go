package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Common-Pattern/claudeflow/internal/config"
)

// fake builds Options whose tooling is entirely scripted, so the checks can be
// tested on a host that has none of it — or all of it working.
type fake struct {
	missing map[string]bool
	out     map[string]string
	fails   map[string]bool
}

func newFake() *fake {
	return &fake{missing: map[string]bool{}, out: map[string]string{}, fails: map[string]bool{}}
}

func (f *fake) options(cfg config.Config, haveConfig bool) Options {
	return Options{
		Cfg: cfg, HaveConfig: haveConfig,
		Look: func(name string) (string, error) {
			if f.missing[name] {
				return "", errors.New("not found")
			}
			return "/usr/bin/" + name, nil
		},
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			key := strings.TrimSpace(name + " " + strings.Join(args, " "))
			// Failures are matched independently of canned output: a command
			// can be scripted to fail without also having a recorded stdout.
			for k := range f.fails {
				if strings.HasPrefix(key, k) {
					return f.out[k], errors.New("command failed")
				}
			}
			for k, v := range f.out {
				if strings.HasPrefix(key, k) {
					return v, nil
				}
			}
			return "ok", nil
		},
	}
}

func healthy(t *testing.T) (*fake, config.Config) {
	t.Helper()
	root := t.TempDir()
	composeFile := filepath.Join(root, "compose.yaml")
	if err := os.WriteFile(composeFile, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := config.Default()
	cfg.Repo, cfg.User = "acme/widgets", "alice"
	cfg.Compose.File = "compose.yaml"
	cfg.Compose.Bin = []string{"docker", "compose"}
	cfg.Paths.Root = root
	cfg.Paths.State = filepath.Join(root, ".claudeflow")

	f := newFake()
	f.out["git --version"] = "git version 2.53.0"
	f.out["git config --get user.name"] = "Ada"
	f.out["git config --get user.email"] = "ada@example.com"
	f.out["gh --version"] = "gh version 2.98.0"
	f.out["gh auth status"] = "github.com\n  ✓ Logged in to github.com account alice"
	f.out["claude --version"] = "2.1.278"
	f.out["docker compose version"] = "Docker Compose version v5.5.1"
	return f, cfg
}

func find(r Report, name string) Result {
	for _, x := range r {
		if x.Name == name {
			return x
		}
	}
	return Result{Name: name, Status: "missing"}
}

func TestHealthyHostPasses(t *testing.T) {
	f, cfg := healthy(t)
	r := Run(t.Context(), f.options(cfg, true))
	if r.Failed() {
		t.Errorf("a healthy host reported failures:\n%s", r)
	}
	ok, _, fail := r.Counts()
	if fail != 0 || ok == 0 {
		t.Errorf("counts: %d ok, %d fail", ok, fail)
	}
}

// The check that matters most: landing creates a merge commit, and without an
// identity it fails part-way through a run, after the work is done.
func TestMissingGitIdentityFails(t *testing.T) {
	f, cfg := healthy(t)
	f.out["git config --get user.email"] = ""
	r := Run(t.Context(), f.options(cfg, true))

	got := find(r, "git identity")
	if got.Status != Fail {
		t.Errorf("git identity = %s, want fail", got.Status)
	}
	if !strings.Contains(got.Fix, "git config") {
		t.Errorf("fix = %q, want it to give the command", got.Fix)
	}
	if !r.Failed() {
		t.Error("report does not read as failed")
	}
}

func TestUnauthenticatedGHFails(t *testing.T) {
	f, cfg := healthy(t)
	f.fails["gh auth status"] = true
	r := Run(t.Context(), f.options(cfg, true))

	got := find(r, "gh auth")
	if got.Status != Fail {
		t.Errorf("gh auth = %s, want fail", got.Status)
	}
	if !strings.Contains(got.Fix, "gh auth login") {
		t.Errorf("fix = %q, want the login command", got.Fix)
	}
}

func TestMissingToolsFail(t *testing.T) {
	for _, tool := range []string{"git", "gh", "claude"} {
		t.Run(tool, func(t *testing.T) {
			f, cfg := healthy(t)
			f.missing[tool] = true
			r := Run(t.Context(), f.options(cfg, true))
			if !r.Failed() {
				t.Errorf("missing %s did not fail the report:\n%s", tool, r)
			}
		})
	}
}

// A provider that accepts being invoked and then hangs is one of the faults
// this exists to find, so the check asks it something rather than only looking
// it up on PATH.
func TestComposeThatWillNotRunFails(t *testing.T) {
	f, cfg := healthy(t)
	f.fails["docker compose version"] = true
	r := Run(t.Context(), f.options(cfg, true))

	got := find(r, "compose")
	if got.Status != Fail {
		t.Errorf("compose = %s, want fail", got.Status)
	}
}

func TestMissingComposeFileFails(t *testing.T) {
	f, cfg := healthy(t)
	cfg.Compose.File = "absent.yaml"
	r := Run(t.Context(), f.options(cfg, true))

	if got := find(r, "compose file"); got.Status != Fail {
		t.Errorf("compose file = %s, want fail", got.Status)
	}
}

func TestUnreadableRepoFails(t *testing.T) {
	f, cfg := healthy(t)
	f.fails["gh repo view"] = true
	r := Run(t.Context(), f.options(cfg, true))

	got := find(r, "repo access")
	if got.Status != Fail {
		t.Errorf("repo access = %s, want fail", got.Status)
	}
	if !strings.Contains(got.Detail, cfg.Repo) {
		t.Errorf("detail = %q, want it to name the repo", got.Detail)
	}
}

// A checkout without the integration branch fetched is worth saying, but it is
// one `git fetch` away and does not stop a run being attempted.
func TestMissingIntegrationRefWarnsRatherThanFails(t *testing.T) {
	f, cfg := healthy(t)
	f.fails["git -C "+cfg.Paths.Root+" rev-parse --verify"] = true
	f.out["git -C "+cfg.Paths.Root+" rev-parse --verify"] = ""
	r := Run(t.Context(), f.options(cfg, true))

	got := find(r, "checkout")
	if got.Status != Warn {
		t.Errorf("checkout = %s, want warn", got.Status)
	}
	if r.Failed() {
		t.Error("a warning made the whole report fail")
	}
}

func TestNotAGitRepositoryFails(t *testing.T) {
	f, cfg := healthy(t)
	f.fails["git -C "+cfg.Paths.Root+" rev-parse --git-dir"] = true
	f.out["git -C "+cfg.Paths.Root+" rev-parse --git-dir"] = ""
	r := Run(t.Context(), f.options(cfg, true))

	if got := find(r, "checkout"); got.Status != Fail {
		t.Errorf("checkout = %s, want fail", got.Status)
	}
}

// Without a config the tooling checks still run: "is this host capable" is a
// useful question on its own.
func TestWithoutConfigStillChecksTooling(t *testing.T) {
	f, _ := healthy(t)
	r := Run(t.Context(), f.options(config.Config{}, false))

	if got := find(r, "git"); got.Status != OK {
		t.Errorf("git = %s, want ok even with no config", got.Status)
	}
	if got := find(r, "configuration"); got.Status != Warn {
		t.Errorf("configuration = %s, want warn", got.Status)
	}
	if r.Failed() {
		t.Errorf("no config should not fail the report:\n%s", r)
	}
	// Checks that need a config must not have run.
	if got := find(r, "repo access"); got.Status != "missing" {
		t.Error("repo access ran without a configuration")
	}
}

func TestUnwritableStateDirFails(t *testing.T) {
	f, cfg := healthy(t)
	blocked := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(blocked, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg.Paths.State = filepath.Join(blocked, "state")

	r := Run(t.Context(), f.options(cfg, true))
	if os.Geteuid() == 0 {
		t.Skip("running as root; permissions do not apply")
	}
	if got := find(r, "state directory"); got.Status != Fail {
		t.Errorf("state directory = %s, want fail", got.Status)
	}
}

func TestReportRendering(t *testing.T) {
	r := Report{
		{"git", OK, "git version 2.53.0", ""},
		{"gh auth", Fail, "not authenticated", "run: gh auth login"},
		{"checkout", Warn, "no origin/preview", "git fetch origin"},
	}
	out := r.String()
	for _, want := range []string{"ok  ", "FAIL", "warn", "gh auth login", "1 ok, 1 warning(s), 1 failure(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report missing %q:\n%s", want, out)
		}
	}
	// A passing check's fix is noise.
	if strings.Count(out, "git version 2.53.0") != 1 {
		t.Error("unexpected rendering of the passing check")
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("a\nb"); got != "a" {
		t.Errorf("= %q, want a", got)
	}
	if got := firstLine("only"); got != "only" {
		t.Errorf("= %q, want only", got)
	}
}

func ExampleReport_String() {
	r := Report{{"git", OK, "git version 2.53.0", ""}}
	fmt.Print(r)
	// Output:
	// ok    git                    git version 2.53.0
	//
	// 1 ok, 0 warning(s), 0 failure(s)
}

// Some providers print a banner before answering. podman announces which
// external implementation it delegates to, so the first line of output is not
// reliably the version.
func TestVersionLineSkipsBanners(t *testing.T) {
	out := ">>>> Executing external compose provider \"/usr/bin/docker-compose\". <<<<\n\nDocker Compose version v5.5.1"
	if got := versionLine(out); got != "Docker Compose version v5.5.1" {
		t.Errorf("versionLine = %q, want the version", got)
	}
}

func TestVersionLineStripsColour(t *testing.T) {
	if got := versionLine("\x1b[4mgit version 2.53.0\x1b[0m"); got != "git version 2.53.0" {
		t.Errorf("versionLine = %q", got)
	}
}

func TestVersionLineFallsBackToFirstRealLine(t *testing.T) {
	if got := versionLine(">>>> banner <<<<\n\nsomething else"); got != "something else" {
		t.Errorf("versionLine = %q, want the first non-banner line", got)
	}
}
