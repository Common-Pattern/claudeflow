package tick

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/land"
	"github.com/Common-Pattern/claudeflow/internal/runner"
	"github.com/Common-Pattern/claudeflow/internal/state"
	"github.com/Common-Pattern/claudeflow/skills"
)

// advanceAwaitingCI moves every run whose agent has gone and whose pull request
// is open.
//
// Waiting on checks used to be the agent's step 8: it sat in `gh pr checks
// --watch` for ten or twenty minutes, holding a slot and a model session to do
// nothing but poll. Here the same wait costs one API call per tick. It also
// removes the step an agent is most likely to get wrong — the one run that
// reached this point backgrounded the wait and ended its turn, leaving a
// finished pull request that nothing would ever merge.
//
// Green lands without an agent at all. Red is the only case that needs one, and
// it gets a fresh one with the failure already in hand.
func (e Engine) advanceAwaitingCI(ctx context.Context) error {
	runs, err := e.Store.Runs()
	if err != nil {
		return err
	}
	for _, r := range runs {
		if !r.InPhase(state.PhaseAwaitingCI) || r.PR == 0 {
			continue
		}
		if err := e.advanceOne(ctx, r); err != nil {
			e.logf("%s: %v", r.ID(), err)
		}
	}
	return nil
}

func (e Engine) advanceOne(ctx context.Context, r state.Run) error {
	checks, err := e.Client.Checks(ctx, r.PR)
	if err != nil {
		return fmt.Errorf("read checks on #%d: %w", r.PR, err)
	}
	if len(checks) == 0 {
		// No checks yet is not evidence of anything. In particular it is not
		// "green": a merge here would land before CI was ever scheduled.
		e.logf("%s: no checks reported on #%d yet", r.ID(), r.PR)
		return nil
	}
	if pending := forge.Pending(checks); len(pending) > 0 {
		e.logf("%s: #%d still running %d check(s)", r.ID(), r.PR, len(pending))
		return nil
	}

	if failed := forge.Failed(checks); len(failed) > 0 {
		return e.handleRedCI(ctx, r, failed)
	}
	return e.landRun(ctx, r)
}

// landRun merges a run's pull request now that its checks are green.
func (e Engine) landRun(ctx context.Context, r state.Run) error {
	e.logf("%s: #%d is green; landing", r.ID(), r.PR)

	unlock, err := e.lockLanding()
	if err != nil {
		return err
	}
	defer unlock()

	l := land.Lander{
		Cfg: e.Cfg, Client: e.Client, Repo: e.Repo, Log: e.Log,
		Required: e.Cfg.RequiredChecks,
	}
	res, err := l.Land(ctx, r.PR, r.Worktree)
	if err != nil {
		e.logf("%s: could not land #%d: %v", r.ID(), r.PR, err)
		return e.finishRun(ctx, r, e.Cfg.Labels.Blocked, fmt.Sprintf(
			"Checks passed on #%d but landing did not complete:\n\n```\n%v\n```\n\nThe pull request is still open.", r.PR, err))
	}

	body := fmt.Sprintf("Landed #%d on `%s` as `%s`.", res.PR, e.Cfg.Branches.Integration, short(res.MergedSHA))
	if len(res.Closes) > 0 {
		body += fmt.Sprintf("\n\nThe standing pull request will close this issue when it merges into `%s`.", e.Cfg.Branches.Base)
	}
	return e.finishRun(ctx, r, e.Cfg.Labels.Landed, body)
}

// handleRedCI sends one agent at the failing checks, up to the configured cap.
func (e Engine) handleRedCI(ctx context.Context, r state.Run, failed []string) error {
	names := strings.Join(failed, ", ")

	if r.Attempts >= e.Cfg.Limits.FixAttempts {
		e.logf("%s: #%d still failing after %d attempt(s); giving up", r.ID(), r.PR, r.Attempts)
		return e.finishRun(ctx, r, e.Cfg.Labels.Blocked, fmt.Sprintf(
			"Checks on #%d are still failing after %d attempt(s): **%s**.\n\nThe pull request is open and the branch is intact; it needs a look.",
			r.PR, r.Attempts, names))
	}
	if liveFixFor(mustRuns(e), r.PR) {
		return nil
	}

	slot, err := e.allocator().Free(ctx, claimedSlots(liveSlotRuns(mustRuns(e))))
	if err != nil {
		e.logf("%s: #%d needs a fix run but no slot is free", r.ID(), r.PR)
		return nil
	}

	r.Attempts++
	r.Phase = state.PhaseRunning
	r.Slot = slot
	if err := e.Store.SaveRun(r); err != nil {
		return err
	}
	e.logf("%s: #%d failed %s; starting fix attempt %d", r.ID(), r.PR, names, r.Attempts)

	if err := e.startFix(ctx, r, names); err != nil {
		// Put it back to waiting so the next tick can try again rather than
		// losing the run entirely.
		r.Phase = state.PhaseAwaitingCI
		if serr := e.Store.SaveRun(r); serr != nil {
			return serr
		}
		return fmt.Errorf("start fix for #%d: %w", r.PR, err)
	}
	return nil
}

// finishRun resolves the issue and clears the run.
func (e Engine) finishRun(ctx context.Context, r state.Run, label, body string) error {
	if r.Kind != state.KindReview {
		if err := e.Client.Comment(ctx, r.Ref, body); err != nil {
			e.logf("%s: could not comment: %v", r.ID(), err)
		}
		// Clear every other outcome, not just the working label. An issue that
		// is both landed and blocked tells a reader nothing, and the stale one
		// outlives the run that set it.
		stale := []string{e.Cfg.Labels.Working}
		for _, other := range e.Cfg.Labels.Resolution() {
			if other != label {
				stale = append(stale, other)
			}
		}
		if err := e.Client.EditLabels(ctx, r.Ref, []string{label}, stale); err != nil {
			e.logf("%s: could not set %s: %v", r.ID(), label, err)
		}
	}
	return e.Store.DeleteRun(r.ID())
}

// awaitCI parks a finished agent's run on its pull request.
//
// The stack comes down — the slot is what is scarce, and nothing about waiting
// for checks needs it. The worktree stays, because landing merges the
// integration branch into the branch when it has fallen behind.
func (e Engine) awaitCI(ctx context.Context, r state.Run, pr int) error {
	e.logf("%s: opened #%d; claudeflow takes it from here", r.ID(), pr)
	if r.Kind.HoldsSlot() {
		if err := e.stack(r.Slot).Down(ctx); err != nil {
			e.logf("%s: teardown reported a problem: %v", r.ID(), err)
		}
	}
	r.PR = pr
	r.Phase = state.PhaseAwaitingCI
	r.PID = 0
	// The slot is released with the stack. Keeping the number would reserve an
	// environment for a run that is only waiting on a web request.
	r.Slot = 0
	return e.Store.SaveRun(r)
}

func mustRuns(e Engine) []state.Run {
	runs, err := e.Store.Runs()
	if err != nil {
		return nil
	}
	return runs
}

func liveSlotRuns(runs []state.Run) []state.Run {
	var out []state.Run
	for _, r := range runs {
		if r.InPhase(state.PhaseRunning) && r.Kind.HoldsSlot() {
			out = append(out, r)
		}
	}
	return out
}

func liveFixFor(runs []state.Run, pr int) bool {
	for _, r := range runs {
		if r.PR == pr && r.InPhase(state.PhaseRunning) {
			return true
		}
	}
	return false
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// lockLanding serialises landing across runs.
//
// Two runs merging into the integration branch at once each pass checks
// against a base the other is about to move, and the checkout ends up
// fast-forwarded to a tree neither of them tested.
func (e Engine) lockLanding() (func(), error) {
	path := filepath.Join(e.Store.Dir(), "land.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open landing lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("take landing lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// startFix brings the run's environment back up and sends an agent at the
// failing checks, with the failure already named.
func (e Engine) startFix(ctx context.Context, r state.Run, failing string) error {
	st := e.stack(r.Slot)
	st.Env = e.stackEnv(r.Slot, r.Worktree, r.Branch)
	if err := st.Down(ctx); err != nil {
		e.logf("%s: pre-emptive teardown: %v", r.ID(), err)
	}
	if err := st.Up(ctx); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	if err := st.WaitReady(ctx, e.Cfg.Compose.ReadyTimeout, 2*time.Second); err != nil {
		if derr := st.Down(ctx); derr != nil {
			e.logf("%s: teardown after a failed start: %v", r.ID(), derr)
		}
		return fmt.Errorf("stack did not become ready: %w", err)
	}

	instructions, err := skills.Read(skills.Fix, e.Cfg.Agent.SkillsDir)
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf(
		"The checks on pull request #%d are failing: %s. Fix them, following the run instructions in your system prompt exactly. This is attempt %d of %d.",
		r.PR, failing, r.Attempts, e.Cfg.Limits.FixAttempts)

	args := []string{
		"-p", prompt,
		"--append-system-prompt", string(instructions),
		"--permission-mode", "bypassPermissions",
		"--output-format", "text",
	}
	if e.Cfg.Agent.Model != "" {
		args = append(args, "--model", e.Cfg.Agent.Model)
	}
	args = append(args, e.Cfg.Agent.ExtraArgs...)

	logPath := filepath.Join(e.Store.LogDir(),
		fmt.Sprintf("fix-%d-%s.log", r.PR, e.now().UTC().Format("20060102T150405")))

	env := e.runEnv(ctx, Start{Kind: r.Kind, Ref: r.Ref, Slot: r.Slot}, r)
	env["CLAUDEFLOW_PR"] = strconv.Itoa(r.PR)
	env["CLAUDEFLOW_FAILING_CHECKS"] = failing
	env["CLAUDEFLOW_ATTEMPT"] = strconv.Itoa(r.Attempts)

	// A fix run is a new process. Keeping the first run's start time made
	// every fix look like a recycled pid, and the next tick reaped it and took
	// its stack down while the agent was still working.
	r.Started = e.now()
	proc, err := runner.Start(context.WithoutCancel(ctx), runner.Spawn{
		Command: e.Cfg.Agent.Command,
		Args:    args,
		Dir:     r.Worktree,
		Env:     env,
		Timeout: e.Cfg.Limits.BuildTimeout,
		LogPath: logPath,
	})
	if err != nil {
		return fmt.Errorf("start agent: %w", err)
	}

	r.PID = proc.PID()
	r.Log = logPath
	if err := e.Store.SaveRun(r); err != nil {
		return err
	}
	e.logf("%s: fix run started (pid %d, slot %d, log %s)", r.ID(), r.PID, r.Slot, logPath)
	return nil
}
