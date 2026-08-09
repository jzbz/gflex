package usbfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// multiConfigBlob is a descriptor blob for a device declaring two
// configurations, which is the shape that makes flattening them together wrong:
// interface number 1 appears in both, naming two entirely different interfaces.
// Configuration 1 puts the vendor-class bulk pair there; configuration 2 puts an
// unrelated HID interface at the same number.
func multiConfigBlob() []byte {
	var b []byte
	add := func(d ...byte) { b = append(b, d...) }

	// Device descriptor, bNumConfigurations = 2.
	add(0x12, descTypeDevice, 0x00, 0x02, 0x00, 0x00, 0x00, 0x40,
		0xBF, 0x37, 0x0F, 0x80, 0x00, 0x01, 0x01, 0x02, 0x03, 0x02)

	// Configuration 1: bConfigurationValue = 1.
	add(0x09, descTypeConfig, 0x20, 0x00, 0x02, 0x01, 0x00, 0x80, 0x32)
	// Interface 0 alt 0: AudioControl, no endpoints.
	add(0x09, descTypeInterface, 0x00, 0x00, 0x00, 0x01, 0x01, 0x00, 0x00)
	// Interface 1 alt 0: vendor class with a bulk pair -- the bootloader shape.
	add(0x09, descTypeInterface, 0x01, 0x00, 0x02, 0xFF, 0x00, 0x00, 0x00)
	add(0x07, descTypeEndpoint, 0x01, 0x02, 0x40, 0x00, 0x00)
	add(0x07, descTypeEndpoint, 0x81, 0x02, 0x40, 0x00, 0x00)

	// Configuration 2: bConfigurationValue = 2. Deliberately not contiguous
	// with an index, and reusing interface number 1 for something else.
	add(0x09, descTypeConfig, 0x18, 0x00, 0x01, 0x02, 0x00, 0x80, 0x32)
	// Interface 1 alt 0: HID, one interrupt IN endpoint. Vendor class 0xFF is
	// absent here, so a bootloader selection confined to this configuration must
	// find nothing at all.
	add(0x09, descTypeInterface, 0x01, 0x00, 0x01, 0x03, 0x00, 0x00, 0x00)
	add(0x07, descTypeEndpoint, 0x83, 0x03, 0x08, 0x00, 0x0A)

	return b
}

// Each interface must record the configuration that declared it, and the
// configurations must be reported separately -- the whole point being that
// interface 1 means two different things here.
func TestParseDescriptorsSplitsConfigurations(t *testing.T) {
	cfg, err := ParseDescriptors(multiConfigBlob())
	if err != nil {
		t.Fatalf("ParseDescriptors: %v", err)
	}
	if len(cfg.Configurations) != 2 {
		t.Fatalf("got %d configurations, want 2: %+v", len(cfg.Configurations), cfg.Configurations)
	}
	if cfg.Configurations[0].Value != 1 || cfg.Configurations[1].Value != 2 {
		t.Errorf("bConfigurationValues = %d, %d; want 1, 2",
			cfg.Configurations[0].Value, cfg.Configurations[1].Value)
	}
	if n := len(cfg.Configurations[0].Interfaces); n != 2 {
		t.Errorf("configuration 1 has %d interfaces, want 2", n)
	}
	if n := len(cfg.Configurations[1].Interfaces); n != 1 {
		t.Errorf("configuration 2 has %d interfaces, want 1", n)
	}

	// ParseDescriptors alone cannot know which one is live, so it must not
	// pretend: Active stays 0 and Interfaces stays the union.
	if cfg.Active != 0 {
		t.Errorf("ParseDescriptors set Active = %d, want 0", cfg.Active)
	}
	if len(cfg.Interfaces) != 3 {
		t.Fatalf("union has %d interfaces, want 3", len(cfg.Interfaces))
	}
	want := []uint8{1, 1, 2}
	for i, iface := range cfg.Interfaces {
		if iface.ConfigurationValue != want[i] {
			t.Errorf("interface %d (number %d) belongs to configuration %d, want %d",
				i, iface.Number, iface.ConfigurationValue, want[i])
		}
	}

	// The two interface number 1s are distinguishable only by configuration.
	in1 := cfg.InterfacesIn(1)
	if len(in1) != 2 || in1[1].Number != 1 || in1[1].Class != 0xFF {
		t.Errorf("InterfacesIn(1) = %+v, want interfaces 0 and vendor-class 1", in1)
	}
	in2 := cfg.InterfacesIn(2)
	if len(in2) != 1 || in2[0].Number != 1 || in2[0].Class != 0x03 {
		t.Errorf("InterfacesIn(2) = %+v, want the HID interface 1", in2)
	}
	if got := cfg.InterfacesIn(3); got != nil {
		t.Errorf("InterfacesIn(3) = %+v, want nil for a configuration that is not declared", got)
	}
}

// A single-configuration device -- every VFLEX anyone has seen -- must parse
// exactly as it did before configurations were tracked.
func TestParseDescriptorsSingleConfiguration(t *testing.T) {
	cfg, err := ParseDescriptors(sampleBlob())
	if err != nil {
		t.Fatalf("ParseDescriptors: %v", err)
	}
	if len(cfg.Configurations) != 1 || cfg.Configurations[0].Value != 1 {
		t.Fatalf("configurations = %+v, want one with value 1", cfg.Configurations)
	}
	if len(cfg.Configurations[0].Interfaces) != 3 || len(cfg.Interfaces) != 3 {
		t.Errorf("interface counts = %d in the configuration, %d in the union; want 3 and 3",
			len(cfg.Configurations[0].Interfaces), len(cfg.Interfaces))
	}
	for _, iface := range cfg.Interfaces {
		if iface.ConfigurationValue != 1 {
			t.Errorf("interface %d alt %d has configuration %d, want 1",
				iface.Number, iface.Alt, iface.ConfigurationValue)
		}
	}
}

func TestRestrictToActive(t *testing.T) {
	// The narrowing that matters: configuration 2 is live, so the vendor-class
	// interface of configuration 1 must not be selectable.
	cfg, err := ParseDescriptors(multiConfigBlob())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Active = 2
	cfg.restrictToActive()
	if len(cfg.Interfaces) != 1 || cfg.Interfaces[0].Class != 0x03 {
		t.Errorf("Interfaces = %+v, want only the HID interface of configuration 2", cfg.Interfaces)
	}
	// Configurations is never narrowed: diagnostics still show the whole device.
	if len(cfg.Configurations) != 2 {
		t.Errorf("Configurations was narrowed too: %+v", cfg.Configurations)
	}

	tests := []struct {
		name   string
		active uint8
		blob   []byte
		want   int
	}{
		// Unknown or unconfigured: nothing to narrow to, so nothing is dropped.
		{"active unknown", 0, multiConfigBlob(), 3},
		// A value sysfs reports but the descriptors do not declare is a
		// disagreement, not a licence to return an empty descriptor set.
		{"active not declared", 7, multiConfigBlob(), 3},
		// One configuration: byte-identical behaviour to before.
		{"single configuration", 1, sampleBlob(), 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseDescriptors(tc.blob)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Active = tc.active
			cfg.restrictToActive()
			if len(cfg.Interfaces) != tc.want {
				t.Errorf("got %d interfaces, want %d: %+v", len(cfg.Interfaces), tc.want, cfg.Interfaces)
			}
		})
	}
}

func TestFirstConfigurationValue(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want uint8
	}{
		// SPEC.md §10.1's "configuration 1", arrived at from the descriptors.
		{"declared", Config{Configurations: []Configuration{{Value: 1}, {Value: 2}}}, 1},
		// The first value is used, not the literal 1: numbering is a convention.
		{"not numbered from one", Config{Configurations: []Configuration{{Value: 3}}}, 3},
		// 0 is the "unconfigured" sentinel; selecting it would deconfigure the
		// device, so it can never be the answer.
		{"zero skipped", Config{Configurations: []Configuration{{Value: 0}, {Value: 4}}}, 4},
		{"none declared", Config{}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.FirstConfigurationValue(); got != tc.want {
				t.Errorf("FirstConfigurationValue = %d, want %d", got, tc.want)
			}
		})
	}
}

// sysfsDevice builds a Device whose SysPath points at a fixture directory. It
// deliberately has no usable fd: these tests only exercise the sysfs read.
func sysfsDevice(t *testing.T, attr string) *Device {
	t.Helper()
	dir := t.TempDir()
	if attr != "" {
		if err := os.WriteFile(filepath.Join(dir, "bConfigurationValue"), []byte(attr), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Device{ref: DeviceRef{Path: "/dev/bus/usb/001/007", SysPath: dir}, claimed: map[int]bool{}}
}

func TestSysfsConfiguration(t *testing.T) {
	tests := []struct {
		name  string
		attr  string
		want  uint8
		wantK bool
	}{
		{"configured", "1\n", 1, true},
		{"unconfigured", "0\n", 0, true},
		{"high value", "255\n", 255, true},
		{"absent", "", 0, false},
		{"not a number", "wat\n", 0, false},
		// Out of range for a bConfigurationValue; treated as unreadable rather
		// than truncated to a byte that would select something real.
		{"out of range", "256\n", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := sysfsDevice(t, tc.attr)
			got, ok := d.sysfsConfiguration()
			if got != tc.want || ok != tc.wantK {
				t.Errorf("sysfsConfiguration = %d, %v; want %d, %v", got, ok, tc.want, tc.wantK)
			}
		})
	}
	// No sysfs path at all: the GET_CONFIGURATION fallback's reason for existing.
	d := &Device{ref: DeviceRef{Path: "/dev/bus/usb/001/007"}, claimed: map[int]bool{}}
	if _, ok := d.sysfsConfiguration(); ok {
		t.Error("a DeviceRef with no SysPath reported a configuration")
	}
}

// A device that already reports a configuration must not be written to: usbfs
// turns a redundant SET_CONFIGURATION into usb_reset_configuration(), and this
// runs immediately before a firmware write. There is no fd here, so any attempt
// to issue the ioctl would fail loudly rather than silently pass.
func TestEnsureConfiguredLeavesAConfiguredDeviceAlone(t *testing.T) {
	d := sysfsDevice(t, "1\n")
	got, err := d.EnsureConfigured(context.Background())
	if err != nil {
		t.Fatalf("EnsureConfigured: %v", err)
	}
	if got != 1 {
		t.Errorf("EnsureConfigured = %d, want 1", got)
	}
}

// When neither sysfs nor the device answers, the failure has to be
// distinguishable from "unconfigured": the caller may select a configuration in
// response to the second and must not in response to the first.
func TestConfigurationUnknownIsDistinct(t *testing.T) {
	// No SysPath, and an fd that is not a usbfs node, so the control-transfer
	// fallback answers ENOTTY -- standing in for a device that will not answer
	// GET_CONFIGURATION.
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := &Device{ref: DeviceRef{Path: "/dev/bus/usb/001/007"}, claimed: map[int]bool{}, f: f}
	_, err = d.Configuration(context.Background())
	if !errors.Is(err, ErrConfigUnknown) {
		t.Fatalf("Configuration err = %v, want ErrConfigUnknown", err)
	}
	if _, err := d.EnsureConfigured(context.Background()); !errors.Is(err, ErrConfigUnknown) {
		t.Fatalf("EnsureConfigured err = %v, want ErrConfigUnknown", err)
	}
}
