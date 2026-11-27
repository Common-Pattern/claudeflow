// Package data prepares and publishes the directory of data every run is given.
//
// A project's prepare command fills an empty staging directory. Only when it
// exits zero is that directory renamed into place as a new generation and the
// `current` link swung to it, both atomic on one filesystem, so a reader sees
// either the old generation or the new one and never a partial write. A
// generation is never changed after it is published: a run resolves `current`
// once, when it starts, and keeps that path for its whole life, so a refresh
// cannot move data under a run that is reading it.
//
// Layout under Dir:
//
//	current               -> generations/<stamp>
//	generations/<stamp>/  one per successful prepare
//	staging-<stamp>/      a prepare in progress, removed if it fails
//	empty/                what runs are given before anything is prepared
//	prepare.lock          held for the length of a prepare
//	prepare.log           the most recent prepare's output
//	prepare.took          how long the most recent successful prepare took
package data

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/runner"
)

// stampLayout names generations so that they sort by age.
const stampLayout = "20060102T150405Z"

// Store is one project's data directory.
type Store struct {
	Dir string
	Now func() time.Time
}

func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s Store) currentLink() string    { return filepath.Join(s.Dir, "current") }
func (s Store) generationsDir() string { return filepath.Join(s.Dir, "generations") }

// LogPath is where the most recent prepare's output is kept.
func (s Store) LogPath() string { return filepath.Join(s.Dir, "prepare.log") }

// Generation is one published result of a prepare.
type Generation struct {
	Path     string
	Prepared time.Time
}

// Current is the generation runs are given now, if one has been published.
func (s Store) Current() (Generation, bool) {
	target, err := os.Readlink(s.currentLink())
	if err != nil {
		return Generation{}, false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(s.Dir, target)
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		return Generation{}, false
	}
	at, err := time.Parse(stampLayout, filepath.Base(target))
	if err != nil {
		return Generation{}, false
	}
	return Generation{Path: target, Prepared: at}, true
}

// ForRun is the directory a run starting now should be given: the current
// generation, or an empty directory when nothing has been prepared yet, so a
// Compose file that bind-mounts it still starts.
func (s Store) ForRun() (string, error) {
	if g, ok := s.Current(); ok {
		return g.Path, nil
	}
	empty := filepath.Join(s.Dir, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		return "", fmt.Errorf("prepare the empty data directory: %w", err)
	}
	return empty, nil
}

// Due reports whether a prepare should start now for the data never to be
// older than maxAge: there is no current generation, or it is within the
// lead of expiring.
//
// A generation's age counts from when its prepare started, which is no later
// than when its data was taken. The lead is twice the last prepare's
// duration, so a slower one than last time still finishes in time; without a
// record of one, it is timeout, the longest a prepare may take. Either way it
// is capped at half of maxAge, so a slow prepare is not run continuously.
func (s Store) Due(maxAge, timeout time.Duration) bool {
	g, ok := s.Current()
	if !ok {
		return true
	}
	return s.now().Sub(g.Prepared) >= maxAge-s.lead(maxAge, timeout)
}

func (s Store) lead(maxAge, timeout time.Duration) time.Duration {
	lead := timeout
	if raw, err := os.ReadFile(s.tookPath()); err == nil {
		if took, err := time.ParseDuration(strings.TrimSpace(string(raw))); err == nil {
			lead = 2 * took
		}
	}
	return min(lead, maxAge/2)
}

func (s Store) tookPath() string { return filepath.Join(s.Dir, "prepare.took") }

// Prepare runs command in dir to produce a new generation and publishes it.
// It holds a lock for its length, so the supervisor and a manual prepare do
// not run at once; a second caller waits for the first.
func (s Store) Prepare(ctx context.Context, command, dir string, env map[string]string, timeout time.Duration) (Generation, error) {
	if err := os.MkdirAll(s.generationsDir(), 0o755); err != nil {
		return Generation{}, err
	}
	// The directory holds whatever the project prepared — typically derived
	// from production — so only the operator may list it. Generations inside
	// are opened up below so that a container's own users can read them.
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		return Generation{}, err
	}
	unlock, err := lock(filepath.Join(s.Dir, "prepare.lock"))
	if err != nil {
		return Generation{}, err
	}
	defer unlock()

	started := s.now()
	stamp := started.Format(stampLayout)
	staging := filepath.Join(s.Dir, "staging-"+stamp)
	final := filepath.Join(s.generationsDir(), stamp)
	if _, err := os.Stat(final); err == nil {
		return Generation{}, fmt.Errorf("a generation stamped %s already exists; try again in a second", stamp)
	}
	if err := os.RemoveAll(staging); err != nil {
		return Generation{}, err
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return Generation{}, err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()

	full := runner.Env{"CLAUDEFLOW_DATA_OUT": staging}
	for k, v := range env {
		full[k] = v
	}
	_ = os.Remove(s.LogPath())
	r := runner.Runner{Dir: dir, Env: full, Timeout: timeout, LogPath: s.LogPath()}
	res, err := r.Exec(ctx, command)
	if err != nil {
		return Generation{}, fmt.Errorf("data.prepare: %w (see %s)", err, s.LogPath())
	}
	if res.ExitCode != 0 {
		return Generation{}, fmt.Errorf("data.prepare exited %d (see %s)", res.ExitCode, s.LogPath())
	}

	if err := openUp(staging); err != nil {
		return Generation{}, fmt.Errorf("make the prepared data readable: %w", err)
	}
	if err := os.Rename(staging, final); err != nil {
		return Generation{}, fmt.Errorf("publish the prepared data: %w", err)
	}
	published = true
	if err := s.point(final); err != nil {
		return Generation{}, err
	}
	_ = os.WriteFile(s.tookPath(), []byte(s.now().Sub(started).Round(time.Second).String()+"\n"), 0o600)
	at, _ := time.Parse(stampLayout, stamp)
	return Generation{Path: final, Prepared: at}, nil
}

// point swings `current` to target atomically: a new link beside it, renamed
// over it.
func (s Store) point(target string) error {
	rel, err := filepath.Rel(s.Dir, target)
	if err != nil {
		rel = target
	}
	tmp := s.currentLink() + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(rel, tmp); err != nil {
		return fmt.Errorf("link the new generation: %w", err)
	}
	if err := os.Rename(tmp, s.currentLink()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("make the new generation current: %w", err)
	}
	return nil
}

// Prune removes generations that are neither current, the one before it, nor
// named in inUse, and staging directories a killed prepare left behind. The
// previous generation is kept too, because a run may have resolved it just
// before the swap and not yet been recorded.
func (s Store) Prune(inUse []string) ([]string, error) {
	keep := map[string]bool{}
	for _, p := range inUse {
		if p != "" {
			keep[filepath.Clean(p)] = true
		}
	}
	entries, err := os.ReadDir(s.generationsDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	cur, ok := s.Current()
	if ok {
		keep[filepath.Clean(cur.Path)] = true
		for i, n := range names {
			if filepath.Join(s.generationsDir(), n) == cur.Path && i > 0 {
				keep[filepath.Join(s.generationsDir(), names[i-1])] = true
			}
		}
	}
	var removed []string
	for _, n := range names {
		p := filepath.Join(s.generationsDir(), n)
		if keep[p] {
			continue
		}
		if !ok {
			// No current generation is a broken link, not an empty store;
			// removing everything on that reading would be unrecoverable.
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return removed, err
		}
		removed = append(removed, p)
	}
	s.pruneStaging()
	return removed, nil
}

// pruneStaging removes staging directories, unless a prepare holds the lock.
func (s Store) pruneStaging() {
	f, err := os.OpenFile(filepath.Join(s.Dir, "prepare.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	entries, _ := os.ReadDir(s.Dir)
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "staging-") {
			_ = os.RemoveAll(filepath.Join(s.Dir, e.Name()))
		}
	}
}

// openUp makes everything under dir readable, and its directories
// traversable, by any user. A container's processes are not the operator: the
// database server that restores a dump runs as its own user, in a user
// namespace of its own. Access to the generations is gated by Dir's own mode.
func openUp(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		mode := fi.Mode().Perm() | 0o444
		if d.IsDir() {
			mode |= 0o111
		}
		return os.Chmod(p, mode)
	})
}

func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
