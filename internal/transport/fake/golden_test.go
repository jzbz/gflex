package fake

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzbz/gflex/internal/proto"
)

// goldenPath locates testdata/golden/frames.json from this package directory.
const goldenPath = "../../../testdata/golden/frames.json"

// vector is one entry of testdata/golden/frames.json.
type vector struct {
	Name    string `json:"name"`
	Dir     string `json:"dir"`     // "tx" host->device, "rx" device->host
	Cmd     int    `json:"cmd"`     // command code, flag bits excluded
	Write   bool   `json:"write"`   // whether FlagWrite is set
	Payload string `json:"payload"` // hex, no separators
	Frame   string `json:"frame"`   // hex, no separators
	MIDI    string `json:"midi"`    // hex, space-separated 3-byte messages
	USBMIDI string `json:"usbmidi"` // hex, space-separated 4-byte event packets
	Dropped bool   `json:"dropped"` // the receive state machine must discard it
	Note    string `json:"note"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(goldenPath))
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var v []vector
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("parse golden vectors: %v", err)
	}
	if len(v) == 0 {
		t.Fatal("golden vector file is empty")
	}
	return v
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// TestGoldenVectors checks that every vector is internally consistent: the
// frame really is what the command, write flag and payload build; the MIDI
// stream really is that frame encoded; and decoding the MIDI stream gives the
// frame back. If any of those drift the file has rotted.
func TestGoldenVectors(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			frame := unhex(t, v.Frame)
			midi := unhex(t, v.MIDI)
			payload := unhex(t, v.Payload)

			if v.Dir != "tx" && v.Dir != "rx" {
				t.Errorf("dir = %q, want tx or rx", v.Dir)
			}

			// A well-formed vector must be exactly what the encoder produces
			// from its semantic fields. Malformed vectors exist precisely
			// because proto.Build cannot produce them.
			if !v.Dropped {
				built, err := proto.Build(proto.Cmd(v.Cmd), payload, v.Write, false)
				if err != nil {
					t.Fatalf("proto.Build: %v", err)
				}
				if !bytes.Equal(built, frame) {
					t.Errorf("frame = %x, proto.Build gives %x", frame, built)
				}
				parsed, err := proto.Parse(frame)
				if err != nil {
					t.Fatalf("proto.Parse: %v", err)
				}
				if parsed.Cmd != proto.Cmd(v.Cmd) || parsed.Write != v.Write {
					t.Errorf("parsed cmd=%d write=%v, want cmd=%d write=%v",
						parsed.Cmd, parsed.Write, v.Cmd, v.Write)
				}
				if !bytes.Equal(parsed.Payload, payload) {
					t.Errorf("parsed payload = %x, want %x", parsed.Payload, payload)
				}
			}

			if got := encodeFrameMIDI(frame); !bytes.Equal(got, midi) {
				t.Errorf("encoded MIDI = % x\nwant             % x", got, midi)
			}

			// Decode in one gulp.
			got := newFrameDecoder().feed(midi)
			if v.Dropped {
				if len(got) != 0 {
					t.Fatalf("decoded %d frames from a stream that must be dropped: %x", len(got), got)
				}
			} else {
				if len(got) != 1 {
					t.Fatalf("decoded %d frames, want 1", len(got))
				}
				if !bytes.Equal(got[0], frame) {
					t.Errorf("decoded %x, want %x", got[0], frame)
				}
			}

			// Decode one byte per call: a real read can split a MIDI message,
			// so the parser state has to survive across calls (SPEC.md §3.3).
			dec := newFrameDecoder()
			var split [][]byte
			for _, b := range midi {
				split = append(split, dec.feed([]byte{b})...)
			}
			if len(split) != len(got) {
				t.Fatalf("byte-at-a-time decode produced %d frames, whole-buffer produced %d", len(split), len(got))
			}
			for i := range split {
				if !bytes.Equal(split[i], got[i]) {
					t.Errorf("byte-at-a-time frame %d = %x, want %x", i, split[i], got[i])
				}
			}

			// Decode with system realtime bytes sprinkled between every byte:
			// clock and active sensing may appear anywhere, including between
			// the data bytes of another message, and must not disturb it.
			dec = newFrameDecoder()
			var noisy [][]byte
			for _, b := range midi {
				noisy = append(noisy, dec.feed([]byte{0xF8, b, 0xFE})...)
			}
			if len(noisy) != len(got) {
				t.Fatalf("realtime-interleaved decode produced %d frames, want %d", len(noisy), len(got))
			}
			for i := range noisy {
				if !bytes.Equal(noisy[i], got[i]) {
					t.Errorf("realtime-interleaved frame %d = %x, want %x", i, noisy[i], got[i])
				}
			}

			// The USB-MIDI form is the same messages, each in a 4-byte event
			// packet (SPEC.md §4.2).
			usb := unhex(t, v.USBMIDI)
			if packed := packUSBMIDI(t, midi); !bytes.Equal(packed, usb) {
				t.Errorf("packed USB-MIDI = % x\nwant                % x", packed, usb)
			}
			if unpacked := unpackUSBMIDI(t, usb); !bytes.Equal(unpacked, midi) {
				t.Errorf("unpacked USB-MIDI = % x\nwant                  % x", unpacked, midi)
			}
		})
	}
}

// TestGoldenVectorsCoverSpec pins the six vectors SPEC.md §15 publishes, so a
// future edit cannot quietly drop one.
func TestGoldenVectorsCoverSpec(t *testing.T) {
	want := map[string]string{
		"0208":         "800000 900002 900008 a00000",
		"04922ee0":     "800000 900004 900902 90020e 900e00 a00000",
		"04931388":     "800000 900004 900903 900103 900808 a00000",
		"0697bb800ce4": "800000 900006 900907 900b0b 900800 90000c 900e04 a00000",
		"038f00":       "800000 900003 90080f 900000 a00000",
		"0214":         "800000 900002 900104 a00000",
	}
	have := make(map[string]string)
	for _, v := range loadVectors(t) {
		have[strings.ToLower(v.Frame)] = strings.ToLower(v.MIDI)
	}
	for frame, midi := range want {
		got, ok := have[frame]
		if !ok {
			t.Errorf("frame %s from SPEC.md §15 is missing from the golden file", frame)
			continue
		}
		if got != midi {
			t.Errorf("frame %s: golden MIDI %q, SPEC.md §15 says %q", frame, got, midi)
		}
	}
}

// packUSBMIDI wraps each 3-byte MIDI message in a USB-MIDI event packet:
// byte0 = (cableNumber << 4) | codeIndexNumber, with cable 0 and the code index
// equal to the status nibble for these three message types (SPEC.md §4.2).
// It is written here rather than imported so the golden file is checked against
// a second implementation.
func packUSBMIDI(t *testing.T, midi []byte) []byte {
	t.Helper()
	if len(midi)%3 != 0 {
		t.Fatalf("golden MIDI is %d bytes, not a whole number of 3-byte messages", len(midi))
	}
	out := make([]byte, 0, len(midi)/3*4)
	for i := 0; i < len(midi); i += 3 {
		status := midi[i]
		if status < 0x80 {
			t.Fatalf("message at offset %d does not start with a status byte: %#x", i, status)
		}
		out = append(out, status>>4, status, midi[i+1], midi[i+2])
	}
	return out
}

// cinLength is the USB-MIDI code-index-number to byte-count table.
var cinLength = [16]int{0, 0, 2, 3, 3, 1, 2, 3, 3, 3, 3, 3, 2, 2, 3, 1}

// unpackUSBMIDI walks 4-byte packets, skipping empty ones, and returns the MIDI
// byte stream they carry.
func unpackUSBMIDI(t *testing.T, buf []byte) []byte {
	t.Helper()
	if len(buf)%4 != 0 {
		t.Fatalf("golden USB-MIDI is %d bytes, not a whole number of 4-byte packets", len(buf))
	}
	var out []byte
	for i := 0; i < len(buf); i += 4 {
		if buf[i] == 0 {
			continue // reserved/empty packet
		}
		out = append(out, buf[i+1:i+1+cinLength[buf[i]&0x0F]]...)
	}
	return out
}
