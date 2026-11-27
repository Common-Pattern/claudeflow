package data

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clock(t *time.Time) func() time.Time { return func() time.Time { return *t } }

func TestForRunIsAnEmptyDirectoryBeforeAnythingIsPrepared(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	dir, err := s.ForRun()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("ForRun = %s holding %v (%v), want an empty directory", dir, entries, err)
	}
	if !s.Due(time.Hour) {
		t.Error("nothing prepared is not due")
	}
}

func TestPreparePublishesAGeneration(t *testing.T) {
	now := time.Date(2026, 11, 27, 4, 0, 0, 0, time.UTC)
	s := Store{Dir: filepath.Join(t.TempDir(), "data"), Now: clock(&now)}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	g, err := s.Prepare(context.Background(), `umask 077; echo "$GREETING" > "$CLAUDEFLOW_DATA_OUT/dump"`, t.TempDir(),
		map[string]string{"GREETING": "hello"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Prepared.Equal(now) {
		t.Errorf("prepared at %s, want %s", g.Prepared, now)
	}
	cur, ok := s.Current()
	if !ok || cur.Path != g.Path {
		t.Fatalf("current = %+v %v, want %s", cur, ok, g.Path)
	}
	raw, err := os.ReadFile(filepath.Join(g.Path, "dump"))
	if err != nil || strings.TrimSpace(string(raw)) != "hello" {
		t.Fatalf("dump = %q, %v", raw, err)
	}
	// Opened up for a container's users, whatever the hook's umask was.
	if fi, _ := os.Stat(filepath.Join(g.Path, "dump")); fi.Mode().Perm()&0o004 == 0 {
		t.Errorf("dump mode %v is not world-readable", fi.Mode().Perm())
	}
	// And the store itself closed to everyone else.
	if fi, _ := os.Stat(s.Dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("store mode %v, want 0700", fi.Mode().Perm())
	}
	if s.Due(time.Hour) {
		t.Error("a generation prepared just now is due")
	}
	now = now.Add(2 * time.Hour)
	if !s.Due(time.Hour) {
		t.Error("a two-hour-old generation is not due at one hour")
	}
}

// A failed prepare leaves the previous generation current and no staging
// directory behind.
func TestFailedPrepareKeepsThePrevious(t *testing.T) {
	now := time.Date(2026, 11, 27, 4, 0, 0, 0, time.UTC)
	s := Store{Dir: t.TempDir(), Now: clock(&now)}
	first, err := s.Prepare(context.Background(), `touch "$CLAUDEFLOW_DATA_OUT/a"`, t.TempDir(), nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	_, err = s.Prepare(context.Background(), `test "$CLAUDEFLOW_DATA_PREVIOUS" = "`+first.Path+`" && touch "$CLAUDEFLOW_DATA_OUT/half" && exit 3`, t.TempDir(), nil, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "exited 3") {
		t.Fatalf("err = %v, want exit 3", err)
	}
	if cur, _ := s.Current(); cur.Path != first.Path {
		t.Errorf("current = %s, want the first generation %s", cur.Path, first.Path)
	}
	matches, _ := filepath.Glob(filepath.Join(s.Dir, "staging-*"))
	if len(matches) != 0 {
		t.Errorf("staging left behind: %v", matches)
	}
}

// Prune keeps the current generation, the one before it, and any a run
// refers to.
func TestPruneKeepsWhatIsInUse(t *testing.T) {
	now := time.Date(2026, 11, 27, 4, 0, 0, 0, time.UTC)
	s := Store{Dir: t.TempDir(), Now: clock(&now)}
	var gens []Generation
	for range 4 {
		g, err := s.Prepare(context.Background(), "true", t.TempDir(), nil, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		gens = append(gens, g)
		now = now.Add(time.Hour)
	}
	removed, err := s.Prune([]string{gens[0].Path, ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != gens[1].Path {
		t.Fatalf("removed %v, want only %s", removed, gens[1].Path)
	}
	for _, g := range []Generation{gens[0], gens[2], gens[3]} {
		if _, err := os.Stat(g.Path); err != nil {
			t.Errorf("%s was removed: %v", g.Path, err)
		}
	}
}
