package usbfs

import (
	"testing"
	"unsafe"
)

// The expected values were taken from a C program including
// <linux/usbdevice_fs.h> on x86-64:
//
//	CONTROL=0xC0185500 BULK=0xC0185502 SETINTERFACE=0x80085504
//	SETCONFIGURATION=0x80045505
//	CLAIM=0x8004550F   RELEASE=0x80045510 IOCTL=0xC0105512
//	DISCONNECT=0x5516  CONNECT=0x5517     DISCONNECT_CLAIM=0x8108551B
//	sizeof ctrl=24 bulk=24 setintf=8 ioctl=16 disconnect_claim=264

// TestIOCArithmetic checks the request-number packing independently of the Go
// struct layouts by feeding it the sizes the C header produces.
func TestIOCArithmetic(t *testing.T) {
	const U = usbdevfsMagic
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"USBDEVFS_CONTROL", ioWR(U, 0, 24), 0xC0185500},
		{"USBDEVFS_BULK", ioWR(U, 2, 24), 0xC0185502},
		{"USBDEVFS_SETINTERFACE", ioR(U, 4, 8), 0x80085504},
		{"USBDEVFS_SETCONFIGURATION", ioR(U, 5, 4), 0x80045505},
		{"USBDEVFS_CLAIMINTERFACE", ioR(U, 15, 4), 0x8004550F},
		{"USBDEVFS_RELEASEINTERFACE", ioR(U, 16, 4), 0x80045510},
		{"USBDEVFS_IOCTL", ioWR(U, 18, 16), 0xC0105512},
		{"USBDEVFS_DISCONNECT", ioNone(U, 22), 0x00005516},
		{"USBDEVFS_CONNECT", ioNone(U, 23), 0x00005517},
		{"USBDEVFS_DISCONNECT_CLAIM", ioR(U, 27, 264), 0x8108551B},
		// _IOW is unused by usbfs but the direction bit must still be right:
		// _IOW('U', 1, 4) = 0x40045501.
		{"_IOW('U',1,4)", ioc(iocWrite, U, 1, 4), 0x40045501},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = 0x%08X, want 0x%08X", tc.name, tc.got, tc.want)
		}
	}
}

// TestRequestNumbers checks the numbers the package actually issues, which
// depend on the Go struct layouts matching the C ones.
func TestRequestNumbers(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("expected values are the 64-bit ones")
	}
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"ioctlControl", ioctlControl, 0xC0185500},
		{"ioctlBulk", ioctlBulk, 0xC0185502},
		{"ioctlSetInterface", ioctlSetInterface, 0x80085504},
		{"ioctlSetConfiguration", ioctlSetConfiguration, 0x80045505},
		{"ioctlClaimInterface", ioctlClaimInterface, 0x8004550F},
		{"ioctlReleaseInterface", ioctlReleaseInterface, 0x80045510},
		{"ioctlIoctl", ioctlIoctl, 0xC0105512},
		{"ioctlDisconnect", ioctlDisconnect, 0x00005516},
		{"ioctlConnect", ioctlConnect, 0x00005517},
		{"ioctlDisconnectClaim", ioctlDisconnectClaim, 0x8108551B},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = 0x%08X, want 0x%08X", tc.name, tc.got, tc.want)
		}
	}
}

// TestStructLayout pins the field offsets the kernel reads. Getting the
// pointer alignment padding wrong here would silently hand the kernel a
// garbage data pointer.
func TestStructLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("expected offsets are the 64-bit ones")
	}
	checks := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"sizeof(ctrlTransfer)", unsafe.Sizeof(ctrlTransfer{}), 24},
		{"offsetof(ctrlTransfer.timeout)", unsafe.Offsetof(ctrlTransfer{}.timeout), 8},
		{"offsetof(ctrlTransfer.data)", unsafe.Offsetof(ctrlTransfer{}.data), 16},
		{"sizeof(bulkTransfer)", unsafe.Sizeof(bulkTransfer{}), 24},
		{"offsetof(bulkTransfer.data)", unsafe.Offsetof(bulkTransfer{}.data), 16},
		{"sizeof(setInterface)", unsafe.Sizeof(setInterface{}), 8},
		{"sizeof(usbdevfsIoctl)", unsafe.Sizeof(usbdevfsIoctl{}), 16},
		{"offsetof(usbdevfsIoctl.data)", unsafe.Offsetof(usbdevfsIoctl{}.data), 8},
		{"sizeof(disconnectClaim)", unsafe.Sizeof(disconnectClaim{}), 264},
		{"offsetof(disconnectClaim.driver)", unsafe.Offsetof(disconnectClaim{}.driver), 8},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// TestNestedIoctlCodesFitInt32 guards the usbdevfs_ioctl.ioctl_code field,
// which is a signed 32-bit int carrying another request number.
func TestNestedIoctlCodesFitInt32(t *testing.T) {
	for _, code := range []uintptr{ioctlDisconnect, ioctlConnect} {
		if uintptr(int32(code)) != code {
			t.Errorf("nested ioctl code 0x%08X does not survive int32", code)
		}
	}
}
