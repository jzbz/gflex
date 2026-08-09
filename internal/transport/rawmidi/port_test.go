package rawmidi

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A FIFO stands in for the rawmidi character device: both are pollable,
// non-seekable byte streams, and Linux lets us open a FIFO O_RDWR to get a
// loopback. That exercises the parts of the transport that can actually
// deadlock -- the netpoller registration and Close() unblocking a parked
// reader -- without any hardware.
func fifoTransport(t *testing.T) (*port, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "midiC9D0")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	tr, err := Open(path)
	if err != nil {
		t.Fatalf("open fifo: %v", err)
	}
	p, ok := tr.(*port)
	if !ok {
		t.Fatalf("Open returned %T, want *port", tr)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, path
}

func TestPortNameIsThePath(t *testing.T) {
	p, path := fifoTransport(t)
	if p.Name() != path {
		t.Errorf("Name() = %q, want %q", p.Name(), path)
	}
}

// The descriptor must end up in the runtime netpoller; if it does not, reads
// fall back to the EAGAIN retry loop and this test documents that regression.
func TestPortIsPollable(t *testing.T) {
	p, _ := fifoTransport(t)
	if !p.pollable {
		t.Error("descriptor was not registered with the netpoller; " +
			"reads will use the degraded retry path")
	}
}

func TestPortRoundTrip(t *testing.T) {
	p, _ := fifoTransport(t)

	// The golden set-voltage stream from SPEC.md §15, truncated to the start
	// marker and one data message.
	want := []byte{0x80, 0x00, 0x00, 0x90, 0x00, 0x04}
	if err := p.WriteMIDI(want); err != nil {
		t.Fatalf("WriteMIDI: %v", err)
	}
	got := make([]byte, 0, len(want))
	buf := make([]byte, 4)
	for len(got) < len(want) {
		n, err := p.ReadMIDI(buf)
		if err != nil {
			t.Fatalf("ReadMIDI: %v", err)
		}
		got = append(got, buf[:n]...)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("round trip: got % x, want % x", got, want)
	}
}

// The bug this whole design exists to prevent: a reader parked in ReadMIDI must
// come back when Close() is called, not hang until the device sends something.
func TestCloseUnblocksReader(t *testing.T) {
	p, _ := fifoTransport(t)

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := p.ReadMIDI(buf)
		done <- err
	}()

	// Give the reader time to actually block.
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("reader returned early with %v", err)
	default:
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("reader returned no error after Close")
		}
		if !errors.Is(err, os.ErrClosed) {
			t.Errorf("reader error = %v, want it to wrap os.ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not unblock the reader: deadlock")
	}
}

func TestCloseIsIdempotentAndConcurrent(t *testing.T) {
	p, _ := fifoTransport(t)

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() { errs <- p.Close() }()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Close: %v", err)
		}
	}
	if err := p.Close(); err != nil {
		t.Errorf("repeat Close: %v", err)
	}
}

func TestIOAfterCloseReportsClosed(t *testing.T) {
	p, _ := fifoTransport(t)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadMIDI(make([]byte, 4)); !errors.Is(err, os.ErrClosed) {
		t.Errorf("ReadMIDI after Close = %v, want os.ErrClosed", err)
	}
	if err := p.WriteMIDI([]byte{0x80, 0x00, 0x00}); !errors.Is(err, os.ErrClosed) {
		t.Errorf("WriteMIDI after Close = %v, want os.ErrClosed", err)
	}
}
