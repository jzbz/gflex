package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Frame errors.
var (
	ErrPayloadTooLong = errors.New("proto: payload exceeds maximum frame size")
	ErrShortFrame     = errors.New("proto: frame shorter than the 2-byte preamble")
	ErrBadLength      = errors.New("proto: declared frame length is invalid")
)

// Frame is a decoded protocol frame.
type Frame struct {
	// Cmd is the command code with flag bits already masked off.
	Cmd Cmd
	// Write reports whether FlagWrite was set in the command byte.
	Write bool
	// Scratchpad reports whether FlagScratchpad was set. The vendor app never
	// sets this bit; its meaning is undetermined.
	Scratchpad bool
	// Payload is the frame body, excluding the two preamble bytes.
	Payload []byte
}

// Build encodes a request frame.
//
// The write flag marks the frame as carrying a value to be stored. The
// scratchpad flag is exposed for completeness but is never set by the vendor
// application and its effect on the device is unknown; callers should not set
// it outside a deliberate raw/experimental path.
//
// Build enforces only the limit inherent to the frame format -- the single-byte
// length field. It deliberately does not enforce the tighter MaxPayloadLen: that
// ceiling belongs to the MIDI receive state machine, and the bootloader's bulk
// path legitimately sends larger frames. Callers on the MIDI path should check
// MaxPayloadLen themselves; see FitsMIDI.
func Build(c Cmd, payload []byte, write, scratchpad bool) ([]byte, error) {
	if len(payload) > MaxEncodablePayloadLen {
		return nil, fmt.Errorf("%w: %d bytes, maximum %d", ErrPayloadTooLong, len(payload), MaxEncodablePayloadLen)
	}
	b := uint8(c) & CmdCodeMask
	if scratchpad {
		b |= FlagScratchpad
	}
	if write {
		b |= FlagWrite
	}
	out := make([]byte, PreambleLen+len(payload))
	out[0] = uint8(len(out))
	out[1] = b
	copy(out[PreambleLen:], payload)
	return out, nil
}

// Read builds a zero-payload read request: [0x02, cmd].
func Read(c Cmd) []byte {
	f, err := Build(c, nil, false, false)
	if err != nil {
		panic(err) // unreachable: an empty payload always fits
	}
	return f
}

// Write builds a write request: [2+n, cmd|0x80, payload...].
func Write(c Cmd, payload []byte) ([]byte, error) {
	return Build(c, payload, true, false)
}

// Parse decodes a received frame.
//
// It reproduces the vendor client's lenient length handling: if the declared
// length is absent, below the preamble size, or larger than the bytes actually
// received, the whole buffer is used instead. Callers that need to know a frame
// was malformed should compare len(raw) against the returned frame size.
func Parse(raw []byte) (Frame, error) {
	if len(raw) < PreambleLen {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrShortFrame, len(raw))
	}
	declared := int(raw[0])
	end := declared
	if declared < PreambleLen || declared > len(raw) {
		end = len(raw)
	}
	b := raw[1]
	return Frame{
		Cmd:        Cmd(b & CmdCodeMask),
		Write:      b&FlagWrite != 0,
		Scratchpad: b&FlagScratchpad != 0,
		Payload:    raw[PreambleLen:end],
	}, nil
}

// ValidResponseLen reports whether n is a length the device's receive path
// would accept for a frame. Used by the framer to decide whether to dispatch.
func ValidResponseLen(n int) bool {
	return n >= PreambleLen && n <= MaxFrameLen
}

// FitsMIDI reports whether a built frame is small enough to survive the MIDI
// receive state machine at the far end. Frames longer than this are dropped
// without a diagnostic, so callers on the MIDI path should check before sending
// rather than waiting out a response timeout.
func FitsMIDI(frame []byte) bool { return len(frame) <= MaxFrameLen }

// ---------------------------------------------------------------------------
// Payload codecs. Every scalar in this protocol is big-endian.
// ---------------------------------------------------------------------------

// EncodeU16 encodes a big-endian uint16 payload (voltage, current, tolerance).
func EncodeU16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

// DecodeU16 decodes a big-endian uint16 from the start of a payload.
func DecodeU16(payload []byte) (uint16, error) {
	if len(payload) < 2 {
		return 0, fmt.Errorf("proto: need 2 payload bytes, got %d", len(payload))
	}
	return binary.BigEndian.Uint16(payload), nil
}

// EncodeI32 encodes a big-endian signed int32 payload (ADC offset and scale).
//
// These two fields are genuinely signed: the vendor client decodes them with
// JavaScript shift operators, which yield a signed 32-bit result, and encodes
// negative inputs as two's complement.
func EncodeI32(v int32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(v))
	return b
}

// DecodeI32 decodes a big-endian signed int32 from the start of a payload.
func DecodeI32(payload []byte) (int32, error) {
	if len(payload) < 4 {
		return 0, fmt.Errorf("proto: need 4 payload bytes, got %d", len(payload))
	}
	return int32(binary.BigEndian.Uint32(payload)), nil
}

// EncodeVLimit encodes the user voltage-limit payload.
//
// The wire order is HIGH first, then LOW, in both directions. The vendor's own
// API signature takes (low, high), which is the reverse; that reversal is kept
// out of this encoder deliberately.
func EncodeVLimit(lowMv, highMv uint16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], highMv)
	binary.BigEndian.PutUint16(b[2:4], lowMv)
	return b
}

// DecodeVLimit decodes the user voltage-limit payload: high at [0:2], low at [2:4].
func DecodeVLimit(payload []byte) (lowMv, highMv uint16, err error) {
	if len(payload) < 4 {
		return 0, 0, fmt.Errorf("proto: need 4 payload bytes for vlimit, got %d", len(payload))
	}
	highMv = binary.BigEndian.Uint16(payload[0:2])
	lowMv = binary.BigEndian.Uint16(payload[2:4])
	return lowMv, highMv, nil
}

// DecodeVMeasure decodes the measurement response: raw ADC counts followed by
// the device's own calibrated millivolt value.
func DecodeVMeasure(payload []byte) (rawADC, calibratedMv uint16, err error) {
	if len(payload) < 4 {
		return 0, 0, fmt.Errorf("proto: need 4 payload bytes for vmeasure, got %d", len(payload))
	}
	return binary.BigEndian.Uint16(payload[0:2]), binary.BigEndian.Uint16(payload[2:4]), nil
}

// ---------------------------------------------------------------------------
// LED setting. The wire value is the inverse of the user-facing toggle.
// ---------------------------------------------------------------------------

// EncodeLEDAlwaysOn converts the user-facing "LED Always On" setting to its
// wire byte. The command is named DISABLE_LED_DURING_OPERATION, so the sense is
// inverted: always-on is 0, suppressed-while-green is 1.
func EncodeLEDAlwaysOn(alwaysOn bool) byte {
	if alwaysOn {
		return 0
	}
	return 1
}

// DecodeLEDAlwaysOn converts a wire byte to the user-facing setting.
func DecodeLEDAlwaysOn(b byte) bool { return b == 0 }

// ---------------------------------------------------------------------------
// Identity strings.
// ---------------------------------------------------------------------------

// DecodeString extracts a device identity string from a payload.
//
// The firmware NUL-pads short strings. This drops NUL, the Unicode replacement
// character, and every byte outside printable ASCII, then trims surrounding
// space. It deliberately does not reproduce the vendor client's decoder, which
// UTF-8-decodes the whole frame including the preamble and then slices off two
// characters -- that misaligns on any byte >= 0x80.
func DecodeString(payload []byte) string {
	var sb strings.Builder
	sb.Grow(len(payload))
	for _, b := range payload {
		if b >= 0x20 && b <= 0x7E {
			sb.WriteByte(b)
		}
	}
	return strings.TrimSpace(sb.String())
}

// SerialUsable reports whether a decoded serial number is long enough to trust.
// The vendor client applies the same four-character minimum before using a
// serial to identify a unit.
func SerialUsable(s string) bool { return len(s) >= 4 }

// Hex renders a byte slice as lowercase space-separated hex, for logging and
// for --verbose output.
func Hex(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*3-1)
	for i, c := range b {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, digits[c>>4], digits[c&0x0F])
	}
	return string(out)
}
