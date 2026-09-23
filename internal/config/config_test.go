package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every project must ship a Compose file, so it is part of the minimum.
const minimal = `
repo: acme/widgets
user: alice
compose:
  file: claudeflow/compose.yaml
`

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal), "/checkout")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Labels.Queued != "claude" {
		t.Errorf("Labels.Queued = %q, want claude", cfg.Labels.Queued)
	}
	if cfg.Limits.MaxBuilds != 2 {
		t.Errorf("MaxBuilds = %d, want 2", cfg.Limits.MaxBuilds)
	}
	if cfg.Limits.BuildTimeout != 150*time.Minute {
		t.Errorf("BuildTimeout = %v, want 150m", cfg.Limits.BuildTimeout)
	}
	if cfg.Agent.Command != "claude" {
		t.Errorf("Agent.Command = %q, want claude", cfg.Agent.Command)
	}
}

func TestParseOverridesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal+`
labels:
  queued: bot
limits:
  maxBuilds: 1
  buildTimeout: 45m
`), "/checkout")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Labels.Queued != "bot" {
		t.Errorf("Labels.Queued = %q, want bot", cfg.Labels.Queued)
	}
	if cfg.Limits.MaxBuilds != 1 {
		t.Errorf("MaxBuilds = %d, want 1", cfg.Limits.MaxBuilds)
	}
	if cfg.Limits.BuildTimeout != 45*time.Minute {
		t.Errorf("BuildTimeout = %v, want 45m", cfg.Limits.BuildTimeout)
	}
	// A field the override did not mention keeps its default.
	if cfg.Labels.Blocked != "claude:blocked" {
		t.Errorf("Labels.Blocked = %q, want the default", cfg.Labels.Blocked)
	}
}

// Integration defaulting to Base is what lets a project with no staging branch
// leave the whole branches block out.
func TestIntegrationDefaultsToBase(t *testing.T) {
	cfg, err := Parse([]byte(minimal+"branches:\n  base: trunk\n"), "/checkout")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Branches.Integration != "trunk" {
		t.Errorf("Integration = %q, want trunk", cfg.Branches.Integration)
	}
	if !cfg.Branches.SingleBranch() {
		t.Error("SingleBranch() = false, want true when base == integration")
	}
}

func TestPathDefaultsResolveAgainstBaseDir(t *testing.T) {
	cfg, err := Parse([]byte(minimal), "/checkout")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Paths.Root != "/checkout" {
		t.Errorf("Root = %q, want /checkout", cfg.Paths.Root)
	}
	if want := filepath.Join("/checkout", ".claudeflow"); cfg.Paths.State != want {
		t.Errorf("State = %q, want %q", cfg.Paths.State, want)
	}
	if want := filepath.Join("/checkout", ".claudeflow", "worktrees"); cfg.Paths.Worktrees != want {
		t.Errorf("Worktrees = %q, want %q", cfg.Paths.Worktrees, want)
	}
}

func TestRelativeRootResolvesAgainstBaseDir(t *testing.T) {
	cfg, err := Parse([]byte(minimal+"paths:\n  root: ../app\n"), "/etc/claudeflow")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := filepath.Join("/etc/claudeflow", "../app"); cfg.Paths.Root != want {
		t.Errorf("Root = %q, want %q", cfg.Paths.Root, want)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"missing repo", "user: alice\n", "repo is required"},
		{"malformed repo", "repo: widgets\nuser: alice\n", "must be owner/name"},
		{"repo with empty owner", "repo: /widgets\nuser: alice\n", "must be owner/name"},
		{"missing user", "repo: a/b\n", "user is required"},
		{"empty label", minimal + "labels:\n  blocked: \"\"\n", "labels.blocked"},
		{"duplicate labels", minimal + "labels:\n  landed: claude:blocked\n", "more than one state"},
		{"slots inverted", minimal + "slots:\n  min: 5\n  max: 2\n", "exceeds slots.max"},
		{"negative slot", minimal + "slots:\n  min: -1\n  max: 2\n", "must not be negative"},
		{"builds exceed slots", minimal + "slots:\n  min: 1\n  max: 2\nlimits:\n  maxBuilds: 9\n", "available slots"},
		{"zero timeout", minimal + "limits:\n  planTimeout: 0s\n", "timeouts must be positive"},
		{"no compose file", "repo: a/b\nuser: u\n", "compose.file is required"},
		{"zero ready timeout", "repo: a/b\nuser: u\ncompose:\n  file: c.yaml\n  readyTimeout: 0s\n", "compose.readyTimeout"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml), "/checkout")
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want error containing %q", tc.name, tc.wantErr)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error does not wrap ErrInvalid: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidAcceptsAFullConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
repo: acme/widgets
user: alice
branches:
  base: main
  integration: preview
  prefix: claude/
limits:
  maxBuilds: 2
  maxPlanning: 2
slots:
  min: 1
  max: 6
compose:
  file: claudeflow/compose.yaml
  projectPrefix: cf
  profiles: [spa]
hooks:
  install: pnpm install --frozen-lockfile
  verify: pnpm verify:ci
`), "/checkout")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Owner() != "acme" || cfg.Name() != "widgets" {
		t.Errorf("Owner/Name = %q/%q", cfg.Owner(), cfg.Name())
	}
	if cfg.SlotCount() != 6 {
		t.Errorf("SlotCount() = %d, want 6", cfg.SlotCount())
	}
	if cfg.Branches.SingleBranch() {
		t.Error("SingleBranch() = true, want false when integration differs from base")
	}
	if got := cfg.Compose.Project(3); got != "cf-3" {
		t.Errorf("Compose.Project(3) = %q, want cf-3", got)
	}
}

// Each slot is its own Compose project; that naming is what makes a teardown
// unable to reach another run's resources.
func TestComposeProjectNaming(t *testing.T) {
	if got := (Compose{}).Project(2); got != "cf-2" {
		t.Errorf("default prefix: Project(2) = %q, want cf-2", got)
	}
	if got := (Compose{ProjectPrefix: "sb"}).Project(5); got != "sb-5" {
		t.Errorf("Project(5) = %q, want sb-5", got)
	}
	a, b := (Compose{}).Project(1), (Compose{}).Project(2)
	if a == b {
		t.Error("two slots produced the same project name")
	}
}

func TestLabelsHelpers(t *testing.T) {
	l := Default().Labels
	if got := len(l.All()); got != 6 {
		t.Errorf("All() returned %d labels, want 6", got)
	}
	res := l.Resolution()
	if len(res) != 3 {
		t.Fatalf("Resolution() returned %d, want 3", len(res))
	}
	for _, label := range res {
		if label == l.Queued || label == l.Working {
			t.Errorf("Resolution() must not include the in-flight label %q", label)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("Load on a missing file succeeded, want error")
	}
}

// A relative root propagates into every derived path, and a relative path is
// meaningless to anything that does not share this process's working
// directory. A Compose bind mount refuses one outright.
func TestPathsAreAlwaysAbsolute(t *testing.T) {
	cfg, err := Parse([]byte(minimal+"paths:\n  root: .\n"), ".")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for name, p := range map[string]string{
		"Root": cfg.Paths.Root, "State": cfg.Paths.State, "Worktrees": cfg.Paths.Worktrees,
	} {
		if !filepath.IsAbs(p) {
			t.Errorf("Paths.%s = %q, want an absolute path", name, p)
		}
	}
}

func TestExplicitRelativePathsAreResolved(t *testing.T) {
	cfg, err := Parse([]byte(minimal+"paths:\n  root: .\n  state: var/flow\n"), ".")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !filepath.IsAbs(cfg.Paths.State) {
		t.Errorf("Paths.State = %q, want absolute", cfg.Paths.State)
	}
	if !filepath.IsAbs(cfg.Paths.Worktrees) {
		t.Errorf("Paths.Worktrees = %q, want absolute", cfg.Paths.Worktrees)
	}
}

// The queued label marks membership, not state: it is applied once and never
// removed. Swapping it for the working label on every claim churned the
// timeline for no information and made a running issue look un-owned.
func TestIsQueued(t *testing.T) {
	l := Default().Labels
	for _, tc := range []struct {
		name   string
		labels []string
		want   bool
	}{
		{"queued only", []string{l.Queued}, true},
		{"queued with an unrelated label", []string{l.Queued, "bug"}, true},
		{"queued and planning", []string{l.Queued, l.Planning}, true},
		{"running", []string{l.Queued, l.Working}, false},
		{"landed", []string{l.Queued, l.Landed}, false},
		{"answered", []string{l.Queued, l.Question}, false},
		{"blocked", []string{l.Queued, l.Blocked}, false},
		{"not ours", []string{"bug"}, false},
		{"nothing", nil, false},
	} {
		if got := l.IsQueued(tc.labels); got != tc.want {
			t.Errorf("IsQueued(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A key the tool does not read is a mistake, not a comment. A lenient parser
// accepts a typo, a key under the wrong block, and a setting left behind by a
// version that stopped reading it — and in each case the operator has written
// down an intention the tool silently does not hold.
func TestUnknownFieldIsRejected(t *testing.T) {
	raw := []byte(`
repo: acme/widgets
user: alice
compose:
  file: compose.yaml
agent:
  autoUpdate: true
`)
	_, err := Parse(raw, t.TempDir())
	if err == nil {
		t.Fatal("Parse accepted a key no version of the tool reads")
	}
	if !strings.Contains(err.Error(), "autoUpdate") {
		t.Errorf("err = %v, want it to name the offending key", err)
	}
}

// paths.state is relative to the checkout, not to whatever directory the
// process happens to be in: `claudeflow -c ../other/claudeflow.yaml status`
// otherwise read an empty state directory — no runs, every issue free to
// dispatch again — and created a stray one where it stood.
func TestRelativeStateResolvesAgainstRoot(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`
repo: acme/widgets
user: alice
compose:
  file: compose.yaml
paths:
  root: .
  state: .claudeflow
`)
	cfg, err := Parse(raw, dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := filepath.Join(cfg.Paths.Root, ".claudeflow")
	if cfg.Paths.State != want {
		t.Errorf("state = %q, want %q", cfg.Paths.State, want)
	}
	if cfg.Paths.Worktrees != filepath.Join(want, "worktrees") {
		t.Errorf("worktrees = %q, want it under the state directory", cfg.Paths.Worktrees)
	}
}
