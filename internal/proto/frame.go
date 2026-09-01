package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Frame errors. These two are the whole set the package can produce.
//
// There is deliberately no error for an invalid declared length: Parse must
// reproduce the vendor client's lenient fallback (SPEC.md §5.2) rather than
// reject, so that condition is reported through Frame.DeclaredValid instead of
// through a sentinel nothing would ever return.
var (
	ErrPayloadTooLong = errors.New("proto: payload exceeds maximum frame size")
	ErrShortFrame     = errors.New("proto: frame shorter than the 2-byte preamble")
)

// Frame is a decoded protocol frame.
type Frame struct {
	// Cmd is the command code with flag bits already masked off.
	Cmd Cmd
	// Write reports whether FlagWrite was set in the command byte.
	Write bool
	// Scratchpad reports whether FlagScratchpad was set. The vendor app never
	// sets this bit. On the one unit measured it is validate-and-discard: the
	// write is acknowledged and echoed back but never takes effect (SPEC.md
	// §14.4).
	Scratchpad bool
	// Payload is the frame body, excluding the two preamble bytes.
	Payload []byte
	// DeclaredLen is the length byte as received, before validation.
	DeclaredLen int
	// DeclaredValid reports that DeclaredLen itself bounded the frame -- it was
	// within [PreambleLen, len(raw)] and no lenient fallback was applied.
	//
	// The distinction is what separates a frame from noise, and it cannot be
	// recovered from Payload afterwards: CmdBootloaderWriteChunk is command
	// code 0, so under the fallback an all-zero buffer parses as a well-formed
	// WRITE_CHUNK frame whose payload length happens to match the buffer
	// exactly. The bootloader's sibling parser carries the same flag for the
	// same reason; see bootloader.Response.
	DeclaredValid bool
}

// Build encodes a request frame.
//
// The write flag marks the frame as carrying a value to be stored. The
// scratchpad flag is exposed for completeness but is never set by the vendor
// application, and on the one unit measured it makes a write validate and then
// discard -- acknowledged, echoed back, never committed (SPEC.md §14.4). It is
// therefore a trap outside a deliberate raw/experimental path, where a
// successful-looking response means the value was not stored.
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
// It reproduces the vendor client's lenient length handling, which SPEC.md §5.2
// mandates: if the declared length is absent, below the preamble size, or larger
// than the bytes actually received, the whole buffer is used instead and no
// error is returned.
//
// Callers that must not act on a malformed frame check Frame.DeclaredValid.
// Comparing len(raw) against the returned payload length does not work and is
// exactly backwards: under the fallback the payload runs to the end of the
// buffer, so the two agree precisely when the declared length was bogus, while a
// well-formed frame followed by padding is the case where they differ.
func Parse(raw []byte) (Frame, error) {
	if len(raw) < PreambleLen {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrShortFrame, len(raw))
	}
	declared := int(raw[0])
	valid := declared >= PreambleLen && declared <= len(raw)
	end := declared
	if !valid {
		end = len(raw)
	}
	b := raw[1]
	return Frame{
		Cmd:           Cmd(b & CmdCodeMask),
		Write:         b&FlagWrite != 0,
		Scratchpad:    b&FlagScratchpad != 0,
		Payload:       raw[PreambleLen:end],
		DeclaredLen:   declared,
		DeclaredValid: valid,
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
// LED colour (command 13). SPEC.md §6.2.
// ---------------------------------------------------------------------------

// LEDColor is a colour the LED can be driven to with CmdFlashLEDSeqAdvanced.
//
// The command was UNKNOWN for as long as the only source was the shipped vendor
// application, which never issues it. The vendor's own published library does:
// tundra-labs/lib.vflex.app documents the write and its eight colour values,
// and ships a CLI that sends exactly this frame. The rest of the payload was
// then measured on hardware (SPEC.md §6.2, §14.17); see LEDColorPayload.
//
// Values outside this table are refused rather than masked, and not
// consistently: 9 and 11 are acknowledged and discarded, while 32 turns the LED
// off. Nothing here sends one, and ParseLEDColor will not produce one.
type LEDColor uint8

// The eight colour values the vendor library defines.
const (
	LEDOff     LEDColor = 0
	LEDRed     LEDColor = 1
	LEDGreen   LEDColor = 2
	LEDBlue    LEDColor = 3
	LEDWhite   LEDColor = 4
	LEDYellow  LEDColor = 5
	LEDMagenta LEDColor = 6
	LEDCyan    LEDColor = 7
)

// ledColorNames is the whole colour vocabulary, in wire order so that a listing
// reads as the value table it is.
var ledColorNames = []string{
	LEDOff: "off", LEDRed: "red", LEDGreen: "green", LEDBlue: "blue",
	LEDWhite: "white", LEDYellow: "yellow", LEDMagenta: "magenta", LEDCyan: "cyan",
}

// LEDColorNames returns every colour name, in wire order, for help text and for
// the "valid colours are" line of a parse failure.
func LEDColorNames() []string { return append([]string(nil), ledColorNames...) }

// String returns the colour name, or a numeric form for a value outside the
// table -- which is reachable, since nothing says the device rejects one.
func (c LEDColor) String() string {
	if int(c) < len(ledColorNames) {
		return ledColorNames[c]
	}
	return fmt.Sprintf("color(%d)", uint8(c))
}

// ParseLEDColor resolves a colour name, case-insensitively.
func ParseLEDColor(s string) (LEDColor, bool) {
	for i, name := range ledColorNames {
		if strings.EqualFold(s, name) {
			return LEDColor(i), true
		}
	}
	return 0, false
}

// LEDColorPayload builds the payload of an LED colour write: [10, 1, c, 2, 0].
//
// The name FLASH_LED_SEQUENCE_ADVANCED is literal. Measured on hardware
// (SPEC.md §6.2, §14.17), the payload is a counted list of colour records:
//
//	[ inert, count, colour, 2, colour, 2, ..., 0 ]
//
// The leading byte does nothing -- 0 and 10 are indistinguishable. The second
// counts the records that follow. Each record is a colour and a marker byte
// that must read exactly 2; any other value suppresses the record after it, in
// every position tried. The list is terminated by 0.
//
// So the vendor's five bytes are the one-record case, which is why they read as
// a plain "set the colour", and that is what this builds. A list of two or more
// plays once as the frame lands -- the first colour holds and the rest flash
// past -- rather than looping, so there is nothing a caller would want from the
// longer form that this does not already give them. `gflex raw` reaches it.
//
// The effect latches in RAM until the next write and does not survive a power
// cycle, so this costs no flash wear.
func LEDColorPayload(c LEDColor) []byte {
	return []byte{10, 1, byte(c), 2, 0}
}

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
		if PrintableASCII(b) {
			sb.WriteByte(b)
		}
	}
	return strings.TrimSpace(sb.String())
}

// PrintableASCII reports whether b is printable ASCII: 0x20 to 0x7E, space
// through tilde.
//
// This one predicate is the whole of the terminal-safety rule this tool applies
// to text it did not write. What it keeps out is NUL padding, invalid UTF-8,
// and -- the byte that matters -- the ESC that introduces an ANSI control
// sequence. Device identity strings, rawmidi port names, firmware version
// strings and WebSocket close reasons are all printed to a terminal, and any
// one of them could otherwise repaint or clear it.
//
// It is exported so the filters that apply it can share it: DecodeString above,
// and printableASCII in internal/bootloader, which keeps its own loop because
// it takes a string and bounds the result where this one takes a bounded
// payload and does not. They differ in what they consume, not in what they
// consider printable, and a rule stated in two places is a rule that can drift
// in one.
func PrintableASCII(b byte) bool { return b >= 0x20 && b <= 0x7E }

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
