// Package runner starts and supervises the child processes claudeflow owns:
// the project's hook commands and the agent CLI itself.
//
// Two things distinguish this from calling os/exec directly. Every child is
// put in its own session, so a run can be stopped as a group rather than
// leaving the agent's own children behind holding a slot. And a run that ran
// out of wall clock is reported differently from one that failed on the work,
// because only the second says anything about the code.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Env is extra environment for a child, layered on top of the daemon's own.
type Env map[string]string

// Result is everything a finished process reported.
type Result struct {
	// ExitCode is the process's own status, or ExitTimeout when the wall clock
	// ran out, or 128+signal when something killed it.
	ExitCode int
	// Stdout and Stderr hold the tail of each stream, up to MaxCapturedBytes.
	// The log file, where one is configured, is the complete transcript.
	Stdout string
	Stderr string
	// TimedOut reports that the process was killed for exceeding its timeout
	// rather than exiting on its own.
	TimedOut bool
}

const (
	// ExitTimeout is the exit code reported for a timed-out process. It matches
	// coreutils timeout(1) so that a transcript read by hand says the same
	// thing as one read by claudeflow.
	ExitTimeout = 124
	// ExitUnknown is reported when a process produced no status at all, which
	// happens only when waiting on it failed.
	ExitUnknown = -1
)

// DefaultStopGrace is how long a process has to handle SIGTERM before SIGKILL.
const DefaultStopGrace = 10 * time.Second

// MaxCapturedBytes bounds how much of each stream is held in memory. An agent
// run lasts hours and the daemon holds every live run at once, so the whole
// transcript goes to the log file and only the tail is kept here — the tail is
// where a CLI's refusal or a final error appears.
var MaxCapturedBytes = 1 << 20

// Runner executes shell commands — the project's hooks — and waits for them.
type Runner struct {
	// Dir is the working directory. Empty means the daemon's own.
	Dir string
	// Env is layered over os.Environ().
	Env Env
	// Timeout is the wall clock a command gets. Zero means none.
	Timeout time.Duration
	// LogPath, when set, receives stdout and stderr interleaved, appended.
	LogPath string
}

// Exec runs command through sh, so that the variable expansion, pipes and
// redirection a project writes in its config behave as they do in a shell.
//
// A non-zero exit, a signal and a timeout are all outcomes, reported in the
// Result with a nil error. The error is non-nil only when the command could
// not be started at all, when waiting on it failed, or when ctx was cancelled
// by the caller.
func (r *Runner) Exec(ctx context.Context, command string) (Result, error) {
	p, err := Start(ctx, Spawn{
		Command: "sh",
		Args:    []string{"-c", command},
		Dir:     r.Dir,
		Env:     r.Env,
		Timeout: r.Timeout,
		LogPath: r.LogPath,
	})
	if err != nil {
		return Result{}, err
	}
	res := p.Wait()
	if err := p.Err(); err != nil {
		return res, fmt.Errorf("run %q: %w", command, err)
	}
	return res, nil
}

// Spawn describes an agent invocation. Unlike a hook it is an argv, not a
// shell string: the prompt is an argument and must not be re-parsed.
type Spawn struct {
	// Command is the binary, resolved against PATH.
	Command string
	Args    []string
	Dir     string
	Env     Env
	// Timeout is the wall clock the process gets. Zero means none.
	Timeout time.Duration
	// LogPath receives stdout and stderr interleaved, appended.
	LogPath string
	// StopGrace overrides DefaultStopGrace for this process.
	StopGrace time.Duration
}

// Process is a started child and its supervision.
type Process struct {
	cmd  *exec.Cmd
	pgid int

	// StopGrace is how long SIGTERM is given before SIGKILL follows. Writing it
	// after Stop has been called has no effect on that call.
	StopGrace time.Duration

	stdout *capture
	stderr *capture
	logs   io.Closer

	timedOut atomic.Bool
	canceled atomic.Bool

	done    chan struct{}
	result  Result
	waitErr error
}

// Start begins the process and returns as soon as it has a PID; it does not
// wait for the process to finish.
//
// The child is given its own session, so its PID is also its process group id
// and everything it spawns inherits that group. Stopping the group is the only
// way to reach an agent's own children; one left behind keeps holding the run's
// slot after the run is gone.
func Start(ctx context.Context, s Spawn) (*Process, error) {
	if s.Command == "" {
		return nil, errors.New("runner: spawn has no command")
	}

	stdout := newCapture()
	stderr := newCapture()
	outw, errw := io.Writer(stdout), io.Writer(stderr)
	var logs io.Closer
	if s.LogPath != "" {
		f, err := openLog(s.LogPath)
		if err != nil {
			return nil, err
		}
		// Hand the child the *os.File itself, not a wrapper.
		//
		// os/exec only passes a descriptor straight through when Stdout is an
		// *os.File. Given any other io.Writer it creates a pipe and copies from
		// it in a goroutine belonging to THIS process — so a supervisor that
		// exits after starting a detached run takes the copier with it, and
		// every byte the child writes afterwards goes nowhere. The symptom is a
		// run that plainly worked against a transcript of zero bytes, which
		// also blinds the usage-limit check that reads it.
		//
		// The cost is that Result.Stdout and Result.Stderr stay empty for a
		// run started this way. That is the right trade: a detached run's
		// output belongs in the file, and callers read it from there.
		outw, errw = f, f
		logs = f
	}

	cmd := exec.Command(s.Command, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = environ(s.Env)
	cmd.Stdout = outw
	cmd.Stderr = errw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		if logs != nil {
			logs.Close()
		}
		return nil, fmt.Errorf("start %s: %w", s.Command, err)
	}

	grace := s.StopGrace
	if grace <= 0 {
		grace = DefaultStopGrace
	}
	p := &Process{
		cmd:       cmd,
		pgid:      cmd.Process.Pid,
		StopGrace: grace,
		stdout:    stdout,
		stderr:    stderr,
		logs:      logs,
		done:      make(chan struct{}),
	}
	go p.supervise(ctx, s.Timeout)
	return p, nil
}

// PID returns the child's process id, which is also its process group id.
func (p *Process) PID() int { return p.cmd.Process.Pid }

// Wait blocks until the process has exited and been reaped, and returns what
// it reported. It may be called any number of times and from any goroutine.
func (p *Process) Wait() Result {
	<-p.done
	return p.result
}

// Err returns the reason supervision failed, as distinct from the process
// failing: waiting on the child errored, or the caller's context was
// cancelled. A non-zero exit, a signal and a timeout are not errors.
func (p *Process) Err() error {
	<-p.done
	if p.waitErr != nil {
		return p.waitErr
	}
	if p.canceled.Load() {
		return context.Canceled
	}
	return nil
}

// Stop ends the process and everything it spawned: SIGTERM to the group, then
// SIGKILL to the group once StopGrace has passed. It blocks until the child
// has been reaped.
//
// SIGTERM comes first so that the agent CLI can write out its transcript and
// release its own children; killing outright loses the record of why the run
// was stopped.
func (p *Process) Stop() error {
	err := p.terminate()
	<-p.done
	return err
}

func (p *Process) supervise(ctx context.Context, timeout time.Duration) {
	waited := make(chan error, 1)
	go func() { waited <- p.cmd.Wait() }()

	var expired <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		expired = t.C
	}
	cancelled := ctx.Done()

	for {
		select {
		case err := <-waited:
			p.finish(err)
			return
		case <-expired:
			expired = nil
			p.timedOut.Store(true)
			go func() { _ = p.terminate() }()
		case <-cancelled:
			cancelled = nil
			p.canceled.Store(true)
			go func() { _ = p.terminate() }()
		}
	}
}

// terminate signals the group and escalates in the background, so that callers
// of it from the supervision loop do not block the reaping of the child they
// are waiting for.
func (p *Process) terminate() error {
	grace := p.StopGrace
	if err := p.signalGroup(syscall.SIGTERM); err != nil {
		return err
	}
	select {
	case <-p.done:
		return nil
	case <-time.After(grace):
	}
	return p.signalGroup(syscall.SIGKILL)
}

// signalGroup signals the whole process group. The negative PID is what
// reaches the agent's children; signalling p.pgid alone leaves them running.
func (p *Process) signalGroup(sig syscall.Signal) error {
	err := syscall.Kill(-p.pgid, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return fmt.Errorf("signal %s to group %d: %w", sig, p.pgid, err)
}

func (p *Process) finish(waitErr error) {
	res := Result{
		ExitCode: exitCode(p.cmd.ProcessState),
		Stdout:   p.stdout.String(),
		Stderr:   p.stderr.String(),
		TimedOut: p.timedOut.Load(),
	}
	if res.TimedOut {
		res.ExitCode = ExitTimeout
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		p.waitErr = fmt.Errorf("wait for pid %d: %w", p.pgid, waitErr)
	}
	if p.logs != nil {
		p.logs.Close()
	}
	p.result = res
	close(p.done)
}

// Alive reports whether a process with this id exists.
//
// It is a signal-0 probe and nothing more. It proves that *a* process holds
// the id, not that it is the one that held it before: PIDs are recycled, so a
// daemon re-adopting a run recorded by an earlier daemon can be told "alive"
// about an unrelated process. Where the answer decides whether to adopt, use
// AliveSince, which checks the kernel's start time for the id against the one
// recorded for the run.
//
// EPERM counts as alive: the id is held by a process owned by another user,
// which is still a process.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// StartTimeTolerance is how far a process's kernel start time may sit from the
// start time recorded for a run and still be taken as the same process. A run
// record is written immediately before the fork, so the gap is normally under
// a second; the allowance covers a coarse clock and a slow write.
var StartTimeTolerance = 60 * time.Second

// AliveSince reports whether pid is running and began around started, which is
// the question a daemon re-adopting a run after a restart actually has.
//
// It reads the kernel's own start time for the id, so a recycled id is
// rejected unless it happens to have been reused within StartTimeTolerance of
// the original. Where the start time cannot be read — no /proc, another
// mount namespace — it degrades to Alive, and can then be wrong about a
// recycled id in the same way.
func AliveSince(pid int, started time.Time) bool {
	if !Alive(pid) {
		return false
	}
	actual, ok := processStart(pid)
	if !ok {
		return true
	}
	gap := actual.Sub(started)
	if gap < 0 {
		gap = -gap
	}
	return gap <= StartTimeTolerance
}

// processStart reads when the kernel says pid began, which is the only start
// time no bookkeeping of ours can get wrong.
//
// It is field 22 of /proc/<pid>/stat, in clock ticks since boot. It is NOT the
// mtime of /proc/<pid>: that is when the kernel instantiated the proc inode,
// which is whenever something first looked, and again after the inode is
// evicted. On a host up for three weeks most processes carried an mtime days
// after their start, so a live run re-adopted after a restart could be judged
// a recycled pid and reaped with its agent still working.
func processStart(pid int) (time.Time, bool) {
	raw, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "stat"))
	if err != nil {
		return time.Time{}, false
	}
	ticks, ok := statStartTicks(string(raw))
	if !ok {
		return time.Time{}, false
	}
	boot, ok := bootTime()
	if !ok {
		return time.Time{}, false
	}
	return boot.Add(time.Duration(ticks) * time.Second / clockTicks), true
}

// clockTicks is USER_HZ, the unit of /proc/<pid>/stat's start time. The kernel
// fixes it at 100 for userspace on every architecture Linux runs on; reading it
// properly needs sysconf, and with it cgo.
const clockTicks = 100

// statStartTicks extracts the start time from a /proc/<pid>/stat line. The
// command name is field 2 and may itself hold spaces and parentheses, so the
// fields are counted from the last ')'.
func statStartTicks(stat string) (uint64, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	// After the name come the state (field 3) onwards; start time is field 22.
	fields := strings.Fields(stat[i+1:])
	const startField = 22 - 3
	if len(fields) <= startField {
		return 0, false
	}
	n, err := strconv.ParseUint(fields[startField], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// bootTime reads when the system booted, from /proc/stat's btime line.
func bootTime() (time.Time, bool) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			return time.Unix(secs, 0), true
		}
	}
	return time.Time{}, false
}

func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return ExitUnknown
	}
	if code := ps.ExitCode(); code >= 0 {
		return code
	}
	// A signalled process reports -1 through ExitCode. Report it the way a
	// shell does, so that 143 in a transcript reads as "SIGTERM" rather than
	// as an exit status the agent chose.
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ExitUnknown
}

func environ(extra Env) []string {
	if len(extra) == 0 {
		return os.Environ()
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := os.Environ()
	for _, k := range keys {
		env = append(env, k+"="+extra[k])
	}
	return env
}

// openLog appends to path, creating its parent. Appending is what makes a
// retried run readable: truncating would erase the transcript of the attempt
// that explains why there was a retry.
func openLog(path string) (*os.File, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("prepare log dir for %s: %w", path, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log %s: %w", path, err)
	}
	return f, nil
}

// capture keeps the last MaxCapturedBytes of a stream.
type capture struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func newCapture() *capture { return &capture{limit: MaxCapturedBytes} }

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	if c.limit > 0 && len(c.buf) > c.limit {
		c.buf = append(c.buf[:0], c.buf[len(c.buf)-c.limit:]...)
	}
	return len(p), nil
}

func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}
