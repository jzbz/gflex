package usbfs

import (
	"context"
	"fmt"
	"runtime"
	"time"
	"unsafe"
)

// Standard requests and timing for the configuration read.
const (
	// reqTypeStandardDeviceIn is bmRequestType for a device-to-host, standard
	// request addressed to the device itself. The recipient matters: usbfs lets
	// a device-recipient control transfer through without an interface claim,
	// whereas an interface-recipient one would be refused until we hold it.
	reqTypeStandardDeviceIn uint8 = 0x80
	// reqGetConfiguration is the standard GET_CONFIGURATION request
	// (USB 2.0 §9.4.2). It answers with a single byte: the currently selected
	// bConfigurationValue, or 0 when the device is not configured.
	reqGetConfiguration uint8 = 0x08
	// configurationTimeout bounds the GET_CONFIGURATION fallback. It is a
	// one-byte answer from an idle device; anything slower than this is a device
	// that is not going to answer.
	configurationTimeout = 1 * time.Second
)

// Configuration reports the bConfigurationValue the device currently has
// selected. 0 means the device is attached but not configured, which on Linux
// normally only happens when the kernel could not choose a configuration at
// enumeration.
//
// sysfs is asked first and the device only as a fallback, for three reasons:
// the sysfs value is what the *kernel* believes, which is what actually governs
// whether an interface claim can succeed; it costs no bus traffic on a path that
// runs immediately before a firmware write; and GET_CONFIGURATION is exactly the
// kind of standard request a minimal bootloader is apt to stall (libusb's Linux
// backend prefers sysfs for the same reason). The fallback exists for a Device
// opened from a hand-built DeviceRef with no SysPath.
//
// An error wrapping ErrConfigUnknown means neither source answered. That is
// specifically not the same as "unconfigured": callers must not act on it by
// selecting a configuration, because setting one that is already correct resets
// device state (see SetConfiguration).
func (d *Device) Configuration(ctx context.Context) (uint8, error) {
	if v, ok := d.sysfsConfiguration(); ok {
		return v, nil
	}
	buf := make([]byte, 1)
	n, err := d.Control(ctx, reqTypeStandardDeviceIn, reqGetConfiguration, 0, 0, buf, configurationTimeout)
	if err != nil {
		return 0, fmt.Errorf("%w on %s: sysfs did not answer and GET_CONFIGURATION failed: %w",
			ErrConfigUnknown, d.ref.Path, err)
	}
	if n < 1 {
		return 0, fmt.Errorf("%w on %s: GET_CONFIGURATION returned %d bytes, want 1",
			ErrConfigUnknown, d.ref.Path, n)
	}
	return buf[0], nil
}

// sysfsConfiguration reads bConfigurationValue from the device's sysfs
// directory. The attribute is world-readable and exists for every USB device,
// so the only reasons this fails are a DeviceRef built without a SysPath and a
// device that was unplugged between enumeration and now.
func (d *Device) sysfsConfiguration() (uint8, bool) {
	if d.ref.SysPath == "" {
		return 0, false
	}
	v, ok := readIntAttr(d.ref.SysPath, "bConfigurationValue")
	if !ok || v < 0 || v > 255 {
		return 0, false
	}
	return uint8(v), true
}

// SetConfiguration selects a configuration by its bConfigurationValue
// (USBDEVFS_SETCONFIGURATION).
//
// ⚠ Read Configuration first and call this only when the device is
// unconfigured, or use EnsureConfigured, which does exactly that. Selecting the
// configuration that is already in force is not a no-op: usbfs turns it into
// usb_reset_configuration() (drivers/usb/core/devio.c, proc_setconfig), which
// resets every endpoint's data toggle and clears halt state across the whole
// device. On a device mid-protocol that is a silent state reset, and this
// package's one caller runs immediately before a firmware write.
//
// The kernel also refuses with EBUSY while any interface of the current
// configuration is claimed -- by us or by anyone -- so this has to happen before
// ClaimInterface, not after.
//
// The cached descriptors are dropped on success: which interfaces exist, and
// which one Config.Interfaces narrows to, both depend on the configuration that
// was just changed.
func (d *Device) SetConfiguration(value uint8) error {
	v := uint32(value)
	_, err := d.ioctl(fmt.Sprintf("set configuration %d", value), ioctlSetConfiguration, unsafe.Pointer(&v), true)
	runtime.KeepAlive(&v)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.cfg = nil
	d.mu.Unlock()
	return nil
}

// EnsureConfigured selects the device's first configuration if none is
// selected, and reports the bConfigurationValue that ends up active.
//
// This is SPEC.md §10.1 phase 2's "select configuration 1 if unset", with the
// emphasis on *if unset*: the read comes first and nothing is written unless the
// device reads back as unconfigured. On Linux the kernel configures a device at
// enumeration, so the overwhelmingly common outcome is that this reads 1 and
// does nothing at all.
//
// Errors:
//
//   - a configuration that could not be determined wraps ErrConfigUnknown and
//     means nothing was written. A caller that can proceed without knowing --
//     which is anything that worked before this existed -- should treat it as
//     non-fatal rather than deconfiguring a device on a guess.
//   - any other error means the device was definitely unconfigured and could not
//     be configured, so nothing downstream can claim an interface either.
func (d *Device) EnsureConfigured(ctx context.Context) (uint8, error) {
	cur, err := d.Configuration(ctx)
	if err != nil {
		return 0, err
	}
	if cur != 0 {
		return cur, nil
	}
	cfg, err := d.Descriptors()
	if err != nil {
		return 0, err
	}
	want := cfg.FirstConfigurationValue()
	if err := d.SetConfiguration(want); err != nil {
		return 0, fmt.Errorf("usbfs: %s reports no configuration selected and configuration %d "+
			"could not be selected: %w", d.ref.Path, want, err)
	}
	return want, nil
}
