// Package housekeep reclaims what finished runs leave behind.
//
// claudeflow keeps a worktree after landing, because fixing CI needs it.
// Nothing then removes it, and a worktree is not cheap: its own dependency tree
// is routinely over a gigabyte. Left alone this fills a disk one landed issue
// at a time.
//
// Every removal is gated on the branch being merged into the integration
// branch. That is the whole safety argument: a merged branch's worktree holds
// nothing that is not already on the integration branch.
package housekeep

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/git"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

// TeardownFunc takes a slot's environment down.
//
// Reaping normally does this when a run ends. It is repeated here because a
// supervisor that was killed rather than shut down never reaped, and the
// stack it left behind would hold the slot indefinitely.
type TeardownFunc func(ctx context.Context, slot int) error

// AliveFunc reports whether a recorded run is still going.
type AliveFunc func(pid int) bool

// Keeper reclaims worktrees, branches, run records and logs.
type Keeper struct {
	Cfg   config.Config
	Repo  *git.Repo
	Store *state.Store
	// Teardown removes a slot's environment before its worktree is removed.
	// Optional; without it environments are left alone.
	Teardown TeardownFunc
	// Alive decides whether a run record still has a process behind it.
	Alive AliveFunc
	// LogRetention is how long transcripts are kept. Zero means forever.
	LogRetention time.Duration
	// DryRun reports what would happen without doing any of it.
	DryRun bool
	Log    func(format string, args ...any)
	Now    func() time.Time
}

func (k Keeper) logf(format string, args ...any) {
	if k.Log != nil {
		k.Log(format, args...)
	}
}

func (k Keeper) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

// Report says what a pass did, or would do.
type Report struct {
	RemovedWorktrees []string
	RemovedBranches  []string
	ClearedRuns      []string
	DeletedLogs      int
	// OrphanSlots are slots that look occupied with no run behind them. They
	// are reported, never acted on.
	OrphanSlots []int
}

// Run performs one housekeeping pass.
func (k Keeper) Run(ctx context.Context) (Report, error) {
	var rep Report

	if err := k.Repo.Fetch(ctx); err != nil {
		k.logf("fetch failed; working from the last known refs: %v", err)
	}

	live, err := k.liveWorktrees()
	if err != nil {
		return rep, err
	}

	worktrees, err := k.Repo.Worktrees(ctx)
	if err != nil {
		return rep, fmt.Errorf("list worktrees: %w", err)
	}
	integration := "origin/" + k.Cfg.Branches.Integration
	reclaimable, err := git.ReclaimableWorktrees(ctx, k.Repo, worktrees, k.Cfg.Branches.Prefix, integration)
	if err != nil {
		return rep, fmt.Errorf("decide reclaimable worktrees: %w", err)
	}

	for _, wt := range reclaimable {
		// A live run owns its worktree whatever the branch looks like. A branch
		// can be merged while a run is still pushing a follow-up to it.
		if slot, busy := live[wt.Path]; busy {
			k.logf("keeping %s — a run is live in it on slot %d", wt.Path, slot)
			continue
		}
		if err := k.reclaim(ctx, wt); err != nil {
			k.logf("could not reclaim %s: %v", wt.Path, err)
			continue
		}
		rep.RemovedWorktrees = append(rep.RemovedWorktrees, wt.Path)
		rep.RemovedBranches = append(rep.RemovedBranches, wt.Branch)
	}

	if !k.DryRun {
		if err := k.Repo.PruneWorktrees(ctx); err != nil {
			k.logf("prune failed: %v", err)
		}
	}

	cleared, err := k.clearDeadRuns()
	if err != nil {
		return rep, err
	}
	rep.ClearedRuns = cleared

	deleted, err := k.deleteOldLogs()
	if err != nil {
		return rep, err
	}
	rep.DeletedLogs = deleted

	return rep, nil
}

// reclaim tears the environment down, then removes the worktree, then deletes
// the branch. The order is load-bearing: deleting a branch that is checked out
// in a worktree fails, and tearing down after removal would have lost the
// project's own teardown script along with the worktree.
func (k Keeper) reclaim(ctx context.Context, wt git.Worktree) error {
	k.logf("reclaiming %s (%s — merged into the integration branch)", wt.Path, wt.Branch)
	if k.DryRun {
		return nil
	}

	if k.Teardown != nil {
		if slot, ok := slotOfWorktree(wt.Path); ok {
			if err := k.Teardown(ctx, slot); err != nil {
				k.logf("teardown for slot %d reported a problem: %v", slot, err)
			}
		}
	}
	if err := k.Repo.RemoveWorktree(ctx, wt.Path); err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	if err := k.Repo.DeleteBranch(ctx, wt.Branch); err != nil {
		return fmt.Errorf("delete branch: %w", err)
	}
	return nil
}

// liveWorktrees maps a worktree path to the slot of the live run holding it.
func (k Keeper) liveWorktrees() (map[string]int, error) {
	runs, err := k.Store.Runs()
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, r := range runs {
		if r.Worktree == "" {
			continue
		}
		if k.Alive == nil || k.Alive(r.PID) {
			out[r.Worktree] = r.Slot
		}
	}
	return out, nil
}

func (k Keeper) clearDeadRuns() ([]string, error) {
	runs, err := k.Store.Runs()
	if err != nil {
		return nil, err
	}
	var cleared []string
	for _, r := range runs {
		if k.Alive != nil && k.Alive(r.PID) {
			continue
		}
		if k.Alive == nil {
			continue
		}
		k.logf("clearing stale run record %s", r.ID())
		if !k.DryRun {
			if err := k.Store.DeleteRun(r.ID()); err != nil {
				return nil, err
			}
		}
		cleared = append(cleared, r.ID())
	}
	return cleared, nil
}

func (k Keeper) deleteOldLogs() (int, error) {
	if k.LogRetention <= 0 {
		return 0, nil
	}
	dir := k.Store.LogDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("list logs: %w", err)
	}
	cutoff := k.now().Add(-k.LogRetention)
	deleted := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if !k.DryRun {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return deleted, fmt.Errorf("remove log %s: %w", e.Name(), err)
			}
		}
		deleted++
	}
	return deleted, nil
}

// slotOfWorktree recovers the slot a worktree was created for from its name.
// claudeflow names them "<kind>-<ref>-slot<N>" precisely so that teardown can
// still find the slot after the run record has gone.
func slotOfWorktree(path string) (int, bool) {
	base := filepath.Base(path)
	idx := strings.LastIndex(base, "-slot")
	if idx < 0 {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(base[idx+len("-slot"):], "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}
