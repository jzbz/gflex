package usbfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	d := &Device{ref: DeviceRef{Path: "/dev/bus/usb/001/007"}, claimed: map[int]claimState{}}
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

// deviceWithSysfs is device() plus the sysfs tree the rebind check reads,
// because that check asks the filesystem rather than the kernel and a scripted
// ioctl alone cannot answer it. bound maps an interface number to whether a
// driver is bound to it; an interface left out of the map gets no directory at
// all, which is how a device that has left the bus looks.
//
// The layout mirrors the real one -- a device directory plus one
// <device>:<bConfigurationValue>.<interface> directory per interface, with a
// "driver" symlink where a driver is bound. enumerate_test.go's fakeSysfs builds
// attribute files rather than symlinks and interfaces rather than devices, so
// this is a sibling of it, not a use of it. The configuration number here is 1
// and is deliberately not what the code under test looks for; it globs.
func (s *scriptedIoctl) deviceWithSysfs(t *testing.T, bound map[int]bool) *Device {
	t.Helper()
	root := t.TempDir()
	sys := filepath.Join(root, "1-1")
	mkdirFixture(t, sys)
	mkdirFixture(t, filepath.Join(root, "snd-usb-audio"))
	for num, isBound := range bound {
		mkdirFixture(t, interfaceDir(sys, num))
		if isBound {
			bindFixtureDriver(t, sys, num)
		}
	}
	d := s.device()
	d.ref.SysPath = sys
	return d
}

func interfaceDir(sysPath string, num int) string {
	return fmt.Sprintf("%s:1.%d", sysPath, num)
}

func mkdirFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// bindFixtureDriver and unbindFixtureDriver stand in for the kernel binding and
// unbinding a driver, so a scripted ioctl can move sysfs the way the real one
// would -- or, for the case this all exists for, fail to.
func bindFixtureDriver(t *testing.T, sysPath string, num int) {
	t.Helper()
	link := filepath.Join(interfaceDir(sysPath, num), "driver")
	if err := os.Symlink(filepath.Join(filepath.Dir(sysPath), "snd-usb-audio"), link); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
}

func unbindFixtureDriver(t *testing.T, sysPath string, num int) {
	t.Helper()
	if err := os.Remove(filepath.Join(interfaceDir(sysPath, num), "driver")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
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
	if !d.claimed[1].detached {
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
	d.claimed[1] = claimState{detached: true} // claimed with a kernel driver detached

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
	d.claimed[2] = claimState{}

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
	d.claimed[1] = claimState{detached: true}

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
	d.claimed[0] = claimState{detached: true}
	d.claimed[1] = claimState{}

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

// ---------------------------------------------------------------------------
// Verifying the rebind.
//
// USBDEVFS_CONNECT answering success does not mean a driver bound; on the kernel
// a VFLEX was driven on (2026-08-21, serial 58b4f621) it returned success and
// left interface 1.1 with no driver, so /dev/snd/midiC2D* stayed gone until a
// replug. The fake kernel below is scripted to behave that way: it drops the
// driver symlink when the interface is claimed and, unless a test says
// otherwise, does not put it back on USBDEVFS_CONNECT.
// ---------------------------------------------------------------------------

// unbindingKernel scripts sysfs to follow the ioctls: the claim unbinds, and
// rebindOnConnect decides whether USBDEVFS_CONNECT binds anything back.
func unbindingKernel(t *testing.T, sysPath *string, rebindOnConnect bool) func(ioctlCall) error {
	return func(c ioctlCall) error {
		switch {
		case c.req == ioctlDisconnectClaim, c.req == ioctlIoctl && c.inner == ioctlDisconnect:
			unbindFixtureDriver(t, *sysPath, c.iface)
		case c.req == ioctlIoctl && c.inner == ioctlConnect:
			if rebindOnConnect {
				bindFixtureDriver(t, *sysPath, c.iface)
			}
		}
		return nil
	}
}

// The finding itself: every ioctl succeeds, and the interface is still
// driverless afterwards. Reporting that is the only way the user learns that
// their ALSA MIDI port is not coming back on its own.
func TestReleaseInterfaceReportsADriverThatDidNotRebind(t *testing.T) {
	var sys string
	s := &scriptedIoctl{}
	s.respond = unbindingKernel(t, &sys, false)
	d := s.deviceWithSysfs(t, map[int]bool{1: true})
	sys = d.ref.SysPath

	if err := d.ClaimInterface(1, true); err != nil {
		t.Fatalf("ClaimInterface: %v", err)
	}
	err := d.ReleaseInterface(1)
	if !errors.Is(err, ErrDriverNotRebound) {
		t.Fatalf("ReleaseInterface err = %v, want one wrapping ErrDriverNotRebound", err)
	}
	if !s.saw(ioctlIoctl, ioctlConnect, 1) {
		t.Error("the verdict was reached without ever asking the kernel to reattach")
	}
	if !strings.Contains(err.Error(), "replug") {
		t.Errorf("error %q does not tell the user to replug, which is the only remedy", err)
	}
}

// The control that keeps the check from being a permanent complaint: a kernel
// that does rebind must produce no error at all. It also proves the check reads
// sysfs *after* the reattach, since the driver is unbound for the whole span
// between the claim and the connect.
func TestReleaseInterfaceAcceptsADriverThatRebound(t *testing.T) {
	var sys string
	s := &scriptedIoctl{}
	s.respond = unbindingKernel(t, &sys, true)
	d := s.deviceWithSysfs(t, map[int]bool{1: true})
	sys = d.ref.SysPath

	if err := d.ClaimInterface(1, true); err != nil {
		t.Fatalf("ClaimInterface: %v", err)
	}
	if err := d.ReleaseInterface(1); err != nil {
		t.Fatalf("ReleaseInterface = %v, want nil: the driver is bound again", err)
	}
}

// The false alarm this must not raise. The bootloader claims a vendor-class
// interface that no driver was ever bound to, with detach requested all the
// same, and it is expected to stay unbound after the release -- so the firmware
// path must come through silent. Nothing distinguishes this from the case above
// except what sysfs said before the claim, which is why that is read then.
func TestReleaseInterfaceSaysNothingAboutAnInterfaceThatNeverHadADriver(t *testing.T) {
	var sys string
	s := &scriptedIoctl{}
	s.respond = unbindingKernel(t, &sys, false)
	d := s.deviceWithSysfs(t, map[int]bool{2: false})
	sys = d.ref.SysPath

	if err := d.ClaimInterface(2, true); err != nil {
		t.Fatalf("ClaimInterface: %v", err)
	}
	if err := d.ReleaseInterface(2); err != nil {
		t.Fatalf("ReleaseInterface = %v, want nil: this interface never had a driver to lose", err)
	}
	if !s.saw(ioctlIoctl, ioctlConnect, 2) {
		t.Error("the reattach was skipped; the check may gate the ioctl rather than its verdict")
	}
}

// The pre-3.4 fallback detaches and claims in two steps, and has to carry the
// same knowledge through to the release -- otherwise the finding would go
// unreported on exactly the kernels least likely to rebind cleanly.
func TestReleaseInterfaceReportsAnUnreboundDriverAfterTheFallbackClaim(t *testing.T) {
	var sys string
	s := &scriptedIoctl{}
	kernel := unbindingKernel(t, &sys, false)
	s.respond = func(c ioctlCall) error {
		if c.req == ioctlDisconnectClaim {
			return unix.ENOTTY // a kernel older than 3.4: forces the fallback
		}
		return kernel(c)
	}
	d := s.deviceWithSysfs(t, map[int]bool{1: true})
	sys = d.ref.SysPath

	if err := d.ClaimInterface(1, true); err != nil {
		t.Fatalf("ClaimInterface: %v", err)
	}
	if !s.saw(ioctlIoctl, ioctlDisconnect, 1) {
		t.Fatal("the fallback was not taken, so this proves nothing about it")
	}
	if err := d.ReleaseInterface(1); !errors.Is(err, ErrDriverNotRebound) {
		t.Fatalf("ReleaseInterface err = %v, want one wrapping ErrDriverNotRebound", err)
	}
}

// Not being able to tell is not a failure. A Device built from a hand-made
// DeviceRef has no SysPath, and a device that has left the bus takes its sysfs
// directory with it; neither is a driver that refused to bind, and reporting
// either as one would put a replug instruction in front of users whose MIDI port
// is fine.
func TestReleaseInterfaceStaysSilentWhenSysfsCannotAnswer(t *testing.T) {
	tests := []struct {
		name    string
		sysPath func(t *testing.T, d *Device)
	}{
		{
			name:    "no sysfs path at all",
			sysPath: func(*testing.T, *Device) {},
		},
		{
			name: "the device left the bus",
			sysPath: func(t *testing.T, d *Device) {
				// A device directory with no interface directory under it: the
				// glob matches nothing, which is a different answer from
				// matching a directory that holds no driver symlink.
				root := t.TempDir()
				d.ref.SysPath = filepath.Join(root, "1-1")
				mkdirFixture(t, d.ref.SysPath)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &scriptedIoctl{respond: func(ioctlCall) error { return nil }}
			d := s.device()
			tc.sysPath(t, d)
			// hadDriver set by hand: even told that a driver was there, an
			// unanswerable sysfs must not become a verdict.
			d.claimed[1] = claimState{detached: true, hadDriver: true}

			if err := d.ReleaseInterface(1); err != nil {
				t.Fatalf("ReleaseInterface = %v, want nil when the binding state cannot be read", err)
			}
			if !s.saw(ioctlIoctl, ioctlConnect, 1) {
				t.Error("the reattach itself was skipped")
			}
		})
	}
}

// The interface directory is found by globbing, because the configuration
// number in its name is whatever the device has selected and is not always
// readable. A device sitting in configuration 2 must be read as accurately as
// one in configuration 1.
func TestInterfaceDriverBoundGlobsTheConfigurationNumber(t *testing.T) {
	root := t.TempDir()
	sys := filepath.Join(root, "1-1")
	mkdirFixture(t, filepath.Join(root, "snd-usb-audio"))
	mkdirFixture(t, sys)
	// Configuration 2, interface 1, driver bound.
	dir := fmt.Sprintf("%s:2.1", sys)
	mkdirFixture(t, dir)
	if err := os.Symlink(filepath.Join(root, "snd-usb-audio"), filepath.Join(dir, "driver")); err != nil {
		t.Fatal(err)
	}
	// Interface 10 of the same configuration, unbound: it must not be mistaken
	// for interface 1 by a glob that matched loosely.
	mkdirFixture(t, fmt.Sprintf("%s:2.10", sys))

	d := &Device{ref: DeviceRef{Path: "/dev/bus/usb/001/007", SysPath: sys}, claimed: map[int]claimState{}}
	if bound, known := d.interfaceDriverBound(1); !known || !bound {
		t.Errorf("interfaceDriverBound(1) = %v, %v; want bound and known", bound, known)
	}
	if bound, known := d.interfaceDriverBound(10); !known || bound {
		t.Errorf("interfaceDriverBound(10) = %v, %v; want unbound and known", bound, known)
	}
	if bound, known := d.interfaceDriverBound(3); known || bound {
		t.Errorf("interfaceDriverBound(3) = %v, %v; want cannot-tell for an interface with no directory", bound, known)
	}
}
