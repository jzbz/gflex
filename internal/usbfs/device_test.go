package usbfs

import (
	"errors"
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// A scripted ioctl layer.
//
// Everything below turns on what happens when a management ioctl *fails*, and a
// real device cannot be asked to fail a CLAIMINTERFACE or a RELEASEINTERFACE on
// demand -- the interesting errnos (ENOMEM, EINVAL, a kernel too old for
// USBDEVFS_DISCONNECT_CLAIM) are not reachable from a test that talks to
// hardware. Device.ioctlFn exists for exactly this.
// ---------------------------------------------------------------------------

// ioctlCall is one ioctl, decoded down to the fields the claim/release
// bookkeeping turns on.
type ioctlCall struct {
	// req is the outer request number, e.g. ioctlClaimInterface.
	req uintptr
	// inner is the nested request code when req is ioctlIoctl -- which is how
	// USBDEVFS_DISCONNECT and USBDEVFS_CONNECT are reached -- and 0 otherwise.
	inner uintptr
	// iface is the interface number the argument names, or -1 when the
	// argument does not name one.
	iface int
}

func decodeIoctl(req uintptr, arg unsafe.Pointer) ioctlCall {
	c := ioctlCall{req: req, iface: -1}
	switch req {
	case ioctlClaimInterface, ioctlReleaseInterface:
		c.iface = int(*(*uint32)(arg))
	case ioctlDisconnectClaim:
		c.iface = int((*disconnectClaim)(arg).iface)
	case ioctlIoctl:
		a := (*usbdevfsIoctl)(arg)
		c.iface, c.inner = int(a.ifno), uintptr(a.ioctlCode)
	}
	return c
}

// scriptedIoctl records every ioctl and answers each one from respond, which
// returns the errno to fail with or nil to succeed.
type scriptedIoctl struct {
	respond func(ioctlCall) error
	calls   []ioctlCall
}

func (s *scriptedIoctl) device() *Device {
	d := &Device{ref: DeviceRef{Path: "/dev/bus/usb/001/007"}, claimed: map[int]bool{}}
	d.ioctlFn = func(op string, req uintptr, arg unsafe.Pointer, _ bool) (int, error) {
		c := decodeIoctl(req, arg)
		s.calls = append(s.calls, c)
		if errno := s.respond(c); errno != nil {
			return 0, wrapErrno(op, d.ref.Path, errno)
		}
		return 0, nil
	}
	return d
}

// saw reports whether the recorded trace contains a matching ioctl.
func (s *scriptedIoctl) saw(req, inner uintptr, iface int) bool {
	for _, c := range s.calls {
		if c.req == req && c.inner == inner && c.iface == iface {
			return true
		}
	}
	return false
}

// On the pre-3.4 fallback the driver is unbound by the time the claim is
// attempted, so a claim that fails there leaves snd-usb-audio detached with
// nothing in d.claimed to tell Close or ReleaseInterface it is owed a rebind.
// The reattach has to happen in ClaimInterface itself.
func TestClaimInterfaceReattachesWhenTheFallbackClaimFails(t *testing.T) {
	s := &scriptedIoctl{respond: func(c ioctlCall) error {
		switch c.req {
		case ioctlDisconnectClaim:
			return unix.ENOTTY // a kernel older than 3.4: forces the fallback
		case ioctlClaimInterface:
			return unix.ENOMEM
		}
		return nil
	}}
	d := s.device()

	err := d.ClaimInterface(1, true)
	if !errors.Is(err, unix.ENOMEM) {
		t.Fatalf("ClaimInterface err = %v, want the ENOMEM the claim answered", err)
	}
	if !s.saw(ioctlIoctl, ioctlDisconnect, 1) {
		t.Fatal("the fallback never detached anything, so this proves nothing about reattaching")
	}
	if !s.saw(ioctlIoctl, ioctlConnect, 1) {
		t.Error("the driver was detached and the claim then failed, but USBDEVFS_CONNECT was never issued")
	}
	if len(d.claimed) != 0 {
		t.Errorf("an interface that was never claimed is recorded as claimed: %v", d.claimed)
	}
}

// The control for the test above: an interface claimed without detaching owes
// no reattach, so a failure there must issue no USBDEVFS_CONNECT. Without this,
// code that reattached unconditionally would pass.
func TestClaimInterfaceWithoutDetachNeverReattaches(t *testing.T) {
	s := &scriptedIoctl{respond: func(c ioctlCall) error {
		if c.req == ioctlClaimInterface {
			return unix.EBUSY
		}
		return nil
	}}
	d := s.device()

	if err := d.ClaimInterface(1, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("ClaimInterface err = %v, want ErrBusy", err)
	}
	if s.saw(ioctlIoctl, ioctlConnect, 1) {
		t.Error("USBDEVFS_CONNECT issued for an interface whose driver was never detached")
	}
	if s.saw(ioctlIoctl, ioctlDisconnect, 1) {
		t.Error("USBDEVFS_DISCONNECT issued without detachKernelDriver")
	}
}

// A claim that succeeds through the fallback is recorded as having detached,
// which is what makes the release reattach later.
func TestClaimInterfaceFallbackRecordsTheDetach(t *testing.T) {
	s := &scriptedIoctl{respond: func(c ioctlCall) error {
		if c.req == ioctlDisconnectClaim {
			return unix.ENOTTY
		}
		return nil
	}}
	d := s.device()

	if err := d.ClaimInterface(1, true); err != nil {
		t.Fatalf("ClaimInterface: %v", err)
	}
	if !d.claimed[1] {
		t.Errorf("the fallback claim did not record its detach: %v", d.claimed)
	}
}

// The release ioctl failing does not cancel the obligation: this process
// unbound the driver, so it still owes the rebind. Reading that fact and
// discarding it before the release had even been attempted was the defect.
func TestReleaseInterfaceReattachesAfterAFailedRelease(t *testing.T) {
	s := &scriptedIoctl{respond: func(c ioctlCall) error {
		if c.req == ioctlReleaseInterface {
			return unix.EINVAL
		}
		return nil
	}}
	d := s.device()
	d.claimed[1] = true // claimed with a kernel driver detached

	err := d.ReleaseInterface(1)
	if !errors.Is(err, unix.EINVAL) {
		t.Fatalf("ReleaseInterface err = %v, want the EINVAL the release answered", err)
	}
	if !s.saw(ioctlIoctl, ioctlConnect, 1) {
		t.Error("the release failed and the kernel driver was never reattached")
	}
	if len(d.claimed) != 0 {
		t.Errorf("the interface is still recorded as claimed: %v", d.claimed)
	}
}

// The control: an interface claimed without a detach must never be reattached,
// however the release goes.
func TestReleaseInterfaceWithoutDetachNeverReattaches(t *testing.T) {
	s := &scriptedIoctl{respond: func(ioctlCall) error { return nil }}
	d := s.device()
	d.claimed[2] = false

	if err := d.ReleaseInterface(2); err != nil {
		t.Fatalf("ReleaseInterface: %v", err)
	}
	if s.saw(ioctlIoctl, ioctlConnect, 2) {
		t.Error("USBDEVFS_CONNECT issued for an interface whose driver was never detached")
	}
}

// A device that has gone away released everything on its way out and has no
// driver left to rebind, so this is a success with nothing to do -- the case the
// bootloader jump depends on (SPEC.md §10.1).
func TestReleaseInterfaceOnAVanishedDevice(t *testing.T) {
	s := &scriptedIoctl{respond: func(c ioctlCall) error {
		if c.req == ioctlReleaseInterface {
			return unix.ENODEV
		}
		return nil
	}}
	d := s.device()
	d.claimed[1] = true

	if err := d.ReleaseInterface(1); err != nil {
		t.Fatalf("ReleaseInterface on a vanished device = %v, want nil", err)
	}
	if s.saw(ioctlIoctl, ioctlConnect, 1) {
		t.Error("USBDEVFS_CONNECT issued for a device that is no longer there")
	}
}

// Close is the cleanup path both callers rely on -- usbmidi through a deferred
// Close, the flasher explicitly -- so the reattach has to survive the trip
// through it, and only for the interfaces that are actually owed one.
func TestCloseReattachesEveryDetachedInterface(t *testing.T) {
	s := &scriptedIoctl{respond: func(ioctlCall) error { return nil }}
	d := s.device()
	// Close closes the fd as well; /dev/null stands in for the usbfs node,
	// since no ioctl reaches it.
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	d.f = f
	d.claimed[0] = true
	d.claimed[1] = false

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !s.saw(ioctlIoctl, ioctlConnect, 0) {
		t.Error("interface 0 was claimed with a detach and never reattached")
	}
	if s.saw(ioctlIoctl, ioctlConnect, 1) {
		t.Error("interface 1 was claimed without a detach and was reattached anyway")
	}
}
