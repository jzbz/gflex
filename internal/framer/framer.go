package framer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jzbz/gflex/internal/proto"
)

// ErrClosed is returned by SendFrame after the Framer has been closed.
var ErrClosed = errors.New("framer: closed")

// ErrReaderStuck is reported by Close when the reader goroutine was still
// inside the transport's ReadMIDI after closeGrace, so Close could not be the
// barrier it normally is. See Close.
var ErrReaderStuck = errors.New("framer: the reader goroutine did not exit")

// readBufSize is the size of a single ReadMIDI request. Inbound frames are at
// most 64 bytes, i.e. 198 MIDI bytes, so this comfortably holds several.
const readBufSize = 512

// idleBackoff is how long readLoop waits after a transport reports that no
// bytes are available, so that a transport whose "nothing yet" is instant
// cannot spin the CPU. Only usbmidi reaches it, and only for a zero-length or
// padding-only IN transfer; its ordinary quiet case is a 100 ms transfer
// timeout. On the default rawmidi transport the branch is unreachable
// altogether: the node is opened O_NONBLOCK so the read parks in the netpoller,
// and a zero-byte read from an os.File surfaces as io.EOF, never as (0, nil).
//
// It is not free when it does fire, and it is no longer small next to the
// pacing. The device does not pace what it sends: SPEC.md §14 Q15's timings put
// a whole multi-message response inside a few milliseconds, so an inbound byte
// can certainly be waiting sooner than 2 ms -- the outbound ByteDelay says
// nothing about the inbound direction. With the default ByteDelay now 1 ms
// (SPEC.md §14.15), 2 ms is larger than a whole outbound gap rather than noise
// against it, so on usbmidi a quiet spell can cost more than the pacing does.
// It is still bounded at one stall per response-arrival edge, and the real
// bound is narrower than that: the branch cannot be reached at all on the
// default rawmidi transport, for the reason above. Shrink it only alongside a
// measurement of usbmidi's zero-length-IN behaviour, which nothing here has.
const idleBackoff = 2 * time.Millisecond

// defaultCloseGrace bounds how long Close waits for the reader goroutine to
// leave ReadMIDI and exit. It is an order of magnitude above the longest a
// shipped transport can take to notice a close -- usbmidi's IN transfer carries
// a 100 ms timeout and rawmidi's read is evicted from the netpoller
// immediately -- so a healthy close never approaches it, and a bound that large
// still cannot make a CLI feel slow on the one path that reaches it.
const defaultCloseGrace = time.Second

// Framer couples a byte-level transport to the MIDI frame codec.
//
// Start launches a reader goroutine that publishes decoded protocol frames on
// Frames() and transport read errors on Errors(); SendFrame encodes and paces
// outbound frames. It performs no command matching and knows nothing about
// command semantics -- that is the session layer's job.
//
// SendFrame is safe for concurrent use, though the protocol itself is strictly
// half-duplex and the session layer serialises everything anyway (SPEC.md §5.3).
type Framer struct {
	t         proto.Transport
	byteDelay time.Duration
	dec       *Decoder

	frames chan []byte
	errs   chan error
	done   chan struct{}
	// readerExit is closed by readLoop on its way out, after frames and errs,
	// so that Close can tell the reader has actually gone rather than merely
	// been asked to. Close closes it itself when there is no reader to wait for.
	readerExit chan struct{}
	// closeGrace bounds the wait in Close. Only tests change it.
	closeGrace time.Duration

	writeMu sync.Mutex // serialises the message-by-message write of one frame

	mu      sync.Mutex // guards started and closed
	started bool
	closed  bool
}

// New returns a Framer over t. byteDelay is the pause inserted between outbound
// MIDI messages; pass proto.ByteDelay (1 ms) for the tool's default, or 20 ms
// to match the vendor client exactly. Zero disables pacing entirely; the
// closest thing SPEC.md §14.15 measured, 1 ns, dropped 2.5% and 3.3% of
// commands on the two units, so the CLI refuses zero and only tests should ask
// for it here.
//
// The caller keeps ownership of nothing: Close closes t.
func New(t proto.Transport, byteDelay time.Duration) *Framer {
	return &Framer{
		t:         t,
		byteDelay: byteDelay,
		dec:       NewDecoder(),
		// Buffered so a burst of frames, or an error nobody is listening for,
		// cannot wedge the reader goroutine.
		frames:     make(chan []byte, 16),
		errs:       make(chan error, 4),
		done:       make(chan struct{}),
		readerExit: make(chan struct{}),
		closeGrace: defaultCloseGrace,
	}
}

// SetDropHook installs a callback for frames the decoder discards. It must be
// called before Start.
func (f *Framer) SetDropHook(fn DropFunc) { f.dec.SetDropHook(fn) }

// Start launches the reader goroutine. It is idempotent, and a no-op after
// Close.
func (f *Framer) Start() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started || f.closed {
		return
	}
	f.started = true
	go f.readLoop()
}

// Frames returns the channel of decoded protocol frames. It is closed once the
// reader goroutine has exited, which Close both causes and (barring
// ErrReaderStuck) waits for.
func (f *Framer) Frames() <-chan []byte { return f.frames }

// Errors returns the channel of transport read errors. A read error is
// terminal: the reader goroutine reports it and stops. Errors caused by Close
// are suppressed. The channel is closed alongside Frames.
func (f *Framer) Errors() <-chan error { return f.errs }

// Close stops the reader, closes the underlying transport, and waits for the
// reader goroutine to exit. It is idempotent and returns the transport's close
// error, if any.
//
// The wait is what makes "closed" a barrier for the receive side: once Close
// returns, the reader is out of the transport, no further frame or error can
// appear on Frames() or Errors(), and no drop hook can still fire. (It is not a
// barrier for a SendFrame already past its closed check and inside WriteMIDI.
// Waiting on writeMu as well could park Close for a whole frame's worth of
// ByteDelay pacing, and it buys nothing: both shipped transports fail a write
// immediately once closed, so such a writer is already unwinding.) Callers depend on
// that. `gflex scan` closes the session and then tells the user to unplug the
// unit; the firmware flow closes a MIDI session, waits out a re-enumeration and
// opens a new transport on the same device (SPEC.md §10.1). A reader still
// parked in a read on the old descriptor during either of those is a descriptor
// this process has not actually let go of -- neither transport releases the fd
// until its in-flight read returns, so re-opening a rawmidi node that is opened
// exclusively per direction (SPEC.md §4.1) could report a spurious EBUSY, and a
// usbmidi IN transfer can still be outstanding on an interface Close has
// already handed back to snd-usb-audio.
//
// The wait is bounded because it cannot be trusted absolutely. A blocked
// ReadMIDI is only interruptible because Close pulls the transport out from
// under it: rawmidi's read unblocks because *os.File.Close evicts the netpoller
// wait, and usbmidi's because its transfer carries its own 100 ms timeout.
// Neither is a promise the proto.Transport interface makes, and usbfs cannot
// abort an ioctl the kernel has already submitted. Waiting unconditionally
// would trade a leaked goroutine for a hung CLI, which is the worse failure --
// so Close gives up after closeGrace, reports ErrReaderStuck and returns,
// degrading to a non-barrier close in precisely the case where the barrier was
// unobtainable. Callers already treat a Close error as a diagnostic rather than
// a command failure; a stuck reader must never be reported as "the write
// failed" (SPEC.md §17, on ErrReadBack).
func (f *Framer) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		// A second or concurrent Close is a barrier too, otherwise the property
		// above would hold only for whichever caller happened to be first. The
		// first caller owns the error.
		f.awaitReader()
		return nil
	}
	f.closed = true
	started := f.started
	f.mu.Unlock()

	// Signal before closing the transport, so the read error the close provokes
	// is recognised as expected rather than reported.
	close(f.done)
	err := f.t.Close()
	if err != nil {
		err = fmt.Errorf("framer: closing transport %s: %w", f.t.Name(), err)
	}
	if !started {
		// Nobody will ever run readLoop, so close the channels here instead.
		// Start is already a no-op once closed is set, so this cannot race with
		// a reader that is about to come into existence.
		close(f.frames)
		close(f.errs)
		close(f.readerExit)
	}
	if !f.awaitReader() {
		return errors.Join(err, fmt.Errorf(
			"%w within %s: it is still inside %s.ReadMIDI, which closing the transport did not interrupt; "+
				"the transport is closed and nothing further will be delivered, but the goroutine and the "+
				"descriptor underneath it are held until that read returns",
			ErrReaderStuck, f.closeGrace, f.t.Name()))
	}
	return err
}

// awaitReader waits up to closeGrace for readLoop to exit, reporting whether it
// did. It never blocks indefinitely: once done is closed every send in readLoop
// has a ready case, so the only thing that can hold the reader is the
// transport's own read.
func (f *Framer) awaitReader() bool {
	select {
	case <-f.readerExit:
		return true // the overwhelmingly common case: no timer, no wait
	default:
	}
	t := time.NewTimer(f.closeGrace)
	defer t.Stop()
	select {
	case <-f.readerExit:
		return true
	case <-t.C:
		return false
	}
}

// readLoop is the sole sender on f.frames and f.errs, which is why it may close
// them.
func (f *Framer) readLoop() {
	// Declared first so it runs last: readerExit is what Close waits on, and it
	// must not be observable before the channels a caller may still be draining
	// have been closed.
	defer close(f.readerExit)
	defer close(f.frames)
	defer close(f.errs)

	buf := make([]byte, readBufSize)
	for {
		n, err := f.t.ReadMIDI(buf)
		if n > 0 {
			// A read can end mid-message; the decoder keeps the state.
			for _, frame := range f.dec.Feed(buf[:n]) {
				select {
				case f.frames <- frame:
				case <-f.done:
					return
				}
			}
		}
		if err != nil {
			select {
			case <-f.done:
				// Expected: Close closed the transport out from under the read.
			default:
				select {
				case f.errs <- fmt.Errorf("framer: reading from %s: %w", f.t.Name(), err):
				default: // nobody listening and the buffer is full; drop it
				}
			}
			return
		}
		if n == 0 {
			// A transport may legitimately return (0, nil) to mean "nothing
			// available right now" -- usbmidi does, for a zero-length IN packet
			// or a transfer carrying only padding, both of which complete
			// instantly. Re-entering ReadMIDI immediately would pin a core
			// issuing ioctls for as long as the device stays quiet. The wait
			// can delay a reply by up to idleBackoff -- the device does not
			// pace what it sends (SPEC.md §14 Q15) -- but only once per
			// arrival edge; see idleBackoff for why that is cheap.
			select {
			case <-f.done:
				return
			case <-time.After(idleBackoff):
			}
			continue
		}
		select {
		case <-f.done:
			return
		default:
		}
	}
}

// SendFrame encodes frame and writes it to the transport one MIDI message at a
// time, pausing byteDelay after the start marker and after each data message
// but not after the end marker (SPEC.md §3.1).
//
// Unlike the vendor client, which swallows every write exception and lets the
// caller discover the failure as a timeout five seconds later, this returns the
// real error. Cancelling ctx aborts between messages; the frame is then
// truncated on the wire and the device's receiver will drop it for want of an
// end-of-frame marker.
func (f *Framer) SendFrame(ctx context.Context, frame []byte) error {
	select {
	case <-f.done:
		return ErrClosed
	default:
	}

	msgs := MIDIMessages(frame)

	f.writeMu.Lock()
	defer f.writeMu.Unlock()

	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for i, m := range msgs {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("framer: sending frame %s: %w", proto.Hex(frame), err)
		}
		if err := f.t.WriteMIDI(m); err != nil {
			return fmt.Errorf("framer: writing MIDI message %d/%d of frame %s: %w",
				i+1, len(msgs), proto.Hex(frame), err)
		}
		// No delay after the final message: the vendor client sleeps after the
		// start marker and after each data byte only.
		if i == len(msgs)-1 || f.byteDelay <= 0 {
			continue
		}
		if timer == nil {
			timer = time.NewTimer(f.byteDelay)
		} else {
			timer.Reset(f.byteDelay)
		}
		select {
		case <-timer.C:
		case <-ctx.Done():
			return fmt.Errorf("framer: sending frame %s: %w", proto.Hex(frame), ctx.Err())
		}
	}
	return nil
}
