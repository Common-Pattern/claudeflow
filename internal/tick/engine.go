package tick

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

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

func (e Engine) allocator() slot.Allocator {
	a := slot.Allocator{Min: e.Cfg.Slots.Min, Max: e.Cfg.Slots.Max}
	if e.Cfg.Slots.BusyCheck != "" {
		a.Busy = func(ctx context.Context, n int) (bool, error) {
			r := runner.Runner{
				Dir:     e.Cfg.Paths.Root,
				Env:     runner.Env{"CLAUDEFLOW_SLOT": strconv.Itoa(n)},
				Timeout: 30 * time.Second,
			}
			res, err := r.Exec(ctx, e.Cfg.Slots.BusyCheck)
			if err != nil {
				return false, err
			}
			// Exit 0 means busy, following the convention of test(1).
			return res.ExitCode == 0, nil
		}
	}
	return a
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
		if r.Kind.HoldsSlot() && e.Cfg.Hooks.EnvDown != "" {
			if err := e.hook(ctx, e.Cfg.Hooks.EnvDown, r.Slot, r.Worktree, r.Ref, r.Branch); err != nil {
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

	proc, err := runner.Start(ctx, runner.Spawn{
		Command: e.Cfg.Agent.Command,
		Args:    e.agentArgs(s),
		Dir:     dir,
		Env:     e.runEnv(s, run),
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
	if err := skills.Install(filepath.Join(worktree, ".claude", "skills"), e.Cfg.Agent.SkillsDir); err != nil {
		return "", "", fmt.Errorf("install skills: %w", err)
	}
	for _, h := range []struct{ name, cmd string }{
		{"install", e.Cfg.Hooks.Install},
		{"envUp", e.Cfg.Hooks.EnvUp},
	} {
		if h.cmd == "" {
			continue
		}
		e.logf("%s-%d: running the %s hook", s.Kind, s.Ref, h.name)
		if err := e.hookLogged(ctx, h.cmd, s.Slot, worktree, s.Ref, branch, logPath); err != nil {
			return "", "", fmt.Errorf("%s hook: %w", h.name, err)
		}
	}
	return branch, worktree, nil
}

func (e Engine) hook(ctx context.Context, cmd string, slot int, worktree string, ref int, branch string) error {
	return e.hookLogged(ctx, cmd, slot, worktree, ref, branch, "")
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

func (e Engine) agentArgs(s Start) []string {
	skill := map[state.Kind]string{
		state.KindBuild:  "claudeflow-issue",
		state.KindReview: "claudeflow-review",
		state.KindPlan:   "claudeflow-planning",
	}[s.Kind]

	subject := fmt.Sprintf("GitHub issue #%d", s.Ref)
	if s.Kind == state.KindReview {
		subject = fmt.Sprintf("the review comments on pull request #%d", s.Ref)
	}
	prompt := fmt.Sprintf("Work %s end to end. Invoke the `%s` skill now and follow it exactly — it is the complete specification for this run.", subject, skill)

	args := []string{"-p", prompt, "--permission-mode", "bypassPermissions", "--output-format", "text"}
	if e.Cfg.Agent.Model != "" {
		args = append(args, "--model", e.Cfg.Agent.Model)
	}
	return append(args, e.Cfg.Agent.ExtraArgs...)
}

func (e Engine) runEnv(s Start, r state.Run) runner.Env {
	l := e.Cfg.Labels
	return runner.Env{
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
}
