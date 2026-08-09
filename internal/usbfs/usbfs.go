// Package usbfs is a minimal, pure-Go binding to the Linux usbfs interface:
// the character devices under /dev/bus/usb/BBB/DDD driven by the ioctls in
// include/uapi/linux/usbdevice_fs.h.
//
// It exists because the VFLEX firmware update path (SPEC.md §10) speaks a
// vendor-class (0xFF) bulk protocol with no MIDI framing at all, so ALSA
// rawmidi cannot reach it. The obvious alternatives -- gousb and
// gotmc/libusb -- both require cgo and libusb-1.0, which forfeits the static
// CGO_ENABLED=0 binary this project ships (SPEC.md §4.3). Everything here is
// therefore syscalls and struct layout, with no C.
//
// Scope is deliberately narrow: enumerate by vendor ID, open, parse
// descriptors, claim/release an interface, and run synchronous control and
// bulk/interrupt transfers. There is no asynchronous URB submission, no
// isochronous support and no hotplug notification.
//
// Permissions: /dev/bus/usb nodes are root-only on a stock system. SPEC.md §4.4
// explains why a udev rule is required for this package (unlike the rawmidi
// path, which systemd already grants via 70-uaccess.rules) and ships the rule.
// Errors carrying ErrPermission repeat that hint.
package usbfs

import (
	"errors"
	"fmt"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Error classes callers branch on. Every error this package returns for a
// failed syscall matches exactly one of these under errors.Is, and also matches
// the underlying syscall.Errno, so both
//
//	errors.Is(err, usbfs.ErrNoDevice)
//	errors.Is(err, unix.ENODEV)
//
// work.
var (
	// ErrPermission is EACCES or EPERM: the process may not open or drive the
	// usbfs node. Almost always a missing udev rule.
	ErrPermission = errors.New("usbfs: permission denied")
	// ErrNoDevice is ENODEV, ENXIO, ENOENT or ESHUTDOWN: the device is gone.
	// This is an expected, non-fatal outcome immediately after
	// CMD_JUMP_APP_TO_BOOTLOADER, which makes the unit re-enumerate
	// (SPEC.md §10.1) -- the bootloader flow must treat it as success, not
	// failure.
	ErrNoDevice = errors.New("usbfs: device not present")
	// ErrBusy is EBUSY: the interface is already claimed, either by a kernel
	// driver (snd-usb-audio in application mode) or by another usbfs process.
	ErrBusy = errors.New("usbfs: device or interface busy")
	// ErrTimeout is ETIMEDOUT: the transfer did not complete in time.
	ErrTimeout = errors.New("usbfs: transfer timed out")
	// ErrStall is EPIPE: the endpoint stalled. For a control transfer this
	// usually means the device rejected the request.
	ErrStall = errors.New("usbfs: endpoint stalled")
	// ErrNotSupported is ENOTTY or ENOSYS: this kernel does not implement the
	// ioctl. Only reachable on kernels older than 3.4, which lack
	// USBDEVFS_DISCONNECT_CLAIM.
	ErrNotSupported = errors.New("usbfs: ioctl not supported by this kernel")
	// ErrTooLarge is returned before the syscall when a transfer exceeds the
	// kernel's per-ioctl buffer limit; the kernel would answer EINVAL, which is
	// far less informative.
	ErrTooLarge = errors.New("usbfs: transfer larger than the usbfs buffer limit")
	// ErrConfigUnknown means the active USB configuration could not be
	// determined -- neither sysfs nor the device answered. It is deliberately
	// distinct from "the device is unconfigured": a caller may select a
	// configuration in response to the latter but must never do so in response
	// to this, because selecting one that is already correct resets device state
	// (Device.SetConfiguration).
	ErrConfigUnknown = errors.New("usbfs: active USB configuration could not be determined")
)

// Error is a failed usbfs operation. It unwraps to both an error class above
// and the raw syscall.Errno.
type Error struct {
	// Op names the operation, e.g. "claim interface 1".
	Op string
	// Path is the device node the operation was attempted on, if known.
	Path string
	// Errno is the raw kernel error.
	Errno syscall.Errno
	// Class is one of the sentinels above, or nil for an unclassified errno.
	Class error
	// Hint is human-facing remediation advice, or empty.
	Hint string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("usbfs: ")
	b.WriteString(e.Op)
	if e.Path != "" {
		b.WriteString(" on ")
		b.WriteString(e.Path)
	}
	b.WriteString(": ")
	b.WriteString(e.Errno.Error())
	if e.Hint != "" {
		b.WriteString(" (")
		b.WriteString(e.Hint)
		b.WriteString(")")
	}
	return b.String()
}

// Unwrap exposes both the class sentinel and the errno, so errors.Is matches
// either one. Multi-error Unwrap requires Go 1.20 or newer.
func (e *Error) Unwrap() []error {
	if e.Class == nil {
		return []error{e.Errno}
	}
	return []error{e.Class, e.Errno}
}

// classify maps an errno onto an exported sentinel plus a remediation hint.
// The hints matter: the two failure modes users actually hit are a missing udev
// rule and an interface held by snd-usb-audio, and neither "permission denied"
// nor "device or resource busy" tells them what to do about it.
func classify(errno syscall.Errno) (hint string, err error) {
	switch errno {
	case unix.EACCES, unix.EPERM:
		return `install the udev rule (SUBSYSTEM=="usb", ATTR{idVendor}=="37bf", MODE="0660", TAG+="uaccess") and replug the device, or run as root`, ErrPermission
	case unix.ENODEV, unix.ENXIO, unix.ESHUTDOWN, unix.ENOENT:
		return "the device disconnected or re-enumerated; this is expected right after a jump to the bootloader", ErrNoDevice
	case unix.EBUSY:
		return "a kernel driver or another process holds this interface; claiming with detach, or stopping PipeWire/JACK, may help", ErrBusy
	case unix.ETIMEDOUT:
		return "the device did not answer within the timeout", ErrTimeout
	case unix.EPIPE:
		return "the device stalled the endpoint, i.e. rejected the request", ErrStall
	case unix.ENOTTY, unix.ENOSYS:
		return "kernel too old for this usbfs ioctl", ErrNotSupported
	case unix.EOVERFLOW:
		return "the device sent more data than the supplied buffer holds", nil
	}
	return "", nil
}

// wrapErrno builds an *Error from any error that carries a syscall.Errno
// (including *os.PathError and *os.SyscallError). Errors without one are
// wrapped plainly so nothing is swallowed.
func wrapErrno(op, path string, err error) error {
	if err == nil {
		return nil
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		if path != "" {
			return fmt.Errorf("usbfs: %s on %s: %w", op, path, err)
		}
		return fmt.Errorf("usbfs: %s: %w", op, err)
	}
	hint, class := classify(errno)
	return &Error{Op: op, Path: path, Errno: errno, Class: class, Hint: hint}
}
