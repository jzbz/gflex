package usbmidi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/usbfs"
)

// Errors reported by this package.
var (
	// ErrNoDevice is returned by OpenAuto when no device with the Tundra Labs
	// vendor ID is present.
	ErrNoDevice = errors.New("usbmidi: no VFLEX USB device found")
	// ErrMultipleDevices is returned by OpenAuto when more than one candidate
	// is present; the caller must pick one and call Open.
	ErrMultipleDevices = errors.New("usbmidi: more than one VFLEX USB device found")
	// ErrNoMIDIInterface is returned when the device's descriptors contain no
	// interface alt setting that looks like USB-MIDI with both directions.
	ErrNoMIDIInterface = errors.New("usbmidi: no usable USB-MIDI interface in the device descriptors")
	// ErrClosed is returned by ReadMIDI and WriteMIDI after Close.
	ErrClosed = errors.New("usbmidi: transport is closed")
)

// Interface classification constants from the USB Device Class Definition for
// MIDI Devices 1.0.
const (
	classAudio            uint8 = 0x01 // bInterfaceClass AUDIO
	classVendor           uint8 = 0xFF // bInterfaceClass VENDOR_SPECIFIC
	subClassMIDIStreaming uint8 = 0x03 // bInterfaceSubClass MIDISTREAMING
)

// Default transfer timeouts.
const (
	// DefaultReadTimeout bounds a single IN transfer. A USB IN endpoint read
	// blocks until the device has something to say, which for this protocol is
	// most of the time never, so the read loop needs to come back up for air
	// regularly enough to notice a Close. See (*transport).ReadMIDI for what a
	// timeout is translated into.
	DefaultReadTimeout = 100 * time.Millisecond
	// DefaultWriteTimeout bounds a single OUT transfer. Writes are a handful of
	// 4-byte packets, so anything short of a wedged device completes at once.
	DefaultWriteTimeout = 2 * time.Second
	// defaultPacketSize is the fallback IN buffer size when the endpoint's
	// declared maximum packet size cannot hold even one 4-byte event packet.
	// 64 is the full-speed bulk maximum and what snd-usb-audio uses for
	// USB-MIDI 1.0.
	defaultPacketSize = 64
	// maxPacketSizeMask selects the size bits of wMaxPacketSize; the rest are
	// the transactions-per-microframe multiplier (USB 2.0 §9.6.6). See bufSize.
	maxPacketSizeMask = 0x07FF
	// ctxSlack is added to the transfer timeout when deriving the context
	// deadline, so the transfer's own timeout fires first and the context
	// deadline stays a backstop.
	ctxSlack = 500 * time.Millisecond
)

// Options tunes an opened transport. The zero value selects the defaults, which
// is what both CLI entry points pass; the fields exist so the read and write
// paths can be tested without waiting out DefaultReadTimeout.
//
// There is deliberately no log sink here. One existed to dump the descriptors an
// unknown unit declared, but nothing ever wired it, and the descriptors have
// since been recorded from hardware (SPEC.md §14 Q3). What a device declares
// still reaches the user where it matters: SelectInterface puts Describe's
// output in the error it returns when nothing matches.
type Options struct {
	// ReadTimeout bounds a single IN transfer; zero means DefaultReadTimeout.
	ReadTimeout time.Duration
	// WriteTimeout bounds a single OUT transfer; zero means DefaultWriteTimeout.
	WriteTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = DefaultReadTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = DefaultWriteTimeout
	}
	return o
}

// SelectInterface picks the interface alt setting to claim for USB-MIDI.
//
// The rule from SPEC.md §4.2 is (Class == 0x01 || Class == 0xFF) &&
// SubClass == 0x03 with both an IN and an OUT endpoint. A shipped unit declares
// MIDIStreaming as audio class 01/03 on interface 1.1, with bulk endpoints 0x83
// IN and 0x02 OUT and 64-byte packets (SPEC.md §14 Q3, measured on hardware), so
// that is the interface this rule selects on a healthy device. The vendor class
// stays accepted as deliberate breadth for firmware that differs, and an
// audio-class MIDIStreaming interface is preferred when both are present.
//
// That unit's vendor-class interface 1.2 is present while the application runs,
// not only in the bootloader (SPEC.md §14, "Corrections this produced" -- the
// same mistake the bootloader package had to be fixed for). Its
// bInterfaceSubClass was not recorded, so whether it is even a candidate here is
// unknown; it loses to the audio-class interface either way.
//
// Endpoint transfer type is deliberately not part of the rule. snd-usb-audio
// accepts interrupt endpoints for USB-MIDI as readily as bulk ones
// (midi.c:2006), so hardcoding bulk here would reject a perfectly working
// device, even though the unit measured declares none.
func SelectInterface(cfg *usbfs.Config) (usbfs.Interface, error) {
	if cfg == nil {
		return usbfs.Interface{}, fmt.Errorf("%w: no configuration descriptor", ErrNoMIDIInterface)
	}
	best := -1
	bestScore := 0
	for i, iface := range cfg.Interfaces {
		if iface.SubClass != subClassMIDIStreaming {
			continue
		}
		var score int
		switch iface.Class {
		case classAudio:
			score = 2
		case classVendor:
			score = 1
		default:
			continue
		}
		if _, _, ok := endpointsFor(iface); !ok {
			continue
		}
		// Strictly greater, so the earliest interface wins a tie and selection
		// is deterministic across runs.
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		return usbfs.Interface{}, fmt.Errorf("%w; descriptors: %s", ErrNoMIDIInterface, Describe(cfg))
	}
	return cfg.Interfaces[best], nil
}

// endpointsFor returns the first usable endpoint in each direction.
//
// "Usable" is exactly snd-usb-audio's own test for a USB-MIDI endpoint
// (midi.c:2006): bulk or interrupt, nothing else. Isochronous and control
// endpoints are skipped rather than rejecting the whole interface, so an
// interface that declares a stray one alongside real MIDI endpoints still
// works. Direction comes from bit 7 of bEndpointAddress, which is set for IN.
func endpointsFor(iface usbfs.Interface) (in, out usbfs.Endpoint, ok bool) {
	var haveIn, haveOut bool
	for _, e := range iface.Endpoints {
		if !e.IsBulk() && !e.IsInterrupt() {
			continue
		}
		if e.Address&0x80 != 0 {
			if !haveIn {
				in, haveIn = e, true
			}
			continue
		}
		if !haveOut {
			out, haveOut = e, true
		}
	}
	return in, out, haveIn && haveOut
}

// Describe renders a configuration's interfaces and endpoints on one line, for
// the error returned when no interface matches. That error is how a user reports
// what a device declared, so it has to carry the whole configuration.
func Describe(cfg *usbfs.Config) string {
	if cfg == nil || len(cfg.Interfaces) == 0 {
		return "(none)"
	}
	var sb strings.Builder
	for i, iface := range cfg.Interfaces {
		if i > 0 {
			sb.WriteString("; ")
		}
		fmt.Fprintf(&sb, "iface %d alt %d class %02x/%02x/%02x",
			iface.Number, iface.Alt, iface.Class, iface.SubClass, iface.Protocol)
		for _, ep := range iface.Endpoints {
			fmt.Fprintf(&sb, " ep %02x %s mps %d", ep.Address, endpointKind(ep), ep.MaxPacketSize)
		}
	}
	return sb.String()
}

func endpointKind(ep usbfs.Endpoint) string {
	switch {
	case ep.IsBulk():
		return "bulk"
	case ep.IsInterrupt():
		return "int"
	default:
		return fmt.Sprintf("attr%02x", ep.Attributes)
	}
}

// Open claims the device's USB-MIDI interface and returns it as a
// proto.Transport. Use OpenWithOptions to override the transfer timeouts.
func Open(ref usbfs.DeviceRef) (proto.Transport, error) {
	return OpenWithOptions(ref, Options{})
}

// OpenWithOptions is Open with tunable transfer timeouts.
//
// It claims the interface with kernel-driver detach, because snd-usb-audio
// binds to it as soon as the device enumerates. usbfs performs the detach and
// the claim atomically (USBDEVFS_DISCONNECT_CLAIM), closing the race where udev
// rebinds the driver in between. While the interface is claimed the ALSA card
// and its /dev/snd/midiC*D* node are gone from the rest of the system, so any
// PipeWire or DAW client loses the port until Close (SPEC.md §4.2).
func OpenWithOptions(ref usbfs.DeviceRef, opts Options) (proto.Transport, error) {
	opts = opts.withDefaults()

	dev, err := usbfs.Open(ref)
	if err != nil {
		return nil, fmt.Errorf("usbmidi: open %s: %w", ref.Path, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = dev.Close()
		}
	}()

	cfg, err := dev.Descriptors()
	if err != nil {
		return nil, fmt.Errorf("usbmidi: read descriptors of %s: %w", ref.Path, err)
	}

	iface, err := SelectInterface(cfg)
	if err != nil {
		return nil, err
	}
	in, out, _ := endpointsFor(iface) // presence guaranteed by SelectInterface

	if err := dev.ClaimInterface(iface.Number, true); err != nil {
		return nil, fmt.Errorf("usbmidi: claim interface %d on %s: %w", iface.Number, ref.Path, err)
	}
	// Registered after the close deferral above so it runs first: release, then
	// close. A bail-out from here on must put snd-usb-audio back.
	defer func() {
		if !ok {
			_ = dev.ReleaseInterface(iface.Number)
		}
	}()

	// Alt 0 is already current after a claim; setting it again is a needless
	// round trip that some devices dislike.
	if iface.Alt != 0 {
		if err := dev.SetInterface(iface.Number, iface.Alt); err != nil {
			return nil, fmt.Errorf("usbmidi: select interface %d alt %d on %s: %w", iface.Number, iface.Alt, ref.Path, err)
		}
	}

	t := newTransport(dev, ref, iface, in, out, opts)
	ok = true
	return t, nil
}

// OpenAuto finds the single attached VFLEX by vendor ID and opens it.
func OpenAuto() (proto.Transport, usbfs.DeviceRef, error) {
	refs, err := usbfs.Enumerate(proto.VendorID)
	if err != nil {
		return nil, usbfs.DeviceRef{}, fmt.Errorf("usbmidi: enumerate usb devices: %w", err)
	}
	switch len(refs) {
	case 0:
		// The product ID is deliberately not part of the match: the vendor's
		// own app filters on the vendor ID alone and no PID is corroborated
		// anywhere (SPEC.md §14.1).
		return nil, usbfs.DeviceRef{}, fmt.Errorf("%w (vendor %04x); check that the device is attached and that the udev rule granting access to /dev/bus/usb is installed", ErrNoDevice, proto.VendorID)
	case 1:
	default:
		paths := make([]string, len(refs))
		for i, r := range refs {
			paths[i] = fmt.Sprintf("%s (bus %d addr %d)", r.Path, r.Bus, r.Addr)
		}
		return nil, usbfs.DeviceRef{}, fmt.Errorf("%w: %s", ErrMultipleDevices, strings.Join(paths, ", "))
	}
	tr, err := Open(refs[0])
	if err != nil {
		return nil, refs[0], err
	}
	return tr, refs[0], nil
}

// bufSize picks the IN scratch-buffer size from the endpoint's maximum packet
// size, falling back to 64 when the descriptor reports something that cannot
// hold even one 4-byte USB-MIDI event packet.
//
// Only bits 10:0 of wMaxPacketSize are the size. On a high-speed periodic
// endpoint bits 12:11 hold the number of additional transactions per microframe
// (USB 2.0 §9.6.6), so an interrupt endpoint declaring 0x1400 has a 1024-byte
// maximum, not a 5120-byte one. Interrupt endpoints are accepted deliberately
// (see SelectInterface), which is what makes the multiplier a live case even
// though the shipped unit declares bulk endpoints only (SPEC.md §14 Q3).
// Masking also caps the result at 2047, well under usbfs's 16384-byte transfer
// ceiling, so no descriptor value can size a buffer that usbfs.Transfer would
// refuse outright as too large -- which ReadMIDI would report as a hard error
// rather than as the timeout an idle device produces.
func bufSize(ep usbfs.Endpoint) int {
	n := int(ep.MaxPacketSize & maxPacketSizeMask)
	if n < 4 {
		return defaultPacketSize
	}
	return n
}

// usbDevice is the slice of *usbfs.Device this transport uses. It exists so the
// read, write and close paths can be exercised without hardware; the only
// production implementation is *usbfs.Device.
type usbDevice interface {
	Transfer(ctx context.Context, endpoint uint8, data []byte, timeout time.Duration) (int, error)
	ReleaseInterface(num int) error
	Close() error
}

// transport is the proto.Transport implementation over a claimed USB-MIDI
// interface.
//
// Field ownership follows the proto.Transport contract of one reader goroutine
// concurrent with one writer goroutine: scratch and pending belong to the
// reader, txbuf to the writer, and nothing else mutates after construction
// except the close state, which is atomic.
type transport struct {
	dev   usbDevice
	ref   usbfs.DeviceRef
	iface usbfs.Interface
	in    usbfs.Endpoint
	out   usbfs.Endpoint
	name  string

	readTimeout  time.Duration
	writeTimeout time.Duration

	// ctx is cancelled by Close so a transfer that has not yet been submitted is
	// refused rather than started. It does not unblock one already in flight:
	// usbfs consults the context only when deciding whether to submit and cannot
	// abort an ioctl the kernel already has (usbfs.Device.Transfer), so an
	// in-flight transfer still runs out its own timeout. That timeout, not this
	// cancellation, is what framer.Close sizes its close grace against.
	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  error

	scratch []byte // reader-owned: raw IN transfer buffer
	pending []byte // reader-owned: unpacked MIDI bytes that did not fit in p
	txbuf   []byte // writer-owned: packed event packets
}

// newTransport wires up a claimed interface. opts must already have defaults
// applied.
func newTransport(dev usbDevice, ref usbfs.DeviceRef, iface usbfs.Interface, in, out usbfs.Endpoint, opts Options) *transport {
	ctx, cancel := context.WithCancel(context.Background())
	return &transport{
		dev:          dev,
		ref:          ref,
		iface:        iface,
		in:           in,
		out:          out,
		readTimeout:  opts.ReadTimeout,
		writeTimeout: opts.WriteTimeout,
		ctx:          ctx,
		cancel:       cancel,
		scratch:      make([]byte, bufSize(in)),
		name: fmt.Sprintf("usb %04x:%04x bus %d addr %d iface %d",
			ref.VendorID, ref.ProductID, ref.Bus, ref.Addr, iface.Number),
	}
}

// Name identifies the endpoint for diagnostics, e.g.
// "usb 37bf:800f bus 1 addr 7 iface 1".
func (t *transport) Name() string { return t.name }

// WriteMIDI packs each complete MIDI message in p into a USB-MIDI event packet
// and writes the packets to the OUT endpoint, batching as many as fit in one
// wMaxPacketSize transfer.
//
// Batching is safe for this protocol: the pacing that matters is the framer's
// inter-message delay (SPEC.md §3.1), which happens above this layer by calling
// WriteMIDI once per message. A caller that hands over a whole frame at once is
// explicitly asking for the packets to go out together.
func (t *transport) WriteMIDI(p []byte) error {
	if t.closed.Load() {
		return ErrClosed
	}
	msgs, err := splitMessages(p)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return nil
	}

	perTransfer := bufSize(t.out) / 4
	if perTransfer < 1 {
		perTransfer = 1
	}
	t.txbuf = t.txbuf[:0]
	for _, m := range msgs {
		pkt := PackPacket(m)
		if pkt == nil {
			return fmt.Errorf("usbmidi: MIDI message %s cannot be encoded as a USB-MIDI event packet", proto.Hex(m))
		}
		t.txbuf = append(t.txbuf, pkt...)
		if len(t.txbuf)/4 >= perTransfer {
			if err := t.writeAll(t.txbuf); err != nil {
				return err
			}
			t.txbuf = t.txbuf[:0]
		}
	}
	if len(t.txbuf) > 0 {
		return t.writeAll(t.txbuf)
	}
	return nil
}

// writeAll pushes one transfer's worth of packets, looping if the host
// controller reports a short write.
func (t *transport) writeAll(b []byte) error {
	for len(b) > 0 {
		ctx, cancel := context.WithTimeout(t.ctx, t.writeTimeout+ctxSlack)
		n, err := t.dev.Transfer(ctx, t.out.Address, b, t.writeTimeout)
		cancel()
		if err != nil {
			if t.closed.Load() {
				return ErrClosed
			}
			return fmt.Errorf("usbmidi: write to %s ep %02x: %w", t.name, t.out.Address, err)
		}
		if n <= 0 {
			return fmt.Errorf("usbmidi: write to %s ep %02x made no progress", t.name, t.out.Address)
		}
		b = b[n:]
	}
	return nil
}

// ReadMIDI reads one transfer from the IN endpoint, unpacks the event packets
// into a plain MIDI byte stream and copies as much as fits into p, buffering
// the remainder for the next call.
//
// A transfer timeout is reported as (0, nil), not as an error. An IN endpoint
// on a half-duplex request/response device is idle almost all the time, so
// timeouts are the normal case; surfacing them as errors would fill the
// framer's error channel with noise and give a caller no way to distinguish
// "nothing to read" from "the device fell off the bus". Callers that poll
// should therefore treat a (0, nil) return as "try again", not as EOF.
//
// After Close, ReadMIDI first hands back whatever the last transfer left
// buffered and only then returns ErrClosed. Bytes the device already sent are
// not thrown away by a Close that races them in; the closed check sits after the
// drain for exactly that reason.
func (t *transport) ReadMIDI(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(t.pending) > 0 {
		n := copy(p, t.pending)
		t.pending = t.pending[n:]
		return n, nil
	}
	if t.closed.Load() {
		return 0, ErrClosed
	}

	ctx, cancel := context.WithTimeout(t.ctx, t.readTimeout+ctxSlack)
	n, err := t.dev.Transfer(ctx, t.in.Address, t.scratch, t.readTimeout)
	cancel()
	if err != nil {
		if t.closed.Load() {
			return 0, ErrClosed
		}
		if isTimeout(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("usbmidi: read from %s ep %02x: %w", t.name, t.in.Address, err)
	}
	if n <= 0 {
		return 0, nil
	}

	midi := UnpackPackets(t.scratch[:n])
	if len(midi) == 0 {
		// A transfer of nothing but padding packets: legal, not an error.
		return 0, nil
	}
	c := copy(p, midi)
	// UnpackPackets allocates a fresh slice, so retaining its tail cannot alias
	// the scratch buffer the next transfer overwrites.
	t.pending = midi[c:]
	return c, nil
}

// Close releases the interface and closes the device.
//
// Releasing is what makes snd-usb-audio rebind, restoring the ALSA card and the
// /dev/snd/midiC*D* node that claiming took away. It is attempted even if the
// device handle is in a bad state, and its error is reported rather than
// swallowed, because a silently skipped release leaves the user's MIDI port
// missing until they replug the device.
func (t *transport) Close() error {
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		t.cancel() // refuse any transfer not yet submitted
		var errs []error
		if err := t.dev.ReleaseInterface(t.iface.Number); err != nil {
			errs = append(errs, fmt.Errorf("usbmidi: release interface %d (the ALSA MIDI port may stay missing until replug): %w", t.iface.Number, err))
		}
		if err := t.dev.Close(); err != nil {
			errs = append(errs, fmt.Errorf("usbmidi: close %s: %w", t.ref.Path, err))
		}
		t.closeErr = errors.Join(errs...)
	})
	return t.closeErr
}

// isTimeout reports whether err is a transfer timeout rather than a real
// failure.
//
// usbfs.ErrTimeout is the documented sentinel and the raw ETIMEDOUT is what it
// unwraps to, but both are checked, along with a context deadline (usbfs
// refuses to submit a transfer whose deadline has already passed) and, as a
// last resort, the error text. Misreading a genuine failure as a timeout would
// turn a dead device into a silent stall, so nothing broader is matched.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, usbfs.ErrTimeout) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var te interface{ Timeout() bool }
	if errors.As(err, &te) && te.Timeout() {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "timed out") || strings.Contains(s, "timeout")
}
