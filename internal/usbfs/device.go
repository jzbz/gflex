package usbfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ErrDriverNotRebound reports that a kernel driver this process detached is
// still not bound to the interface after USBDEVFS_CONNECT returned success.
//
// The two are not the same thing, and that is the whole point of this sentinel.
// USBDEVFS_CONNECT means "the re-probe request was accepted" -- the kernel calls
// device_attach() on the interface and reports whether the *call* worked, not
// whether any driver claimed it. A driver that declines, or that cannot bind an
// interface in isolation, leaves the interface unbound behind a successful
// ioctl.
//
// That is exactly what a VFLEX does. Driving a second unit (serial 58b4f621) on
// 2026-08-21, `gflex --transport usb info` left /dev/snd/midiC2D* gone and it
// never came back: sysfs showed interface 1.1 (MIDIStreaming) with no driver
// bound while 1.0 stayed bound to snd-usb-audio, and a standalone probe issued
// USBDEVFS_CONNECT on 1.1 and got success while the interface stayed unbound.
// snd-usb-audio appears to bind the MIDI interface only while probing the whole
// audio function, so re-probing 1.1 alone does not recreate the card's rawmidi
// node. Only a physical replug restored it.
//
// Nothing in this package can undo that, so an error carrying this sentinel is a
// report rather than a failure to retry: the remedy to offer the user is to
// replug the device.
var ErrDriverNotRebound = errors.New("usbfs: the detached kernel driver did not rebind; replug the device")

// Transfer sizing and timing limits.
const (
	// MaxBulkTransferSize is the kernel's MAX_USBFS_BUFFER_SIZE. A larger
	// USBDEVFS_BULK is rejected with EINVAL, so it is caught here instead
	// where the message can say why.
	MaxBulkTransferSize = 16384
	// MaxControlTransferSize is the kernel's control-transfer limit: usbfs
	// bounces wLength above one page. One page is at least 4 KiB on every
	// architecture, so this is the portable floor.
	MaxControlTransferSize = 4096
	// DefaultTimeout is substituted when a caller passes a zero timeout and
	// the context carries no deadline. usbfs reads a zero timeout as "wait
	// forever", which would wedge the CLI on an unresponsive device; nothing
	// in this package ever passes the kernel a literal zero.
	DefaultTimeout = 5 * time.Second

	// maxTimeoutMs caps the millisecond field so a very distant context
	// deadline cannot overflow it.
	maxTimeoutMs = 1<<31 - 1

	// maxDescriptorBytes bounds the descriptor read. Real blobs are a few
	// hundred bytes; this only stops a runaway loop.
	maxDescriptorBytes = 64 * 1024

	// driverRebindSettle bounds how long attachDriver keeps looking for a driver
	// on an interface it has just asked the kernel to re-probe, and
	// driverRebindPoll is how often it looks. USB drivers are probed
	// synchronously from device_attach(), so on the kernel ErrDriverNotRebound
	// was observed on the answer is already final when the ioctl returns; the
	// wait is insurance against a kernel that probes asynchronously, where
	// reading the gap between the ioctl and the bind as a failure would tell a
	// user to replug a device that was about to come back on its own. It is only
	// ever paid on the path that is about to report a problem.
	driverRebindSettle = 50 * time.Millisecond
	driverRebindPoll   = 10 * time.Millisecond
)

// Device is an open usbfs handle.
//
// A Device is safe for concurrent use. Transfers do not serialise against each
// other -- the kernel handles concurrent URBs on distinct endpoints -- but the
// bookkeeping of claimed interfaces does.
type Device struct {
	f   *os.File
	ref DeviceRef

	// ioctlFn, when non-nil, stands in for the syscall. Nothing in production
	// ever sets it: it exists so the claim/release bookkeeping -- which decides
	// whether a detached kernel driver ever gets rebound, and whose failure
	// modes all live on error paths a real device will not produce on demand --
	// can be driven against scripted errnos. It is set at construction and
	// never written afterwards, so it needs no lock.
	ioctlFn func(op string, req uintptr, arg unsafe.Pointer, retryEINTR bool) (int, error)

	mu sync.Mutex
	// claimed maps an interface number to what claiming it took away, which is
	// what tells ReleaseInterface what it owes the system back.
	claimed map[int]claimState
	cfg     *Config
}

// claimState is what one claimed interface owes when it is released.
type claimState struct {
	// detached records that the claim asked the kernel to unbind whatever driver
	// held the interface, which is what makes the release owe a USBDEVFS_CONNECT.
	detached bool
	// hadDriver records that sysfs showed a driver actually bound to the
	// interface immediately before the claim.
	//
	// It is deliberately not the same fact as detached. A claim with detach
	// requested succeeds whether or not anything was bound -- the bootloader's
	// vendor-class interface has no driver at all and is claimed the same way --
	// and once the claim has happened there is no way to tell the two apart. Yet
	// that difference is precisely what decides whether an interface left
	// unbound after the reattach is a regression worth reporting
	// (ErrDriverNotRebound, snd-usb-audio in application mode) or the expected
	// resting state that reporting would turn into a false alarm on the firmware
	// path. So it is read before anything is detached, and carried here.
	hadDriver bool
}

// Open opens the usbfs node named by ref.
//
// The node is root-only on a stock system; a failure here is usually
// ErrPermission and means the udev rule is missing (SPEC.md §4.4).
func Open(ref DeviceRef) (*Device, error) {
	if ref.Path == "" {
		return nil, errors.New("usbfs: DeviceRef has no device node path")
	}
	f, err := os.OpenFile(ref.Path, os.O_RDWR, 0)
	if err != nil {
		return nil, wrapErrno("open", ref.Path, err)
	}
	return &Device{f: f, ref: ref, claimed: make(map[int]claimState)}, nil
}

// String renders the device for diagnostics.
func (d *Device) String() string { return d.ref.String() }

// Close releases any interfaces still claimed -- reattaching kernel drivers as
// it goes -- and then closes the file descriptor.
//
// The order matters. Closing the fd alone would make the kernel drop the usbfs
// claim, but it would not rebind the driver that was detached, so the ALSA card
// would stay missing. See ReleaseInterface.
func (d *Device) Close() error {
	d.mu.Lock()
	nums := make([]int, 0, len(d.claimed))
	for n := range d.claimed {
		nums = append(nums, n)
	}
	d.mu.Unlock()
	sort.Ints(nums)

	var firstErr error
	for _, n := range nums {
		if err := d.ReleaseInterface(n); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := d.f.Close(); err != nil && firstErr == nil {
		firstErr = wrapErrno("close", d.ref.Path, err)
	}
	return firstErr
}

// Descriptors reads and parses the device's descriptor blob.
//
// Reading a usbfs file from offset 0 yields the 18-byte device descriptor
// immediately followed by every configuration descriptor tree, which is a
// cheaper and more complete source than either sysfs or a GET_DESCRIPTOR
// control transfer. The result is cached and must be treated as read-only;
// SetConfiguration drops the cache, since it changes the answer.
//
// Because the blob flattens every configuration together, the returned
// Config.Interfaces is narrowed to the configuration the device currently has
// selected whenever the device declares more than one and that selection can be
// determined from sysfs. Without the narrowing, a selection by interface number
// could match an interface that only exists in some other configuration and then
// claim a different interface with the same number, or none at all. Config.
// Configurations always holds the full picture. The active value is read from
// sysfs alone, never from the device, so this stays free of bus traffic and of a
// context; a device whose configuration cannot be read that way is simply left
// unnarrowed, which is what every caller did before this existed.
//
// This is also where the fd is checked against the device that enumeration
// described: a mismatched idVendor is an error, not a Config, because every
// later decision -- which interface to claim, which endpoints to drive, whether
// to stream firmware at it -- is made from the descriptors this returns.
func (d *Device) Descriptors() (*Config, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cfg != nil {
		return d.cfg, nil
	}
	raw, err := d.readRawDescriptors()
	if err != nil {
		return nil, err
	}
	cfg, err := ParseDescriptors(raw)
	if err != nil {
		return nil, fmt.Errorf("usbfs: %s: %w", d.ref.Path, err)
	}
	// The fd is the first authoritative statement of what is actually on the
	// other end. Enumeration read idVendor from sysfs and then *synthesised*
	// the node path from busnum/devnum (Enumerate), and /dev/bus/usb/BBB/DDD is
	// a bus-address slot the kernel reuses -- so a device unplugged between
	// enumeration and Open can be replaced by an unrelated one that happens to
	// land in the freed slot, and nothing before this point would notice.
	// Both sides are guarded on being non-zero: a hand-built DeviceRef carries
	// no vendor ID (see Configuration), and a blob whose device descriptor was
	// shorter than 12 bytes yields none either.
	if d.ref.VendorID != 0 && cfg.VendorID != 0 && cfg.VendorID != d.ref.VendorID {
		return nil, fmt.Errorf("usbfs: %s now reports vendor %04x, but enumeration saw %04x: "+
			"the device at this bus address was replaced", d.ref.Path, cfg.VendorID, d.ref.VendorID)
	}
	// Narrowed before the value is published, so nothing ever observes the
	// union on a device where the union would be wrong.
	if v, ok := d.sysfsConfiguration(); ok {
		cfg.Active = v
		cfg.restrictToActive()
	}
	d.cfg = cfg
	return cfg, nil
}

func (d *Device) readRawDescriptors() ([]byte, error) {
	const chunk = 4096
	buf := make([]byte, 0, chunk)
	tmp := make([]byte, chunk)
	var off int64
	for len(buf) < maxDescriptorBytes {
		// ReadAt (pread) rather than Seek+Read: it needs no lock against
		// concurrent transfers and cannot be disturbed by them. usbfs
		// implements llseek, so the fd supports pread.
		n, err := d.f.ReadAt(tmp, off)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			off += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, wrapErrno("read descriptors", d.ref.Path, err)
		}
		if n == 0 {
			break
		}
	}
	if len(buf) == 0 {
		return nil, fmt.Errorf("usbfs: %s returned no descriptor bytes", d.ref.Path)
	}
	return buf, nil
}

// ClaimInterface claims an interface for this process.
//
// With detachKernelDriver set, any kernel driver bound to the interface is
// detached first. The preferred mechanism is USBDEVFS_DISCONNECT_CLAIM, which
// detaches and claims in a single ioctl: doing it in two steps leaves a window
// in which udev can rebind the driver before the claim lands, and the claim
// then fails with EBUSY (SPEC.md §4.2). The two-step path is kept only as a
// fallback for kernels older than 3.4.
//
// Detaching has a visible cost on a VFLEX in application mode: while
// snd-usb-audio is detached the ALSA card and its /dev/snd/midiC*D* node
// disappear, so any DAW or PipeWire client loses the port. Always Close (or
// ReleaseInterface) so the driver is put back -- and note that on the kernel
// ErrDriverNotRebound records, putting it back does not always work: the port
// then stays missing until the device is replugged, and the release says so.
func (d *Device) ClaimInterface(num int, detachKernelDriver bool) error {
	if detachKernelDriver {
		// Read before anything is detached: afterwards "we unbound it" and
		// "nothing was ever bound" look identical, and only the first makes an
		// interface still unbound after the reattach a problem. See claimState.
		hadDriver, _ := d.interfaceDriverBound(num)
		// EXCEPT_DRIVER with "usbfs" means "detach whatever driver is bound
		// unless it is usbfs itself". That is the right filter when the bound
		// driver's name is not known ahead of time -- which is the case here,
		// since the VFLEX presents as snd-usb-audio in application mode and as
		// nothing at all in bootloader mode.
		err := d.claimDetaching(num, disconnectClaimExceptDriver, "usbfs")
		if err == nil {
			d.markClaimed(num, claimState{detached: true, hadDriver: hadDriver})
			return nil
		}
		// Only an unimplemented ioctl justifies the racy fallback. usbfs
		// answers ENOTTY for a request number it does not know, so anything
		// else -- EBUSY, EINVAL for a bad interface number, EACCES -- is a
		// real failure and is reported as it happened rather than being
		// masked by a second attempt.
		if !errors.Is(err, ErrNotSupported) {
			return err
		}
		return d.detachThenClaim(num, hadDriver)
	}
	if err := d.claim(num); err != nil {
		return err
	}
	d.markClaimed(num, claimState{})
	return nil
}

// detachThenClaim is the pre-3.4 fallback: USBDEVFS_DISCONNECT followed by
// USBDEVFS_CLAIMINTERFACE, with the window between them that
// USBDEVFS_DISCONNECT_CLAIM exists to close.
//
// Between the two ioctls the kernel driver is already unbound, so from the
// moment the detach succeeds this process owes the system a reattach -- whether
// or not the claim that follows works. The claim can genuinely fail there
// (ENOMEM, EINVAL for an interface number claimintf rejects, or udev rebinding
// and a second process winning), and simply returning would leave snd-usb-audio
// unbound with nothing anywhere recording it: d.claimed would be empty, so
// neither ReleaseInterface nor Close would ever issue USBDEVFS_CONNECT and the
// user's ALSA MIDI port would stay missing until the device was replugged.
// The reattach therefore happens here, immediately, rather than being left to a
// cleanup path that has no way to know it is owed.
func (d *Device) detachThenClaim(num int, hadDriver bool) error {
	if err := d.detachDriver(num); err != nil {
		return err
	}
	if err := d.claim(num); err != nil {
		// Nothing is claimed, so nothing goes in d.claimed; the obligation is
		// discharged here instead. A failure to rebind is reported alongside
		// the claim failure rather than replacing it -- the claim failure is
		// what the caller asked about, the rebind failure is what the user has
		// to act on.
		if aerr := d.attachDriver(num, hadDriver); aerr != nil && !errors.Is(aerr, ErrNoDevice) {
			return errors.Join(err, fmt.Errorf(
				"usbfs: interface %d could not be claimed and the kernel driver detached to claim it "+
					"is not bound again (the ALSA MIDI port will stay missing until the device "+
					"is replugged): %w", num, aerr))
		}
		return err
	}
	d.markClaimed(num, claimState{detached: true, hadDriver: hadDriver})
	return nil
}

// ReleaseInterface releases an interface and, if this process detached a kernel
// driver to claim it, asks the kernel to rebind that driver.
//
// The reattach is not optional politeness. On a VFLEX in application mode the
// detached driver is snd-usb-audio, and leaving it detached makes the user's
// ALSA MIDI port vanish until the device is physically replugged -- so the
// default rawmidi transport stops working and the failure looks like broken
// hardware. Closing the fd does not rebind it; only USBDEVFS_CONNECT does.
//
// It is also not enough. USBDEVFS_CONNECT can return success and leave the
// interface with no driver on it, which is what a VFLEX was observed doing
// (ErrDriverNotRebound), so the result is checked against sysfs rather than
// assumed and the returned error tells the user to replug. That check runs only
// where a driver was seen bound before the claim, so the bootloader's
// vendor-class interface -- which has no driver to lose -- cannot trip it.
//
// The reattach is therefore attempted whatever USBDEVFS_RELEASEINTERFACE
// answered, and the bookkeeping entry is dropped only afterwards. A release
// failing for any reason outside the ErrNoDevice set says nothing about whether
// a driver is still detached, so skipping the rebind on that path would discard
// the only record that one is owed: d.claimed is where Close looks, and the
// entry is gone by then.
func (d *Device) ReleaseInterface(num int) error {
	d.mu.Lock()
	st := d.claimed[num]
	d.mu.Unlock()

	n := uint32(num)
	_, relErr := d.ioctl(fmt.Sprintf("release interface %d", num), ioctlReleaseInterface, unsafe.Pointer(&n), true)
	runtime.KeepAlive(&n)

	// A device that has gone away has already released everything, and there is
	// no driver left to rebind, so that is a success with nothing left to do.
	gone := errors.Is(relErr, ErrNoDevice)
	if gone {
		relErr = nil
	}

	var attachErr error
	if st.detached && !gone {
		// ENODATA (no driver wanted the interface) is already swallowed by
		// attachDriver; ErrNoDevice here means the device vanished between the
		// two ioctls, which again leaves nothing to rebind.
		//
		// "is not bound again" rather than "could not be reattached" because
		// this now covers two different things: an ioctl that failed, and an
		// ioctl that succeeded while leaving the interface driverless
		// (ErrDriverNotRebound). The user's next move is the same either way,
		// and the wrapped error says which happened.
		if err := d.attachDriver(num, st.hadDriver); err != nil && !errors.Is(err, ErrNoDevice) {
			attachErr = fmt.Errorf("usbfs: the kernel driver detached to claim interface %d is not bound "+
				"again (the ALSA MIDI port will stay missing until the device is replugged): %w", num, err)
		}
	}

	// The claim is gone either way -- a second USBDEVFS_RELEASEINTERFACE from
	// Close would answer EINVAL and mask whatever really happened -- so the
	// entry is dropped once the reattach has had its one chance.
	d.mu.Lock()
	delete(d.claimed, num)
	d.mu.Unlock()

	return errors.Join(relErr, attachErr)
}

// SetInterface selects an alternate setting. The interface must already be
// claimed; the kernel refuses otherwise.
func (d *Device) SetInterface(num, alt int) error {
	si := setInterface{iface: uint32(num), altSetting: uint32(alt)}
	_, err := d.ioctl(fmt.Sprintf("set interface %d alt %d", num, alt), ioctlSetInterface, unsafe.Pointer(&si), true)
	runtime.KeepAlive(&si)
	return err
}

// Control runs a synchronous control transfer on endpoint 0.
//
// requestType is bmRequestType; its top bit selects the direction, so data is
// filled in by the device when requestType&0x80 is set and sent to the device
// otherwise. The return value is the number of bytes actually transferred,
// which for an IN transfer may be shorter than len(data).
//
// See Transfer for how ctx and timeout interact.
func (d *Device) Control(ctx context.Context, requestType, request uint8, value, index uint16, data []byte, timeout time.Duration) (int, error) {
	if len(data) > MaxControlTransferSize {
		return 0, fmt.Errorf("%w: %d bytes, the control limit is %d", ErrTooLarge, len(data), MaxControlTransferSize)
	}
	ms, err := timeoutMs(ctx, timeout)
	if err != nil {
		return 0, err
	}
	ptr, keep := bufferPointer(data)
	ct := ctrlTransfer{
		bRequestType: requestType,
		bRequest:     request,
		wValue:       value,
		wIndex:       index,
		wLength:      uint16(len(data)),
		timeout:      ms,
		data:         ptr,
	}
	n, err := d.ioctl(fmt.Sprintf("control transfer bmRequestType=0x%02x bRequest=0x%02x", requestType, request),
		ioctlControl, unsafe.Pointer(&ct), false)
	runtime.KeepAlive(keep)
	runtime.KeepAlive(&ct)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Transfer runs a synchronous transfer on a bulk or interrupt endpoint.
//
// One entry point covers both because USBDEVFS_BULK does: the kernel looks up
// the endpoint descriptor and, for an interrupt endpoint, rewrites the pipe and
// submits an interrupt URB instead (usb_bulk_msg in drivers/usb/core/message.c).
// The name is a misnomer, not a constraint. Callers still need Endpoint.IsBulk
// and IsInterrupt for their own descriptor validation (SPEC.md §4.2).
//
// endpoint is the full bEndpointAddress, i.e. with 0x80 set for an IN endpoint;
// that is what the ioctl expects, and it is also what determines the direction
// of the transfer. Do not pass the bare 4-bit endpoint number (SPEC.md §10.2).
//
// Timeouts: usbfs takes a millisecond timeout in which 0 means "wait forever".
// timeout and ctx's deadline are both honoured -- the shorter of the two wins,
// and a zero timeout with no deadline falls back to DefaultTimeout so the
// kernel never receives a zero. Cancelling ctx does *not* abort a transfer
// already in flight: that would need the asynchronous URB interface, which this
// package deliberately does not implement. ctx is checked before submitting and
// bounds how long the ioctl can block, which is enough for a CLI.
func (d *Device) Transfer(ctx context.Context, endpoint uint8, data []byte, timeout time.Duration) (int, error) {
	if len(data) > MaxBulkTransferSize {
		return 0, fmt.Errorf("%w: %d bytes, the usbfs limit is %d", ErrTooLarge, len(data), MaxBulkTransferSize)
	}
	ms, err := timeoutMs(ctx, timeout)
	if err != nil {
		return 0, err
	}
	ptr, keep := bufferPointer(data)
	bt := bulkTransfer{
		ep:      uint32(endpoint),
		length:  uint32(len(data)),
		timeout: ms,
		data:    ptr,
	}
	dir := "OUT"
	if endpoint&endpointDirMask != 0 {
		dir = "IN"
	}
	n, err := d.ioctl(fmt.Sprintf("transfer %d bytes %s on endpoint 0x%02x", len(data), dir, endpoint),
		ioctlBulk, unsafe.Pointer(&bt), false)
	runtime.KeepAlive(keep)
	runtime.KeepAlive(&bt)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func (d *Device) markClaimed(num int, st claimState) {
	d.mu.Lock()
	d.claimed[num] = st
	d.mu.Unlock()
}

func (d *Device) claim(num int) error {
	n := uint32(num)
	_, err := d.ioctl(fmt.Sprintf("claim interface %d", num), ioctlClaimInterface, unsafe.Pointer(&n), true)
	runtime.KeepAlive(&n)
	return err
}

// claimDetaching issues USBDEVFS_DISCONNECT_CLAIM: detach (subject to flags)
// and claim, atomically.
func (d *Device) claimDetaching(num int, flags uint32, driver string) error {
	if len(driver) > maxDriverName {
		return fmt.Errorf("usbfs: driver name %q is longer than %d bytes", driver, maxDriverName)
	}
	dc := disconnectClaim{iface: uint32(num), flags: flags}
	// The kernel compares the whole fixed-size array, so the tail must stay
	// zeroed; copy never writes past the name.
	copy(dc.driver[:], driver)
	_, err := d.ioctl(fmt.Sprintf("detach and claim interface %d", num), ioctlDisconnectClaim, unsafe.Pointer(&dc), true)
	runtime.KeepAlive(&dc)
	return err
}

// detachDriver unbinds whatever kernel driver holds the interface, without
// claiming it. Only used on kernels too old for USBDEVFS_DISCONNECT_CLAIM.
func (d *Device) detachDriver(num int) error {
	err := d.interfaceIoctl(num, ioctlDisconnect, fmt.Sprintf("detach kernel driver from interface %d", num))
	// ENODATA means no driver was bound, which is the desired end state.
	if err != nil && errors.Is(err, unix.ENODATA) {
		return nil
	}
	return err
}

// attachDriver asks the kernel to rebind the in-tree driver for the interface
// and, when verify is set, checks that one actually did.
//
// The ioctl's own answer is not the answer the user cares about: USBDEVFS_CONNECT
// reports that the re-probe was accepted, not that a driver bound, and on a
// VFLEX those come apart (ErrDriverNotRebound). sysfs is the only place that
// says which happened, so it is read afterwards and an interface still sitting
// there driverless is reported rather than passed off as success.
//
// verify must be true only where a driver was seen bound before this process
// detached it. An interface that never had one -- the bootloader's vendor-class
// interface -- is expected to stay unbound, and reporting that would be a false
// alarm on the firmware path.
func (d *Device) attachDriver(num int, verify bool) error {
	err := d.interfaceIoctl(num, ioctlConnect, fmt.Sprintf("reattach kernel driver to interface %d", num))
	// ENODATA here means no driver wanted the interface -- nothing to rebind,
	// which is not a failure.
	if err != nil && errors.Is(err, unix.ENODATA) {
		err = nil
	}
	if err != nil || !verify {
		return err
	}
	if bound, known := d.driverRebound(num); known && !bound {
		return fmt.Errorf("%w: the kernel accepted the re-probe of interface %d on %s, but sysfs shows "+
			"no driver bound to it", ErrDriverNotRebound, num, d.ref.Path)
	}
	return nil
}

// driverRebound is interfaceDriverBound with a short settle window, for use
// right after a re-probe was requested. See driverRebindSettle for why the wait
// exists; an interface that is already bound, or a binding state that cannot be
// determined at all, returns at once and waits for nothing.
func (d *Device) driverRebound(num int) (bound, known bool) {
	deadline := time.Now().Add(driverRebindSettle)
	for {
		bound, known = d.interfaceDriverBound(num)
		if bound || !known || !time.Now().Before(deadline) {
			return bound, known
		}
		time.Sleep(driverRebindPoll)
	}
}

// interfaceDriverBound reports whether a kernel driver is bound to interface num
// right now, and whether that could be determined at all.
//
// An interface's sysfs directory is the device's own directory suffixed with
// ":<bConfigurationValue>.<interface>" -- /sys/bus/usb/devices/5-1.4.4:1.1 for
// interface 1 of a VFLEX in configuration 1 -- and a bound interface has a
// "driver" symlink inside it while an unbound one has none. The configuration
// number is whatever the device currently has selected and is not always
// readable (see Configuration), so the directory is globbed rather than assumed
// to be 1. Any match answers: sysfs only exposes interfaces of the active
// configuration, so at most one configuration's directories are present.
//
// "Cannot tell" is a distinct answer from "no driver" and every caller must
// treat it as such. A Device opened from a hand-built DeviceRef carries no
// SysPath at all, and a device unplugged mid-call takes its sysfs directory with
// it; neither is a driver that failed to bind.
func (d *Device) interfaceDriverBound(num int) (bound, known bool) {
	if d.ref.SysPath == "" {
		return false, false
	}
	dirs, err := filepath.Glob(fmt.Sprintf("%s:*.%d", d.ref.SysPath, num))
	if err != nil || len(dirs) == 0 {
		return false, false
	}
	for _, dir := range dirs {
		// Lstat, not Stat: the question is whether the symlink is there, not
		// whether whatever it points at can be reached.
		if _, err := os.Lstat(filepath.Join(dir, "driver")); err == nil {
			return true, true
		}
	}
	return false, true
}

// interfaceIoctl runs USBDEVFS_IOCTL, the wrapper that targets one interface
// with a nested request code.
func (d *Device) interfaceIoctl(num int, code uintptr, op string) error {
	arg := usbdevfsIoctl{ifno: int32(num), ioctlCode: int32(code)}
	_, err := d.ioctl(op, ioctlIoctl, unsafe.Pointer(&arg), true)
	runtime.KeepAlive(&arg)
	return err
}

// ioctl issues one ioctl on the device fd and returns its non-negative result
// (the transferred byte count, for the transfer ioctls).
//
// retryEINTR must be false for anything that moves data: retrying a transfer
// that the kernel may already have submitted could duplicate a write. It is
// safe for the idempotent management ioctls. In practice usbfs waits
// uninterruptibly, so EINTR is not expected either way.
//
// SyscallConn().Control is used rather than File.Fd() so that a concurrent
// Close cannot pull the descriptor out from under the syscall, and so the
// descriptor is not silently switched to blocking mode as a side effect.
func (d *Device) ioctl(op string, req uintptr, arg unsafe.Pointer, retryEINTR bool) (int, error) {
	if d.ioctlFn != nil {
		return d.ioctlFn(op, req, arg, retryEINTR)
	}
	return d.syscallIoctl(op, req, arg, retryEINTR)
}

func (d *Device) syscallIoctl(op string, req uintptr, arg unsafe.Pointer, retryEINTR bool) (int, error) {
	rc, err := d.f.SyscallConn()
	if err != nil {
		return 0, wrapErrno(op, d.ref.Path, err)
	}
	var (
		n     int
		errno syscall.Errno
	)
	if cerr := rc.Control(func(fd uintptr) {
		for {
			r1, _, e := unix.Syscall(unix.SYS_IOCTL, fd, req, uintptr(arg))
			n, errno = int(r1), e
			if e == unix.EINTR && retryEINTR {
				continue
			}
			return
		}
	}); cerr != nil {
		return 0, wrapErrno(op, d.ref.Path, cerr)
	}
	if errno != 0 {
		return 0, wrapErrno(op, d.ref.Path, errno)
	}
	return n, nil
}

// timeoutMs converts a caller timeout and a context deadline into the
// millisecond value usbfs wants, and reports a context that has already expired
// before anything is submitted.
func timeoutMs(ctx context.Context, timeout time.Duration) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("usbfs: transfer not submitted: %w", err)
	}
	eff := timeout
	if dl, ok := ctx.Deadline(); ok {
		remaining := time.Until(dl)
		if remaining <= 0 {
			return 0, fmt.Errorf("usbfs: transfer not submitted: %w", context.DeadlineExceeded)
		}
		if eff <= 0 || remaining < eff {
			eff = remaining
		}
	}
	if eff <= 0 {
		eff = DefaultTimeout
	}
	// Round up: a sub-millisecond budget must not become 0, which usbfs reads
	// as "block forever".
	ms := (int64(eff) + int64(time.Millisecond) - 1) / int64(time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	if ms > maxTimeoutMs {
		ms = maxTimeoutMs
	}
	return uint32(ms), nil
}

// bufferPointer yields the pointer to hand the kernel plus the slice to keep
// alive across the syscall. A zero-length transfer is legal on the wire (a
// zero-length packet is a real USB event), so it gets a one-byte scratch
// buffer rather than a nil pointer.
func bufferPointer(b []byte) (unsafe.Pointer, []byte) {
	if len(b) == 0 {
		z := make([]byte, 1)
		return unsafe.Pointer(&z[0]), z
	}
	return unsafe.Pointer(&b[0]), b
}
