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

// transcript renders a run's log for a comment: a link where transcripts are
// served, and the path otherwise.
//
// The path is kept in both cases. It is the only form that still means
// something once the server is off, the retention window has passed, or the
// reader is on the host rather than the network — and a link without it sends
// someone hunting for a file they are standing on.
func (e Engine) transcript(logPath string) string {
	if logPath == "" {
		return "_none_"
	}
	if url := e.Cfg.Transcripts.URL(logPath); url != "" {
		return fmt.Sprintf("%s (`%s` on the host)", url, logPath)
	}
	return fmt.Sprintf("`%s`", logPath)
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
	// Runs waiting on checks are advanced before anything new is dispatched:
	// landing finished work matters more than starting more of it, and a
	// landed run frees nothing until it is resolved.
	if err := e.advanceAwaitingCI(ctx); err != nil {
		e.logf("advancing checks: %v", err)
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
		// A wedged provider must fail one run rather than block the tick, and
		// with it every reap, dispatch and status until someone intervenes.
		Timeout: e.Cfg.Compose.ReadyTimeout,
	}
}

// stackEnv is what the Compose file may interpolate. The slot is the knob a
// project uses to offset published ports.
func (e Engine) stackEnv(n int, worktree, branch string) map[string]string {
	env := map[string]string{
		"CLAUDEFLOW_SLOT":     strconv.Itoa(n),
		"CLAUDEFLOW_WORKTREE": worktree,
		"CLAUDEFLOW_BRANCH":   branch,
		"CLAUDEFLOW_PROJECT":  e.Cfg.Compose.Project(n),
	}
	// The project's own additions last, so it can point the compose
	// implementation at an engine without a host-wide setting.
	for k, v := range e.Cfg.Compose.Env {
		env[k] = v
	}
	return env
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
		// A run waiting on checks has no process by design, but it still owns
		// its issue: counting only live pids would dispatch a second run on an
		// issue whose pull request is already open.
		if r.InPhase(state.PhaseAwaitingCI) || runner.AliveSince(r.PID, r.Started) {
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
		if r.InPhase(state.PhaseAwaitingCI) {
			// Not this function's business: no process, by design.
			continue
		}
		if runner.AliveSince(r.PID, r.Started) {
			continue
		}
		e.logf("%s: run finished", r.ID())

		// An agent that opened a pull request has done its part. The waiting
		// is claudeflow's from here, so the run is parked rather than resolved.
		if r.Branch != "" {
			if pr, err := e.Client.PRForBranch(ctx, r.Branch); err == nil && pr > 0 {
				if err := e.awaitCI(ctx, r, pr); err != nil {
					e.logf("%s: %v", r.ID(), err)
				}
				continue
			}
		}

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
	return e.releaseStrandedClaims(ctx)
}

// releaseStrandedClaims re-queues issues claimed by a run that never recorded
// itself.
//
// Start claims the label before spawning, so that two ticks cannot both start
// on one issue. A spawn that fails releases its own claim, but a supervisor
// that dies between the claim and the record — killed, restarted, the machine
// going down — leaves the claim behind with no run record. Reap walks run
// records, so it never sees these, and the issue sits in the working state
// forever: invisible to dispatch, and reported as in-flight by status.
func (e Engine) releaseStrandedClaims(ctx context.Context) error {
	claimed, err := e.Client.OpenIssues(ctx, e.Cfg.Labels.Working)
	if err != nil {
		return fmt.Errorf("list claimed issues: %w", err)
	}
	if len(claimed) == 0 {
		return nil
	}

	live := map[int]struct{}{}
	runs, err := e.Store.Runs()
	if err != nil {
		return err
	}
	for _, r := range runs {
		live[r.Ref] = struct{}{}
	}

	for _, issue := range claimed {
		if _, ok := live[issue.Number]; ok {
			continue
		}
		e.logf("issue #%d: claimed with no run behind it; re-queueing", issue.Number)
		if err := e.Client.EditLabels(ctx, issue.Number,
			nil, []string{e.Cfg.Labels.Working}); err != nil {
			e.logf("issue #%d: could not re-queue: %v", issue.Number, err)
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
		return e.Client.EditLabels(ctx, r.Ref, nil, []string{e.Cfg.Labels.Working})
	}

	body := fmt.Sprintf("The unattended run exited without finishing.\n\n- Transcript: %s\n\nComment here, or re-apply `%s`, to try again.",
		e.transcript(r.Log), e.Cfg.Labels.Queued)
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
// The claim happens before the spawn — the opposite order would let two ticks
// start two runs on one issue. A spawn that then fails resolves its own claim
// here, as blocked.
//
// It used to leave the claim for the next tick's stranded-claim sweep, which
// re-queued it. A failure that is not transient — a compose binary that is not
// there, a Compose file that does not parse — then failed identically on every
// tick: the working label went on and came off every two minutes, forever, and
// the reason was never written anywhere a reader of the issue would see it.
func (e Engine) Start(ctx context.Context, s Start) error {
	// Claiming ADDS the working label. The queued label stays: it says the
	// issue is the agent's, which does not stop being true while a run is on
	// it. Removing and restoring it on every transition churned the timeline
	// and told a reader nothing.
	if err := e.Client.EditLabels(ctx, s.Ref, []string{e.Cfg.Labels.Working}, e.Cfg.Labels.Resolution()); err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	if err := e.start(ctx, s); err != nil {
		if rerr := e.releaseFailedStart(ctx, s, err); rerr != nil {
			e.logf("%s #%d: could not release the claim: %v", s.Kind, s.Ref, rerr)
		}
		return err
	}
	return nil
}

// releaseFailedStart resolves the claim of a run that never began.
func (e Engine) releaseFailedStart(ctx context.Context, s Start, cause error) error {
	if s.Kind == state.KindReview {
		// The claim sits on a pull request, and a review run's outcome is not
		// labelled; the review marker is still owed, so it is retried.
		return e.Client.EditLabels(ctx, s.Ref, nil, []string{e.Cfg.Labels.Working})
	}
	body := fmt.Sprintf("The unattended run could not start:\n\n```\n%v\n```\n\nComment here, or re-apply `%s`, to try again.",
		cause, e.Cfg.Labels.Queued)
	if err := e.Client.Comment(ctx, s.Ref, body); err != nil {
		e.logf("%s #%d: could not comment: %v", s.Kind, s.Ref, err)
	}
	return e.Client.EditLabels(ctx, s.Ref, []string{e.Cfg.Labels.Blocked}, []string{e.Cfg.Labels.Working})
}

// start does everything after the claim: environment, spawn, record.
func (e Engine) start(ctx context.Context, s Start) error {
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

	// Started is what re-adoption checks the pid's kernel start time against,
	// so it is stamped at the fork. Stamped before prepare, it sat minutes early
	// behind the install hook and a cold stack, and a live run read as a
	// recycled pid and was reaped under its agent.
	run.Started = e.now()
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
		name := strings.ToUpper(strings.ReplaceAll(service, "-", "_"))
		// A URL is only useful for something that speaks HTTP. The port and
		// host are given separately so a project can build any address it
		// needs — a Postgres DSN, a Redis URL, anything.
		out["CLAUDEFLOW_PORT_"+name] = hostPort
		out["CLAUDEFLOW_HOST_"+name] = e.Cfg.Compose.URLHost()
		out["CLAUDEFLOW_URL_"+name] = fmt.Sprintf("http://%s:%s", e.Cfg.Compose.URLHost(), hostPort)
	}
	return out
}

// expandAgentEnv resolves the project's declared environment against the
// stack's addresses, so a value can name a service without knowing its port.
func expandAgentEnv(declared map[string]string, resolved map[string]string) map[string]string {
	out := make(map[string]string, len(declared))
	for k, v := range declared {
		out[k] = os.Expand(v, func(name string) string { return resolved[name] })
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
		resolved := e.serviceURLs(ctx, s.Slot)
		for k, v := range resolved {
			env[k] = v
		}
		for k, v := range expandAgentEnv(e.Cfg.Agent.Env, resolved) {
			env[k] = v
		}
	}
	return env
}
