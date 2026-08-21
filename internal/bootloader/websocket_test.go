package bootloader

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
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
		// The second fragment's header declares 8 more bytes, taking the message
		// to 16 against a limit of 10 — and its payload is deliberately absent
		// from the stream. A reader that allocates the fragment first and only
		// then checks the total would block on those eight bytes and report a
		// truncated read; refusing from the header is what keeps the peak at the
		// advertised cap instead of twice it.
		var in []byte
		in = append(in, 0x02, 8)
		in = append(in, make([]byte, 8)...)
		in = append(in, 0x80, 8)
		c := newWSConn(bytes.NewReader(in), &bytes.Buffer{}, &cycleReader{b: []byte{1, 2, 3, 4}}, 10)
		_, _, err := c.readMessage()
		if err == nil {
			t.Fatal("expected an error for fragments summing over the limit")
		}
		if !strings.Contains(err.Error(), "exceeds the 10-byte limit") {
			t.Errorf("error = %q, want the fragment refused from its header", err.Error())
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
	_, err := Fetch(t.Context(), insecureWSScheme+"://127.0.0.1:1/bootloader", "  ", 0)
	if err == nil {
		t.Fatal("expected an error for an empty serial")
	}
	// The serial is checked before anything is dialled, so this must be the
	// reason rather than the connection to port 1 failing.
	if !strings.Contains(err.Error(), "serial number") {
		t.Errorf("error = %q, want it to name the missing serial", err.Error())
	}
}

// The image and the CRC it is checked against arrive in the same document, so a
// cleartext fetch authenticates neither: whoever answers chooses both. A plain
// ws:// or http:// URL is refused outright, and the refusal has to name the one
// spelling that accepts cleartext on purpose — otherwise the operator's only
// route is to give up or to stop reading the error.
func TestFetchRefusesCleartextURL(t *testing.T) {
	t.Parallel()
	for _, u := range []string{"ws://127.0.0.1:1/bootloader", "http://127.0.0.1:1/bootloader"} {
		_, err := Fetch(t.Context(), u, "VF001234", time.Second)
		if err == nil {
			t.Fatalf("Fetch(%q) succeeded; a cleartext firmware fetch must be refused", u)
		}
		// A dial failure would also be an error, so the message is what
		// distinguishes a refusal from merely failing to reach port 1.
		if !strings.Contains(err.Error(), insecureWSScheme) {
			t.Errorf("Fetch(%q) error = %q, want it to name the %s:// downgrade", u, err.Error(), insecureWSScheme)
		}
	}
}

// A Close frame's reason is server-controlled text that goes straight into an
// error and on to a terminal. Stripping only invalid UTF-8 -- what this used to
// do -- leaves ESC and every other C0 byte intact, which is enough to repaint
// the operator's screen from a Close frame.
func TestCloseErrorSanitisesReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		reason  string
		want    string
		absent  string
		present string
	}{
		{name: "ansi sequence", reason: "\x1b[2Jgone", want: "[2Jgone"},
		{name: "control bytes", reason: "no\x00pe\x07", want: "nope"},
		{name: "invalid utf-8", reason: "bad\xff\xfe", want: "bad"},
		{name: "plain text survives", reason: "server busy", want: "server busy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload := append([]byte{0x03, 0xF3}, tc.reason...)
			err := closeError(payload)
			if err == nil {
				t.Fatal("closeError returned nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error = %q, want it to contain %q", msg, tc.want)
			}
			for i := 0; i < len(msg); i++ {
				if b := msg[i]; b < 0x20 || b > 0x7E {
					t.Fatalf("error contains byte 0x%02x at offset %d: %q", b, i, msg)
				}
			}
		})
	}

	// The reason is bounded independently of the frame reader's 125-byte
	// control-frame rule, for a payload that reaches closeError from elsewhere.
	long := append([]byte{0x03, 0xF3}, strings.Repeat("x", 400)...)
	if n := len(closeError(long).Error()); n > 200 {
		t.Errorf("error is %d bytes for a 400-byte reason; the reason is not bounded", n)
	}
}

// wsUpgradeServer runs a one-shot listener that completes the opening handshake
// and then hands the accepted connection to serve. It returns the URL to dial.
//
// The listener speaks cleartext on loopback, so the URL carries the explicit
// downgrade scheme: wsDial refuses a plain ws:// endpoint outright.
func wsUpgradeServer(t *testing.T, serve func(conn net.Conn, req *http.Request, accept string)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		ln.Close()
		<-done
	})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		serve(conn, req, wsAcceptKey(req.Header.Get("Sec-WebSocket-Key")))
	}()
	return insecureWSScheme + "://" + ln.Addr().String() + "/bootloader"
}

// http.ReadResponse imposes no limit of its own on the status line or the
// headers -- net/textproto passes math.MaxInt64 as its bound -- so before the
// cap the only thing between us and a server flooding headers at line rate was
// the 15 s fetch deadline. The flood here stops after 4 MiB precisely so that a
// regression fails on the assertion instead of exhausting the machine.
func TestWSDialBoundsUpgradeResponseHeaders(t *testing.T) {
	t.Parallel()
	url := wsUpgradeServer(t, func(conn net.Conn, _ *http.Request, _ string) {
		if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"); err != nil {
			return
		}
		line := "X-Flood: " + strings.Repeat("a", 1024) + "\r\n"
		for written := 0; written < 4<<20; written += len(line) {
			if _, err := io.WriteString(conn, line); err != nil {
				return
			}
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	c, err := wsDial(ctx, url)
	elapsed := time.Since(start)
	if err == nil {
		c.Close()
		t.Fatal("an endless header stream completed the handshake")
	}
	if !strings.Contains(err.Error(), "exceed") {
		t.Errorf("error = %q, want it to name the header limit", err.Error())
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %s: the read is not bounded by anything but the deadline", elapsed)
	}
}

// The header limit must not survive the handshake. A server is free to pipeline
// the first frame into the same packet as the 101 response, so those bytes are
// already inside the bufio.Reader; the reader therefore has to be kept, with its
// limit lifted rather than replaced. The message here is deliberately larger
// than maxUpgradeResponseBytes: a limit left in place would fail this.
func TestWSDialKeepsPipelinedFrameAndLiftsHeaderLimit(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte{0x5A}, 60000)
	if len(payload) <= maxUpgradeResponseBytes {
		t.Fatalf("payload of %d bytes does not exceed the %d-byte header limit", len(payload), maxUpgradeResponseBytes)
	}
	url := wsUpgradeServer(t, func(conn net.Conn, _ *http.Request, accept string) {
		var out bytes.Buffer
		out.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
		header := []byte{0x82, 126, 0, 0}
		binary.BigEndian.PutUint16(header[2:], uint16(len(payload)))
		out.Write(header)
		// The headers, the frame header and the first slice of the payload
		// leave in one write, so bufio reads past the header terminator.
		out.Write(payload[:100])
		if _, err := conn.Write(out.Bytes()); err != nil {
			return
		}
		if _, err := conn.Write(payload[100:]); err != nil {
			return
		}
		// Stay open until the client is done, then drain its close frame.
		io.Copy(io.Discard, conn)
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := wsDial(ctx, url)
	if err != nil {
		t.Fatalf("wsDial: %v", err)
	}
	defer c.Close()
	op, msg, err := c.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if op != opBinary {
		t.Errorf("opcode = 0x%x, want 0x%x", op, opBinary)
	}
	if !bytes.Equal(msg, payload) {
		t.Errorf("message is %d bytes, want %d (and equal)", len(msg), len(payload))
	}
}

// A response that is not 101, and a wrong accept key, must still be refused with
// the limit in place -- the bound is a read cap, not a replacement for the
// handshake checks.
func TestWSDialStillChecksTheHandshake(t *testing.T) {
	t.Parallel()
	t.Run("wrong accept key", func(t *testing.T) {
		t.Parallel()
		url := wsUpgradeServer(t, func(conn net.Conn, _ *http.Request, _ string) {
			io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
				"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
				"Sec-WebSocket-Accept: not-the-right-key\r\n\r\n")
			io.Copy(io.Discard, conn)
		})
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if c, err := wsDial(ctx, url); err == nil {
			c.Close()
			t.Fatal("a wrong Sec-WebSocket-Accept was accepted")
		} else if !strings.Contains(err.Error(), "Accept mismatch") {
			t.Errorf("error = %q, want an accept-key mismatch", err.Error())
		}
	})
	t.Run("not 101", func(t *testing.T) {
		t.Parallel()
		url := wsUpgradeServer(t, func(conn net.Conn, _ *http.Request, _ string) {
			io.WriteString(conn, "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n")
			io.Copy(io.Discard, conn)
		})
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if c, err := wsDial(ctx, url); err == nil {
			c.Close()
			t.Fatal("a 404 completed the handshake")
		} else if !strings.Contains(err.Error(), "expected 101") {
			t.Errorf("error = %q, want it to name the expected status", err.Error())
		}
	})
}
