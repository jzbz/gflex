// Package session is layer 3 of the VFLEX stack (SPEC.md §12): the command
// table made typed, on top of the framer's SOF/nibble/EOF codec.
//
// Everything in this package funnels through one single-flight mutex. The VFLEX
// protocol is strictly half-duplex with exactly one command outstanding: there
// is no request tag and no pipelining, so a second concurrent command would be
// satisfied by the first one's response (SPEC.md §5.3). The vendor client
// serialises the same way, through a promise-chain mutex.
//
// There is no NACK and no device-reported error anywhere in this protocol
// (SPEC.md §5.2). A command either receives a frame whose command code matches,
// or it times out. In particular a *mismatched* response is not an error: it is
// traced and dropped while the wait stays pending, exactly as the reference
// client does, because the matching frame may still be behind it.
package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jzbz/gflex/internal/framer"
	"github.com/jzbz/gflex/internal/proto"
)

// Session errors.
//
// ErrTimeout and ErrNoConnection carry the vendor client's own wording
// (SPEC.md §9.6) rather than idiomatic lowercase Go error strings. That is
// deliberate: these strings are what users will paste into a search box, so
// they must match the app they are migrating from.
var (
	// ErrTimeout is returned when no frame carrying the requested command code
	// arrived before the deadline. It is the only failure mode a well-formed
	// command has.
	ErrTimeout = errors.New("Response timeout exceeded")

	// ErrNoConnection is returned once the session has been closed.
	ErrNoConnection = errors.New("No connection established")

	// ErrTransportClosed is returned when the framer shuts down mid-command,
	// e.g. because the device was unplugged.
	ErrTransportClosed = errors.New("session: transport closed while awaiting a response")

	// ErrReadBack marks a write that the device ACKNOWLEDGED but whose
	// confirming read failed. The setting did take effect; only the
	// verification did not. Callers must not report this as "the write
	// failed" -- on a voltage write that would tell the user the rail is
	// unchanged while it is live at the new value.
	ErrReadBack = errors.New("write succeeded but could not be read back")
)

// Options configures a Session. The zero value is valid and selects the vendor
// application's own defaults.
type Options struct {
	// ByteDelay is the pause the framer inserts between MIDI messages.
	// Defaults to proto.ByteDelay (20 ms).
	ByteDelay time.Duration
	// Timeout is the per-command response timeout. Defaults to
	// proto.DefaultTimeout (5 s).
	Timeout time.Duration
	// ReadyTimeout is the total budget VoltageMv spends waiting for a device
	// that is not ready yet -- one that answers 0 mV, or does not answer at
	// all, because it was only just plugged in (SPEC.md §6.5, §7). Defaults to
	// DefaultReadyTimeout (10 s); a value <= 0 selects that default.
	//
	// It is a budget, not a delay. A device that answers the first read waits
	// zero, so raising it costs nothing on healthy hardware; lower it for a
	// command that should fail fast, or to keep a test from sleeping.
	ReadyTimeout time.Duration
	// Trace, when non-nil, is called with every protocol frame that crosses the
	// session boundary. dir is "tx" or "rx". The slice is a private copy and
	// may be retained. Received frames are traced even when they are dropped
	// for a command-code mismatch, since that is precisely the case an operator
	// needs to see.
	Trace func(dir string, frame []byte)
}

// Session owns a framer and serialises all device access through it.
type Session struct {
	fr           *framer.Framer
	timeout      time.Duration
	readyTimeout time.Duration
	traceFn      func(dir string, frame []byte)

	// sem is a one-slot semaphore rather than a sync.Mutex so that a caller
	// waiting its turn can still be cancelled through its context.
	sem chan struct{}

	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

// New builds a Session over t and starts the framer's receive loop. The caller
// must Close the Session, which closes the framer and the transport.
func New(t proto.Transport, opts Options) *Session {
	byteDelay := opts.ByteDelay
	if byteDelay <= 0 {
		byteDelay = proto.ByteDelay
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = proto.DefaultTimeout
	}
	readyTimeout := opts.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = DefaultReadyTimeout
	}
	s := &Session{
		fr:           framer.New(t, byteDelay),
		timeout:      timeout,
		readyTimeout: readyTimeout,
		traceFn:      opts.Trace,
		sem:          make(chan struct{}, 1),
	}
	s.fr.Start()
	return s
}

// Close shuts the session down. It is safe to call more than once and safe to
// call concurrently with an in-flight command, which will then fail with
// ErrTransportClosed rather than waiting out its timeout.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.closeErr = s.fr.Close()
	})
	return s.closeErr
}

// Do issues one command and waits for the matching response.
//
// The returned frame's payload is a private copy and may be retained.
func (s *Session) Do(ctx context.Context, cmd proto.Cmd, payload []byte, write bool) (proto.Frame, error) {
	return s.DoRaw(ctx, cmd, payload, write, false)
}

// DoRaw is Do with control over the scratchpad flag.
//
// FlagScratchpad (0x40) is never set by the shipped vendor application and its
// volatile-versus-committed meaning is undetermined (SPEC.md §5.1, §14.4). It
// is exposed here only so a raw escape hatch can reach it; nothing in this
// package sets it.
func (s *Session) DoRaw(ctx context.Context, cmd proto.Cmd, payload []byte, write, scratchpad bool) (proto.Frame, error) {
	return s.exchange(ctx, cmd, payload, write, scratchpad, s.timeout)
}

// SendNoACK writes a pre-built frame and returns without waiting for any
// response. It still takes the single-flight lock, so it cannot interleave with
// a command that is awaiting a response.
//
// This exists for the one command that never answers: the jump to the
// bootloader, after which the device disconnects immediately (SPEC.md §6.1).
func (s *Session) SendNoACK(ctx context.Context, frame []byte) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()

	s.drainStale()
	s.trace("tx", frame)
	if err := s.fr.SendFrame(ctx, frame); err != nil {
		return fmt.Errorf("send frame %s: %w", proto.Hex(frame), err)
	}
	return nil
}

// exchange is the single point through which every command passes.
func (s *Session) exchange(ctx context.Context, cmd proto.Cmd, payload []byte, write, scratchpad bool, timeout time.Duration) (proto.Frame, error) {
	tx, err := proto.Build(cmd, payload, write, scratchpad)
	if err != nil {
		return proto.Frame{}, fmt.Errorf("%s: %w", cmd, err)
	}
	// proto.Build enforces only the length-byte ceiling, because the bootloader's
	// bulk path legitimately exceeds 64 bytes. On this path it would not: the
	// device's MIDI receive state machine drops an oversize frame without a
	// diagnostic, so sending one buys nothing but a timeout.
	if !proto.FitsMIDI(tx) {
		return proto.Frame{}, fmt.Errorf("%s: frame is %d bytes, which the device's MIDI receive path drops (maximum %d): %w",
			cmd, len(tx), proto.MaxFrameLen, proto.ErrPayloadTooLong)
	}
	if err := s.acquire(ctx); err != nil {
		return proto.Frame{}, fmt.Errorf("%s: %w", cmd, err)
	}
	defer s.release()

	// Anything sitting in the receive channel now arrived while no command was
	// outstanding: a late answer to a command that already timed out, or an
	// unsolicited frame. The reference client logs these as
	// "unexpected_frame_while_waiting" and drops them; dropping them here as
	// well keeps a stale frame from satisfying this command by accident.
	s.drainStale()

	s.trace("tx", tx)
	if err := s.fr.SendFrame(ctx, tx); err != nil {
		// The vendor's transport swallows send failures and lets them surface
		// as a timeout five seconds later (SPEC.md §7). Report them directly.
		return proto.Frame{}, fmt.Errorf("%s: send failed (tx %s): %w", cmd, proto.Hex(tx), err)
	}
	return s.await(ctx, cmd, tx, timeout)
}

// await blocks until a frame carrying cmd arrives, the deadline expires, or the
// context is cancelled.
func (s *Session) await(ctx context.Context, cmd proto.Cmd, tx []byte, timeout time.Duration) (proto.Frame, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	frames := s.fr.Frames()
	errs := s.fr.Errors()

	// Transport-level problems (a malformed frame dropped by the receive state
	// machine, a short read) are informational: the protocol has no NACK, so a
	// command is only ever failed by its deadline. The most recent one is kept
	// so the eventual timeout can name a probable cause.
	var lastTransportErr error

	for {
		select {
		case <-ctx.Done():
			return proto.Frame{}, fmt.Errorf("%s: %w (tx %s)", cmd, ctx.Err(), proto.Hex(tx))

		case <-timer.C:
			// The device may still answer this command after we have given up
			// on it. Because the protocol carries no tag or sequence number,
			// such a frame is indistinguishable from the answer to the NEXT
			// command with the same code -- and the codes that get issued
			// back-to-back are exactly the dangerous ones (18 for the voltage
			// write/read-back, 23 for the vlimit read/write/read-back). A
			// non-blocking drain before the next send cannot catch it, because
			// the frame may still be arriving while that send is in progress.
			//
			// So absorb the late answer here, on the already-failed path, where
			// spending a moment costs nothing.
			s.drainFor(ctx, settleAfterTimeout)
			if lastTransportErr != nil {
				return proto.Frame{}, fmt.Errorf("%s: %w after %s (tx %s); last transport error: %v",
					cmd, ErrTimeout, timeout, proto.Hex(tx), lastTransportErr)
			}
			return proto.Frame{}, fmt.Errorf("%s: %w after %s (tx %s)", cmd, ErrTimeout, timeout, proto.Hex(tx))

		case err, ok := <-errs:
			if !ok {
				// Nil-ing the channel parks this case forever; a closed
				// channel would otherwise spin the select.
				errs = nil
				continue
			}
			if err != nil {
				lastTransportErr = err
			}

		case raw, ok := <-frames:
			if !ok {
				// The framer's reader publishes its terminal error on errs and
				// only then closes both channels, so the cause is already
				// buffered and this select may simply have picked the closed
				// frames channel first. Take it before reporting -- and prefer
				// it to anything already in lastTransportErr, which may be a
				// merely informational parse error from an earlier frame.
				//
				// "transport closed" alone tells a user nothing; the wrapped
				// cause is what says the device was unplugged (ENODEV, EOF)
				// rather than that something generic went wrong.
				if errs != nil {
					select {
					case err, ok := <-errs:
						if ok && err != nil {
							lastTransportErr = err
						}
					default:
					}
				}
				if lastTransportErr != nil {
					return proto.Frame{}, fmt.Errorf("%s: %w (tx %s): %w",
						cmd, ErrTransportClosed, proto.Hex(tx), lastTransportErr)
				}
				return proto.Frame{}, fmt.Errorf("%s: %w (tx %s)", cmd, ErrTransportClosed, proto.Hex(tx))
			}
			s.trace("rx", raw)

			f, err := proto.Parse(raw)
			if err != nil {
				// Fewer than two bytes: ignored without clearing the pending
				// state, so the command can still be satisfied (SPEC.md §5.2).
				lastTransportErr = err
				continue
			}
			if f.Cmd != cmd {
				// "ack_cmd_mismatch": drop the frame but leave the wait
				// pending. Flag bits were already masked off by proto.Parse and
				// are never inspected.
				continue
			}
			// Copy: the payload aliases the framer's receive buffer.
			f.Payload = append([]byte(nil), f.Payload...)
			return f, nil
		}
	}
}

// acquire takes the single-flight lock, honouring ctx while queued.
func (s *Session) acquire(ctx context.Context) error {
	if s.closed.Load() {
		return ErrNoConnection
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.sem <- struct{}{}:
	}
	if s.closed.Load() {
		s.release()
		return ErrNoConnection
	}
	return nil
}

func (s *Session) release() { <-s.sem }

// trace hands a private copy of frame to the trace hook, if one is installed.
func (s *Session) trace(dir string, frame []byte) {
	if s.traceFn == nil {
		return
	}
	s.traceFn(dir, append([]byte(nil), frame...))
}

// settleAfterTimeout is how long exchange keeps absorbing frames after a
// command has timed out, so that a late answer cannot be mistaken for the
// answer to the next command with the same code. It is short relative to the
// 5 s timeout that preceded it and only ever runs on a path that has already
// failed.
const settleAfterTimeout = 400 * time.Millisecond

// Draining, and why a closed channel is not a reason to stop.
//
// Both drains below watch two channels, and the framer closes them one after
// the other when its reader exits, so for a moment one is closed while the
// other still holds frames. A receive from a closed channel is *always* ready,
// so the select then chooses at random between "the closed channel" and "a
// frame that is genuinely queued", and returning on the first !ok leaves
// whatever was queued on the other one in place. That is wrong twice over: a
// closed channel yields its buffered values before it ever reports !ok, so !ok
// means only that THIS channel is exhausted -- and what stays behind is exactly
// the stale answer these drains exist to remove. The protocol has no tag and no
// sequence number (SPEC.md §5.2, §5.3), so a leftover frame whose command code
// matches becomes the next command's answer, and the codes issued back-to-back
// are the dangerous ones: 18 for the voltage write and its read-back, 23 for
// vlimit.
//
// So each channel is nil-ed as it is exhausted, which parks its case forever,
// and draining continues until nothing is left anywhere.

// drainFor absorbs and discards frames for at most d, returning early if both
// channels are exhausted or ctx is cancelled. Only call this while holding the
// single-flight lock.
func (s *Session) drainFor(ctx context.Context, d time.Duration) {
	frames := s.fr.Frames()
	errs := s.fr.Errors()
	timer := time.NewTimer(d)
	defer timer.Stop()
	for frames != nil || errs != nil {
		select {
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		case raw, ok := <-frames:
			if !ok {
				frames = nil
				continue
			}
			s.trace("rx-late", raw)
		case _, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
		}
	}
	// Both channels are closed and empty: nothing can arrive later, so there is
	// no point sitting out the rest of d.
}

// drainStale empties the framer's channels without blocking. Only call this
// while holding the single-flight lock.
func (s *Session) drainStale() {
	frames := s.fr.Frames()
	errs := s.fr.Errors()
	for frames != nil || errs != nil {
		select {
		case raw, ok := <-frames:
			if !ok {
				frames = nil
				continue
			}
			s.trace("rx", raw)
		case _, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
		default:
			return
		}
	}
}

// sleepCtx waits for d and reports why it stopped: nil once the full duration
// has elapsed, ctx.Err() if the context ended first.
//
// A non-positive d is NOT a no-op: it returns ctx.Err(), so the call remains a
// cancellation checkpoint rather than a way to skip one. Both callers sit
// inside retry loops -- VoltageMv's backoff, which clips its last wait to the
// remaining budget, and the PDO chunk retry delay -- where a wait that
// collapsed to zero must not also swallow the operator's Ctrl-C for another
// round trip. internal/cli and internal/bootloader carry their own copies of
// this helper; keep this one's semantics if they are ever unified.
func sleepCtx(ctx context.Context, d time.Duration) error {
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
