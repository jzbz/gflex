package usbfs

import "unsafe"

// ---------------------------------------------------------------------------
// ioctl request-number arithmetic (include/uapi/asm-generic/ioctl.h)
//
// A request number is a packed 32-bit word:
//
//	 31 30 | 29 ......... 16 | 15 ..... 8 | 7 ..... 0
//	  dir  |      size       |    type    |    nr
//
// The direction bits are from the *userspace* point of view and are, confusingly,
// the opposite of what the macro names suggest: _IOR (the caller reads a result)
// sets _IOC_READ = 2, _IOW sets _IOC_WRITE = 1, _IOWR sets both.
//
// These are derived here rather than pasted as magic numbers so that the size
// field always agrees with the Go structs below -- including on a 32-bit build,
// where every struct containing a pointer shrinks and every affected request
// number changes with it.
// ---------------------------------------------------------------------------

const (
	iocNRBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14
	iocDirBits  = 2

	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits     // 8
	iocSizeShift = iocTypeShift + iocTypeBits // 16
	iocDirShift  = iocSizeShift + iocSizeBits // 30

	iocSizeMask = (1 << iocSizeBits) - 1
	iocDirMask  = (1 << iocDirBits) - 1

	iocNone  = 0
	iocWrite = 1
	iocRead  = 2

	// usbdevfsMagic is the 'U' type byte shared by every usbfs ioctl.
	usbdevfsMagic = 'U'
)

// ioc packs an ioctl request number. dir is iocNone, iocWrite, iocRead or their
// OR; size is the size of the argument struct in bytes.
func ioc(dir, typ, nr, size uintptr) uintptr {
	return (dir&iocDirMask)<<iocDirShift |
		(size&iocSizeMask)<<iocSizeShift |
		typ<<iocTypeShift |
		nr<<iocNRShift
}

func ioR(typ, nr, size uintptr) uintptr  { return ioc(iocRead, typ, nr, size) }
func ioWR(typ, nr, size uintptr) uintptr { return ioc(iocRead|iocWrite, typ, nr, size) }
func ioNone(typ, nr uintptr) uintptr     { return ioc(iocNone, typ, nr, 0) }

// ---------------------------------------------------------------------------
// Argument structs.
//
// Each mirrors its C counterpart field for field. Go's alignment rules match
// the C ABI here, so the trailing pointer lands at the same offset (16 on
// 64-bit, 12 on 32-bit) and no explicit padding fields are needed; the layout
// is asserted against the real header in ioctl_test.go.
//
// The data members are unsafe.Pointer rather than uintptr on purpose: a
// uintptr is invisible to the garbage collector, so a Go buffer referenced only
// by one could be collected or (if it lived on a stack) moved out from under
// the kernel. As unsafe.Pointer the field is a real pointer slot that the GC
// scans and stack copying updates. Callers additionally runtime.KeepAlive the
// backing slice across the syscall.
// ---------------------------------------------------------------------------

// struct usbdevfs_ctrltransfer
type ctrlTransfer struct {
	bRequestType uint8
	bRequest     uint8
	wValue       uint16
	wIndex       uint16
	wLength      uint16
	timeout      uint32 // milliseconds; 0 means wait forever
	data         unsafe.Pointer
}

// struct usbdevfs_bulktransfer
type bulkTransfer struct {
	ep      uint32 // full bEndpointAddress, IN has 0x80 set
	length  uint32
	timeout uint32 // milliseconds; 0 means wait forever
	data    unsafe.Pointer
}

// struct usbdevfs_setinterface
type setInterface struct {
	iface      uint32
	altSetting uint32
}

// struct usbdevfs_ioctl -- the "run an ioctl against one interface" wrapper,
// used to reach USBDEVFS_DISCONNECT and USBDEVFS_CONNECT.
type usbdevfsIoctl struct {
	ifno      int32
	ioctlCode int32
	data      unsafe.Pointer
}

// maxDriverName is USBDEVFS_MAXDRIVERNAME; the struct field is one larger to
// leave room for the NUL.
const maxDriverName = 255

// struct usbdevfs_disconnect_claim
type disconnectClaim struct {
	iface  uint32
	flags  uint32
	driver [maxDriverName + 1]byte
}

// disconnectClaimExceptDriver is USBDEVFS_DISCONNECT_CLAIM_EXCEPT_DRIVER: a
// filter on the driver currently bound to the interface, detaching it only when
// its name differs from driver[]. The kernel also defines
// USBDEVFS_DISCONNECT_CLAIM_IF_DRIVER (0x01), the opposite filter, and treats
// neither flag set as "detach whatever is bound"; neither is used here, because
// the VFLEX presents as snd-usb-audio in application mode and as nothing at all
// in bootloader mode, so the bound driver's name is never known ahead of time.
const disconnectClaimExceptDriver = 0x02

// Request numbers. These are constants in all but name -- Go cannot call a
// function from a const expression, and nothing ever writes to them. The
// expected 64-bit values are in the comments and are asserted by the tests.
var (
	ioctlControl      = ioWR(usbdevfsMagic, 0, unsafe.Sizeof(ctrlTransfer{})) // 0xC0185500
	ioctlBulk         = ioWR(usbdevfsMagic, 2, unsafe.Sizeof(bulkTransfer{})) // 0xC0185502
	ioctlSetInterface = ioR(usbdevfsMagic, 4, unsafe.Sizeof(setInterface{}))  // 0x80085504
	// USBDEVFS_SETCONFIGURATION is _IOR('U', 5, unsigned int): the argument is a
	// bare bConfigurationValue, not a struct, so the size is that of a C
	// unsigned int on every architecture Linux supports.
	ioctlSetConfiguration = ioR(usbdevfsMagic, 5, unsafe.Sizeof(uint32(0)))          // 0x80045505
	ioctlClaimInterface   = ioR(usbdevfsMagic, 15, unsafe.Sizeof(uint32(0)))         // 0x8004550F
	ioctlReleaseInterface = ioR(usbdevfsMagic, 16, unsafe.Sizeof(uint32(0)))         // 0x80045510
	ioctlIoctl            = ioWR(usbdevfsMagic, 18, unsafe.Sizeof(usbdevfsIoctl{}))  // 0xC0105512
	ioctlDisconnect       = ioNone(usbdevfsMagic, 22)                                // 0x00005516
	ioctlConnect          = ioNone(usbdevfsMagic, 23)                                // 0x00005517
	ioctlDisconnectClaim  = ioR(usbdevfsMagic, 27, unsafe.Sizeof(disconnectClaim{})) // 0x8108551B
)
