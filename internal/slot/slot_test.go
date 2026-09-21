package slot

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func busySet(ns ...int) BusyFunc {
	set := map[int]bool{}
	for _, n := range ns {
		set[n] = true
	}
	return func(_ context.Context, n int) (bool, error) { return set[n], nil }
}

func TestFreePicksLowestAvailable(t *testing.T) {
	a := Allocator{Min: 1, Max: 6}
	got, err := a.Free(t.Context(), nil)
	if err != nil {
		t.Fatalf("Free: %v", err)
	}
	if got != 1 {
		t.Errorf("Free = %d, want 1", got)
	}
}

func TestFreeSkipsClaimed(t *testing.T) {
	a := Allocator{Min: 1, Max: 6}
	got, err := a.Free(t.Context(), []int{1, 2})
	if err != nil {
		t.Fatalf("Free: %v", err)
	}
	if got != 3 {
		t.Errorf("Free = %d, want 3", got)
	}
}

// The whole reason BusyFunc exists: another checkout can hold a slot that
// claudeflow's own records know nothing about.
func TestFreeSkipsExternallyBusy(t *testing.T) {
	a := Allocator{Min: 1, Max: 6, Busy: busySet(1, 2, 3)}
	got, err := a.Free(t.Context(), nil)
	if err != nil {
		t.Fatalf("Free: %v", err)
	}
	if got != 4 {
		t.Errorf("Free = %d, want 4 — slots 1..3 are busy externally", got)
	}
}

func TestFreeCombinesClaimedAndBusy(t *testing.T) {
	a := Allocator{Min: 1, Max: 6, Busy: busySet(3, 4)}
	got, err := a.Free(t.Context(), []int{1, 2})
	if err != nil {
		t.Fatalf("Free: %v", err)
	}
	if got != 5 {
		t.Errorf("Free = %d, want 5", got)
	}
}

func TestFreeExhausted(t *testing.T) {
	a := Allocator{Min: 1, Max: 2, Busy: busySet(2)}
	_, err := a.Free(t.Context(), []int{1})
	if !errors.Is(err, ErrNone) {
		t.Errorf("err = %v, want ErrNone", err)
	}
}

// A slot whose state could not be determined must not be allocated: two
// environments on one port block is worse than waiting for the next tick.
func TestFreeTreatsCheckErrorAsBusy(t *testing.T) {
	a := Allocator{Min: 1, Max: 2, Busy: func(_ context.Context, n int) (bool, error) {
		if n == 1 {
			return false, fmt.Errorf("probe failed")
		}
		return false, nil
	}}
	got, err := a.Free(t.Context(), nil)
	if err != nil {
		t.Fatalf("Free: %v", err)
	}
	if got != 2 {
		t.Errorf("Free = %d, want 2 — slot 1's probe errored and must be skipped", got)
	}
}

func TestFreeAllProbesErrorMeansNone(t *testing.T) {
	a := Allocator{Min: 1, Max: 3, Busy: func(context.Context, int) (bool, error) {
		return false, fmt.Errorf("probe failed")
	}}
	if _, err := a.Free(t.Context(), nil); !errors.Is(err, ErrNone) {
		t.Errorf("err = %v, want ErrNone", err)
	}
}

func TestFreeEmptyRange(t *testing.T) {
	a := Allocator{Min: 5, Max: 1}
	if _, err := a.Free(t.Context(), nil); !errors.Is(err, ErrNone) {
		t.Errorf("err = %v, want ErrNone", err)
	}
}

func TestFreeWithoutBusyFunc(t *testing.T) {
	a := Allocator{Min: 2, Max: 3}
	got, err := a.Free(t.Context(), []int{2})
	if err != nil {
		t.Fatalf("Free: %v", err)
	}
	if got != 3 {
		t.Errorf("Free = %d, want 3", got)
	}
}

// A project reserving slot 0 for its main stack sets Min to 1, and 0 must
// never be handed out.
func TestFreeRespectsMin(t *testing.T) {
	a := Allocator{Min: 1, Max: 3}
	for range 3 {
		got, err := a.Free(t.Context(), nil)
		if err != nil {
			t.Fatalf("Free: %v", err)
		}
		if got < 1 {
			t.Fatalf("Free = %d, want >= 1", got)
		}
	}
}

func TestCount(t *testing.T) {
	for _, tc := range []struct{ min, max, want int }{
		{1, 6, 6}, {0, 0, 1}, {2, 4, 3}, {5, 1, 0},
	} {
		if got := (Allocator{Min: tc.min, Max: tc.max}).Count(); got != tc.want {
			t.Errorf("Allocator{%d,%d}.Count() = %d, want %d", tc.min, tc.max, got, tc.want)
		}
	}
}
