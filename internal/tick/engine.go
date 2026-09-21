package tick

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/compose"
	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/git"
	"github.com/Common-Pattern/claudeflow/internal/runner"
	"github.com/Common-Pattern/claudeflow/internal/slot"
	"github.com/Common-Pattern/claudeflow/internal/state"
	"github.com/Common-Pattern/claudeflow/skills"
)

// Engine observes, plans and acts. One call to Once is one tick.
type Engine struct {
	Cfg    config.Config
	Client forge.Client
	Store  *state.Store
	Repo   *git.Repo
	Log    func(format string, args ...any)
	// Now is injectable for tests.
	Now func() time.Time
}

func (e Engine) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log(format, args...)
	}
}

func (e Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Once runs a single tick: reap what finished, watch both comment channels,
// then start what the plan says to start.
func (e Engine) Once(ctx context.Context) error {
	if err := e.Reap(ctx); err != nil {
		return err
	}
	if err := DispatchBlocked(e.Store, e.now()); err != nil {
		e.logf("not dispatching: %v", err)
		return nil
	}

	w := Watcher{Client: e.Client, Store: e.Store, Cfg: e.Cfg}
	requeued, err := w.Requeue(ctx)
	if err != nil {
		return err
	}
	for _, n := range requeued {
		e.logf("issue #%d: new comment; re-queued", n)
	}

	pr, owed, err := w.ReviewOwed(ctx)
	if err != nil {
		// Two standing pull requests is a state a human must fix. Report it
		// and keep the issue lane working rather than halting everything.
		e.logf("standing pull request: %v", err)
		pr, owed = 0, false
	}

	open, err := e.Client.OpenIssues(ctx, e.Cfg.Labels.Queued)
	if err != nil {
		return fmt.Errorf("list queued issues: %w", err)
	}
	build, plan := SplitQueued(open, e.Cfg.Labels)

	live, err := e.liveRuns()
	if err != nil {
		return err
	}

	starts := Plan(ctx, Inputs{
		Now: e.now(), Live: live,
		QueuedBuild: build, QueuedPlan: plan,
		ReviewOwed: owed, StandingPR: pr,
		Limits: e.Cfg.Limits,
		Alloc:  e.allocator(),
	})

	for _, s := range starts {
		if err := e.Start(ctx, s); err != nil {
			e.logf("%s #%d: could not start: %v", s.Kind, s.Ref, err)
			continue
		}
		if s.Kind == state.KindReview {
			if err := w.ClearReviewOwed(s.Ref); err != nil {
				e.logf("could not clear the review marker: %v", err)
			}
		}
	}
	return nil
}

// stack returns the Compose stack for a slot. Each slot is its own Compose
// project, which is what makes isolation structural: a teardown can only reach
// resources its own project owns.
func (e Engine) stack(n int) compose.Compose {
	return compose.Compose{
		Bin:      e.Cfg.Compose.Bin,
		File:     e.Cfg.Compose.File,
		Project:  e.Cfg.Compose.Project(n),
		Dir:      e.Cfg.Paths.Root,
		Profiles: e.Cfg.Compose.Profiles,
		Env:      e.stackEnv(n, "", ""),
	}
}

// stackEnv is what the Compose file may interpolate. The slot is the knob a
// project uses to offset published ports.
func (e Engine) stackEnv(n int, worktree, branch string) map[string]string {
	return map[string]string{
		"CLAUDEFLOW_SLOT":     strconv.Itoa(n),
		"CLAUDEFLOW_WORKTREE": worktree,
		"CLAUDEFLOW_BRANCH":   branch,
		"CLAUDEFLOW_PROJECT":  e.Cfg.Compose.Project(n),
	}
}

func (e Engine) allocator() slot.Allocator {
	return slot.Allocator{
		Min: e.Cfg.Slots.Min, Max: e.Cfg.Slots.Max,
		// A slot is busy when its Compose project has containers. That covers
		// a stack another checkout left up, which claudeflow's own records
		// cannot see, and needs nothing configured.
		Busy: func(ctx context.Context, n int) (bool, error) {
			return e.stack(n).Running(ctx)
		},
	}
}

// liveRuns returns run records whose process is still going, reaping is left
// to Reap so that Once sees a consistent picture.
func (e Engine) liveRuns() ([]state.Run, error) {
	all, err := e.Store.Runs()
	if err != nil {
		return nil, err
	}
	var live []state.Run
	for _, r := range all {
		if runner.AliveSince(r.PID, r.Started) {
			live = append(live, r)
		}
	}
	return live, nil
}

// Reap resolves runs whose process is gone.
func (e Engine) Reap(ctx context.Context) error {
	all, err := e.Store.Runs()
	if err != nil {
		return err
	}
	for _, r := range all {
		if runner.AliveSince(r.PID, r.Started) {
			continue
		}
		e.logf("%s: run finished", r.ID())

		if r.Kind != state.KindReview {
			if err := e.resolveAbandoned(ctx, r); err != nil {
				e.logf("%s: %v", r.ID(), err)
			}
		}
		if r.Kind.HoldsSlot() {
			e.logf("%s: taking the stack down on slot %d", r.ID(), r.Slot)
			if err := e.stack(r.Slot).Down(ctx); err != nil {
				e.logf("%s: teardown reported a problem: %v", r.ID(), err)
			}
		}
		if err := e.Store.DeleteRun(r.ID()); err != nil {
			return err
		}
	}
	return nil
}

// resolveAbandoned marks an issue blocked when its run exited without setting
// its own label.
//
// The agent normally resolves itself. A run still carrying the working label
// crashed, hit its wall clock, or the machine restarted — none of which is a
// reason to retry unattended, because a run that died for an unknown reason
// will usually die the same way and eat a slot each time.
func (e Engine) resolveAbandoned(ctx context.Context, r state.Run) error {
	labels, err := e.Client.Labels(ctx, r.Ref)
	if err != nil {
		return fmt.Errorf("read labels: %w", err)
	}
	working := false
	for _, l := range labels {
		if l == e.Cfg.Labels.Working {
			working = true
		}
	}
	if !working {
		return nil
	}

	if limited := e.hitUsageLimit(r); limited {
		until := e.now().Add(e.Cfg.Limits.RateLimitBackoff)
		e.logf("%s: agent reported a usage limit; pausing dispatch until %s", r.ID(), until.Format(time.RFC3339))
		if err := e.Store.SetRateLimited(until); err != nil {
			return err
		}
		// Not the issue's fault and not a code problem: put it back in the
		// queue rather than spending the operator's remaining capacity.
		return e.Client.EditLabels(ctx, r.Ref, []string{e.Cfg.Labels.Queued}, []string{e.Cfg.Labels.Working})
	}

	body := fmt.Sprintf("The unattended run exited without finishing.\n\n- Transcript: `%s`\n\nComment here, or re-apply `%s`, to try again.",
		r.Log, e.Cfg.Labels.Queued)
	if err := e.Client.Comment(ctx, r.Ref, body); err != nil {
		e.logf("%s: could not comment: %v", r.ID(), err)
	}
	return e.Client.EditLabels(ctx, r.Ref, []string{e.Cfg.Labels.Blocked}, []string{e.Cfg.Labels.Working})
}

func (e Engine) hitUsageLimit(r state.Run) bool {
	if r.Log == "" {
		return false
	}
	raw, err := os.ReadFile(r.Log)
	if err != nil {
		return false
	}
	return runner.HitUsageLimit(string(raw))
}

// Start claims the work and begins a run.
//
// The claim happens before the spawn. If the spawn then fails the issue sits
// claimed with no record, and the next tick's Reap resolves it — the opposite
// order would let two ticks start two runs on one issue.
func (e Engine) Start(ctx context.Context, s Start) error {
	if err := e.Client.EditLabels(ctx, s.Ref, []string{e.Cfg.Labels.Working}, []string{e.Cfg.Labels.Queued}); err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	// Record where the thread stands now, so a comment posted before the run
	// started is not replayed as new when it ends.
	if latest, err := e.Client.LatestCommentBy(ctx, forge.TargetIssue, s.Ref, e.Cfg.User); err == nil {
		_ = e.Store.MarkSeen(issueKey(s.Ref), latest)
	}

	run := state.Run{Kind: s.Kind, Ref: s.Ref, Slot: s.Slot, Started: e.now()}
	run.Log = filepath.Join(e.Store.LogDir(), fmt.Sprintf("%s-%s.log", run.ID(), e.now().UTC().Format("20060102T150405")))

	if s.Kind.HoldsSlot() {
		var err error
		if run.Branch, run.Worktree, err = e.prepare(ctx, s, run.Log); err != nil {
			return err
		}
	}

	timeout := e.Cfg.Limits.BuildTimeout
	if s.Kind == state.KindPlan {
		timeout = e.Cfg.Limits.PlanTimeout
	}
	dir := run.Worktree
	if dir == "" {
		dir = e.Cfg.Paths.Root
	}

	// The run outlives the tick that started it. context.WithoutCancel keeps
	// deadlines and values while dropping cancellation, so a supervisor that
	// exits — `claudeflow once` returning, or serve taking a SIGTERM — does not
	// take its children with it.
	//
	// Without this, `once` killed the agent microseconds after spawning it: the
	// signal context is cancelled by main's deferred stop() as the command
	// returns, and the run died having written nothing at all. A run is bounded
	// by its own Timeout, not by how long the supervisor happens to live.
	args, err := e.agentArgs(s)
	if err != nil {
		return err
	}

	proc, err := runner.Start(context.WithoutCancel(ctx), runner.Spawn{
		Command: e.Cfg.Agent.Command,
		Args:    args,
		Dir:     dir,
		Env:     e.runEnv(ctx, s, run),
		Timeout: timeout,
		LogPath: run.Log,
	})
	if err != nil {
		return fmt.Errorf("start agent: %w", err)
	}
	run.PID = proc.PID()

	if err := e.Store.SaveRun(run); err != nil {
		return err
	}
	e.logf("%s: started (pid %d, slot %d, log %s)", run.ID(), run.PID, run.Slot, run.Log)
	return nil
}

// prepare creates the worktree and brings the environment up.
func (e Engine) prepare(ctx context.Context, s Start, logPath string) (branch, worktree string, err error) {
	branch = e.Cfg.Branches.Prefix + string(s.Kind) + "-" + strconv.Itoa(s.Ref)
	if s.Kind == state.KindReview {
		// One stable branch, not one per pull request number: the standing
		// pull request's number changes every time one is merged, and keying
		// off it strands a worktree and a branch on every rollover.
		branch = e.Cfg.Branches.Prefix + "review"
	}
	name := fmt.Sprintf("%s-%d-slot%d", s.Kind, s.Ref, s.Slot)
	worktree = filepath.Join(e.Cfg.Paths.Worktrees, name)

	if err := e.Repo.Fetch(ctx); err != nil {
		e.logf("fetch before worktree: %v", err)
	}
	if _, statErr := os.Stat(worktree); statErr != nil {
		if err := e.Repo.AddWorktree(ctx, worktree, branch, "origin/"+e.Cfg.Branches.Integration); err != nil {
			return "", "", fmt.Errorf("create worktree: %w", err)
		}
	}
	// Install runs before the stack comes up: a Compose file that bind-mounts
	// the worktree needs its dependencies already in place.
	if e.Cfg.Hooks.Install != "" {
		e.logf("%s-%d: running the install hook", s.Kind, s.Ref)
		if err := e.hookLogged(ctx, e.Cfg.Hooks.Install, s.Slot, worktree, s.Ref, branch, logPath); err != nil {
			return "", "", fmt.Errorf("install hook: %w", err)
		}
	}

	st := e.stack(s.Slot)
	st.Env = e.stackEnv(s.Slot, worktree, branch)

	// Take down whatever the slot's project is holding first. A previous run
	// that was killed rather than reaped leaves containers behind, and Up
	// against them starts nothing.
	if err := st.Down(ctx); err != nil {
		e.logf("%s-%d: pre-emptive teardown: %v", s.Kind, s.Ref, err)
	}

	e.logf("%s-%d: bringing the stack up on slot %d (project %s)", s.Kind, s.Ref, s.Slot, st.Project)
	if err := st.Up(ctx); err != nil {
		return "", "", fmt.Errorf("compose up: %w", err)
	}
	if err := st.WaitReady(ctx, e.Cfg.Compose.ReadyTimeout, 2*time.Second); err != nil {
		// Leave nothing half-up holding the slot.
		if derr := st.Down(ctx); derr != nil {
			e.logf("%s-%d: teardown after a failed start: %v", s.Kind, s.Ref, derr)
		}
		return "", "", fmt.Errorf("stack did not become ready: %w", err)
	}
	return branch, worktree, nil
}

// serviceURLs asks Compose which host port each exposed service landed on.
//
// The Compose file publishes ephemeral ports, so this is the only way to know
// them — and the reason nothing in this system does port arithmetic.
func (e Engine) serviceURLs(ctx context.Context, n int) map[string]string {
	out := map[string]string{}
	if len(e.Cfg.Compose.Expose) == 0 {
		return out
	}
	st := e.stack(n)
	for service, port := range e.Cfg.Compose.Expose {
		addr, err := st.Port(ctx, service, port)
		if err != nil || addr == "" {
			e.logf("could not resolve the published port for %s: %v", service, err)
			continue
		}
		// Compose reports "0.0.0.0:49153"; the host half is not dialable from
		// elsewhere, so keep the port and substitute the configured host.
		hostPort := addr
		if i := strings.LastIndex(addr, ":"); i >= 0 {
			hostPort = addr[i+1:]
		}
		key := "CLAUDEFLOW_URL_" + strings.ToUpper(strings.ReplaceAll(service, "-", "_"))
		out[key] = fmt.Sprintf("http://%s:%s", e.Cfg.Compose.URLHost(), hostPort)
	}
	return out
}

func (e Engine) hookLogged(ctx context.Context, cmd string, slot int, worktree string, ref int, branch, logPath string) error {
	dir := worktree
	if dir == "" {
		dir = e.Cfg.Paths.Root
	}
	r := runner.Runner{
		Dir: dir,
		Env: runner.Env{
			"CLAUDEFLOW_SLOT":     strconv.Itoa(slot),
			"CLAUDEFLOW_WORKTREE": worktree,
			"CLAUDEFLOW_BRANCH":   branch,
			"CLAUDEFLOW_ISSUE":    strconv.Itoa(ref),
		},
		Timeout: e.Cfg.Limits.BuildTimeout,
		LogPath: logPath,
	}
	res, err := r.Exec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr)
	}
	return nil
}

// agentArgs builds the agent invocation.
//
// The run's instructions go in as --append-system-prompt rather than as a skill
// the agent is asked to look up. Three reasons, and the first is the one that
// bit: a planning run has no worktree, so there is nowhere to install a skill
// file, and it would have been told to invoke something that does not exist.
// Second, installing skills means writing into the project's checkout, which
// shows up in its git status. Third, the embedded copy is then exactly what
// runs, so a skill cannot drift from the binary that depends on its wording.
func (e Engine) agentArgs(s Start) ([]string, error) {
	name := map[state.Kind]skills.Name{
		state.KindBuild:  skills.Issue,
		state.KindReview: skills.Review,
		state.KindPlan:   skills.Planning,
	}[s.Kind]

	instructions, err := skills.Read(name, e.Cfg.Agent.SkillsDir)
	if err != nil {
		return nil, err
	}

	subject := fmt.Sprintf("GitHub issue #%d", s.Ref)
	if s.Kind == state.KindReview {
		subject = fmt.Sprintf("the review comments on pull request #%d", s.Ref)
	}
	prompt := fmt.Sprintf("Work %s end to end, following the run instructions in your system prompt exactly. They are the complete specification for this run.", subject)

	args := []string{
		"-p", prompt,
		"--append-system-prompt", string(instructions),
		"--permission-mode", "bypassPermissions",
		"--output-format", "text",
	}
	if e.Cfg.Agent.Model != "" {
		args = append(args, "--model", e.Cfg.Agent.Model)
	}
	return append(args, e.Cfg.Agent.ExtraArgs...), nil
}

func (e Engine) runEnv(ctx context.Context, s Start, r state.Run) runner.Env {
	l := e.Cfg.Labels
	env := runner.Env{
		"CLAUDEFLOW_ISSUE":          strconv.Itoa(s.Ref),
		"CLAUDEFLOW_PR":             strconv.Itoa(s.Ref),
		"CLAUDEFLOW_REPO":           e.Cfg.Repo,
		"CLAUDEFLOW_USER":           e.Cfg.User,
		"CLAUDEFLOW_SLOT":           strconv.Itoa(s.Slot),
		"CLAUDEFLOW_BRANCH":         r.Branch,
		"CLAUDEFLOW_WORKTREE":       r.Worktree,
		"CLAUDEFLOW_BASE":           e.Cfg.Branches.Base,
		"CLAUDEFLOW_INTEGRATION":    e.Cfg.Branches.Integration,
		"CLAUDEFLOW_VERIFY":         e.Cfg.Hooks.Verify,
		"CLAUDEFLOW_LABEL_QUEUED":   l.Queued,
		"CLAUDEFLOW_LABEL_WORKING":  l.Working,
		"CLAUDEFLOW_LABEL_LANDED":   l.Landed,
		"CLAUDEFLOW_LABEL_QUESTION": l.Question,
		"CLAUDEFLOW_LABEL_BLOCKED":  l.Blocked,
		"CLAUDEFLOW_LABEL_PLANNING": l.Planning,
	}
	if s.Kind.HoldsSlot() {
		for k, v := range e.serviceURLs(ctx, s.Slot) {
			env[k] = v
		}
	}
	return env
}
