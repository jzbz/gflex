package usbfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSysfs builds a sysfs-shaped fixture tree. Each entry maps a directory
// name to the attribute files inside it; a nil map means an empty directory.
func fakeSysfs(t *testing.T, tree map[string]map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, attrs := range tree {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range attrs {
			if err := os.WriteFile(filepath.Join(dir, k), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func usbDev(vid, pid, bus, dev string) map[string]string {
	return map[string]string{"idVendor": vid, "idProduct": pid, "busnum": bus, "devnum": dev}
}

func fixtureTree() map[string]map[string]string {
	return map[string]map[string]string{
		// The unit we are looking for.
		"1-1": usbDev("37bf", "800f", "1", "7"),
		// An interface directory. Real ones carry no idVendor, but this one
		// does, to prove the colon filter runs before the attribute read.
		"1-1:1.0": usbDev("37bf", "800f", "1", "7"),
		// A second VFLEX on another bus, to exercise the sort.
		"2-1.4": usbDev("37bf", "800f", "2", "12"),
		// Root hubs and an unrelated device.
		"usb1": usbDev("1d6b", "0002", "1", "1"),
		"3-2":  usbDev("046d", "c52b", "3", "4"),
		// Missing devnum: no usbfs node can be addressed, so it is skipped.
		"4-1": {"idVendor": "37bf", "idProduct": "800f", "busnum": "4"},
		// Not a USB device directory at all.
		"power": nil,
		// Unparseable attribute values.
		"5-1": usbDev("zzzz", "800f", "5", "1"),
		"6-1": usbDev("37bf", "800f", "six", "1"),
	}
}

func TestEnumerateInVendorMatch(t *testing.T) {
	root := fakeSysfs(t, fixtureTree())

	refs, err := EnumerateIn(root, "/dev/bus/usb", 0x37BF)
	if err != nil {
		t.Fatalf("EnumerateIn: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2: %v", len(refs), refs)
	}

	// Sorted by bus then address.
	want := []DeviceRef{
		{Path: "/dev/bus/usb/001/007", Bus: 1, Addr: 7, VendorID: 0x37BF, ProductID: 0x800F, SysPath: filepath.Join(root, "1-1")},
		{Path: "/dev/bus/usb/002/012", Bus: 2, Addr: 12, VendorID: 0x37BF, ProductID: 0x800F, SysPath: filepath.Join(root, "2-1.4")},
	}
	for i, w := range want {
		if refs[i] != w {
			t.Errorf("refs[%d] = %+v, want %+v", i, refs[i], w)
		}
	}
}

func TestEnumerateInMatchAll(t *testing.T) {
	root := fakeSysfs(t, fixtureTree())

	refs, err := EnumerateIn(root, "/dev/bus/usb", 0)
	if err != nil {
		t.Fatalf("EnumerateIn: %v", err)
	}
	// Everything with a full, parseable attribute set: usb1, 1-1, 2-1.4, 3-2.
	// The interface directory, the one missing devnum and the two with
	// unparseable values are all excluded.
	wantPaths := []string{
		"/dev/bus/usb/001/001",
		"/dev/bus/usb/001/007",
		"/dev/bus/usb/002/012",
		"/dev/bus/usb/003/004",
	}
	if len(refs) != len(wantPaths) {
		t.Fatalf("got %d refs, want %d: %v", len(refs), len(wantPaths), refs)
	}
	for i, p := range wantPaths {
		if refs[i].Path != p {
			t.Errorf("refs[%d].Path = %q, want %q", i, refs[i].Path, p)
		}
	}
}

func TestEnumerateInNoMatch(t *testing.T) {
	root := fakeSysfs(t, fixtureTree())
	refs, err := EnumerateIn(root, "/dev/bus/usb", 0xDEAD)
	if err != nil {
		t.Fatalf("EnumerateIn: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("got %d refs, want none: %v", len(refs), refs)
	}
}

func TestEnumerateInMissingRoot(t *testing.T) {
	if _, err := EnumerateIn(filepath.Join(t.TempDir(), "absent"), "/dev/bus/usb", 0x37BF); err == nil {
		t.Fatal("expected an error for a missing sysfs root")
	}
}

// TestEnumerateRealSysfs runs the walk against the machine's own sysfs. It
// opens nothing and needs no privileges -- sysfs attributes are world-readable
// -- but it catches a fixture that has drifted from the real layout. Skipped
// where sysfs is not mounted (containers, non-Linux CI).
func TestEnumerateRealSysfs(t *testing.T) {
	if _, err := os.Stat(DefaultSysfsRoot); err != nil {
		t.Skipf("no %s on this machine: %v", DefaultSysfsRoot, err)
	}
	refs, err := Enumerate(0)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(refs) == 0 {
		t.Skip("no USB devices present")
	}
	for _, r := range refs {
		if r.Bus <= 0 || r.Addr <= 0 {
			t.Errorf("implausible bus/addr in %+v", r)
		}
		if !strings.HasPrefix(r.Path, DefaultDevRoot+"/") {
			t.Errorf("bad node path in %+v", r)
		}
		if fi, err := os.Stat(r.Path); err != nil {
			t.Errorf("%s: no usbfs node for an enumerated device: %v", r.Path, err)
		} else if fi.Mode()&os.ModeCharDevice == 0 {
			t.Errorf("%s is not a character device", r.Path)
		}
	}
	t.Logf("enumerated %d USB devices, first: %s", len(refs), refs[0])
}

func TestDeviceRefString(t *testing.T) {
	r := DeviceRef{Path: "/dev/bus/usb/001/007", Bus: 1, Addr: 7, VendorID: 0x37BF, ProductID: 0x800F}
	const want = "/dev/bus/usb/001/007 (bus 1 addr 7, 37bf:800f)"
	if got := r.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
