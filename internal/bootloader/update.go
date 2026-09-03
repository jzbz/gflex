package bootloader

import (
	"context"
	"errors"
	"fmt"

	"github.com/jzbz/gflex/internal/usbfs"
)

// UpdateOptions tunes a whole-image update.
//
// The zero value is the conservative one: stream mode, verify whenever the
// image carries a CRC, and refuse to start an image that could not be verified.
type UpdateOptions struct {
	// ExpectSerial, when non-empty, is compared against the serial the
	// bootloader reports before anything is written. A mismatch aborts. This is
	// the same invariant the PDO scan enforces: the unit you addressed must be
	// the unit you are about to change. Set it whenever the serial is known —
	// the image is fetched for one specific unit (SPEC.md §10.3), and a
	// bootloader answering with a different serial is a different unit.
	ExpectSerial string

	// ACKMode forces the slow acknowledged path from the first attempt instead
	// of only after a failed verification.
	ACKMode bool

	// CRC overrides the byte the device's verify response must equal, and marks
	// the image as verifiable even when it declared no CRC of its own. It is a
	// pointer because 0x00 is a legitimate expected CRC, so there is no spare
	// value left to mean "unset"; nil means "use whatever the image declared".
	CRC *uint8

	// SkipVerify suppresses the CRC check on an image that does carry one.
	// Verification is skipped anyway for an image with no CRC at all; this flag
	// is only for deliberately not checking one that could be checked, and it
	// then needs Force for the same reason that case does.
	SkipVerify bool

	// Force permits the update to finish by starting an image that was never
	// verified. That is the documented behaviour of the CLI's --force with a
	// raw .bin: SPEC.md §13 interlock 6 requires verification when there is a
	// CRC to verify against, and with no CRC there is nothing to check at all —
	// the algorithm is unknown and nothing host-side can compute one
	// (SPEC.md §10.2, §14.12).
	//
	// It does not, and must not, bypass a CRC *mismatch*. A device that
	// reported the wrong CRC is never told to jump, whatever the caller asks
	// for (SPEC.md §10.5).
	Force bool

	// SkipJump leaves the device in the bootloader after a successful flash,
	// instead of sending CMD_BOOTLOAD_END. An unverifiable image may be flashed
	// without Force when this is set, because nothing unverified is ever run:
	// the unit stays in the bootloader, where it is always re-flashable.
	SkipJump bool

	// Progress receives flash progress; may be nil.
	Progress func(Progress)

	// Log receives one-line human-readable status messages, including the
	// warnings that make an unverified flash explicit. It may be nil, but a
	// user-facing caller should always supply one: skipping verification is
	// reported here and nowhere else.
	Log func(string)
}

// UpdateResult reports what an update actually did. Update always returns a
// non-nil result, including alongside an error, so a caller can report how far
// the sequence got.
type UpdateResult struct {
	// Serial is what the bootloader reported, empty if it could not be read.
	Serial string
	// ExpectedCRC is the byte verification compared against: the image's, or
	// UpdateOptions.CRC when that overrode it. Meaningful only when Verifiable.
	ExpectedCRC uint8
	// Verifiable reports whether an expected CRC was available at all.
	Verifiable bool
	// CRC is the byte the device computed, valid only when CRCChecked.
	CRC uint8
	// CRCChecked reports whether verification ran and matched.
	CRCChecked bool
	// Unverified reports that the image was flashed without being verified —
	// either it carried no CRC or SkipVerify was set. It is the flag a caller
	// should surface to the user, since the flash succeeded in the sense that
	// every byte was written but nothing confirms what landed.
	Unverified bool
	// ACKModeUsed reports whether the acknowledged (slow) path was used for the
	// flash that finally succeeded.
	ACKModeUsed bool
	// Reflashed reports whether a second, acknowledged pass was needed.
	Reflashed bool
	// JumpedToApp reports whether CMD_BOOTLOAD_END was sent.
	JumpedToApp bool
}

// Update runs the bootloader half of a firmware update as a single sequence:
// confirm identity, stream the image, verify the CRC, and jump back to the
// application.
//
// This is the one entry point callers need. Flasher's individual methods stay
// exported for diagnostics and for the recovery path, which has to read a
// serial number before it knows which image to fetch — but the ordering, the
// delays and the interlocks live here so that exactly one copy of them exists.
// A second copy of this sequence in a caller is how the two drifted apart
// before: one applied POST_FLASH_DELAY_MS twice and dropped the sentinel that
// distinguishes a CRC mismatch from any other failure.
//
// It deliberately covers only phases 3 and 4 of SPEC.md §10.1. The jump *into*
// the bootloader is an application-mode MIDI command and belongs to the session
// layer (Session.JumpToBootloader), and the parameter replay that restores the
// settings a flash erases is Session.PostUpdateInit (SPEC.md §10.4). Keeping
// both out of here is what lets this package stay free of any dependency on the
// MIDI stack. The caller owns the phases either side:
//
//	before  Session.JumpToBootloader, require a disconnect within 3 s as proof
//	        the jump took, wait BOOTLOADER_MODE_SWITCH_DELAY_MS (4 s), Connect.
//	after   close the usbfs device so the interface is released, wait 4 s, wait
//	        for the unit to re-enumerate in application mode, then
//	        Session.PostUpdateInit.
//
// A caller must not add its own POST_FLASH_DELAY_MS wait before verification:
// Flash already ends with one, and a second doubles the settle time of every
// flash (SPEC.md §10.1 says 2 s, not 4).
//
// Verification policy, decided before the first byte is written:
//
//   - an image with a CRC (its own, or UpdateOptions.CRC) is always verified
//     unless SkipVerify was explicitly set;
//   - on a mismatch the image is re-flashed once in acknowledged mode, which is
//     the vendor's own recovery, and if it still does not match
//     CMD_BOOTLOAD_END is *not* sent: the unit stays in the bootloader, where
//     it is re-flashable, rather than being told to run an image we could not
//     verify (SPEC.md §10.5, §13 interlock 6). The error wraps ErrCRCMismatch;
//   - an image that cannot be verified at all (a raw .bin, no CRC anywhere) is
//     refused with ErrUnverifiable before anything is written, unless Force
//     says the caller accepts starting it, or SkipJump means it will not be
//     started. Force never applies to a mismatch.
func Update(ctx context.Context, dev *usbfs.Device, iface usbfs.Interface, fw *Firmware, opts UpdateOptions) (*UpdateResult, error) {
	return update(ctx, NewFlasher(dev, iface), fw, opts)
}

// update is Update over an already-built Flasher, so the sequencing can be
// tested without a USB device.
func update(ctx context.Context, f *Flasher, fw *Firmware, opts UpdateOptions) (*UpdateResult, error) {
	res := &UpdateResult{}
	if fw == nil {
		return res, errors.New("bootloader: no firmware image to flash")
	}
	// Page geometry is checked before anything reaches the wire, and this is
	// the only real guard on it: the device computes its CRC over what it was
	// told to write, so an image split on the wrong geometry can verify
	// perfectly well (see DefaultPageSize). A device left half-written and only
	// then rejected is the worst outcome available, so the whole image has to
	// be known flashable before the first WRITE_CHUNK.
	if err := fw.Validate(); err != nil {
		return res, err
	}
	logf := func(format string, args ...any) {
		if opts.Log != nil {
			opts.Log(fmt.Sprintf(format, args...))
		}
	}
	report := func(p Progress) {
		if opts.Progress != nil {
			opts.Progress(p)
		}
	}

	// 1. Settle the verification policy up front, so that an update which can
	//    only end in an unverified jump is refused before the flash rather than
	//    after it.
	expectCRC, crcKnown := fw.CRC, fw.CRCKnown
	if opts.CRC != nil {
		expectCRC, crcKnown = *opts.CRC, true
	}
	res.ExpectedCRC, res.Verifiable = expectCRC, crcKnown
	willVerify := crcKnown && !opts.SkipVerify
	if !willVerify {
		reason := "the image declares no CRC, so a successful flash cannot be confirmed"
		if crcKnown {
			reason = fmt.Sprintf("CRC verification was skipped at the caller's request, "+
				"although the image declares 0x%02X", expectCRC)
		}
		switch {
		case opts.SkipJump:
			// Nothing unverified will be started, and a unit left in the
			// bootloader is always re-flashable, so this needs no Force.
			logf("warning: %s; the unit will be left in bootloader mode, where it is still re-flashable", reason)
		case !opts.Force:
			return res, fmt.Errorf("%w: %s. Nothing has been written to the device. "+
				"Flashing it anyway means telling the unit to run an image that was never checked; "+
				"set Force (the CLI's --force) to accept that", ErrUnverifiable, reason)
		default:
			logf("WARNING: %s. Flashing and starting it unverified at the caller's request: "+
				"a corrupted write or a wrongly split image will NOT be detected here, and the unit "+
				"may not come back on its own (SPEC.md §10.5).", reason)
		}
	}

	// 2. Confirm we are talking to the right unit before writing anything.
	report(Progress{Phase: PhaseSerial, Chunk: -1, TotalPages: len(fw.Pages)})
	serial, err := f.Serial(ctx)
	if err != nil {
		if opts.ExpectSerial != "" {
			return res, fmt.Errorf("bootloader: cannot confirm the unit's identity before flashing: %w", err)
		}
		// Without an expected serial this is only diagnostic, so a bootloader
		// that will not answer the app-protocol read is not fatal.
		logf("warning: could not read the serial number from the bootloader: %v", err)
	} else {
		res.Serial = serial
		logf("bootloader reports serial %s", serial)
		if opts.ExpectSerial != "" && serial != opts.ExpectSerial {
			return res, fmt.Errorf("%w: expected %q, bootloader reports %q. Nothing has been written",
				ErrSerialMismatch, opts.ExpectSerial, serial)
		}
	}

	// 3. Flash, in stream mode unless the caller insists otherwise. Flash ends
	//    with POST_FLASH_DELAY_MS itself; nothing here waits again.
	ackMode := opts.ACKMode
	logf("flashing %d pages of %d bytes (%d bytes total) in %s mode",
		len(fw.Pages), fw.PageSize(), fw.TotalBytes(), modeName(ackMode))
	if err := f.Flash(ctx, fw, ackMode, opts.Progress); err != nil {
		return res, err
	}
	res.ACKModeUsed = ackMode

	// 4. Verify.
	if !willVerify {
		res.Unverified = true
	} else {
		report(Progress{Phase: PhaseVerify, Chunk: -1, TotalPages: len(fw.Pages)})
		crc, err := f.Verify(ctx)
		if err != nil {
			return res, err
		}
		res.CRC = crc
		if crc != expectCRC {
			// The vendor's own recovery: repeat the whole image with every
			// frame acknowledged, which is slower but immune to the device
			// dropping a streamed chunk. It is retried even when the first pass
			// was already acknowledged — a second attempt costs time, and the
			// alternative on a failed flash write is a unit that stays in the
			// bootloader for no reason.
			logf("CRC mismatch (device 0x%02X, image 0x%02X); re-flashing with acknowledgements", crc, expectCRC)
			res.Reflashed = true
			if err := f.Flash(ctx, fw, true, opts.Progress); err != nil {
				return res, err
			}
			res.ACKModeUsed = true
			report(Progress{Phase: PhaseVerify, Chunk: -1, TotalPages: len(fw.Pages)})
			crc, err = f.Verify(ctx)
			if err != nil {
				return res, err
			}
			res.CRC = crc
		}
		if crc != expectCRC {
			// "expects", not "the image declares": UpdateOptions.CRC may have
			// supplied or replaced the image's own value.
			return res, fmt.Errorf(
				"%w: device reports 0x%02X, the update expects 0x%02X. CMD_BOOTLOAD_END was "+
					"deliberately not sent: the unit is still in bootloader mode (slow blinking "+
					"white LED), is not bricked, and can be re-flashed",
				ErrCRCMismatch, crc, expectCRC)
		}
		res.CRCChecked = true
		logf("CRC verified: 0x%02X", crc)
	}

	// 5. Leave the bootloader. Every route to CMD_BOOTLOAD_END passes through
	//    here, and the guard below is the last one: a mismatch has already
	//    returned above, so the only way to start an unverified image is the
	//    Force path that was announced before the flash began. Should any
	//    future path reach this point having skipped verification without it,
	//    the update stops instead of starting an unchecked image.
	if opts.SkipJump {
		logf("leaving the device in bootloader mode at the caller's request")
		return res, nil
	}
	if !res.CRCChecked && !opts.Force {
		return res, fmt.Errorf("%w: refusing to send CMD_BOOTLOAD_END for an image that was not verified",
			ErrUnverifiable)
	}
	report(Progress{Phase: PhaseEnd, Chunk: -1, TotalPages: len(fw.Pages)})
	if err := f.JumpToApp(ctx); err != nil {
		// "It may already have jumped" is only true of a device that has left
		// the bus. Every other failure means the frame was not accepted while
		// the unit was still there: usbfs.Device.Transfer refuses to submit at
		// all once the context is done, and a stall or a bulkWriteTimeout
		// expiry is the same story from the other end. Reporting any of those
		// as a completed jump is how a unit that is sitting in the bootloader
		// gets described to its owner as "the update SUCCEEDED and the jump was
		// sent, do NOT re-flash" — advice that forbids the one action that
		// would fix it (SPEC.md §10.5).
		//
		// "probably" is deliberate: a jump that races its own disconnect can
		// surface as an errno usbfs does not classify as ErrNoDevice, so this
		// can name the bootloader for a unit that did leave it. That direction
		// is the safe one — the operator re-enters the bootloader and finds
		// nothing to do — and claim()'s ErrApplicationMode guard catches it.
		if !errors.Is(err, usbfs.ErrNoDevice) {
			return res, fmt.Errorf("bootloader: CMD_BOOTLOAD_END was not delivered, so the unit is "+
				"probably still in bootloader mode (slow blinking white LED) and can be re-flashed: %w", err)
		}
		logf("warning: %v (the device may already have jumped)", err)
	}
	res.JumpedToApp = true
	return res, nil
}

func modeName(ack bool) string {
	if ack {
		return "acknowledged"
	}
	return "stream"
}
