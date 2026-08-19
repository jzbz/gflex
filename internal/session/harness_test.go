package session

import (
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
// set to a single nanosecond so the suite does not pay the 20 ms per MIDI
// message the vendor client uses on real hardware.
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

// mustBuild builds a raw protocol frame for injection via Device.Push and for
// the byte-level expectations in table tests.
func mustBuild(cmd proto.Cmd, payload []byte, write bool) []byte {
	f, err := proto.Build(cmd, payload, write, false)
	if err != nil {
		panic(err)
	}
	return f
}
