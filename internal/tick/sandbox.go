package tick

import (
	"context"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/runner"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

// Sandbox is where a run happens: the checkout it works in, the project's
// stack, and the agent process itself.
//
// Everything else the engine does — scheduling, labels, comments, waiting on
// checks, landing — is a conversation with GitHub and does not care where a
// run is. Keeping the where behind one interface is what lets a run move off
// the host without the rest of the engine knowing.
//
// The local sandbox is the original arrangement: a worktree of the host's
// checkout, a Compose project per slot on the host's engine, and the agent as
// a detached host process. The container sandbox gives every run a fresh
// container with its own checkout and its own nested stack.
type Sandbox interface {
	// Launch prepares the run's environment and starts its agent, returning
	// once the agent is running. The returned run records whatever Alive,
	// Stop and Teardown need to find it again — after a supervisor restart
	// too, since they are given only the record.
	Launch(ctx context.Context, l Launch) (state.Run, error)
	// Alive reports whether the run's agent is still going.
	Alive(r state.Run) bool
	// Stop asks the run's agent, and everything it started, to stop.
	Stop(r state.Run) error
	// Teardown releases the run's environment once its agent is gone. It is
	// safe to call on a run that holds nothing.
	Teardown(ctx context.Context, r state.Run) error
	// Busy reports whether a slot is occupied, by a run or by anything else
	// the sandbox can see.
	Busy(ctx context.Context, slot int) (bool, error)
}

// Launch is what a sandbox needs to start one agent.
type Launch struct {
	// Run is the record being started: kind, ref, slot, branch and log are
	// set. On a resume, Worktree and Sandbox are whatever the previous agent
	// left.
	Run state.Run
	// Resume means the branch already exists and the run is going back into
	// it — a fix, a revision, a conflict resolution.
	Resume bool

	Command string
	Args    []string
	// Env is everything the agent is told except where it is: the sandbox
	// adds CLAUDEFLOW_WORKTREE and the stack's service addresses, which only
	// it knows.
	Env     runner.Env
	Timeout time.Duration
}

// sandbox returns the configured sandbox, the local one by default.
func (e Engine) sandbox() Sandbox {
	if e.Sandbox != nil {
		return e.Sandbox
	}
	return localSandbox{e: e}
}

// RunAlive reports whether a recorded run's agent is still going.
func (e Engine) RunAlive(r state.Run) bool { return e.sandbox().Alive(r) }

// StopRun asks a recorded run's agent to stop.
func (e Engine) StopRun(r state.Run) error { return e.sandbox().Stop(r) }
