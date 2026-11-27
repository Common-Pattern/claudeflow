package tick

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/git"
	"github.com/Common-Pattern/claudeflow/internal/hostlogin"
	"github.com/Common-Pattern/claudeflow/internal/sandbox"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

// containerSandbox gives every run a fresh container: its own clone of the
// branch, its own Compose stack on a podman nested inside, and the agent.
//
// What reaches the host is the run's log, its comment inbox, and whatever it
// pushes. A run that leaves a mess leaves it in a container that is deleted
// when it is reaped.
type containerSandbox struct {
	e Engine
	p sandbox.Podman
}

// NewContainerSandbox builds the container sandbox for e's configuration.
func NewContainerSandbox(e Engine) (Sandbox, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find the claudeflow binary to mount into sandboxes: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	c := e.Cfg.Sandbox
	p := sandbox.Podman{
		Bin:    c.Bin,
		Image:  c.Image,
		Prefix: "claudeflow-" + slug(e.Cfg.Repo),
		Repo:   e.Cfg.Repo,
		Pass:   c.Env,
		Mounts: c.Mounts,
		Self:   self,
		Mirror: gitDir(e.Cfg.Paths.Root),
	}
	return containerSandbox{e: e, p: p}, nil
}

// workdir is where a run's checkout lives inside its container.
func (s containerSandbox) workdir() string {
	_, name, _ := strings.Cut(s.e.Cfg.Repo, "/")
	return "/home/podman/work/" + name
}

func (s containerSandbox) remote() string {
	if s.e.Cfg.Sandbox.Remote != "" {
		return s.e.Cfg.Sandbox.Remote
	}
	return "https://github.com/" + s.e.Cfg.Repo + ".git"
}

func (s containerSandbox) Launch(ctx context.Context, l Launch) (state.Run, error) {
	e, r := s.e, l.Run
	cfg := e.Cfg
	workdir := s.workdir()

	spec := sandbox.Spec{
		RunID:      r.ID(),
		Remote:     s.remote(),
		Workdir:    workdir,
		Branch:     r.Branch,
		StartPoint: cfg.Branches.Integration,
		Stack:      r.Kind.HoldsSlot(),
		Command:    l.Command,
		Args:       l.Args,
		Env:        maps.Clone(map[string]string(l.Env)),
		Timeout:    l.Timeout,
		Log:        r.Log,
	}
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	var binaries []string
	if bin := hostBinary(l.Command); bin != "" {
		// The host's own build, so a run reports the version the host does
		// and cannot drift from it. It must not update itself in a mount the
		// run cannot write to.
		spec.Command = bin
		spec.Env["DISABLE_AUTOUPDATER"] = "1"
		binaries = append(binaries, bin)
	}
	// The host's Claude Code directory, shared at its own path: the login and
	// the locks the CLI refreshes it under, and the history the run's
	// transcripts belong in. A run is one more process using it, as a local
	// run is. See package hostlogin.
	claudeDir := hostlogin.ConfigDir()
	if _, err := os.Stat(claudeDir); err != nil {
		e.logf("%s: no Claude Code directory on this host to share with the sandbox (%v); the agent will have no login unless sandbox.env or sandbox.mounts gives it one", r.ID(), err)
		claudeDir = ""
	} else {
		spec.Env["CLAUDE_CONFIG_DIR"] = claudeDir
		if err := hostlogin.ShareAccount(hostlogin.AccountPath(), hostlogin.SharedAccountPath()); err != nil {
			e.logf("%s: could not give sandboxes the host's Claude account (%v); runs may report as a new install", r.ID(), err)
		}
	}
	if s.p.Mirror != "" {
		spec.Mirror = sandbox.MirrorPath
	}
	spec.AuthorName, spec.AuthorEmail = authorOf(ctx, e.Repo)
	if spec.Stack {
		spec.Install = cfg.Hooks.Install
		spec.ComposeFile = cfg.Compose.File
		spec.ComposeProfiles = cfg.Compose.Profiles
		spec.Expose = cfg.Compose.Expose
		spec.ReadyTimeout = cfg.Compose.ReadyTimeout
		spec.AgentEnv = cfg.Agent.Env
		// compose.env is not passed: it exists to point Compose at the
		// host's engine, and inside the sandbox the engine is the nested one.
		spec.ComposeEnv = map[string]string{
			"CLAUDEFLOW_SLOT":     strconv.Itoa(r.Slot),
			"CLAUDEFLOW_WORKTREE": workdir,
			"CLAUDEFLOW_BRANCH":   r.Branch,
			"CLAUDEFLOW_PROJECT":  "cf",
		}
	}

	specPath := s.specPath(r.ID())
	if err := spec.Write(specPath); err != nil {
		return r, fmt.Errorf("write sandbox spec: %w", err)
	}

	secrets := map[string]string{}
	if token := githubToken(ctx); token != "" {
		secrets["GH_TOKEN"] = token
	} else if strings.HasPrefix(spec.Remote, "https://") {
		e.logf("%s: no GitHub token found (GH_TOKEN, GITHUB_TOKEN or `gh auth token`); the sandbox will not be able to push", r.ID())
	}

	start := sandbox.Start{
		RunID:    r.ID(),
		Slot:     r.Slot,
		Spec:     specPath,
		Log:      r.Log,
		Inbox:    l.Env["CLAUDEFLOW_INBOX"],
		Secrets:  secrets,
		Binaries: binaries,
	}
	if claudeDir != "" {
		start.ClaudeDir, start.ClaudePath = claudeDir, claudeDir
	}

	r.Started = e.now()
	name, err := s.p.Launch(ctx, start)
	if err != nil {
		return r, err
	}
	r.Sandbox = name
	r.PID = 0
	r.Worktree = ""
	return r, nil
}

func (s containerSandbox) specPath(id string) string {
	return filepath.Join(s.e.Store.Dir(), "sandbox", id+".json")
}

// Alive errs towards alive. Reading a live run as dead tears its container
// down under its agent; reading a dead one as alive costs a tick.
func (s containerSandbox) Alive(r state.Run) bool {
	if r.Sandbox == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	running, err := s.p.Running(ctx, r.Sandbox)
	if err != nil {
		s.e.logf("%s: could not inspect sandbox %s, assuming it is still running: %v", r.ID(), r.Sandbox, err)
		return true
	}
	return running
}

func (s containerSandbox) Stop(r state.Run) error {
	if r.Sandbox == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return s.p.Stop(ctx, r.Sandbox)
}

func (s containerSandbox) Teardown(ctx context.Context, r state.Run) error {
	if r.Sandbox == "" {
		return nil
	}
	_ = os.Remove(s.specPath(r.ID()))
	return s.p.Remove(ctx, r.Sandbox)
}

// hostBinary is the agent CLI as the host runs it, when it is a single native
// executable that can run in the sandbox as it is: the resolved path, or "".
//
// An install that is a script — npm's is a Node entry point beside its
// dependencies — is not one file to mount, and the image's own copy is used.
func hostBinary(command string) string {
	path, err := exec.LookPath(command)
	if err != nil {
		return ""
	}
	if path, err = filepath.EvalSymlinks(path); err != nil {
		return ""
	}
	if path, err = filepath.Abs(path); err != nil {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != "\x7fELF" {
		return ""
	}
	return path
}

func (s containerSandbox) Busy(ctx context.Context, slot int) (bool, error) {
	return s.p.SlotBusy(ctx, slot)
}

// landingWorktree gives landing a checkout of a run's branch as pushed, for a
// run whose work happened somewhere the host cannot see.
//
// Landing merges the integration branch in when the pull request has fallen
// behind, and pushes the result; that needs a checkout. This one belongs to
// the supervisor alone — no agent ever runs in it — and is reset to the remote
// branch every time, so nothing a previous landing left in it carries over.
func (e Engine) landingWorktree(ctx context.Context, r state.Run) (string, error) {
	if r.Branch == "" {
		return "", fmt.Errorf("%s has no branch to land from", r.ID())
	}
	path := filepath.Join(e.Cfg.Paths.Worktrees, fmt.Sprintf("%s-%d-land", r.Kind, r.Ref))
	if err := e.Repo.Fetch(ctx); err != nil {
		return "", fmt.Errorf("fetch before landing: %w", err)
	}
	if err := e.ensureWorktree(ctx, path, r.Branch); err != nil {
		return "", err
	}
	if _, err := git.New(path).Run(ctx, "reset", "--hard", "origin/"+r.Branch); err != nil {
		return "", fmt.Errorf("reset the landing checkout to origin/%s: %w", r.Branch, err)
	}
	return path, nil
}

// githubToken finds the token a sandbox authenticates to GitHub with: the
// environment's, or the one gh has stored for the operator.
func githubToken(ctx context.Context) string {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// authorOf is the git identity the operator commits under in the checkout, so
// a sandboxed run's commits read the same as a host run's.
func authorOf(ctx context.Context, repo *git.Repo) (name, email string) {
	if repo == nil {
		return "", ""
	}
	name, _ = repo.Run(ctx, "config", "user.name")
	email, _ = repo.Run(ctx, "config", "user.email")
	return strings.TrimSpace(name), strings.TrimSpace(email)
}

// gitDir is the checkout's git directory, for clones to borrow objects from.
// Empty when there is none, and a clone then fetches everything.
func gitDir(root string) string {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// slug makes a repository name safe for container and volume names.
func slug(repo string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(repo) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
