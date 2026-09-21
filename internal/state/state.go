// Package state persists what claudeflow knows between ticks.
//
// There is no database. A tick reconstructs its whole picture from GitHub
// labels plus the few files this package owns, so a tick that crashes loses
// nothing: the next one reads the same world back.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kind distinguishes the three sorts of run.
type Kind string

const (
	// KindBuild implements an issue and lands it.
	KindBuild Kind = "build"
	// KindReview addresses review comments on the standing pull request.
	KindReview Kind = "review"
	// KindPlan is a conversation: no worktree, no slot, no code.
	KindPlan Kind = "plan"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool { return k == KindBuild || k == KindReview || k == KindPlan }

// HoldsSlot reports whether a run of this kind occupies an environment slot.
// Planning runs read the checkout in place and write nothing, so they hold
// none, which is why they are budgeted separately.
func (k Kind) HoldsSlot() bool { return k != KindPlan }

// Run records one live run.
//
// The record outlives the supervising process deliberately. claudeflow serve
// writes it before starting the child and removes it after reaping, so a
// daemon that is restarted — or killed — can read back exactly what was in
// flight, re-adopt what is still alive by PID, and resolve what is not.
type Run struct {
	Kind Kind `json:"kind"`
	Ref  int  `json:"ref"`
	Slot int  `json:"slot"`
	// PID is the agent process. Zero means it was recorded but never started.
	PID      int       `json:"pid"`
	Branch   string    `json:"branch,omitempty"`
	Worktree string    `json:"worktree,omitempty"`
	Log      string    `json:"log"`
	Started  time.Time `json:"started"`
}

// ID is the stable key for a run, and the basename of its record.
func (r Run) ID() string { return string(r.Kind) + "-" + strconv.Itoa(r.Ref) }

// Store is a directory holding run records, seen-markers and flags.
type Store struct{ dir string }

// Open prepares dir and returns a Store over it.
func Open(dir string) (*Store, error) {
	for _, sub := range []string{"runs", "seen", "logs"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("prepare state dir: %w", err)
		}
	}
	return &Store{dir: dir}, nil
}

// Dir returns the store's root.
func (s *Store) Dir() string { return s.dir }

// LogDir returns where run transcripts are written.
func (s *Store) LogDir() string { return filepath.Join(s.dir, "logs") }

func (s *Store) runPath(id string) string   { return filepath.Join(s.dir, "runs", id+".json") }
func (s *Store) seenPath(key string) string { return filepath.Join(s.dir, "seen", key) }

// SaveRun writes a run record, replacing any existing one.
func (s *Store) SaveRun(r Run) error {
	if !r.Kind.Valid() {
		return fmt.Errorf("save run: unknown kind %q", r.Kind)
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode run: %w", err)
	}
	return writeFileAtomic(s.runPath(r.ID()), append(raw, '\n'))
}

// Runs returns every recorded run, ordered by start time so the oldest is
// reaped and reported first.
func (s *Store) Runs() ([]Run, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "runs"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list runs: %w", err)
	}
	var runs []Run
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, "runs", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read run %s: %w", e.Name(), err)
		}
		var r Run
		if err := json.Unmarshal(raw, &r); err != nil {
			// A corrupt record must not wedge every future tick. Report it and
			// let the caller decide; skipping silently would hide a bug.
			return nil, fmt.Errorf("decode run %s: %w", e.Name(), err)
		}
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Started.Before(runs[j].Started) })
	return runs, nil
}

// DeleteRun removes a run record. Removing one that is already gone succeeds.
func (s *Store) DeleteRun(id string) error {
	err := os.Remove(s.runPath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete run %s: %w", id, err)
	}
	return nil
}

// NoteLatest records the newest instruction seen on a thread and reports
// whether it is something not yet acted on.
//
// The first sight of a thread records without reporting new. Without that,
// switching claudeflow on — or the standing pull request rolling over to a new
// number — would replay every comment ever written as if it had just arrived.
func (s *Store) NoteLatest(key string, latest time.Time) (isNew bool, err error) {
	if latest.IsZero() {
		return false, nil
	}
	path := s.seenPath(key)
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, s.MarkSeen(key, latest)
	case err != nil:
		return false, fmt.Errorf("read seen marker %s: %w", key, err)
	}
	seen, perr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if perr != nil {
		// An unreadable marker is treated as first sight rather than as "act on
		// everything": re-acting is the expensive direction.
		return false, s.MarkSeen(key, latest)
	}
	if !latest.After(seen) {
		return false, nil
	}
	return true, s.MarkSeen(key, latest)
}

// MarkSeen records a thread's position without acting on it.
func (s *Store) MarkSeen(key string, at time.Time) error {
	return writeFileAtomic(s.seenPath(key), []byte(at.UTC().Format(time.RFC3339Nano)+"\n"))
}

// ForgetSeenExcept drops seen-markers with the given prefix other than keep.
// A marker for a pull request that has since merged can never fire again,
// because only the current standing number is ever consulted.
func (s *Store) ForgetSeenExcept(prefix, keep string) error {
	entries, err := os.ReadDir(filepath.Join(s.dir, "seen"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("list seen markers: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || name == keep {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, "seen", name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove seen marker %s: %w", name, err)
		}
	}
	return nil
}

// Flag is a named on-disk switch.
type Flag string

const (
	// Paused stops dispatch. Live runs continue.
	Paused Flag = "PAUSED"
	// RateLimited holds the time dispatch may resume after a usage limit.
	RateLimited Flag = "RATE_LIMITED_UNTIL"
)

// SetFlag raises a flag, optionally carrying a value.
func (s *Store) SetFlag(f Flag, value string) error {
	return writeFileAtomic(filepath.Join(s.dir, string(f)), []byte(value))
}

// ClearFlag lowers a flag. Clearing one that is not raised succeeds.
func (s *Store) ClearFlag(f Flag) error {
	err := os.Remove(filepath.Join(s.dir, string(f)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear flag %s: %w", f, err)
	}
	return nil
}

// FlagRaised reports whether a flag is set.
func (s *Store) FlagRaised(f Flag) bool {
	_, err := os.Stat(filepath.Join(s.dir, string(f)))
	return err == nil
}

// SetRateLimited pauses dispatch until the given time.
func (s *Store) SetRateLimited(until time.Time) error {
	return s.SetFlag(RateLimited, until.UTC().Format(time.RFC3339))
}

// RateLimitedUntil returns the time dispatch may resume, if a backoff is in
// force. An unparseable or elapsed marker reports false.
func (s *Store) RateLimitedUntil(now time.Time) (time.Time, bool) {
	raw, err := os.ReadFile(filepath.Join(s.dir, string(RateLimited)))
	if err != nil {
		return time.Time{}, false
	}
	until, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
	if err != nil || !now.Before(until) {
		return time.Time{}, false
	}
	return until, true
}

// writeFileAtomic replaces a file in one step, so a tick interrupted mid-write
// cannot leave a half-written record that the next tick fails to decode.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
