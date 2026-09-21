// Package compose runs a project's environment as a Compose stack.
//
// Every project claudeflow drives must ship a Compose file. That is the whole
// environment contract: claudeflow brings the stack up before a run and takes
// it down afterwards, and each run gets its own Compose project, so isolation
// is namespacing rather than convention. There is nothing shell-shaped left to
// configure, and no way for one run's teardown to reach another's resources.
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Compose runs one project's stack.
type Compose struct {
	// Bin is the compose command, already split — {"docker","compose"} or
	// {"podman","compose"}. Empty means Detect is consulted.
	Bin []string
	// File is the Compose file, relative to Dir or absolute.
	File string
	// Project namespaces every container, network and volume the stack owns.
	Project string
	// Dir is the working directory commands run from.
	Dir string
	// Profiles are Compose profiles to enable.
	Profiles []string
	// Env is set on the compose process, so the file can interpolate it.
	Env map[string]string
	// Timeout bounds every compose command. Zero means DefaultTimeout.
	//
	// This is not belt-and-braces. podman's compose has been observed to wedge
	// in epoll waiting on a health condition it never resolves, and an
	// unbounded command there blocks the whole supervisor: no reaping, no
	// dispatch, no status, until someone notices and kills it by hand. A stuck
	// provider must fail one run, not stop the system.
	Timeout time.Duration
}

// DefaultTimeout bounds a compose command that sets none.
const DefaultTimeout = 15 * time.Minute

// ErrNoCompose means no compose implementation was found.
var ErrNoCompose = fmt.Errorf("no compose implementation found: install docker compose or podman compose")

// Detect returns the compose command to use.
//
// docker is preferred where both exist: podman's compose is a separate Python
// implementation with a different flag set — notably no `up --wait` — so code
// written against it works on docker, and not always the other way round.
func Detect() ([]string, error) {
	for _, candidate := range [][]string{{"docker", "compose"}, {"podman", "compose"}} {
		if _, err := exec.LookPath(candidate[0]); err != nil {
			continue
		}
		cmd := exec.Command(candidate[0], append(candidate[1:], "version")...)
		if err := cmd.Run(); err == nil {
			return candidate, nil
		}
	}
	return nil, ErrNoCompose
}

func (c Compose) bin() ([]string, error) {
	if len(c.Bin) > 0 {
		return c.Bin, nil
	}
	return Detect()
}

// args builds a full command line, with the global flags compose wants before
// the subcommand.
func (c Compose) args(sub ...string) ([]string, error) {
	bin, err := c.bin()
	if err != nil {
		return nil, err
	}
	out := append([]string{}, bin...)
	if c.Project != "" {
		out = append(out, "-p", c.Project)
	}
	if c.File != "" {
		out = append(out, "-f", c.File)
	}
	for _, p := range c.Profiles {
		out = append(out, "--profile", p)
	}
	return append(out, sub...), nil
}

func (c Compose) run(ctx context.Context, sub ...string) (string, error) {
	argv, err := c.args(sub...)
	if err != nil {
		return "", err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Kill the whole group: podman compose is a wrapper that spawns the real
	// implementation, and signalling only the wrapper leaves that behind.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return cmd.Process.Kill()
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = c.Dir
	cmd.Env = composeEnv(c.Env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return stdout.String(), fmt.Errorf("%s: timed out after %s: %w", strings.Join(argv, " "), timeout, ctx.Err())
		}
		return stdout.String(), fmt.Errorf("%s: %s", strings.Join(argv, " "), strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// systemdMarkers are the variables systemd sets for a unit it manages.
//
// They are stripped before running compose. podman reads INVOCATION_ID as
// "systemd is managing me", and under it `up -d` created the first service and
// then sat in epoll indefinitely — no error, no progress, and the supervisor
// blocked behind it. JOURNAL_STREAM additionally makes every container log to
// the supervisor's journal, drowning its own output.
//
// Stripping them is not a workaround for podman: the containers belong to the
// run, not to the supervisor's unit, so claiming otherwise was the error.
var systemdMarkers = []string{
	"INVOCATION_ID",
	"JOURNAL_STREAM",
	"NOTIFY_SOCKET",
	"LISTEN_FDS",
	"LISTEN_PID",
	"LISTEN_FDNAMES",
	"MAINPID",
	"MANAGERPID",
}

// composeEnv returns the environment for a compose command: the process
// environment without systemd's markers, plus the caller's additions.
func composeEnv(extra map[string]string) []string {
	drop := make(map[string]struct{}, len(systemdMarkers))
	for _, k := range systemdMarkers {
		drop[k] = struct{}{}
	}
	var out []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, skip := drop[name]; skip {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// Up starts the stack detached.
//
// `--wait` is deliberately not passed: podman's compose does not accept it and
// fails the whole command. Readiness is WaitReady's job instead, which is the
// one place hand-rolled polling is correct — there is no portable waiter.
func (c Compose) Up(ctx context.Context) error {
	_, err := c.run(ctx, "up", "-d")
	return err
}

// Down stops the stack and removes its volumes.
//
// The volumes go because a run's database is disposable by design: the next
// run on this project must not inherit rows the last one wrote.
//
// This is safe in a way a shell teardown is not. A script can name any
// resource, so a teardown run from the wrong place can destroy another
// checkout's data; a Compose project can only reach what it owns.
func (c Compose) Down(ctx context.Context) error {
	_, err := c.run(ctx, "down", "-v", "--remove-orphans")
	return err
}

// Running reports whether the project has any container.
func (c Compose) Running(ctx context.Context) (bool, error) {
	out, err := c.run(ctx, "ps", "-q")
	if err != nil {
		// A project that has never existed is not an error, and compose
		// implementations disagree about whether to say so on stderr.
		return false, nil
	}
	return strings.TrimSpace(out) != "", nil
}

// Port returns the host address a service's container port is published on,
// so a caller can hand a browsable URL to the agent without the Compose file
// and the config having to agree on a number.
func (c Compose) Port(ctx context.Context, service string, port int) (string, error) {
	out, err := c.run(ctx, "port", service, strconv.Itoa(port))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Health is a container's readiness as compose reports it.
type Health struct {
	Service string
	State   string
	Health  string
}

// Ready reports whether a container counts as up.
//
// A container with no healthcheck reports an empty Health, and "running" is
// then the best answer available. Treating that as not-ready would hang on
// every service the project chose not to probe.
func (h Health) Ready() bool {
	if h.Health != "" {
		return strings.EqualFold(h.Health, "healthy")
	}
	return strings.EqualFold(h.State, "running")
}

// Failed reports whether a container has given up.
func (h Health) Failed() bool {
	return strings.EqualFold(h.Health, "unhealthy") ||
		strings.EqualFold(h.State, "exited") ||
		strings.EqualFold(h.State, "dead")
}

// Status returns each service's current state.
func (c Compose) Status(ctx context.Context) ([]Health, error) {
	out, err := c.run(ctx, "ps", "--format", "json")
	if err != nil {
		return nil, err
	}
	return parseStatus(out)
}

// parseStatus reads compose's ps output.
//
// The two implementations disagree on shape: docker emits one JSON object per
// line, podman emits a single array. Accepting both is cheaper than pinning an
// implementation.
func parseStatus(out string) ([]Health, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil, nil
	}
	type row struct {
		Service string `json:"Service"`
		Name    string `json:"Name"`
		State   string `json:"State"`
		Health  string `json:"Health"`
		Status  string `json:"Status"`
	}
	toHealth := func(r row) Health {
		h := Health{Service: r.Service, State: r.State, Health: r.Health}
		if h.Service == "" {
			h.Service = r.Name
		}
		// podman reports "Up 3 seconds (healthy)" in Status and leaves Health
		// empty, so the parenthetical is the only readiness signal there.
		if h.Health == "" && strings.Contains(r.Status, "(healthy)") {
			h.Health = "healthy"
		}
		if h.Health == "" && strings.Contains(r.Status, "(unhealthy)") {
			h.Health = "unhealthy"
		}
		if h.State == "" && strings.HasPrefix(r.Status, "Up") {
			h.State = "running"
		}
		return h
	}

	if strings.HasPrefix(trimmed, "[") {
		var rows []row
		if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
			return nil, fmt.Errorf("decode compose ps: %w", err)
		}
		out := make([]Health, 0, len(rows))
		for _, r := range rows {
			out = append(out, toHealth(r))
		}
		return out, nil
	}

	var healths []Health
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("decode compose ps line: %w", err)
		}
		healths = append(healths, toHealth(r))
	}
	return healths, nil
}

// ErrUnhealthy means a service failed rather than took its time.
type ErrUnhealthy struct{ Services []string }

func (e *ErrUnhealthy) Error() string {
	return "services did not come up: " + strings.Join(e.Services, ", ")
}

// WaitReady blocks until every service is ready, one has failed, or timeout.
//
// This is hand-rolled polling, which is normally the wrong answer. It is right
// here because there is no portable waiter: `up --wait` exists in docker's
// compose and not podman's, and using it would make the tool silently
// implementation-specific.
func (c Compose) WaitReady(ctx context.Context, timeout, interval time.Duration) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var last []Health
	for {
		status, err := c.Status(ctx)
		if err == nil {
			last = status
			var failed []string
			ready := len(status) > 0
			for _, h := range status {
				if h.Failed() {
					failed = append(failed, h.Service)
				}
				if !h.Ready() {
					ready = false
				}
			}
			if len(failed) > 0 {
				return &ErrUnhealthy{Services: failed}
			}
			if ready {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for the stack: %s", timeout, summarise(last))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func summarise(hs []Health) string {
	if len(hs) == 0 {
		return "no containers"
	}
	parts := make([]string, 0, len(hs))
	for _, h := range hs {
		state := h.State
		if h.Health != "" {
			state += "/" + h.Health
		}
		parts = append(parts, h.Service+"="+state)
	}
	return strings.Join(parts, " ")
}
