package sandbox

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeEngine struct {
	mu      sync.Mutex
	ps      string
	inspect map[string]string
	ran     []string
	block   chan struct{}
}

func (f *fakeEngine) podman(_ context.Context, args ...string) (string, error) {
	switch args[0] {
	case "ps":
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.ps, nil
	case "container":
		return f.inspect[args[len(args)-1]], nil
	case "healthcheck":
		if f.block != nil {
			<-f.block
		}
		f.mu.Lock()
		f.ran = append(f.ran, args[2])
		f.mu.Unlock()
	}
	return "", nil
}

func (f *fakeEngine) runs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ran...)
}

func waitRuns(t *testing.T, f *fakeEngine, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(f.runs()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("runs = %v, want %d", f.runs(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return f.runs()
}

// A container's check runs at once, then at its own interval; one without a
// healthcheck is never checked.
func TestHealthchecksRunEachAtItsInterval(t *testing.T) {
	f := &fakeEngine{
		ps: "db\nweb\nplain\n",
		inspect: map[string]string{
			"db":    "CMD-SHELL pg_isready|2s\n",
			"web":   "CMD-SHELL curl -f localhost|10s\n",
			"plain": "\n",
		},
	}
	h := &Healthchecks{Podman: f.podman}
	t0 := time.Unix(1000, 0)
	ctx := context.Background()

	h.Tick(ctx, t0)
	got := waitRuns(t, f, 2)
	if strings.Join(slices.Sorted(slices.Values(got)), ",") != "db,web" {
		t.Fatalf("first tick ran %v, want db and web at once", got)
	}
	h.Tick(ctx, t0.Add(time.Second))
	h.Tick(ctx, t0.Add(2*time.Second))
	waitRuns(t, f, 3)
	time.Sleep(20 * time.Millisecond)
	if got := f.runs(); len(got) != 3 || got[2] != "db" {
		t.Fatalf("after 2s ran %v, want db again and not web", got)
	}
	h.Tick(ctx, t0.Add(10*time.Second))
	waitRuns(t, f, 5)
	for _, id := range f.runs() {
		if id == "plain" {
			t.Fatalf("checked a container with no healthcheck")
		}
	}
}

// A slow check is not piled on by the next tick.
func TestHealthchecksDoNotOverlap(t *testing.T) {
	f := &fakeEngine{ps: "db", inspect: map[string]string{"db": "CMD true|1s"}, block: make(chan struct{})}
	h := &Healthchecks{Podman: f.podman}
	t0 := time.Unix(1000, 0)
	h.Tick(context.Background(), t0)
	h.Tick(context.Background(), t0.Add(5*time.Second))
	close(f.block)
	waitRuns(t, f, 1)
	time.Sleep(20 * time.Millisecond)
	if got := f.runs(); len(got) != 1 {
		t.Fatalf("runs = %v, want one while the first was still going", got)
	}
}

func TestHealthcheckInterval(t *testing.T) {
	for out, want := range map[string]time.Duration{
		"CMD-SHELL true|3s":  3 * time.Second,
		"CMD-SHELL true|0s":  defaultHealthInterval,
		"NONE|0s":            0,
		"":                   0,
		"CMD-SHELL true|bad": defaultHealthInterval,
	} {
		f := &fakeEngine{inspect: map[string]string{"c": out}}
		h := &Healthchecks{Podman: f.podman}
		if got := h.inspect(context.Background(), "c"); got != want {
			t.Errorf("inspect(%q) = %v, want %v", out, got, want)
		}
	}
}
