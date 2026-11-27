package tick

import (
	"context"
	"fmt"
	"syscall"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/runner"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

// localSandbox runs on the host: a worktree of the host's checkout, a Compose
// project per slot on the host's engine, and the agent as a detached process
// in its own process group.
type localSandbox struct{ e Engine }

func (s localSandbox) Launch(ctx context.Context, l Launch) (state.Run, error) {
	e, r := s.e, l.Run
	if r.Kind.HoldsSlot() {
		if l.Resume {
			if err := e.stackUp(ctx, r.Slot, r.Worktree, r.Branch, r.ID()); err != nil {
				return r, err
			}
		} else {
			wt, err := e.prepare(ctx, r)
			if err != nil {
				return r, err
			}
			r.Worktree = wt
		}
	}

	env := runner.Env{}
	for k, v := range l.Env {
		env[k] = v
	}
	env["CLAUDEFLOW_WORKTREE"] = r.Worktree
	if r.Kind.HoldsSlot() {
		resolved := e.serviceURLs(ctx, r.Slot)
		for k, v := range resolved {
			env[k] = v
		}
		for k, v := range expandAgentEnv(e.Cfg.Agent.Env, resolved) {
			env[k] = v
		}
	}

	dir := r.Worktree
	if dir == "" {
		dir = e.Cfg.Paths.Root
	}
	// Started is what re-adoption checks the pid's kernel start time against,
	// so it is stamped at the fork. Stamped before prepare, it sat minutes
	// early behind the install hook and a cold stack, and a live run read as a
	// recycled pid and was reaped under its agent.
	r.Started = e.now()
	// The run outlives the tick that started it. context.WithoutCancel keeps
	// deadlines and values while dropping cancellation, so a supervisor that
	// exits — `claudeflow once` returning, or serve taking a SIGTERM — does
	// not take its children with it.
	//
	// Without this, `once` killed the agent microseconds after spawning it:
	// the signal context is cancelled by main's deferred stop() as the command
	// returns, and the run died having written nothing at all. A run is
	// bounded by its own Timeout, not by how long the supervisor happens to
	// live.
	proc, err := runner.Start(context.WithoutCancel(ctx), runner.Spawn{
		Command: l.Command,
		Args:    l.Args,
		Dir:     dir,
		Env:     env,
		Timeout: l.Timeout,
		LogPath: r.Log,
	})
	if err != nil {
		return r, fmt.Errorf("start agent: %w", err)
	}
	r.PID = proc.PID()
	return r, nil
}

func (s localSandbox) Alive(r state.Run) bool {
	return r.PID != 0 && runner.AliveSince(r.PID, r.Started)
}

// Stop signals the whole group: the agent spawns children, and leaving them
// behind holds the slot forever.
func (s localSandbox) Stop(r state.Run) error {
	if r.PID == 0 {
		return nil
	}
	return syscall.Kill(-r.PID, syscall.SIGTERM)
}

func (s localSandbox) Teardown(ctx context.Context, r state.Run) error {
	if !r.Kind.HoldsSlot() || r.Slot == 0 {
		return nil
	}
	return s.e.stack(r.Slot).Down(ctx)
}

func (s localSandbox) Busy(ctx context.Context, slot int) (bool, error) {
	return s.e.stack(slot).Running(ctx)
}

// stackUp brings a slot's stack up from nothing and waits for it, leaving
// nothing half-up if it does not become ready.
func (e Engine) stackUp(ctx context.Context, slot int, worktree, branch, id string) error {
	st := e.stack(slot)
	st.Env = e.stackEnv(slot, worktree, branch)
	// Take down whatever the slot's project is holding first. A previous run
	// that was killed rather than reaped leaves containers behind, and Up
	// against them starts nothing.
	if err := st.Down(ctx); err != nil {
		e.logf("%s: pre-emptive teardown: %v", id, err)
	}
	e.logf("%s: bringing the stack up on slot %d (project %s)", id, slot, st.Project)
	if err := st.Up(ctx); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	if err := st.WaitReady(ctx, e.Cfg.Compose.ReadyTimeout, 2*time.Second); err != nil {
		if derr := st.Down(ctx); derr != nil {
			e.logf("%s: teardown after a failed start: %v", id, derr)
		}
		return fmt.Errorf("stack did not become ready: %w", err)
	}
	return nil
}
