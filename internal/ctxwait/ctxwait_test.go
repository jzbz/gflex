package ctxwait

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSleepElapses is the ordinary case: the full duration is waited out and
// nothing is reported.
func TestSleepElapses(t *testing.T) {
	t.Parallel()
	start := time.Now()
	if err := Sleep(context.Background(), 20*time.Millisecond); err != nil {
		t.Fatalf("Sleep = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("returned after %v, want the full 20ms", elapsed)
	}
}

// A wait in progress is cut short by a cancellation rather than slept through,
// which is what makes every retry loop in the tree interruptible.
func TestSleepCancelledWhileWaiting(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if err := Sleep(ctx, 10*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v; cancellation must cut the sleep short", elapsed)
	}
}

// An already-cancelled context is reported before any timer is armed, so a long
// wait on a dead context costs nothing.
func TestSleepPositiveDurationHonoursCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := Sleep(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep(cancelled, 1m) = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("slept %v on a cancelled context", elapsed)
	}
}

// TestSleepNonPositiveIsACancellationCheckpoint pins the semantics the cli copy
// of this helper drifted away from before the three were unified.
//
// A non-positive duration is not a no-op: it returns ctx.Err(). The divergence
// was reachable -- `scan --settle` takes any duration the user types, scan
// waits here during the unplug/replug handover, and the guard immediately after
// (waitForDevice) tests device presence before it consults ctx.Done(). A Ctrl-C
// landing there was swallowed twice over and the scan carried on.
func TestSleepNonPositiveIsACancellationCheckpoint(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, d := range []time.Duration{0, -time.Second} {
		if err := Sleep(ctx, d); !errors.Is(err, context.Canceled) {
			t.Errorf("Sleep(cancelled, %v) = %v, want context.Canceled: a non-positive "+
				"duration must stay a cancellation checkpoint, not skip one", d, err)
		}
	}
}

// The other half: a non-positive duration against a live context still costs
// nothing and reports no error, so the callers that compute a delay of zero on
// their first iteration are not turned into failures.
func TestSleepNonPositiveOnALiveContextSucceeds(t *testing.T) {
	t.Parallel()
	if err := Sleep(context.Background(), 0); err != nil {
		t.Errorf("Sleep(live, 0) = %v, want nil", err)
	}
	if err := Sleep(context.Background(), -time.Second); err != nil {
		t.Errorf("Sleep(live, -1s) = %v, want nil", err)
	}
}
