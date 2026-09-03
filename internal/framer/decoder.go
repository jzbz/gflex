package framer

import (
	"fmt"

	"github.com/jzbz/gflex/internal/proto"
)

// DropFunc is notified whenever the decoder discards buffered frame data. The
// vendor client drops these silently and the pending command then times out
// five seconds later with no diagnostic (SPEC.md §3.3); the hook exists so a
// Go implementation can say what actually happened.
//
// buffered is a copy of the accumulator at the moment of the drop and may be
// nil when nothing meaningful was buffered.
type DropFunc func(reason string, buffered []byte)

// Decoder reassembles protocol frames from an inbound MIDI byte stream.
//
// It is a status-byte-driven MIDI parser, not a fixed 3-byte stride. The vendor
// client can get away with a stride because Web MIDI hands it exactly one
// complete message per event; ALSA rawmidi delivers an unframed byte stream in
// which a single-byte realtime message, a two-byte program change, or running
// status would desynchronise a stride permanently (SPEC.md §3.3). All parser
// state persists across Feed calls, so a message split over several reads is
// reassembled correctly.
//
// A Decoder is not safe for concurrent use.
type Decoder struct {
	// status is the MIDI status byte currently in effect, or 0 when there is
	// none. It survives a completed channel message to implement running
	// status, and is cleared by any system common message.
	status byte
	data   [2]byte
	nData  int // data bytes collected for the message in progress
	want   int // data bytes the message in progress needs
	sysex  bool

	// acc is the protocol-frame accumulator, filled by Note On messages and
	// flushed by the end-of-frame marker.
	acc []byte
	// overflow counts the bytes discarded past the cap in the frame currently
	// being accumulated, so the drop hook hears about the overflow once per
	// frame instead of once per byte. See reportOverflow.
	overflow int
	onDrop   DropFunc
}

// NewDecoder returns a Decoder with an empty accumulator.
func NewDecoder() *Decoder {
	return &Decoder{acc: make([]byte, 0, proto.MaxFrameLen)}
}

// SetDropHook installs a callback invoked when buffered frame data is
// discarded. Pass nil to remove it. Call it before the decoder is handed to a
// Framer, or before any concurrent Feed; the hook is not guarded by a lock.
func (d *Decoder) SetDropHook(fn DropFunc) { d.onDrop = fn }

// Feed consumes a chunk of the inbound MIDI byte stream and returns every
// protocol frame completed by it, in order. The chunk may begin or end
// mid-message; leftover state is carried to the next call. The returned frames
// are freshly allocated copies and are not aliased by later calls.
func (d *Decoder) Feed(p []byte) [][]byte {
	var out [][]byte
	for _, b := range p {
		d.feedByte(b, &out)
	}
	return out
}

func (d *Decoder) feedByte(b byte, out *[][]byte) {
	switch {
	case b >= 0xF8:
		// System Realtime (clock, active sensing, reset). These are permitted
		// anywhere in the stream, including between the status and data bytes
		// of another message, and must not disturb the parse in progress.

	case b >= 0x80:
		// Any status byte resyncs: whatever partial message was in flight is
		// abandoned. It also terminates a SysEx run, which is how a truncated
		// SysEx recovers.
		d.sysex = false
		d.nData = 0
		d.data = [2]byte{}
		if b == statusSysExStart {
			// SysEx is an unbounded run ending at 0xF7. The VFLEX never sends
			// one (SPEC.md §3.1) but other traffic on a shared port might, and
			// swallowing the payload is what keeps us in sync if it does.
			d.sysex = true
			d.status = 0
			d.want = 0
			return
		}
		d.status = b
		d.want = expectedDataBytes(b)
		if d.want == 0 {
			// Tune Request, EOX, undefined system common: complete immediately.
			d.complete(out)
		}

	default:
		if d.sysex {
			return // SysEx payload byte, not ours
		}
		if d.status == 0 {
			return // data byte with no status in effect: nothing to attach it to
		}
		d.data[d.nData] = b
		d.nData++
		if d.nData >= d.want {
			d.complete(out)
		}
	}
}

// complete dispatches the finished MIDI message and prepares for the next one.
func (d *Decoder) complete(out *[][]byte) {
	st := d.status
	d.nData = 0
	if st >= 0x80 && st < 0xF0 {
		// Channel message: the status is retained so that bare data bytes
		// following it are parsed as a repeat (MIDI running status). Whether
		// the device relies on running status is unknown, but the kernel may
		// compress on the way out and unrelated traffic certainly can.
		if frame := d.message(st, d.data[0], d.data[1]); frame != nil {
			*out = append(*out, frame)
		}
		return
	}
	// System common cancels running status.
	d.status = 0
}

// message applies the frame state machine of SPEC.md §3.3 to one complete MIDI
// channel message and returns a finished protocol frame, or nil.
func (d *Decoder) message(status, d1, d2 byte) []byte {
	switch status & 0xF0 { // the channel nibble carries no information
	case StatusFrameStart:
		// Report the overflow of the frame being abandoned before the drop that
		// abandons it, so the notices arrive in the order the events happened.
		d.reportOverflow()
		// Start of frame. A marker arriving mid-frame discards the partial one;
		// that is the intended resync, not an error.
		if len(d.acc) > 0 {
			d.drop("start-of-frame marker arrived mid-frame", d.acc)
		}
		d.acc = d.acc[:0]

	case StatusFrameData:
		if len(d.acc) < proto.MaxFrameLen {
			d.acc = append(d.acc, (d1&0x0F)<<4|(d2&0x0F))
		} else {
			// The reference implementation caps the accumulator without
			// resetting it, so an over-long frame still dispatches a truncated
			// prefix rather than being dropped. Reproduced deliberately. Only
			// the count is kept here; reportOverflow tells the hook at the next
			// marker.
			d.overflow++
		}

	case StatusFrameEnd:
		// Ahead of the length check below, so the overflow notice precedes the
		// one that carries the accumulator rather than following it.
		d.reportOverflow()
		var frame []byte
		if len(d.acc) >= proto.PreambleLen {
			n := int(d.acc[0])
			// n <= len(d.acc) is the clause that does the work here: the
			// accumulator is capped at MaxFrameLen above, so it already
			// subsumes ValidResponseLen's own upper bound. That upper bound is
			// kept regardless -- it mirrors the receive check of SPEC.md §3.3
			// verbatim and is what still rejects an over-long frame if the cap
			// is ever raised. TestDecoderDeclaredLengthAboveMaxIsRejected pins
			// it, since no stream can reach it while the cap stands.
			if proto.ValidResponseLen(n) && n <= len(d.acc) {
				frame = make([]byte, n)
				copy(frame, d.acc)
			} else {
				d.drop(fmt.Sprintf("declared frame length %d is invalid for %d buffered bytes", n, len(d.acc)), d.acc)
			}
		} else if len(d.acc) > 0 {
			d.drop("end-of-frame marker with fewer than 2 buffered bytes", d.acc)
		}
		d.acc = d.acc[:0]
		return frame
	}
	return nil
}

// reportOverflow tells the hook, once, how much of the frame just ended was
// discarded past the 64-byte cap, and arms the counter for the next frame.
//
// One notice per frame rather than one per byte: the excess is len(frame)-64
// and unbounded, and the drop hook is the diagnostic channel of SPEC.md §3.3
// -- gflex monitor's queue is short by design, so a per-byte notice for the
// least informative event floods out the "declared frame length ..." notice
// that follows it and actually carries the accumulator. The counter is flushed
// by a marker, so a stream that sends none reports nothing; that is the same
// bargain the accumulator itself already makes, since its contents are only
// ever reported at a marker too.
func (d *Decoder) reportOverflow() {
	if d.overflow == 0 {
		return
	}
	n := d.overflow
	d.overflow = 0
	d.drop(fmt.Sprintf("frame exceeded %d bytes; %d excess byte(s) discarded", proto.MaxFrameLen, n), nil)
}

func (d *Decoder) drop(reason string, buffered []byte) {
	if d.onDrop == nil {
		return
	}
	var cp []byte
	if len(buffered) > 0 {
		cp = make([]byte, len(buffered))
		copy(cp, buffered)
	}
	d.onDrop(reason, cp)
}

// System messages the parser must size correctly even though the VFLEX never
// emits them.
const (
	statusSysExStart   byte = 0xF0
	statusTimeCode     byte = 0xF1 // 1 data byte
	statusSongPosition byte = 0xF2 // 2 data bytes
	statusSongSelect   byte = 0xF3 // 1 data byte
)

// expectedDataBytes reports how many data bytes follow a MIDI status byte.
// 0xF0 (SysEx) is unbounded and is handled separately by the caller.
func expectedDataBytes(status byte) int {
	if status < 0xF0 {
		switch status & 0xF0 {
		case 0xC0, 0xD0: // program change, channel pressure
			return 1
		default: // 0x80, 0x90, 0xA0, 0xB0, 0xE0
			return 2
		}
	}
	switch status {
	case statusTimeCode, statusSongSelect:
		return 1
	case statusSongPosition:
		return 2
	default: // 0xF4, 0xF5, 0xF6 tune request, 0xF7 EOX
		return 0
	}
}
