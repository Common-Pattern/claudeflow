// Package config loads and validates a project's claudeflow configuration.
//
// Everything a project needs to say about itself lives in one YAML file:
// which repository, whose comments count as instructions, what the labels and
// branches are called, how much may run at once, and the hooks that bring a
// working environment up and tear it down. The orchestration itself is fixed;
// only these knobs move between projects.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is a project's complete claudeflow configuration.
type Config struct {
	// Repo is the GitHub repository in "owner/name" form.
	Repo string `yaml:"repo"`
	// User is the single account whose issue and PR comments are treated as
	// instructions. The agent comments on the same threads, so without this
	// filter it would answer itself indefinitely.
	User string `yaml:"user"`

	Labels   Labels   `yaml:"labels"`
	Branches Branches `yaml:"branches"`
	Limits   Limits   `yaml:"limits"`
	Slots    Slots    `yaml:"slots"`
	Hooks    Hooks    `yaml:"hooks"`
	Agent    Agent    `yaml:"agent"`
	Paths    Paths    `yaml:"paths"`
}

// Labels names the labels that carry the state machine. They are configurable
// because label vocabulary is a house style, not a mechanism.
type Labels struct {
	// Queued is applied by a human and means "work this".
	Queued string `yaml:"queued"`
	// Working is the claim. Swapping Queued for Working is the lock.
	Working string `yaml:"working"`
	// Landed means the work is merged into the integration branch.
	Landed string `yaml:"landed"`
	// Question means the agent asked something and is waiting on a reply.
	Question string `yaml:"question"`
	// Blocked means something broke, as distinct from Question.
	Blocked string `yaml:"blocked"`
	// Planning, alongside Queued, inverts the run into conversation only.
	Planning string `yaml:"planning"`
}

// All returns every configured label, in a stable order.
func (l Labels) All() []string {
	return []string{l.Queued, l.Working, l.Landed, l.Question, l.Blocked, l.Planning}
}

// Resolution returns the labels that end a run, which a re-queue must clear.
func (l Labels) Resolution() []string {
	return []string{l.Landed, l.Question, l.Blocked}
}

// Branches names the two branches the workflow moves work between.
type Branches struct {
	// Base is what the integration branch eventually merges into, and is never
	// touched by the agent. Usually "main".
	Base string `yaml:"base"`
	// Integration is where the agent lands work and where a human tests it.
	// Equal to Base for projects with no staging branch.
	Integration string `yaml:"integration"`
	// Prefix is prepended to every branch the agent creates.
	Prefix string `yaml:"prefix"`
}

// SingleBranch reports whether the project lands directly on its base branch,
// in which case there is no standing integration pull request to maintain.
func (b Branches) SingleBranch() bool { return b.Base == b.Integration }

// Limits caps what may run at once and for how long.
type Limits struct {
	// MaxBuilds is how many slot-holding runs may be live at once.
	MaxBuilds int `yaml:"maxBuilds"`
	// MaxPlanning is budgeted separately: planning runs hold no slot and
	// finish in minutes, so queueing them behind builds would turn a
	// conversation into a day.
	MaxPlanning int `yaml:"maxPlanning"`

	BuildTimeout time.Duration `yaml:"buildTimeout"`
	PlanTimeout  time.Duration `yaml:"planTimeout"`

	// RateLimitBackoff is how long dispatch pauses after the agent CLI reports
	// a usage limit. Plan capacity is shared with the operator's own sessions,
	// so a run that retries into a limit takes capacity from the person it is
	// working for.
	RateLimitBackoff time.Duration `yaml:"rateLimitBackoff"`

	// FixAttempts is how many times a run may push a fix at failing CI before
	// giving up.
	FixAttempts int `yaml:"fixAttempts"`
}

// Slots describes the pool of isolated environments runs are allocated from.
//
// A slot is whatever the project needs it to be: a port block and a database,
// a container set, a cloud namespace. claudeflow only allocates the number and
// hands it to the hooks.
type Slots struct {
	// Min and Max bound the allocatable range, inclusive. A project that
	// reserves slot 0 for its main stack sets Min to 1.
	Min int `yaml:"min"`
	Max int `yaml:"max"`

	// BusyCheck is run with CLAUDEFLOW_SLOT set and decides whether a slot is
	// in use by anything at all, including work claudeflow did not start.
	// Exit 0 means busy.
	//
	// This exists because slot resources are usually global to the machine
	// rather than to one checkout, so claudeflow's own bookkeeping cannot see
	// another checkout's environment occupying the slot.
	BusyCheck string `yaml:"busyCheck"`
}

// Hooks are the project-owned commands that make a checkout workable. Each runs
// with CLAUDEFLOW_SLOT, CLAUDEFLOW_WORKTREE, CLAUDEFLOW_BRANCH and
// CLAUDEFLOW_ISSUE in the environment.
type Hooks struct {
	// Install prepares a fresh worktree, typically a dependency install.
	Install string `yaml:"install"`
	// EnvUp brings the slot's environment up and seeds it.
	EnvUp string `yaml:"envUp"`
	// EnvDown tears it down. It must be safe to run twice and must not destroy
	// shared resources.
	EnvDown string `yaml:"envDown"`
	// Verify is the project's full check suite, named here so the skills can
	// refer to it without knowing the project.
	Verify string `yaml:"verify"`
}

// Agent describes how to invoke the coding agent.
type Agent struct {
	// Command is the agent CLI binary.
	Command string `yaml:"command"`
	// Model is passed through to it.
	Model string `yaml:"model"`
	// ExtraArgs are appended to every invocation.
	ExtraArgs []string `yaml:"extraArgs"`
	// SkillsDir overrides the skills shipped with claudeflow.
	SkillsDir string `yaml:"skillsDir"`
	// AutoUpdate runs the agent CLI's own update command once a day, when
	// nothing is live.
	AutoUpdate bool `yaml:"autoUpdate"`
}

// Paths locates the checkout and claudeflow's own state.
type Paths struct {
	// Root is the repository checkout runs are created from, and the one that
	// is fast-forwarded after a merge.
	Root string `yaml:"root"`
	// State holds run records, seen-markers and logs. Defaults to
	// <Root>/.claudeflow.
	State string `yaml:"state"`
	// Worktrees is where run worktrees are created. Defaults to
	// <State>/worktrees.
	Worktrees string `yaml:"worktrees"`
}

// Default returns a configuration with every optional field populated. Only
// Repo, User and Paths.Root have no sensible default.
func Default() Config {
	return Config{
		Labels: Labels{
			Queued:   "claude",
			Working:  "claude:working",
			Landed:   "claude:landed",
			Question: "claude:question",
			Blocked:  "claude:blocked",
			Planning: "claude:planning",
		},
		// Integration is deliberately left empty: it is derived from Base after
		// decoding, so that a config setting only `base` gets an integration
		// branch that follows it. Pre-filling it here would make it non-empty,
		// the derive step would skip it, and overriding `base` alone would
		// silently leave work landing on "main".
		Branches: Branches{Base: "main", Prefix: "claude/"},
		Limits: Limits{
			MaxBuilds:        2,
			MaxPlanning:      2,
			BuildTimeout:     150 * time.Minute,
			PlanTimeout:      30 * time.Minute,
			RateLimitBackoff: time.Hour,
			FixAttempts:      3,
		},
		Slots: Slots{Min: 1, Max: 4},
		Agent: Agent{Command: "claude", Model: "opus", AutoUpdate: true},
	}
}

// Load reads and validates a configuration file, applying defaults for every
// field the file leaves out.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw, filepath.Dir(path))
}

// Parse decodes a configuration from YAML. Relative paths resolve against
// baseDir, so a config file can sit inside the checkout it describes.
func Parse(raw []byte, baseDir string) (Config, error) {
	cfg := Default()
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDerivedDefaults(baseDir)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) applyDerivedDefaults(baseDir string) {
	if c.Branches.Integration == "" {
		c.Branches.Integration = c.Branches.Base
	}
	if c.Paths.Root == "" {
		c.Paths.Root = baseDir
	}
	if !filepath.IsAbs(c.Paths.Root) && baseDir != "" {
		c.Paths.Root = filepath.Join(baseDir, c.Paths.Root)
	}
	if c.Paths.State == "" {
		c.Paths.State = filepath.Join(c.Paths.Root, ".claudeflow")
	}
	if c.Paths.Worktrees == "" {
		c.Paths.Worktrees = filepath.Join(c.Paths.State, "worktrees")
	}
}

// ErrInvalid is the sentinel every validation failure wraps.
var ErrInvalid = errors.New("invalid config")

// Validate reports the first thing wrong with the configuration.
func (c Config) Validate() error {
	if c.Repo == "" {
		return fmt.Errorf("%w: repo is required", ErrInvalid)
	}
	if owner, name, ok := strings.Cut(c.Repo, "/"); !ok || owner == "" || name == "" {
		return fmt.Errorf("%w: repo %q must be owner/name", ErrInvalid, c.Repo)
	}
	if c.User == "" {
		return fmt.Errorf("%w: user is required — without it the agent would treat its own comments as instructions", ErrInvalid)
	}
	for name, label := range map[string]string{
		"queued": c.Labels.Queued, "working": c.Labels.Working,
		"landed": c.Labels.Landed, "question": c.Labels.Question,
		"blocked": c.Labels.Blocked, "planning": c.Labels.Planning,
	} {
		if label == "" {
			return fmt.Errorf("%w: labels.%s must not be empty", ErrInvalid, name)
		}
	}
	if dup := firstDuplicate(c.Labels.All()); dup != "" {
		return fmt.Errorf("%w: label %q is used for more than one state", ErrInvalid, dup)
	}
	if c.Branches.Base == "" {
		return fmt.Errorf("%w: branches.base is required", ErrInvalid)
	}
	if c.Slots.Min > c.Slots.Max {
		return fmt.Errorf("%w: slots.min (%d) exceeds slots.max (%d)", ErrInvalid, c.Slots.Min, c.Slots.Max)
	}
	if c.Slots.Min < 0 {
		return fmt.Errorf("%w: slots.min must not be negative", ErrInvalid)
	}
	if c.Limits.MaxBuilds < 0 || c.Limits.MaxPlanning < 0 {
		return fmt.Errorf("%w: limits must not be negative", ErrInvalid)
	}
	if n := c.Slots.Max - c.Slots.Min + 1; c.Limits.MaxBuilds > n {
		return fmt.Errorf("%w: limits.maxBuilds (%d) exceeds the %d available slots", ErrInvalid, c.Limits.MaxBuilds, n)
	}
	if c.Limits.BuildTimeout <= 0 || c.Limits.PlanTimeout <= 0 {
		return fmt.Errorf("%w: timeouts must be positive", ErrInvalid)
	}
	if c.Hooks.EnvUp != "" && c.Hooks.EnvDown == "" {
		return fmt.Errorf("%w: hooks.envUp is set without hooks.envDown, which would leak an environment per run", ErrInvalid)
	}
	if c.Paths.Root == "" {
		return fmt.Errorf("%w: paths.root is required", ErrInvalid)
	}
	return nil
}

// Owner returns the repository owner.
func (c Config) Owner() string { owner, _, _ := strings.Cut(c.Repo, "/"); return owner }

// Name returns the repository name.
func (c Config) Name() string { _, name, _ := strings.Cut(c.Repo, "/"); return name }

// SlotCount returns how many slots the pool holds.
func (c Config) SlotCount() int { return c.Slots.Max - c.Slots.Min + 1 }

func firstDuplicate(values []string) string {
	seen := make(map[string]struct{}, len(values))
	for _, v := range values {
		if _, dup := seen[v]; dup {
			return v
		}
		seen[v] = struct{}{}
	}
	return ""
}
