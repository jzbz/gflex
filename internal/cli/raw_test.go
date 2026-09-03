package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/transport/fake"
)

// oversizeVoltageWrite builds a well-formed frame one byte past the ceiling the
// MIDI receive state machine enforces: byte[0] declares the whole 65 bytes and
// byte[1] is a CMD_VOLTAGE_MV write, so nothing but the total length is wrong
// with it.
func oversizeVoltageWrite() []byte {
	raw := make([]byte, proto.MaxFrameLen+1)
	raw[0] = byte(len(raw))
	raw[1] = 0x92
	return raw
}

// TestRawRefusesAFrameTheReceiverDropsBeforeOpeningTheDevice covers the gate
// `raw` did not have.
//
// The device discards any frame over proto.MaxFrameLen (SPEC.md §3.3), so
// waiting for a response to one can only time out. session.exchange knew that
// and refused -- but only after app.connect had claimed the device, and on
// --transport usb claiming it detaches snd-usb-audio, which on at least one
// kernel costs the ALSA MIDI port until the unit is replugged (SPEC.md §4.2).
// A whole physical replug for a frame that was never going to be sent, and
// reported as a generic failure, while the sibling length-byte mistake one gate
// up is a usage error caught on the command line.
func TestRawRefusesAFrameTheReceiverDropsBeforeOpeningTheDevice(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)
	opened := 0
	open := tr.app.testTransport
	tr.app.testTransport = func(ctx context.Context) (proto.Transport, string, error) {
		opened++
		return open(ctx)
	}

	raw := oversizeVoltageWrite()
	err := tr.run(t, "raw", proto.Hex(raw), "--yes")
	if err == nil {
		t.Fatalf("a %d-byte frame was accepted on the acknowledged path", len(raw))
	}
	if code := ExitCode(err); code != ExitUsage {
		t.Errorf("ExitCode = %d, want ExitUsage (%d): %v", code, ExitUsage, err)
	}
	if !strings.Contains(err.Error(), "--no-ack") {
		t.Errorf("the refusal does not name the way through:\n%v", err)
	}
	if opened != 0 {
		t.Errorf("the device was opened %d time(s) for a frame that could never be sent", opened)
	}
	if frames := dev.Sent(); len(frames) != 0 {
		t.Errorf("the device was asked %v", cmdNames(frames))
	}
}

// The same frame with --no-ack still goes out. Sending one the receiver is
// known to drop is a legitimate probe -- the firmware's real receive ceiling is
// one of the open questions `raw` exists for -- so the answer there is a
// warning, not a refusal: the command reported `sent` and exit 0 with nothing
// saying the frame had been discarded on arrival.
func TestRawOversizeFrameIsWarnedAboutRatherThanRefusedWithNoACK(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)

	raw := oversizeVoltageWrite()
	if err := tr.run(t, "raw", proto.Hex(raw), "--yes", "--no-ack"); err != nil {
		t.Fatalf("`raw <oversize> --yes --no-ack`: %v", err)
	}
	got := tr.stderr.String()
	if !strings.Contains(got, "warning") || !strings.Contains(got, "drops anything over") {
		t.Errorf("nothing said the device would discard the frame:\n%s", got)
	}
}

// A frame AT the ceiling is not over it: it goes out with nothing said. Without
// this the gate could refuse or warn about everything and the two tests above
// would pass for the wrong reason.
func TestRawSendsAFrameAtTheCeilingUnwarned(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)

	raw := make([]byte, proto.MaxFrameLen)
	raw[0] = byte(len(raw))
	raw[1] = 0x92
	if err := tr.run(t, "raw", proto.Hex(raw), "--yes", "--no-ack"); err != nil {
		t.Fatalf("a %d-byte frame, the largest the receiver takes: %v", len(raw), err)
	}
	if strings.Contains(tr.stderr.String(), "drops anything over") {
		t.Errorf("a frame at the ceiling was reported as being over it:\n%s", tr.stderr.String())
	}
	if !tr.sent(t, proto.CmdVoltageMv) {
		t.Errorf("the frame never reached the device; frames: %v", cmdNames(dev.Sent()))
	}
}
