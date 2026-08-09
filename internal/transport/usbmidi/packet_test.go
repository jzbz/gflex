package usbmidi

import (
	"bytes"
	"testing"

	"github.com/jzbz/gflex/internal/proto"
)

// The three message forms the VFLEX protocol ever produces (SPEC.md §4.2).
func TestPackPacketVFLEXMessages(t *testing.T) {
	tests := []struct {
		name string
		msg  []byte
		want []byte
	}{
		{"start of frame", []byte{0x80, 0x00, 0x00}, []byte{0x08, 0x80, 0x00, 0x00}},
		{"data byte", []byte{0x90, 0x0E, 0x00}, []byte{0x09, 0x90, 0x0E, 0x00}},
		{"end of frame", []byte{0xA0, 0x00, 0x00}, []byte{0x0A, 0xA0, 0x00, 0x00}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PackPacket(tc.msg)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("PackPacket(%s) = %s, want %s", proto.Hex(tc.msg), proto.Hex(got), proto.Hex(tc.want))
			}
			if back := UnpackPackets(got); !bytes.Equal(back, tc.msg) {
				t.Fatalf("round trip = %s, want %s", proto.Hex(back), proto.Hex(tc.msg))
			}
		})
	}
}

func TestPackPacketMessageClasses(t *testing.T) {
	tests := []struct {
		name string
		msg  []byte
		want []byte
	}{
		// Channel voice: CIN == status high nibble, channel nibble irrelevant.
		{"note off ch 3", []byte{0x83, 0x40, 0x7F}, []byte{0x08, 0x83, 0x40, 0x7F}},
		{"control change", []byte{0xB0, 0x07, 0x64}, []byte{0x0B, 0xB0, 0x07, 0x64}},
		{"pitch bend", []byte{0xE0, 0x00, 0x40}, []byte{0x0E, 0xE0, 0x00, 0x40}},
		// Two-byte channel voice.
		{"program change", []byte{0xC0, 0x05}, []byte{0x0C, 0xC0, 0x05, 0x00}},
		{"channel pressure", []byte{0xD9, 0x33}, []byte{0x0D, 0xD9, 0x33, 0x00}},
		// System common.
		{"mtc quarter frame", []byte{0xF1, 0x21}, []byte{0x02, 0xF1, 0x21, 0x00}},
		{"song position", []byte{0xF2, 0x10, 0x20}, []byte{0x03, 0xF2, 0x10, 0x20}},
		{"song select", []byte{0xF3, 0x02}, []byte{0x02, 0xF3, 0x02, 0x00}},
		{"tune request", []byte{0xF6}, []byte{0x05, 0xF6, 0x00, 0x00}},
		// System realtime.
		{"clock", []byte{0xF8}, []byte{0x05, 0xF8, 0x00, 0x00}},
		{"active sensing", []byte{0xFE}, []byte{0x05, 0xFE, 0x00, 0x00}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PackPacket(tc.msg)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("PackPacket(%s) = %s, want %s", proto.Hex(tc.msg), proto.Hex(got), proto.Hex(tc.want))
			}
			// cinLength must agree with the encoder, or unpacking loses bytes.
			if back := UnpackPackets(got); !bytes.Equal(back, tc.msg) {
				t.Fatalf("round trip = %s, want %s", proto.Hex(back), proto.Hex(tc.msg))
			}
		})
	}
}

func TestPackPacketRejectsUnclassifiable(t *testing.T) {
	tests := []struct {
		name string
		msg  []byte
	}{
		{"empty", nil},
		{"running status", []byte{0x40, 0x7F}},
		{"too long", []byte{0x90, 0x01, 0x02, 0x03}},
		{"note on missing velocity", []byte{0x90, 0x01}},
		{"program change with two data bytes", []byte{0xC0, 0x01, 0x02}},
		{"sysex start", []byte{0xF0, 0x7E, 0x00}},
		{"end of sysex", []byte{0xF7}},
		{"undefined f4", []byte{0xF4}},
		{"undefined f5", []byte{0xF5}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PackPacket(tc.msg); got != nil {
				t.Fatalf("PackPacket(%s) = %s, want nil", proto.Hex(tc.msg), proto.Hex(got))
			}
		})
	}
}

func TestUnpackPacketsCINLengths(t *testing.T) {
	// One packet per CIN, all data slots distinct so a wrong length is visible.
	var buf []byte
	for cin := 0; cin < 16; cin++ {
		buf = append(buf, byte(cin), 0xA1, 0xB2, 0xC3)
	}
	got := UnpackPackets(buf)

	var want []byte
	for cin := 0; cin < 16; cin++ {
		if cin == 0 {
			continue // byte0 == 0x00 is padding, skipped entirely
		}
		full := []byte{0xA1, 0xB2, 0xC3}
		want = append(want, full[:cinLength[cin]]...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("UnpackPackets = %s, want %s", proto.Hex(got), proto.Hex(want))
	}
}

func TestUnpackPacketsMixedBuffer(t *testing.T) {
	buf := []byte{
		0x08, 0x80, 0x00, 0x00, // start of frame
		0x00, 0x00, 0x00, 0x00, // zero padding, skipped
		0x09, 0x90, 0x00, 0x02, // data byte 0x02
		0x05, 0xF8, 0x00, 0x00, // single-byte realtime clock
		0x0C, 0xC0, 0x07, 0x00, // two-byte program change
		0x0A, 0xA0, 0x00, 0x00, // end of frame
		0x09, 0x90, 0x00, // trailing partial packet, ignored
	}
	want := []byte{
		0x80, 0x00, 0x00,
		0x90, 0x00, 0x02,
		0xF8,
		0xC0, 0x07,
		0xA0, 0x00, 0x00,
	}
	if got := UnpackPackets(buf); !bytes.Equal(got, want) {
		t.Fatalf("UnpackPackets = %s, want %s", proto.Hex(got), proto.Hex(want))
	}
}

func TestUnpackPacketsEmptyAndShort(t *testing.T) {
	for _, in := range [][]byte{nil, {}, {0x09}, {0x09, 0x90, 0x00}} {
		if got := UnpackPackets(in); len(got) != 0 {
			t.Fatalf("UnpackPackets(%s) = %s, want empty", proto.Hex(in), proto.Hex(got))
		}
	}
	// A run of pure padding yields nothing but is not an error.
	if got := UnpackPackets(make([]byte, 16)); len(got) != 0 {
		t.Fatalf("UnpackPackets(padding) = %s, want empty", proto.Hex(got))
	}
}

// goldenVectors are the frame -> MIDI stream pairs of SPEC.md §15.
var goldenVectors = []struct {
	name  string
	frame []byte
	midi  []byte
}{
	{
		"read serial number",
		[]byte{0x02, 0x08},
		[]byte{
			0x80, 0x00, 0x00,
			0x90, 0x00, 0x02,
			0x90, 0x00, 0x08,
			0xA0, 0x00, 0x00,
		},
	},
	{
		"set voltage 12.000 V",
		[]byte{0x04, 0x92, 0x2E, 0xE0},
		[]byte{
			0x80, 0x00, 0x00,
			0x90, 0x00, 0x04,
			0x90, 0x09, 0x02,
			0x90, 0x02, 0x0E,
			0x90, 0x0E, 0x00, // velocity 0 -- the Note-On hazard of SPEC.md §3.2
			0xA0, 0x00, 0x00,
		},
	},
	{
		"set current limit 5000 mA",
		[]byte{0x04, 0x93, 0x13, 0x88},
		[]byte{
			0x80, 0x00, 0x00,
			0x90, 0x00, 0x04,
			0x90, 0x09, 0x03,
			0x90, 0x01, 0x03,
			0x90, 0x08, 0x08,
			0xA0, 0x00, 0x00,
		},
	},
	{
		"set vlimit low=3300 high=48000",
		[]byte{0x06, 0x97, 0xBB, 0x80, 0x0C, 0xE4},
		[]byte{
			0x80, 0x00, 0x00,
			0x90, 0x00, 0x06,
			0x90, 0x09, 0x07,
			0x90, 0x0B, 0x0B,
			0x90, 0x08, 0x00,
			0x90, 0x00, 0x0C,
			0x90, 0x0E, 0x04,
			0xA0, 0x00, 0x00,
		},
	},
	{
		"led always on",
		[]byte{0x03, 0x8F, 0x00},
		[]byte{
			0x80, 0x00, 0x00,
			0x90, 0x00, 0x03,
			0x90, 0x08, 0x0F,
			0x90, 0x00, 0x00,
			0xA0, 0x00, 0x00,
		},
	},
	{
		"jump to bootloader",
		[]byte{0x02, 0x14},
		[]byte{
			0x80, 0x00, 0x00,
			0x90, 0x00, 0x02,
			0x90, 0x01, 0x04,
			0xA0, 0x00, 0x00,
		},
	},
}

// nibbleEncode reproduces the framer's MIDI encoding (SPEC.md §3.1) locally, so
// this package's golden data can be validated without importing framer.
func nibbleEncode(frame []byte) []byte {
	out := []byte{0x80, 0x00, 0x00}
	for _, b := range frame {
		out = append(out, 0x90, (b>>4)&0x0F, b&0x0F)
	}
	return append(out, 0xA0, 0x00, 0x00)
}

func TestGoldenVectorsPackToUSBMIDI(t *testing.T) {
	for _, tc := range goldenVectors {
		t.Run(tc.name, func(t *testing.T) {
			// Confirms the hardcoded MIDI stream really is the encoding of the
			// hardcoded frame, so a typo in the table cannot pass silently.
			if enc := nibbleEncode(tc.frame); !bytes.Equal(enc, tc.midi) {
				t.Fatalf("golden data disagrees: encode(%s) = %s, table says %s",
					proto.Hex(tc.frame), proto.Hex(enc), proto.Hex(tc.midi))
			}

			msgs, err := splitMessages(tc.midi)
			if err != nil {
				t.Fatalf("splitMessages: %v", err)
			}
			if want := len(tc.frame) + 2; len(msgs) != want {
				t.Fatalf("split into %d messages, want %d", len(msgs), want)
			}

			var packets []byte
			for _, m := range msgs {
				p := PackPacket(m)
				if p == nil {
					t.Fatalf("PackPacket(%s) = nil", proto.Hex(m))
				}
				packets = append(packets, p...)
			}
			if len(packets) != 4*len(msgs) {
				t.Fatalf("got %d packet bytes for %d messages", len(packets), len(msgs))
			}
			if back := UnpackPackets(packets); !bytes.Equal(back, tc.midi) {
				t.Fatalf("round trip = %s, want %s", proto.Hex(back), proto.Hex(tc.midi))
			}
		})
	}
}

// The one USB-level packet stream SPEC.md §15 spells out in full.
func TestGoldenSetVoltagePacketStream(t *testing.T) {
	const idx = 1 // "set voltage 12.000 V"
	want := []byte{
		0x08, 0x80, 0x00, 0x00,
		0x09, 0x90, 0x00, 0x04,
		0x09, 0x90, 0x09, 0x02,
		0x09, 0x90, 0x02, 0x0E,
		0x09, 0x90, 0x0E, 0x00,
		0x0A, 0xA0, 0x00, 0x00,
	}
	msgs, err := splitMessages(goldenVectors[idx].midi)
	if err != nil {
		t.Fatalf("splitMessages: %v", err)
	}
	var got []byte
	for _, m := range msgs {
		got = append(got, PackPacket(m)...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("packets = %s, want %s", proto.Hex(got), proto.Hex(want))
	}
}

func TestSplitMessages(t *testing.T) {
	t.Run("running status", func(t *testing.T) {
		msgs, err := splitMessages([]byte{0x90, 0x00, 0x02, 0x00, 0x08})
		if err != nil {
			t.Fatalf("splitMessages: %v", err)
		}
		want := [][]byte{{0x90, 0x00, 0x02}, {0x90, 0x00, 0x08}}
		if len(msgs) != len(want) {
			t.Fatalf("got %d messages, want %d", len(msgs), len(want))
		}
		for i := range want {
			if !bytes.Equal(msgs[i], want[i]) {
				t.Fatalf("message %d = %s, want %s", i, proto.Hex(msgs[i]), proto.Hex(want[i]))
			}
		}
	})

	t.Run("realtime interleaved mid-message", func(t *testing.T) {
		msgs, err := splitMessages([]byte{0x90, 0x00, 0xF8, 0x02})
		if err != nil {
			t.Fatalf("splitMessages: %v", err)
		}
		want := [][]byte{{0xF8}, {0x90, 0x00, 0x02}}
		if len(msgs) != len(want) {
			t.Fatalf("got %d messages, want %d", len(msgs), len(want))
		}
		for i := range want {
			if !bytes.Equal(msgs[i], want[i]) {
				t.Fatalf("message %d = %s, want %s", i, proto.Hex(msgs[i]), proto.Hex(want[i]))
			}
		}
	})

	t.Run("errors", func(t *testing.T) {
		bad := map[string][]byte{
			"trailing partial":        {0x90, 0x00},
			"leading data":            {0x02, 0x03},
			"truncated by new status": {0x90, 0x00, 0x80, 0x00, 0x00},
			"sysex":                   {0xF0, 0x7E, 0xF7},
		}
		for name, in := range bad {
			if _, err := splitMessages(in); err == nil {
				t.Errorf("%s: splitMessages(%s) = nil error, want error", name, proto.Hex(in))
			}
		}
	})

	t.Run("empty", func(t *testing.T) {
		msgs, err := splitMessages(nil)
		if err != nil || len(msgs) != 0 {
			t.Fatalf("splitMessages(nil) = %v, %v", msgs, err)
		}
	})
}

// Every byte value must survive the nibble encoding, packing and unpacking, in
// line with the encode->decode round trip SPEC.md §12 requires of the framer.
func TestAllByteValuesRoundTrip(t *testing.T) {
	frame := make([]byte, 0, 256)
	for i := 0; i < 256; i++ {
		frame = append(frame, byte(i))
	}
	midi := nibbleEncode(frame)
	msgs, err := splitMessages(midi)
	if err != nil {
		t.Fatalf("splitMessages: %v", err)
	}
	var packets []byte
	for _, m := range msgs {
		p := PackPacket(m)
		if p == nil {
			t.Fatalf("PackPacket(%s) = nil", proto.Hex(m))
		}
		packets = append(packets, p...)
	}
	if !bytes.Equal(UnpackPackets(packets), midi) {
		t.Fatal("nibble-encoded stream did not survive the USB-MIDI packet round trip")
	}
}
