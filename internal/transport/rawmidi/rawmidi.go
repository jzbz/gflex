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
//
// It used to be justified as "an order of magnitude below the protocol's 20 ms
// inter-message delay, so it cannot become the bottleneck". That stopped being
// true when the default pacing moved to 1 ms (SPEC.md §14.15): 2 ms is now twice
// a whole outbound gap, not a tenth of one, so on this path a quiet spell can
// cost more than the pacing does.
//
// It is left at 2 ms anyway, because the path it guards is the one place that
// cannot be measured from here. It runs only when the netpoller registration
// failed, which no shipped configuration has produced -- the healthy path parks
// in the netpoller and never polls at all -- so there is no observation to tune
// against, and a smaller value would trade unmeasured latency for a spin on a
// descriptor that is already misbehaving. Measure that path before shrinking it.
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
// problem, ErrNotFound when the node is gone, ErrNotADevice when the path
// opened to a regular file.
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
	// The caller checked a name; nobody has yet checked the object. internal/cli
	// stats a --port path and refuses anything that is not a character device
	// (SPEC.md §11), but a stat and an open are two syscalls: a path replaced
	// between them passes the check and is opened anyway, and the first frame is
	// then written into whatever it now names. Asking the kernel about the
	// descriptor already held closes that window, because the descriptor cannot
	// be swapped out from under us.
	//
	// It refuses a regular file, not "anything that is not a character device",
	// and the difference is the whole design of the check. A regular file is the
	// only thing MIDI frames corrupt silently -- the write succeeds, the file
	// grows, nothing on stderr says so -- while every other kind of file either
	// fails the open or is a legitimate byte stream. Demanding ModeCharDevice
	// would additionally refuse a FIFO, which is what the tests use as a
	// hardware stand-in for exactly the deadlock behaviour this transport exists
	// to get right.
	//
	// O_NOFOLLOW is the wrong tool for the same reason: /dev/snd/by-path holds
	// udev symlinks to real nodes (pci-0000:c4:00.1 -> ../controlC0), so
	// refusing a symlinked final component would refuse a path an operator can
	// reasonably pass to --port. It is the object at the end that matters, and
	// fstat is what reports it.
	//
	// A stat that fails on a descriptor we just opened is not itself a reason to
	// refuse a port that opened cleanly, so only a positive answer stops us.
	if fi, serr := f.Stat(); serr == nil && fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s is a regular file. Expected an ALSA rawmidi node such as "+
			"/dev/snd/midiC1D0; nothing was written to it. Run \"gflex devices\" to list what is "+
			"present", ErrNotADevice, path)
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
