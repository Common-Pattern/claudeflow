// Command claudeflow drives a coding agent from GitHub issue labels.
//
// It claims a labelled issue, creates a git worktree, brings up a
// project-defined environment, runs the agent in it, and lands the result on
// the integration branch. See the README for configuration.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/compose"
	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/doctor"
	"github.com/Common-Pattern/claudeflow/internal/forge"
	"github.com/Common-Pattern/claudeflow/internal/git"
	"github.com/Common-Pattern/claudeflow/internal/housekeep"
	"github.com/Common-Pattern/claudeflow/internal/land"
	"github.com/Common-Pattern/claudeflow/internal/runner"
	"github.com/Common-Pattern/claudeflow/internal/state"
	"github.com/Common-Pattern/claudeflow/internal/tick"
)

// Set by the linker at release time; see .goreleaser.yaml.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `claudeflow — drive a coding agent from GitHub issue labels

Usage:
  claudeflow [-c config] <command> [arguments]

Commands:
  serve          supervise runs continuously
  once           run a single tick and exit
  status         show runs, queues and flags
  pause          stop starting new runs; live runs continue
  resume         start dispatching again
  stop           pause, then stop every live run
  land <pr>      merge a green pull request and sync the checkout
  housekeep      reclaim landed worktrees now
  doctor         check dependencies, authentication and configuration
  labels         create or update the labels in the repository
  version        print version information

Flags:
  -c path        config file (default: claudeflow.yaml, then .claudeflow.yaml)
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "claudeflow:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("c", "", "config file")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		return errors.New("no command given")
	}
	cmd, rest := args[0], args[1:]

	if cmd == "version" {
		fmt.Printf("claudeflow %s (%s, built %s)\n", version, commit, date)
		return nil
	}

	// doctor runs before the config is required: "can this host run anything"
	// is a useful question when the config is the thing that is wrong.
	cfg, cfgErr := loadConfig(*cfgPath)
	if cmd == "doctor" {
		return runDoctor(cfg, cfgErr)
	}
	if cfgErr != nil {
		return cfgErr
	}
	err := error(nil)
	_ = err
	st, err := state.Open(cfg.Paths.State)
	if err != nil {
		return err
	}

	// Interrupts stop the supervisor, not the runs it started. A run is a
	// detached process with its own group and its own record on disk, so a
	// restarted supervisor re-adopts it rather than orphaning it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		return serve(ctx, cfg, st, rest)
	case "once":
		return engine(cfg, st).Once(ctx)
	case "status":
		return status(ctx, cfg, st)
	case "pause":
		if err := st.SetFlag(state.Paused, ""); err != nil {
			return err
		}
		fmt.Println("paused — live runs continue")
		return nil
	case "resume":
		if err := st.ClearFlag(state.Paused); err != nil {
			return err
		}
		if err := st.ClearFlag(state.RateLimited); err != nil {
			return err
		}
		fmt.Println("resumed")
		return nil
	case "stop":
		return stopAll(st)
	case "land":
		return landPR(ctx, cfg, st, rest)
	case "housekeep":
		return runHousekeep(ctx, cfg, st, rest)
	case "labels":
		return ensureLabels(ctx, cfg)
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// runDoctor reports on the host and exits non-zero if anything would stop a
// run, so it is usable as a precondition in a script.
func runDoctor(cfg config.Config, cfgErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	report := doctor.Run(ctx, doctor.Options{
		Cfg: cfg, HaveConfig: cfgErr == nil, CfgErr: cfgErr,
		Version: version,
		// Version lookups go through gh, which is already a dependency and
		// already holds the credentials. An offline host gets "could not
		// check" rather than a failure.
		Latest: doctor.GHLatest(doctor.ExecRunner),
	})
	fmt.Print(report)
	if report.Failed() {
		return errors.New("doctor found problems that would stop a run")
	}
	return nil
}

func loadConfig(path string) (config.Config, error) {
	if path != "" {
		return config.Load(path)
	}
	for _, candidate := range []string{"claudeflow.yaml", "claudeflow.yml", ".claudeflow.yaml"} {
		if _, err := os.Stat(candidate); err == nil {
			return config.Load(candidate)
		}
	}
	return config.Config{}, errors.New("no config file found; pass -c or add claudeflow.yaml")
}

func engine(cfg config.Config, st *state.Store) tick.Engine {
	return tick.Engine{
		Cfg:    cfg,
		Client: forge.NewGH(cfg.Repo),
		Store:  st,
		Repo:   git.New(cfg.Paths.Root),
		Log:    logf,
	}
}

// stackFor returns the Compose stack for a slot. Each slot is its own Compose
// project, so a teardown can only reach what that project owns.
func stackFor(cfg config.Config, slot int) compose.Compose {
	return compose.Compose{
		Bin:     cfg.Compose.Bin,
		File:    cfg.Compose.File,
		Project: cfg.Compose.Project(slot),
		Dir:     cfg.Paths.Root,
		Env:     map[string]string{"CLAUDEFLOW_SLOT": strconv.Itoa(slot)},
	}
}

func logf(format string, args ...any) {
	fmt.Printf("%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func serve(ctx context.Context, cfg config.Config, st *state.Store, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	interval := fs.Duration("interval", 2*time.Minute, "how often to tick")
	if err := fs.Parse(args); err != nil {
		return err
	}

	e := engine(cfg, st)
	logf("claudeflow %s supervising %s every %s", version, cfg.Repo, *interval)

	// Tick immediately rather than waiting out the first interval: a restart
	// should re-adopt and reap straight away, not minutes later.
	for {
		if err := e.Once(ctx); err != nil {
			logf("tick failed: %v", err)
		}
		select {
		case <-ctx.Done():
			logf("shutting down; live runs are left running")
			return nil
		case <-time.After(*interval):
		}
	}
}

func status(ctx context.Context, cfg config.Config, st *state.Store) error {
	now := time.Now()
	fmt.Printf("repo:    %s\n", cfg.Repo)
	fmt.Printf("branch:  %s → %s\n", cfg.Branches.Integration, cfg.Branches.Base)
	fmt.Printf("limits:  %d builds, %d planning · slots %d–%d\n",
		cfg.Limits.MaxBuilds, cfg.Limits.MaxPlanning, cfg.Slots.Min, cfg.Slots.Max)

	switch err := tick.DispatchBlocked(st, now); {
	case errors.Is(err, tick.ErrPaused):
		fmt.Println("dispatch: PAUSED")
	case errors.Is(err, tick.ErrRateLimited):
		until, _ := st.RateLimitedUntil(now)
		fmt.Printf("dispatch: backing off until %s\n", until.Format(time.RFC3339))
	default:
		fmt.Println("dispatch: active")
	}

	runs, err := st.Runs()
	if err != nil {
		return err
	}
	fmt.Println()
	if len(runs) == 0 {
		fmt.Println("no runs")
	} else {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "KIND\tREF\tSLOT\tPID\tSTATE\tAGE")
		for _, r := range runs {
			label := "exited"
			if runner.AliveSince(r.PID, r.Started) {
				label = "live"
			}
			fmt.Fprintf(w, "%s\t#%d\t%d\t%d\t%s\t%dm\n",
				r.Kind, r.Ref, r.Slot, r.PID, label, int(now.Sub(r.Started).Minutes()))
		}
		w.Flush()
	}

	cl := forge.NewGH(cfg.Repo)
	l := cfg.Labels
	for _, group := range []struct{ title, label string }{
		{"waiting on you", l.Question},
		{"in planning", l.Planning},
		{"queued", l.Queued},
		{"landed", l.Landed},
		{"blocked", l.Blocked},
	} {
		issues, err := cl.OpenIssues(ctx, group.label)
		if err != nil {
			continue
		}
		fmt.Printf("\n%s:\n", group.title)
		if len(issues) == 0 {
			fmt.Println("  (none)")
		}
		for _, i := range issues {
			fmt.Printf("  #%d %s\n", i.Number, i.Title)
		}
	}
	return nil
}

func stopAll(st *state.Store) error {
	if err := st.SetFlag(state.Paused, ""); err != nil {
		return err
	}
	runs, err := st.Runs()
	if err != nil {
		return err
	}
	for _, r := range runs {
		if !runner.AliveSince(r.PID, r.Started) {
			continue
		}
		fmt.Printf("stopping %s (pid %d)\n", r.ID(), r.PID)
		// Signal the whole group: the agent spawns children, and leaving them
		// behind holds the slot forever.
		if err := syscall.Kill(-r.PID, syscall.SIGTERM); err != nil {
			fmt.Fprintf(os.Stderr, "  could not signal: %v\n", err)
		}
	}
	fmt.Println("stopped. 'claudeflow resume' to dispatch again.")
	return nil
}

func landPR(ctx context.Context, cfg config.Config, st *state.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("land needs a pull request number")
	}
	pr, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("pull request number: %w", err)
	}
	worktree, _ := os.Getwd()
	if len(args) > 1 {
		worktree = args[1]
	}

	// Serialise: two runs landing at once each pass CI against a base the
	// other is about to move, and the checkout ends up fast-forwarded to a
	// tree neither of them tested.
	unlock, err := lockFile(filepath.Join(cfg.Paths.State, "land.lock"))
	if err != nil {
		return err
	}
	defer unlock()

	cl := forge.NewGH(cfg.Repo)
	l := land.Lander{
		Cfg: cfg, Client: cl, Repo: git.New(cfg.Paths.Root), Log: logf,
		RunHook: func(ctx context.Context, cmd string) error {
			r := runner.Runner{Dir: cfg.Paths.Root, Timeout: cfg.Limits.BuildTimeout}
			_, err := r.Exec(ctx, cmd)
			return err
		},
		Wait: func(ctx context.Context, pr int) ([]forge.Check, error) {
			return waitChecks(ctx, cl, pr)
		},
	}
	res, err := l.Land(ctx, pr, worktree)
	if err != nil {
		return err
	}
	fmt.Printf("landed #%d at %s\n", res.PR, res.MergedSHA)
	_ = st
	return nil
}

// waitChecks polls until nothing is pending. gh's own --watch is not used here
// because its exit status conflates "a check failed" with "checks are still
// arriving", and this caller needs the final set either way.
func waitChecks(ctx context.Context, cl forge.Client, pr int) ([]forge.Check, error) {
	for {
		checks, err := cl.Checks(ctx, pr)
		if err != nil {
			return nil, err
		}
		if len(checks) > 0 && len(forge.Pending(checks)) == 0 {
			return checks, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

func runHousekeep(ctx context.Context, cfg config.Config, st *state.Store, args []string) error {
	fs := flag.NewFlagSet("housekeep", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "report without changing anything")
	retain := fs.Duration("retain-logs", 14*24*time.Hour, "how long to keep transcripts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	k := housekeep.Keeper{
		Cfg: cfg, Repo: git.New(cfg.Paths.Root), Store: st,
		Alive:        runner.Alive,
		LogRetention: *retain,
		DryRun:       *dry,
		Log:          logf,
		Teardown: func(ctx context.Context, slot int) error {
			return stackFor(cfg, slot).Down(ctx)
		},
	}
	rep, err := k.Run(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("reclaimed %d worktree(s), cleared %d run record(s), deleted %d log(s)\n",
		len(rep.RemovedWorktrees), len(rep.ClearedRuns), rep.DeletedLogs)
	return nil
}

func ensureLabels(ctx context.Context, cfg config.Config) error {
	cl := forge.NewGH(cfg.Repo)
	l := cfg.Labels
	for _, spec := range []struct{ name, color, desc string }{
		{l.Queued, "1f6feb", "Hand this issue to the agent"},
		{l.Working, "d4a72c", "The agent is working on this"},
		{l.Landed, "1a7f37", "Landed on the integration branch; awaiting your check"},
		{l.Question, "8250df", "The agent asked you something; reply to unblock it"},
		{l.Blocked, "cf222e", "The agent stopped; something broke. See its comment"},
		{l.Planning, "0e8a16", "With the queued label: talk it through, write no code"},
	} {
		if err := cl.EnsureLabel(ctx, spec.name, spec.color, spec.desc); err != nil {
			return err
		}
		fmt.Printf("  %s\n", spec.name)
	}
	return nil
}

// lockFile takes an exclusive advisory lock, blocking until it is available.
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
