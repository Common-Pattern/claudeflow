// Package doctor checks that a host can actually run claudeflow.
//
// Every check here corresponds to something that has failed in practice, and
// all of them share a shape: the failure arrives late, during a run, long after
// the cause. A missing git identity surfaces as a merge conflict message after
// an agent has done an hour's work; a compose provider that hangs surfaces as a
// run that never starts. Finding them before the first run costs seconds.
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/compose"
	"github.com/Common-Pattern/claudeflow/internal/config"
)

// Status is how a check came out.
type Status string

const (
	// OK means the check passed.
	OK Status = "ok"
	// Warn means claudeflow will run, but something is worth knowing.
	Warn Status = "warn"
	// Fail means a run would not get far.
	Fail Status = "fail"
)

// Result is one check's outcome.
type Result struct {
	Name   string
	Status Status
	Detail string
	// Fix is what to do about it, shown only when the check did not pass.
	Fix string
}

// Report is every result from one run.
type Report []Result

// Failed reports whether anything failed outright.
func (r Report) Failed() bool {
	for _, x := range r {
		if x.Status == Fail {
			return true
		}
	}
	return false
}

// Counts returns how many of each status the report holds.
func (r Report) Counts() (ok, warn, fail int) {
	for _, x := range r {
		switch x.Status {
		case OK:
			ok++
		case Warn:
			warn++
		case Fail:
			fail++
		}
	}
	return ok, warn, fail
}

// String renders a report for a terminal.
func (r Report) String() string {
	var b strings.Builder
	for _, x := range r {
		mark := map[Status]string{OK: "ok  ", Warn: "warn", Fail: "FAIL"}[x.Status]
		fmt.Fprintf(&b, "%s  %-22s %s\n", mark, x.Name, x.Detail)
		if x.Fix != "" && x.Status != OK {
			fmt.Fprintf(&b, "      %s\n", x.Fix)
		}
	}
	ok, warn, fail := r.Counts()
	fmt.Fprintf(&b, "\n%d ok, %d warning(s), %d failure(s)\n", ok, warn, fail)
	return b.String()
}

// Runner executes a command and returns its combined output. It exists so the
// checks can be tested without the tools they look for.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner runs commands for real, with a short timeout.
//
// The timeout matters: a compose provider that hangs is one of the faults this
// package exists to find, and a doctor that hangs alongside it reports nothing.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Options configures a run.
type Options struct {
	// Cfg is the loaded configuration. The zero value skips every check that
	// needs one, so `doctor` still reports on tooling without a config.
	Cfg config.Config
	// HaveConfig distinguishes a zero Cfg from a real one.
	HaveConfig bool
	// Run executes commands. Nil means ExecRunner.
	Run Runner
	// Look reports a binary's path. Nil means exec.LookPath.
	Look func(string) (string, error)
}

func (o Options) run() Runner {
	if o.Run != nil {
		return o.Run
	}
	return ExecRunner
}

func (o Options) look() func(string) (string, error) {
	if o.Look != nil {
		return o.Look
	}
	return exec.LookPath
}

// Run performs every check and returns the report.
func Run(ctx context.Context, o Options) Report {
	var r Report
	r = append(r, checkGit(ctx, o), checkGitIdentity(ctx, o), checkGH(ctx, o), checkGHAuth(ctx, o))
	if o.HaveConfig {
		r = append(r, checkRepoAccess(ctx, o))
	}
	r = append(r, checkAgent(ctx, o), checkCompose(ctx, o))
	if o.HaveConfig {
		r = append(r, checkComposeFile(ctx, o), checkStateDir(o), checkCheckout(ctx, o))
	} else {
		r = append(r, Result{
			Name: "configuration", Status: Warn,
			Detail: "no config file found",
			Fix:    "run from a directory with claudeflow.yaml, or pass -c <path>, to check the rest",
		})
	}
	return r
}

func checkGit(ctx context.Context, o Options) Result {
	path, err := o.look()("git")
	if err != nil {
		return Result{"git", Fail, "not on PATH", "install git"}
	}
	out, err := o.run()(ctx, "git", "--version")
	if err != nil {
		return Result{"git", Fail, "found but would not run: " + path, err.Error()}
	}
	return Result{"git", OK, out, ""}
}

// checkGitIdentity is here because landing creates a merge commit.
//
// Without an identity that merge fails with "Committer identity unknown" —
// after the agent has done all of the work, and from inside a run where nobody
// is watching. A fresh container or CI runner is the usual place it bites.
func checkGitIdentity(ctx context.Context, o Options) Result {
	name, nameErr := o.run()(ctx, "git", "config", "--get", "user.name")
	email, emailErr := o.run()(ctx, "git", "config", "--get", "user.email")
	if nameErr != nil || emailErr != nil || name == "" || email == "" {
		return Result{
			"git identity", Fail, "user.name or user.email is unset",
			`landing creates a merge commit and will fail part-way through a run: git config --global user.email "you@example.com"`,
		}
	}
	return Result{"git identity", OK, fmt.Sprintf("%s <%s>", name, email), ""}
}

func checkGH(ctx context.Context, o Options) Result {
	if _, err := o.look()("gh"); err != nil {
		return Result{"gh", Fail, "not on PATH", "install the GitHub CLI: https://cli.github.com"}
	}
	out, err := o.run()(ctx, "gh", "--version")
	if err != nil {
		return Result{"gh", Fail, "found but would not run", err.Error()}
	}
	return Result{"gh", OK, firstLine(out), ""}
}

// checkGHAuth matters more than the usual "is it installed" check: claudeflow
// holds no token of its own, so whatever gh can do is exactly what it can do.
func checkGHAuth(ctx context.Context, o Options) Result {
	out, err := o.run()(ctx, "gh", "auth", "status")
	if err != nil {
		return Result{
			"gh auth", Fail, "not authenticated",
			"run: gh auth login",
		}
	}
	account := "authenticated"
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "account") {
			account = strings.TrimSpace(line)
			break
		}
	}
	return Result{"gh auth", OK, account, ""}
}

func checkRepoAccess(ctx context.Context, o Options) Result {
	repo := o.Cfg.Repo
	if _, err := o.run()(ctx, "gh", "repo", "view", repo, "--json", "name"); err != nil {
		return Result{
			"repo access", Fail, "cannot read " + repo,
			"check the name in claudeflow.yaml, and that this account can see it",
		}
	}
	return Result{"repo access", OK, repo, ""}
}

func checkAgent(ctx context.Context, o Options) Result {
	cmd := o.Cfg.Agent.Command
	if cmd == "" {
		cmd = "claude"
	}
	if _, err := o.look()(cmd); err != nil {
		return Result{"agent cli", Fail, cmd + " not on PATH", "install it, or set agent.command"}
	}
	out, err := o.run()(ctx, cmd, "--version")
	if err != nil {
		return Result{"agent cli", Fail, cmd + " found but would not run", err.Error()}
	}
	return Result{"agent cli", OK, firstLine(out), ""}
}

// checkCompose resolves the implementation and asks it its version.
//
// Asking is the point. Some providers accept being invoked and then hang on
// the first real command, which presents as a run that never starts rather
// than as an error.
func checkCompose(ctx context.Context, o Options) Result {
	bin := o.Cfg.Compose.Bin
	if len(bin) == 0 {
		detected, err := compose.Detect()
		if err != nil {
			return Result{
				"compose", Fail, "no implementation found",
				"install docker compose or podman compose, or set compose.bin",
			}
		}
		bin = detected
	}
	args := append(append([]string{}, bin[1:]...), "version")
	out, err := o.run()(ctx, bin[0], args...)
	if err != nil {
		return Result{
			"compose", Fail, strings.Join(bin, " ") + " would not run",
			"check compose.bin and compose.env in the configuration",
		}
	}
	return Result{"compose", OK, fmt.Sprintf("%s — %s", strings.Join(bin, " "), versionLine(out)), ""}
}

func checkComposeFile(ctx context.Context, o Options) Result {
	rel := o.Cfg.Compose.File
	path := rel
	if !filepath.IsAbs(path) {
		path = filepath.Join(o.Cfg.Paths.Root, rel)
	}
	if _, err := os.Stat(path); err != nil {
		return Result{"compose file", Fail, "not found: " + path, "set compose.file, relative to paths.root"}
	}
	return Result{"compose file", OK, path, ""}
}

func checkStateDir(o Options) Result {
	dir := o.Cfg.Paths.State
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{"state directory", Fail, "cannot create " + dir, err.Error()}
	}
	probe := filepath.Join(dir, ".doctor-write-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		return Result{"state directory", Fail, "not writable: " + dir, err.Error()}
	}
	_ = os.Remove(probe)
	return Result{"state directory", OK, dir, ""}
}

// checkCheckout confirms paths.root is a git repository with the configured
// branches, since every run is cut from it.
func checkCheckout(ctx context.Context, o Options) Result {
	root := o.Cfg.Paths.Root
	if _, err := o.run()(ctx, "git", "-C", root, "rev-parse", "--git-dir"); err != nil {
		return Result{"checkout", Fail, root + " is not a git repository", "set paths.root to the checkout runs are cut from"}
	}
	ref := "origin/" + o.Cfg.Branches.Integration
	if _, err := o.run()(ctx, "git", "-C", root, "rev-parse", "--verify", ref); err != nil {
		return Result{
			"checkout", Warn, fmt.Sprintf("%s has no %s", root, ref),
			"run: git -C " + root + " fetch origin",
		}
	}
	return Result{"checkout", OK, fmt.Sprintf("%s (%s)", root, ref), ""}
}

// versionLine picks the line that looks like a version.
//
// Some providers print a banner before answering — podman announces which
// external implementation it is delegating to — so the first line of output is
// not reliably the answer.
func versionLine(s string) string {
	var fallback string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(stripANSI(line))
		if line == "" || strings.HasPrefix(line, ">>>>") {
			continue
		}
		if fallback == "" {
			fallback = line
		}
		if strings.Contains(strings.ToLower(line), "version") {
			return line
		}
	}
	return fallback
}

// stripANSI removes colour escapes, which banners tend to carry.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
