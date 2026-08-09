package usbfs

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWrapErrnoClassification(t *testing.T) {
	tests := []struct {
		errno unix.Errno
		class error
	}{
		{unix.EACCES, ErrPermission},
		{unix.EPERM, ErrPermission},
		{unix.ENODEV, ErrNoDevice},
		{unix.ENXIO, ErrNoDevice},
		{unix.ESHUTDOWN, ErrNoDevice},
		{unix.EBUSY, ErrBusy},
		{unix.ETIMEDOUT, ErrTimeout},
		{unix.EPIPE, ErrStall},
		{unix.ENOTTY, ErrNotSupported},
	}
	for _, tc := range tests {
		err := wrapErrno("claim interface 0", "/dev/bus/usb/001/007", tc.errno)
		if !errors.Is(err, tc.class) {
			t.Errorf("%v: not classified as %v (%v)", tc.errno, tc.class, err)
		}
		// The raw errno must survive too, so callers can match either level.
		if !errors.Is(err, tc.errno) {
			t.Errorf("%v: raw errno lost in %v", tc.errno, err)
		}
		if !strings.Contains(err.Error(), "/dev/bus/usb/001/007") {
			t.Errorf("%v: message omits the device path: %v", tc.errno, err)
		}
	}
}

// A permission failure has to say what to do about it: a missing udev rule is
// the single most likely reason this package fails at all (SPEC.md §4.4).
func TestWrapErrnoPermissionHint(t *testing.T) {
	err := wrapErrno("open", "/dev/bus/usb/001/007", &os.PathError{
		Op: "open", Path: "/dev/bus/usb/001/007", Err: unix.EACCES,
	})
	if !errors.Is(err, ErrPermission) {
		t.Fatalf("PathError not unwrapped to an errno: %v", err)
	}
	if !strings.Contains(err.Error(), "udev") {
		t.Errorf("no udev hint in %q", err.Error())
	}
}

func TestWrapErrnoNonErrno(t *testing.T) {
	sentinel := errors.New("boom")
	err := wrapErrno("open", "/dev/bus/usb/001/007", sentinel)
	if !errors.Is(err, sentinel) {
		t.Fatalf("non-errno error was swallowed: %v", err)
	}
	var e *Error
	if errors.As(err, &e) {
		t.Fatalf("non-errno error became an *Error: %v", err)
	}
}

func TestTimeoutMs(t *testing.T) {
	background := context.Background()

	// A plain timeout passes through.
	if ms, err := timeoutMs(background, 250*time.Millisecond); err != nil || ms != 250 {
		t.Errorf("timeoutMs(250ms) = %d, %v; want 250, nil", ms, err)
	}

	// Zero means "wait forever" to usbfs, so it must never reach the kernel.
	ms, err := timeoutMs(background, 0)
	if err != nil {
		t.Fatalf("timeoutMs(0): %v", err)
	}
	if ms != uint32(DefaultTimeout/time.Millisecond) {
		t.Errorf("timeoutMs(0) = %d, want the DefaultTimeout substitution %d",
			ms, DefaultTimeout/time.Millisecond)
	}

	// A sub-millisecond budget rounds up rather than down to the forever value.
	ctx, cancel := context.WithTimeout(background, 100*time.Microsecond)
	defer cancel()
	if ms, err := timeoutMs(ctx, time.Second); err != nil || ms != 1 {
		t.Errorf("sub-millisecond deadline gave %d, %v; want 1, nil", ms, err)
	}

	// The context deadline clamps a longer caller timeout.
	ctx2, cancel2 := context.WithTimeout(background, 40*time.Millisecond)
	defer cancel2()
	if ms, err := timeoutMs(ctx2, time.Hour); err != nil || ms == 0 || ms > 40 {
		t.Errorf("clamped timeout = %d, %v; want 1..40, nil", ms, err)
	}

	// A caller timeout shorter than the deadline wins.
	if ms, err := timeoutMs(ctx2, 5*time.Millisecond); err != nil || ms != 5 {
		t.Errorf("timeoutMs(5ms under a 40ms deadline) = %d, %v; want 5, nil", ms, err)
	}
}

func TestTimeoutMsContextAlreadyDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := timeoutMs(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}

	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()
	if _, err := timeoutMs(expired, time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

// A zero-length transfer is legal, so bufferPointer must still hand the kernel
// a real address.
func TestBufferPointer(t *testing.T) {
	p, keep := bufferPointer(nil)
	if p == nil || len(keep) != 1 {
		t.Errorf("bufferPointer(nil) = %v, %v; want a 1-byte scratch buffer", p, keep)
	}
	data := []byte{1, 2, 3}
	p, keep = bufferPointer(data)
	if p == nil || &keep[0] != &data[0] {
		t.Error("bufferPointer did not point at the caller's buffer")
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(DeviceRef{}); err == nil {
		t.Fatal("expected an error opening a DeviceRef with no path")
	}
}

func TestTransferRejectsOversizedBuffer(t *testing.T) {
	d := &Device{ref: DeviceRef{Path: "/dev/bus/usb/001/007"}, claimed: map[int]bool{}}
	if _, err := d.Transfer(context.Background(), 0x81, make([]byte, MaxBulkTransferSize+1), time.Second); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
	if _, err := d.Control(context.Background(), 0x21, 0x22, 1, 0, make([]byte, MaxControlTransferSize+1), time.Second); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}
