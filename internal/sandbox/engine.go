package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The nested podman a sandbox runs is missing two things the host's has,
// because the host's come from systemd and there is no systemd in a sandbox:
// the API socket Docker Compose talks to, and the timers that run
// healthchecks. Without the socket `podman compose` cannot use Docker Compose,
// which is the compose provider projects pick when podman-compose will not do.
// Without the timers a healthcheck never runs, so a service that has one
// stays "starting" for ever: the stack never reads as ready, and anything that
// waits on `condition: service_healthy` waits indefinitely.
//
// So the sandbox provides both, for its whole life — the agent brings up
// containers of its own, and they need the same.

// apiSocket is where the nested podman serves its API.
const apiSocket = "/tmp/claudeflow-podman/podman.sock"

// serveAPI starts the nested podman's API service and points DOCKER_HOST at
// it, for compose here and for everything the agent runs. A DOCKER_HOST the
// container was already given is left alone.
func serveAPI(ctx context.Context, note func(string, ...any)) {
	if os.Getenv("DOCKER_HOST") != "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(apiSocket), 0o700); err != nil {
		note("could not start the podman API: %v", err)
		return
	}
	cmd := exec.CommandContext(ctx, "podman", "system", "service", "--time=0", "unix://"+apiSocket)
	if err := cmd.Start(); err != nil {
		note("could not start the podman API: %v", err)
		return
	}
	go func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if info, err := os.Stat(apiSocket); err == nil && info.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			note("the podman API did not come up at %s", apiSocket)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	os.Setenv("DOCKER_HOST", "unix://"+apiSocket)
}

// defaultHealthInterval is Docker's and podman's interval for a healthcheck
// that names none.
const defaultHealthInterval = 30 * time.Second

// Healthchecks runs the healthchecks of the nested engine's containers, each
// at its own interval, until ctx ends — what systemd's timers do on a host.
type Healthchecks struct {
	// Podman runs podman with args and returns its standard output.
	Podman func(ctx context.Context, args ...string) (string, error)
	// Every is how often running containers are looked for.
	Every time.Duration

	mu       sync.Mutex
	interval map[string]time.Duration // zero: the container has no healthcheck
	next     map[string]time.Time
	running  map[string]bool
}

// Run checks until ctx ends.
func (h *Healthchecks) Run(ctx context.Context) {
	t := time.NewTicker(h.Every)
	defer t.Stop()
	for {
		h.Tick(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick starts the healthcheck of every running container that is due one. A
// check that is still going is not started again.
func (h *Healthchecks) Tick(ctx context.Context, now time.Time) {
	out, err := h.Podman(ctx, "ps", "--quiet", "--no-trunc")
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.interval == nil {
		h.interval, h.next, h.running = map[string]time.Duration{}, map[string]time.Time{}, map[string]bool{}
	}
	live := map[string]bool{}
	for _, id := range strings.Fields(out) {
		live[id] = true
		every, known := h.interval[id]
		if !known {
			every = h.inspect(ctx, id)
			h.interval[id] = every
			// The first check runs at once, as it would on the host: a
			// service is not ready until one has passed.
			h.next[id] = now
		}
		if every == 0 || h.running[id] || now.Before(h.next[id]) {
			continue
		}
		h.running[id] = true
		h.next[id] = now.Add(every)
		go func(id string) {
			_, _ = h.Podman(ctx, "healthcheck", "run", id)
			h.mu.Lock()
			delete(h.running, id)
			h.mu.Unlock()
		}(id)
	}
	for id := range h.interval {
		if !live[id] {
			delete(h.interval, id)
			delete(h.next, id)
		}
	}
}

// inspect reads a container's healthcheck interval; zero when it has none.
func (h *Healthchecks) inspect(ctx context.Context, id string) time.Duration {
	out, err := h.Podman(ctx, "container", "inspect", "--format",
		`{{if .Config.Healthcheck}}{{join .Config.Healthcheck.Test " "}}|{{.Config.Healthcheck.Interval}}{{end}}`, id)
	if err != nil {
		return 0
	}
	test, interval, ok := strings.Cut(strings.TrimSpace(out), "|")
	if !ok || test == "" || strings.HasPrefix(test, "NONE") {
		return 0
	}
	if d, err := time.ParseDuration(interval); err == nil && d > 0 {
		return d
	}
	return defaultHealthInterval
}

func podman(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", args...).Output()
	if err != nil {
		return "", fmt.Errorf("podman %s: %w", args[0], err)
	}
	return string(out), nil
}
