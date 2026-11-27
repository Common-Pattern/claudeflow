package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MirrorPath is where the host's git directory is mounted, read-only, for a
// clone to borrow objects from.
const MirrorPath = "/claudeflow/mirror.git"

// storagePath is where rootless podman keeps images and containers for the
// image's podman user.
const storagePath = "/home/podman/.local/share/containers"

// Podman starts and inspects sandbox containers on the host's rootless podman.
type Podman struct {
	// Bin is the podman binary. Empty means "podman".
	Bin string
	// Image is what every sandbox starts from.
	Image string
	// Prefix names everything this project creates: containers are
	// "<prefix>-<run>" and per-slot storage volumes "<prefix>-slot<n>".
	Prefix string
	// Repo labels every container, so a slot is only busy for its own project.
	Repo string
	// Pass names environment variables handed to every container by name.
	Pass []string
	// Mounts are extra "host:container[:ro]" bind mounts.
	Mounts []string
	// Self is the claudeflow binary on the host. It is mounted at the same
	// path inside, and is the container's command.
	Self string
	// Mirror is a git directory on the host to lend objects from. Optional.
	Mirror string
}

// Start describes one sandbox to start.
type Start struct {
	RunID string
	// Slot is the run's environment slot; zero for a run that holds none.
	// A slot's nested image storage is kept between runs on that slot, so
	// the stack's images are not pulled every time.
	Slot int
	// Spec is the spec file, on the host.
	Spec string
	// Log is the run's transcript and Inbox its comment inbox, both on the
	// host and both mounted at the same paths inside.
	Log   string
	Inbox string
	// Secrets are set on the podman process and passed into the container by
	// name, so their values appear in no command line.
	Secrets map[string]string
	// ClaudeDir, when set, is a host directory mounted read-write at
	// ClaudePath as the agent CLI's config directory: the host's own, shared.
	ClaudeDir  string
	ClaudePath string
	// Binaries are host executables mounted read-only at their host paths:
	// the agent CLI, so a run uses the build the host does.
	Binaries []string
	// Data is the run's generation of prepared data, mounted read-only at
	// its host path. Optional.
	Data string
}

func (p Podman) bin() string {
	if p.Bin == "" {
		return "podman"
	}
	return p.Bin
}

// Name is the container a run's sandbox is.
func (p Podman) Name(runID string) string { return p.Prefix + "-" + runID }

// Labels.
const (
	labelRepo = "claudeflow.repo"
	labelSlot = "claudeflow.slot"
	labelRun  = "claudeflow.run"
)

// RunArgs is the podman command line that starts s, without the binary.
//
// The security options are what rootless podman inside a rootless container
// needs, and they are relaxations of the container's own confinement, not of
// the host's: keep-id maps the image's podman user to the operator, so files
// the run writes through its mounts are the operator's, and everything else
// the run touches is inside a user namespace the operator owns.
func (p Podman) RunArgs(s Start) []string {
	name := p.Name(s.RunID)
	args := []string{
		"run", "--detach", "--name", name,
		"--label", labelRepo + "=" + p.Repo,
		"--label", labelSlot + "=" + strconv.Itoa(s.Slot),
		"--label", labelRun + "=" + s.RunID,
		"--init",
		// No cap on processes beyond the operator's own, as a host run has:
		// podman's default of 2048 is spent by a build tool that runs every
		// package's tasks at once, and the run's nested stack shares it.
		"--pids-limit=-1",
		"--userns=keep-id",
		"--device", "/dev/fuse",
		"--device", "/dev/net/tun",
		"--security-opt", "label=disable",
		"--security-opt", "unmask=ALL",
		"--security-opt", "seccomp=unconfined",
		"--volume", p.Self + ":" + p.Self + ":ro",
		"--volume", s.Spec + ":" + s.Spec + ":ro",
		"--volume", s.Log + ":" + s.Log,
	}
	if s.Inbox != "" {
		args = append(args, "--volume", s.Inbox+":"+s.Inbox)
	}
	if p.Mirror != "" {
		args = append(args, "--volume", p.Mirror+":"+MirrorPath+":ro")
	}
	if s.ClaudeDir != "" {
		args = append(args, "--volume", s.ClaudeDir+":"+s.ClaudePath)
	}
	for _, b := range s.Binaries {
		args = append(args, "--volume", b+":"+b+":ro")
	}
	if s.Data != "" {
		args = append(args, "--volume", s.Data+":"+s.Data+":ro")
	}
	if s.Slot != 0 {
		args = append(args, "--volume", fmt.Sprintf("%s-slot%d:%s", p.Prefix, s.Slot, storagePath))
	}
	for _, m := range p.Mounts {
		args = append(args, "--volume", expandHome(m))
	}
	for _, name := range p.passed(s) {
		args = append(args, "--env", name)
	}
	return append(args, p.Image, p.Self, "sandbox-run", s.Spec)
}

// passed lists the variables handed in by name: the configured ones that are
// set, and every secret.
func (p Podman) passed(s Start) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range p.Pass {
		if _, ok := os.LookupEnv(n); ok && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for n := range s.Secrets {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Launch starts the sandbox and returns its container name.
func (p Podman) Launch(ctx context.Context, s Start) (string, error) {
	name := p.Name(s.RunID)
	// A container of this name is a previous run's that was never torn down
	// — the supervisor died between its exit and the reap. Nothing live can
	// own it: the engine starts a run only when none is live for its issue.
	if _, err := p.run(ctx, nil, "rm", "--force", "--ignore", name); err != nil {
		return "", err
	}
	// Bind mounts of files need the file to exist.
	for _, f := range []string{s.Log} {
		if err := touch(f); err != nil {
			return "", err
		}
	}
	if s.Inbox != "" {
		if err := os.MkdirAll(s.Inbox, 0o755); err != nil {
			return "", err
		}
	}
	// A mount whose source does not exist yet is created as a directory, as
	// Docker does and podman does not. Some sources come and go with the
	// host: /run/user is emptied at every boot, and the directories a tool
	// keeps there exist only once it has run.
	for _, m := range p.Mounts {
		if err := ensureSource(expandHome(m)); err != nil {
			return "", err
		}
	}
	if _, err := p.run(ctx, s.Secrets, p.RunArgs(s)...); err != nil {
		return "", fmt.Errorf("start sandbox: %w", err)
	}
	return name, nil
}

// Running reports whether a sandbox's container is still running. A container
// that does not exist is not running.
func (p Podman) Running(ctx context.Context, name string) (bool, error) {
	out, err := p.run(ctx, nil, "container", "inspect", "--format", "{{.State.Running}}", name)
	if err != nil {
		if strings.Contains(err.Error(), "no such container") {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

// Stop asks the container to stop, giving the agent time to finish a write.
func (p Podman) Stop(ctx context.Context, name string) error {
	_, err := p.run(ctx, nil, "stop", "--ignore", "--time", "30", name)
	return err
}

// Remove deletes the container, and with it the run's checkout and stack. The
// slot's image storage is a volume and stays.
func (p Podman) Remove(ctx context.Context, name string) error {
	_, err := p.run(ctx, nil, "rm", "--force", "--ignore", "--time", "10", name)
	return err
}

// SlotBusy reports whether any of this project's sandboxes, running or not,
// holds slot n.
func (p Podman) SlotBusy(ctx context.Context, n int) (bool, error) {
	out, err := p.run(ctx, nil, "ps", "--all", "--quiet",
		"--filter", "label="+labelRepo+"="+p.Repo,
		"--filter", "label="+labelSlot+"="+strconv.Itoa(n))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func (p Podman) run(ctx context.Context, secrets map[string]string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.bin(), args...)
	cmd.Env = os.Environ()
	for k, v := range secrets {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", p.bin(), args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func touch(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// ensureSource creates the host side of a "host:container[:opts]" mount when
// nothing is there.
func ensureSource(mount string) error {
	host, _, _ := strings.Cut(mount, ":")
	if host == "" || !filepath.IsAbs(host) {
		return nil
	}
	if _, err := os.Lstat(host); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return os.MkdirAll(host, 0o700)
}

func expandHome(mount string) string {
	if !strings.HasPrefix(mount, "~/") {
		return mount
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return mount
	}
	return filepath.Join(home, mount[2:])
}
