package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jzbz/gflex/internal/bootloader"
	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/session"
	"github.com/jzbz/gflex/internal/usbfs"
)

// Timings from the vendor's own update sequence (SPEC.md §10.1). These are not
// guesses: they were measured by the vendor and encoded in the shipped client.
//
// POST_FLASH_DELAY_MS is deliberately absent. It belongs to the flash itself,
// which is the bootloader package's job (Flasher.Flash ends with
// bootloader.PostFlashDelay before any CRC can be asked for). A copy here is
// how this file came to apply it twice, waiting 4 s where SPEC.md §10.1 says
// 2 s -- the general hazard of keeping a second copy of the update sequence.
const (
	// disconnectTimeout is how long the jump to the bootloader has to produce
	// a disconnect before it is judged to have failed.
	disconnectTimeout = 3 * time.Second
	// bootloaderModeSwitchDelay is BOOTLOADER_MODE_SWITCH_DELAY_MS: the settle
	// time after the device drops off the bus and before it is reopened as a
	// vendor-class device.
	bootloaderModeSwitchDelay = 4 * time.Second
	// postJumpDelay is the settle time after CMD_BOOTLOAD_END.
	postJumpDelay = 4 * time.Second
	// reenumerationTimeout is how long the device has to come back in
	// application mode after the jump.
	reenumerationTimeout = 15 * time.Second
)

// bootloaderLEDNote describes what the user is looking at while the unit is in
// bootloader mode (SPEC.md §1.1).
const bootloaderLEDNote = "the LED blinks white slowly while the unit is in bootloader mode"

// Seams for testing. All three are the real functions in every build; a test
// substitutes them to drive the CLI half of an update without hardware.
var (
	updateFirmware    = bootloader.Update
	fetchFirmware     = bootloader.Fetch
	connectBootloader = bootloader.Connect
)

func newFirmwareCommand(app *App) *cobra.Command {
	cmd := group(&cobra.Command{
		Use:   "firmware",
		Short: "Read the firmware version, enter the bootloader, or flash an image",
	})
	cmd.AddCommand(newFirmwareVersionCommand(app), newFirmwareFetchCommand(app),
		newFirmwareBootloaderCommand(app), newFirmwareFlashCommand(app))
	return cmd
}

func newFirmwareVersionCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Read the firmware version from the device",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				if app.DryRun {
					return app.dryRun(f, proto.Read(proto.CmdFirmwareVersion))
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				v, err := c.Session.FirmwareVersion(ctx)
				if err != nil {
					return fmt.Errorf("reading the firmware version: %w", err)
				}
				f.KV("fw_id", "firmware", v, v)
				// The PD capability scan is hard-gated on 5.0.0 (SPEC.md §9).
				//
				// Compared here rather than through Session.FirmwareAtLeast,
				// which starts by reading the version off the device again:
				// this command's entire body is one read, and asking twice
				// doubled both its device time and its worst-case wait on a
				// unit that has stopped answering. The comparison itself needs
				// no device -- session.VersionAtLeast is a pure function of the
				// string -- so the one read above answers both what to print
				// and whether to warn, which is what --dry-run has always
				// advertised above with its single Read frame.
				//
				// It also stops a failure being swallowed: the second read's
				// error was discarded (`err == nil && !ok`), so a dropped
				// response -- routine in a protocol with no NACK (SPEC.md §5.2)
				// -- silently omitted the note from a unit that had already
				// told us its version was too old.
				if !session.VersionAtLeast(v, 5, 0, 0) {
					f.Note("")
					f.Note("%s", msgFirmwareTooOld)
				}
				return nil
			})
		},
	}
}

// newFirmwareFetchCommand downloads the image the vendor service holds for this
// unit and reports what it is, without flashing anything.
//
// `flash --fetch` exists already, but it is all or nothing: it downloads and
// then rewrites the device in the same breath, and it refuses --dry-run because
// fetching needs the serial and a dry run must not read from the device
// (SPEC.md §13.8). That leaves no way to answer the question anyone sensible
// asks first -- what would you put on my device? -- without agreeing to the one
// irreversible operation this tool performs.
//
// So this command reads exactly one thing from the unit, its serial number, and
// otherwise only talks to the network. It writes nothing to the device, sends no
// bootloader frame, and cannot leave the unit anywhere it was not already.
func newFirmwareFetchCommand(app *App) *cobra.Command {
	var (
		wsURL   string
		timeout time.Duration
		outPath string
		raw     bool
	)
	cmd := &cobra.Command{
		Use:   "fetch",
		Short: "Download this unit's firmware image and report it, without flashing",
		Long: "fetch reads the unit's serial number, asks the vendor service for the image it\n" +
			"holds for that serial, and prints what came back: version, page geometry and the\n" +
			"expected CRC.\n\n" +
			"Nothing is written to the device. The only device access is the serial read, which\n" +
			"is what the service is keyed on -- note that it therefore leaves this unit's serial\n" +
			"number with the vendor, exactly as the vendor's own app does.\n\n" +
			"With -o the image is saved in a form `firmware flash <file>` can read back, so an\n" +
			"update can be inspected first, applied later, and re-applied without the network if\n" +
			"a flash has to be retried.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				serial, err := app.readSerialQuietly(ctx, f)
				if err != nil {
					return err
				}
				f.KV("serial_num", "serial", serial, serial)
				if raw {
					// The unparsed path exists because the parsed one can fail
					// on a payload that is perfectly fine -- it is the vendor's
					// shape, not ours, and SPEC.md §10.3 describes it from a
					// single observation. When it does, the bytes are the only
					// thing that makes the difference diagnosable.
					msg, err := bootloader.FetchRaw(ctx, wsURL, serial, timeout)
					if err != nil {
						return fmt.Errorf("fetching the image for serial %s: %w", serial, err)
					}
					f.KV("bytes", "payload", len(msg), fmt.Sprintf("%d bytes (unparsed)", len(msg)))
					if outPath == "" {
						return codedf(ExitUsage, "--raw needs -o: the payload is not something to print")
					}
					if err := os.WriteFile(outPath, msg, 0o644); err != nil {
						return fmt.Errorf("writing %s: %w", outPath, err)
					}
					f.KV("path", "saved", outPath, outPath)
					return nil
				}
				fw, err := bootloader.Fetch(ctx, wsURL, serial, timeout)
				if err != nil {
					return fmt.Errorf("fetching the image for serial %s: %w", serial, err)
				}
				return app.reportFetched(f, fw, outPath)
			})
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&wsURL, "ws-url", bootloader.DefaultWSURL,
		"WebSocket endpoint to ask; must be wss:// or https:// (see `firmware flash --help`)")
	fl.DurationVar(&timeout, "fetch-timeout", bootloader.DefaultFetchTimeout,
		"budget for the whole download (SPEC.md §10.3)")
	fl.StringVarP(&outPath, "out", "o", "",
		"save the image here, in a form `firmware flash <file>` reads back")
	fl.BoolVar(&raw, "raw", false,
		"save the service's reply verbatim instead of parsing it; needs -o, and is for diagnosing a payload this tool cannot read")
	return cmd
}

// reportFetched prints what the service returned and optionally saves it.
func (a *App) reportFetched(f Formatter, fw *bootloader.Firmware, outPath string) error {
	f.KV("fw_id", "image version", fw.Version, orUnknown(fw.Version))
	f.KV("pages", "pages", len(fw.Pages), fmt.Sprintf("%d", len(fw.Pages)))
	f.KV("page_size", "page size", fw.PageSize(), fmt.Sprintf("%d bytes", fw.PageSize()))
	f.KV("total_bytes", "total", fw.TotalBytes(), fmt.Sprintf("%d bytes", fw.TotalBytes()))
	if fw.CRCKnown {
		f.KV("crc", "expected crc", fw.CRC, fmt.Sprintf("0x%02x", fw.CRC))
	} else {
		// An image with no CRC cannot be verified after flashing, and SPEC.md
		// §10.5 is explicit that an unverified image is the one thing never to
		// jump back into. Say so here rather than at the point of no return.
		f.KV("crc", "expected crc", nil, "none -- this image cannot be verified after flashing")
	}
	if outPath == "" {
		return nil
	}
	data, err := marshalFirmware(fw)
	if err != nil {
		return err
	}
	// 0o644 rather than 0o600: a firmware image is not a secret, and a file the
	// user cannot read back with ordinary tooling defeats the point of saving it.
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}
	f.KV("path", "saved", outPath, outPath)
	return nil
}

// marshalFirmware renders a fetched image in the JSON shape ParseImage accepts,
// so that what is saved is something the flash path can actually read back.
//
// The pages are written as an array of arrays, which is the shape that carries
// its own page split -- writing a flat byte list instead would discard the
// geometry and leave the reader guessing at --page-size, on the one path where
// a wrong split can flash and even verify cleanly (SPEC.md §10.2).
func marshalFirmware(fw *bootloader.Firmware) ([]byte, error) {
	doc := map[string]any{
		"app_version": fw.Version,
		"page_size":   fw.PageSize(),
		"app_bin":     fw.Pages,
	}
	if fw.CRCKnown {
		doc["crc"] = fw.CRC
	}
	data, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return nil, fmt.Errorf("rendering the image: %w", err)
	}
	return append(data, '\n'), nil
}

func orUnknown(s string) string {
	if s == "" {
		return "(the image carried no version string)"
	}
	return s
}

func newFirmwareBootloaderCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "bootloader",
		Short: "Put the device into bootloader mode",
		Long: "bootloader sends CMD_JUMP_APP_TO_BOOTLOADER and the device disconnects\n" +
			"immediately. There is no acknowledgement -- the disconnect is the only evidence\n" +
			"the jump took, and on --transport usb even that is unavailable.\n\n" +
			"In bootloader mode there is no MIDI interface at all, so no other command in this\n" +
			"tool will find the device until it is flashed and jumped back, or power-cycled.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				if app.DryRun {
					return app.dryRun(f, proto.Read(proto.CmdJumpAppToBootloader))
				}
				if err := app.confirm(ctx, "Put the device into bootloader mode? It will disconnect and stop "+
					"responding to every command except a firmware flash."); err != nil {
					return err
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				jerr := c.Session.JumpToBootloader(ctx)
				// The device leaves the bus as it jumps and there is nothing
				// further to read from it, so the port is released here rather
				// than on the way out of the function: the presence check below
				// wants nothing of ours holding the node.
				c.Close()
				if jerr != nil {
					return fmt.Errorf("jumping to the bootloader: %w", jerr)
				}
				f.KV("bootloader", "mode", "bootloader", "jump sent; "+bootloaderLEDNote)
				app.reportJumpEvidence(ctx, f)
				return nil
			})
		},
	}
}

// midiPresenceMeaningful reports whether the presence of a VFLEX ALSA rawmidi
// node tracks the device rather than this process.
//
// waitForDevice watches for that node. Under --transport usb the node is not in
// play at all: usbmidi.Open claims the MIDI interface with a kernel-driver
// detach (SPEC.md §4.2), so snd-usb-audio is unbound and the node is gone
// whether or not the device jumped -- and on a headless box the driver may
// never have been loaded. Its absence there proves nothing, and its presence
// proves nothing either.
func (a *App) midiPresenceMeaningful() bool { return a.Transport != transportUSB }

// reportJumpEvidence says what is actually known about a bare `firmware
// bootloader` jump, which has no later step to fall back on.
//
// The jump is unacknowledged, so a disconnect is the only evidence there is
// (SPEC.md §10.1). Where that evidence cannot be gathered the user is told so
// outright: printing nothing would leave a successful-looking line implying a
// confirmation that never happened.
func (a *App) reportJumpEvidence(ctx context.Context, f Formatter) {
	if !a.midiPresenceMeaningful() {
		f.Diag("note: the jump could NOT be confirmed on --transport %s. The only proof available "+
			"is the device leaving the bus, and this transport detaches the kernel MIDI driver "+
			"itself, so the port is absent either way. Check the LED instead: %s.",
			a.Transport, bootloaderLEDNote)
		return
	}
	if err := waitForDevice(ctx, false, disconnectTimeout); err != nil {
		f.Diag("warning: the device was still visible after %s; the jump may not have taken",
			disconnectTimeout)
	}
}

func newFirmwareFlashCommand(app *App) *cobra.Command {
	var (
		recoverMode  bool
		fetch        bool
		wsURL        string
		fetchTimeout time.Duration
		force        bool
		crcArg       string
		ackFirst     bool
		pageSizeArg  string
	)
	cmd := &cobra.Command{
		Use:   "flash [file]",
		Short: "Flash a firmware image",
		Long: "flash runs the whole update sequence of SPEC.md §10.1:\n\n" +
			"  1. read the serial number in application mode\n" +
			"  2. load the image (a local file, or --fetch to pull one for this serial)\n" +
			"  3. jump to the bootloader and wait for the device to leave the bus\n" +
			"  4. reopen it as a vendor-class device and confirm the serial still matches\n" +
			"  5. stream every page, then verify the CRC\n" +
			"  6. only on a matching CRC, jump back to the application\n" +
			"  7. replay the settings a flash erases\n\n" +
			"If verification fails the device is deliberately left in bootloader mode: it is\n" +
			"re-flashable, not bricked. Use --recover to pick up from there.\n\n" +
			"A raw .bin carries no page geometry of its own and is split into --page-size byte\n" +
			"pages (512 by default); a JSON image and a --fetch download carry their own page\n" +
			"split, so --page-size cannot be combined with either.\n\n" +
			"This needs --yes, or an interactive confirmation (SPEC.md §13.6).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				path := ""
				if len(args) == 1 {
					path = args[0]
				}
				if path == "" && !fetch {
					return codedf(ExitUsage, "give a firmware file, or --fetch to pull one from the vendor service")
				}
				if path != "" && fetch {
					return codedf(ExitUsage, "give a firmware file or --fetch, not both")
				}
				// -1 is "no expected CRC given", which is what the image's
				// own value is checked against when it has one.
				crc := -1
				if strings.TrimSpace(crcArg) != "" {
					v, err := parseCRCByte(crcArg)
					if err != nil {
						return err
					}
					crc = v
				}
				if fetchTimeout <= 0 {
					return codedf(ExitUsage, "--fetch-timeout must be positive, got %s", fetchTimeout)
				}
				pageSize64, err := parseDecimalInt(pageSizeArg, "--page-size", "a 32-bit page size", 32)
				if err != nil {
					return err
				}
				pageSize := int(pageSize64)
				// The library treats a page size <= 0 as "unset, use the
				// default" so that LoadOptions has a workable zero value. That
				// is right for the zero value and wrong for an explicit
				// negative: silently substituting 512 for a stated geometry,
				// on the one path where wrong geometry can flash and even
				// verify cleanly (SPEC.md §10.2, §14.12), is not a fallback
				// anyone asked for. The library refuses a negative too, on its
				// own behalf; catching it here as well is deliberate defence in
				// depth -- this check runs before any device is opened, and it
				// answers with a usage exit code and the flag's own name rather
				// than a load error surfaced from three layers down.
				if pageSize < 0 {
					return codedf(ExitUsage, "--page-size must be positive, got %d (0 or unset means the "+
						"%d-byte default)", pageSize, bootloader.DefaultPageSize)
				}
				// A fetched image is the vendor's JSON payload and carries its
				// own page split (SPEC.md §10.3); --page-size would be
				// silently ignored, and a silently ignored geometry flag on
				// the flash path is a usage error, not a shrug.
				if pageSize != 0 && fetch {
					return codedf(ExitUsage, "--page-size splits a raw .bin; a --fetch image carries "+
						"its own page split, so the flag would be ignored -- drop one of the two")
				}
				return app.runFlash(ctx, f, flashOpts{
					path:         path,
					recover:      recoverMode,
					fetch:        fetch,
					wsURL:        wsURL,
					fetchTimeout: fetchTimeout,
					force:        force,
					crc:          crc,
					ackFirst:     ackFirst,
					pageSize:     pageSize,
				})
			})
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&recoverMode, "recover", false,
		"skip the jump and talk straight to a unit already in bootloader mode (slow-blinking white LED)")
	fl.BoolVar(&fetch, "fetch", false, "fetch the image for this unit's serial from the vendor service")
	// The TLS requirement belongs in the help rather than only in the dial
	// error: the image and the CRC it is checked against arrive in the same
	// document, so a cleartext fetch authenticates neither (SPEC.md §10.3).
	// Someone pointing this at a lab endpoint should learn the spelling of the
	// downgrade before the refusal, not from it.
	fl.StringVar(&wsURL, "ws-url", bootloader.DefaultWSURL,
		"WebSocket endpoint used by --fetch; must be wss:// or https:// -- a cleartext endpoint is "+
			"refused unless its URL says ws+insecure:// in full")
	// --timeout is the per-command response timeout for the MIDI protocol and
	// has no bearing on an HTTP/WebSocket download; using it here bounded a
	// whole firmware download by 5 s. The budget SPEC.md §10.3 records for the
	// fetch is 15 s, which is bootloader.DefaultFetchTimeout.
	fl.DurationVar(&fetchTimeout, "fetch-timeout", bootloader.DefaultFetchTimeout,
		"budget for the whole --fetch download (SPEC.md §10.3); --timeout bounds MIDI commands, not this")
	fl.BoolVar(&force, "force", false, "flash an image that carries no CRC, skipping verification")
	// A string parsed by parseCRCByte rather than IntVar, for the reason given
	// there: a CRC is compared rather than applied, so a misread value fails
	// silently by matching for the wrong reason.
	fl.StringVar(&crcArg, "crc", "", "expected CRC byte, when the image does not carry one; "+
		"decimal, or hex with an explicit 0x prefix")
	fl.BoolVar(&ackFirst, "ack-mode", false, "stream in acknowledged mode from the start (slower, more robust)")
	// A string parsed by parseDecimalInt rather than IntVar, for the reason
	// given there: pflag parses an integer flag with base 0, so `--page-size
	// 0200` would be read as octal 128 rather than 200 -- and both of those are
	// geometries the loader accepts, so nothing downstream can catch the
	// substitution. This is the one flag in the tool whose wrong value can
	// flash *and verify* cleanly (SPEC.md §10.2, §14.12), which makes it the
	// last one that may quietly mean a different number than the one typed.
	fl.StringVar(&pageSizeArg, "page-size", "0", fmt.Sprintf(
		"split a raw .bin into flash pages of this many bytes, plain decimal (0 means the %d-byte "+
			"default, which is an assumption about the part, not a value read from it); set it to "+
			"the part's real page size -- a wrongly split raw image can flash and even verify "+
			"cleanly (SPEC.md §10.2). Not for JSON images or --fetch, which carry their own split",
		bootloader.DefaultPageSize))
	return cmd
}

type flashOpts struct {
	path         string
	recover      bool
	fetch        bool
	wsURL        string
	fetchTimeout time.Duration
	force        bool
	crc          int // -1 when not overridden
	ackFirst     bool
	pageSize     int // 0 when not overridden; raw .bin only
}

func (a *App) runFlash(ctx context.Context, f Formatter, o flashOpts) error {
	var (
		appSerial  string
		appVersion string // "" when it could not be read, or under --recover
		fw         *bootloader.Firmware
		err        error
	)

	// --dry-run must not touch the device at all, so it is handled before
	// anything is opened. That rules out the two modes whose first step is to
	// read the serial number off the hardware.
	if a.DryRun {
		switch {
		case o.recover:
			return codedf(ExitUsage, "--dry-run cannot be combined with --recover: "+
				"there is nothing to show until the bootloader has been opened")
		case o.fetch:
			return codedf(ExitUsage, "--dry-run cannot be combined with --fetch: "+
				"fetching an image needs the serial number, which means reading from the device")
		}
		fw, err = a.loadFirmware(ctx, o, "")
		if err != nil {
			return err
		}
		if err := a.applyDryRun(f, CheckFlash(len(fw.Pages), fw.Version, fw.CRCKnown, o.force)); err != nil {
			return err
		}
		return a.dryRunFlash(f, fw)
	}

	// --- phase 1: application mode ------------------------------------------
	if !o.recover {
		c, cerr := a.connect(ctx, f)
		if cerr != nil {
			return cerr
		}
		appSerial, err = c.Session.SerialNumber(ctx)
		if err != nil {
			c.Close()
			return fmt.Errorf("reading the serial number: %w", err)
		}
		f.Diag("device serial %s", appSerial)

		// The version running *before* the flash, read here because this is the
		// only place it can still be asked for: after the update the old image
		// is gone. It decides one thing -- whether SPEC.md §10.4's vlimit
		// rewrite is forced on a major 4 -> 5 jump (see crossesToMajor5) -- and
		// nothing else, so a failure to read it is a diagnostic, not an abort.
		// A read is all this costs; nothing is written.
		if v, verr := c.Session.FirmwareVersion(ctx); verr != nil {
			f.Diag("warning: could not read the firmware version before the flash: %v", verr)
		} else {
			appVersion = v
			f.Diag("device firmware %s", appVersion)
		}

		fw, err = a.loadFirmware(ctx, o, appSerial)
		if err != nil {
			c.Close()
			return err
		}
		if err := a.apply(ctx, f, CheckFlash(len(fw.Pages), fw.Version, fw.CRCKnown, o.force)); err != nil {
			c.Close()
			return err
		}

		f.Diag("jumping to the bootloader...")
		jerr := c.Session.JumpToBootloader(ctx)
		// Released before the presence check for the same reason as in the
		// bare `firmware bootloader` command: the device is on its way off the
		// bus and nothing more can be read from it.
		c.Close()
		if jerr != nil {
			return fmt.Errorf("jumping to the bootloader: %w", jerr)
		}
		if err := a.confirmJump(ctx, f, appSerial); err != nil {
			return err
		}
		if err := sleepCtx(ctx, bootloaderModeSwitchDelay); err != nil {
			return err
		}
	}

	// --- phase 2: bootloader mode -------------------------------------------
	dev, iface, err := openBootloaderInterface(ctx, f)
	if err != nil {
		return err
	}
	// Deferred, and not closed the moment the flash is over the way the MIDI
	// port is above. bootloader.Update's caller contract lists closing the
	// usbfs device first among the steps that follow it, and this is the one
	// caller that does not do it there; the reason is that here there is
	// nothing for an early release to unblock. The MIDI port is released early
	// because the presence check that follows watches an ALSA node this process
	// would otherwise be holding. Neither half of that applies to this handle:
	// the bootloader interface is vendor-class with no kernel driver bound
	// (usbfs.Device.ClaimInterface says so), so the release owes the system no
	// rebind, and CMD_BOOTLOAD_END resets the unit, which comes back at a fresh
	// bus address -- this fd names a device instance that has already left the
	// bus and cannot hold the new node against snd-usb-audio. Holding it to the
	// end instead means one release point covering every early return between
	// here and the end of phase 5.
	//
	// That last step is reasoning about how usbfs behaves rather than something
	// measured -- SPEC.md §14.16 (bootloader re-enumeration) is still open --
	// so if a flash is ever seen to stall in phase 4 waiting for the unit to
	// come back, moving the release to just after runUpdate is the first thing
	// to try. Device.Close is not idempotent (it re-closes the *os.File), so
	// that change needs a once-guard rather than a second bare call.
	defer dev.Close()

	expectSerial := appSerial
	if expectSerial == "" {
		// No identity yet: --recover skipped application mode entirely, and a
		// unit that answered the serial read with an empty payload leaves us
		// here too. Either way it has to come from the bootloader, because an
		// update given no expected serial enforces no match at all, and
		// flashing a unit that was never identified is precisely what the
		// serial invariant exists to prevent (SPEC.md §10.1). This one read is
		// the only bootloader traffic issued from here; the update re-reads the
		// serial and enforces the match before a single page is written.
		blSerial, serr := bootloader.NewFlasher(dev, iface).Serial(ctx)
		if serr != nil {
			return fmt.Errorf("reading the serial number from the bootloader: %w", serr)
		}
		f.Diag("bootloader serial %s", blSerial)
		expectSerial = blSerial
	}
	if fw == nil {
		// --recover: the image can only be chosen now, because --fetch asks the
		// vendor service for the one belonging to this serial (SPEC.md §10.3)
		// and the confirmation prompt names what is about to be flashed.
		fw, err = a.loadFirmware(ctx, o, expectSerial)
		if err != nil {
			return err
		}
		if err := a.apply(ctx, f, CheckFlash(len(fw.Pages), fw.Version, fw.CRCKnown, o.force)); err != nil {
			return err
		}
	}

	// --- phase 3: flash, verify, and jump back ------------------------------
	res, err := a.runUpdate(ctx, f, dev, iface, fw, o, expectSerial)
	if err != nil {
		return err
	}
	if res.CRCChecked {
		f.KV("crc", "crc", res.CRC, fmt.Sprintf("0x%02x (verified)", res.CRC))
	}
	serial := res.Serial
	if serial == "" {
		serial = expectSerial
	}

	// --- phase 4: wait for the application image to come back ---------------
	// From here on the update itself is over: the CRC verified (or the
	// unverified start was explicitly forced) and CMD_BOOTLOAD_END was sent,
	// so every error return must go through postFlashFailure -- a bare error
	// here reads as a failed update, discards the buffered crc line (Flush
	// runs only on success), and sends someone to re-flash firmware that is
	// fine.
	if err := a.awaitApplicationReturn(ctx, f, res); err != nil {
		return err
	}

	// --- phase 5: restore the settings a flash erases -----------------------
	c, err := a.connect(ctx, f)
	if err != nil {
		// The flash is already over and it worked; only the replay is lost. Say
		// which is which, or "reconnecting after the update: no device" reads as
		// a failed update and sends someone to flash it again.
		return a.postFlashFailure(f, res, "reconnecting to the unit", err)
	}
	defer c.Close()

	// Read the new version before the replay rather than after it: the replay
	// needs it. It is an ordinary read and changes nothing, so it does not
	// disturb the §10.4 sequence that follows.
	version, verr := c.Session.FirmwareVersion(ctx)
	if verr != nil {
		f.Diag("warning: could not read the firmware version back: %v", verr)
	}
	forceVLimit := decideForcedVLimit(f, appVersion, version)

	rep := replaySettings(ctx, f, c.Session, forceVLimit)

	f.KV("fw_id", "firmware", version, version)
	f.KV("serial_num", "serial", serial, serial)
	f.KV("restored", "restored settings", rep.restored, fmt.Sprintf("%d", len(rep.restored)))
	if len(rep.failed) == 0 {
		// JSON-only: an empty list keeps the object's shape stable across runs,
		// while the human block stays quiet when there is nothing to report.
		f.KV("not_restored", "", rep.failed, "")
	} else {
		f.KV("not_restored", "settings NOT restored", rep.failed, fmt.Sprintf("%d", len(rep.failed)))
	}
	for _, line := range rep.restored {
		f.Note("  %s", line)
	}
	reportReplayIncomplete(f, rep, res)
	// Deliberately nil, and not a failure exit: the flash -- what this command
	// exists to do -- succeeded, the unit is running the new image, and the
	// settings that did not come back are named above along with the commands
	// that set them. Returning an error here would also skip Formatter.Flush,
	// throwing away the version, the serial and the JSON object, and would push
	// a user whose firmware is fine towards re-flashing it.
	return nil
}

// replaySettings runs the mandatory post-update sequence of SPEC.md §10.4 and
// classifies what it narrated, echoing each line as it happens so a slow replay
// is visibly making progress.
//
// forceVLimit comes from the version comparison in decideForcedVLimit; this is
// the layer that holds both versions, which is why the decision is made here and
// not inside the session (see Session.PostUpdateInitForce).
func replaySettings(ctx context.Context, f Formatter, s *session.Session, forceVLimit bool) replayReport {
	rep := replayReport{restored: []string{}, failed: []string{}}
	if err := s.PostUpdateInitForce(ctx, forceVLimit, func(line string) {
		rep.add(line)
		f.Diag("  %s", line)
	}); err != nil {
		// The only error PostUpdateInitForce returns is a cancelled or expired
		// context; every per-step failure comes through the log above and the
		// sequence carries on regardless (SPEC.md §10.4).
		rep.cut = err
		f.Diag("warning: the post-update sequence reported: %v", err)
	}
	return rep
}

// replayReport splits the lines session.PostUpdateInitForce narrates its replay
// with into the steps that took and the steps that did not.
//
// The distinction is the whole point. By the time the replay runs the flash is
// over: the image was written, verified against the device's own CRC, and the
// unit was told to jump into it. A replay step that fails leaves one setting at
// whatever the flash left it as -- it does not mean the firmware is bad -- and
// the two must never be reported as one undifferentiated failure, because the
// actions they call for are opposite. Re-flashing a unit whose firmware is
// already fine is the more dangerous of the two operations by a wide margin
// (SPEC.md §10, §13.6), and a generic "the update failed" is exactly what sends
// someone to do it. Counting the failures in with the successes, which is what
// a single list of log lines did, hides the same thing more quietly: "restored
// settings: 7" reads as seven settings restored whether or not three of the
// seven say "failed".
type replayReport struct {
	restored []string // steps that took, as the session worded them
	failed   []string // steps that did not
	cut      error    // non-nil when the sequence was cut short (context ended)
}

// add classifies one line of replay narration.
//
// The strings are the only channel there is: the replay is error-tolerant by
// design, so a failed step is reported through the log callback and the
// sequence continues, and the returned error covers a cancelled context and
// nothing else (SPEC.md §10.4). session.PostUpdateInitForce emits exactly two
// shapes per step, "<what> ok" and "<what> failed: <err>"; the vlimit read it
// opens with narrates itself in prose and is neither, so it is neither counted
// as restored nor reported as lost. Matching on strings across a package
// boundary is unattractive, and the alternative -- reporting a write that
// failed as a setting that was restored -- is worse. It cannot drift
// unnoticed: TestPartialReplayIsReportedAsPartial drives the real session and
// fails if the wording moves.
func (r *replayReport) add(line string) {
	switch {
	case strings.HasSuffix(line, " ok"):
		r.restored = append(r.restored, line)
	case strings.Contains(line, " failed: "):
		r.failed = append(r.failed, line)
	}
}

// incomplete reports whether anything the §10.4 replay was meant to restore was
// left unrestored.
func (r replayReport) incomplete() bool { return len(r.failed) > 0 || r.cut != nil }

// reportReplayIncomplete says what a partly failed replay does and does not
// mean, and is silent when everything took.
//
// It writes through Diag, so the wording reaches the user on stderr in both
// output modes and immediately, rather than being buffered into a result block
// that a later failure could keep from ever being flushed.
func reportReplayIncomplete(f Formatter, rep replayReport, res *bootloader.UpdateResult) {
	if !rep.incomplete() {
		return
	}
	f.Diag("")
	f.Diag("the firmware update itself SUCCEEDED -- %s, and the unit came back running it.",
		flashOutcomePhrase(res))
	if rep.cut != nil {
		f.Diag("what did not finish is the replay of the settings a flash erases (SPEC.md §10.4): %v", rep.cut)
	}
	if len(rep.failed) > 0 {
		f.Diag("what did not take is part of the replay of the settings a flash erases (SPEC.md §10.4):")
		for _, line := range rep.failed {
			f.Diag("  %s", line)
		}
	}
	f.Diag("each line names the setting and the value the replay wanted. Do NOT re-flash for this;")
	f.Diag("write them yourself -- `gflex vlimit set --low <mV> --high <mV>`, `gflex current set <mA>`,")
	f.Diag("`gflex tolerance set --nominal <mV>`, `gflex calibrate adc --offset <n> --scale <n>`,")
	f.Diag("`gflex authlock set <level>` -- and check the result with `gflex info --all`.")
}

// flashOutcomePhrase states in one clause what the flash actually established.
//
// The claim has to match what was checked. --force on an image that carries no
// CRC flashes without verifying anything (SPEC.md §10.2: the algorithm is
// unknown and nothing host-side can compute one), and promising a verified
// image there would be a lie in the one direction that matters. Every message
// that has to separate "the firmware is fine" from "a later step failed" takes
// its wording from here so the two cannot drift apart.
func flashOutcomePhrase(res *bootloader.UpdateResult) string {
	if res == nil || !res.CRCChecked {
		return "the image was written, but nothing verified it: it declared no CRC"
	}
	return "the image was written and the device's own CRC verified it"
}

// decideForcedVLimit reports whether the vlimit pair must be rewritten
// unconditionally after this flash, and says so on stderr either way when the
// answer is not obvious from the versions alone.
func decideForcedVLimit(f Formatter, before, after string) bool {
	if crossesToMajor5(before, after) {
		f.Diag("firmware %s -> %s crosses the major 4 -> 5 boundary: rewriting the voltage-limit "+
			"window to the %d/%d defaults, as SPEC.md §10.4 requires",
			before, after, proto.DefaultVLimitLowMv, proto.DefaultVLimitHighMv)
		return true
	}
	// Name the one case where the rule could have applied and could not be
	// evaluated, rather than leaving the user to assume that it was.
	if _, ok := majorVersion(before); ok {
		return false
	}
	if maj, ok := majorVersion(after); ok && maj >= 5 {
		f.Diag("note: the version this unit ran before the flash is unknown -- it is never read " +
			"under --recover, and a failed read leaves it unknown too -- so the vlimit rewrite " +
			"SPEC.md §10.4 performs on a major 4 -> 5 jump could not be evaluated. The window was " +
			"still rewritten if it read back erased or implausible; check it with `gflex vlimit get`.")
	}
	return false
}

// crossesToMajor5 reports whether an update moved the unit across the major
// version boundary that SPEC.md §10.4 singles out: "setVLimit(3300, 48000) --
// only if the read-back was invalid, or on a major 4 -> 5 jump".
//
// Both versions must be readable and must parse, or this is false. That is the
// deliberate direction of the uncertainty, and the reasoning runs both ways:
//
//   - Forcing when we should not have replaces a window the user chose with the
//     3300/48000 default, which is the widest window there is. Widening the
//     guard rail is the direction that ends with 20 V on a 5 V pedal, and it is
//     why SPEC.md §13.3 has `vlimit set` confirm a widening at all --
//     interactively, or with --yes when there is no terminal. (There is no
//     global --force flag: SPEC.md §11, §13's preamble. The forcing named in
//     this comment is session.PostUpdateInitForce's forceVLimit argument to the
//     §10.4 replay, decided here rather than by the user.) Doing it silently,
//     on a guess about a version string, is not defensible.
//   - Not forcing when we should have is bounded by the check that is always
//     applied: PostUpdateInitForce reads the window back first and rewrites the
//     defaults anyway whenever the pair is missing or implausible
//     (session.VLimitPlausible). The force flag only decides the case where a
//     plausible-looking window survived the flash, and leaving the user's own
//     window in place there is the conservative outcome.
//
// So: unreadable or unparseable on either side means no forced rewrite, and
// decideForcedVLimit says so out loud when the rule might have applied.
//
// The test is the boundary rather than the literal pair 4 -> 5: an image that
// takes a unit from 4.x to 6.x has crossed it just the same, and a downgrade
// has not crossed it at all.
func crossesToMajor5(before, after string) bool {
	pre, ok := majorVersion(before)
	if !ok {
		return false
	}
	post, ok := majorVersion(after)
	if !ok {
		return false
	}
	return pre < 5 && post >= 5
}

// majorVersion extracts the leading numeric component of a device version
// string, reporting false when there is none to extract.
//
// session.VersionComponents is the vendor's own parse (SPEC.md §10.3) and is
// used rather than a second one here: it yields no components at all for an
// empty or non-numeric string, which is exactly the "unknown" this needs and
// must not be confused with a genuine major 0.
func majorVersion(v string) (int, bool) {
	c := session.VersionComponents(v)
	if len(c) == 0 {
		return 0, false
	}
	return c[0], true
}

// runUpdate hands phases 3 and 4 of SPEC.md §10.1 -- stream every page, verify
// the CRC, jump back to the application -- to the bootloader package.
//
// Every rule of that sequence lives there: the single POST_FLASH_DELAY_MS
// settle before a CRC can be requested, the whole-image retry in acknowledged
// mode after a mismatch, and the interlock that withholds CMD_BOOTLOAD_END when
// the CRC still does not match (SPEC.md §10.1, §10.5). This file used to carry
// a second implementation of all three, and the two drifted: the copy here
// waited POST_FLASH_DELAY again on top of the one Flasher.Flash already
// performs (4 s against the specified 2 s) and compared the CRC itself, so
// bootloader.ErrCRCMismatch could never reach a caller. What stays here is what
// is genuinely CLI-side: progress on stderr, and the wording of a failure.
func (a *App) runUpdate(ctx context.Context, f Formatter, dev *usbfs.Device, iface usbfs.Interface,
	fw *bootloader.Firmware, o flashOpts, expectSerial string,
) (*bootloader.UpdateResult, error) {
	pw := a.newProgressWriter()
	opts := bootloader.UpdateOptions{
		ExpectSerial: expectSerial,
		ACKMode:      o.ackFirst,
		// Force carries --force through to the bootloader's own interlock.
		// Without this mapping the flag stopped at CheckFlash: a raw .bin with
		// --force passed the CLI check, the unit was jumped into the
		// bootloader, and bootloader.update then refused with ErrUnverifiable
		// -- telling the user to set the very flag they had set, with the unit
		// stranded in bootloader mode. Force only permits *starting* an image
		// that carries no CRC (SPEC.md §10.2, §13 interlock 6); the bootloader
		// layer never lets it bypass a CRC mismatch (SPEC.md §10.5).
		Force: o.force,
		// SkipVerify is deliberately never set from --force. --force only
		// unblocks the interlock on an image that carries no CRC, which the
		// update package already declines to verify (Firmware.CRCKnown);
		// mapping it here would newly skip verification on an image that does
		// have one, which is the one direction that must not be taken.
		Progress: pw.report,
		Log: func(line string) {
			pw.clear()
			f.Diag("%s", line)
		},
	}
	res, err := updateFirmware(ctx, dev, iface, fw, opts)
	pw.clear()
	if err != nil {
		return nil, a.updateError(err, o)
	}
	if res.Reflashed {
		f.Diag("note: the image needed a second, acknowledged pass before it verified")
	}
	return res, nil
}

// updateError translates a failed update into the CLI's exit codes and the
// instruction that resolves it. The sentinels are kept wrapped so that
// errors.Is still works on the returned error.
func (a *App) updateError(err error, o flashOpts) error {
	switch {
	case errors.Is(err, bootloader.ErrCRCMismatch):
		// The unit is in the bootloader and re-flashable; the package's own
		// message says so, so only the command that resumes is added here.
		return codedSelfExplanatory(ExitFailure,
			"%w.\n  Retry with:  gflex firmware flash --recover %s --yes", err, recoverArg(o))
	case errors.Is(err, bootloader.ErrSerialMismatch):
		// Identity is checked before anything is written, so this one really
		// can promise that nothing was flashed.
		return codedSelfExplanatory(ExitFailure,
			"%w.\n  Nothing has been flashed. Disconnect any other VFLEX and retry.", err)
	}
	return fmt.Errorf("updating the firmware: %w\n  %s, and the unit is still re-flashable", err, bootloaderLEDNote)
}

// confirmJump establishes that the unit really did leave the bus, which is the
// only evidence the unacknowledged jump produces (SPEC.md §10.1), and aborts
// the update before anything is written if it did not.
func (a *App) confirmJump(ctx context.Context, f Formatter, appSerial string) error {
	if !a.midiPresenceMeaningful() {
		f.Diag("note: the jump cannot be confirmed on --transport %s -- this transport detaches the "+
			"kernel MIDI driver itself, so the port is absent either way. Continuing to the "+
			"bootloader interface, where the unit's serial number is checked before any page "+
			"is written.", a.Transport)
		return nil
	}
	if err := waitForDevice(ctx, false, disconnectTimeout); err == nil {
		return nil
	}
	return codedSelfExplanatory(ExitFailure,
		"the device did not disconnect within %s, so the jump to the bootloader was not confirmed.\n"+
			"  Nothing has been flashed. %s",
		disconnectTimeout, a.describeVisibleUnit(ctx, f, appSerial))
}

// describeVisibleUnit identifies the VFLEX still on the bus after an
// unconfirmed jump.
//
// The old wording -- "the unit should still be in application mode" -- is only
// true when the port still visible belongs to the unit that was addressed. With
// a second VFLEX attached the reassurance can be exactly backwards: ours may
// have jumped and the port being seen is the other one's. The serial number is
// known here, and the read that settles it is read-only and bounded by
// --timeout, so it is cheaper to ask than to guess.
func (a *App) describeVisibleUnit(ctx context.Context, f Formatter, appSerial string) string {
	seen, err := a.readSerialQuietly(ctx, f)
	if err != nil {
		f.Diag("could not identify the VFLEX still on the bus: %v", err)
		seen = ""
	}
	return unconfirmedJumpNote(appSerial, seen)
}

// readSerialQuietly reopens the device and reads its serial number, for
// diagnosis only.
func (a *App) readSerialQuietly(ctx context.Context, f Formatter) (string, error) {
	c, err := a.connect(ctx, f)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.Session.SerialNumber(ctx)
}

// unconfirmedJumpNote phrases what is actually known once a jump produced no
// disconnect: the unit did not go, a different unit is what we are looking at,
// or nothing can be said at all.
func unconfirmedJumpNote(appSerial, seenSerial string) string {
	switch {
	case seenSerial == "":
		return fmt.Sprintf("A VFLEX is still on the bus but would not identify itself, so whether unit %s\n"+
			"  jumped is unknown. If its LED is blinking white slowly it is in the bootloader and\n"+
			"  `--recover` will resume; otherwise retry the flash.", appSerial)
	case seenSerial == appSerial:
		return fmt.Sprintf("Unit %s is still answering in application mode, so the jump did not take.", appSerial)
	default:
		return fmt.Sprintf("The VFLEX still on the bus reports serial %s, which is not the %s this update\n"+
			"  addressed: that is a second unit, and %s may in fact have jumped. Disconnect the\n"+
			"  other unit and retry -- with `--recover` if %s is blinking white slowly.",
			seenSerial, appSerial, appSerial, appSerial)
	}
}

// waitForApplicationMode waits for the device to come back after
// CMD_BOOTLOAD_END (SPEC.md §10.1 step 4).
//
// On the default rawmidi transport the ALSA node reappearing is the signal. On
// --transport usb no ALSA node is involved -- and on a headless box
// snd-usb-audio may not even be loaded -- so waiting for one would fail an
// update that had in fact succeeded, and skip the mandatory §10.4 replay with
// the device's settings still erased. USB presence is the weaker but valid
// signal there: it cannot tell application mode from bootloader mode, so the
// reconnect that follows is what actually proves the device is back.
func (a *App) waitForApplicationMode(ctx context.Context, timeout time.Duration) error {
	if a.midiPresenceMeaningful() {
		if err := waitForDevice(ctx, true, timeout); err != nil {
			return fmt.Errorf("the device did not come back within %s after the jump: %w", timeout, err)
		}
		return nil
	}
	const poll = 250 * time.Millisecond
	deadline := time.Now().Add(timeout)
	for {
		if usbPresent() {
			return nil
		}
		if time.Now().After(deadline) {
			return codedf(ExitNoDevice,
				"no device with vendor 0x%04X came back on the USB bus within %s after the jump",
				proto.VendorID, timeout)
		}
		if err := sleepCtx(ctx, poll); err != nil {
			return err
		}
	}
}

// awaitApplicationReturn is phase 4 of runFlash: the settle after
// CMD_BOOTLOAD_END, then the wait for the unit to re-enumerate in application
// mode (SPEC.md §10.1 step 4).
//
// It runs entirely after the update has succeeded, so its failures are the
// benign kind that used to be reported as the malignant kind: a Ctrl-C landing
// in the 4 s settle surfaced as nothing but "gflex: interrupted", and a hub
// that re-enumerates slower than 15 s exited as a missing device -- both
// reading as a failed update, when the firmware was already written, verified
// and jumped into. Every error return is therefore phrased by
// postFlashFailure.
func (a *App) awaitApplicationReturn(ctx context.Context, f Formatter, res *bootloader.UpdateResult) error {
	if err := sleepCtx(ctx, postJumpDelay); err != nil {
		return a.postFlashFailure(f, res, "waiting out the post-jump settle", err)
	}
	if err := a.waitForApplicationMode(ctx, reenumerationTimeout); err != nil {
		return a.postFlashFailure(f, res, "waiting for the unit to come back in application mode", err)
	}
	return nil
}

// postFlashFailure phrases every error that can occur once the update itself
// is over: the image verified (or its unverified start was explicitly forced)
// and CMD_BOOTLOAD_END was sent, so the unit is booting the new image and the
// one instruction that matters is "do NOT re-flash it". Re-flashing a unit
// whose firmware is fine is the most dangerous operation this tool has
// (SPEC.md §10, §13.6), and a generic failure exit is exactly what sends
// someone to do it.
//
// Everything the user needs survives two exit paths that would otherwise
// swallow it: Flush runs only on success, so the buffered crc-verified line
// would be discarded, and Execute prints nothing but "gflex: interrupted" for
// an error chained to context.Canceled. So the evidence and the guidance go
// out immediately on stderr through Diag -- what was skipped (the SPEC.md
// §10.4 settings replay and the version read-back) and how to run each by
// hand, in the same wording reportReplayIncomplete uses for the same reason.
// The command still exits non-zero, because it did not finish: the returned
// error keeps the original chain (sentinels, exit code) and suppresses the
// generic per-code hint, which would diagnose a "missing" device that merely
// re-enumerated slowly.
func (a *App) postFlashFailure(f Formatter, res *bootloader.UpdateResult, what string, err error) error {
	f.Diag("")
	f.Diag("the firmware update itself SUCCEEDED -- %s -- and the jump into it was sent.",
		flashOutcomePhrase(res))
	if res != nil && res.CRCChecked {
		// The crc-verified evidence, restated here because the buffered result
		// block it was recorded in will never be flushed on this path.
		f.Diag("verified CRC: 0x%02X", res.CRC)
	}
	f.Diag("Do NOT re-flash the unit for this. What failed is %s: %v", what, err)
	f.Diag("")
	f.Diag("what did not run is the replay of the settings a flash erases (SPEC.md §10.4) and the")
	f.Diag("version read-back, so the unit is running the new image with those values as the flash")
	f.Diag("left them. Reconnect and check `gflex info --all`; restore anything missing with")
	f.Diag("`gflex vlimit set --low <mV> --high <mV>`, `gflex current set <mA>`,")
	f.Diag("`gflex tolerance set --nominal <mV>`, `gflex calibrate adc --offset <n> --scale <n>`,")
	f.Diag("`gflex authlock set <level>`, and read the version with `gflex firmware version`.")
	return &CodedError{
		Code:   ExitCode(err),
		Err:    fmt.Errorf("the firmware update SUCCEEDED, but %s failed: %w", what, err),
		NoHint: true,
	}
}

// loadFirmware reads the image from disk, or fetches it for this serial, and
// applies --crc.
func (a *App) loadFirmware(ctx context.Context, o flashOpts, serial string) (*bootloader.Firmware, error) {
	var (
		fw  *bootloader.Firmware
		err error
	)
	if o.fetch {
		if !proto.SerialUsable(serial) {
			return nil, codedf(ExitFailure, "cannot fetch firmware: the serial number read back as %q", serial)
		}
		a.stderrf("fetching firmware for serial %s from %s ...\n", serial, o.wsURL)
		// o.fetchTimeout, not a.Timeout: see the --fetch-timeout flag.
		fw, err = fetchFirmware(ctx, o.wsURL, serial, o.fetchTimeout)
		if err != nil {
			return nil, fmt.Errorf("fetching firmware for serial %s: %w", serial, err)
		}
	} else {
		// --page-size is refused on every JSON image, because whether the
		// loader would honour it depends on the shape *inside* the document.
		// The object payload takes its split from the payload's own page_size
		// and ignores LoadOptions.PageSize outright. A bare array is honoured
		// only when its elements turn out to be byte values -- a flat image
		// with no split of its own, which bootloader.ParseImage treats exactly
		// like a raw .bin -- and ignored when they are pages. Telling those two
		// apart means parsing the whole document, which is the loader's job,
		// not a sniff's, and being wrong about it means silently ignoring the
		// one flag whose whole purpose is guarding raw-image geometry, on the
		// path where a wrong split can flash and even verify cleanly (SPEC.md
		// §10.2, §14.12). So the refusal is deliberately wider than the set of
		// images that would actually ignore the flag: it costs a bare-array
		// user the ability to state a geometry from this command, which is a
		// worse outcome only if they cannot hand the same bytes over as a .bin.
		//
		// A sniff error is deliberately not acted on here: LoadFileWithOptions
		// reads the same file next and reports the failure with the path in it.
		if o.pageSize != 0 {
			if isJSON, jerr := fileLooksJSON(o.path); jerr == nil && isJSON {
				return nil, codedf(ExitUsage, "--page-size splits a raw .bin, but %s is a JSON image, "+
					"where the page split is the payload's to state; drop the flag, or pass the "+
					"image as a raw .bin", o.path)
			}
		}
		// Page-geometry validation (divisibility by ChunksPerPage, chunk fit)
		// is the library's alone; its error names the exact rule violated and
		// is surfaced verbatim inside the wrap below.
		fw, err = bootloader.LoadFileWithOptions(o.path, bootloader.LoadOptions{PageSize: o.pageSize})
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", o.path, err)
		}
	}
	if o.crc >= 0 {
		// --crc supplies the expected byte for an image that carries none (a
		// raw .bin) and overrides one that does. It is applied to the image
		// because that is the only channel the update has for an expected CRC,
		// and because CRCKnown is what decides whether verification happens at
		// all. The two are recorded separately for a reason: a JSON image may
		// legitimately declare a CRC of 0x00, and reading that as "no CRC
		// supplied" would silently skip verification on a valid image.
		fw.CRC = uint8(o.crc)
		fw.CRCKnown = true
	}
	return fw, nil
}

// fileLooksJSON reports whether the image at path is one of the JSON payload
// shapes rather than a raw binary, by the same rule bootloader.ParseImage
// detects the format with: the first non-whitespace byte is '{' or '['. The
// duplication is one switch on one byte, and it exists so that --page-size can
// be refused on a JSON image *before* the loader is handed it -- see
// loadFirmware for why that refusal deliberately covers every JSON shape and
// not only the ones whose split really does come from the payload. A wholly
// whitespace or unreadable file is not judged here: the loader reads the same
// file next and its error names the path.
func fileLooksJSON(path string) (bool, error) {
	fh, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer fh.Close()
	buf := make([]byte, 4096)
	for {
		n, rerr := fh.Read(buf)
		for _, b := range buf[:n] {
			switch b {
			case ' ', '\t', '\r', '\n':
				// The same leading whitespace ParseImage trims.
			default:
				return b == '{' || b == '[', nil
			}
		}
		if rerr != nil {
			return false, nil
		}
	}
}

// openBootloaderInterface opens the vendor-class interface and turns a failure
// into CLI guidance.
//
// It does NOT retry. bootloader.Connect owns that loop -- it re-enumerates for
// bootloader.ConnectRetryWindow because the device is still coming back on the
// bus after the mode switch -- and this function used to wrap it in a second
// loop of its own (15 s, 500 ms apart). The two multiplied instead of bounding
// each other: Connect burns its whole window before it ever returns an error,
// so the outer loop could only add another full pass on top of it, and the
// failure then quoted two different budgets in one message ("after 15s: ...
// after 8s"). Someone watching a stalled flash had no way to tell which one
// they were in. One retry loop, and it belongs to the package that knows what
// it is waiting for; do not add another here.
func openBootloaderInterface(ctx context.Context, f Formatter) (*usbfs.Device, usbfs.Interface, error) {
	// Said before the call, not after a first failure: Connect blocks for its
	// whole window, so a message that waits for one is a message nobody sees
	// while they are waiting for it.
	f.Diag("opening the bootloader interface (retrying for up to %s while the device enumerates)...",
		bootloader.ConnectRetryWindow)
	dev, iface, err := connectBootloader(ctx)
	if err == nil {
		return dev, iface, nil
	}
	// An interrupted run is not a missing device and must not be reported as
	// one: Connect returns the context's own error when it is cancelled
	// mid-wait, and ExitNoDevice would tell someone who pressed Ctrl-C that
	// their unit had vanished.
	if ctx.Err() != nil {
		return nil, usbfs.Interface{}, err
	}
	hint := ""
	if !usbPresent() {
		hint = fmt.Sprintf("\n  No device with vendor 0x%04X is on the bus at all.", proto.VendorID)
	}
	// The window is not restated here: Connect owns the retry and names its own
	// budget in the error being wrapped.
	return nil, usbfs.Interface{}, codedf(ExitNoDevice,
		"could not open the bootloader interface: %v%s\n"+
			"  The bootloader is a raw USB interface, not MIDI, so it needs a udev rule:\n"+
			"    sudo gflex install-udev\n"+
			"  %s.", err, hint, bootloaderLEDNote)
}

func recoverArg(o flashOpts) string {
	if o.fetch {
		return "--fetch"
	}
	return o.path
}

// progressWriter renders flash progress on stderr, and keeps the log lines the
// update interleaves with it from being written on top of a partial line.
//
// Progress is a single line rewritten with a carriage return, so anything else
// printed while it is pending has to be preceded by a newline -- but only when
// something is actually pending, or the output grows a blank line per message.
type progressWriter struct {
	w     io.Writer // nil when progress is not rendered at all
	dirty bool
}

// newProgressWriter builds the reporter for this invocation. Progress is
// suppressed for --json (stdout must stay a single object) and when stderr is
// not a terminal, where a carriage-returned line is just noise in a log.
func (a *App) newProgressWriter() *progressWriter {
	if a.AsJSON || !isTerminal(fdOf(a.stderr)) {
		return &progressWriter{}
	}
	return &progressWriter{w: a.stderr}
}

// report renders one Progress. Only the per-page phases have a page and chunk
// worth showing; serial, settle, verify and end are one-shot markers.
func (p *progressWriter) report(pr bootloader.Progress) {
	if p.w == nil {
		return
	}
	switch pr.Phase {
	case bootloader.PhaseChunk:
		fmt.Fprintf(p.w, "\r  %-12s page %d/%d  chunk %d/%d   ",
			pr.Phase, pr.Page+1, pr.TotalPages, pr.Chunk+1, pr.TotalChunks)
	case bootloader.PhaseCommit:
		fmt.Fprintf(p.w, "\r  %-12s page %d/%d               ", pr.Phase, pr.Page+1, pr.TotalPages)
	default:
		fmt.Fprintf(p.w, "\r  %-12s                          ", pr.Phase)
	}
	p.dirty = true
}

// clear ends a pending progress line so the next output starts on its own.
func (p *progressWriter) clear() {
	if p.w == nil || !p.dirty {
		return
	}
	fmt.Fprintln(p.w)
	p.dirty = false
}

func (a *App) stderrf(format string, args ...any) {
	fmt.Fprintf(a.stderr, format, args...)
}

// dryRunFlash reports what an update would do without sending anything. The
// bootloader speaks raw bulk frames with no MIDI encoding at all (SPEC.md
// §10.2), so the frame/MIDI pair that --dry-run prints elsewhere only applies
// to the one command sent over MIDI: the jump.
func (a *App) dryRunFlash(f Formatter, fw *bootloader.Firmware) error {
	f.KV("dry_run", "dry run", true, "nothing was sent to the device")
	f.KV("fw_version", "image version", fw.Version, fw.Version)
	f.KV("pages", "pages", len(fw.Pages), fmt.Sprintf("%d", len(fw.Pages)))
	if fw.CRCKnown {
		f.KV("crc", "image crc", fw.CRC, fmt.Sprintf("0x%02x", fw.CRC))
	} else {
		// Reporting 0x00 here would read as a CRC of zero rather than the
		// absence of one, and the difference decides whether the flash gets
		// verified at all.
		f.KV("crc", "image crc", nil, "none -- verification will be skipped")
	}
	if err := a.dryRun(f, proto.Read(proto.CmdJumpAppToBootloader)); err != nil {
		return err
	}
	f.Note("")
	f.Note("After the jump the device speaks raw bulk USB, not MIDI: the bootloader frames")
	f.Note("carry the same [len, cmd|0x80] preamble but no nibble encoding (SPEC.md §10.2).")
	return nil
}
