package framer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
)

// h parses the spec's notation for a MIDI stream or frame: hex bytes separated
// by spaces, with optional "|" message separators.
func h(t *testing.T, s string) []byte {
	t.Helper()
	clean := strings.NewReplacer(" ", "", "|", "", "\n", "", "\t", "").Replace(s)
	b, err := hex.DecodeString(clean)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// golden holds the vectors of SPEC.md §15, verbatim.
var golden = []struct {
	name  string
	frame string
	midi  string
}{
	{
		name:  "read serial number",
		frame: "02 08",
		midi:  "80 00 00 | 90 00 02 | 90 00 08 | A0 00 00",
	},
	{
		name:  "set voltage 12.000 V",
		frame: "04 92 2E E0",
		midi:  "80 00 00 | 90 00 04 | 90 09 02 | 90 02 0E | 90 0E 00 | A0 00 00",
	},
	{
		name:  "set current limit 5000 mA",
		frame: "04 93 13 88",
		midi:  "80 00 00 | 90 00 04 | 90 09 03 | 90 01 03 | 90 08 08 | A0 00 00",
	},
	{
		name:  "set vlimit low=3300 high=48000",
		frame: "06 97 BB 80 0C E4",
		midi:  "80 00 00 | 90 00 06 | 90 09 07 | 90 0B 0B | 90 08 00 | 90 00 0C | 90 0E 04 | A0 00 00",
	},
	{
		name:  "led always on",
		frame: "03 8F 00",
		midi:  "80 00 00 | 90 00 03 | 90 08 0F | 90 00 00 | A0 00 00",
	},
	{
		name:  "jump to bootloader",
		frame: "02 14",
		midi:  "80 00 00 | 90 00 02 | 90 01 04 | A0 00 00",
	},
}

// ---------------------------------------------------------------------------
// Encoder
// ---------------------------------------------------------------------------

func TestEncodeMIDIGolden(t *testing.T) {
	for _, v := range golden {
		t.Run(v.name, func(t *testing.T) {
			frame, want := h(t, v.frame), h(t, v.midi)
			if got := EncodeMIDI(frame); !bytes.Equal(got, want) {
				t.Errorf("EncodeMIDI(%s)\n got %s\nwant %s",
					proto.Hex(frame), proto.Hex(got), proto.Hex(want))
			}
		})
	}
}

func TestDecodeGolden(t *testing.T) {
	for _, v := range golden {
		t.Run(v.name, func(t *testing.T) {
			want, stream := h(t, v.frame), h(t, v.midi)
			d := NewDecoder()
			frames := d.Feed(stream)
			if len(frames) != 1 {
				t.Fatalf("got %d frames, want 1: %v", len(frames), frames)
			}
			if !bytes.Equal(frames[0], want) {
				t.Errorf("decoded %s, want %s", proto.Hex(frames[0]), proto.Hex(want))
			}
		})
	}
}

func TestMIDIMessagesGolden(t *testing.T) {
	for _, v := range golden {
		t.Run(v.name, func(t *testing.T) {
			frame, stream := h(t, v.frame), h(t, v.midi)
			msgs := MIDIMessages(frame)
			if len(msgs) != len(frame)+2 {
				t.Fatalf("got %d messages, want %d", len(msgs), len(frame)+2)
			}
			var flat []byte
			for i, m := range msgs {
				if len(m) != MessageLen {
					t.Fatalf("message %d has length %d, want %d", i, len(m), MessageLen)
				}
				if cap(m) != MessageLen {
					t.Errorf("message %d has capacity %d; slices must be clamped so append cannot bleed into the next", i, cap(m))
				}
				flat = append(flat, m...)
			}
			if !bytes.Equal(flat, stream) {
				t.Errorf("concatenation\n got %s\nwant %s", proto.Hex(flat), proto.Hex(stream))
			}
			if msgs[0][0] != StatusFrameStart {
				t.Errorf("first message status %#02x, want %#02x", msgs[0][0], StatusFrameStart)
			}
			if last := msgs[len(msgs)-1]; last[0] != StatusFrameEnd {
				t.Errorf("last message status %#02x, want %#02x", last[0], StatusFrameEnd)
			}
		})
	}
}

func TestEncodeMIDIEmptyFrame(t *testing.T) {
	// Degenerate but well-defined: the markers with nothing between them.
	want := []byte{StatusFrameStart, 0, 0, StatusFrameEnd, 0, 0}
	if got := EncodeMIDI(nil); !bytes.Equal(got, want) {
		t.Errorf("EncodeMIDI(nil) = %s, want %s", proto.Hex(got), proto.Hex(want))
	}
	// The receiver drops it: fewer than two accumulated bytes.
	if frames := NewDecoder().Feed(want); len(frames) != 0 {
		t.Errorf("empty frame decoded to %v, want nothing", frames)
	}
}

// TestRoundTripAllBytes covers every possible payload byte value.
func TestRoundTripAllBytes(t *testing.T) {
	for i := 0; i <= 0xFF; i++ {
		b := byte(i)
		frame := []byte{0x03, byte(proto.CmdSerialNumber), b}
		stream := EncodeMIDI(frame)

		// Every emitted data byte must be 7-bit safe (in fact 4-bit): this is
		// the whole point of the nibble encoding (SPEC.md §3.1).
		for j := 0; j < len(stream); j += MessageLen {
			if stream[j+1] > 0x0F || stream[j+2] > 0x0F {
				t.Fatalf("byte %#02x encoded to non-nibble data %#02x %#02x", b, stream[j+1], stream[j+2])
			}
		}

		frames := NewDecoder().Feed(stream)
		if len(frames) != 1 || !bytes.Equal(frames[0], frame) {
			t.Fatalf("byte %#02x: round trip gave %v, want %s", b, frames, proto.Hex(frame))
		}
	}
}

// TestVelocityZeroBytes exercises SPEC.md §3.2: any byte whose low nibble is
// zero encodes as a Note On with velocity 0, which MIDI convention equates to
// Note Off. Our own decoder must key on the status byte alone and never
// reinterpret such a message as a start-of-frame marker.
func TestVelocityZeroBytes(t *testing.T) {
	for hi := 0; hi <= 0x0F; hi++ {
		b := byte(hi << 4)
		msg := MIDIMessages([]byte{b})[1] // [0] is the start marker
		if msg[0] != StatusFrameData || msg[1] != byte(hi) || msg[2] != 0x00 {
			t.Fatalf("byte %#02x encoded as % x, want 90 %02x 00", b, msg, hi)
		}
	}

	// A whole frame of low-nibble-zero bytes must survive intact.
	frame := []byte{0x04, 0x90, 0x20, 0xF0}
	frames := NewDecoder().Feed(EncodeMIDI(frame))
	if len(frames) != 1 || !bytes.Equal(frames[0], frame) {
		t.Fatalf("velocity-0 frame round trip gave %v, want %s", frames, proto.Hex(frame))
	}
}

// ---------------------------------------------------------------------------
// Decoder state machine
// ---------------------------------------------------------------------------

// feedAll runs a stream through a fresh decoder and returns every frame.
func feedAll(stream []byte) [][]byte {
	return NewDecoder().Feed(stream)
}

func mustOneFrame(t *testing.T, frames [][]byte, want []byte) {
	t.Helper()
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1: %v", len(frames), frames)
	}
	if !bytes.Equal(frames[0], want) {
		t.Fatalf("got frame %s, want %s", proto.Hex(frames[0]), proto.Hex(want))
	}
}

func TestDecoderIgnoresSystemRealtimeAnywhere(t *testing.T) {
	// 0xF8 clock and 0xFE active sensing may land between any two bytes,
	// including between a status byte and its data (SPEC.md §3.3).
	stream := EncodeMIDI([]byte{0x03, 0x8F, 0x00})
	var noisy []byte
	for _, b := range stream {
		noisy = append(noisy, 0xF8, b, 0xFE)
	}
	noisy = append(noisy, 0xFF) // system reset, also realtime
	mustOneFrame(t, feedAll(noisy), []byte{0x03, 0x8F, 0x00})
}

func TestDecoderTwoByteMessageDoesNotDesync(t *testing.T) {
	// A 0xC0 program change carries a single data byte. A fixed 3-byte stride
	// would be permanently misaligned after this.
	var s []byte
	s = append(s, StatusFrameStart, 0, 0)
	s = append(s, StatusFrameData, 0x00, 0x03)
	s = append(s, 0xC0, 0x7F) // 2-byte message
	s = append(s, StatusFrameData, 0x08, 0x0F)
	s = append(s, 0xD3, 0x40) // channel pressure, also 2 bytes
	s = append(s, StatusFrameData, 0x00, 0x00)
	s = append(s, StatusFrameEnd, 0, 0)
	mustOneFrame(t, feedAll(s), []byte{0x03, 0x8F, 0x00})
}

func TestDecoderRunningStatus(t *testing.T) {
	// After a complete channel message, bare data byte pairs repeat the last
	// status. All three data messages below share one 0x90 status byte.
	s := []byte{
		StatusFrameStart, 0, 0,
		StatusFrameData, 0x00, 0x03,
		0x08, 0x0F, // running status -> 0x8F
		0x00, 0x00, // running status -> 0x00
		StatusFrameEnd, 0, 0,
	}
	mustOneFrame(t, feedAll(s), []byte{0x03, 0x8F, 0x00})
}

func TestDecoderRunningStatusCancelledBySystemCommon(t *testing.T) {
	// A system common message cancels running status, so the data bytes that
	// follow have no status to attach to and must be discarded rather than
	// appended to the frame.
	s := []byte{
		StatusFrameStart, 0, 0,
		StatusFrameData, 0x00, 0x02,
		0xF6,       // tune request: system common, zero data bytes
		0x0A, 0x0B, // orphaned data bytes
		StatusFrameData, 0x00, 0x08,
		StatusFrameEnd, 0, 0,
	}
	mustOneFrame(t, feedAll(s), []byte{0x02, 0x08})
}

func TestDecoderMidFrameStartResetsAccumulator(t *testing.T) {
	var s []byte
	s = append(s, StatusFrameStart, 0, 0)
	s = append(s, StatusFrameData, 0x0F, 0x0F) // garbage 0xFF
	s = append(s, StatusFrameData, 0x0F, 0x0F)
	s = append(s, EncodeMIDI([]byte{0x02, 0x08})...) // a fresh start marker
	mustOneFrame(t, feedAll(s), []byte{0x02, 0x08})
}

func TestDecoderInvalidDeclaredLengths(t *testing.T) {
	// Build a frame body directly, bypassing the length rules the encoder obeys.
	build := func(body ...byte) []byte {
		var s []byte
		s = append(s, StatusFrameStart, 0, 0)
		for _, b := range body {
			s = append(s, StatusFrameData, (b>>4)&0x0F, b&0x0F)
		}
		return append(s, StatusFrameEnd, 0, 0)
	}
	long := make([]byte, 3)
	long[0] = 0x41 // 65: above the 64-byte receive cap

	cases := []struct {
		name string
		body []byte
	}{
		{"declared zero", []byte{0x00, 0x11}},
		{"declared one", []byte{0x01, 0x11}},
		{"declared above 64", long},
		{"declared beyond buffered", []byte{0x0A, 0x11, 0x22}},
		{"single byte buffered", []byte{0x02}},
		{"nothing buffered", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if frames := feedAll(build(c.body...)); len(frames) != 0 {
				t.Fatalf("got %v, want the frame dropped", frames)
			}
		})
	}
}

func TestDecoderOverlongFrameDispatchesTruncatedPrefix(t *testing.T) {
	// The reference caps the accumulator at 64 bytes without resetting it, so
	// an over-long frame whose declared length is <= 64 still dispatches
	// (SPEC.md §3.3). Reproduced deliberately.
	body := make([]byte, 70)
	body[0] = proto.MaxFrameLen
	for i := 1; i < len(body); i++ {
		body[i] = byte(i)
	}
	var s []byte
	s = append(s, StatusFrameStart, 0, 0)
	for _, b := range body {
		s = append(s, StatusFrameData, (b>>4)&0x0F, b&0x0F)
	}
	s = append(s, StatusFrameEnd, 0, 0)

	frames := feedAll(s)
	mustOneFrame(t, frames, body[:proto.MaxFrameLen])

	// And the following frame must still decode: the cap must not leave the
	// accumulator poisoned.
	d := NewDecoder()
	d.Feed(s)
	mustOneFrame(t, d.Feed(EncodeMIDI([]byte{0x02, 0x08})), []byte{0x02, 0x08})
}

func TestDecoderOverlongFrameWithOverlongDeclaredLengthIsDropped(t *testing.T) {
	body := make([]byte, 70)
	body[0] = 70 // > 64, so the receive path rejects it outright
	var s []byte
	s = append(s, StatusFrameStart, 0, 0)
	for _, b := range body {
		s = append(s, StatusFrameData, (b>>4)&0x0F, b&0x0F)
	}
	s = append(s, StatusFrameEnd, 0, 0)
	if frames := feedAll(s); len(frames) != 0 {
		t.Fatalf("got %v, want the frame dropped", frames)
	}
}

func TestDecoderSplitAcrossFeeds(t *testing.T) {
	stream := EncodeMIDI([]byte{0x04, 0x92, 0x2E, 0xE0})
	want := []byte{0x04, 0x92, 0x2E, 0xE0}

	t.Run("byte at a time", func(t *testing.T) {
		d := NewDecoder()
		var got [][]byte
		for _, b := range stream {
			got = append(got, d.Feed([]byte{b})...)
		}
		mustOneFrame(t, got, want)
	})

	t.Run("one message over three feeds", func(t *testing.T) {
		// Split precisely inside the third MIDI message.
		d := NewDecoder()
		var got [][]byte
		got = append(got, d.Feed(stream[:6])...)
		got = append(got, d.Feed(stream[6:7])...) // status only
		got = append(got, d.Feed(stream[7:8])...) // first data byte
		got = append(got, d.Feed(stream[8:])...)  // second data byte and the rest
		mustOneFrame(t, got, want)
	})

	t.Run("every split point", func(t *testing.T) {
		for i := 0; i <= len(stream); i++ {
			d := NewDecoder()
			got := append([][]byte{}, d.Feed(stream[:i])...)
			got = append(got, d.Feed(stream[i:])...)
			if len(got) != 1 || !bytes.Equal(got[0], want) {
				t.Fatalf("split at %d gave %v", i, got)
			}
		}
	})
}

func TestDecoderIgnoresUnrelatedMIDI(t *testing.T) {
	// Control change, pitch bend, MTC quarter frame, song position, song
	// select, tune request, and a SysEx run all pass through a shared port
	// without disturbing the frame in flight. The VFLEX never sends SysEx
	// (SPEC.md §3.1) but the parser must not desync if something else does.
	var s []byte
	s = append(s, 0xB0, 0x07, 0x64) // CC volume, before any frame
	s = append(s, StatusFrameStart, 0, 0)
	s = append(s, StatusFrameData, 0x00, 0x03)
	s = append(s, 0xB2, 0x0A, 0x40) // CC pan
	s = append(s, 0xE0, 0x00, 0x40) // pitch bend
	s = append(s, StatusFrameData, 0x08, 0x0F)
	s = append(s, 0xF1, 0x05)                         // MTC quarter frame, 1 data byte
	s = append(s, 0xF2, 0x01, 0x02)                   // song position, 2 data bytes
	s = append(s, 0xF3, 0x00)                         // song select, 1 data byte
	s = append(s, 0xF6)                               // tune request, 0 data bytes
	s = append(s, 0xF0, 0x7E, 0x00, 0x06, 0x01, 0xF7) // SysEx identity request
	s = append(s, StatusFrameData, 0x00, 0x00)
	s = append(s, StatusFrameEnd, 0, 0)
	s = append(s, 0xB0, 0x79, 0x00) // CC reset-all-controllers, after
	mustOneFrame(t, feedAll(s), []byte{0x03, 0x8F, 0x00})
}

func TestDecoderTruncatedSysExRecovers(t *testing.T) {
	// SysEx is unbounded; only a status byte ends it. A truncated run must not
	// swallow the frame that follows.
	var s []byte
	s = append(s, 0xF0, 0x01, 0x02, 0x03) // no terminating 0xF7
	s = append(s, EncodeMIDI([]byte{0x02, 0x08})...)
	mustOneFrame(t, feedAll(s), []byte{0x02, 0x08})
}

func TestDecoderTrailingPartialMessageIgnored(t *testing.T) {
	stream := EncodeMIDI([]byte{0x02, 0x08})
	stream = append(stream, StatusFrameData, 0x00) // half a message, no more
	mustOneFrame(t, feedAll(stream), []byte{0x02, 0x08})
}

func TestDecoderMultipleFramesInOneFeed(t *testing.T) {
	var s []byte
	s = append(s, EncodeMIDI([]byte{0x02, 0x08})...)
	s = append(s, EncodeMIDI([]byte{0x02, 0x0B})...)
	s = append(s, EncodeMIDI([]byte{0x04, 0x12, 0x2E, 0xE0})...)
	frames := feedAll(s)
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	want := [][]byte{{0x02, 0x08}, {0x02, 0x0B}, {0x04, 0x12, 0x2E, 0xE0}}
	for i := range want {
		if !bytes.Equal(frames[i], want[i]) {
			t.Errorf("frame %d = %s, want %s", i, proto.Hex(frames[i]), proto.Hex(want[i]))
		}
	}
}

func TestDecoderFramesAreCopies(t *testing.T) {
	d := NewDecoder()
	first := d.Feed(EncodeMIDI([]byte{0x02, 0x08}))
	saved := first[0]
	d.Feed(EncodeMIDI([]byte{0x02, 0x0B}))
	if !bytes.Equal(saved, []byte{0x02, 0x08}) {
		t.Fatalf("earlier frame was clobbered: %s", proto.Hex(saved))
	}
}

func TestDecoderReset(t *testing.T) {
	d := NewDecoder()
	d.Feed([]byte{StatusFrameStart, 0, 0, StatusFrameData, 0x0F, 0x0F, StatusFrameData})
	d.Reset()
	// A stale partial message and a stale accumulator must both be gone.
	mustOneFrame(t, d.Feed(EncodeMIDI([]byte{0x02, 0x08})), []byte{0x02, 0x08})
}

func TestDecoderDropHook(t *testing.T) {
	var reasons []string
	d := NewDecoder()
	d.SetDropHook(func(reason string, buffered []byte) {
		reasons = append(reasons, reason)
	})
	// Declared length 10 with only 3 bytes buffered: silently dropped by the
	// reference client, which is why the hook exists.
	d.Feed([]byte{
		StatusFrameStart, 0, 0,
		StatusFrameData, 0x00, 0x0A,
		StatusFrameData, 0x01, 0x01,
		StatusFrameData, 0x02, 0x02,
		StatusFrameEnd, 0, 0,
	})
	if len(reasons) != 1 {
		t.Fatalf("drop hook called %d times, want 1: %v", len(reasons), reasons)
	}
	if !strings.Contains(reasons[0], "10") {
		t.Errorf("reason %q does not mention the declared length", reasons[0])
	}
}

// ---------------------------------------------------------------------------
// Fuzz
// ---------------------------------------------------------------------------

// FuzzDecoder asserts the two invariants that matter: the decoder never panics
// on arbitrary input, and it never emits a frame the protocol would consider
// invalid.
func FuzzDecoder(f *testing.F) {
	for _, v := range golden {
		clean := strings.NewReplacer(" ", "", "|", "").Replace(v.midi)
		b, err := hex.DecodeString(clean)
		if err != nil {
			f.Fatalf("bad golden vector %q: %v", v.midi, err)
		}
		f.Add(b)
	}
	f.Add([]byte{})
	f.Add([]byte{0x90, 0x00, 0x02})
	f.Add([]byte{0xF0, 0x01, 0x02})
	f.Add([]byte{0x80, 0x00, 0x00, 0xA0, 0x00, 0x00})
	f.Add(bytes.Repeat([]byte{0x90, 0x0F, 0x0F}, 100))

	f.Fuzz(func(t *testing.T, data []byte) {
		d := NewDecoder()
		var frames [][]byte
		// Feed in irregular chunks so buffer-split handling is fuzzed too.
		for i := 0; i < len(data); {
			n := 1 + int(data[i])%7
			if i+n > len(data) {
				n = len(data) - i
			}
			frames = append(frames, d.Feed(data[i:i+n])...)
			i += n
		}
		for _, fr := range frames {
			if !proto.ValidResponseLen(len(fr)) {
				t.Fatalf("emitted a frame of invalid length %d: %s", len(fr), proto.Hex(fr))
			}
			if int(fr[0]) != len(fr) {
				t.Fatalf("frame declares length %d but is %d bytes: %s", fr[0], len(fr), proto.Hex(fr))
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Framer
// ---------------------------------------------------------------------------

// memTransport is an in-memory proto.Transport. Writes are recorded; reads are
// fed from a channel. It deliberately splits reads at arbitrary boundaries.
type memTransport struct {
	rx     chan []byte
	closed chan struct{}

	mu        sync.Mutex
	writes    [][]byte
	lastWrite time.Time
	failAt    int // 1-based index of the write that fails; 0 means never
	writeErr  error

	readErr   error // returned once rx is closed
	pending   []byte
	closeErr  error
	closeOnce sync.Once
}

func newMemTransport() *memTransport {
	return &memTransport{
		rx:       make(chan []byte, 8),
		closed:   make(chan struct{}),
		writeErr: errors.New("simulated write failure"),
		readErr:  io.EOF,
	}
}

func (m *memTransport) WriteMIDI(p []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes = append(m.writes, append([]byte(nil), p...))
	m.lastWrite = time.Now()
	if m.failAt > 0 && len(m.writes) == m.failAt {
		return m.writeErr
	}
	return nil
}

func (m *memTransport) ReadMIDI(p []byte) (int, error) {
	// Only the framer's single reader goroutine calls this, so pending needs
	// no lock (proto.Transport permits one reader and one writer).
	if len(m.pending) == 0 {
		select {
		case b, ok := <-m.rx:
			if !ok {
				return 0, m.readErr
			}
			m.pending = b
		case <-m.closed:
			return 0, os.ErrClosed
		}
	}
	n := copy(p, m.pending)
	m.pending = m.pending[n:]
	return n, nil
}

func (m *memTransport) Name() string { return "mem" }

func (m *memTransport) Close() error {
	m.closeOnce.Do(func() { close(m.closed) })
	return m.closeErr
}

func (m *memTransport) written() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.writes...)
}

func (m *memTransport) lastWriteAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastWrite
}

func TestFramerSendFrameEmitsEveryMessage(t *testing.T) {
	for _, v := range golden {
		t.Run(v.name, func(t *testing.T) {
			tr := newMemTransport()
			f := New(tr, 0)
			defer f.Close()

			frame := h(t, v.frame)
			if err := f.SendFrame(context.Background(), frame); err != nil {
				t.Fatalf("SendFrame: %v", err)
			}
			got := tr.written()
			want := MIDIMessages(frame)
			if len(got) != len(want) {
				t.Fatalf("wrote %d messages, want %d", len(got), len(want))
			}
			for i := range want {
				if !bytes.Equal(got[i], want[i]) {
					t.Errorf("message %d = % x, want % x", i, got[i], want[i])
				}
			}
		})
	}
}

func TestFramerSendFramePacing(t *testing.T) {
	const delay = 30 * time.Millisecond
	tr := newMemTransport()
	f := New(tr, delay)
	defer f.Close()

	frame := []byte{0x02, 0x08} // 4 messages, so 3 inter-message delays
	start := time.Now()
	if err := f.SendFrame(context.Background(), frame); err != nil {
		t.Fatalf("SendFrame: %v", err)
	}
	elapsed := time.Since(start)
	returned := time.Now()

	if want := 3 * delay; elapsed < want {
		t.Errorf("took %v, want at least %v (one delay after the start marker and after each data byte)", elapsed, want)
	}
	// SPEC.md §3.1: no delay after the end marker. If there were one, the gap
	// between the final write and the return would be a full byteDelay.
	if gap := returned.Sub(tr.lastWriteAt()); gap > delay/2 {
		t.Errorf("returned %v after the final write; there must be no trailing delay", gap)
	}
}

func TestFramerSendFrameNoDelayWhenZero(t *testing.T) {
	tr := newMemTransport()
	f := New(tr, 0)
	defer f.Close()
	start := time.Now()
	if err := f.SendFrame(context.Background(), []byte{0x06, 0x97, 0xBB, 0x80, 0x0C, 0xE4}); err != nil {
		t.Fatalf("SendFrame: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("byteDelay 0 still took %v", elapsed)
	}
}

func TestFramerSendFrameContextCancel(t *testing.T) {
	tr := newMemTransport()
	f := New(tr, time.Second)
	defer f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := f.SendFrame(ctx, []byte{0x02, 0x08})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want a wrapped context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("cancellation took %v; it must interrupt the inter-message delay", elapsed)
	}
	if n := len(tr.written()); n == 0 || n > 2 {
		t.Errorf("wrote %d messages before cancelling, want 1 or 2", n)
	}
}

func TestFramerSendFrameSurfacesWriteError(t *testing.T) {
	// The vendor client swallows write failures and lets them surface five
	// seconds later as a generic timeout (SPEC.md §7). We do not.
	tr := newMemTransport()
	tr.failAt = 2
	f := New(tr, 0)
	defer f.Close()

	err := f.SendFrame(context.Background(), []byte{0x02, 0x08})
	if !errors.Is(err, tr.writeErr) {
		t.Fatalf("got %v, want the transport's write error wrapped", err)
	}
	if !strings.Contains(err.Error(), "2/4") {
		t.Errorf("error %q should identify which message failed", err)
	}
}

func TestFramerReceivesFrames(t *testing.T) {
	tr := newMemTransport()
	f := New(tr, 0)
	defer f.Close()
	f.Start()
	f.Start() // idempotent

	stream := EncodeMIDI([]byte{0x0A, 0x08, 'V', 'F', '0', '0', '1', '2', '3', '4'})
	// Deliver in awkward slices so the decoder must carry state across reads.
	tr.rx <- stream[:5]
	tr.rx <- stream[5:6]
	tr.rx <- stream[6:]

	select {
	case frame := <-f.Frames():
		want := []byte{0x0A, 0x08, 'V', 'F', '0', '0', '1', '2', '3', '4'}
		if !bytes.Equal(frame, want) {
			t.Fatalf("got %s, want %s", proto.Hex(frame), proto.Hex(want))
		}
	case err := <-f.Errors():
		t.Fatalf("unexpected error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a frame")
	}
}

func TestFramerReportsReadError(t *testing.T) {
	tr := newMemTransport()
	tr.readErr = errors.New("device went away")
	f := New(tr, 0)
	defer f.Close()
	f.Start()

	close(tr.rx)

	select {
	case err := <-f.Errors():
		if !errors.Is(err, tr.readErr) {
			t.Fatalf("got %v, want the transport's read error wrapped", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the read error")
	}
	// A read error is terminal: the reader stops and the channels close.
	select {
	case _, ok := <-f.Frames():
		if ok {
			t.Fatal("Frames() yielded a frame after a terminal read error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Frames() was not closed after a terminal read error")
	}
}

func TestFramerCloseStopsReader(t *testing.T) {
	tr := newMemTransport()
	f := New(tr, 0)
	f.Start()

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	select {
	case _, ok := <-f.Frames():
		if ok {
			t.Fatal("Frames() yielded a frame after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader goroutine did not exit on Close")
	}
	// The error caused by closing the transport must not be reported.
	select {
	case err, ok := <-f.Errors():
		if ok {
			t.Fatalf("Close reported a spurious error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Errors() was not closed")
	}
}

func TestFramerCloseWithoutStart(t *testing.T) {
	f := New(newMemTransport(), 0)
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := <-f.Frames(); ok {
		t.Fatal("Frames() should be closed and empty")
	}
	if _, ok := <-f.Errors(); ok {
		t.Fatal("Errors() should be closed and empty")
	}
	f.Start() // must not resurrect the reader
	if err := f.SendFrame(context.Background(), []byte{0x02, 0x08}); !errors.Is(err, ErrClosed) {
		t.Fatalf("SendFrame after Close = %v, want ErrClosed", err)
	}
}

func TestFramerCloseReportsTransportError(t *testing.T) {
	tr := newMemTransport()
	tr.closeErr = errors.New("ioctl failed")
	f := New(tr, 0)
	if err := f.Close(); !errors.Is(err, tr.closeErr) {
		t.Fatalf("got %v, want the transport's close error wrapped", err)
	}
}

func TestFramerConcurrentSends(t *testing.T) {
	// Frames must not interleave on the wire even if two goroutines send at
	// once. (The session layer serialises anyway, but the framer must not be
	// the thing that corrupts a frame.)
	tr := newMemTransport()
	f := New(tr, time.Millisecond)
	defer f.Close()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.SendFrame(context.Background(), []byte{0x02, 0x08}); err != nil {
				t.Errorf("SendFrame: %v", err)
			}
		}()
	}
	wg.Wait()

	msgs := tr.written()
	if len(msgs) != 4*4 {
		t.Fatalf("wrote %d messages, want 16", len(msgs))
	}
	for i, m := range msgs {
		var want byte
		switch i % 4 {
		case 0:
			want = StatusFrameStart
		case 3:
			want = StatusFrameEnd
		default:
			want = StatusFrameData
		}
		if m[0] != want {
			t.Fatalf("message %d has status %#02x, want %#02x: frames interleaved", i, m[0], want)
		}
	}
}

// TestFramerRoundTripThroughDecoder checks that what SendFrame puts on the wire
// is exactly what the receive path reconstructs.
func TestFramerRoundTripThroughDecoder(t *testing.T) {
	tr := newMemTransport()
	f := New(tr, 0)
	defer f.Close()

	frames := [][]byte{
		{0x02, 0x08},
		{0x04, 0x92, 0x2E, 0xE0},
		{0x06, 0x97, 0xBB, 0x80, 0x0C, 0xE4},
	}
	for _, fr := range frames {
		if err := f.SendFrame(context.Background(), fr); err != nil {
			t.Fatalf("SendFrame: %v", err)
		}
	}
	var wire []byte
	for _, m := range tr.written() {
		wire = append(wire, m...)
	}
	got := feedAll(wire)
	if len(got) != len(frames) {
		t.Fatalf("decoded %d frames, want %d", len(got), len(frames))
	}
	for i := range frames {
		if !bytes.Equal(got[i], frames[i]) {
			t.Errorf("frame %d = %s, want %s", i, proto.Hex(got[i]), proto.Hex(frames[i]))
		}
	}
}

// idleTransport always reports "nothing available" instantly, the way usbmidi
// does for a zero-length IN packet.
type idleTransport struct {
	calls  atomic.Int64
	closed chan struct{}
	once   sync.Once
}

func newIdleTransport() *idleTransport {
	return &idleTransport{closed: make(chan struct{})}
}

func (t *idleTransport) WriteMIDI([]byte) error { return nil }
func (t *idleTransport) ReadMIDI(p []byte) (int, error) {
	select {
	case <-t.closed:
		return 0, io.EOF
	default:
	}
	t.calls.Add(1)
	return 0, nil
}
func (t *idleTransport) Name() string { return "idle" }
func (t *idleTransport) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

// TestReadLoopDoesNotSpinOnIdle is a regression test: readLoop used to re-enter
// ReadMIDI immediately when a transport returned (0, nil), which pinned a core
// for as long as the device stayed quiet. usbmidi returns exactly that for a
// zero-length IN packet and for a transfer of nothing but padding, both of
// which complete instantly.
func TestReadLoopDoesNotSpinOnIdle(t *testing.T) {
	tr := newIdleTransport()
	f := New(tr, 0)
	f.Start()
	time.Sleep(100 * time.Millisecond)
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	n := tr.calls.Load()
	// With a 2 ms backoff, 100 ms permits roughly 50 calls. Without any
	// backoff this ran into the hundreds of thousands.
	if n > 1000 {
		t.Errorf("ReadMIDI called %d times in 100ms; the read loop is spinning", n)
	}
	if n == 0 {
		t.Error("ReadMIDI was never called; the read loop is not running")
	}
	t.Logf("ReadMIDI calls in 100ms: %d", n)
}

// ---------------------------------------------------------------------------
// Close as a barrier
// ---------------------------------------------------------------------------

// lateReadTransport models the case Close has to survive: a read that was
// already inside the transport when Close ran and takes a while to come back,
// delivering one last frame on its way out. rawmidi does exactly this -- the
// pending read is evicted from the netpoller by Close and unwinds afterwards --
// and it is why the descriptor is not released the instant Close returns.
type lateReadTransport struct {
	closed  chan struct{}
	entered chan struct{}
	stream  []byte
	delay   time.Duration

	once   sync.Once
	enter  sync.Once
	served atomic.Bool
}

func newLateReadTransport(stream []byte, delay time.Duration) *lateReadTransport {
	return &lateReadTransport{
		closed:  make(chan struct{}),
		entered: make(chan struct{}),
		stream:  stream,
		delay:   delay,
	}
}

func (t *lateReadTransport) ReadMIDI(p []byte) (int, error) {
	t.enter.Do(func() { close(t.entered) })
	<-t.closed
	if t.served.Load() {
		return 0, io.EOF
	}
	// The read was already in the kernel when the close landed, so it returns
	// its bytes rather than an error -- and only after taking its time.
	time.Sleep(t.delay)
	t.served.Store(true)
	return copy(p, t.stream), nil
}

func (t *lateReadTransport) WriteMIDI([]byte) error { return nil }
func (t *lateReadTransport) Name() string           { return "late" }
func (t *lateReadTransport) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

// TestFramerCloseIsABarrier is the regression test for a Close that signalled
// the reader and returned without waiting for it. The reader could then still
// be inside ReadMIDI -- and still publishing on Frames() -- after Close had
// returned, so a caller that closed a session and immediately reopened the
// device (SPEC.md §10.1's firmware flow, and `gflex scan`'s unplug handover)
// was racing a goroutine still holding the old descriptor.
func TestFramerCloseIsABarrier(t *testing.T) {
	const delay = 100 * time.Millisecond
	tr := newLateReadTransport(EncodeMIDI([]byte{0x02, 0x08}), delay)
	f := New(tr, 0)
	f.Start()
	<-tr.entered // the reader is parked in ReadMIDI

	start := time.Now()
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Fatalf("Close returned after %s, before the reader's %s read could have finished", elapsed, delay)
	}

	assertReaderGone(t, f)
}

// assertReaderGone checks that readLoop has already exited, without blocking:
// draining Frames() must reach the closed channel through nothing but frames
// that were in flight when Close ran. A receive that would have to wait means
// the reader is still alive and Close was not a barrier.
func assertReaderGone(t *testing.T, f *Framer) {
	t.Helper()
	for {
		select {
		case _, ok := <-f.Frames():
			if !ok {
				return
			}
		default:
			t.Fatal("Close returned while the reader goroutine was still running")
		}
	}
}

// stuckTransport never lets go of a read, which is the case Close deliberately
// does not wait out.
type stuckTransport struct {
	release chan struct{} // closed by the test, never by Close
	entered chan struct{}
	enter   sync.Once
}

func newStuckTransport() *stuckTransport {
	return &stuckTransport{release: make(chan struct{}), entered: make(chan struct{})}
}

func (t *stuckTransport) ReadMIDI([]byte) (int, error) {
	t.enter.Do(func() { close(t.entered) })
	<-t.release
	return 0, io.EOF
}

func (t *stuckTransport) WriteMIDI([]byte) error { return nil }
func (t *stuckTransport) Name() string           { return "stuck" }
func (t *stuckTransport) Close() error           { return nil }

// TestFramerCloseReportsStuckReader covers the other half of the judgement: a
// transport whose read Close cannot interrupt must not hang the caller. usbfs
// cannot abort an ioctl the kernel has already submitted, so this is reachable
// on real hardware; a Close that blocked forever there would be worse than one
// that is not a barrier.
func TestFramerCloseReportsStuckReader(t *testing.T) {
	tr := newStuckTransport()
	f := New(tr, 0)
	f.closeGrace = 50 * time.Millisecond
	f.Start()
	<-tr.entered
	// Let the leaked goroutine go at the end of the test, so -race does not see
	// it outlive the run.
	t.Cleanup(func() { close(tr.release) })

	done := make(chan error, 1)
	go func() { done <- f.Close() }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrReaderStuck) {
			t.Fatalf("Close = %v, want ErrReaderStuck", err)
		}
		if !strings.Contains(err.Error(), "stuck.ReadMIDI") {
			t.Errorf("Close error does not name the transport: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung on a reader it cannot interrupt")
	}
}

// TestFramerCloseStuckReaderStillReportsTransportError checks that giving up on
// the reader does not swallow the transport's own close error, which for
// usbmidi is the one that says the ALSA MIDI port may stay missing until the
// user replugs the device.
func TestFramerCloseStuckReaderStillReportsTransportError(t *testing.T) {
	tr := newStuckTransport()
	f := New(&closeErrTransport{stuckTransport: tr, err: errors.New("release interface failed")}, 0)
	f.closeGrace = 50 * time.Millisecond
	f.Start()
	<-tr.entered
	t.Cleanup(func() { close(tr.release) })

	err := f.Close()
	if !errors.Is(err, ErrReaderStuck) {
		t.Errorf("Close = %v, want it to report ErrReaderStuck", err)
	}
	if !strings.Contains(err.Error(), "release interface failed") {
		t.Errorf("Close = %v, want it to also report the transport's close error", err)
	}
}

// closeErrTransport is a stuckTransport whose Close fails.
type closeErrTransport struct {
	*stuckTransport
	err error
}

func (t *closeErrTransport) Close() error { return t.err }

// TestFramerSecondCloseIsAlsoABarrier: the barrier has to hold for whoever
// calls Close, not just for whoever gets there first. A session and a deferred
// cleanup path both call it.
func TestFramerSecondCloseIsAlsoABarrier(t *testing.T) {
	tr := newLateReadTransport(EncodeMIDI([]byte{0x02, 0x08}), 50*time.Millisecond)
	f := New(tr, 0)
	f.Start()
	<-tr.entered

	first := make(chan struct{})
	go func() {
		defer close(first)
		if err := f.Close(); err != nil {
			t.Errorf("first Close: %v", err)
		}
	}()
	// Racing the first Close is the point: this one must not return early just
	// because closed is already set.
	if err := f.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	assertReaderGone(t, f)
	<-first
}
