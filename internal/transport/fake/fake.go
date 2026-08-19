// Package fake provides an in-memory VFLEX stand-in for tests.
//
// A Device speaks the real wire protocol over an in-memory pipe. It accepts the
// MIDI byte stream package framer produces, decodes it with its own
// implementation of the device-side receive state machine (SPEC.md §3.3),
// dispatches each decoded frame to a handler, and encodes the reply back into
// the nibble MIDI representation (SPEC.md §3.1) where the host can read it.
//
// Typical use:
//
//	dev := fake.NewTypical()
//	defer dev.Close()
//	s := session.New(dev.Transport(), session.Options{})
//	serial, err := s.SerialNumber(ctx)   // "VF001234"
//
// A Device is safe for concurrent use: the framer reads from one goroutine
// while the test writes and reconfigures from another. Handlers run without any
// Device lock held, so a handler may call back into the Device it belongs to.
package fake

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/jzbz/gflex/internal/proto"
)

// ErrClosed is returned by writes to a Device that has been closed, and by
// Push/PushMIDI once the Device is closed or unplugged.
var ErrClosed = errors.New("fake: device is closed")

// ErrUnplugged is the read error Unplug installs when it is handed a nil
// error. Tests that want the host to see a specific cause (an ENODEV string,
// say) pass their own.
var ErrUnplugged = errors.New("fake: device unplugged")

// TransportName is what the Device's transport reports from Name().
const TransportName = "fake:vflex"

// Fault injects a defect into the response to a command, so tests can exercise
// the error paths that real hardware only produces when something is wrong. The
// zero Fault is well-behaved and changes nothing.
//
// A Fault is applied after the payload has been produced, so it composes with
// handlers and canned responses alike.
type Fault struct {
	// Drop suppresses the response entirely. The command then times out, which
	// is the only failure mode the protocol itself has: there is no NACK and no
	// device-reported error anywhere (SPEC.md §5.2).
	Drop bool

	// Delay holds the response back for this long before it becomes readable.
	// Use it to sit either side of a session timeout.
	Delay time.Duration

	// Mismatch replaces the response's command code with MismatchCmd, keeping
	// the flag bits of the request. A host must log this and keep waiting
	// rather than treat it as a hard error (SPEC.md §5.2).
	Mismatch    bool
	MismatchCmd proto.Cmd

	// BadLength overwrites the response frame's length byte with LengthByte
	// after the frame is built. Values below 2, above 64, or larger than the
	// bytes actually sent are dropped by a correct receiver.
	BadLength  bool
	LengthByte byte
}

// Device is an in-memory VFLEX.
type Device struct {
	mu        sync.Mutex
	cond      *sync.Cond // signals rx, closed or unplugged
	closed    bool
	unplugErr error // non-nil once Unplug has run; see Unplug for semantics

	rx   []byte   // device -> host bytes waiting to be read
	sent [][]byte // host -> device frames, in arrival order
	dec  *frameDecoder

	handlers  map[proto.Cmd]func(proto.Frame) []byte
	responses map[proto.Cmd][]byte
	faults    map[proto.Cmd]Fault
	global    Fault
	def       func(proto.Frame) []byte

	timers   map[uint64]*time.Timer // pending delayed responses
	timerSeq uint64

	// regMu guards registers. It is deliberately separate from mu so that a
	// register handler, which runs while mu is not held, can never deadlock
	// against the dispatch path.
	regMu     sync.Mutex
	registers map[proto.Cmd][]byte

	tr *transport
}

// New returns a Device with no handlers registered.
//
// Its default behaviour is to echo: every request is answered with a frame
// carrying the same command code, the same write flag, and the request's own
// payload as the response payload. Override that with SetDefault, or answer
// individual commands with SetResponse or SetHandler. For a device that behaves
// like real hardware, use NewTypical.
func New() *Device {
	d := &Device{
		dec:       newFrameDecoder(),
		handlers:  make(map[proto.Cmd]func(proto.Frame) []byte),
		responses: make(map[proto.Cmd][]byte),
		faults:    make(map[proto.Cmd]Fault),
		timers:    make(map[uint64]*time.Timer),
		registers: make(map[proto.Cmd][]byte),
		def:       EchoPayload,
	}
	d.cond = sync.NewCond(&d.mu)
	d.tr = &transport{d: d}
	return d
}

// EchoPayload is the default responder: it answers with the request's own
// payload, which for a read means an empty payload.
//
// The result is never nil even for an empty payload, because nil is the
// responder convention for "do not answer".
func EchoPayload(f proto.Frame) []byte {
	return append([]byte{}, f.Payload...)
}

// Transport returns the Device's proto.Transport view. The same instance is
// returned on every call; closing it closes the Device.
func (d *Device) Transport() proto.Transport { return d.tr }

// SetResponse registers a canned response payload for cmd. The Device replies
// with a frame carrying cmd, the request's write flag, and this payload.
//
// A nil payload registers an empty payload, i.e. a bare two-byte
// acknowledgement. To make the Device not answer at all, use
// SetFault(cmd, Fault{Drop: true}).
func (d *Device) SetResponse(cmd proto.Cmd, payload []byte) {
	p := append([]byte{}, payload...)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.responses[cmd] = p
}

// SetHandler registers a handler for cmd, taking precedence over any canned
// response. The handler receives the decoded request and returns the response
// payload.
//
// Returning nil means "do not answer", which models a command the firmware
// ignores; return an empty non-nil slice for a two-byte acknowledgement.
// Passing a nil handler removes the registration. Handlers run on the writer's
// goroutine with no Device lock held.
func (d *Device) SetHandler(cmd proto.Cmd, h func(f proto.Frame) []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h == nil {
		delete(d.handlers, cmd)
		return
	}
	d.handlers[cmd] = h
}

// SetDefault replaces the responder used for commands with neither a handler
// nor a canned response. A nil argument makes unregistered commands silent,
// which is how a real device behaves for commands its firmware does not
// implement.
func (d *Device) SetDefault(h func(f proto.Frame) []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.def = h
}

// SetFault installs a per-command Fault, overriding any global fault for that
// command. Pass the zero Fault to make the command well-behaved again.
func (d *Device) SetFault(cmd proto.Cmd, f Fault) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.faults[cmd] = f
}

// SetGlobalFault installs a Fault applied to every command that has no
// per-command Fault of its own.
func (d *Device) SetGlobalFault(f Fault) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.global = f
}

// ClearFaults removes the global fault and every per-command fault.
func (d *Device) ClearFaults() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.global = Fault{}
	clear(d.faults)
}

// ---------------------------------------------------------------------------
// Registers: settable values that survive a write and are returned by reads.
// ---------------------------------------------------------------------------

// SetRegister makes cmd behave like a stored setting: a read returns the
// current bytes, a write replaces them and echoes the new value back. This is
// what most of the VFLEX's read/write commands do, and it is what makes a
// write-then-read-back sequence return the value that was written.
func (d *Device) SetRegister(cmd proto.Cmd, initial []byte) {
	d.StoreRegister(cmd, initial)
	d.SetHandler(cmd, func(f proto.Frame) []byte {
		if f.Write {
			d.StoreRegister(cmd, f.Payload)
		}
		v, _ := d.Register(cmd)
		return v
	})
}

// StoreRegister sets the bytes stored for cmd without going through the wire,
// seeding or forcing device state from a test.
func (d *Device) StoreRegister(cmd proto.Cmd, v []byte) {
	c := append([]byte{}, v...)
	d.regMu.Lock()
	defer d.regMu.Unlock()
	d.registers[cmd] = c
}

// Register returns a copy of the bytes stored for cmd, and whether cmd has any
// register at all. Use it to assert what a write actually landed on the device.
func (d *Device) Register(cmd proto.Cmd) ([]byte, bool) {
	d.regMu.Lock()
	defer d.regMu.Unlock()
	v, ok := d.registers[cmd]
	if !ok {
		return nil, false
	}
	return append([]byte{}, v...), true
}

// ---------------------------------------------------------------------------
// Observation and injection.
// ---------------------------------------------------------------------------

// Sent returns a copy of every request frame the Device has decoded, in order,
// so a test can assert the exact bytes transmitted. Frames the receive state
// machine dropped as malformed never appear here.
func (d *Device) Sent() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][]byte, len(d.sent))
	for i, f := range d.sent {
		out[i] = append([]byte(nil), f...)
	}
	return out
}

// SentHex renders Sent in proto.Hex form ("04 92 2e e0"), one string per
// frame, for table-driven comparisons against expected wire traffic.
func (d *Device) SentHex() []string {
	frames := d.Sent()
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = proto.Hex(f)
	}
	return out
}

// ClearSent discards the recorded request frames.
func (d *Device) ClearSent() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = nil
}

// Push makes frame readable by the host as if the device had sent it
// unprompted, without any request having arrived. Use it to exercise the host's
// unexpected-frame path (SPEC.md §5.2); whether real firmware ever does this is
// undetermined (SPEC.md §14.14).
func (d *Device) Push(frame []byte) error {
	return d.PushMIDI(encodeFrameMIDI(frame))
}

// PushMIDI makes raw MIDI bytes readable by the host verbatim, with no framing
// applied. Use it to feed a host decoder deliberately malformed streams:
// truncated messages, mid-frame start markers, unrelated channel messages.
func (d *Device) PushMIDI(raw []byte) error {
	if !d.enqueue(raw) {
		return ErrClosed
	}
	return nil
}

// Unplug models the cable being yanked mid-session: err (ErrUnplugged when
// nil) becomes the terminal error the host's reader sees, the way a real
// unplug surfaces as ENODEV out of a blocked read.
//
// The semantics are chosen for what hot-unplug tests need, and each side is
// deliberate:
//
//   - Reads first drain any bytes already queued, then fail with err on every
//     subsequent call. Bytes that left the device before the unplug are the
//     kernel buffer's to deliver, so a test can Push responses, Unplug, and
//     know the host will decode all of them and THEN hit the error --
//     deterministically, with no race against the reader goroutine. (This is
//     exactly the ordering a session's stale-frame drains have to survive.)
//   - Writes keep succeeding, and the frames they complete are still decoded
//     and recorded by Sent -- so a test can assert the host really did
//     transmit a command after the death -- but nothing is dispatched or
//     answered: there is no device left to answer. A real unplug may fail the
//     write as well; that is the transport's own error path, exercised in the
//     transport's tests, and failing writes here would make the
//     send-succeeds-then-no-answer shape impossible to stage.
//   - Push and PushMIDI fail with ErrClosed, and already-scheduled delayed
//     replies are discarded: nothing new can come FROM a device that is gone.
//
// Unplug is idempotent and the first error wins. It is a no-op on a closed
// Device, and Close still works normally afterwards.
func (d *Device) Unplug(err error) {
	if err == nil {
		err = ErrUnplugged
	}
	d.mu.Lock()
	if d.closed || d.unplugErr != nil {
		d.mu.Unlock()
		return
	}
	d.unplugErr = err
	timers := d.timers
	d.timers = make(map[uint64]*time.Timer)
	d.cond.Broadcast()
	d.mu.Unlock()

	// Stop outside the lock, as Close does: a timer callback that has already
	// started is blocked on d.mu, and will find the Device unplugged when its
	// enqueue runs, which refuses.
	for _, t := range timers {
		t.Stop()
	}
}

// Close releases the Device and unblocks any read waiting for data. It is
// idempotent, so closing both the Device and its transport is safe.
func (d *Device) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	timers := d.timers
	d.timers = make(map[uint64]*time.Timer)
	d.cond.Broadcast()
	d.mu.Unlock()

	// Stop outside the lock: a timer callback that has already started is
	// blocked on d.mu, and will find the Device closed and do nothing.
	for _, t := range timers {
		t.Stop()
	}
	return nil
}

// ---------------------------------------------------------------------------
// The transport view.
// ---------------------------------------------------------------------------

// transport adapts Device to proto.Transport. It is a separate type so that
// Device's own API (SetHandler, Sent, ...) stays out of the interface a
// production transport has to satisfy.
type transport struct{ d *Device }

func (t *transport) WriteMIDI(p []byte) error       { return t.d.writeMIDI(p) }
func (t *transport) ReadMIDI(p []byte) (int, error) { return t.d.readMIDI(p) }
func (t *transport) Name() string                   { return TransportName }
func (t *transport) Close() error                   { return t.d.Close() }

// writeMIDI consumes host-to-device MIDI bytes, decoding and dispatching any
// frames they complete. Bytes may arrive in any chunking, including one message
// split across calls.
func (d *Device) writeMIDI(p []byte) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return ErrClosed
	}
	frames := d.dec.feed(p)
	for _, f := range frames {
		// Record a copy: the dispatch path hands these buffers to handlers as
		// proto.Frame.Payload, and a handler that mutates its argument must not
		// be able to rewrite history.
		d.sent = append(d.sent, append([]byte(nil), f...))
	}
	unplugged := d.unplugErr != nil
	d.mu.Unlock()

	if unplugged {
		// Decoded and recorded so a test can see what the host transmitted,
		// but never answered: the device is gone. See Unplug.
		return nil
	}
	// Dispatch with the lock released so handlers can call back into the
	// Device, and so a delayed reply never blocks the writer.
	for _, f := range frames {
		d.dispatch(f)
	}
	return nil
}

// readMIDI returns device-to-host bytes, blocking until at least one is
// available. It returns io.EOF once the Device is closed and drained, so a
// reader goroutine ends cleanly instead of spinning; after Unplug it drains
// what was already queued and then returns the unplug error instead.
func (d *Device) readMIDI(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.rx) == 0 && !d.closed && d.unplugErr == nil {
		d.cond.Wait()
	}
	if len(d.rx) == 0 {
		// The unplug error takes precedence over the close EOF: the unplug is
		// what killed the link, and Close arriving later (a session tearing
		// itself down) must not rewrite the cause the reader reports.
		if d.unplugErr != nil {
			return 0, d.unplugErr
		}
		return 0, io.EOF
	}
	n := copy(p, d.rx)
	if n == len(d.rx) {
		d.rx = d.rx[:0]
	} else {
		d.rx = append(d.rx[:0], d.rx[n:]...)
	}
	return n, nil
}

// dispatch answers one decoded request frame.
func (d *Device) dispatch(raw []byte) {
	f, err := proto.Parse(raw)
	if err != nil {
		return // unreachable: the decoder never emits fewer than 2 bytes
	}

	d.mu.Lock()
	h := d.handlers[f.Cmd]
	canned, hasCanned := d.responses[f.Cmd]
	def := d.def
	fault, ok := d.faults[f.Cmd]
	if !ok {
		fault = d.global
	}
	d.mu.Unlock()

	if fault.Drop {
		return
	}

	var payload []byte
	switch {
	case h != nil:
		payload = h(f)
	case hasCanned:
		payload = canned
	case def != nil:
		payload = def(f)
	default:
		return
	}
	if payload == nil {
		return // the responder declined to answer
	}
	d.reply(f, payload, fault)
}

// reply builds, corrupts if asked to, encodes and queues one response.
func (d *Device) reply(req proto.Frame, payload []byte, fault Fault) {
	if len(payload) > proto.MaxPayloadLen {
		// A real device cannot exceed this either: the receiver drops anything
		// over 64 bytes, so there is no point emitting it.
		payload = payload[:proto.MaxPayloadLen]
	}

	cmd := req.Cmd
	if fault.Mismatch {
		cmd = fault.MismatchCmd
	}
	// Echo the write flag: a write is answered by an echo the host may parse as
	// if it were a read (SPEC.md §5.2). Whether real firmware echoes the flag
	// bits at all is undetermined (SPEC.md §14.13); the host masks them off
	// before comparing, so echoing is the safe choice.
	frame, err := proto.Build(cmd, payload, req.Write, false)
	if err != nil {
		return // unreachable: payload was clamped above
	}
	if fault.BadLength {
		frame[0] = fault.LengthByte
	}
	d.sendAfter(encodeFrameMIDI(frame), fault.Delay)
}

// sendAfter queues MIDI bytes for the host, optionally after a delay.
func (d *Device) sendAfter(midi []byte, delay time.Duration) {
	if delay <= 0 {
		d.enqueue(midi)
		return
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.timerSeq++
	id := d.timerSeq
	// The callback takes d.mu, which we still hold, so it cannot observe
	// d.timers before the assignment below completes.
	d.timers[id] = time.AfterFunc(delay, func() {
		d.mu.Lock()
		delete(d.timers, id)
		d.mu.Unlock()
		d.enqueue(midi)
	})
	d.mu.Unlock()
}

// enqueue appends bytes to the host-readable buffer and wakes any reader.
// It reports false if the Device is closed or unplugged: nothing more can come
// FROM a device in either state.
func (d *Device) enqueue(midi []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.unplugErr != nil {
		return false
	}
	d.rx = append(d.rx, midi...)
	d.cond.Broadcast()
	return true
}
