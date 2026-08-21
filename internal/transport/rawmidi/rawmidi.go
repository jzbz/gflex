// Package rawmidi implements the default VFLEX transport: a pure-Go ALSA
// rawmidi stream over /dev/snd/midiC<card>D<device>.
//
// No cgo, no alsa-lib and no ioctls are involved. alsa-lib's "hw" rawmidi
// plugin does nothing more than open the node and read/write it, and the kernel
// treats a rawmidi node as a transparent byte FIFO: it neither reframes nor
// canonicalises MIDI messages. That matters here because every VFLEX protocol
// byte is carried as a Note-On whose velocity is the byte's low nibble, so one
// byte in sixteen is a Note-On with velocity 0 -- which a "helpful" MIDI
// abstraction layer would rewrite to Note-Off and corrupt (SPEC.md §3.2). Going
// straight to rawmidi avoids that class of middleware entirely, and keeps the
// binary buildable with CGO_ENABLED=0 (SPEC.md §4.1).
//
// Discovery is anchored on the USB vendor ID rather than on the port name the
// vendor app matches. The advertised name is now known -- "Werewolf VFLEX",
// measured on one unit (SPEC.md §14.2) -- and the vendor app's "vflex"
// substring does match it, but a name is firmware-dependent and anything at all
// can claim one, whereas the vendor ID is assigned. The substring is kept as a
// fallback for ports with no USB parent to trace (SPEC.md §3.4).
package rawmidi

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jzbz/gflex/internal/proto"
)

// idlePoll is the retry interval used only on the degraded path where the
// kernel refused to register the descriptor with Go's netpoller (see port).
// It is an order of magnitude below the protocol's 20 ms inter-message delay,
// so it cannot become the bottleneck.
const idlePoll = 2 * time.Millisecond

// port is an open rawmidi device node.
//
// Concurrency design, and why the file is opened O_NONBLOCK:
//
// The framer runs a dedicated reader goroutine that sits in ReadMIDI, so
// Close() must be able to unblock it -- a Close() that deadlocks is the obvious
// failure mode for this transport. Opening with O_NONBLOCK makes os.OpenFile
// hand the descriptor to the runtime netpoller (os.newFile registers any
// descriptor opened non-blocking, and ALSA rawmidi implements .poll, so epoll
// accepts it). A blocked Read is then parked in the netpoller rather than in
// the kernel read(2), and *os.File.Close evicts it: the pending read returns
// os.ErrClosed and the real close(2) is deferred until it does, so there is no
// descriptor-reuse race either. That is the whole reason for using *os.File
// here instead of a bare fd.
//
// If registration were to fail -- which would need a kernel whose rawmidi node
// has no .poll -- the descriptor stays non-blocking and unpolled, so reads
// would spin on EAGAIN. That case is detected at open time and handled by the
// idlePoll retry loop below; it still cannot deadlock.
type port struct {
	f    *os.File
	path string

	// pollable records whether the netpoller took the descriptor.
	pollable bool

	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

// Open opens an ALSA rawmidi device node, e.g. /dev/snd/midiC1D0, for reading
// and writing. Failures are classified into the package's sentinel errors:
// ErrBusy when another MIDI client holds the port, ErrPermission for a udev/ACL
// problem, ErrNotFound when the node is gone.
func Open(path string) (proto.Transport, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: empty device path", ErrNotFound)
	}
	// O_NONBLOCK is what gets this descriptor into the netpoller; see the type
	// comment on port. os.OpenFile adds O_CLOEXEC itself.
	f, err := os.OpenFile(path, os.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, classifyOpenError(path, err)
	}
	p := &port{f: f, path: path}
	// SetReadDeadline reports os.ErrNoDeadline when the descriptor could not be
	// registered with the netpoller. Clearing an unset deadline is a no-op, so
	// this is a side-effect-free probe.
	if err := f.SetReadDeadline(time.Time{}); err == nil {
		p.pollable = true
	}
	return p, nil
}

// Name returns the device node path, for diagnostics.
func (p *port) Name() string { return p.path }

// ReadMIDI reads whatever bytes are available. It may return a partial MIDI
// message; the framer reassembles across calls (SPEC.md §3.3).
func (p *port) ReadMIDI(b []byte) (int, error) {
	for {
		if p.closed.Load() {
			return 0, fmt.Errorf("rawmidi: read %s: %w", p.path, os.ErrClosed)
		}
		n, err := p.f.Read(b)
		switch {
		case err == nil:
			return n, nil
		case errors.Is(err, io.EOF):
			// Returned unwrapped: callers commonly compare against io.EOF.
			return n, io.EOF
		case !p.pollable && errors.Is(err, unix.EAGAIN):
			// Degraded path only: no netpoller, so wait and retry. The closed
			// check at the top of the loop is what makes Close() effective.
			time.Sleep(idlePoll)
		case errors.Is(err, os.ErrClosed):
			return n, fmt.Errorf("rawmidi: read %s: %w", p.path, os.ErrClosed)
		default:
			return n, fmt.Errorf("rawmidi: read %s: %w", p.path, err)
		}
	}
}

// WriteMIDI writes one or more complete MIDI messages, retrying until the whole
// buffer has been accepted.
func (p *port) WriteMIDI(b []byte) error {
	for len(b) > 0 {
		if p.closed.Load() {
			return fmt.Errorf("rawmidi: write %s: %w", p.path, os.ErrClosed)
		}
		n, err := p.f.Write(b)
		if n > 0 {
			b = b[n:]
		}
		switch {
		case err == nil:
			continue
		case !p.pollable && errors.Is(err, unix.EAGAIN):
			// The kernel's rawmidi output FIFO is full. Only reachable on the
			// degraded path; with the netpoller *os.File waits for us.
			time.Sleep(idlePoll)
		case errors.Is(err, os.ErrClosed):
			return fmt.Errorf("rawmidi: write %s: %w", p.path, os.ErrClosed)
		default:
			return fmt.Errorf("rawmidi: write %s: %w", p.path, err)
		}
	}
	return nil
}

// Close releases the device node and unblocks any goroutine parked in
// ReadMIDI. It is idempotent and safe to call concurrently with reads, writes
// and other Closes.
func (p *port) Close() error {
	// Set the flag first so the degraded-path loops above stop retrying.
	p.closed.Store(true)
	p.closeOnce.Do(func() {
		// *os.File.Close evicts pending netpoller waits, so a blocked reader
		// wakes with os.ErrClosed instead of hanging until the device sends
		// something. The descriptor itself is not released until that reader
		// returns, so nothing can reuse the fd number underneath it.
		if err := p.f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			p.closeErr = fmt.Errorf("rawmidi: close %s: %w", p.path, err)
		}
	})
	return p.closeErr
}
