package session

import (
	"fmt"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/transport/fake"
)

// The scripted VFLEX these tests run against is transport/fake.Device, which
// speaks the real wire protocol over an in-memory pipe with its own,
// framer-independent implementation of the device-side MIDI receive machine
// (SPEC.md §3.1, §3.3). This package once carried a private double of all
// that; it was deleted when fake was adopted, so the device model is
// maintained in exactly one place and a framing change there is exercised by
// these tests automatically.
//
// Porting notes, for reading the tests below against fake's API:
//
//   - fake handlers return response PAYLOADS and the Device builds the frame
//     (echoing the request's command and write flag, which the host masks
//     off, SPEC.md §5.2). A test that needs a frame the Device would never
//     build -- a mismatched command code, flag bits set, a sub-preamble runt
//     -- injects it with Device.Push (or stages a fake.Fault) instead of
//     returning it.
//   - unscripted commands are made silent (SetDefault(nil)) to match real
//     firmware ignoring a command, so unanswered-command tests time out
//     honestly rather than being answered by fake's echo default.
//   - hot-unplug is Device.Unplug, whose contract (queued bytes drain first,
//     writes still transmit and are recorded but nothing answers) is pinned
//     by fake's own tests.

// newTestSession wires a Session to a fresh, silent fake.Device. ByteDelay is
// set to a single nanosecond so the suite pays no pacing per MIDI message --
// neither the 1 ms default nor the 20 ms the vendor client uses on real
// hardware. A fake device needs none of it: the pacing exists for the wire.
func newTestSession(t *testing.T, opts Options) (*Session, *fake.Device) {
	t.Helper()
	d := fake.New()
	d.SetDefault(nil)
	if opts.ByteDelay == 0 {
		opts.ByteDelay = time.Nanosecond
	}
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Second
	}
	s := New(d.Transport(), opts)
	t.Cleanup(func() { _ = s.Close() })
	return s, d
}

// enodevWrites wraps a Device's transport with the one thing fake.Device
// deliberately will not do: fail the WRITE after the device has gone.
//
// Its Unplug doc says so in terms -- "a real unplug may fail the write as well;
// that is the transport's own error path" -- because a fake that failed writes
// could not stage the send-succeeds-then-no-answer shape the other unplug tests
// need. But the send leg is a distinct classification problem: the framer gates
// SendFrame on its own done channel, which a reader dying from a transport
// error never closes, so the next command is still transmitted into a port that
// is gone. On real hardware the kernel has already swapped in the disconnected
// file operations by then and rawmidi returns the errno verbatim
// (`rawmidi: write %s: %w`), which is exactly what this reproduces.
type enodevWrites struct {
	proto.Transport
	dead atomic.Bool
}

// unplug makes every subsequent WriteMIDI fail with ENODEV. Reads are left
// alone: a test that wants those to fail too calls fake.Device.Unplug as well.
func (t *enodevWrites) unplug() { t.dead.Store(true) }

func (t *enodevWrites) WriteMIDI(p []byte) error {
	if t.dead.Load() {
		return fmt.Errorf("rawmidi: write /dev/snd/midiC1D0: %w", syscall.ENODEV)
	}
	return t.Transport.WriteMIDI(p)
}

// newFailingWriteSession is newTestSession with the transport above spliced in,
// so a test can kill the send leg at a chosen moment.
func newFailingWriteSession(t *testing.T, opts Options) (*Session, *fake.Device, *enodevWrites) {
	t.Helper()
	d := fake.New()
	d.SetDefault(nil)
	tr := &enodevWrites{Transport: d.Transport()}
	if opts.ByteDelay == 0 {
		opts.ByteDelay = time.Nanosecond
	}
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Second
	}
	s := New(tr, opts)
	t.Cleanup(func() { _ = s.Close() })
	return s, d, tr
}

// mustBuild builds a raw protocol frame for injection via Device.Push and for
// the byte-level expectations in table tests.
func mustBuild(cmd proto.Cmd, payload []byte, write bool) []byte {
	f, err := proto.Build(cmd, payload, write, false)
	if err != nil {
		panic(err)
	}
	return f
}
