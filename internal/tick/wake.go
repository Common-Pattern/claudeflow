package tick

import (
	"context"
	"time"
)

// Waker decides when the supervisor's next tick is due: when interval has
// passed since the last one, or sooner, when Changed reports that GitHub has
// something new.
//
// A full tick costs a dozen or more API calls, so it cannot run every few
// seconds. Changed costs one, and nothing at all against the rate limit when
// the answer is "no" — so asking it every Every is how a label or a comment
// gets acted on in seconds rather than up to a whole interval later. The
// interval stays as the backstop: if Changed fails, or misses something, the
// next tick is no later than it was before Changed existed.
type Waker struct {
	// Interval is the longest the supervisor waits between ticks.
	Interval time.Duration
	// Every is how often Changed is asked. Zero or negative disables it, and
	// Wait then waits out the interval.
	Every time.Duration
	// Changed reports whether GitHub has anything new since it last answered.
	Changed func(ctx context.Context) (bool, error)
	// Log receives a failing Changed, once per run of failures.
	Log func(format string, args ...any)

	failing bool
}

// Wait blocks until the next tick is due, and reports whether it ended early
// because something changed. It returns ctx's error when ctx is cancelled.
//
// Changes made by the tick itself — a label it edited, a comment it posted —
// are seen here as changes, and bring the following tick forward. That costs
// one extra tick, which finds nothing to do and changes nothing, so it does
// not repeat.
func (w *Waker) Wait(ctx context.Context) (bool, error) {
	deadline := time.NewTimer(w.Interval)
	defer deadline.Stop()
	if w.Every <= 0 || w.Changed == nil {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		}
	}
	poll := time.NewTicker(w.Every)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		case <-poll.C:
			if w.check(ctx) {
				return true, nil
			}
		}
	}
}

func (w *Waker) check(ctx context.Context) bool {
	// One request, bounded so that a hung gh cannot hold the next tick past
	// its deadline by more than this.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	changed, err := w.Changed(ctx)
	if err != nil {
		// A network blip should not write a line every few seconds until it
		// clears; the first failure is the one worth reading.
		if !w.failing && w.Log != nil && ctx.Err() == nil {
			w.Log("watching for changes: %v; ticking every %s until it recovers", err, w.Interval)
		}
		w.failing = true
		return false
	}
	if w.failing && w.Log != nil {
		w.Log("watching for changes: recovered")
	}
	w.failing = false
	return changed
}
