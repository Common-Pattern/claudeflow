package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/compose"
	"github.com/Common-Pattern/claudeflow/internal/runner"
)

// stackProject is the Compose project every sandbox uses for its stack. One
// name is enough: the stack runs on podman nested inside the run's own
// container, so there is nothing else in that engine for it to collide with.
const stackProject = "cf"

// credentialHelper answers git's credential requests with GH_TOKEN, so the
// clone and the agent's pushes authenticate as the operator without the token
// being written to a file or a remote URL.
const credentialHelper = `!f() { test "$1" = get || exit 0; echo username=x-access-token; echo "password=${GH_TOKEN:-$GITHUB_TOKEN}"; }; f`

// Inside prepares the run a spec describes and runs its agent, returning the
// agent's exit code. It is what the sandbox container runs.
//
// Progress lines go to the run's log, ahead of the agent's own output, so a
// run that never reaches its agent still explains itself in its transcript.
func Inside(ctx context.Context, specPath string) (int, error) {
	spec, err := ReadSpec(specPath)
	if err != nil {
		return 1, err
	}
	// podman stop sends SIGTERM here. Cancelling the context is how runner
	// stops the agent's whole process group rather than leaving it orphaned.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	note := func(format string, args ...any) {
		appendLog(spec.Log, "claudeflow sandbox: "+fmt.Sprintf(format, args...))
	}

	if spec.Stack {
		cleanNestedEngine(ctx, note)
	}
	note("cloning %s into %s", spec.Remote, spec.Workdir)
	if err := checkout(ctx, spec, note); err != nil {
		note("checkout failed: %v", err)
		return 1, err
	}

	env := runner.Env{}
	for k, v := range spec.Env {
		env[k] = v
	}
	env["CLAUDEFLOW_WORKTREE"] = spec.Workdir

	var stack compose.Compose
	if spec.Stack {
		if spec.Install != "" {
			note("running the install hook")
			r := runner.Runner{Dir: spec.Workdir, Env: env, Timeout: spec.Timeout, LogPath: spec.Log}
			res, err := r.Exec(ctx, spec.Install)
			if err != nil {
				return 1, fmt.Errorf("install hook: %w", err)
			}
			if res.ExitCode != 0 {
				note("install hook exited %d", res.ExitCode)
				return 1, fmt.Errorf("install hook: exit %d", res.ExitCode)
			}
		}
		stack = compose.Compose{
			File:     spec.ComposeFile,
			Profiles: spec.ComposeProfiles,
			Project:  stackProject,
			Dir:      spec.Workdir,
			Env:      spec.ComposeEnv,
			Timeout:  spec.ReadyTimeout + time.Minute,
		}
		note("bringing the stack up")
		if err := stack.Up(ctx); err != nil {
			note("compose up failed: %v", err)
			return 1, fmt.Errorf("compose up: %w", err)
		}
		if err := stack.WaitReady(ctx, spec.ReadyTimeout, 2*time.Second); err != nil {
			note("stack did not become ready: %v", err)
			return 1, fmt.Errorf("stack did not become ready: %w", err)
		}
		resolved := serviceAddresses(ctx, stack, spec.Expose, note)
		for k, v := range resolved {
			env[k] = v
		}
		for k, v := range spec.AgentEnv {
			env[k] = os.Expand(v, func(name string) string { return resolved[name] })
		}
	}

	note("starting the agent")
	proc, err := runner.Start(ctx, runner.Spawn{
		Command: spec.Command,
		Args:    spec.Args,
		Dir:     spec.Workdir,
		Env:     env,
		Timeout: spec.Timeout,
		LogPath: spec.Log,
	})
	if err != nil {
		note("could not start the agent: %v", err)
		return 1, err
	}
	res := proc.Wait()

	if spec.Stack {
		// Best effort, and bounded: the container is removed after this
		// anyway, and its nested engine with it.
		downCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := stack.Down(downCtx); err != nil {
			note("compose down: %v", err)
		}
	}
	return res.ExitCode, nil
}

// checkout clones the remote and leaves the run's branch checked out: the
// branch as pushed when it exists, or a new one cut from StartPoint.
func checkout(ctx context.Context, spec Spec, note func(string, ...any)) error {
	args := []string{"-c", "credential.helper=" + credentialHelper, "clone", "--no-checkout", "--quiet"}
	if spec.Mirror != "" {
		args = append(args, "--reference-if-able", spec.Mirror)
	}
	args = append(args, spec.Remote, spec.Workdir)
	if err := git(ctx, "", args...); err != nil {
		return err
	}
	for _, kv := range [][2]string{
		{"credential.helper", credentialHelper},
		{"user.name", spec.AuthorName},
		{"user.email", spec.AuthorEmail},
	} {
		if kv[1] == "" {
			continue
		}
		if err := git(ctx, spec.Workdir, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	start := "origin/" + spec.StartPoint
	if spec.Branch == "" {
		return git(ctx, spec.Workdir, "checkout", "--quiet", "--detach", start)
	}
	if git(ctx, spec.Workdir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+spec.Branch) == nil {
		note("continuing %s as pushed", spec.Branch)
		return git(ctx, spec.Workdir, "checkout", "--quiet", "-B", spec.Branch, "origin/"+spec.Branch)
	}
	note("cutting %s from %s", spec.Branch, start)
	return git(ctx, spec.Workdir, "checkout", "--quiet", "--no-track", "-B", spec.Branch, start)
}

// cleanNestedEngine removes whatever a previous run on this slot left in the
// nested engine's storage. The storage outlives a run so that images are not
// pulled afresh every time, but containers, volumes and networks are the
// previous run's state, and starting from them is exactly what a sandbox is
// for not doing.
func cleanNestedEngine(ctx context.Context, note func(string, ...any)) {
	for _, args := range [][]string{
		{"rm", "--all", "--force", "--time", "0"},
		{"pod", "rm", "--all", "--force"},
		{"volume", "prune", "--force"},
		{"network", "prune", "--force"},
	} {
		if out, err := exec.CommandContext(ctx, "podman", args...).CombinedOutput(); err != nil {
			note("cleaning the nested engine (podman %s): %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
}

// serviceAddresses asks the nested Compose where each exposed service landed.
// The agent is in the same network namespace as the published ports, so the
// host to dial is always localhost.
func serviceAddresses(ctx context.Context, stack compose.Compose, expose map[string]int, note func(string, ...any)) map[string]string {
	out := map[string]string{}
	for service, port := range expose {
		addr, err := stack.Port(ctx, service, port)
		if err != nil || addr == "" {
			note("could not resolve the published port for %s: %v", service, err)
			continue
		}
		hostPort := addr
		if i := strings.LastIndex(addr, ":"); i >= 0 {
			hostPort = addr[i+1:]
		}
		name := strings.ToUpper(strings.ReplaceAll(service, "-", "_"))
		out["CLAUDEFLOW_PORT_"+name] = hostPort
		out["CLAUDEFLOW_HOST_"+name] = "localhost"
		out["CLAUDEFLOW_URL_"+name] = "http://localhost:" + hostPort
	}
	return out
}

func git(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(redact(args), " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// redact keeps the credential helper's body out of error messages; it names
// a variable, not a value, but a long shell function makes the error unreadable.
func redact(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "credential.helper=") {
			a = "credential.helper=…"
		}
		out[i] = a
	}
	return out
}

func appendLog(path, line string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), line)
}
