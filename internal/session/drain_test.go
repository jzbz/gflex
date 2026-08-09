package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
)

// staleThenDeadDev hands the framer one batch of MIDI bytes and then fails every
// subsequent read, which reproduces the ordering the drains have to survive: the
// reader publishes its terminal error, closes the error channel and then the
// frame channel, leaving frames buffered behind an already-closed channel.
type staleThenDeadDev struct {
	pending []byte // MIDI bytes still to hand over; touched only by ReadMIDI
	err     error

	mu   sync.Mutex
	sent int

	closed    chan struct{}
	closeOnce sync.Once
}

func newStaleThenDeadDev(frames [][]byte, err error) *staleThenDeadDev {
	var midi []byte
	for _, f := range frames {
		midi = append(midi, encodeTestMIDI(f)...)
	}
	return &staleThenDeadDev{pending: midi, err: err, closed: make(chan struct{})}
}

func (d *staleThenDeadDev) Name() string { return "stale-then-dead" }

func (d *staleThenDeadDev) WriteMIDI([]byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent++
	return nil
}

// writes reports how many MIDI messages the host has put on the wire.
func (d *staleThenDeadDev) writes() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sent
}

// ReadMIDI is called from the framer's single reader goroutine only.
func (d *staleThenDeadDev) ReadMIDI(p []byte) (int, error) {
	if len(d.pending) > 0 {
		n := copy(p, d.pending)
		d.pending = d.pending[n:]
		return n, nil
	}
	return 0, d.err
}

func (d *staleThenDeadDev) Close() error {
	d.closeOnce.Do(func() { close(d.closed) })
	return nil
}

// staleFrames returns n identical serial-number responses, enough of them that a
// drain which stops at the first closed channel is caught.
//
// Once the error channel is closed, a receive from it is permanently ready, so a
// select between it and a queued frame is a coin flip: one buffered frame would
// expose the bug only half the time. Twelve reduce that to one run in 4096, and
// the frames channel holds sixteen.
func staleFrames(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = mustBuild(proto.CmdSerialNumber, []byte("STALE001"), false)
	}
	return out
}

// deadSession builds a session whose framer has already died with n stale frames
// still queued, and waits until that has actually happened. Draining the error
// channel here is what makes the wait deterministic: it returns only once the
// reader has published its error and closed the channel, by which point every
// frame it decoded is sitting in the frame buffer.
func deadSession(t *testing.T, n int) (*Session, *staleThenDeadDev) {
	t.Helper()
	d := newStaleThenDeadDev(staleFrames(n), errors.New("read /dev/snd/midiC1D0: no such device"))
	s := New(d, Options{ByteDelay: time.Nanosecond, Timeout: 200 * time.Millisecond})
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
	if d.writes() == 0 {
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
