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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/compose"
	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/scaffold"
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
	// CfgErr is why the configuration did not load, when it did not. A parse
	// error reported as "no config file found" sends the reader looking for a
	// missing file that is right there.
	CfgErr error
	// CfgPath is the file the configuration was read from, named in the
	// report so it is clear which one was checked.
	CfgPath string
	// Run executes commands. Nil means ExecRunner.
	Run Runner
	// Look reports a binary's path. Nil means exec.LookPath.
	Look func(string) (string, error)
	// Version is claudeflow's own version, so it can check itself.
	Version string
	// Latest reports a project's newest release. Nil skips every lag check,
	// which is what an offline host wants.
	Latest Latest
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
	r = append(r, checkSelf(ctx, o))
	r = append(r, checkGit(ctx, o), checkGitIdentity(ctx, o), checkGH(ctx, o), checkGHAuth(ctx, o))
	r = append(r, checkConfig(o))
	if o.HaveConfig {
		r = append(r, checkRepoAccess(ctx, o))
	}
	r = append(r, checkAgent(ctx, o), checkAgentUpdates(ctx, o), checkCompose(ctx, o))
	if o.HaveConfig {
		r = append(r, checkComposeFile(ctx, o), checkStateDir(o), checkCheckout(ctx, o))
	}
	return r
}

// checkSelf reports claudeflow's own version against its latest release.
//
// A supervisor that is behind is the one failure nobody goes looking for: it
// runs, it looks healthy, and it carries whatever was wrong with the build it
// is on. This host ran a binary for a day after the fix for its own thrashing
// had shipped.
// checkConfig reports on the configuration itself: that there is one, which
// one, and that it parsed and validated.
//
// Loading it is the check. The parser rejects unknown keys and Validate
// refuses a config that would fail later — an empty user, a label used for two
// states, more builds than there are slots — so a configuration that loaded is
// one every other check can be run against.
func checkConfig(o Options) Result {
	if o.HaveConfig {
		path := o.CfgPath
		if path == "" {
			path = "(supplied)"
		}
		// A scaffolded configuration parses and validates while still saying
		// OWNER/NAME, because that is a well-formed owner/name. Saying so here
		// beats the reader meeting it three rows down as a repository that
		// cannot be read.
		if unfilled := scaffold.Unfilled(o.Cfg.Repo, o.Cfg.User); len(unfilled) > 0 {
			return Result{
				"configuration", Warn,
				fmt.Sprintf("%s — still to fill in: %s", path, strings.Join(unfilled, ", ")),
				"edit " + path,
			}
		}
		return Result{"configuration", OK, path + " — parsed and valid", ""}
	}
	if o.CfgErr == nil || errors.Is(o.CfgErr, fs.ErrNotExist) {
		// No configuration is not a fault on its own: `doctor` in a fresh
		// checkout is a reasonable thing to run, and everything above this
		// line still answers.
		return Result{
			Name: "configuration", Status: Warn,
			Detail: "none found",
			Fix:    "run: claudeflow init — or pass -c <path> to check an existing one",
		}
	}
	// A configuration that exists and will not load stops every run.
	head := firstLine(o.CfgErr.Error())
	return Result{
		Name: "configuration", Status: Fail,
		Detail: head,
		Fix:    strings.TrimSpace(strings.TrimPrefix(o.CfgErr.Error(), head)),
	}
}

func checkSelf(ctx context.Context, o Options) Result {
	v := o.Version
	if v == "" || v == "dev" {
		return Result{
			"claudeflow", Warn, "development build",
			"a release binary reports its version and can be checked against the latest",
		}
	}
	return judge(ctx, o, Result{"claudeflow", OK, v, ""}, v, upstreamSelf)
}

// checkAgentUpdates asks the agent CLI whether it keeps itself current.
//
// claudeflow deliberately does not update the agent: the CLI has its own
// updater, and a supervisor that reinstalls a tool mid-flight is a worse idea
// than one that says the updater is off. But an agent CLI that never moves is a
// slow problem — it pins the model alias, the skills and the bug fixes to
// whatever was installed once.
//
// The question is put to `claude doctor`, which answers it directly. Nothing
// here reads the CLI's own state files: those are internal, and a doctor that
// depends on another tool's internals is wrong the moment that tool changes.
func checkAgentUpdates(ctx context.Context, o Options) Result {
	cmd := o.Cfg.Agent.Command
	if cmd == "" {
		cmd = "claude"
	}
	if _, err := o.look()(cmd); err != nil {
		// checkAgent has already failed this; do not say it twice.
		return Result{"agent updates", Warn, "cannot ask " + cmd, "install the agent CLI"}
	}
	out, err := o.run()(ctx, cmd, "doctor")
	if err != nil {
		return Result{
			"agent updates", Warn, "could not ask " + cmd + " about updates",
			"run: " + cmd + " doctor",
		}
	}
	state, ok := field(out, "Auto-updates:")
	if !ok {
		return Result{
			"agent updates", Warn, "could not read the update setting",
			"run: " + cmd + " doctor",
		}
	}
	detail := state
	if channel, ok := field(out, "Auto-update channel:"); ok {
		detail += " (" + channel + " channel)"
	}
	if strings.HasPrefix(strings.ToLower(state), "enabled") {
		return Result{"agent updates", OK, detail, ""}
	}
	return Result{
		"agent updates", Warn, detail,
		"the agent CLI will not update itself; unset DISABLE_AUTOUPDATER, or upgrade it on a schedule of your own",
	}
}

// field reads a "Label: value" line out of a tool's report.
func field(out, label string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(stripANSI(line))
		if v, ok := strings.CutPrefix(line, label); ok {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
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
	// No lag check: git.git publishes tags, not GitHub releases, and every
	// host installs git from its distribution anyway.
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
	line := firstLine(out)
	return judge(ctx, o, Result{"gh", OK, line, ""}, line, upstreamGH)
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
	line := versionLine(out)
	r := Result{"compose", OK, fmt.Sprintf("%s — %s", strings.Join(bin, " "), line), ""}
	return note(ctx, o, r, line, composeUpstream(out))
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
