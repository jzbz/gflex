package usbmidi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"github.com/jzbz/gflex/internal/usbfs"
)

const (
	attrControl   uint8 = 0x00
	attrIsoch     uint8 = 0x01
	attrBulk      uint8 = 0x02
	attrInterrupt uint8 = 0x03
)

func ep(addr, attrs uint8, mps uint16) usbfs.Endpoint {
	return usbfs.Endpoint{Address: addr, Attributes: attrs, MaxPacketSize: mps}
}

// A plausible audio-class control interface, which must never be selected: it
// is subclass 0x01 (AUDIOCONTROL), not 0x03 (MIDISTREAMING).
func audioControlIface(num int) usbfs.Interface {
	return usbfs.Interface{Number: num, Alt: 0, Class: classAudio, SubClass: 0x01}
}

func TestSelectInterface(t *testing.T) {
	bulkMIDI := usbfs.Interface{
		Number: 1, Alt: 0, Class: classAudio, SubClass: subClassMIDIStreaming,
		Endpoints: []usbfs.Endpoint{ep(0x01, attrBulk, 64), ep(0x81, attrBulk, 64)},
	}
	// SPEC.md §4.2: snd-usb-audio accepts interrupt endpoints for USB-MIDI, so
	// an interrupt-only interface must still be selected.
	interruptMIDI := usbfs.Interface{
		Number: 2, Alt: 1, Class: classVendor, SubClass: subClassMIDIStreaming,
		Endpoints: []usbfs.Endpoint{ep(0x82, attrInterrupt, 32), ep(0x02, attrInterrupt, 32)},
	}

	tests := []struct {
		name    string
		cfg     *usbfs.Config
		wantNum int
		wantErr bool
	}{
		{
			name:    "audio class bulk",
			cfg:     &usbfs.Config{Interfaces: []usbfs.Interface{audioControlIface(0), bulkMIDI}},
			wantNum: 1,
		},
		{
			name:    "vendor class interrupt only",
			cfg:     &usbfs.Config{Interfaces: []usbfs.Interface{audioControlIface(0), interruptMIDI}},
			wantNum: 2,
		},
		{
			name:    "audio class preferred over vendor class",
			cfg:     &usbfs.Config{Interfaces: []usbfs.Interface{interruptMIDI, bulkMIDI}},
			wantNum: 1,
		},
		{
			name: "first of two equal candidates wins",
			cfg: &usbfs.Config{Interfaces: []usbfs.Interface{
				bulkMIDI,
				{Number: 5, Class: classAudio, SubClass: subClassMIDIStreaming,
					Endpoints: []usbfs.Endpoint{ep(0x03, attrBulk, 64), ep(0x83, attrBulk, 64)}},
			}},
			wantNum: 1,
		},
		{
			name: "wrong subclass rejected",
			cfg: &usbfs.Config{Interfaces: []usbfs.Interface{
				{Number: 1, Class: classAudio, SubClass: 0x02,
					Endpoints: []usbfs.Endpoint{ep(0x01, attrBulk, 64), ep(0x81, attrBulk, 64)}},
			}},
			wantErr: true,
		},
		{
			name: "wrong class rejected",
			cfg: &usbfs.Config{Interfaces: []usbfs.Interface{
				{Number: 1, Class: 0x03 /* HID */, SubClass: subClassMIDIStreaming,
					Endpoints: []usbfs.Endpoint{ep(0x01, attrBulk, 64), ep(0x81, attrBulk, 64)}},
			}},
			wantErr: true,
		},
		{
			name: "out endpoint only rejected",
			cfg: &usbfs.Config{Interfaces: []usbfs.Interface{
				{Number: 1, Class: classAudio, SubClass: subClassMIDIStreaming,
					Endpoints: []usbfs.Endpoint{ep(0x01, attrBulk, 64)}},
			}},
			wantErr: true,
		},
		{
			name: "in endpoint only rejected",
			cfg: &usbfs.Config{Interfaces: []usbfs.Interface{
				{Number: 1, Class: classAudio, SubClass: subClassMIDIStreaming,
					Endpoints: []usbfs.Endpoint{ep(0x81, attrBulk, 64)}},
			}},
			wantErr: true,
		},
		{
			// snd-usb-audio skips anything that is not bulk or interrupt, and
			// usbfs cannot drive an isochronous endpoint at all.
			name: "isochronous endpoints rejected",
			cfg: &usbfs.Config{Interfaces: []usbfs.Interface{
				{Number: 1, Class: classAudio, SubClass: subClassMIDIStreaming,
					Endpoints: []usbfs.Endpoint{ep(0x01, attrIsoch, 64), ep(0x81, attrIsoch, 64)}},
			}},
			wantErr: true,
		},
		{
			name: "stray control endpoint skipped, real ones still found",
			cfg: &usbfs.Config{Interfaces: []usbfs.Interface{
				{Number: 4, Class: classAudio, SubClass: subClassMIDIStreaming,
					Endpoints: []usbfs.Endpoint{
						ep(0x85, attrControl, 8), ep(0x05, attrControl, 8),
						ep(0x86, attrInterrupt, 64), ep(0x06, attrBulk, 64),
					}},
			}},
			wantNum: 4,
		},
		{
			name:    "no interfaces",
			cfg:     &usbfs.Config{},
			wantErr: true,
		},
		{
			name:    "nil config",
			cfg:     nil,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectInterface(tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SelectInterface = interface %d, want error", got.Number)
				}
				if !errors.Is(err, ErrNoMIDIInterface) {
					t.Fatalf("error %v does not wrap ErrNoMIDIInterface", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SelectInterface: %v", err)
			}
			if got.Number != tc.wantNum {
				t.Fatalf("selected interface %d, want %d", got.Number, tc.wantNum)
			}
			in, out, ok := endpointsFor(got)
			if !ok {
				t.Fatal("selected interface has no usable endpoint pair")
			}
			if in.Address&0x80 == 0 || out.Address&0x80 != 0 {
				t.Errorf("endpoints have the wrong direction: in %02x out %02x", in.Address, out.Address)
			}
			for _, e := range []usbfs.Endpoint{in, out} {
				if !e.IsBulk() && !e.IsInterrupt() {
					t.Errorf("endpoint %02x is neither bulk nor interrupt (attrs %02x)", e.Address, e.Attributes)
				}
			}
		})
	}
}

// The failure message has to say what the device did declare, because the
// VFLEX's real descriptors are unknown (SPEC.md §14.3) and this error is how a
// user reports them.
func TestSelectInterfaceErrorDescribesDescriptors(t *testing.T) {
	cfg := &usbfs.Config{Interfaces: []usbfs.Interface{
		{Number: 3, Alt: 2, Class: classAudio, SubClass: 0x01,
			Endpoints: []usbfs.Endpoint{ep(0x84, attrInterrupt, 9)}},
	}}
	_, err := SelectInterface(cfg)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"iface 3", "alt 2", "ep 84", "int"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestDescribe(t *testing.T) {
	if got := Describe(nil); got != "(none)" {
		t.Errorf("Describe(nil) = %q", got)
	}
	if got := Describe(&usbfs.Config{}); got != "(none)" {
		t.Errorf("Describe(empty) = %q", got)
	}
	cfg := &usbfs.Config{Interfaces: []usbfs.Interface{
		{Number: 1, Alt: 0, Class: classAudio, SubClass: subClassMIDIStreaming, Protocol: 0,
			Endpoints: []usbfs.Endpoint{ep(0x01, attrBulk, 64), ep(0x81, attrInterrupt, 64)}},
	}}
	got := Describe(cfg)
	for _, want := range []string{"iface 1", "class 01/03/00", "ep 01 bulk mps 64", "ep 81 int mps 64"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe = %q, missing %q", got, want)
		}
	}
}

func TestBufSize(t *testing.T) {
	tests := []struct {
		mps  uint16
		want int
	}{
		{64, 64},
		{512, 512},
		{4, 4},
		{0, defaultPacketSize}, // descriptor unreadable
		{3, defaultPacketSize}, // smaller than one event packet
	}
	for _, tc := range tests {
		if got := bufSize(ep(0x81, attrBulk, tc.mps)); got != tc.want {
			t.Errorf("bufSize(mps=%d) = %d, want %d", tc.mps, got, tc.want)
		}
	}
}

// Every shape a timeout can arrive in must be recognised, and nothing else may
// be, or a dead device would look like an idle one.
func TestIsTimeout(t *testing.T) {
	timeouts := []error{
		usbfs.ErrTimeout,
		&usbfs.Error{Op: "bulk transfer", Errno: syscall.ETIMEDOUT, Class: usbfs.ErrTimeout},
		syscall.ETIMEDOUT,
		fmt.Errorf("usbfs: transfer: %w", syscall.ETIMEDOUT),
		context.DeadlineExceeded,
		fmt.Errorf("usbfs: %w", context.DeadlineExceeded),
		errors.New("usbfs: bulk transfer timed out"),
		errors.New("usbfs: timeout waiting for URB"),
	}
	for _, err := range timeouts {
		if !isTimeout(err) {
			t.Errorf("isTimeout(%v) = false, want true", err)
		}
	}
	notTimeouts := []error{
		nil,
		syscall.ENODEV,
		usbfs.ErrNoDevice,
		usbfs.ErrBusy,
		&usbfs.Error{Op: "bulk transfer", Errno: syscall.ENODEV, Class: usbfs.ErrNoDevice},
		fmt.Errorf("usbfs: submit urb: %w", syscall.EPIPE),
		errors.New("usbfs: device disconnected"),
		context.Canceled,
	}
	for _, err := range notTimeouts {
		if isTimeout(err) {
			t.Errorf("isTimeout(%v) = true, want false", err)
		}
	}
}

func TestOptionsDefaults(t *testing.T) {
	o := Options{}.withDefaults()
	if o.ReadTimeout != DefaultReadTimeout || o.WriteTimeout != DefaultWriteTimeout {
		t.Fatalf("withDefaults = %+v", o)
	}
	o = Options{ReadTimeout: 1, WriteTimeout: 2}.withDefaults()
	if o.ReadTimeout != 1 || o.WriteTimeout != 2 {
		t.Fatalf("withDefaults overrode explicit values: %+v", o)
	}
	var lines []string
	o = Options{Log: func(s string) { lines = append(lines, s) }}.withDefaults()
	o.logf("hello %d", 42)
	if len(lines) != 1 || lines[0] != "hello 42" {
		t.Fatalf("logf produced %v", lines)
	}
	// A nil Log must be a no-op, not a panic.
	Options{}.logf("ignored")
}
