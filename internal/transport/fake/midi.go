package fake

import "github.com/jzbz/gflex/internal/proto"

// This file is a second, independent implementation of the VFLEX MIDI framing
// described in SPEC.md §3.1 and §3.3. It deliberately does not import
// internal/framer: if the device side of a round-trip test shared code with the
// host side, a bug in that shared code would round-trip cleanly and the test
// would prove nothing.

// The three MIDI status bytes the protocol uses, all on channel 0. The channel
// nibble is masked off and ignored in both directions (SPEC.md §3.3).
const (
	statusSOF  = 0x80 // Note Off: start of frame, resets the accumulator
	statusData = 0x90 // Note On: one message per protocol byte, note=hi nibble, velocity=lo
	statusEOF  = 0xA0 // Poly Key Pressure: end of frame, dispatch
)

// encodeFrameMIDI renders a protocol frame as the MIDI byte stream that carries
// it: a start marker, one Note-On per protocol byte splitting the byte into its
// high nibble (note number) and low nibble (velocity), then an end marker.
//
// Splitting each byte in two is what makes the protocol 7-bit safe: both data
// bytes of every emitted message are 0x00-0x0F, so an 8-bit protocol byte such
// as the 0x80 write flag survives a channel that only permits 7-bit data.
// A byte whose low nibble is zero encodes as a Note-On with velocity 0, which
// MIDI convention treats as a Note-Off; nothing in the Linux path rewrites it,
// but see SPEC.md §3.2 for why that matters.
func encodeFrameMIDI(frame []byte) []byte {
	out := make([]byte, 0, 3*(len(frame)+2))
	out = append(out, statusSOF, 0x00, 0x00)
	for _, b := range frame {
		out = append(out, statusData, (b>>4)&0x0F, b&0x0F)
	}
	out = append(out, statusEOF, 0x00, 0x00)
	return out
}

// midiDataBytes reports how many data bytes follow status byte b.
// A negative result means "variable length": a System Exclusive payload, which
// this protocol never uses and which is swallowed until the next status byte.
func midiDataBytes(b byte) int {
	switch {
	case b < 0x80:
		return 0 // not a status byte
	case b < 0xF0:
		switch b & 0xF0 {
		case 0xC0, 0xD0: // program change, channel pressure
			return 1
		default: // note off/on, poly pressure, control change, pitch bend
			return 2
		}
	}
	switch b {
	case 0xF0: // system exclusive
		return -1
	case 0xF1, 0xF3: // MTC quarter frame, song select
		return 1
	case 0xF2: // song position pointer
		return 2
	default: // 0xF4-0xF7 and the realtime range: no data bytes
		return 0
	}
}

// frameDecoder turns an arbitrarily chunked MIDI byte stream into protocol
// frames, exactly as the device's receive path does.
//
// It is a status-byte-driven parser rather than a fixed 3-byte stride. A stride
// only works when each read holds exactly one complete message, which is true
// of Web MIDI and false of a byte stream: one realtime byte, a 2-byte message,
// or a read that splits a message would desynchronise it permanently
// (SPEC.md §3.3). The parser therefore keeps its state across Feed calls and
// supports running status.
//
// A frameDecoder is not safe for concurrent use; Device serialises access.
type frameDecoder struct {
	status byte    // current status byte, 0 when none is established
	want   int     // data bytes the current status expects, -1 while in sysex
	data   [2]byte // data bytes gathered for the current message
	n      int     // how many of them have arrived
	acc    []byte  // protocol bytes accumulated since the last start marker
}

// newFrameDecoder returns a decoder with an empty accumulator.
func newFrameDecoder() *frameDecoder {
	return &frameDecoder{acc: make([]byte, 0, proto.MaxFrameLen)}
}

// feed consumes a chunk of the MIDI stream and returns every complete protocol
// frame it completed. Frames are freshly allocated and owned by the caller.
func (d *frameDecoder) feed(p []byte) [][]byte {
	var out [][]byte
	for _, b := range p {
		switch {
		case b >= 0xF8:
			// System realtime (clock, active sensing, reset) may be
			// interleaved anywhere, even between the data bytes of another
			// message. Ignore without disturbing the parser state.
			continue

		case b >= 0x80:
			// Status byte: resync. Any partial message is abandoned.
			d.status = b
			d.n = 0
			d.want = midiDataBytes(b)
			if d.want == 0 {
				// Nothing follows (tune request, end-of-exclusive, the
				// undefined system-common codes). System common also cancels
				// running status, so leave no status established.
				d.status = 0
			}

		default:
			// Data byte.
			if d.status == 0 || d.want <= 0 {
				// Orphan data byte, or a System Exclusive payload we are
				// swallowing. Either way it is not ours.
				continue
			}
			d.data[d.n] = b
			d.n++
			if d.n < d.want {
				continue
			}
			d.n = 0 // running status: the next data byte starts a new message
			if frame, ok := d.message(); ok {
				out = append(out, frame)
			}
			if d.status >= 0xF0 {
				d.status = 0 // system common does not run on
			}
		}
	}
	return out
}

// message applies the device's frame state machine to one complete MIDI
// message and reports a frame when the message completed one.
func (d *frameDecoder) message() ([]byte, bool) {
	switch d.status & 0xF0 { // the channel nibble is ignored in both directions
	case statusSOF:
		d.acc = d.acc[:0]

	case statusData:
		// Reassemble the protocol byte from the two nibbles. The cap is a
		// silent discard, not an abort: an over-long frame still dispatches its
		// 64-byte prefix, because the accumulator is not reset here
		// (SPEC.md §3.3).
		if len(d.acc) < proto.MaxFrameLen {
			d.acc = append(d.acc, (d.data[0]&0x0F)<<4|(d.data[1]&0x0F))
		}

	case statusEOF:
		var frame []byte
		if len(d.acc) >= proto.PreambleLen {
			// The declared length governs. A length below the preamble size,
			// above the 64-byte cap, or larger than what actually arrived means
			// the frame is dropped with no diagnostic and the pending command
			// simply times out.
			n := int(d.acc[0])
			if n >= proto.PreambleLen && n <= len(d.acc) && n <= proto.MaxFrameLen {
				frame = append([]byte(nil), d.acc[:n]...)
			}
		}
		d.acc = d.acc[:0]
		if frame != nil {
			return frame, true
		}
	}
	return nil, false
}
