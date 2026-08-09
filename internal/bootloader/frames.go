// Package bootloader implements the VFLEX firmware-update path: the
// vendor-class (0xFF) bulk USB protocol the device speaks after
// CMD_JUMP_APP_TO_BOOTLOADER, plus the two ways a firmware image is obtained
// (a local file, or the vendor's WebSocket service).
//
// It is deliberately independent of the MIDI side of the product. The
// application-mode jump that puts the device into the bootloader, and the
// post-update parameter replay that restores settings a flash erased, both
// belong to the session layer; this package starts from an already-enumerated
// bootloader device and ends at CMD_BOOTLOAD_END. See SPEC.md §10.
//
// Update is the entry point: Connect, then Update, and the whole
// flash-verify-jump sequence — its ordering, its delays and the interlocks that
// decide whether the device is ever told to run what was written — lives inside
// it. Flasher's individual methods are exported for diagnostics and recovery,
// but a caller that re-assembles the sequence out of them owns a second copy
// that will drift from this one.
//
// The wire format here is the same two-byte preamble as the application
// protocol, but written as raw bulk bytes: there is no nibble encoding and
// there are no MIDI start/end markers (SPEC.md §10.2).
package bootloader

import (
	"errors"
	"fmt"

	"github.com/jzbz/gflex/internal/proto"
)

// ChunksPerPage is the number of WRITE_CHUNK frames one firmware page is split
// into. It is fixed at 8 by the protocol; the chunk *size* is not a constant
// and is derived from the page size of the image being flashed (SPEC.md §10.2).
const ChunksPerPage = 8

// writeChunkHeaderLen is the WRITE_CHUNK payload prefix: page id high, page id
// low, chunk id. The page id is the one big-endian u16 in the bootloader
// protocol, matching the application protocol's big-endian rule (SPEC.md §5.1).
const writeChunkHeaderLen = 3

// MaxPages is how many pages one image can address, imposed by that u16 page
// id. Page 65536 would wrap to page 0 and overwrite the start of the image the
// device has already committed, so an oversized image is rejected before the
// first write rather than allowed to corrupt itself halfway through. No real
// VFLEX image comes close — 65536 pages is 32 MB at the usual 512-byte page.
const MaxPages = 1 << 16

// maxBootloaderFrameLen is the ceiling imposed by the single-byte length field.
//
// This is deliberately *not* proto.MaxFrameLen (64). That 64-byte bound is a
// property of the MIDI receive state machine (SPEC.md §3.3) and does not apply
// to raw bulk traffic: a 512-byte page yields 64-byte chunks and therefore a
// 69-byte WRITE_CHUNK frame, which is perfectly legal on the bulk endpoint.
const maxBootloaderFrameLen = proto.MaxEncodableFrameLen

// MaxChunkSize is the largest chunk that still fits in one WRITE_CHUNK frame,
// and so bounds the usable page size at MaxChunkSize * ChunksPerPage.
const MaxChunkSize = maxBootloaderFrameLen - proto.PreambleLen - writeChunkHeaderLen

// ErrFrameTooLong reports a frame whose total length will not fit the one-byte
// length field.
var ErrFrameTooLong = errors.New("bootloader: frame exceeds the one-byte length field")

// buildFrame encodes [len, cmd|flags, payload...].
//
// proto.Build produces exactly this layout, bounded only by the single-byte
// length field, which is the same bound the bulk path has. The tighter 64-byte
// MIDI ceiling deliberately does not apply here: a 512-byte page yields 64-byte
// chunks and therefore a 69-byte WRITE_CHUNK frame, which is legal on the bulk
// endpoint. The error is re-wrapped as ErrFrameTooLong so callers sizing pages
// against MaxChunkSize get an error naming the bootloader's own limit.
func buildFrame(c proto.Cmd, payload []byte, write bool) ([]byte, error) {
	f, err := proto.Build(c, payload, write, false)
	if err == nil {
		return f, nil
	}
	if errors.Is(err, proto.ErrPayloadTooLong) {
		return nil, fmt.Errorf("%w: %s needs %d bytes, maximum %d",
			ErrFrameTooLong, c, proto.PreambleLen+len(payload), maxBootloaderFrameLen)
	}
	return nil, fmt.Errorf("bootloader: encoding %s: %w", c, err)
}

// cmdByte assembles the command byte: code in the low six bits, write flag on
// top. The scratchpad flag is never set in bootloader mode.
func cmdByte(c proto.Cmd, write bool) byte {
	b := byte(c) & proto.CmdCodeMask
	if write {
		b |= proto.FlagWrite
	}
	return b
}

// shortFrame builds a payload-free frame. A zero-length payload always fits, so
// the error path is unreachable; it falls back to the literal encoding rather
// than panicking, because this is library code.
func shortFrame(c proto.Cmd, write bool) []byte {
	f, err := buildFrame(c, nil, write)
	if err != nil {
		return []byte{proto.PreambleLen, cmdByte(c, write)}
	}
	return f
}

// WriteChunkFrame builds CMD_BOOTLOADER_WRITE_CHUNK:
//
//	[len, 0x80, pageHi, pageLo, chunkID, data...]
//
// The page id is a big-endian uint16 (SPEC.md §10.2).
func WriteChunkFrame(page uint16, chunk uint8, data []byte) ([]byte, error) {
	payload := make([]byte, writeChunkHeaderLen+len(data))
	payload[0] = byte(page >> 8)
	payload[1] = byte(page)
	payload[2] = chunk
	copy(payload[writeChunkHeaderLen:], data)
	f, err := buildFrame(proto.CmdBootloaderWriteChunk, payload, true)
	if err != nil {
		return nil, fmt.Errorf("bootloader: page %d chunk %d: %w", page, chunk, err)
	}
	return f, nil
}

// CommitPageFrame builds CMD_BOOTLOADER_COMMIT_PAGE: [0x02, 0x81].
func CommitPageFrame() []byte { return shortFrame(proto.CmdBootloaderCommitPage, true) }

// VerifyWriteFrame builds the write form of CMD_BOOTLOADER_VERIFY: [0x02, 0x82].
// It asks the device to compute the CRC over what was just flashed.
func VerifyWriteFrame() []byte { return shortFrame(proto.CmdBootloaderVerify, true) }

// VerifyReadFrame builds the read form of CMD_BOOTLOADER_VERIFY: [0x02, 0x02].
// It asks the device to report the CRC it computed.
func VerifyReadFrame() []byte { return shortFrame(proto.CmdBootloaderVerify, false) }

// BootloadEndFrame builds CMD_BOOTLOAD_END: [0x02, 0x03]. The device jumps to
// the application image and disconnects; there is no acknowledgement.
//
// Never send this after a CRC mismatch: leaving the unit in the bootloader is
// what keeps it re-flashable (SPEC.md §10.5, §13 interlock 6).
func BootloadEndFrame() []byte { return shortFrame(proto.CmdBootloadEnd, false) }

// SerialReadFrame builds the ordinary application-protocol serial read,
// [0x02, 0x08]. The bootloader answers this one app command, which is how the
// host confirms it is talking to the unit it means to flash (SPEC.md §10.1).
func SerialReadFrame() []byte { return proto.Read(proto.CmdSerialNumber) }

// ---------------------------------------------------------------------------
// Page splitting
// ---------------------------------------------------------------------------

// ErrBadPageLength reports a page that cannot be divided into ChunksPerPage
// equal chunks.
var ErrBadPageLength = errors.New("invalid firmware payload")

// SplitPage divides one firmware page into exactly ChunksPerPage equal chunks.
//
// The chunk size is a property of the image, not a protocol constant: whatever
// page size the firmware payload uses simply has to divide by 8 (SPEC.md §10.2).
// The returned slices alias page and must not be modified.
func SplitPage(page []byte) ([][]byte, error) {
	if len(page) == 0 {
		return nil, fmt.Errorf("%w: page is empty", ErrBadPageLength)
	}
	if len(page)%ChunksPerPage != 0 {
		return nil, fmt.Errorf("%w: data length %d not divisible by %d",
			ErrBadPageLength, len(page), ChunksPerPage)
	}
	size := len(page) / ChunksPerPage
	if size > MaxChunkSize {
		return nil, fmt.Errorf("%w: page of %d bytes yields %d-byte chunks, maximum %d",
			ErrBadPageLength, len(page), size, MaxChunkSize)
	}
	chunks := make([][]byte, ChunksPerPage)
	for i := range chunks {
		chunks[i] = page[i*size : (i+1)*size]
	}
	return chunks, nil
}

// ---------------------------------------------------------------------------
// Response parsing
// ---------------------------------------------------------------------------

// ErrShortResponse reports a bulk read too short to contain a frame preamble.
var ErrShortResponse = errors.New("bootloader: response shorter than the 2-byte preamble")

// Response is a decoded bootloader acknowledgement.
type Response struct {
	// Raw is the bytes as they came off the endpoint.
	Raw []byte
	// DeclaredLen is the length byte the device sent, before validation.
	DeclaredLen int
	// Length is the effective frame length after the vendor client's lenient
	// fallback (SPEC.md §5.2).
	Length int
	// Cmd is the command code with the flag bits masked off. The device may or
	// may not echo the write flag; it is never inspected (SPEC.md §14.13).
	Cmd proto.Cmd
	// Payload is Raw[2:Length].
	Payload []byte
	// CRC is the single CRC byte of a VERIFY response, valid only when HasCRC.
	CRC uint8
	// HasCRC reports whether a CRC byte was present.
	HasCRC bool
}

// ParseResponse decodes a bulk read from the bootloader.
//
// It reproduces the vendor client's lenient length handling: a declared length
// that is below the preamble size or longer than what actually arrived is
// discarded in favour of the real buffer length (SPEC.md §5.2). Bulk reads are
// commonly padded to the endpoint's packet size, so Raw is usually longer than
// Length and the declared value is the one that matters.
//
// The CRC of a VERIFY response is a *single* byte at offset 2 — not a
// multi-byte checksum. The algorithm behind it is unknown (SPEC.md §10.2,
// §14.12); the host only ever compares it against the value shipped with the
// image.
func ParseResponse(raw []byte) (Response, error) {
	if len(raw) < proto.PreambleLen {
		return Response{}, fmt.Errorf("%w: %d bytes", ErrShortResponse, len(raw))
	}
	declared := int(raw[0])
	length := declared
	if declared < proto.PreambleLen || declared > len(raw) {
		length = len(raw)
	}
	r := Response{
		Raw:         raw,
		DeclaredLen: declared,
		Length:      length,
		Cmd:         proto.Cmd(raw[1] & proto.CmdCodeMask),
		Payload:     raw[proto.PreambleLen:length],
	}
	// The CRC test is against the bytes actually received rather than the
	// effective length, so a device that under-declares its verify response
	// still yields a usable CRC.
	if r.Cmd == proto.CmdBootloaderVerify && len(raw) >= proto.PreambleLen+1 {
		r.CRC = raw[proto.PreambleLen]
		r.HasCRC = true
	}
	return r, nil
}
