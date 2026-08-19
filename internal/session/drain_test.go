package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/transport/fake"
)

// deadSession builds a session whose framer has already died with n stale
// serial-number frames still queued, and waits until that has actually
// happened -- the ordering the drains have to survive: the reader publishes
// its terminal error, closes the error channel and then the frame channel,
// leaving frames buffered behind an already-closed channel.
//
// The fixture leans on two documented fake.Device guarantees: an unplugged
// Device delivers everything Push queued before failing the read, so all n
// frames deterministically reach the framer ahead of the error; and writes
// after the unplug still transmit and are recorded by Sent, which is what
// lets TestStaleFrameCannotAnswerNextCommand prove a command really went out.
// Draining the error channel here is what makes the wait deterministic: it
// returns only once the reader has published its error and closed the
// channel, by which point every frame it decoded is sitting in the frame
// buffer.
//
// n should be large: once the error channel is closed, a receive from it is
// permanently ready, so a select between it and a queued frame is a coin flip
// and one buffered frame would expose a stop-at-first-closed-channel bug only
// half the time. Twelve reduce that to one run in 4096, and the framer's
// frame channel holds sixteen.
func deadSession(t *testing.T, n int) (*Session, *fake.Device) {
	t.Helper()
	d := fake.New()
	d.SetDefault(nil)
	for i := 0; i < n; i++ {
		if err := d.Push(mustBuild(proto.CmdSerialNumber, []byte("STALE001"), false)); err != nil {
			t.Fatalf("push stale frame %d: %v", i, err)
		}
	}
	d.Unplug(errors.New("read /dev/snd/midiC1D0: no such device"))

	s := New(d.Transport(), Options{ByteDelay: time.Nanosecond, Timeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = s.Close() })

	for range s.fr.Errors() { //nolint:revive // drained purely to wait for closure
	}
	if got := len(s.fr.Frames()); got != n {
		t.Fatalf("%d frames buffered, want %d: the fixture is not reproducing the case", got, n)
	}
	return s, d
}

// TestDrainStaleEmptiesBothChannels: drainStale must not stop at the first
// closed channel.
//
// The framer closes its error channel before its frame channel, so returning on
// the first !ok leaves the queued frames in place -- and the very next command
// then takes one of them as its own answer, which is precisely what the drain
// exists to prevent (SPEC.md §5.2, §5.3).
func TestDrainStaleEmptiesBothChannels(t *testing.T) {
	s, _ := deadSession(t, 12)

	s.drainStale() // nothing else is running, so the single-flight lock is free

	if got := len(s.fr.Frames()); got != 0 {
		t.Errorf("%d frames still queued after drainStale; a closed channel means stop reading that channel, not stop draining", got)
	}
}

// TestDrainForEmptiesBothChannels is the same requirement for the post-timeout
// settle drain, which is the one that absorbs a late answer so it cannot satisfy
// the NEXT command with the same code.
func TestDrainForEmptiesBothChannels(t *testing.T) {
	s, _ := deadSession(t, 12)

	start := time.Now()
	s.drainFor(context.Background(), settleAfterTimeout)

	if got := len(s.fr.Frames()); got != 0 {
		t.Errorf("%d frames still queued after drainFor", got)
	}
	// Exhausting both channels means nothing more can arrive, so it must not sit
	// out the rest of the settle window.
	if elapsed := time.Since(start); elapsed >= settleAfterTimeout {
		t.Errorf("drainFor took %v; with both channels closed it should return at once", elapsed)
	}
}

// TestStaleFrameCannotAnswerNextCommand is the failure the two tests above
// prevent, seen from the outside: a dead transport with a stale serial-number
// response still queued must fail the next read, not answer it with the stale
// value. A serial number is the mildest case to demonstrate; the same drain
// guards command 18 (voltage) and 23 (vlimit), where the answer to the previous
// command is a voltage the caller would then act on.
func TestStaleFrameCannotAnswerNextCommand(t *testing.T) {
	s, d := deadSession(t, 12)

	got, err := s.SerialNumber(context.Background())
	// The command really was issued: the drain happens before the send, so this
	// is the stale frame being discarded rather than the command being skipped.
	// (An unplugged fake.Device still decodes and records what the host
	// transmits, precisely so this can be asserted.)
	if len(d.Sent()) == 0 {
		t.Error("nothing was transmitted; the test is not exercising the drain-then-send path")
	}
	if err == nil {
		t.Fatalf("SerialNumber returned %q from a frame queued before the command was sent", got)
	}
	if !errors.Is(err, ErrTransportClosed) {
		t.Errorf("error = %v, want ErrTransportClosed", err)
	}
	if strings.Contains(err.Error(), "STALE001") {
		t.Errorf("error = %v, want no trace of the stale payload", err)
	}
}

// TestSleepCtxSemantics pins what sleepCtx does, in particular for a
// non-positive duration: it is a cancellation checkpoint, not a no-op. The
// copies in internal/cli and internal/bootloader disagree on this, so pin the
// one this package's retry loops depend on.
func TestSleepCtxSemantics(t *testing.T) {
	t.Run("elapsed", func(t *testing.T) {
		start := time.Now()
		if err := sleepCtx(context.Background(), 20*time.Millisecond); err != nil {
			t.Fatalf("sleepCtx = %v, want nil", err)
		}
		if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
			t.Errorf("returned after %v, want the full 20ms", elapsed)
		}
	})

	t.Run("cancelled while waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		if err := sleepCtx(ctx, 10*time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("sleepCtx = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("waited %v; cancellation must cut the sleep short", elapsed)
		}
	})

	t.Run("non-positive duration is still a checkpoint", func(t *testing.T) {
		if err := sleepCtx(context.Background(), 0); err != nil {
			t.Errorf("sleepCtx(live, 0) = %v, want nil", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := sleepCtx(ctx, 0); !errors.Is(err, context.Canceled) {
			t.Errorf("sleepCtx(cancelled, 0) = %v, want context.Canceled: a zero wait must not swallow a cancellation", err)
		}
		if err := sleepCtx(ctx, -time.Second); !errors.Is(err, context.Canceled) {
			t.Errorf("sleepCtx(cancelled, -1s) = %v, want context.Canceled", err)
		}
	})
}
