package session

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
)

// fakeDev is a minimal scripted VFLEX standing in for a real transport.
//
// It deliberately implements the MIDI nibble codec of SPEC.md §3.1 from the
// specification text rather than reusing the framer, so these tests
// cross-check the framer's encoding instead of assuming it. Package
// internal/transport/fake did not exist when this package was written; if it
// is adopted later this double can be deleted wholesale.
type fakeDev struct {
	mu       sync.Mutex
	sent     [][]byte // decoded frames received from the host, in order
	handlers map[proto.Cmd]func(proto.Frame) []byte

	toHost    chan []byte // MIDI byte chunks headed for the host
	closeCh   chan struct{}
	closeOnce sync.Once

	// MIDI receive state. Touched only from WriteMIDI, which the Transport
	// contract guarantees is called from at most one goroutine.
	status byte
	data   []byte
	need   int
	acc    []byte

	// Read-side leftover, touched only from ReadMIDI.
	leftover []byte
}

func newFakeDev() *fakeDev {
	return &fakeDev{
		handlers: make(map[proto.Cmd]func(proto.Frame) []byte),
		toHost:   make(chan []byte, 64),
		closeCh:  make(chan struct{}),
	}
}

// SetHandler installs a responder for cmd. Returning nil sends nothing, which
// is how a command is made to time out.
func (d *fakeDev) SetHandler(cmd proto.Cmd, h func(proto.Frame) []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[cmd] = h
}

// SetPayload makes cmd answer with a fixed payload, flags clear.
func (d *fakeDev) SetPayload(cmd proto.Cmd, payload []byte) {
	d.SetHandler(cmd, func(proto.Frame) []byte {
		return mustBuild(cmd, payload, false)
	})
}

// Sent returns the frames the host has transmitted so far.
func (d *fakeDev) Sent() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][]byte, len(d.sent))
	copy(out, d.sent)
	return out
}

// SentHex renders Sent as hex strings for table comparisons.
func (d *fakeDev) SentHex() []string {
	frames := d.Sent()
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = proto.Hex(f)
	}
	return out
}

// Emit pushes a raw protocol frame to the host, nibble-encoded.
func (d *fakeDev) Emit(frame []byte) {
	select {
	case d.toHost <- encodeTestMIDI(frame):
	case <-d.closeCh:
	default: // channel full: drop rather than deadlock the writer
	}
}

func (d *fakeDev) Name() string { return "fake" }

func (d *fakeDev) Close() error {
	d.closeOnce.Do(func() { close(d.closeCh) })
	return nil
}

// WriteMIDI runs the status-byte-driven parser of SPEC.md §3.3: system realtime
// bytes are ignored anywhere, any status byte resyncs, and data bytes
// accumulate until the message is complete.
func (d *fakeDev) WriteMIDI(p []byte) error {
	var complete [][]byte
	for _, b := range p {
		switch {
		case b >= 0xF8: // system realtime, may interrupt anything
			continue
		case b >= 0x80:
			d.status, d.data, d.need = b, d.data[:0], midiDataBytes(b)
		default:
			if d.status == 0 {
				continue
			}
			d.data = append(d.data, b)
			if len(d.data) < d.need {
				continue
			}
			switch d.status & 0xF0 {
			case 0x80: // start of frame
				d.acc = d.acc[:0]
			case 0x90: // one protocol byte per note-on
				d.acc = append(d.acc, (d.data[0]&0x0F)<<4|(d.data[1]&0x0F))
			case 0xA0: // end of frame
				if len(d.acc) >= proto.PreambleLen {
					complete = append(complete, append([]byte(nil), d.acc...))
				}
				d.acc = d.acc[:0]
			}
			d.data = d.data[:0]
		}
	}
	for _, frame := range complete {
		d.dispatch(frame)
	}
	return nil
}

func (d *fakeDev) dispatch(raw []byte) {
	d.mu.Lock()
	d.sent = append(d.sent, raw)
	f, err := proto.Parse(raw)
	var h func(proto.Frame) []byte
	if err == nil {
		h = d.handlers[f.Cmd]
	}
	d.mu.Unlock()

	if h == nil {
		return
	}
	if resp := h(f); resp != nil {
		d.Emit(resp)
	}
}

func (d *fakeDev) ReadMIDI(p []byte) (int, error) {
	for len(d.leftover) == 0 {
		select {
		case chunk := <-d.toHost:
			d.leftover = chunk
		case <-d.closeCh:
			return 0, io.EOF
		}
	}
	n := copy(p, d.leftover)
	d.leftover = d.leftover[n:]
	return n, nil
}

// midiDataBytes reports how many data bytes follow a status byte.
func midiDataBytes(status byte) int {
	switch status & 0xF0 {
	case 0xC0, 0xD0:
		return 1
	case 0xF0:
		return 0
	default:
		return 2
	}
}

// encodeTestMIDI implements SPEC.md §3.1 straight from the specification:
// Note Off start marker, one Note On per protocol byte carrying the high nibble
// as the note and the low nibble as the velocity, Poly Key Pressure end marker.
func encodeTestMIDI(frame []byte) []byte {
	out := make([]byte, 0, 3*(len(frame)+2))
	out = append(out, 0x80, 0x00, 0x00)
	for _, b := range frame {
		out = append(out, 0x90, (b>>4)&0x0F, b&0x0F)
	}
	out = append(out, 0xA0, 0x00, 0x00)
	return out
}

func mustBuild(cmd proto.Cmd, payload []byte, write bool) []byte {
	f, err := proto.Build(cmd, payload, write, false)
	if err != nil {
		panic(err)
	}
	return f
}

// hotplugDev is a transport that answers nothing and whose read fails with a
// scripted error the moment Unplug is called: a device pulled out of the port
// mid-command. wrote reports that the host has put a MIDI message on the wire,
// so a test can unplug at a deterministic point rather than by sleeping.
type hotplugDev struct {
	fail      chan error
	wrote     chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func newHotplugDev() *hotplugDev {
	return &hotplugDev{
		fail:   make(chan error, 1),
		wrote:  make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
}

// Unplug makes the pending ReadMIDI fail with err.
func (d *hotplugDev) Unplug(err error) { d.fail <- err }

func (d *hotplugDev) Name() string { return "hotplug" }

func (d *hotplugDev) WriteMIDI([]byte) error {
	select {
	case d.wrote <- struct{}{}:
	default:
	}
	return nil
}

func (d *hotplugDev) ReadMIDI(p []byte) (int, error) {
	select {
	case err := <-d.fail:
		return 0, err
	case <-d.closed:
		return 0, io.EOF
	}
}

func (d *hotplugDev) Close() error {
	d.closeOnce.Do(func() { close(d.closed) })
	return nil
}

// newTestSession wires a Session to a fresh fakeDev. ByteDelay is set to a
// single nanosecond so the suite does not pay the 20 ms per MIDI message the
// vendor client uses on real hardware.
func newTestSession(t *testing.T, opts Options) (*Session, *fakeDev) {
	t.Helper()
	d := newFakeDev()
	if opts.ByteDelay == 0 {
		opts.ByteDelay = time.Nanosecond
	}
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Second
	}
	s := New(d, opts)
	t.Cleanup(func() { _ = s.Close() })
	return s, d
}
