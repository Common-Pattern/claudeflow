package tick

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestWakerEndsEarlyOnChange(t *testing.T) {
	calls := 0
	w := Waker{Interval: time.Hour, Every: time.Millisecond, Changed: func(context.Context) (bool, error) {
		calls++
		return calls == 3, nil
	}}
	early, err := w.Wait(t.Context())
	if err != nil || !early {
		t.Fatalf("Wait = %v, %v; want an early wake", early, err)
	}
	if calls != 3 {
		t.Errorf("Changed asked %d times, want 3", calls)
	}
}

func TestWakerFallsBackToInterval(t *testing.T) {
	w := Waker{Interval: 20 * time.Millisecond, Every: time.Millisecond, Changed: func(context.Context) (bool, error) {
		return false, nil
	}}
	early, err := w.Wait(t.Context())
	if err != nil || early {
		t.Fatalf("Wait = %v, %v; want the interval to end it", early, err)
	}
}

func TestWakerDisabled(t *testing.T) {
	w := Waker{Interval: 5 * time.Millisecond, Changed: func(context.Context) (bool, error) {
		t.Error("Changed asked with Every unset")
		return true, nil
	}}
	if early, err := w.Wait(t.Context()); err != nil || early {
		t.Fatalf("Wait = %v, %v; want the interval to end it", early, err)
	}
}

func TestWakerCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w := Waker{Interval: time.Hour, Every: time.Hour}
	if _, err := w.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
}

// A failing check falls back to the interval, and logs once per run of
// failures rather than once per check.
func TestWakerLogsFailureOnce(t *testing.T) {
	calls := 0
	var lines []string
	w := Waker{
		Interval: 30 * time.Millisecond, Every: time.Millisecond,
		Changed: func(context.Context) (bool, error) {
			calls++
			return false, errors.New("offline")
		},
		Log: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) },
	}
	if early, _ := w.Wait(t.Context()); early {
		t.Fatal("a failing check woke the supervisor")
	}
	if calls < 2 {
		t.Fatalf("Changed asked %d times; the test needs at least 2", calls)
	}
	if len(lines) != 1 {
		t.Errorf("logged %d lines over %d failures, want 1: %q", len(lines), calls, lines)
	}

	w.Changed = func(context.Context) (bool, error) { return true, nil }
	if early, _ := w.Wait(t.Context()); !early {
		t.Fatal("recovered check did not wake the supervisor")
	}
	if len(lines) != 2 || lines[1] != "watching for changes: recovered" {
		t.Errorf("lines = %q, want a recovery line after the failure", lines)
	}
}
