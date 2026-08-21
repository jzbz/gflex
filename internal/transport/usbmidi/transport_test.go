package usbmidi

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/usbfs"
)

// fakeDev implements usbDevice. Reads are served from a queue of scripted
// results; writes are recorded.
type fakeDev struct {
	reads  []readResult
	writes [][]byte

	released  []int
	closed    int
	releaseIn error
	closeErr  error

	// order records release/close sequencing, which matters: releasing is what
	// makes snd-usb-audio rebind.
	order []string
}

type readResult struct {
	data []byte
	err  error
}

func (f *fakeDev) Transfer(ctx context.Context, endpoint uint8, data []byte, timeout time.Duration) (int, error) {
	if endpoint&0x80 != 0 {
		if len(f.reads) == 0 {
			return 0, syscall.ETIMEDOUT
		}
		r := f.reads[0]
		f.reads = f.reads[1:]
		if r.err != nil {
			return 0, r.err
		}
		return copy(data, r.data), nil
	}
	f.writes = append(f.writes, append([]byte(nil), data...))
	return len(data), nil
}

func (f *fakeDev) ReleaseInterface(num int) error {
	f.released = append(f.released, num)
	f.order = append(f.order, "release")
	return f.releaseIn
}

func (f *fakeDev) Close() error {
	f.closed++
	f.order = append(f.order, "close")
	return f.closeErr
}

func newTestTransport(dev usbDevice, mps uint16) *transport {
	iface := usbfs.Interface{Number: 1, Class: classAudio, SubClass: subClassMIDIStreaming}
	ref := usbfs.DeviceRef{Path: "/dev/bus/usb/001/007", Bus: 1, Addr: 7, VendorID: proto.VendorID, ProductID: 0x800F}
	return newTransport(dev,
		ref, iface,
		ep(0x81, attrBulk, mps), ep(0x01, attrBulk, mps),
		Options{ReadTimeout: 5 * time.Millisecond}.withDefaults())
}

func TestName(t *testing.T) {
	tr := newTestTransport(&fakeDev{}, 64)
	want := "usb 37bf:800f bus 1 addr 7 iface 1"
	if got := tr.Name(); got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
}

func TestWriteMIDIPacksAndBatches(t *testing.T) {
	f := &fakeDev{}
	// wMaxPacketSize 16 holds exactly four event packets, so a six-message
	// frame must go out as 16 bytes then 8.
	tr := newTestTransport(f, 16)

	frame := []byte{0x04, 0x92, 0x2E, 0xE0} // set voltage 12.000 V, SPEC.md §15
	if err := tr.WriteMIDI(nibbleEncode(frame)); err != nil {
		t.Fatalf("WriteMIDI: %v", err)
	}
	if len(f.writes) != 2 {
		t.Fatalf("got %d transfers, want 2: %v", len(f.writes), f.writes)
	}
	if len(f.writes[0]) != 16 || len(f.writes[1]) != 8 {
		t.Fatalf("transfer sizes %d/%d, want 16/8", len(f.writes[0]), len(f.writes[1]))
	}
	got := append(append([]byte{}, f.writes[0]...), f.writes[1]...)
	want := []byte{
		0x08, 0x80, 0x00, 0x00,
		0x09, 0x90, 0x00, 0x04,
		0x09, 0x90, 0x09, 0x02,
		0x09, 0x90, 0x02, 0x0E,
		0x09, 0x90, 0x0E, 0x00,
		0x0A, 0xA0, 0x00, 0x00,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("wire bytes = %s, want %s", proto.Hex(got), proto.Hex(want))
	}
}

func TestWriteMIDISingleTransferWhenItFits(t *testing.T) {
	f := &fakeDev{}
	tr := newTestTransport(f, 64)
	if err := tr.WriteMIDI(nibbleEncode([]byte{0x02, 0x08})); err != nil {
		t.Fatalf("WriteMIDI: %v", err)
	}
	if len(f.writes) != 1 || len(f.writes[0]) != 16 {
		t.Fatalf("got %d transfers %v, want one of 16 bytes", len(f.writes), f.writes)
	}
	// The transmit buffer is reused across calls; a second write must not
	// resend the first one's packets.
	if err := tr.WriteMIDI([]byte{0xA0, 0x00, 0x00}); err != nil {
		t.Fatalf("WriteMIDI: %v", err)
	}
	if len(f.writes) != 2 || !bytes.Equal(f.writes[1], []byte{0x0A, 0xA0, 0x00, 0x00}) {
		t.Fatalf("second transfer = %v", f.writes[1:])
	}
}

func TestWriteMIDIRejectsBadInput(t *testing.T) {
	f := &fakeDev{}
	tr := newTestTransport(f, 64)
	if err := tr.WriteMIDI([]byte{0x90, 0x00}); err == nil {
		t.Fatal("expected an error for an incomplete message")
	}
	if err := tr.WriteMIDI([]byte{0xF0, 0x01, 0xF7}); err == nil {
		t.Fatal("expected an error for SysEx")
	}
	if len(f.writes) != 0 {
		t.Fatalf("a rejected write still reached the device: %v", f.writes)
	}
	// Nothing to send is not an error.
	if err := tr.WriteMIDI(nil); err != nil {
		t.Fatalf("WriteMIDI(nil): %v", err)
	}
}

func TestReadMIDIUnpacksAndBuffersRemainder(t *testing.T) {
	f := &fakeDev{reads: []readResult{{data: []byte{
		0x08, 0x80, 0x00, 0x00,
		0x09, 0x90, 0x00, 0x02,
		0x0A, 0xA0, 0x00, 0x00,
	}}}}
	tr := newTestTransport(f, 64)

	// A short p must not lose the rest of the transfer.
	p := make([]byte, 4)
	n, err := tr.ReadMIDI(p)
	if err != nil || n != 4 {
		t.Fatalf("ReadMIDI = %d, %v", n, err)
	}
	if want := []byte{0x80, 0x00, 0x00, 0x90}; !bytes.Equal(p[:n], want) {
		t.Fatalf("first read = %s, want %s", proto.Hex(p[:n]), proto.Hex(want))
	}

	var rest []byte
	for len(rest) < 5 {
		n, err = tr.ReadMIDI(p)
		if err != nil {
			t.Fatalf("ReadMIDI: %v", err)
		}
		rest = append(rest, p[:n]...)
	}
	if want := []byte{0x00, 0x02, 0xA0, 0x00, 0x00}; !bytes.Equal(rest, want) {
		t.Fatalf("buffered remainder = %s, want %s", proto.Hex(rest), proto.Hex(want))
	}
	// All of that came from a single transfer.
	if len(f.reads) != 0 {
		t.Fatalf("%d scripted reads left unconsumed", len(f.reads))
	}
}

func TestReadMIDITimeoutIsNotAnError(t *testing.T) {
	// An idle IN endpoint times out constantly; the framer's read loop must be
	// able to poll without drowning in errors.
	for _, e := range []error{syscall.ETIMEDOUT, context.DeadlineExceeded} {
		f := &fakeDev{reads: []readResult{{err: e}}}
		tr := newTestTransport(f, 64)
		n, err := tr.ReadMIDI(make([]byte, 64))
		if n != 0 || err != nil {
			t.Fatalf("ReadMIDI after %v = %d, %v; want 0, nil", e, n, err)
		}
	}
}

func TestReadMIDIPaddingOnly(t *testing.T) {
	f := &fakeDev{reads: []readResult{{data: make([]byte, 8)}}}
	tr := newTestTransport(f, 64)
	n, err := tr.ReadMIDI(make([]byte, 64))
	if n != 0 || err != nil {
		t.Fatalf("ReadMIDI = %d, %v; want 0, nil", n, err)
	}
}

func TestReadMIDIRealErrorSurfaces(t *testing.T) {
	f := &fakeDev{reads: []readResult{{err: syscall.ENODEV}}}
	tr := newTestTransport(f, 64)
	_, err := tr.ReadMIDI(make([]byte, 64))
	if !errors.Is(err, syscall.ENODEV) {
		t.Fatalf("ReadMIDI error = %v, want one wrapping ENODEV", err)
	}
	if !strings.Contains(err.Error(), "usb 37bf:800f") {
		t.Errorf("error %q does not name the device", err)
	}
}

func TestReadMIDIZeroLengthBuffer(t *testing.T) {
	f := &fakeDev{}
	tr := newTestTransport(f, 64)
	if n, err := tr.ReadMIDI(nil); n != 0 || err != nil {
		t.Fatalf("ReadMIDI(nil) = %d, %v", n, err)
	}
}

func TestCloseReleasesThenCloses(t *testing.T) {
	f := &fakeDev{}
	tr := newTestTransport(f, 64)
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if want := []string{"release", "close"}; !equalStrings(f.order, want) {
		t.Fatalf("close order = %v, want %v", f.order, want)
	}
	if len(f.released) != 1 || f.released[0] != 1 {
		t.Fatalf("released = %v, want [1]", f.released)
	}

	// Idempotent: a second Close must not release or close twice.
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if len(f.order) != 2 {
		t.Fatalf("second Close touched the device: %v", f.order)
	}

	if _, err := tr.ReadMIDI(make([]byte, 8)); !errors.Is(err, ErrClosed) {
		t.Errorf("ReadMIDI after Close = %v, want ErrClosed", err)
	}
	if err := tr.WriteMIDI([]byte{0xA0, 0x00, 0x00}); !errors.Is(err, ErrClosed) {
		t.Errorf("WriteMIDI after Close = %v, want ErrClosed", err)
	}
}

// ReadMIDI drains what the last transfer left buffered before it reports
// ErrClosed, so a Close racing a final response cannot discard bytes the device
// already sent. TestCloseReleasesThenCloses covers only the empty-buffer case,
// which proves less than it looks: with nothing buffered the closed check inside
// the transfer error path would answer ErrClosed anyway.
func TestReadMIDIAfterCloseDrainsPendingFirst(t *testing.T) {
	f := &fakeDev{reads: []readResult{{data: []byte{
		0x08, 0x80, 0x00, 0x00,
		0x09, 0x90, 0x00, 0x02,
		0x0A, 0xA0, 0x00, 0x00,
	}}}}
	tr := newTestTransport(f, 64)

	// A 4-byte p leaves five unpacked bytes buffered.
	p := make([]byte, 4)
	if n, err := tr.ReadMIDI(p); n != 4 || err != nil {
		t.Fatalf("ReadMIDI = %d, %v", n, err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var rest []byte
	for {
		n, err := tr.ReadMIDI(p)
		if errors.Is(err, ErrClosed) {
			break
		}
		if err != nil {
			t.Fatalf("ReadMIDI after Close: %v", err)
		}
		if n == 0 {
			t.Fatal("ReadMIDI after Close returned neither bytes nor ErrClosed")
		}
		rest = append(rest, p[:n]...)
	}
	if want := []byte{0x00, 0x02, 0xA0, 0x00, 0x00}; !bytes.Equal(rest, want) {
		t.Fatalf("drained %s after Close, want %s", proto.Hex(rest), proto.Hex(want))
	}
}

// A failed release leaves the ALSA MIDI port missing until replug, so it must
// be reported rather than swallowed -- and the device still has to be closed.
func TestCloseReportsReleaseFailure(t *testing.T) {
	f := &fakeDev{releaseIn: syscall.EBUSY}
	tr := newTestTransport(f, 64)
	err := tr.Close()
	if !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("Close error = %v, want one wrapping EBUSY", err)
	}
	if f.closed != 1 {
		t.Fatalf("device closed %d times, want 1", f.closed)
	}
	if !errors.Is(tr.Close(), syscall.EBUSY) {
		t.Error("Close did not report the same error on a repeat call")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
