// Package slot allocates isolated environments to runs.
//
// A slot is an integer and nothing more. What it means is the project's
// business: a port block and a database, a container set, a cloud namespace.
// claudeflow picks a number nothing else is using and hands it to the hooks.
package slot

import (
	"context"
	"errors"
	"fmt"
)

// ErrNone means every slot in the range is taken.
var ErrNone = errors.New("no free slot")

// BusyFunc reports whether a slot is occupied by anything at all.
//
// It exists because slot resources are usually global to the machine rather
// than to one checkout: another checkout, or a developer working by hand, can
// be holding a slot that claudeflow's own records know nothing about. Asking
// the machine is the only way to see them.
//
// An error is treated as busy. Allocating a slot whose state could not be
// determined risks two environments on one port block, which is worse than
// waiting for the next tick.
type BusyFunc func(ctx context.Context, n int) (bool, error)

// Allocator hands out slots from an inclusive range.
type Allocator struct {
	Min, Max int
	// Busy is optional. Without it only claudeflow's own claims are consulted,
	// which is correct when nothing else can touch the environments.
	Busy BusyFunc
}

// Free returns the lowest slot that is neither claimed nor externally busy.
//
// Claimed slots come from claudeflow's live run records. They are checked
// first because that costs nothing, where Busy usually runs a shell command.
func (a Allocator) Free(ctx context.Context, claimed []int) (int, error) {
	if a.Min > a.Max {
		return 0, fmt.Errorf("%w: empty range %d..%d", ErrNone, a.Min, a.Max)
	}
	taken := make(map[int]struct{}, len(claimed))
	for _, c := range claimed {
		taken[c] = struct{}{}
	}
	for n := a.Min; n <= a.Max; n++ {
		if _, ok := taken[n]; ok {
			continue
		}
		if a.Busy == nil {
			return n, nil
		}
		busy, err := a.Busy(ctx, n)
		if err != nil || busy {
			continue
		}
		return n, nil
	}
	return 0, fmt.Errorf("%w in range %d..%d", ErrNone, a.Min, a.Max)
}

// Count returns how many slots the range holds.
func (a Allocator) Count() int {
	if a.Min > a.Max {
		return 0
	}
	return a.Max - a.Min + 1
}
