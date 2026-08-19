package bootloader

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This file is a deliberately minimal RFC 6455 client. The firmware fetch does
// exactly one thing — send a serial number as a text frame, read one reply —
// so a full WebSocket library would be a large dependency for a single request.
// What is implemented is the part that is not optional: the handshake, the
// mandatory client-to-server masking, all three payload-length encodings,
// fragmentation, and the control frames that a compliant server may interleave
// at any time.

// wsGUID is the RFC 6455 §1.3 magic string appended to the client key before
// hashing to form Sec-WebSocket-Accept.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// MaxMessageBytes caps a single inbound message. Firmware images are on the
// order of tens of kilobytes; the ceiling exists so that a hostile or broken
// server cannot force an unbounded allocation with a 64-bit length field.
const MaxMessageBytes = 8 << 20

// WebSocket opcodes (RFC 6455 §5.2).
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// errWebsocket is the error class for every protocol violation seen on the socket.
var errWebsocket = errors.New("websocket")

// wsConn is a client-side WebSocket connection over an already-upgraded stream.
//
// It is split from the dialer so the frame codec can be exercised over plain
// buffers in tests, with no network involved.
type wsConn struct {
	conn   net.Conn // nil when driven from buffers in tests
	r      *bufio.Reader
	w      io.Writer
	rand   io.Reader // masking-key source; crypto/rand in production
	maxMsg int
}

// newWSConn wraps a byte stream that has already completed the HTTP upgrade.
func newWSConn(r io.Reader, w io.Writer, randSrc io.Reader, maxMsg int) *wsConn {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	if randSrc == nil {
		randSrc = rand.Reader
	}
	if maxMsg <= 0 {
		maxMsg = MaxMessageBytes
	}
	return &wsConn{r: br, w: w, rand: randSrc, maxMsg: maxMsg}
}

// Close closes the underlying connection, if there is one.
func (c *wsConn) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// writeFrame emits one unfragmented frame.
//
// Every client-to-server frame MUST be masked (RFC 6455 §5.3) — servers are
// required to fail the connection on an unmasked frame — so the mask is not
// optional even though the payload here is a serial number. The header and the
// masked payload go out in one Write so a frame is never split across TCP
// segments by us.
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	if opcode >= opClose && len(payload) > 125 {
		return fmt.Errorf("%w: control frame payload of %d bytes exceeds 125", errWebsocket, len(payload))
	}
	var key [4]byte
	if _, err := io.ReadFull(c.rand, key[:]); err != nil {
		return fmt.Errorf("%w: generating masking key: %w", errWebsocket, err)
	}

	// FIN is always set: we never fragment what we send.
	buf := make([]byte, 0, 14+len(payload))
	buf = append(buf, 0x80|opcode)
	switch n := len(payload); {
	case n < 126:
		buf = append(buf, 0x80|byte(n))
	case n <= 0xFFFF:
		buf = append(buf, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(buf[len(buf)-2:], uint16(n))
	default:
		buf = append(buf, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(buf[len(buf)-8:], uint64(n))
	}
	buf = append(buf, key[:]...)
	for i, b := range payload {
		buf = append(buf, b^key[i%4])
	}
	if _, err := c.w.Write(buf); err != nil {
		return fmt.Errorf("%w: write: %w", errWebsocket, err)
	}
	return nil
}

// readFrame reads one frame header and its payload.
func (c *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var h [2]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return false, 0, nil, wrapRead(err)
	}
	fin = h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		// We negotiate no extensions, so any reserved bit is a protocol error.
		return false, 0, nil, fmt.Errorf("%w: reserved bits set in 0x%02x", errWebsocket, h[0])
	}
	opcode = h[0] & 0x0F
	masked := h[1]&0x80 != 0
	length := uint64(h[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.r, ext[:]); err != nil {
			return false, 0, nil, wrapRead(err)
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.r, ext[:]); err != nil {
			return false, 0, nil, wrapRead(err)
		}
		length = binary.BigEndian.Uint64(ext[:])
		// RFC 6455 §5.2: the most significant bit of a 64-bit length MUST be 0.
		if length&(1<<63) != 0 {
			return false, 0, nil, fmt.Errorf("%w: 64-bit length has the high bit set", errWebsocket)
		}
	}
	if opcode >= opClose {
		// Control frames must be short and must not be fragmented.
		if !fin {
			return false, 0, nil, fmt.Errorf("%w: fragmented control frame (opcode 0x%x)", errWebsocket, opcode)
		}
		if length > 125 {
			return false, 0, nil, fmt.Errorf("%w: control frame of %d bytes exceeds 125", errWebsocket, length)
		}
	}
	if length > uint64(c.maxMsg) {
		return false, 0, nil, fmt.Errorf("%w: frame of %d bytes exceeds the %d-byte limit", errWebsocket, length, c.maxMsg)
	}

	var mask [4]byte
	if masked {
		// A conforming server never masks, but unmasking costs nothing and
		// keeps us working against a server that does.
		if _, err := io.ReadFull(c.r, mask[:]); err != nil {
			return false, 0, nil, wrapRead(err)
		}
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return false, 0, nil, wrapRead(err)
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, opcode, payload, nil
}

// readMessage reads one complete application message, reassembling
// continuation frames and answering any control frames that arrive in between.
// The returned opcode is opText or opBinary.
func (c *wsConn) readMessage() (byte, []byte, error) {
	var (
		msgOp   byte
		buf     []byte
		started bool
	)
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case opPing:
			// A server may ping at any point, including mid-message. Answer
			// with the same application data, then carry on reassembling.
			if err := c.writeFrame(opPong, payload); err != nil {
				return 0, nil, fmt.Errorf("%w: replying to ping: %w", errWebsocket, err)
			}
			continue
		case opPong:
			continue
		case opClose:
			return 0, nil, closeError(payload)
		case opText, opBinary:
			if started {
				return 0, nil, fmt.Errorf("%w: data frame while a fragmented message is in progress", errWebsocket)
			}
			started, msgOp, buf = true, op, payload
		case opContinuation:
			if !started {
				return 0, nil, fmt.Errorf("%w: continuation frame with no message in progress", errWebsocket)
			}
			buf = append(buf, payload...)
		default:
			return 0, nil, fmt.Errorf("%w: unknown opcode 0x%x", errWebsocket, op)
		}
		if len(buf) > c.maxMsg {
			return 0, nil, fmt.Errorf("%w: message exceeds the %d-byte limit", errWebsocket, c.maxMsg)
		}
		if fin {
			return msgOp, buf, nil
		}
	}
}

// maxCloseReasonLen is the RFC 6455 §5.5.1 ceiling on the reason text: a Close
// payload is a control frame (125 bytes) minus the two-byte status code. The
// frame reader already enforces the 125, so this is belt and braces for a
// caller that hands closeError a payload from somewhere else.
const maxCloseReasonLen = 123

// closeError renders a received Close frame as an error. A close before we have
// a message is always a failure for our single-request use.
func closeError(payload []byte) error {
	if len(payload) < 2 {
		return fmt.Errorf("%w: server closed the connection", errWebsocket)
	}
	code := binary.BigEndian.Uint16(payload[:2])
	// The reason is server-controlled text that goes straight into an error and
	// on to a terminal. RFC 6455 §5.5.1 bounds it at 123 bytes but says nothing
	// about it being printable, and stripping only invalid UTF-8 (what this used
	// to do) leaves ESC and every other C0 control byte intact — enough to
	// repaint the operator's screen from a Close frame. Same discipline as the
	// firmware version string, same helper.
	reason := printableASCII(string(payload[2:]), maxCloseReasonLen)
	if reason == "" {
		return fmt.Errorf("%w: server closed the connection (code %d)", errWebsocket, code)
	}
	return fmt.Errorf("%w: server closed the connection (code %d): %s", errWebsocket, code, reason)
}

// closePayload builds the body of a Close frame.
func closePayload(code uint16, reason string) []byte {
	b := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(b, code)
	return append(b, reason...)
}

// wrapRead turns an unexpected EOF into something that names the protocol.
func wrapRead(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: connection closed mid-frame: %w", errWebsocket, err)
	}
	return fmt.Errorf("%w: read: %w", errWebsocket, err)
}

// ---------------------------------------------------------------------------
// Handshake
// ---------------------------------------------------------------------------

// wsAcceptKey computes the Sec-WebSocket-Accept value a server must return for
// the given Sec-WebSocket-Key (RFC 6455 §4.2.2). SHA-1 here is a protocol
// constant, not a security choice.
func wsAcceptKey(key string) string {
	h := sha1.New()
	io.WriteString(h, key)
	io.WriteString(h, wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// maxUpgradeResponseBytes caps what the HTTP upgrade parse may read. A 101
// response is a status line and a handful of headers — a few hundred bytes —
// so 32 KiB is generous for anything legitimate, including a server that piles
// on cookies or proxy headers, while still being a rounding error next to an
// unbounded read.
const maxUpgradeResponseBytes = 32 << 10

// errUpgradeTooLarge is returned to the HTTP parser when the cap is hit. The
// parser wraps or replaces it freely, which is why headerLimitReader also
// records the fact in a field the caller can consult.
var errUpgradeTooLarge = errors.New("upgrade response too large")

// headerLimitReader bounds a stream until it is explicitly unbounded.
//
// io.LimitReader is not usable here: it reports EOF at the limit, which the HTTP
// parser reports as a truncated response rather than as a flood, and it offers
// no way to lift the limit afterwards — and lifting it is required, because the
// same bufio.Reader must go on to carry the frame stream with any pipelined
// first-frame bytes it has already buffered.
type headerLimitReader struct {
	r         io.Reader
	remaining int64
	unlimited bool
	exceeded  bool
}

func (h *headerLimitReader) Read(p []byte) (int, error) {
	if h.unlimited {
		return h.r.Read(p)
	}
	if h.remaining <= 0 {
		h.exceeded = true
		return 0, errUpgradeTooLarge
	}
	if int64(len(p)) > h.remaining {
		p = p[:h.remaining]
	}
	n, err := h.r.Read(p)
	h.remaining -= int64(n)
	return n, err
}

// unbound lifts the limit once the handshake has been accepted.
func (h *headerLimitReader) unbound() { h.unlimited = true }

// wsDial opens a WebSocket connection and completes the opening handshake.
func wsDial(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: bad URL %q: %w", errWebsocket, rawURL, err)
	}
	var secure bool
	switch strings.ToLower(u.Scheme) {
	case "wss", "https":
		secure = true
	case "ws", "http":
		secure = false
	default:
		return nil, fmt.Errorf("%w: unsupported URL scheme %q", errWebsocket, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("%w: URL %q has no host", errWebsocket, rawURL)
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if secure {
			port = "443"
		}
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("%w: dialing %s: %w", errWebsocket, rawURL, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if secure {
		tc := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("%w: TLS handshake with %s: %w", errWebsocket, host, err)
		}
		conn = tc
	}

	var keyBytes [16]byte
	if _, err := io.ReadFull(rand.Reader, keyBytes[:]); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: generating Sec-WebSocket-Key: %w", errWebsocket, err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes[:])

	req := u.RequestURI()
	if req == "" {
		req = "/"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "GET %s HTTP/1.1\r\n", req)
	fmt.Fprintf(&sb, "Host: %s\r\n", u.Host)
	sb.WriteString("Upgrade: websocket\r\n")
	sb.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&sb, "Sec-WebSocket-Key: %s\r\n", key)
	sb.WriteString("Sec-WebSocket-Version: 13\r\n\r\n")
	if _, err := io.WriteString(conn, sb.String()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: sending upgrade request: %w", errWebsocket, err)
	}

	// http.ReadResponse imposes no size limit of its own: net/textproto reads the
	// status line and headers with math.MaxInt64 as its bound, so without this
	// the only thing standing between us and a server that emits headers forever
	// is the 15 s fetch deadline — during which it can hand us as much memory as
	// the link will carry. Every other read in this client is bounded
	// (MaxMessageBytes, the 64-bit length check, the 125-byte control rule); this
	// was the exception.
	lr := &headerLimitReader{r: conn, remaining: maxUpgradeResponseBytes}
	br := bufio.NewReader(lr)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		if lr.exceeded {
			// Name the real cause: the parse error a truncated read produces
			// ("malformed HTTP response") describes the symptom, not the flood.
			return nil, fmt.Errorf("%w: upgrade response headers exceed the %d-byte limit",
				errWebsocket, maxUpgradeResponseBytes)
		}
		return nil, fmt.Errorf("%w: reading upgrade response: %w", errWebsocket, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("%w: server answered %s, expected 101 Switching Protocols", errWebsocket, resp.Status)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		!headerHasToken(resp.Header.Get("Connection"), "upgrade") {
		conn.Close()
		return nil, fmt.Errorf("%w: server did not confirm the upgrade", errWebsocket)
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), wsAcceptKey(key); got != want {
		conn.Close()
		return nil, fmt.Errorf("%w: Sec-WebSocket-Accept mismatch", errWebsocket)
	}

	// The handshake is over, so the header budget is spent and must not apply to
	// the frame stream — an image is far larger than it. Lifting the limit in
	// place, rather than building a fresh reader over conn, is deliberate: br may
	// already hold bytes past the header terminator, and those bytes are the
	// start of the first WebSocket frame. They exist only inside br, so br has to
	// be the reader the connection keeps using.
	lr.unbound()
	c := newWSConn(br, conn, rand.Reader, MaxMessageBytes)
	c.conn = conn
	return c, nil
}

// headerHasToken reports whether a comma-separated header value contains a
// token, case-insensitively.
func headerHasToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Firmware fetch
// ---------------------------------------------------------------------------

// DefaultWSURL is the vendor's production firmware service, derived from the
// same host as their REST API (SPEC.md §10.3). No server-side authentication
// was observed, but that is unverified.
const DefaultWSURL = "wss://vflex-nestjs-prod-ylaqjkd4na-uc.a.run.app/bootloader"

// DefaultFetchTimeout matches the vendor client's 15 s budget for the whole
// exchange (SPEC.md §10.3).
const DefaultFetchTimeout = 15 * time.Second

// Fetch downloads a firmware image from the vendor's WebSocket service.
//
// The protocol is trivial: connect, send the plain serial-number string as a
// single text frame, and read one JSON message back. The reply is decoded with
// the same normalisation LoadFile applies, so the page encoding the server
// happens to use does not matter.
//
// wsURL may be empty, in which case DefaultWSURL is used; timeout may be zero
// for DefaultFetchTimeout. Callers who want to work offline should prefer
// LoadFile — the local file is the primary input and this service is a
// convenience (SPEC.md §10.3).
func Fetch(ctx context.Context, wsURL, serial string, timeout time.Duration) (*Firmware, error) {
	if wsURL == "" {
		wsURL = DefaultWSURL
	}
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	if strings.TrimSpace(serial) == "" {
		return nil, fmt.Errorf("bootloader: fetch needs a serial number to identify the image")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c, err := wsDial(ctx, wsURL)
	if err != nil {
		return nil, fmt.Errorf("bootloader: fetching firmware: %w", err)
	}
	defer c.Close()
	// Closing the socket is the only way to unblock a read that is already in
	// progress, so cancellation is wired straight to Close.
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()

	if err := c.writeFrame(opText, []byte(serial)); err != nil {
		return nil, fmt.Errorf("bootloader: sending serial %q: %w", serial, err)
	}
	_, msg, err := c.readMessage()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("bootloader: fetching firmware for %q: %w", serial, ctxErr)
		}
		return nil, fmt.Errorf("bootloader: fetching firmware for %q: %w", serial, err)
	}
	fw, err := ParseImage(msg, LoadOptions{})
	if err != nil {
		return nil, fmt.Errorf("bootloader: firmware payload for %q: %w", serial, err)
	}
	// Best effort: tell the server we are done. A failure here is irrelevant,
	// we already have the image.
	_ = c.writeFrame(opClose, closePayload(1000, ""))
	return fw, nil
}
