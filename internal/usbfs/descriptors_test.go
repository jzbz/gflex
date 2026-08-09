package usbfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleBlob is a hand-built descriptor blob in the shape usbfs returns:
// device descriptor, configuration descriptor, then interfaces with their
// endpoints, with class-specific descriptors interleaved the way a real
// USB-Audio/MIDI device does it.
func sampleBlob() []byte {
	var b []byte
	add := func(d ...byte) { b = append(b, d...) }

	// Device descriptor. idVendor 0x37BF (Tundra Labs), idProduct 0x800F,
	// both little-endian.
	add(0x12, descTypeDevice, 0x00, 0x02, 0x00, 0x00, 0x00, 0x40,
		0xBF, 0x37, 0x0F, 0x80, 0x00, 0x01, 0x01, 0x02, 0x03, 0x01)

	// Configuration descriptor: skipped by the parser, but must not derail it.
	add(0x09, descTypeConfig, 0x62, 0x00, 0x02, 0x01, 0x00, 0x80, 0x32)

	// Interface 0 alt 0: AudioControl, no endpoints.
	add(0x09, descTypeInterface, 0x00, 0x00, 0x00, 0x01, 0x01, 0x00, 0x00)
	// Class-specific AC header (0x24) -- must be skipped whole.
	add(0x09, descTypeCSInterface, 0x01, 0x00, 0x01, 0x09, 0x00, 0x01, 0x01)

	// Interface 1 alt 0: MIDIStreaming, two endpoints.
	add(0x09, descTypeInterface, 0x01, 0x00, 0x02, 0x01, 0x03, 0x00, 0x00)
	// Class-specific MS header (0x24).
	add(0x07, descTypeCSInterface, 0x01, 0x00, 0x01, 0x41, 0x00)
	// OUT endpoint 0x01, bulk, 64 bytes -- in the 9-byte audio form.
	add(0x09, descTypeEndpoint, 0x01, 0x02, 0x40, 0x00, 0x00, 0x00, 0x00)
	// Class-specific endpoint descriptor (0x25) -- must be skipped.
	add(0x05, descTypeCSEndpoint, 0x01, 0x01, 0x01)
	// IN endpoint 0x81, interrupt, 64 bytes, bInterval 4 -- 7-byte form.
	add(0x07, descTypeEndpoint, 0x81, 0x03, 0x40, 0x00, 0x04)
	add(0x05, descTypeCSEndpoint, 0x01, 0x01, 0x01)

	// Interface 1 alt 1: vendor class, one 512-byte bulk OUT endpoint.
	add(0x09, descTypeInterface, 0x01, 0x01, 0x01, 0xFF, 0x03, 0x00, 0x00)
	add(0x07, descTypeEndpoint, 0x02, 0x02, 0x00, 0x02, 0x00)

	return b
}

func TestParseDescriptors(t *testing.T) {
	cfg, err := ParseDescriptors(sampleBlob())
	if err != nil {
		t.Fatalf("ParseDescriptors: %v", err)
	}
	if cfg.VendorID != 0x37BF || cfg.ProductID != 0x800F {
		t.Errorf("ids = %04x:%04x, want 37bf:800f", cfg.VendorID, cfg.ProductID)
	}
	if len(cfg.Interfaces) != 3 {
		t.Fatalf("got %d interfaces, want 3: %v", len(cfg.Interfaces), cfg.Interfaces)
	}

	// Interface 0: the class-specific descriptor after it must not have been
	// mistaken for an endpoint.
	i0 := cfg.Interfaces[0]
	if i0.Number != 0 || i0.Alt != 0 || i0.Class != 0x01 || i0.SubClass != 0x01 {
		t.Errorf("interface 0 = %+v", i0)
	}
	if len(i0.Endpoints) != 0 {
		t.Errorf("interface 0 has %d endpoints, want 0", len(i0.Endpoints))
	}
	if _, ok := i0.In(); ok {
		t.Error("interface 0 reported an IN endpoint")
	}
	if _, ok := i0.Out(); ok {
		t.Error("interface 0 reported an OUT endpoint")
	}

	// Interface 1 alt 0: the MIDI streaming pair.
	i1 := cfg.Interfaces[1]
	if i1.Number != 1 || i1.Alt != 0 || i1.Class != 0x01 || i1.SubClass != 0x03 {
		t.Errorf("interface 1 alt 0 = %+v", i1)
	}
	if len(i1.Endpoints) != 2 {
		t.Fatalf("interface 1 alt 0 has %d endpoints, want 2", len(i1.Endpoints))
	}
	out, ok := i1.Out()
	if !ok {
		t.Fatal("interface 1 alt 0 has no OUT endpoint")
	}
	if out.Address != 0x01 || !out.IsBulk() || out.IsInterrupt() || out.IsIn() {
		t.Errorf("OUT endpoint = %+v (%s)", out, out)
	}
	if out.MaxPacketSize != 64 {
		t.Errorf("OUT wMaxPacketSize = %d, want 64", out.MaxPacketSize)
	}
	in, ok := i1.In()
	if !ok {
		t.Fatal("interface 1 alt 0 has no IN endpoint")
	}
	if in.Address != 0x81 || !in.IsIn() {
		t.Errorf("IN endpoint = %+v", in)
	}
	// The whole point of reading bmAttributes: this one is interrupt, not bulk.
	if !in.IsInterrupt() || in.IsBulk() {
		t.Errorf("IN endpoint attrs 0x%02x: IsInterrupt=%v IsBulk=%v, want true/false",
			in.Attributes, in.IsInterrupt(), in.IsBulk())
	}
	if in.Interval != 4 || in.Number() != 1 {
		t.Errorf("IN endpoint interval=%d number=%d, want 4/1", in.Interval, in.Number())
	}

	// Interface 1 alt 1 must be reported separately from alt 0.
	i2 := cfg.Interfaces[2]
	if i2.Number != 1 || i2.Alt != 1 || i2.Class != 0xFF {
		t.Errorf("interface 1 alt 1 = %+v", i2)
	}
	if len(i2.Endpoints) != 1 || i2.Endpoints[0].MaxPacketSize != 512 {
		t.Errorf("interface 1 alt 1 endpoints = %+v", i2.Endpoints)
	}
	if _, ok := i2.In(); ok {
		t.Error("interface 1 alt 1 reported an IN endpoint")
	}
}

func TestFindInterface(t *testing.T) {
	cfg, err := ParseDescriptors(sampleBlob())
	if err != nil {
		t.Fatal(err)
	}
	// The selection rule the USB-MIDI transport uses (SPEC.md §4.2).
	got, ok := cfg.FindInterface(func(i Interface) bool {
		return (i.Class == 0x01 || i.Class == 0xFF) && i.SubClass == 0x03
	})
	if !ok || got.Number != 1 || got.Alt != 0 {
		t.Errorf("FindInterface = %+v, %v; want interface 1 alt 0", got, ok)
	}
	if _, ok := cfg.FindInterface(func(i Interface) bool { return i.Class == 0x03 }); ok {
		t.Error("FindInterface matched a class that is not present")
	}
}

func TestParseDescriptorsErrors(t *testing.T) {
	tests := []struct {
		name string
		blob []byte
	}{
		{"empty", nil},
		{"one byte", []byte{0x12}},
		{"not a device descriptor", []byte{0x09, descTypeConfig, 0, 0, 0, 0, 0, 0, 0}},
		{"zero bLength", append(sampleBlob(), 0x00, descTypeInterface)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseDescriptors(tc.blob); !errors.Is(err, ErrBadDescriptor) {
				t.Fatalf("err = %v, want ErrBadDescriptor", err)
			}
		})
	}
}

// TestParseDescriptorsRealDevices runs the parser over every USB device
// attached to this machine. sysfs exposes the same blob usbfs does, in the same
// order, in a world-readable file -- so this is real-hardware coverage of the
// TLV walk (hubs, HID, UVC, UAC with their class-specific descriptors) with no
// privileges and no device access. Skipped when sysfs is absent.
func TestParseDescriptorsRealDevices(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join(DefaultSysfsRoot, "*", "descriptors"))
	if err != nil || len(matches) == 0 {
		t.Skip("no USB descriptor blobs in sysfs on this machine")
	}
	parsed := 0
	for _, p := range matches {
		dir := filepath.Dir(p)
		if strings.Contains(filepath.Base(dir), ":") {
			continue // interface node, not a device
		}
		blob, err := os.ReadFile(p)
		if err != nil {
			continue // the device may have been unplugged mid-test
		}
		cfg, err := ParseDescriptors(blob)
		if err != nil {
			t.Errorf("%s (%d bytes): %v", p, len(blob), err)
			continue
		}
		// The device descriptor at the head of the blob must agree with what
		// sysfs reports separately.
		if vid, ok := readHexAttr(dir, "idVendor"); ok && vid != cfg.VendorID {
			t.Errorf("%s: parsed idVendor %04x, sysfs says %04x", p, cfg.VendorID, vid)
		}
		if pid, ok := readHexAttr(dir, "idProduct"); ok && pid != cfg.ProductID {
			t.Errorf("%s: parsed idProduct %04x, sysfs says %04x", p, cfg.ProductID, pid)
		}
		if len(cfg.Interfaces) == 0 {
			t.Errorf("%s: no interfaces parsed from %d bytes", p, len(blob))
		}
		// The blob carries one configuration descriptor tree per
		// bNumConfigurations (device descriptor byte 17), so the split has to
		// account for all of them -- a miscount means interfaces were attributed
		// to the wrong configuration, which is exactly what the split exists to
		// prevent. This is the only coverage of multi-configuration devices that
		// does not depend on a hand-built blob.
		if want := int(blob[17]); len(cfg.Configurations) != want {
			t.Errorf("%s: parsed %d configurations, bNumConfigurations says %d",
				p, len(cfg.Configurations), want)
		}
		for _, iface := range cfg.Interfaces {
			if iface.ConfigurationValue == 0 {
				t.Errorf("%s: interface %d alt %d was not attributed to any configuration",
					p, iface.Number, iface.Alt)
			}
		}
		parsed++
	}
	t.Logf("parsed %d real descriptor blobs", parsed)
}

// A blob cut short mid-descriptor keeps everything already parsed rather than
// failing outright; a partial read should not lose the interfaces we did see.
func TestParseDescriptorsTruncated(t *testing.T) {
	full := sampleBlob()
	cut := full[:len(full)-4] // chops the trailing endpoint descriptor
	cfg, err := ParseDescriptors(cut)
	if err != nil {
		t.Fatalf("ParseDescriptors: %v", err)
	}
	if len(cfg.Interfaces) != 3 {
		t.Fatalf("got %d interfaces, want 3", len(cfg.Interfaces))
	}
	if n := len(cfg.Interfaces[2].Endpoints); n != 0 {
		t.Errorf("truncated endpoint was decoded anyway: %d endpoints", n)
	}
	if len(cfg.Interfaces[1].Endpoints) != 2 {
		t.Errorf("interfaces before the truncation lost endpoints: %+v", cfg.Interfaces[1])
	}
}
