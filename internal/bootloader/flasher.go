package bootloader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jzbz/gflex/internal/ctxwait"
	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/usbfs"
)

// Timing constants recovered from the vendor's update pipeline (SPEC.md §10.1).
//
// The stream-mode delays are the whole reason a flash is fast: the device is
// not asked to acknowledge anything, so the host paces itself instead.
const (
	// ChunkDelay is the pause after each WRITE_CHUNK in stream mode.
	ChunkDelay = 2 * time.Millisecond
	// CommitDelay is the pause after each COMMIT_PAGE in stream mode; a page
	// commit is a flash write and takes longer than a chunk.
	CommitDelay = 25 * time.Millisecond
	// PostFlashDelay is the settle time after the final page commit, before
	// the CRC is requested (POST_FLASH_DELAY_MS).
	PostFlashDelay = 2000 * time.Millisecond
	// VerifyRoundDelay separates the two verification attempts.
	VerifyRoundDelay = 200 * time.Millisecond
	// verifyAttempts is how many times the write-then-read verify pair is sent
	// before giving up.
	verifyAttempts = 2
	// interFrameDelay separates the two halves of a verify round.
	interFrameDelay = 2 * time.Millisecond
	// bulkWriteTimeout bounds a single OUT transfer.
	bulkWriteTimeout = 5 * time.Second
	// ackPollTimeout is the per-read timeout while waiting for an ACK. Reads
	// are polled rather than issued with the full ACK budget so that a
	// transport which reports a timeout as an error does not abort the wait.
	ackPollTimeout = 200 * time.Millisecond
	// ackRetryPause backs off after a failed read so that a transport which
	// fails instantly cannot turn the wait into a busy loop.
	ackRetryPause = 5 * time.Millisecond
	// controlTimeout bounds the best-effort SET_CONTROL_LINE_STATE transfer.
	controlTimeout = 1 * time.Second
)

// timing groups the flasher's delays and budgets.
//
// These are fields rather than constants only so that the flash sequencing can
// be exercised in tests without real waits; the defaults are exactly the values
// SPEC.md §10.1 records and nothing outside this package can change them.
type timing struct {
	chunk       time.Duration
	commit      time.Duration
	postFlash   time.Duration
	verifyRound time.Duration
	ack         time.Duration
	verify      time.Duration
	serial      time.Duration
}

func defaultTiming() timing {
	return timing{
		chunk:       ChunkDelay,
		commit:      CommitDelay,
		postFlash:   PostFlashDelay,
		verifyRound: VerifyRoundDelay,
		ack:         proto.BootloaderACKTimeout,
		verify:      proto.VerifyTimeout,
		serial:      proto.DefaultTimeout,
	}
}

// bulkDevice is the part of *usbfs.Device the flasher uses. Naming it as an
// interface keeps the sequencing testable without USB hardware.
type bulkDevice interface {
	Transfer(ctx context.Context, endpoint uint8, data []byte, timeout time.Duration) (int, error)
}

// Bulk read sizing. The vendor client reads in 64-byte units; a larger endpoint
// packet size is honoured when the descriptor declares one.
const minReadBufferLen = 64

// Progress phases reported through the Progress callback.
const (
	PhaseSerial = "serial"
	PhaseChunk  = "chunk"
	PhaseCommit = "commit"
	PhaseSettle = "settle"
	PhaseVerify = "verify"
	PhaseEnd    = "end"
)

// Progress describes where a flash has got to. Page and Chunk are zero-based;
// Chunk is -1 for phases that are not per-chunk.
type Progress struct {
	Phase       string
	Page        int
	TotalPages  int
	Chunk       int
	TotalChunks int
}

// Errors returned by the flasher.
var (
	// ErrCRCMismatch reports that the device's CRC did not match the image's.
	// The unit is still in the bootloader and is re-flashable.
	ErrCRCMismatch = errors.New("bootloader: firmware CRC mismatch")
	// ErrACKTimeout reports that no matching acknowledgement arrived.
	ErrACKTimeout = errors.New("bootloader: timed out waiting for acknowledgement")
	// ErrVerifyNoCRC reports a CMD_BOOTLOADER_VERIFY answer that carried no CRC
	// byte. It is a distinct outcome from ErrACKTimeout and a more useful one:
	// the device did answer, it just answered the write form. Verify keeps it in
	// preference to the timeout the round then ends in, because "it replied
	// without a CRC" and "nothing arrived at all" send a user to different
	// places.
	ErrVerifyNoCRC = errors.New("bootloader: verify response carried no CRC byte")
	// ErrNoEndpoints reports an interface without both an IN and an OUT
	// endpoint, which cannot carry the bootloader protocol.
	ErrNoEndpoints = errors.New("bootloader: interface has no IN/OUT endpoint pair")
	// ErrSerialMismatch reports that the unit in the bootloader is not the unit
	// the image was fetched for.
	ErrSerialMismatch = errors.New("bootloader: serial number mismatch")
	// ErrUnverifiable reports an update that could only have ended by starting
	// an image nothing had checked — an image with no CRC, or one whose check
	// was deliberately skipped — without the caller accepting that. It is
	// distinct from ErrCRCMismatch: here nothing disagreed, there was simply
	// nothing to compare against (SPEC.md §10.2, §14.12).
	ErrUnverifiable = errors.New("bootloader: firmware image cannot be verified")
)

// Flasher drives the bootloader protocol over a claimed vendor-class interface.
//
// It is not safe for concurrent use: like the application protocol, the
// bootloader is strictly one command in flight (SPEC.md §5.3).
type Flasher struct {
	dev     bulkDevice
	iface   usbfs.Interface
	in, out usbfs.Endpoint
	setup   error // non-nil when the interface cannot carry the protocol
	readLen int
	t       timing
	// sleep replaces the real wait, so a test can assert *which* delays a
	// sequence applies rather than only how long it took. nil means
	// ctxwait.Sleep, which is what every non-test path uses.
	sleep func(context.Context, time.Duration) error
}

// pause waits for d, through the injected sleep when there is one.
func (f *Flasher) pause(ctx context.Context, d time.Duration) error {
	if f.sleep != nil {
		return f.sleep(ctx, d)
	}
	return ctxwait.Sleep(ctx, d)
}

// NewFlasher builds a Flasher over an already-open device and an interface that
// has already been claimed (see Connect).
//
// Endpoint selection happens here, but a missing endpoint pair is reported from
// the first operation rather than the constructor, which has no error return.
func NewFlasher(dev *usbfs.Device, iface usbfs.Interface) *Flasher {
	f := &Flasher{iface: iface, readLen: minReadBufferLen, t: defaultTiming()}
	if dev == nil {
		// Assigning a nil *usbfs.Device would produce a non-nil interface
		// value, so the field is left alone and every method bails on setup.
		f.setup = errors.New("bootloader: no USB device")
		return f
	}
	f.dev = dev
	in, okIn := iface.In()
	out, okOut := iface.Out()
	if !okIn || !okOut {
		f.setup = fmt.Errorf("%w: interface %d alt %d", ErrNoEndpoints, iface.Number, iface.Alt)
		return f
	}
	f.in, f.out = in, out
	// The endpoint is addressed by its full bEndpointAddress. The vendor's
	// WebUSB code stores a 4-bit endpoint *number*; translating rather than
	// copying that is the trap called out in SPEC.md §10.2.
	if int(in.MaxPacketSize) > f.readLen {
		f.readLen = int(in.MaxPacketSize)
	}
	return f
}

// Interface reports the interface this flasher is bound to.
func (f *Flasher) Interface() usbfs.Interface { return f.iface }

// send writes one frame to the bulk OUT endpoint.
func (f *Flasher) send(ctx context.Context, frame []byte) error {
	if f.setup != nil {
		return f.setup
	}
	if _, err := f.dev.Transfer(ctx, f.out.Address, frame, bulkWriteTimeout); err != nil {
		return fmt.Errorf("bootloader: writing %s: %w", proto.Hex(frame), err)
	}
	return nil
}

// awaitACK reads from the bulk IN endpoint until a response whose command code
// matches want arrives, or the budget expires.
//
// Frames for other commands are discarded and the wait continues, mirroring the
// application protocol's ack_cmd_mismatch behaviour: a mismatch is not a hard
// error, it just is not the frame we are waiting for (SPEC.md §5.2). There is
// no NACK in this protocol, so the only failure mode is silence.
//
// A match additionally requires a strictly well-formed frame (DeclaredValid):
// the lenient length fallback exists for diagnostics, not for acknowledgement
// matching — see the check below.
func (f *Flasher) awaitACK(ctx context.Context, want proto.Cmd, budget time.Duration) (Response, error) {
	if f.setup != nil {
		return Response{}, f.setup
	}
	deadline := time.Now().Add(budget)
	buf := make([]byte, f.readLen)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return Response{}, fmt.Errorf("%w for %s after %s (last read error: %w)",
					ErrACKTimeout, want, budget, lastErr)
			}
			return Response{}, fmt.Errorf("%w for %s after %s", ErrACKTimeout, want, budget)
		}
		n, err := f.dev.Transfer(ctx, f.in.Address, buf, ackPollTimeout)
		if err != nil {
			// A device that has left the bus can never answer, so polling on is
			// pure waste: every further read fails identically, and across the
			// split verify budgets that used to burn up to a minute in 5 ms
			// hops. Abort at once, wrapping rather than replacing the error so
			// callers and ExitCode still see the no-device sentinel.
			//
			// Only ErrNoDevice gets this treatment. ErrStall deliberately does
			// not: whether the bootloader ever stalls its IN endpoint
			// mid-update is one of the hardware unknowns (SPEC.md §14), the
			// budget already bounds the wait, and a timeout reports the stall
			// as the last read error — whereas a transient EPROTO during
			// re-enumeration genuinely recovers, so a blanket abort on all
			// errors would be wrong in the other direction.
			if errors.Is(err, usbfs.ErrNoDevice) {
				return Response{}, fmt.Errorf("bootloader: device left the bus while waiting for %s: %w", want, err)
			}
			// Almost always the poll timing out with nothing to read. Record
			// it for diagnostics and keep waiting until the real budget ends.
			lastErr = err
			if err := f.pause(ctx, ackRetryPause); err != nil {
				return Response{}, err
			}
			continue
		}
		// A read that returns nothing, or bytes too short to be a frame, is as
		// unproductive as a failed one, so it earns the same backoff. usbfs
		// reports a zero-length bulk packet as (0, nil), not as an error, so
		// without this a device emitting them continuously spins one core at
		// ioctl rate for the whole budget — up to VerifyTimeout across the
		// verify attempts — which is exactly what ackRetryPause exists to
		// prevent on the error branch above.
		if n == 0 {
			if err := f.pause(ctx, ackRetryPause); err != nil {
				return Response{}, err
			}
			continue
		}
		resp, err := ParseResponse(buf[:n])
		if err != nil {
			lastErr = err
			if err := f.pause(ctx, ackRetryPause); err != nil {
				return Response{}, err
			}
			continue
		}
		// Match only a strictly well-formed frame: declared length within
		// [PreambleLen, n], no lenient fallback. CMD_BOOTLOADER_WRITE_CHUNK is
		// command code 0, so under the fallback an all-zero bulk packet —
		// classic line noise — parses as a WRITE_CHUNK acknowledgement
		// (declared 0 → whole-buffer length, byte[1]&0x3F == 0), and the paced
		// ACK-mode re-flash after a CRC mismatch would silently degrade into
		// the unpaced streaming whose failure it exists to recover from
		// (SPEC.md §10.5). Noise matches nothing and the wait continues;
		// well-formed frames for other commands are discarded per SPEC.md §5.2.
		if !resp.DeclaredValid || resp.Cmd != want {
			continue
		}
		// Copy out of the reusable read buffer before handing the frame back.
		raw := make([]byte, n)
		copy(raw, buf[:n])
		out, _ := ParseResponse(raw)
		return out, nil
	}
}

// Serial reads the unit's serial number.
//
// The bootloader answers the ordinary application-protocol read [0x02, 0x08]
// over raw bulk, which is how the host confirms it is about to flash the unit
// it thinks it is (SPEC.md §10.1).
func (f *Flasher) Serial(ctx context.Context) (string, error) {
	if err := f.send(ctx, SerialReadFrame()); err != nil {
		return "", err
	}
	resp, err := f.awaitACK(ctx, proto.CmdSerialNumber, f.t.serial)
	if err != nil {
		return "", fmt.Errorf("bootloader: reading serial number: %w", err)
	}
	s := proto.DecodeString(resp.Payload)
	if !proto.SerialUsable(s) {
		return "", fmt.Errorf("bootloader: serial number %q is too short to trust (payload %s)",
			s, proto.Hex(resp.Payload))
	}
	return s, nil
}

// Flash writes every page of fw to the device.
//
// Each page is split into exactly ChunksPerPage WRITE_CHUNK frames followed by
// one COMMIT_PAGE. In stream mode (ackMode false) nothing is acknowledged and
// the host paces itself with ChunkDelay and CommitDelay; in ACK mode every
// frame is waited on with proto.BootloaderACKTimeout, which is far slower but
// is what the vendor falls back to after a failed verification.
//
// Flash always ends with PostFlashDelay so the device has settled before a CRC
// is requested (POST_FLASH_DELAY_MS, SPEC.md §10.1). That wait belongs to this
// method and to nothing else: a caller that adds its own before Verify makes
// every flash wait 4 s where the spec says 2. Update sequences the two and does
// not wait again. progress may be nil.
//
// The whole image is validated before the first frame goes out. That ordering
// is the point: a page geometry rejected halfway through leaves a device with
// part of one image and part of another, which is the one outcome the CRC
// cannot warn about afterwards (see DefaultPageSize).
func (f *Flasher) Flash(ctx context.Context, fw *Firmware, ackMode bool, progress func(Progress)) error {
	if f.setup != nil {
		return f.setup
	}
	// Validate covers every geometry invariant the loop below depends on:
	// equal-length pages, a length divisible by ChunksPerPage, chunks that fit
	// one WRITE_CHUNK frame, and a page count the big-endian uint16 page id can
	// actually address (MaxPages) — without which uint16(i) would wrap and
	// rewrite the start of the image.
	if err := fw.Validate(); err != nil {
		return err
	}
	total := len(fw.Pages)
	report := func(p Progress) {
		if progress != nil {
			progress(p)
		}
	}

	for i, page := range fw.Pages {
		chunks, err := SplitPage(page)
		if err != nil {
			return fmt.Errorf("bootloader: page %d: %w", i, err)
		}
		for c, chunk := range chunks {
			frame, err := WriteChunkFrame(uint16(i), uint8(c), chunk)
			if err != nil {
				return err
			}
			if err := f.send(ctx, frame); err != nil {
				return fmt.Errorf("bootloader: page %d chunk %d: %w", i, c, err)
			}
			if ackMode {
				if _, err := f.awaitACK(ctx, proto.CmdBootloaderWriteChunk, f.t.ack); err != nil {
					return fmt.Errorf("bootloader: page %d chunk %d: %w", i, c, err)
				}
			} else if err := f.pause(ctx, f.t.chunk); err != nil {
				return err
			}
			report(Progress{Phase: PhaseChunk, Page: i, TotalPages: total, Chunk: c, TotalChunks: len(chunks)})
		}

		if err := f.send(ctx, CommitPageFrame()); err != nil {
			return fmt.Errorf("bootloader: committing page %d: %w", i, err)
		}
		if ackMode {
			if _, err := f.awaitACK(ctx, proto.CmdBootloaderCommitPage, f.t.ack); err != nil {
				return fmt.Errorf("bootloader: committing page %d: %w", i, err)
			}
		} else if err := f.pause(ctx, f.t.commit); err != nil {
			return err
		}
		report(Progress{Phase: PhaseCommit, Page: i, TotalPages: total, Chunk: -1, TotalChunks: ChunksPerPage})
	}

	report(Progress{Phase: PhaseSettle, Page: total, TotalPages: total, Chunk: -1})
	return f.pause(ctx, f.t.postFlash)
}

// Verify asks the device for the CRC of what it has flashed.
//
// The write form starts the computation and the read form collects it; both are
// sent, then the answer is awaited. The pair is retried once, VerifyRoundDelay
// apart, within an overall proto.VerifyTimeout budget.
//
// The returned byte is the device's own single-byte CRC. The algorithm is
// unknown (SPEC.md §14.12) — it can only be compared against a value shipped
// with the image, never recomputed locally.
func (f *Flasher) Verify(ctx context.Context) (crc uint8, err error) {
	if f.setup != nil {
		return 0, f.setup
	}
	deadline := time.Now().Add(f.t.verify)
	var lastErr error
	for attempt := 0; attempt < verifyAttempts; attempt++ {
		if attempt > 0 {
			if err := f.pause(ctx, f.t.verifyRound); err != nil {
				return 0, err
			}
		}
		if err := f.send(ctx, VerifyWriteFrame()); err != nil {
			return 0, err
		}
		if err := f.pause(ctx, interFrameDelay); err != nil {
			return 0, err
		}
		if err := f.send(ctx, VerifyReadFrame()); err != nil {
			return 0, err
		}
		// Split what is left of the budget across the attempts still to come,
		// so a silent first round cannot consume the whole 120 s.
		budget := time.Until(deadline) / time.Duration(verifyAttempts-attempt)
		if budget <= 0 {
			break
		}
		// Both forms of CMD_BOOTLOADER_VERIFY mask to the same command code, so
		// an acknowledgement of the *write* form — a bare [0x02, 0x82], no CRC
		// byte — satisfies awaitACK's match. Whether a real unit answers the
		// write form at all is unmeasured (SPEC.md §14.16: no unit has been put
		// into its bootloader), so one that does must not be able to spend an
		// attempt on it: keep listening inside the same round, where the CRC
		// frame the read form was sent for is still to come. Only a round that
		// ends without one moves on.
		roundEnd := time.Now().Add(budget)
		for {
			remaining := time.Until(roundEnd)
			if remaining <= 0 {
				break
			}
			resp, err := f.awaitACK(ctx, proto.CmdBootloaderVerify, remaining)
			if err != nil {
				// awaitACK already fails fast when the device leaves the bus;
				// swallowing that into a retry would spend another round pause
				// and a doomed send on a unit that is definitively gone. Only a
				// silent-but-present device earns the next attempt.
				if errors.Is(err, usbfs.ErrNoDevice) {
					return 0, fmt.Errorf("bootloader: verify: %w", err)
				}
				// A round that already saw a CRC-less answer ends in a timeout by
				// construction -- the loop keeps listening for the CRC frame until
				// the round's budget runs out -- so overwriting the cause here
				// would replace the one thing observed about the device with the
				// silence that necessarily followed it.
				if !errors.Is(lastErr, ErrVerifyNoCRC) {
					lastErr = err
				}
				break
			}
			if resp.HasCRC {
				return resp.CRC, nil
			}
			lastErr = fmt.Errorf("%w (%s)", ErrVerifyNoCRC, proto.Hex(resp.Raw))
		}
	}
	if lastErr == nil {
		lastErr = ErrACKTimeout
	}
	return 0, fmt.Errorf("bootloader: verification failed: %w", lastErr)
}

// JumpToApp sends CMD_BOOTLOAD_END, leaving the bootloader for the application
// image. The device disconnects immediately and never acknowledges, so this is
// fire-and-forget.
//
// Never call this after a CRC mismatch: an unverified image left running is a
// unit that may not come back, whereas a unit left in the bootloader can always
// be re-flashed (SPEC.md §10.5).
func (f *Flasher) JumpToApp(ctx context.Context) error {
	if err := f.send(ctx, BootloadEndFrame()); err != nil {
		// The device can vanish as it jumps, so a write error here is
		// ambiguous rather than conclusive; the caller decides what to make
		// of it.
		return fmt.Errorf("bootloader: sending CMD_BOOTLOAD_END: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Device discovery
// ---------------------------------------------------------------------------

// USB constants for the bootloader interface.
const (
	// VendorClass is the bInterfaceClass the bootloader advertises. There is
	// no MIDI in bootloader mode at all.
	VendorClass uint8 = 0xFF
	// reqTypeClassInterfaceOut is bmRequestType for a host-to-device,
	// class-specific request addressed to an interface.
	reqTypeClassInterfaceOut uint8 = 0x21
	// reqSetControlLineState is the CDC request the vendor client issues after
	// claiming. Whether the device cares is unknown (SPEC.md §14.16), so it is
	// sent best-effort and any error is ignored, exactly as the vendor does.
	reqSetControlLineState uint8 = 0x22
)

// Connect retry policy. The device is still re-enumerating after the jump out
// of application mode, so the first few attempts are expected to find nothing.
//
// Exported because Connect owns this loop and callers need to be able to see
// that, rather than guess and add a second one on top; see Connect.
const (
	// ConnectRetryWindow is how long Connect keeps retrying before giving up.
	ConnectRetryWindow = 8 * time.Second
	// ConnectRetryInterval is the pause between Connect's attempts.
	ConnectRetryInterval = 250 * time.Millisecond
)

// Connect finds the VFLEX bootloader, claims its vendor-class interface and
// returns both so a Flasher can be built over them.
//
// **Connect owns the retry loop.** It retries the whole enumerate-open-claim
// sequence itself, every ConnectRetryInterval (250 ms) for up to
// ConnectRetryWindow (8 s), and only then returns an error. It retries because
// the device disappears and comes back under a new bus address after
// CMD_JUMP_APP_TO_BOOTLOADER; the caller is expected to have already waited
// BOOTLOADER_MODE_SWITCH_DELAY_MS (4 s) before the first call.
//
// A caller must therefore *not* wrap it in a second loop. Doing so does not add
// 8 s of patience to an outer budget, it multiplies: an outer loop that retries
// for N seconds actually waits up to N+8, and every error message that quotes
// its own window then understates the real wait. One call is the whole retry
// policy; if the window is wrong it should change here. The one exception is a
// caller that needs to keep waiting for a fundamentally slower event than
// re-enumeration -- a human replugging the unit, say -- which should say so in
// its own message rather than presenting the sum as one timeout.
//
// A ctx deadline shorter than the window still wins: a cancelled context aborts
// the wait immediately.
//
// The caller owns the returned device and must Close it.
func Connect(ctx context.Context) (*usbfs.Device, usbfs.Interface, error) {
	deadline := time.Now().Add(ConnectRetryWindow)
	var lastErr error
	for {
		dev, iface, err := connectOnce(ctx)
		if err == nil {
			return dev, iface, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			break
		}
		if err := ctxwait.Sleep(ctx, ConnectRetryInterval); err != nil {
			return nil, usbfs.Interface{}, err
		}
	}
	return nil, usbfs.Interface{}, fmt.Errorf(
		"bootloader: no VFLEX bootloader interface on USB vendor 0x%04X after %s "+
			"(a unit in bootloader mode blinks its LED slowly in white): %w",
		proto.VendorID, ConnectRetryWindow, lastErr)
}

// connectOnce is a single enumeration pass.
func connectOnce(ctx context.Context) (*usbfs.Device, usbfs.Interface, error) {
	refs, err := usbfs.Enumerate(proto.VendorID)
	if err != nil {
		return nil, usbfs.Interface{}, fmt.Errorf("bootloader: enumerating USB: %w", err)
	}
	if len(refs) == 0 {
		return nil, usbfs.Interface{}, errors.New("bootloader: no device with vendor 0x37BF is attached")
	}
	var lastErr error
	for _, ref := range refs {
		dev, iface, err := claim(ctx, ref)
		if err != nil {
			lastErr = err
			continue
		}
		return dev, iface, nil
	}
	if lastErr == nil {
		lastErr = errors.New("bootloader: no vendor-class interface with both endpoints")
	}
	return nil, usbfs.Interface{}, lastErr
}

// claim opens one candidate device and claims its bootloader interface.
func claim(ctx context.Context, ref usbfs.DeviceRef) (*usbfs.Device, usbfs.Interface, error) {
	dev, err := usbfs.Open(ref)
	if err != nil {
		return nil, usbfs.Interface{}, fmt.Errorf("bootloader: opening %s: %w", ref.Path, err)
	}
	// SPEC.md §10.1 phase 2: "select configuration 1 if unset". Linux picks a
	// configuration at enumeration, so this almost always reads back as already
	// configured and writes nothing -- which is the point of reading first.
	// Selecting a configuration that is already selected is not idempotent: the
	// kernel turns it into usb_reset_configuration(), a device-wide endpoint
	// state reset, and this runs immediately before a firmware write. It also
	// has to happen before the claim below, which is what the kernel would
	// refuse it after.
	//
	// A device that genuinely is unconfigured has no interfaces to claim at all,
	// and the failure that produces says nothing about why -- which is precisely
	// the hard-to-diagnose case here, since the VFLEX's descriptors have never
	// been dumped (SPEC.md §14.3).
	//
	// ErrConfigUnknown is non-fatal: it means nothing could be read and so
	// nothing was written, leaving the device exactly as every earlier version
	// of this code found it. Guessing instead -- writing a configuration we
	// could not confirm was wrong -- is the one outcome worth avoiding.
	if _, err := dev.EnsureConfigured(ctx); err != nil && !errors.Is(err, usbfs.ErrConfigUnknown) {
		dev.Close()
		return nil, usbfs.Interface{}, fmt.Errorf("bootloader: selecting a USB configuration on %s: %w",
			ref.Path, err)
	}
	cfg, err := dev.Descriptors()
	if err != nil {
		dev.Close()
		return nil, usbfs.Interface{}, fmt.Errorf("bootloader: reading descriptors of %s: %w", ref.Path, err)
	}
	// A unit still running its application also exposes a vendor-class interface
	// (see InApplicationMode), so picking by class alone would select a healthy
	// device and let a --recover flash write to it. Refuse unless the caller
	// says otherwise; the jump path never trips this, because by the time it
	// reconnects the MIDI interface is gone.
	if !AllowApplicationMode && InApplicationMode(cfg) {
		dev.Close()
		return nil, usbfs.Interface{}, fmt.Errorf("%w: %s is running its application, not the bootloader "+
			"(it still presents a MIDI interface). A unit in the bootloader blinks its LED slowly in "+
			"white; put it there with `gflex firmware bootloader`, or flash without --recover and let "+
			"the jump happen as part of the update", ErrApplicationMode, ref.Path)
	}

	iface, ok := PickBootloaderInterface(cfg)
	if !ok {
		dev.Close()
		return nil, usbfs.Interface{}, fmt.Errorf(
			"bootloader: %s has no vendor-class (0x%02X) interface with both IN and OUT endpoints%s; "+
				"the device is probably still in application mode", ref.Path, VendorClass,
			otherConfigurationNote(cfg))
	}
	// Detaching the kernel driver is required: while a driver holds the
	// interface, usbfs will refuse the claim. It also makes any ALSA MIDI node
	// on this device disappear until we release (SPEC.md §4.2).
	if err := dev.ClaimInterface(iface.Number, true); err != nil {
		dev.Close()
		return nil, usbfs.Interface{}, fmt.Errorf("bootloader: claiming interface %d on %s: %w",
			iface.Number, ref.Path, err)
	}
	if iface.Alt != 0 {
		if err := dev.SetInterface(iface.Number, iface.Alt); err != nil {
			dev.ReleaseInterface(iface.Number)
			dev.Close()
			return nil, usbfs.Interface{}, fmt.Errorf("bootloader: selecting alt %d of interface %d on %s: %w",
				iface.Alt, iface.Number, ref.Path, err)
		}
	}
	// Best effort, error deliberately ignored: the vendor client issues this
	// CDC SET_CONTROL_LINE_STATE on a vendor-class interface that may well not
	// implement it, and proceeds regardless (SPEC.md §10.1, §14.16).
	_, _ = dev.Control(ctx, reqTypeClassInterfaceOut, reqSetControlLineState,
		1, uint16(iface.Number), nil, controlTimeout)
	return dev, iface, nil
}

// InApplicationMode reports whether cfg belongs to a unit that is running its
// application rather than sitting in the bootloader.
//
// The signal is the MIDIStreaming interface (USB Audio class 0x01, subclass
// 0x03). SPEC.md §10.1 records that a unit in bootloader mode presents no MIDI
// interface at all, so its presence means the application is running.
//
// This exists because the obvious signal does NOT work: a real VFLEX
// (APP.05.00.00, PID 0x800F, observed 2026-08-21) exposes its vendor-class
// 0xFF interface with both bulk endpoints WHILE THE APPLICATION IS RUNNING --
// interface 1.2, unbound, alongside the MIDI interface on 1.1. SPEC.md §10.1
// had assumed that interface only appears after the jump, so
// PickBootloaderInterface alone will happily select it on a perfectly healthy
// unit and `firmware flash --recover` would then stream WRITE_CHUNK frames at
// a device that never entered the bootloader.
//
// The check fails safe in both directions: MIDI present means refuse (correct
// for an application-mode unit), MIDI absent means proceed (correct for a
// bootloader). Should some future firmware keep MIDI alive in the bootloader,
// this refuses a legitimate recovery rather than permitting a dangerous flash,
// and the caller can override.
func InApplicationMode(cfg *usbfs.Config) bool {
	if cfg == nil {
		return false
	}
	for _, i := range cfg.Interfaces {
		if i.Class == usbAudioClass && i.SubClass == midiStreamingSubClass {
			return true
		}
	}
	return false
}

// The USB Audio class and its MIDIStreaming subclass, which together identify
// the interface a VFLEX speaks the application protocol over.
const (
	usbAudioClass         uint8 = 0x01
	midiStreamingSubClass uint8 = 0x03
)

// PickBootloaderInterface returns the first interface alt setting that is
// vendor-class and has both an IN and an OUT endpoint, matching the vendor
// client's selection rule (SPEC.md §10.1).
//
// Only interfaces belonging to the device's active configuration are eligible.
// usbfs hands back a descriptor blob with every configuration flattened into it,
// and bInterfaceNumber is only unique within one configuration -- so an
// interface matched out of an inactive configuration would be claimed by a
// number that means something else, or nothing, in the configuration the device
// is actually in. Device.Descriptors has usually narrowed Config.Interfaces
// already; the check is repeated here so the rule holds for any Config, and it
// is skipped when Config.Active is 0, which is the "not known, do not filter"
// value (see usbfs.Config).
//
// An interface whose own ConfigurationValue is 0 is a different case again: it
// appeared before any configuration descriptor, a malformed blob that
// usbfs.ParseDescriptors deliberately preserves ("dropping the interface would
// turn a device we can still drive into one we cannot"). It belongs to no
// inactive configuration — it is simply unattributed — so the Active filter
// must not discard it, or this function undoes exactly the salvage the parser
// performed. Such an orphan is kept as a fallback and an interface that
// provably belongs to the active configuration is preferred when both exist.
func PickBootloaderInterface(cfg *usbfs.Config) (usbfs.Interface, bool) {
	if cfg == nil {
		return usbfs.Interface{}, false
	}
	var orphan usbfs.Interface
	var haveOrphan bool
	for _, iface := range cfg.Interfaces {
		if iface.Class != VendorClass {
			continue
		}
		if _, ok := iface.In(); !ok {
			continue
		}
		if _, ok := iface.Out(); !ok {
			continue
		}
		if cfg.Active != 0 && iface.ConfigurationValue != cfg.Active {
			// See the doc comment: 0 marks an orphan from a malformed blob,
			// not membership of an inactive configuration. Remember the first
			// one, but keep scanning for an exact match to prefer.
			if iface.ConfigurationValue == 0 && !haveOrphan {
				orphan, haveOrphan = iface, true
			}
			continue
		}
		return iface, true
	}
	return orphan, haveOrphan
}

// otherConfigurationNote names any *inactive* configuration that does declare a
// vendor-class interface, for the error raised when the active one does not.
//
// Without it that failure reads identically to "this unit is not in bootloader
// mode", which on a multi-configuration device would be flatly wrong and
// unguessable: the interface is right there in the descriptors, just not in the
// configuration the device is in. It returns "" for the single-configuration
// case, so the common message is unchanged.
func otherConfigurationNote(cfg *usbfs.Config) string {
	if cfg.Active == 0 || len(cfg.Configurations) < 2 {
		return ""
	}
	var elsewhere []string
	for _, c := range cfg.Configurations {
		if c.Value == cfg.Active {
			continue
		}
		for _, iface := range c.Interfaces {
			if iface.Class != VendorClass {
				continue
			}
			elsewhere = append(elsewhere, fmt.Sprintf("%d", c.Value))
			break
		}
	}
	if len(elsewhere) == 0 {
		return fmt.Sprintf(" in configuration %d (of %d the device declares)",
			cfg.Active, len(cfg.Configurations))
	}
	return fmt.Sprintf(" in configuration %d, although configuration %s does declare one",
		cfg.Active, strings.Join(elsewhere, ", "))
}
