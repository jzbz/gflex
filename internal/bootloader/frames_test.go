package bootloader

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jzbz/gflex/internal/proto"
)

// The golden layouts of SPEC.md §10.2. These are raw bulk bytes: no nibble
// encoding, no MIDI markers.
func TestFrameBuilders(t *testing.T) {
	t.Parallel()
	chunk, err := WriteChunkFrame(0x0102, 0x03, []byte{0xAA, 0xBB})
	if err != nil {
		t.Fatalf("WriteChunkFrame: %v", err)
	}
	tests := []struct {
		name string
		got  []byte
		want []byte
	}{
		// [len, 0x80, pageHi, pageLo, chunkId, data...] with a big-endian page id.
		{"write chunk", chunk, []byte{0x07, 0x80, 0x01, 0x02, 0x03, 0xAA, 0xBB}},
		{"commit page", CommitPageFrame(), []byte{0x02, 0x81}},
		{"verify write", VerifyWriteFrame(), []byte{0x02, 0x82}},
		{"verify read", VerifyReadFrame(), []byte{0x02, 0x02}},
		{"bootload end", BootloadEndFrame(), []byte{0x02, 0x03}},
		{"serial read", SerialReadFrame(), []byte{0x02, 0x08}},
	}
	for _, tc := range tests {
		if !bytes.Equal(tc.got, tc.want) {
			t.Errorf("%s = %s, want %s", tc.name, proto.Hex(tc.got), proto.Hex(tc.want))
		}
	}
}

// A 512-byte page yields 64-byte chunks and so a 69-byte frame, which is past
// the application protocol's 64-byte receive ceiling but well inside what the
// single length byte can express. This must not be rejected.
func TestWriteChunkFrameOversize(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte{0x5A}, 64)
	f, err := WriteChunkFrame(1, 7, data)
	if err != nil {
		t.Fatalf("WriteChunkFrame: %v", err)
	}
	if len(f) != 69 {
		t.Fatalf("frame length = %d, want 69", len(f))
	}
	if f[0] != 69 {
		t.Errorf("length byte = %d, want 69", f[0])
	}
	if f[1] != 0x80 {
		t.Errorf("command byte = 0x%02x, want 0x80", f[1])
	}
	if f[2] != 0 || f[3] != 1 || f[4] != 7 {
		t.Errorf("header = % x, want 00 01 07", f[2:5])
	}
	if !bytes.Equal(f[5:], data) {
		t.Errorf("payload mismatch")
	}
}

func TestWriteChunkFrameTooLong(t *testing.T) {
	t.Parallel()
	if _, err := WriteChunkFrame(0, 0, make([]byte, MaxChunkSize+1)); err == nil {
		t.Fatal("expected an error for a chunk that overflows the length byte")
	}
	if _, err := WriteChunkFrame(0, 0, make([]byte, MaxChunkSize)); err != nil {
		t.Fatalf("a %d-byte chunk should fit: %v", MaxChunkSize, err)
	}
}

func TestSplitPage(t *testing.T) {
	t.Parallel()
	page := make([]byte, 64)
	for i := range page {
		page[i] = byte(i)
	}
	chunks, err := SplitPage(page)
	if err != nil {
		t.Fatalf("SplitPage: %v", err)
	}
	if len(chunks) != ChunksPerPage {
		t.Fatalf("got %d chunks, want %d", len(chunks), ChunksPerPage)
	}
	for i, c := range chunks {
		if len(c) != 8 {
			t.Fatalf("chunk %d is %d bytes, want 8", i, len(c))
		}
		if !bytes.Equal(c, page[i*8:(i+1)*8]) {
			t.Errorf("chunk %d = % x", i, c)
		}
	}
}

func TestSplitPageNotDivisibleByEight(t *testing.T) {
	t.Parallel()
	_, err := SplitPage(make([]byte, 12))
	if err == nil {
		t.Fatal("expected an error for a page length not divisible by 8")
	}
	const want = "invalid firmware payload: data length 12 not divisible by 8"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestSplitPageEmpty(t *testing.T) {
	t.Parallel()
	if _, err := SplitPage(nil); err == nil {
		t.Fatal("expected an error for an empty page")
	}
}

func TestParseResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		raw         []byte
		wantErr     bool
		wantCmd     proto.Cmd
		wantLen     int
		wantPayload []byte
		wantCRC     uint8
		wantHasCRC  bool
	}{
		{
			name:    "too short",
			raw:     []byte{0x02},
			wantErr: true,
		},
		{
			name:        "verify ack",
			raw:         []byte{0x03, 0x02, 0x5A},
			wantCmd:     proto.CmdBootloaderVerify,
			wantLen:     3,
			wantPayload: []byte{0x5A},
			wantCRC:     0x5A,
			wantHasCRC:  true,
		},
		{
			// Bulk reads come back padded to the endpoint packet size; the
			// declared length is what bounds the frame.
			name:        "verify ack padded to 8 bytes",
			raw:         []byte{0x03, 0x02, 0x5A, 0, 0, 0, 0, 0},
			wantCmd:     proto.CmdBootloaderVerify,
			wantLen:     3,
			wantPayload: []byte{0x5A},
			wantCRC:     0x5A,
			wantHasCRC:  true,
		},
		{
			// declaredLen below the preamble falls back to the real length.
			name:        "declared length zero",
			raw:         []byte{0x00, 0x02, 0x5A},
			wantCmd:     proto.CmdBootloaderVerify,
			wantLen:     3,
			wantPayload: []byte{0x5A},
			wantCRC:     0x5A,
			wantHasCRC:  true,
		},
		{
			// declaredLen longer than what arrived falls back too.
			name:        "declared length overruns the buffer",
			raw:         []byte{0x40, 0x02, 0x5A},
			wantCmd:     proto.CmdBootloaderVerify,
			wantLen:     3,
			wantPayload: []byte{0x5A},
			wantCRC:     0x5A,
			wantHasCRC:  true,
		},
		{
			// A device that under-declares still yields a usable CRC, because
			// the CRC test is against the bytes actually received.
			name:        "under-declared verify still gives a CRC",
			raw:         []byte{0x02, 0x02, 0x5A},
			wantCmd:     proto.CmdBootloaderVerify,
			wantLen:     2,
			wantPayload: []byte{},
			wantCRC:     0x5A,
			wantHasCRC:  true,
		},
		{
			// The write flag is masked off and never inspected.
			name:        "write flag echoed",
			raw:         []byte{0x03, 0x82, 0x77},
			wantCmd:     proto.CmdBootloaderVerify,
			wantLen:     3,
			wantPayload: []byte{0x77},
			wantCRC:     0x77,
			wantHasCRC:  true,
		},
		{
			name:        "commit ack has no CRC",
			raw:         []byte{0x02, 0x81},
			wantCmd:     proto.CmdBootloaderCommitPage,
			wantLen:     2,
			wantPayload: []byte{},
		},
		{
			name:        "write chunk ack",
			raw:         []byte{0x02, 0x80},
			wantCmd:     proto.CmdBootloaderWriteChunk,
			wantLen:     2,
			wantPayload: []byte{},
		},
		{
			// A three-byte non-verify frame must not be mistaken for a CRC.
			name:        "serial ack is not a CRC",
			raw:         []byte{0x04, 0x08, 'A', 'B'},
			wantCmd:     proto.CmdSerialNumber,
			wantLen:     4,
			wantPayload: []byte{'A', 'B'},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseResponse(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseResponse: %v", err)
			}
			if got.Cmd != tc.wantCmd {
				t.Errorf("Cmd = %v, want %v", got.Cmd, tc.wantCmd)
			}
			if got.Length != tc.wantLen {
				t.Errorf("Length = %d, want %d", got.Length, tc.wantLen)
			}
			if !bytes.Equal(got.Payload, tc.wantPayload) {
				t.Errorf("Payload = % x, want % x", got.Payload, tc.wantPayload)
			}
			if got.HasCRC != tc.wantHasCRC {
				t.Errorf("HasCRC = %v, want %v", got.HasCRC, tc.wantHasCRC)
			}
			if got.HasCRC && got.CRC != tc.wantCRC {
				t.Errorf("CRC = 0x%02x, want 0x%02x", got.CRC, tc.wantCRC)
			}
		})
	}
}

func TestParseResponseErrorNamesTheProblem(t *testing.T) {
	t.Parallel()
	_, err := ParseResponse([]byte{0x01})
	if err == nil || !strings.Contains(err.Error(), "preamble") {
		t.Fatalf("error = %v, want it to mention the preamble", err)
	}
}
