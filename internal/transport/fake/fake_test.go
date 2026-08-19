package fake

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
)

// host drives a Device the way the framer does: it writes MIDI on one goroutine
// and decodes the reply stream on another.
type host struct {
	tr     proto.Transport
	frames chan []byte
	done   chan struct{}
}

func newHost(t *testing.T, d *Device) *host {
	t.Helper()
	h := &host{tr: d.Transport(), frames: make(chan []byte, 64), done: make(chan struct{})}
	go func() {
		defer close(h.done)
		dec := newFrameDecoder()
		buf := make([]byte, 16)
		for {
			n, err := h.tr.ReadMIDI(buf)
			for _, f := range dec.feed(buf[:n]) {
				select {
				case h.frames <- f:
				default: // test is not draining; drop rather than block forever
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return h
}

func (h *host) send(t *testing.T, frame []byte) {
	t.Helper()
	if err := h.tr.WriteMIDI(encodeFrameMIDI(frame)); err != nil {
		t.Fatalf("WriteMIDI: %v", err)
	}
}

// recv waits up to d for one response frame.
func (h *host) recv(t *testing.T, d time.Duration) ([]byte, bool) {
	t.Helper()
	select {
	case f := <-h.frames:
		return f, true
	case <-time.After(d):
		return nil, false
	}
}

// mustRecv fails the test if no response arrives.
func (h *host) mustRecv(t *testing.T) []byte {
	t.Helper()
	f, ok := h.recv(t, 2*time.Second)
	if !ok {
		t.Fatal("no response frame")
	}
	return f
}

func TestDefaultEchoesPayload(t *testing.T) {
	d := New()
	defer d.Close()
	h := newHost(t, d)

	// A read has no payload, so the echo is a bare acknowledgement.
	h.send(t, proto.Read(proto.CmdSerialNumber))
	if got, want := h.mustRecv(t), []byte{0x02, 0x08}; !bytes.Equal(got, want) {
		t.Errorf("read response = %x, want %x", got, want)
	}

	// A write is echoed with its payload and its write flag intact.
	w, err := proto.Write(proto.CmdVoltageMv, proto.EncodeU16(12000))
	if err != nil {
		t.Fatal(err)
	}
	h.send(t, w)
	if got, want := h.mustRecv(t), []byte{0x04, 0x92, 0x2E, 0xE0}; !bytes.Equal(got, want) {
		t.Errorf("write echo = %x, want %x", got, want)
	}
}

func TestSetResponseAndHandlerPrecedence(t *testing.T) {
	d := New()
	defer d.Close()
	h := newHost(t, d)

	d.SetResponse(proto.CmdSerialNumber, []byte("VF001234"))
	h.send(t, proto.Read(proto.CmdSerialNumber))
	got := h.mustRecv(t)
	want := append([]byte{0x0A, 0x08}, []byte("VF001234")...)
	if !bytes.Equal(got, want) {
		t.Errorf("canned response = %x, want %x", got, want)
	}

	// A handler wins over a canned response and sees the decoded request.
	var seen proto.Frame
	d.SetHandler(proto.CmdSerialNumber, func(f proto.Frame) []byte {
		seen = f
		return []byte("OTHER123")
	})
	h.send(t, proto.Read(proto.CmdSerialNumber))
	if got := h.mustRecv(t); !bytes.Equal(got[2:], []byte("OTHER123")) {
		t.Errorf("handler response = %x, want payload OTHER123", got)
	}
	if seen.Cmd != proto.CmdSerialNumber || seen.Write {
		t.Errorf("handler saw cmd=%v write=%v", seen.Cmd, seen.Write)
	}

	// A nil return from a handler means "no answer".
	d.SetHandler(proto.CmdSerialNumber, func(proto.Frame) []byte { return nil })
	h.send(t, proto.Read(proto.CmdSerialNumber))
	if f, ok := h.recv(t, 100*time.Millisecond); ok {
		t.Errorf("silent handler still answered with %x", f)
	}

	// Removing the handler falls back to the canned response.
	d.SetHandler(proto.CmdSerialNumber, nil)
	h.send(t, proto.Read(proto.CmdSerialNumber))
	if got := h.mustRecv(t); !bytes.Equal(got[2:], []byte("VF001234")) {
		t.Errorf("after handler removal got %x", got)
	}
}

func TestSetDefaultSilence(t *testing.T) {
	d := New()
	defer d.Close()
	h := newHost(t, d)

	d.SetDefault(nil)
	h.send(t, proto.Read(proto.CmdVoltageMv))
	if f, ok := h.recv(t, 100*time.Millisecond); ok {
		t.Errorf("silent device answered with %x", f)
	}

	d.SetDefault(func(proto.Frame) []byte { return []byte{0xAB} })
	h.send(t, proto.Read(proto.CmdVoltageMv))
	if got, want := h.mustRecv(t), []byte{0x03, 0x12, 0xAB}; !bytes.Equal(got, want) {
		t.Errorf("default responder gave %x, want %x", got, want)
	}
}

func TestFaultDrop(t *testing.T) {
	d := New()
	defer d.Close()
	h := newHost(t, d)

	d.SetFault(proto.CmdVoltageMv, Fault{Drop: true})
	h.send(t, proto.Read(proto.CmdVoltageMv))
	if f, ok := h.recv(t, 150*time.Millisecond); ok {
		t.Errorf("dropped command still answered with %x", f)
	}
	// Other commands are unaffected.
	h.send(t, proto.Read(proto.CmdSerialNumber))
	if _, ok := h.recv(t, time.Second); !ok {
		t.Error("per-command fault leaked to another command")
	}

	// A global fault covers everything without a fault of its own.
	d.ClearFaults()
	d.SetGlobalFault(Fault{Drop: true})
	d.SetFault(proto.CmdSerialNumber, Fault{})
	h.send(t, proto.Read(proto.CmdVoltageMv))
	if f, ok := h.recv(t, 150*time.Millisecond); ok {
		t.Errorf("global drop still answered with %x", f)
	}
	h.send(t, proto.Read(proto.CmdSerialNumber))
	if _, ok := h.recv(t, time.Second); !ok {
		t.Error("per-command fault did not override the global one")
	}
}

func TestFaultMismatch(t *testing.T) {
	d := New()
	defer d.Close()
	h := newHost(t, d)

	d.SetFault(proto.CmdVoltageMv, Fault{Mismatch: true, MismatchCmd: proto.CmdCurrentLimitMa})
	h.send(t, proto.Read(proto.CmdVoltageMv))
	got := h.mustRecv(t)
	if proto.Cmd(got[1]&proto.CmdCodeMask) != proto.CmdCurrentLimitMa {
		t.Errorf("response cmd = %#x, want %v", got[1], proto.CmdCurrentLimitMa)
	}
}

func TestFaultBadLength(t *testing.T) {
	d := New()
	defer d.Close()
	h := newHost(t, d)

	// A length byte larger than the bytes actually sent is dropped by the
	// receive state machine, so the host sees nothing at all (SPEC.md §3.3).
	d.SetFault(proto.CmdVoltageMv, Fault{BadLength: true, LengthByte: 0x40})
	h.send(t, proto.Read(proto.CmdVoltageMv))
	if f, ok := h.recv(t, 150*time.Millisecond); ok {
		t.Errorf("over-long length byte produced a frame: %x", f)
	}

	// A length byte below the preamble is dropped too.
	d.SetFault(proto.CmdVoltageMv, Fault{BadLength: true, LengthByte: 0x01})
	h.send(t, proto.Read(proto.CmdVoltageMv))
	if f, ok := h.recv(t, 150*time.Millisecond); ok {
		t.Errorf("short length byte produced a frame: %x", f)
	}

	// A length byte shorter than what arrived truncates rather than drops.
	d.SetResponse(proto.CmdVoltageMv, []byte{0x13, 0x88})
	d.SetFault(proto.CmdVoltageMv, Fault{BadLength: true, LengthByte: 0x03})
	h.send(t, proto.Read(proto.CmdVoltageMv))
	if got, want := h.mustRecv(t), []byte{0x03, 0x12, 0x13}; !bytes.Equal(got, want) {
		t.Errorf("truncated frame = %x, want %x", got, want)
	}
}

func TestFaultDelay(t *testing.T) {
	d := New()
	defer d.Close()
	h := newHost(t, d)

	d.SetFault(proto.CmdVoltageMv, Fault{Delay: 250 * time.Millisecond})
	start := time.Now()
	h.send(t, proto.Read(proto.CmdVoltageMv))
	if f, ok := h.recv(t, 50*time.Millisecond); ok {
		t.Fatalf("delayed response arrived immediately: %x", f)
	}
	if _, ok := h.recv(t, 2*time.Second); !ok {
		t.Fatal("delayed response never arrived")
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("response after %v, want at least the 250ms delay", elapsed)
	}
}

func TestCloseUnblocksRead(t *testing.T) {
	d := New()
	tr := d.Transport()

	done := make(chan error, 1)
	go func() {
		_, err := tr.ReadMIDI(make([]byte, 8))
		done <- err
	}()

	// Give the reader time to block, then close.
	time.Sleep(20 * time.Millisecond)
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Errorf("blocked read returned %v, want io.EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock ReadMIDI")
	}

	if err := d.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := tr.WriteMIDI([]byte{0x80, 0, 0}); !errors.Is(err, ErrClosed) {
		t.Errorf("write after close returned %v, want ErrClosed", err)
	}
	if err := d.Push([]byte{0x02, 0x08}); !errors.Is(err, ErrClosed) {
		t.Errorf("push after close returned %v, want ErrClosed", err)
	}
}

func TestCloseDrainsPendingBytes(t *testing.T) {
	d := New()
	if err := d.Push([]byte{0x02, 0x08}); err != nil {
		t.Fatal(err)
	}
	d.Close()

	tr := d.Transport()
	buf := make([]byte, 64)
	n, err := tr.ReadMIDI(buf)
	if err != nil {
		t.Fatalf("read after close: %v", err)
	}
	if want := len(encodeFrameMIDI([]byte{0x02, 0x08})); n != want {
		t.Errorf("read %d bytes after close, want the %d already queued", n, want)
	}
	if _, err := tr.ReadMIDI(buf); !errors.Is(err, io.EOF) {
		t.Errorf("second read returned %v, want io.EOF", err)
	}
}

func TestSentRecordsRequests(t *testing.T) {
	d := New()
	defer d.Close()
	h := newHost(t, d)

	frames := [][]byte{
		proto.Read(proto.CmdSerialNumber),
		{0x04, 0x92, 0x2E, 0xE0},
		{0x03, 0x11, 0x00},
	}
	for _, f := range frames {
		h.send(t, f)
		h.mustRecv(t)
	}
	sent := d.Sent()
	if len(sent) != len(frames) {
		t.Fatalf("Sent() has %d frames, want %d", len(sent), len(frames))
	}
	for i := range frames {
		if !bytes.Equal(sent[i], frames[i]) {
			t.Errorf("Sent()[%d] = %x, want %x", i, sent[i], frames[i])
		}
	}

	// Sent returns copies: mutating them must not corrupt the record.
	sent[0][0] = 0xFF
	if d.Sent()[0][0] != 0x02 {
		t.Error("Sent() aliases the Device's own buffers")
	}

	d.ClearSent()
	if len(d.Sent()) != 0 {
		t.Error("ClearSent left frames behind")
	}
}

// TestMalformedRequestsAreDropped covers the receive state machine's rejection
// rules from SPEC.md §3.3: the device must ignore these entirely, neither
// recording nor answering them.
func TestMalformedRequestsAreDropped(t *testing.T) {
	cases := []struct {
		name string
		acc  []byte // bytes accumulated between the start and end markers
	}{
		{"length below preamble", []byte{0x01, 0x08}},
		{"length zero", []byte{0x00, 0x08}},
		{"length exceeds received", []byte{0x08, 0x08, 0x01}},
		{"single byte", []byte{0x02}},
		{"empty frame", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := New()
			defer d.Close()
			h := newHost(t, d)

			h.send(t, tc.acc)
			if f, ok := h.recv(t, 100*time.Millisecond); ok {
				t.Errorf("device answered a malformed frame with %x", f)
			}
			if got := d.Sent(); len(got) != 0 {
				t.Errorf("malformed frame was recorded as %x", got)
			}

			// The device must still be usable afterwards: a dropped frame
			// resets the accumulator rather than wedging it.
			h.send(t, proto.Read(proto.CmdSerialNumber))
			if _, ok := h.recv(t, time.Second); !ok {
				t.Error("device stopped answering after a malformed frame")
			}
		})
	}
}

func TestDecoderResyncAndNoise(t *testing.T) {
	serial := proto.Read(proto.CmdSerialNumber)

	cases := []struct {
		name string
		midi []byte
		want [][]byte
	}{
		{
			name: "mid-frame start marker resets the accumulator",
			midi: []byte{
				0x80, 0x00, 0x00, // start
				0x90, 0x00, 0x09, // a stray protocol byte, discarded by the reset
				0x80, 0x00, 0x00, // start again
				0x90, 0x00, 0x02,
				0x90, 0x00, 0x08,
				0xA0, 0x00, 0x00,
			},
			want: [][]byte{serial},
		},
		{
			name: "unrelated channel messages are ignored",
			midi: []byte{
				0x80, 0x00, 0x00,
				0x90, 0x00, 0x02,
				0xB0, 0x07, 0x7F, // control change, 2 data bytes
				0xC0, 0x05, // program change, 1 data byte
				0xE0, 0x00, 0x40, // pitch bend, 2 data bytes
				0x90, 0x00, 0x08,
				0xA0, 0x00, 0x00,
			},
			want: [][]byte{serial},
		},
		{
			name: "system common consumes its data and cancels running status",
			midi: []byte{
				0x80, 0x00, 0x00,
				0x90, 0x00, 0x02,
				0xF2, 0x01, 0x02, // song position pointer
				0xF3, 0x04, // song select
				0x90, 0x00, 0x08,
				0xA0, 0x00, 0x00,
			},
			want: [][]byte{serial},
		},
		{
			name: "running status data bytes continue the previous message",
			midi: []byte{
				0x80, 0x00, 0x00,
				0x90, 0x00, 0x02,
				0x00, 0x08, // running status: another Note On
				0xA0, 0x00, 0x00,
			},
			want: [][]byte{serial},
		},
		{
			name: "channel nibble is ignored in both directions",
			midi: []byte{
				0x87, 0x00, 0x00,
				0x93, 0x00, 0x02,
				0x93, 0x00, 0x08,
				0xAF, 0x00, 0x00,
			},
			want: [][]byte{serial},
		},
		{
			name: "trailing partial message yields nothing",
			midi: []byte{
				0x80, 0x00, 0x00,
				0x90, 0x00, 0x02,
				0x90, 0x00, 0x08,
				0xA0, 0x00, 0x00,
				0x90, 0x00, // truncated
			},
			want: [][]byte{serial},
		},
		{
			name: "end marker without a start still dispatches",
			midi: []byte{
				0x90, 0x00, 0x02,
				0x90, 0x00, 0x08,
				0xA0, 0x00, 0x00,
			},
			want: [][]byte{serial},
		},
		{
			name: "only the low nibble of each data byte is used",
			midi: []byte{
				0x80, 0x00, 0x00,
				0x90, 0x70, 0x72, // 0x70&0x0F=0, 0x72&0x0F=2 -> 0x02
				0x90, 0x40, 0x78, // -> 0x08
				0xA0, 0x00, 0x00,
			},
			want: [][]byte{serial},
		},
		{
			name: "two frames back to back",
			midi: append(encodeFrameMIDI(serial), encodeFrameMIDI([]byte{0x02, 0x12})...),
			want: [][]byte{serial, {0x02, 0x12}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newFrameDecoder().feed(tc.midi)
			if len(got) != len(tc.want) {
				t.Fatalf("decoded %d frames (%x), want %d", len(got), got, len(tc.want))
			}
			for i := range got {
				if !bytes.Equal(got[i], tc.want[i]) {
					t.Errorf("frame %d = %x, want %x", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestDecoderOverlongFrameTruncates reproduces a deliberate quirk: the 64-byte
// accumulator cap silently discards the excess instead of aborting, so a frame
// declaring 64 bytes still dispatches even though more arrived (SPEC.md §3.3).
func TestDecoderOverlongFrameTruncates(t *testing.T) {
	frame := make([]byte, 70)
	frame[0] = proto.MaxFrameLen // 64
	frame[1] = byte(proto.CmdSerialNumber)
	for i := 2; i < len(frame); i++ {
		frame[i] = byte(i)
	}

	got := newFrameDecoder().feed(encodeFrameMIDI(frame))
	if len(got) != 1 {
		t.Fatalf("decoded %d frames, want 1", len(got))
	}
	if len(got[0]) != proto.MaxFrameLen {
		t.Fatalf("frame is %d bytes, want %d", len(got[0]), proto.MaxFrameLen)
	}
	if !bytes.Equal(got[0], frame[:proto.MaxFrameLen]) {
		t.Errorf("truncated frame = %x, want %x", got[0], frame[:proto.MaxFrameLen])
	}

	// One more byte than the cap and a length that no longer fits: dropped.
	frame[0] = 65
	if got := newFrameDecoder().feed(encodeFrameMIDI(frame)); len(got) != 0 {
		t.Errorf("length 65 dispatched %x, want a drop", got)
	}
}

func TestPushAndPushMIDI(t *testing.T) {
	d := New()
	defer d.Close()
	h := newHost(t, d)

	// An unsolicited frame arrives with no request having been made.
	if err := d.Push([]byte{0x04, 0x12, 0x13, 0x88}); err != nil {
		t.Fatal(err)
	}
	if got, want := h.mustRecv(t), []byte{0x04, 0x12, 0x13, 0x88}; !bytes.Equal(got, want) {
		t.Errorf("pushed frame = %x, want %x", got, want)
	}
	if len(d.Sent()) != 0 {
		t.Error("a pushed frame was recorded as a request")
	}

	// Raw injection bypasses framing entirely.
	if err := d.PushMIDI([]byte{0xF8, 0xFE, 0x80, 0x00, 0x00, 0x90, 0x00, 0x02, 0x90, 0x00, 0x08, 0xA0, 0x00, 0x00}); err != nil {
		t.Fatal(err)
	}
	if got, want := h.mustRecv(t), []byte{0x02, 0x08}; !bytes.Equal(got, want) {
		t.Errorf("raw injection decoded to %x, want %x", got, want)
	}
}

func TestRegisters(t *testing.T) {
	d := New()
	defer d.Close()
	h := newHost(t, d)

	d.SetRegister(proto.CmdVoltageMv, proto.EncodeU16(5000))

	h.send(t, proto.Read(proto.CmdVoltageMv))
	if got, want := h.mustRecv(t), []byte{0x04, 0x12, 0x13, 0x88}; !bytes.Equal(got, want) {
		t.Errorf("initial read = %x, want %x", got, want)
	}

	w, err := proto.Write(proto.CmdVoltageMv, proto.EncodeU16(12000))
	if err != nil {
		t.Fatal(err)
	}
	h.send(t, w)
	if got, want := h.mustRecv(t), []byte{0x04, 0x92, 0x2E, 0xE0}; !bytes.Equal(got, want) {
		t.Errorf("write echo = %x, want %x", got, want)
	}

	h.send(t, proto.Read(proto.CmdVoltageMv))
	if got, want := h.mustRecv(t), []byte{0x04, 0x12, 0x2E, 0xE0}; !bytes.Equal(got, want) {
		t.Errorf("read-back = %x, want %x", got, want)
	}

	if v, ok := d.Register(proto.CmdVoltageMv); !ok || !bytes.Equal(v, []byte{0x2E, 0xE0}) {
		t.Errorf("Register = %x (%v), want 2ee0", v, ok)
	}
	if _, ok := d.Register(proto.CmdCurrentLimitMa); ok {
		t.Error("Register reported an unset command as present")
	}
}

func TestTypicalIdentityAndSettings(t *testing.T) {
	d := NewTypical()
	defer d.Close()
	h := newHost(t, d)

	read := func(c proto.Cmd) proto.Frame {
		t.Helper()
		h.send(t, proto.Read(c))
		f, err := proto.Parse(h.mustRecv(t))
		if err != nil {
			t.Fatalf("parse response to %v: %v", c, err)
		}
		if f.Cmd != c {
			t.Fatalf("response to %v carried %v", c, f.Cmd)
		}
		return f
	}

	if got := proto.DecodeString(read(proto.CmdSerialNumber).Payload); got != TypicalSerial {
		t.Errorf("serial = %q, want %q", got, TypicalSerial)
	}
	if got := proto.DecodeString(read(proto.CmdFirmwareVersion).Payload); got != TypicalFirmware {
		t.Errorf("firmware = %q, want %q", got, TypicalFirmware)
	}
	if got := proto.DecodeString(read(proto.CmdMfgDate).Payload); got != TypicalMfgDate {
		t.Errorf("mfg date = %q, want %q", got, TypicalMfgDate)
	}

	if v, err := proto.DecodeU16(read(proto.CmdVoltageMv).Payload); err != nil || v != proto.DefaultVoltageMv {
		t.Errorf("voltage = %d (%v), want %d", v, err, proto.DefaultVoltageMv)
	}
	if v, err := proto.DecodeU16(read(proto.CmdCurrentLimitMa).Payload); err != nil || v != proto.DefaultCurrentLimitMa {
		t.Errorf("current limit = %d (%v), want %d", v, err, proto.DefaultCurrentLimitMa)
	}
	low, high, err := proto.DecodeVLimit(read(proto.CmdUserVLimit).Payload)
	if err != nil || low != proto.DefaultVLimitLowMv || high != proto.DefaultVLimitHighMv {
		t.Errorf("vlimit = %d/%d (%v), want %d/%d", low, high, err,
			proto.DefaultVLimitLowMv, proto.DefaultVLimitHighMv)
	}
	if p := read(proto.CmdDisableLEDDuringOp).Payload; len(p) != 1 || !proto.DecodeLEDAlwaysOn(p[0]) {
		t.Errorf("led payload = %x, want 00 (always on)", p)
	}
	raw, mv, err := proto.DecodeVMeasure(read(proto.CmdVMeasure).Payload)
	if err != nil || raw != TypicalRawADC || mv != TypicalMeasuredMv {
		t.Errorf("vmeasure = %d/%d (%v), want %d/%d", raw, mv, err, TypicalRawADC, TypicalMeasuredMv)
	}
	if v, err := proto.DecodeI32(read(proto.CmdVMeasureADCOffset).Payload); err != nil || v != 0 {
		t.Errorf("adc offset = %d (%v), want 0", v, err)
	}

	// Writes stick.
	w, err := proto.Write(proto.CmdVoltageMv, proto.EncodeU16(9000))
	if err != nil {
		t.Fatal(err)
	}
	h.send(t, w)
	h.mustRecv(t)
	if v, err := proto.DecodeU16(read(proto.CmdVoltageMv).Payload); err != nil || v != 9000 {
		t.Errorf("voltage after write = %d (%v), want 9000", v, err)
	}
}

func TestTypicalAuthLockIsAsymmetric(t *testing.T) {
	d := NewTypical()
	defer d.Close()
	h := newHost(t, d)

	w, err := proto.Write(proto.CmdAuthLock, []byte{0x02})
	if err != nil {
		t.Fatal(err)
	}
	h.send(t, w)
	if got, want := h.mustRecv(t), []byte{0x03, 0x96, 0x02}; !bytes.Equal(got, want) {
		t.Errorf("authlock write echo = %x, want %x", got, want)
	}

	h.send(t, proto.Read(proto.CmdAuthLock))
	got := h.mustRecv(t)
	// The vendor client reads the level from payload[1]; a reader taking
	// payload[0] must see the same thing while the layout is unverified.
	if want := []byte{0x04, 0x16, 0x02, 0x02}; !bytes.Equal(got, want) {
		t.Errorf("authlock read = %x, want %x", got, want)
	}
}

func TestTypicalPDOLogChunks(t *testing.T) {
	d := NewTypical()
	defer d.Close()
	h := newHost(t, d)

	var blob []byte
	for i := range pdoLogChunks {
		h.send(t, []byte{0x03, byte(proto.CmdPDOLog), byte(i)})
		got := h.mustRecv(t)
		if len(got) != 2+1+pdoLogChunkBytes {
			t.Fatalf("chunk %d response is %d bytes: %x", i, len(got), got)
		}
		if got[2] != byte(i) {
			t.Fatalf("chunk %d response echoed index %d", i, got[2])
		}
		blob = append(blob, got[3:]...)
	}
	if want := TypicalPDOLog(); !bytes.Equal(blob, want) {
		t.Errorf("downloaded log = %x\nwant             %x", blob, want)
	}

	// A chunk beyond the log is unanswered.
	h.send(t, []byte{0x03, byte(proto.CmdPDOLog), byte(pdoLogChunks)})
	if f, ok := h.recv(t, 100*time.Millisecond); ok {
		t.Errorf("out-of-range chunk answered with %x", f)
	}

	// The erase is a write with an empty payload and is acknowledged.
	erase, err := proto.Write(proto.CmdPDOLog, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.send(t, erase)
	if got, want := h.mustRecv(t), []byte{0x02, 0x91}; !bytes.Equal(got, want) {
		t.Errorf("erase ack = %x, want %x", got, want)
	}
	h.send(t, []byte{0x03, byte(proto.CmdPDOLog), 0x00})
	if got := h.mustRecv(t); !bytes.Equal(got[3:], make([]byte, pdoLogChunkBytes)) {
		t.Errorf("chunk 0 after erase = %x, want zeros", got)
	}
}

func TestTypicalJumpToBootloaderIsUnacknowledged(t *testing.T) {
	d := NewTypical()
	defer d.Close()
	h := newHost(t, d)

	h.send(t, proto.Read(proto.CmdJumpAppToBootloader))
	if f, ok := h.recv(t, 150*time.Millisecond); ok {
		t.Errorf("jump command was acknowledged with %x", f)
	}
	if got := d.Sent(); len(got) != 1 || !bytes.Equal(got[0], []byte{0x02, 0x14}) {
		t.Errorf("Sent() = %x, want one 0214 frame", got)
	}
}

func TestTypicalPDOLogLayout(t *testing.T) {
	b := TypicalPDOLog()
	if len(b) != pdoLogChunks*pdoLogChunkBytes {
		t.Fatalf("log is %d bytes, want %d", len(b), pdoLogChunks*pdoLogChunkBytes)
	}
	// Little-endian, unlike every other field in the protocol (SPEC.md §9.3).
	if b[0] != 0x88 || b[1] != 0x13 {
		t.Errorf("target voltage bytes = %02x %02x, want 88 13 (5000 LE)", b[0], b[1])
	}
	if b[4] != 3 {
		t.Errorf("nPdosReceived = %d, want 3", b[4])
	}
	// 5 V 3 A fixed PDO: (5000/50)<<10 | (3000/10) = 0x0001912C.
	if got := uint32(b[10]) | uint32(b[11])<<8 | uint32(b[12])<<16 | uint32(b[13])<<24; got != 0x0001912C {
		t.Errorf("first PDO = %#08x, want 0x0001912c", got)
	}
}

// TestUnplug pins the hot-unplug semantics the doc comment promises, side by
// side, because dependent packages' tests (session's drains, its ready-retry
// classification) stage their scenarios on exactly these guarantees.
func TestUnplug(t *testing.T) {
	t.Run("drains queued bytes then fails with the given error", func(t *testing.T) {
		unplugged := errors.New("read /dev/snd/midiC1D0: no such device")
		d := New()
		defer d.Close()
		if err := d.Push([]byte{0x02, 0x08}); err != nil {
			t.Fatal(err)
		}
		d.Unplug(unplugged)

		// The bytes queued before the unplug are still readable, in full and
		// before any error: this is what makes Push-then-Unplug fixtures
		// deterministic for a reader that starts later.
		tr := d.Transport()
		want := encodeFrameMIDI([]byte{0x02, 0x08})
		got := make([]byte, 0, len(want))
		buf := make([]byte, 7) // deliberately smaller than the stream: drain over several reads
		for len(got) < len(want) {
			n, err := tr.ReadMIDI(buf)
			if err != nil {
				t.Fatalf("read while %d queued bytes remain: %v", len(want)-len(got), err)
			}
			got = append(got, buf[:n]...)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("drained % x, want % x", got, want)
		}
		// Then the error, on this and every later read.
		for i := 0; i < 2; i++ {
			if _, err := tr.ReadMIDI(buf); !errors.Is(err, unplugged) {
				t.Fatalf("read %d after drain = %v, want the unplug error", i, err)
			}
		}
	})

	t.Run("nil error installs ErrUnplugged", func(t *testing.T) {
		d := New()
		defer d.Close()
		d.Unplug(nil)
		if _, err := d.Transport().ReadMIDI(make([]byte, 8)); !errors.Is(err, ErrUnplugged) {
			t.Fatalf("read = %v, want ErrUnplugged", err)
		}
	})

	t.Run("unblocks a waiting reader", func(t *testing.T) {
		unplugged := errors.New("gone")
		d := New()
		defer d.Close()
		done := make(chan error, 1)
		go func() {
			_, err := d.Transport().ReadMIDI(make([]byte, 8))
			done <- err
		}()
		time.Sleep(20 * time.Millisecond) // let the reader block
		d.Unplug(unplugged)
		select {
		case err := <-done:
			if !errors.Is(err, unplugged) {
				t.Errorf("blocked read returned %v, want the unplug error", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Unplug did not unblock ReadMIDI")
		}
	})

	t.Run("writes are recorded but never answered", func(t *testing.T) {
		d := New() // default responder echoes: any answer would be visible
		defer d.Close()
		d.Unplug(nil)

		tr := d.Transport()
		if err := tr.WriteMIDI(encodeFrameMIDI(proto.Read(proto.CmdSerialNumber))); err != nil {
			t.Fatalf("write after unplug: %v", err)
		}
		// The frame is on the record, so a test can prove the host transmitted...
		if got := d.SentHex(); len(got) != 1 || got[0] != "02 08" {
			t.Fatalf("SentHex = %q, want [\"02 08\"]", got)
		}
		// ...but nothing answered it: the read reports the unplug, not an echo.
		if _, err := tr.ReadMIDI(make([]byte, 8)); !errors.Is(err, ErrUnplugged) {
			t.Errorf("read = %v, want ErrUnplugged (an echo would mean the dead device answered)", err)
		}
	})

	t.Run("push fails and pending delayed replies are discarded", func(t *testing.T) {
		d := New()
		defer d.Close()
		h := newHost(t, d)

		// Schedule a delayed reply, then unplug before it becomes readable.
		d.SetFault(proto.CmdVoltageMv, Fault{Delay: 50 * time.Millisecond})
		h.send(t, proto.Read(proto.CmdVoltageMv))
		d.Unplug(nil)

		if err := d.Push([]byte{0x02, 0x08}); !errors.Is(err, ErrClosed) {
			t.Errorf("Push after unplug = %v, want ErrClosed", err)
		}
		// Past the delay, the reply must not have surfaced.
		if f, ok := h.recv(t, 150*time.Millisecond); ok {
			t.Errorf("delayed reply surfaced after unplug: %x", f)
		}
	})

	t.Run("close after unplug keeps the unplug cause", func(t *testing.T) {
		unplugged := errors.New("gone")
		d := New()
		d.Unplug(unplugged)
		if err := d.Close(); err != nil {
			t.Fatalf("Close after Unplug: %v", err)
		}
		// A session tearing down closes the transport after the reader already
		// died; that must not rewrite the error a late read reports.
		if _, err := d.Transport().ReadMIDI(make([]byte, 8)); !errors.Is(err, unplugged) {
			t.Errorf("read = %v, want the unplug error to survive Close", err)
		}
		if err := d.Transport().WriteMIDI([]byte{0x80, 0, 0}); !errors.Is(err, ErrClosed) {
			t.Errorf("write after Close = %v, want ErrClosed", err)
		}
	})
}

// TestSentHex pins the rendering: proto.Hex form, one string per frame, in
// arrival order.
func TestSentHex(t *testing.T) {
	d := New()
	defer d.Close()
	h := newHost(t, d)

	for _, f := range [][]byte{proto.Read(proto.CmdSerialNumber), {0x04, 0x92, 0x2E, 0xE0}} {
		h.send(t, f)
		h.mustRecv(t)
	}
	got := d.SentHex()
	want := []string{"02 08", "04 92 2e e0"}
	if len(got) != len(want) {
		t.Fatalf("SentHex = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SentHex[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestConcurrentUse is meaningful under -race: a reader goroutine, a writer
// goroutine and a reconfiguring goroutine all touch the Device at once.
func TestConcurrentUse(t *testing.T) {
	d := NewTypical()
	h := newHost(t, d)

	req := encodeFrameMIDI(proto.Read(proto.CmdVoltageMv))
	writeErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 50 {
			// t.Fatal is not usable off the test goroutine; report instead.
			if err := d.Transport().WriteMIDI(req); err != nil {
				select {
				case writeErr <- err:
				default:
				}
				return
			}
			if i%10 == 0 {
				d.Sent()
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 50 {
			d.SetResponse(proto.CmdCurrentLimitMa, []byte{0x13, 0x88})
			d.SetFault(proto.CmdChipUUID, Fault{Drop: true})
			_, _ = d.Register(proto.CmdVoltageMv)
		}
	}()
	wg.Wait()
	select {
	case err := <-writeErr:
		t.Fatalf("WriteMIDI under load: %v", err)
	default:
	}

	for range 50 {
		if _, ok := h.recv(t, 2*time.Second); !ok {
			t.Fatal("lost a response under concurrent load")
		}
	}
	d.Close()
	<-h.done
}

// FuzzFrameDecoder asserts the receive state machine never panics and never
// emits a frame violating the length invariants, whatever byte soup arrives.
func FuzzFrameDecoder(f *testing.F) {
	f.Add(encodeFrameMIDI([]byte{0x02, 0x08}))
	f.Add(encodeFrameMIDI([]byte{0x06, 0x97, 0xBB, 0x80, 0x0C, 0xE4}))
	f.Add([]byte{0x80, 0x00, 0x00, 0x90, 0x00, 0x02, 0xA0})
	f.Add([]byte{0xF0, 0x7E, 0x00, 0xF7, 0x90, 0x00, 0x02})
	f.Add([]byte{0xFF, 0xFE, 0xF8, 0x00, 0x01, 0x02})

	f.Fuzz(func(t *testing.T, data []byte) {
		dec := newFrameDecoder()
		var whole [][]byte
		for _, fr := range dec.feed(data) {
			if len(fr) < proto.PreambleLen || len(fr) > proto.MaxFrameLen {
				t.Fatalf("emitted a %d-byte frame: %x", len(fr), fr)
			}
			if int(fr[0]) != len(fr) {
				t.Fatalf("frame declares %d bytes but is %d: %x", fr[0], len(fr), fr)
			}
			whole = append(whole, fr)
		}

		// Chunking must not change the result.
		split := newFrameDecoder()
		var got [][]byte
		for _, b := range data {
			got = append(got, split.feed([]byte{b})...)
		}
		if len(got) != len(whole) {
			t.Fatalf("byte-at-a-time decode gave %d frames, whole-buffer gave %d", len(got), len(whole))
		}
		for i := range got {
			if !bytes.Equal(got[i], whole[i]) {
				t.Fatalf("frame %d differs: %x vs %x", i, got[i], whole[i])
			}
		}
	})
}

// FuzzFrameRoundTrip asserts that any frame the encoder accepts decodes back to
// itself, including every byte value and every velocity-0 hazard.
func FuzzFrameRoundTrip(f *testing.F) {
	f.Add([]byte{0x02, 0x08})
	f.Add([]byte{0x04, 0x92, 0x2E, 0xE0})
	f.Add([]byte{0x03, 0x8F, 0x00})

	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > proto.MaxPayloadLen {
			payload = payload[:proto.MaxPayloadLen]
		}
		frame, err := proto.Build(proto.CmdVoltageMv, payload, true, false)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		got := newFrameDecoder().feed(encodeFrameMIDI(frame))
		if len(got) != 1 || !bytes.Equal(got[0], frame) {
			t.Fatalf("round trip of %x gave %x", frame, got)
		}
	})
}
