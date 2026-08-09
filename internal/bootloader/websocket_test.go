package bootloader

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

// cycleReader is a deterministic stand-in for crypto/rand so that masking keys
// are predictable in tests.
type cycleReader struct {
	b []byte
	i int
}

func (c *cycleReader) Read(p []byte) (int, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	for i := range p {
		p[i] = c.b[c.i%len(c.b)]
		c.i++
	}
	return len(p), nil
}

// testConn wires a wsConn to a canned inbound stream and a captured outbound
// buffer. No network is involved anywhere in these tests.
func testConn(t *testing.T, inbound []byte, key []byte) (*wsConn, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	c := newWSConn(bytes.NewReader(inbound), out, &cycleReader{b: key}, MaxMessageBytes)
	return c, out
}

func mask(payload []byte, key []byte) []byte {
	out := make([]byte, len(payload))
	for i, b := range payload {
		out[i] = b ^ key[i%4]
	}
	return out
}

// Client-to-server frames MUST be masked (RFC 6455 §5.3), and the length must
// use the shortest of the three encodings that fits.
func TestWriteFrameMaskingAndLengthForms(t *testing.T) {
	t.Parallel()
	key := []byte{0x01, 0x02, 0x03, 0x04}

	t.Run("7-bit length", func(t *testing.T) {
		c, out := testConn(t, nil, key)
		if err := c.writeFrame(opText, []byte("AB")); err != nil {
			t.Fatalf("writeFrame: %v", err)
		}
		want := []byte{0x81, 0x82, 0x01, 0x02, 0x03, 0x04, 'A' ^ 0x01, 'B' ^ 0x02}
		if !bytes.Equal(out.Bytes(), want) {
			t.Errorf("got % x, want % x", out.Bytes(), want)
		}
	})

	t.Run("16-bit length", func(t *testing.T) {
		c, out := testConn(t, nil, key)
		payload := bytes.Repeat([]byte{0xAA}, 200)
		if err := c.writeFrame(opBinary, payload); err != nil {
			t.Fatalf("writeFrame: %v", err)
		}
		got := out.Bytes()
		if got[0] != 0x82 || got[1] != 0x80|126 {
			t.Fatalf("header = % x, want 82 fe", got[:2])
		}
		if n := binary.BigEndian.Uint16(got[2:4]); n != 200 {
			t.Errorf("declared length = %d, want 200", n)
		}
		if !bytes.Equal(got[4:8], key) {
			t.Errorf("masking key = % x, want % x", got[4:8], key)
		}
		if !bytes.Equal(got[8:], mask(payload, key)) {
			t.Error("payload is not correctly masked")
		}
	})

	t.Run("64-bit length", func(t *testing.T) {
		c, out := testConn(t, nil, key)
		payload := bytes.Repeat([]byte{0x5A}, 70000)
		if err := c.writeFrame(opBinary, payload); err != nil {
			t.Fatalf("writeFrame: %v", err)
		}
		got := out.Bytes()
		if got[0] != 0x82 || got[1] != 0x80|127 {
			t.Fatalf("header = % x, want 82 ff", got[:2])
		}
		if n := binary.BigEndian.Uint64(got[2:10]); n != 70000 {
			t.Errorf("declared length = %d, want 70000", n)
		}
		if !bytes.Equal(got[10:14], key) {
			t.Errorf("masking key = % x, want % x", got[10:14], key)
		}
		if !bytes.Equal(got[14:], mask(payload, key)) {
			t.Error("payload is not correctly masked")
		}
	})
}

func TestWriteFrameRejectsOversizeControlFrame(t *testing.T) {
	t.Parallel()
	c, _ := testConn(t, nil, []byte{1, 2, 3, 4})
	if err := c.writeFrame(opPing, make([]byte, 126)); err == nil {
		t.Fatal("expected an error for a control frame over 125 bytes")
	}
}

func TestReadMessageLengthForms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		build func() ([]byte, []byte) // frame bytes, expected payload
	}{
		{
			name: "7-bit",
			build: func() ([]byte, []byte) {
				p := []byte("hello")
				return append([]byte{0x81, byte(len(p))}, p...), p
			},
		},
		{
			name: "16-bit",
			build: func() ([]byte, []byte) {
				p := bytes.Repeat([]byte{0x11}, 300)
				h := []byte{0x82, 126, 0, 0}
				binary.BigEndian.PutUint16(h[2:], 300)
				return append(h, p...), p
			},
		},
		{
			name: "64-bit",
			build: func() ([]byte, []byte) {
				p := bytes.Repeat([]byte{0x22}, 70000)
				h := make([]byte, 10)
				h[0], h[1] = 0x82, 127
				binary.BigEndian.PutUint64(h[2:], uint64(len(p)))
				return append(h, p...), p
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frame, want := tc.build()
			c, _ := testConn(t, frame, []byte{1, 2, 3, 4})
			_, got, err := c.readMessage()
			if err != nil {
				t.Fatalf("readMessage: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("payload length %d, want %d", len(got), len(want))
			}
		})
	}
}

// A fragmented message with a ping interleaved: the pong must go out and the
// message must still reassemble in order.
func TestReadMessageContinuationWithPing(t *testing.T) {
	t.Parallel()
	key := []byte{0x01, 0x02, 0x03, 0x04}
	var in []byte
	in = append(in, 0x01, 0x02, 'h', 'e')           // text, FIN clear
	in = append(in, 0x89, 0x04, 'p', 'i', 'n', 'g') // ping, mid-message
	in = append(in, 0x00, 0x02, 'l', 'l')           // continuation, FIN clear
	in = append(in, 0x80, 0x01, 'o')                // continuation, FIN set
	in = append(in, 0x81, 0x02, 'n', 'o')           // never read

	c, out := testConn(t, in, key)
	op, msg, err := c.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if op != opText {
		t.Errorf("opcode = 0x%x, want 0x%x", op, opText)
	}
	if string(msg) != "hello" {
		t.Errorf("message = %q, want %q", msg, "hello")
	}
	wantPong := append([]byte{0x8A, 0x84, 0x01, 0x02, 0x03, 0x04}, mask([]byte("ping"), key)...)
	if !bytes.Equal(out.Bytes(), wantPong) {
		t.Errorf("pong = % x, want % x", out.Bytes(), wantPong)
	}
}

// A server is not supposed to mask, but unmasking anyway costs nothing and
// keeps us working against one that does.
func TestReadMessageAcceptsMaskedServerFrame(t *testing.T) {
	t.Parallel()
	key := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	payload := []byte("masked")
	in := append([]byte{0x81, 0x80 | byte(len(payload))}, key...)
	in = append(in, mask(payload, key)...)

	c, _ := testConn(t, in, []byte{1, 2, 3, 4})
	_, got, err := c.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("message = %q, want %q", got, payload)
	}
}

func TestReadMessageErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"reserved bit set", []byte{0xC1, 0x00}, "reserved bits"},
		{"fragmented control frame", []byte{0x09, 0x00}, "fragmented control frame"},
		{"oversize control frame", append([]byte{0x89, 126, 0, 200}, make([]byte, 200)...), "exceeds 125"},
		{"64-bit high bit set", []byte{0x82, 127, 0x80, 0, 0, 0, 0, 0, 0, 0}, "high bit"},
		{"continuation with no message", []byte{0x80, 0x01, 'x'}, "no message in progress"},
		{"data frame mid-message", []byte{0x01, 0x01, 'a', 0x81, 0x01, 'b'}, "fragmented message is in progress"},
		{"unknown opcode", []byte{0x83, 0x00}, "unknown opcode"},
		{"truncated payload", []byte{0x81, 0x05, 'h', 'i'}, "closed mid-frame"},
		{"close frame", []byte{0x88, 0x02, 0x03, 0xE8}, "code 1000"},
		{"close with reason", append([]byte{0x88, 0x06, 0x03, 0xF3}, []byte("nope")...), "nope"},
		{"empty close", []byte{0x88, 0x00}, "server closed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := testConn(t, tc.in, []byte{1, 2, 3, 4})
			if _, _, err := c.readMessage(); err == nil {
				t.Fatal("expected an error")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// A hostile server must not be able to make us allocate without bound, in
// either a single frame or across fragments.
func TestReadMessageEnforcesMaxSize(t *testing.T) {
	t.Parallel()
	t.Run("single frame", func(t *testing.T) {
		in := append([]byte{0x82, 20}, make([]byte, 20)...)
		c := newWSConn(bytes.NewReader(in), &bytes.Buffer{}, &cycleReader{b: []byte{1, 2, 3, 4}}, 10)
		if _, _, err := c.readMessage(); err == nil {
			t.Fatal("expected an error for a frame over the limit")
		}
	})
	t.Run("across fragments", func(t *testing.T) {
		var in []byte
		in = append(in, 0x02, 8)
		in = append(in, make([]byte, 8)...)
		in = append(in, 0x80, 8)
		in = append(in, make([]byte, 8)...)
		c := newWSConn(bytes.NewReader(in), &bytes.Buffer{}, &cycleReader{b: []byte{1, 2, 3, 4}}, 10)
		if _, _, err := c.readMessage(); err == nil {
			t.Fatal("expected an error for fragments summing over the limit")
		}
	})
}

// The example key/accept pair from RFC 6455 §1.3.
func TestWSAcceptKey(t *testing.T) {
	t.Parallel()
	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	const want = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got := wsAcceptKey(key); got != want {
		t.Errorf("wsAcceptKey(%q) = %q, want %q", key, got, want)
	}
}

func TestHeaderHasToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value string
		want  bool
	}{
		{"Upgrade", true},
		{"upgrade", true},
		{"keep-alive, Upgrade", true},
		{" Upgrade ", true},
		{"keep-alive", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := headerHasToken(tc.value, "upgrade"); got != tc.want {
			t.Errorf("headerHasToken(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestFetchRejectsEmptySerial(t *testing.T) {
	t.Parallel()
	if _, err := Fetch(t.Context(), "ws://127.0.0.1:1/bootloader", "  ", 0); err == nil {
		t.Fatal("expected an error for an empty serial")
	}
}
