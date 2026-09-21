package compose

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArgsOrder(t *testing.T) {
	c := Compose{Bin: []string{"docker", "compose"}, Project: "cf-3", File: "compose.yaml"}
	got, err := c.args("up", "-d")
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	want := []string{"docker", "compose", "-p", "cf-3", "-f", "compose.yaml", "up", "-d"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", got, want)
	}
}

func TestArgsProfiles(t *testing.T) {
	c := Compose{Bin: []string{"podman", "compose"}, Project: "p", File: "f", Profiles: []string{"spa", "docs"}}
	got, _ := c.args("ps")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--profile spa") || !strings.Contains(joined, "--profile docs") {
		t.Errorf("args = %q, want both profiles", joined)
	}
	// Profiles are global flags and must precede the subcommand.
	if strings.Index(joined, "--profile") > strings.Index(joined, " ps") {
		t.Errorf("args = %q, want profiles before the subcommand", joined)
	}
}

func TestArgsOmitsEmptyProjectAndFile(t *testing.T) {
	c := Compose{Bin: []string{"docker", "compose"}}
	got, _ := c.args("down")
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "-p ") || strings.Contains(joined, "-f ") {
		t.Errorf("args = %q, want no empty -p or -f", joined)
	}
}

// docker emits one JSON object per line; podman emits a single array. Both
// have to decode, because pinning one would make the tool silently
// implementation-specific.
func TestParseStatusDockerLines(t *testing.T) {
	out := `{"Service":"postgres","State":"running","Health":"healthy"}
{"Service":"api","State":"running","Health":"starting"}`
	got, err := parseStatus(out)
	if err != nil {
		t.Fatalf("parseStatus: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d, want 2", len(got))
	}
	if !got[0].Ready() {
		t.Error("healthy postgres reported not ready")
	}
	if got[1].Ready() {
		t.Error("starting api reported ready")
	}
}

func TestParseStatusPodmanArray(t *testing.T) {
	out := `[{"Service":"postgres","State":"running","Health":"healthy"}]`
	got, err := parseStatus(out)
	if err != nil {
		t.Fatalf("parseStatus: %v", err)
	}
	if len(got) != 1 || !got[0].Ready() {
		t.Errorf("got %+v, want one ready service", got)
	}
}

// podman leaves Health empty and puts the parenthetical in Status, so that
// string is the only readiness signal there.
func TestParseStatusPodmanStatusString(t *testing.T) {
	out := `[{"Name":"cf-1_api_1","Status":"Up 3 seconds (healthy)"}]`
	got, err := parseStatus(out)
	if err != nil {
		t.Fatalf("parseStatus: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("parsed %d, want 1", len(got))
	}
	if got[0].Health != "healthy" {
		t.Errorf("Health = %q, want healthy from the Status string", got[0].Health)
	}
	if !got[0].Ready() {
		t.Error("reported not ready")
	}
	if got[0].Service != "cf-1_api_1" {
		t.Errorf("Service = %q, want the name used as a fallback", got[0].Service)
	}
}

func TestParseStatusUnhealthyFromStatusString(t *testing.T) {
	got, err := parseStatus(`[{"Name":"x","Status":"Up 30 seconds (unhealthy)"}]`)
	if err != nil {
		t.Fatalf("parseStatus: %v", err)
	}
	if !got[0].Failed() {
		t.Error("an unhealthy container did not report Failed")
	}
}

func TestParseStatusEmpty(t *testing.T) {
	got, err := parseStatus("  \n ")
	if err != nil {
		t.Fatalf("parseStatus: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

func TestParseStatusGarbage(t *testing.T) {
	if _, err := parseStatus("{not json"); err == nil {
		t.Fatal("parseStatus on garbage succeeded, want error")
	}
}

// A service with no healthcheck must count as ready once running, or the wait
// hangs on every service the project chose not to probe.
func TestHealthReadyWithoutAHealthcheck(t *testing.T) {
	if !(Health{State: "running"}).Ready() {
		t.Error("a running container with no healthcheck reported not ready")
	}
	if (Health{State: "exited"}).Ready() {
		t.Error("an exited container reported ready")
	}
	if !(Health{State: "exited"}).Failed() {
		t.Error("an exited container did not report Failed")
	}
}

func TestHealthHealthBeatsState(t *testing.T) {
	// A container can be running and still not ready.
	h := Health{State: "running", Health: "starting"}
	if h.Ready() {
		t.Error("running+starting reported ready; the healthcheck must win")
	}
	if h.Failed() {
		t.Error("running+starting reported failed; it is still coming up")
	}
}

func TestErrUnhealthyMessage(t *testing.T) {
	err := &ErrUnhealthy{Services: []string{"api", "web"}}
	var target *ErrUnhealthy
	if !errors.As(error(err), &target) {
		t.Fatal("ErrUnhealthy is not matchable with errors.As")
	}
	if !strings.Contains(err.Error(), "api") || !strings.Contains(err.Error(), "web") {
		t.Errorf("Error() = %q, want it to name both services", err)
	}
}

func TestSummarise(t *testing.T) {
	got := summarise([]Health{{Service: "api", State: "running", Health: "starting"}})
	if !strings.Contains(got, "api=running/starting") {
		t.Errorf("summarise = %q", got)
	}
	if summarise(nil) != "no containers" {
		t.Errorf("summarise(nil) = %q", summarise(nil))
	}
}

func TestDetect(t *testing.T) {
	bin, err := Detect()
	if err != nil {
		if !errors.Is(err, ErrNoCompose) {
			t.Errorf("err = %v, want ErrNoCompose", err)
		}
		t.Skip("no compose implementation on this host")
	}
	if len(bin) == 0 {
		t.Fatal("Detect returned an empty command")
	}
	t.Logf("detected %v", bin)
}

// ── Integration ──────────────────────────────────────────────────

func composeOrSkip(t *testing.T) []string {
	t.Helper()
	bin, err := Detect()
	if err != nil {
		t.Skip("no compose implementation")
	}
	if _, err := exec.LookPath(bin[0]); err != nil {
		t.Skip("compose binary not runnable")
	}
	return bin
}

const probeStack = `
services:
  probe:
    image: docker.io/library/busybox:latest
    command: ["sh", "-c", "sleep 120"]
`

// Up, WaitReady and Down against a real engine. This is the path that every
// unit test above can only approximate, because the JSON shapes come from the
// engine rather than from this package.
func TestLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	bin := composeOrSkip(t)

	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yaml")
	if err := writeFile(file, probeStack); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := Compose{Bin: bin, File: file, Project: "cftest-lifecycle", Dir: dir}

	t.Cleanup(func() {
		ctx, cancel := contextWithTimeout(2 * time.Minute)
		defer cancel()
		_ = c.Down(ctx)
	})

	ctx, cancel := contextWithTimeout(5 * time.Minute)
	defer cancel()

	if err := c.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := c.WaitReady(ctx, 90*time.Second, time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	running, err := c.Running(ctx)
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if !running {
		t.Error("Running = false after a successful Up")
	}

	if err := c.Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}
	running, err = c.Running(ctx)
	if err != nil {
		t.Fatalf("Running after Down: %v", err)
	}
	if running {
		t.Error("Running = true after Down; the project leaked containers")
	}
}

// Running must answer false for a project that has never existed, rather than
// erroring — slot allocation asks this about every candidate slot.
func TestRunningOnAnUnknownProject(t *testing.T) {
	bin := composeOrSkip(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yaml")
	if err := writeFile(file, probeStack); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := Compose{Bin: bin, File: file, Project: "cftest-never-existed", Dir: dir}

	ctx, cancel := contextWithTimeout(time.Minute)
	defer cancel()
	running, err := c.Running(ctx)
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if running {
		t.Error("Running = true for a project that was never created")
	}
}

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o644) }

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
