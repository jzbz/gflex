package usbfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
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

// FuzzParseDescriptors walks the TLV chain over arbitrary bytes.
//
// These bytes come straight off the device, which is as untrusted as inbound
// MIDI is: a hostile unit can present any chain it likes, and the walk is
// hand-written index arithmetic over bLength values it chooses. So the
// properties worth pinning are that no input reaches a panic and that every
// Config the parser does accept is internally consistent -- both checkable
// without a reference implementation, which is what makes them fuzzable at all.
// TestParseDescriptorsErrors covers four malformed shapes somebody thought of;
// this covers the ones nobody did.
func FuzzParseDescriptors(f *testing.F) {
	full := sampleBlob()
	f.Add(full)
	f.Add(full[:len(full)-4])                                                  // truncated mid-descriptor
	f.Add([]byte{0x12, descTypeDevice})                                        // header only, bLength past the end
	f.Add([]byte{0x0C, descTypeDevice, 0, 0, 0, 0, 0, 0x40, 0xBF, 0x37, 0, 0}) // a device descriptor and nothing else
	f.Add(append(append([]byte{}, full...), 0x00, descTypeInterface))          // zero bLength tail
	// A configuration declaring bConfigurationValue 0 -- the "unconfigured"
	// sentinel, which is also the marker for an interface that preceded any
	// configuration descriptor, so the two are indistinguishable downstream.
	f.Add([]byte{
		0x09, descTypeDevice, 0, 0, 0, 0, 0, 0, 0,
		0x09, descTypeConfig, 0x12, 0, 0x01, 0x00, 0, 0x80, 0x32,
		0x09, descTypeInterface, 0x01, 0x00, 0x02, 0x01, 0x03, 0, 0,
	})

	f.Fuzz(func(t *testing.T, b []byte) {
		cfg, err := ParseDescriptors(b)
		if err != nil {
			if cfg != nil {
				t.Fatalf("ParseDescriptors returned both a Config and an error: %v", err)
			}
			return
		}
		declared := make(map[uint8]bool, len(cfg.Configurations))
		nested := 0
		for _, c := range cfg.Configurations {
			declared[c.Value] = true
			nested += len(c.Interfaces)
		}
		for _, i := range cfg.Interfaces {
			// 0 is the documented marker for an interface descriptor that
			// preceded any configuration descriptor. Any other value must name
			// a configuration the blob actually declared -- otherwise the split
			// has attributed an interface to a configuration that does not
			// exist, and a selection confined to the active one could pick it.
			if i.ConfigurationValue != 0 && !declared[i.ConfigurationValue] {
				t.Fatalf("interface %d alt %d attributed to configuration %d, which was never declared",
					i.Number, i.Alt, i.ConfigurationValue)
			}
		}
		// Configurations is a partition of Interfaces, minus any interface that
		// preceded a configuration descriptor; it can never hold more.
		if nested > len(cfg.Interfaces) {
			t.Fatalf("%d interfaces across configurations but only %d in the union", nested, len(cfg.Interfaces))
		}
		// The blob says which configurations exist, never which one is in
		// force, so only Device.Descriptors may ever set this.
		if cfg.Active != 0 {
			t.Fatalf("ParseDescriptors set Active to %d", cfg.Active)
		}
	})
}

// descriptorDevice builds a Device whose "usbfs node" is a temporary file
// holding blob, so Descriptors can be driven with no USB device present. It has
// no SysPath, so nothing narrows to an active configuration.
func descriptorDevice(t *testing.T, blob []byte, vendorID uint16) *Device {
	t.Helper()
	p := filepath.Join(t.TempDir(), "descriptors")
	if err := os.WriteFile(p, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return &Device{f: f, ref: DeviceRef{Path: p, VendorID: vendorID}, claimed: map[int]claimState{}}
}

// skippingReaderAt answers a pread the way usbfs does for a device whose
// configuration descriptor over-claims wTotalLength: every byte of data is
// counted in the return value, but the bytes in gap are never written into the
// caller's buffer ("simply don't write (skip over) unallocated parts").
type skippingReaderAt struct {
	data     []byte
	gapStart int64
	gapEnd   int64
}

func (s skippingReaderAt) ReadAt(b []byte, off int64) (int, error) {
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := min(len(s.data)-int(off), len(b))
	for i := range n {
		abs := off + int64(i)
		if abs >= s.gapStart && abs < s.gapEnd {
			continue // counted, never written
		}
		b[i] = s.data[abs]
	}
	if int(off)+n >= len(s.data) {
		return n, io.EOF
	}
	return n, nil
}

// The bytes the kernel counts but never writes must come back as zeros, whatever
// chunk of the blob they land in. Those zeros are a bLength of 0, which
// ParseDescriptors refuses -- a deterministic rejection of a device that
// over-claims wTotalLength. The alternative is worse than the rejection: reusing
// the read buffer across chunks would fill the gap with the previous chunk's
// bytes, which parse into interfaces and endpoints the device never declared.
func TestReadDescriptorChunksZeroesBytesTheKernelSkipped(t *testing.T) {
	const chunk = 4096
	// One full chunk of non-zero bytes, then a second read whose whole payload
	// is skipped -- so any staleness shows up as 0xEE rather than as zeros.
	data := make([]byte, chunk+64)
	for i := range data {
		data[i] = 0xEE
	}
	r := skippingReaderAt{data: data, gapStart: chunk, gapEnd: int64(len(data))}

	buf, err := readDescriptorChunks(r, "/dev/bus/usb/001/007")
	if err != nil {
		t.Fatalf("readDescriptorChunks: %v", err)
	}
	if len(buf) != len(data) {
		t.Fatalf("got %d bytes, want the %d the kernel counted", len(buf), len(data))
	}
	for i, b := range buf[chunk:] {
		if b != 0 {
			t.Fatalf("byte %d of the skipped region is 0x%02x, want 0x00", chunk+i, b)
		}
	}
	if !bytes.Equal(buf[:chunk], data[:chunk]) {
		t.Error("the bytes the kernel did write were not returned intact")
	}
}

// Enumeration reads idVendor from sysfs and then synthesises the node path from
// busnum/devnum, and that path is a bus-address slot the kernel reuses -- so the
// fd may not belong to the device sysfs described. The device descriptor at the
// head of the blob is the first authoritative answer, and it has to be checked
// against what enumeration saw rather than merely parsed.
func TestDescriptorsRejectsAReplacedDevice(t *testing.T) {
	// sampleBlob's device descriptor reports 37bf:800f.
	if _, err := descriptorDevice(t, sampleBlob(), 0x37BF).Descriptors(); err != nil {
		t.Fatalf("a matching vendor ID was rejected: %v", err)
	}

	_, err := descriptorDevice(t, sampleBlob(), 0x1234).Descriptors()
	if err == nil {
		t.Fatal("a blob reporting vendor 37bf was accepted against an enumeration that saw 1234")
	}
	for _, want := range []string{"37bf", "1234"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}

	// A hand-built DeviceRef carries no vendor ID at all -- the case
	// Configuration's sysfs fallback exists for -- and the guard must not turn
	// that into a rejection.
	if _, err := descriptorDevice(t, sampleBlob(), 0).Descriptors(); err != nil {
		t.Errorf("a DeviceRef with no vendor ID was rejected: %v", err)
	}
}

// The device descriptor does not leave a usbfs node the way it left the device:
// usbdev_read byte-swaps bcdUSB, idVendor, idProduct and bcdDevice into host
// order, while the configuration trees behind it are copied out raw. Descriptors
// has to read those two fields the way the kernel wrote them, or the
// replaced-device guard above compares a byte-swapped vendor ID against the one
// sysfs reported and rejects every device. On the little-endian architectures
// this ships for (SPEC.md §12) the two orders coincide, so this pins the
// contract; a big-endian build is where it would otherwise come apart.
func TestDescriptorsReadTheDeviceIDsInHostOrder(t *testing.T) {
	blob := sampleBlob()
	binary.NativeEndian.PutUint16(blob[8:10], 0x37BF)
	binary.NativeEndian.PutUint16(blob[10:12], 0x800F)

	cfg, err := descriptorDevice(t, blob, 0x37BF).Descriptors()
	if err != nil {
		t.Fatalf("Descriptors: %v", err)
	}
	if cfg.VendorID != 0x37BF || cfg.ProductID != 0x800F {
		t.Errorf("ids = %04x:%04x, want 37bf:800f", cfg.VendorID, cfg.ProductID)
	}

	// A device descriptor the parse itself declined to decode -- here a bLength
	// running past the end of the blob -- must not acquire IDs from the
	// re-decode: the replaced-device guard reads a zero vendor ID as "the blob
	// did not say" and would otherwise be comparing invented bytes.
	short := &Config{}
	short.adjustDeviceIDsHostOrder([]byte{0x12, descTypeDevice, 0x00, 0x02})
	if short.VendorID != 0 || short.ProductID != 0 {
		t.Errorf("a truncated device descriptor yielded ids %04x:%04x", short.VendorID, short.ProductID)
	}
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
