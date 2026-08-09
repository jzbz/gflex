// Package framer implements layer 2 of the VFLEX stack: the MIDI nibble codec
// and the receive state machine described in SPEC.md §3.
//
// A protocol frame (see package proto) is carried over USB-MIDI as N+2 MIDI
// channel messages: a Note Off marking start-of-frame, one Note On per protocol
// byte whose note number is the byte's high nibble and whose velocity is its low
// nibble, and a Poly Key Pressure marking end-of-frame. The nibble split is the
// protocol's 7-bit-safety mechanism -- every emitted data byte lands in
// 0x00-0x0F, so 8-bit values survive a channel that only permits 7-bit data.
//
// The channel nibble of the status byte is ignored in both directions.
package framer

// MIDI status bytes used by the frame markers. Only the upper nibble is
// significant; the vendor client always emits channel 0 and masks the channel
// off on receive (SPEC.md §3.1, §3.3).
const (
	// StatusFrameStart is Note Off: start of frame.
	StatusFrameStart byte = 0x80
	// StatusFrameData is Note On: one message per protocol byte.
	StatusFrameData byte = 0x90
	// StatusFrameEnd is Poly Key Pressure: end of frame.
	StatusFrameEnd byte = 0xA0
)

// MessageLen is the size of every MIDI message this protocol uses.
const MessageLen = 3

// EncodeMIDI renders a protocol frame as the concatenated MIDI byte stream that
// carries it: start marker, one Note On per frame byte, end marker.
//
// A frame of N bytes produces (N+2)*3 bytes. This is the form used for
// --dry-run output and for tests; transmission goes through MIDIMessages so the
// framer can pace the messages individually.
func EncodeMIDI(frame []byte) []byte {
	out := make([]byte, 0, (len(frame)+2)*MessageLen)
	out = append(out, StatusFrameStart, 0x00, 0x00)
	for _, b := range frame {
		// High nibble becomes the note number, low nibble the velocity.
		// A byte whose low nibble is zero therefore encodes as a Note On with
		// velocity 0 -- equivalent to Note Off by MIDI convention, and thus
		// indistinguishable from a start-of-frame marker to any middleware that
		// canonicalises it. ALSA does not (SPEC.md §3.2), which is why this
		// tool writes rawmidi directly instead of going through a MIDI library.
		out = append(out, StatusFrameData, (b>>4)&0x0F, b&0x0F)
	}
	out = append(out, StatusFrameEnd, 0x00, 0x00)
	return out
}

// MIDIMessages renders a protocol frame as individual 3-byte MIDI messages, in
// transmission order: one start marker, one message per frame byte, one end
// marker.
//
// The returned slices alias a single backing array but are capacity-clamped, so
// appending to one cannot corrupt the next.
func MIDIMessages(frame []byte) [][]byte {
	buf := EncodeMIDI(frame)
	msgs := make([][]byte, 0, len(frame)+2)
	for i := 0; i+MessageLen <= len(buf); i += MessageLen {
		msgs = append(msgs, buf[i:i+MessageLen:i+MessageLen])
	}
	return msgs
}
