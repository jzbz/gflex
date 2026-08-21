package usbfs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Default filesystem roots. Both are injectable through EnumerateIn so the
// enumeration can be tested against a fixture tree.
const (
	// DefaultSysfsRoot is where the kernel lists USB devices and interfaces.
	DefaultSysfsRoot = "/sys/bus/usb/devices"
	// DefaultDevRoot is the usbfs mount point; nodes live at
	// <DefaultDevRoot>/<busnum:%03d>/<devnum:%03d>.
	DefaultDevRoot = "/dev/bus/usb"
)

// DeviceRef identifies one USB device found by enumeration.
type DeviceRef struct {
	// Path is the usbfs character device, e.g. /dev/bus/usb/001/007.
	Path string
	// Bus is busnum and Addr is devnum, the two numbers Path is built from.
	Bus, Addr int
	// VendorID and ProductID are sysfs's idVendor/idProduct.
	VendorID, ProductID uint16
	// SysPath is the sysfs directory for the device, e.g.
	// /sys/bus/usb/devices/1-1. It is the anchor for finding sibling
	// attributes -- notably the ALSA node at <SysPath>/*/sound/card*/midiC*D*
	// that the rawmidi transport globs for (SPEC.md §3.4).
	SysPath string
}

// String renders the reference for diagnostics.
func (r DeviceRef) String() string {
	return fmt.Sprintf("%s (bus %d addr %d, %04x:%04x)", r.Path, r.Bus, r.Addr, r.VendorID, r.ProductID)
}

// Enumerate lists every USB device whose idVendor matches vendorID. A vendorID
// of 0 matches every device.
//
// Matching on the vendor ID is the authoritative way to find a VFLEX: the
// vendor app matches MIDI ports by the substring "vflex" in the port name,
// which is an ALSA property a USB device enumeration cannot see at all, whereas
// 0x37BF appears in the app's own WebUSB filter (SPEC.md §1, §3.4). The product
// ID is deliberately not matched: it is 0x800F in application mode
// (SPEC.md §14.1), but what the unit enumerates as in bootloader mode is still
// unknown (SPEC.md §14.16), and a PID filter would hide exactly the device that
// firmware recovery has to find.
func Enumerate(vendorID uint16) ([]DeviceRef, error) {
	return EnumerateIn(DefaultSysfsRoot, DefaultDevRoot, vendorID)
}

// EnumerateIn is Enumerate against explicit sysfs and usbfs roots.
func EnumerateIn(sysfsRoot, devRoot string, vendorID uint16) ([]DeviceRef, error) {
	entries, err := os.ReadDir(sysfsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("usbfs: %s does not exist; is sysfs mounted and is this a Linux system with USB support?: %w", sysfsRoot, err)
		}
		return nil, fmt.Errorf("usbfs: reading %s: %w", sysfsRoot, err)
	}

	var refs []DeviceRef
	for _, e := range entries {
		name := e.Name()
		// /sys/bus/usb/devices holds both devices ("1-1", "usb1") and
		// interfaces ("1-1:1.0"). Only devices carry idVendor, and the colon
		// is what distinguishes them. Note the entries are symlinks, so
		// e.IsDir() is false for all of them and cannot be used as a filter.
		if strings.Contains(name, ":") {
			continue
		}
		dir := filepath.Join(sysfsRoot, name)

		vid, ok := readHexAttr(dir, "idVendor")
		if !ok {
			continue
		}
		if vendorID != 0 && vid != vendorID {
			continue
		}
		pid, _ := readHexAttr(dir, "idProduct")
		bus, ok1 := readIntAttr(dir, "busnum")
		addr, ok2 := readIntAttr(dir, "devnum")
		if !ok1 || !ok2 {
			// Without both numbers there is no usbfs node to address.
			continue
		}

		refs = append(refs, DeviceRef{
			Path:      fmt.Sprintf("%s/%03d/%03d", devRoot, bus, addr),
			Bus:       bus,
			Addr:      addr,
			VendorID:  vid,
			ProductID: pid,
			SysPath:   dir,
		})
	}

	// Directory order from sysfs is not defined; sort so that "the first
	// device" means the same thing on every run.
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Bus != refs[j].Bus {
			return refs[i].Bus < refs[j].Bus
		}
		return refs[i].Addr < refs[j].Addr
	})
	return refs, nil
}

// readHexAttr reads a sysfs attribute holding a 16-bit hex value with no 0x
// prefix, which is how idVendor and idProduct are rendered.
func readHexAttr(dir, name string) (uint16, bool) {
	s, ok := readAttr(dir, name)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(v), true
}

// readIntAttr reads a sysfs attribute holding a decimal integer.
func readIntAttr(dir, name string) (int, bool) {
	s, ok := readAttr(dir, name)
	if !ok {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return v, true
}

func readAttr(dir, name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}
