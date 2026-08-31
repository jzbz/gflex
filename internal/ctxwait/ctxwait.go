// Package ctxwait holds the one cancellable wait this tool needs.
//
// It exists because the same dozen lines lived three times over -- in
// internal/session, internal/bootloader and internal/cli -- and the copies
// drifted. Two of them returned ctx.Err() for a non-positive duration; the cli
// one returned nil, and that difference was reachable: `scan --settle 0` waits
// here during the unplug/replug handover, and the guard immediately after it
// tests device presence before it consults ctx.Done(), so a Ctrl-C landing
// there was swallowed twice over and the scan carried on past the interrupt.
//
// One copy is one place for those semantics to be stated, pinned by tests and
// relied on by every retry loop in the tree.
package ctxwait

import (
	"context"
	"time"
)

// Sleep waits for d and reports why it stopped: nil once the full duration has
// elapsed, ctx.Err() if the context ended first.
//
// A non-positive d is NOT a no-op: it returns ctx.Err(), so the call remains a
// cancellation checkpoint rather than a way to skip one. Every caller sits
// inside a retry or settle loop -- VoltageMv's backoff clipping its last wait
// to the remaining budget, the PDO chunk retry delay, the flasher's inter-chunk
// pacing, `scan --settle` with whatever duration the user typed -- where a wait
// that collapses to zero must not also swallow the operator's Ctrl-C for
// another round trip.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
