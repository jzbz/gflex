// Package usbmidi implements the VFLEX fallback transport: USB-MIDI spoken
// directly to the device's endpoints through usbfs, bypassing ALSA entirely.
//
// This is the escape route for the two cases the default ALSA rawmidi transport
// cannot serve (SPEC.md §4.1, §4.2):
//
//   - rawmidi is opened exclusively per direction, so a Chrome tab using Web
//     MIDI (the vendor ships one), PipeWire, JACK or a DAW holding the
//     substream makes it fail with EBUSY;
//   - a caller wants exclusive, deterministically paced access to the device.
//
// The cost is real: claiming the interface detaches snd-usb-audio, and while it
// is detached the ALSA card and its /dev/snd/midiC*D* node disappear from the
// rest of the system. Close therefore always releases the interface so the
// kernel driver rebinds; see (*transport).Close.
//
// Unlike the gousb-based design sketched in SPEC.md §12 this package needs no
// cgo: it is built on internal/usbfs, which drives the USBDEVFS ioctls directly.
package usbmidi

import "fmt"

// cableNumber is the USB-MIDI virtual cable used for every packet. The VFLEX
// exposes a single MIDI jack, so cable 0 is the only one that exists.
const cableNumber = 0

// cinLength maps a USB-MIDI Code Index Number to the number of valid MIDI bytes
// carried in the packet's three data slots. Entries 0 and 1 are reserved and
// carry no MIDI data. Verbatim from the USB Device Class Definition for MIDI
// Devices 1.0, table 4-1, and quoted in SPEC.md §4.2.
var cinLength = [16]int{0, 0, 2, 3, 3, 1, 2, 3, 3, 3, 3, 3, 2, 2, 3, 1}

// PackPacket wraps one complete MIDI message in a 4-byte USB-MIDI Event Packet.
//
// The packet is byte0 = (cableNumber << 4) | codeIndexNumber followed by the
// MIDI bytes, zero-padded to three. For channel-voice messages the Code Index
// Number is defined to equal the status byte's high nibble, which is why all
// VFLEX traffic (SPEC.md §4.2, §15) packs so regularly:
//
//	0x80 0x00 0x00  ->  08 80 00 00    start of frame
//	0x90 hi   lo    ->  09 90 hi   lo  one protocol byte, nibble-encoded
//	0xA0 0x00 0x00  ->  0A A0 00 00    end of frame
//
// PackPacket returns nil for anything it cannot classify unambiguously rather
// than guessing: a message with no status byte (running status), a message
// whose length disagrees with its status byte, and SysEx (0xF0 / 0xF7), which
// spans multiple packets with a CIN that depends on stream position. The VFLEX
// protocol contains no SysEx at all (SPEC.md §3.1), so refusing it costs
// nothing and keeps a caller from silently emitting a malformed packet. The
// undefined system-common statuses 0xF4 and 0xF5 are refused for the same
// reason.
func PackPacket(msg []byte) []byte {
	if len(msg) == 0 || len(msg) > 3 {
		return nil
	}
	cin, want, ok := classify(msg[0])
	if !ok || len(msg) != want {
		return nil
	}
	p := make([]byte, 4)
	p[0] = cableNumber<<4 | cin
	copy(p[1:], msg)
	return p
}

// classify maps a MIDI status byte to its USB-MIDI Code Index Number and the
// total message length in bytes (status byte included).
func classify(status byte) (cin byte, length int, ok bool) {
	switch {
	case status < 0x80:
		// A data byte: running status, which carries no status to encode.
		return 0, 0, false
	case status < 0xF0:
		// Channel voice. CIN == status high nibble by definition; Program
		// Change (0xC0) and Channel Pressure (0xD0) carry one data byte, all
		// others carry two.
		cin = status >> 4
		if cin == 0xC || cin == 0xD {
			return cin, 2, true
		}
		return cin, 3, true
	case status >= 0xF8:
		// System realtime: single byte, CIN 0xF ("Single Byte", table 4-1).
		// Not 0x5, which is "single-byte System Common" and belongs to 0xF6
		// alone; the two are one byte long either way, so a receiver that
		// dispatches on the length rather than the code cannot tell, and one
		// that dispatches on the code would read a clock as system common.
		return 0xF, 1, true
	}
	switch status {
	case 0xF1, 0xF3: // MTC quarter frame, Song Select: 2-byte system common
		return 0x2, 2, true
	case 0xF2: // Song Position Pointer: 3-byte system common
		return 0x3, 3, true
	case 0xF6: // Tune Request: 1-byte system common
		return 0x5, 1, true
	}
	// 0xF0 SysEx start, 0xF7 EOX, 0xF4 and 0xF5 undefined.
	return 0, 0, false
}

// UnpackPackets flattens a stream of 4-byte USB-MIDI Event Packets back into a
// plain MIDI byte stream.
//
// It walks a fixed 4-byte stride, skips packets whose first byte is zero (the
// device pads short transfers with them), and takes cinLength[byte0&0x0F] bytes
// from offset 1. A trailing partial packet — possible when a transfer is cut
// short — is discarded rather than treated as an error, and those bytes are
// gone: a USB transfer that ends mid-packet has no continuation, and ReadMIDI
// keeps no raw remainder to prepend to the next transfer. A conforming USB-MIDI
// endpoint never produces one, and the shipped unit's are 64-byte bulk
// endpoints (SPEC.md §14 Q3).
//
// The cable number in the high nibble is deliberately not filtered: the VFLEX
// has one cable, and dropping traffic on the basis of an unverified descriptor
// field would be a silent failure mode.
func UnpackPackets(buf []byte) []byte {
	var out []byte
	for off := 0; off+4 <= len(buf); off += 4 {
		b0 := buf[off]
		if b0 == 0 {
			continue // padding
		}
		n := cinLength[b0&0x0F]
		if n == 0 {
			continue // reserved CIN 0x0/0x1: no MIDI payload
		}
		out = append(out, buf[off+1:off+1+n]...)
	}
	return out
}

// splitMessages divides a MIDI byte stream into complete messages so each can
// be packed into its own event packet.
//
// proto.Transport documents WriteMIDI as carrying whole messages, so this is
// mostly a validator, but it is written as a real parser because the framer is
// free to hand over a whole frame's worth of messages in one call. It handles
// running status (a data byte following a completed message reuses the previous
// status) and system-realtime bytes, which may legally interleave anywhere.
//
// A trailing incomplete message is an error rather than a silent truncation:
// dropping it would corrupt the frame the caller is transmitting.
func splitMessages(p []byte) ([][]byte, error) {
	var (
		msgs   [][]byte
		acc    []byte
		status byte
		want   int
	)
	for i, b := range p {
		switch {
		case b >= 0xF8:
			// System realtime may appear between the bytes of another message
			// without disturbing it, so emit it standalone and keep acc intact.
			msgs = append(msgs, []byte{b})
		case b >= 0x80:
			if acc != nil {
				return nil, fmt.Errorf("usbmidi: status byte 0x%02x at offset %d truncates the preceding message", b, i)
			}
			_, n, ok := classify(b)
			if !ok {
				return nil, fmt.Errorf("usbmidi: MIDI status byte 0x%02x at offset %d is not encodable as a USB-MIDI event packet", b, i)
			}
			status, want = b, n
			if n == 1 {
				msgs = append(msgs, []byte{b})
				status, want = 0, 0
				break
			}
			acc = []byte{b}
		default:
			if want == 0 {
				return nil, fmt.Errorf("usbmidi: data byte 0x%02x at offset %d with no preceding status byte", b, i)
			}
			if acc == nil {
				acc = []byte{status} // running status
			}
			acc = append(acc, b)
			if len(acc) == want {
				msgs = append(msgs, acc)
				acc = nil
			}
		}
	}
	if acc != nil {
		return nil, fmt.Errorf("usbmidi: incomplete trailing MIDI message (%d of %d bytes)", len(acc), want)
	}
	return msgs, nil
}
