package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Common-Pattern/claudeflow/internal/config"
	"github.com/Common-Pattern/claudeflow/internal/data"
	"github.com/Common-Pattern/claudeflow/internal/state"
)

func dataStore(cfg config.Config) data.Store { return data.Store{Dir: cfg.Data.Dir} }

// runData is `claudeflow data [prepare]`: report the current generation, or
// prepare a new one now.
func runData(ctx context.Context, cfg config.Config, st *state.Store, args []string) error {
	if !cfg.Data.Enabled() {
		return errors.New("this project prepares no data: set data.prepare in the config")
	}
	switch {
	case len(args) == 0:
		fmt.Println(dataSummary(cfg, time.Now()))
		return nil
	case len(args) == 1 && args[0] == "prepare":
		g, err := prepareData(ctx, cfg, st)
		if err != nil {
			return err
		}
		fmt.Printf("prepared %s\n", g.Path)
		return nil
	default:
		return errors.New("usage: claudeflow data [prepare]")
	}
}

// dataSummary is one line on the current generation, for status and data.
func dataSummary(cfg config.Config, now time.Time) string {
	g, ok := dataStore(cfg).Current()
	if !ok {
		return fmt.Sprintf("data:    none prepared yet in %s (runs get an empty directory)", cfg.Data.Dir)
	}
	age := now.Sub(g.Prepared).Truncate(time.Minute)
	stale := ""
	if age > cfg.Data.Every {
		stale = fmt.Sprintf(" — older than data.every (%s); see %s", cfg.Data.Every, dataStore(cfg).LogPath())
	}
	return fmt.Sprintf("data:    %s, prepared %s ago%s", g.Path, age, stale)
}

// prepareData runs the project's prepare hook and prunes what no run needs.
func prepareData(ctx context.Context, cfg config.Config, st *state.Store) (data.Generation, error) {
	s := dataStore(cfg)
	logf("data: preparing (log %s)", s.LogPath())
	start := time.Now()
	g, err := s.Prepare(ctx, cfg.Data.Prepare, cfg.Paths.Root, map[string]string{
		"CLAUDEFLOW_REPO": cfg.Repo,
	}, cfg.Data.Timeout)
	if err != nil {
		return g, err
	}
	logf("data: published %s in %s", g.Path, time.Since(start).Truncate(time.Second))
	pruneData(cfg, st)
	return g, nil
}

// pruneData removes generations no recorded run refers to.
func pruneData(cfg config.Config, st *state.Store) {
	runs, err := st.Runs()
	if err != nil {
		// Without the records there is no knowing what is in use.
		logf("data: not pruning: %v", err)
		return
	}
	var inUse []string
	for _, r := range runs {
		inUse = append(inUse, r.Data)
	}
	removed, err := dataStore(cfg).Prune(inUse)
	for _, p := range removed {
		logf("data: removed %s", p)
	}
	if err != nil {
		logf("data: prune: %v", err)
	}
}

// refreshData keeps the prepared data current for as long as serve runs.
//
// It runs beside the tick rather than in it: a prepare can take as long as a
// production restore does, and dispatch must not wait on it. Runs that start
// meanwhile get the generation that is current, which is the point of
// publishing generations rather than writing in place.
func refreshData(ctx context.Context, cfg config.Config, st *state.Store) {
	s := dataStore(cfg)
	retry := min(cfg.Data.Every, time.Hour)
	var failedAt time.Time
	for {
		if s.Due(cfg.Data.Every) && (failedAt.IsZero() || time.Since(failedAt) >= retry) {
			if _, err := prepareData(ctx, cfg, st); err != nil {
				if ctx.Err() != nil {
					return
				}
				failedAt = time.Now()
				logf("data: %v; runs keep the previous generation, retrying in %s", err, retry)
			} else {
				failedAt = time.Time{}
			}
		} else {
			// Runs that ended since the last pass may have released one.
			pruneData(cfg, st)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
}
