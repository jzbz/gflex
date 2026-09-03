package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/jzbz/gflex/internal/ctxwait"
	"github.com/jzbz/gflex/internal/pdo"
	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/session"
)

// The vendor's own strings. Users will search for these verbatim, so they are
// reproduced exactly (SPEC.md §9.6).
const (
	msgFirmwareTooOld = "Power Supply Scan requires VFLEX firmware 5.0.0 or newer. " +
		"Update firmware before scanning."
	msgSerialMismatch = "A different VFLEX serial number was detected. This scan has been aborted."
)

// The serial-read policy SPEC.md §9.2 specifies for the scan workflow: six
// attempts, 300 ms apart. It is deliberately far more patient than the ordinary
// three-attempt reads elsewhere, and the reason is where in the workflow it
// sits -- see readSerialRetrying.
const (
	scanSerialAttempts   = 6
	scanSerialRetryDelay = 300 * time.Millisecond
)

// scanResult is the machine-readable form of a completed scan.
type scanResult struct {
	SerialNum  string     `json:"serial_num"`
	FirmwareID string     `json:"fw_id,omitempty"`
	Log        *pdo.Log   `json:"pdo_log"`
	Match      *pdo.Match `json:"match,omitempty"`
}

func newScanCommand(app *App) *cobra.Command {
	var (
		voltageArg string
		currentArg string
		wait       time.Duration
		settle     time.Duration
		noPrompt   bool
	)
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Guided capture of a power source's PD capabilities",
		Long: "scan walks through the capture workflow of SPEC.md §9.2:\n\n" +
			"  1. check the firmware is 5.0.0 or newer, and latch this unit's serial number\n" +
			"  2. erase the capture log\n" +
			"  3. you unplug the VFLEX and attach it to the source under test for ~5 seconds\n" +
			"  4. you plug it back in here\n" +
			"  5. the serial is re-read and the scan aborts if it is a different unit\n" +
			"  6. the log is downloaded and decoded\n\n" +
			"The serial check is a hard invariant: the unit whose log was erased must be the\n" +
			"unit read back, or the results would describe the wrong capture.\n\n" +
			"For scripting use --no-prompt, which waits for the device to disappear and come\n" +
			"back instead of asking, bounded by --wait.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				var wantV, wantA float64
				haveTarget := cmd.Flags().Changed("voltage") || cmd.Flags().Changed("current")
				if haveTarget {
					// The compatibility check needs both terms: a voltage a
					// source can reach at 100 mA but not at 3 A is a different
					// answer, and guessing the missing one would hide that.
					if voltageArg == "" || currentArg == "" {
						return codedf(ExitUsage, "--voltage and --current must be given together")
					}
					mv, err := ParseVoltage(voltageArg)
					if err != nil {
						return err
					}
					ma, err := ParseCurrent(currentArg)
					if err != nil {
						return err
					}
					// pdo.Evaluate speaks volts and amps because USB-PD does.
					// This is a display/analysis boundary, not the data path:
					// nothing here is written back to the device.
					wantV, wantA = float64(mv)/1000, float64(ma)/1000
				}
				if app.DryRun {
					frames, err := scanFrames()
					if err != nil {
						return err
					}
					return app.dryRun(f, frames...)
				}
				return app.runScan(ctx, f, scanOpts{
					wait:       wait,
					settle:     settle,
					noPrompt:   noPrompt,
					haveTarget: haveTarget,
					voltageV:   wantV,
					currentA:   wantA,
				})
			})
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&voltageArg, "voltage", "", "also report whether this voltage is achievable, e.g. 12 or 12V")
	fl.StringVar(&currentArg, "current", "", "current to check alongside --voltage, e.g. 3 or 3A")
	fl.DurationVar(&wait, "wait", 2*time.Minute, "how long to wait for the device to leave and come back")
	fl.DurationVar(&settle, "settle", 5*time.Second,
		"how long to let the capture settle on the source before expecting the device back (--no-prompt only)")
	fl.BoolVar(&noPrompt, "no-prompt", false, "do not ask anything; wait for the unplug/replug instead")
	return cmd
}

// scanFrames is the --dry-run listing for `scan`, in the order the workflow of
// SPEC.md §9.2 sends them.
//
// The serial read appears twice on purpose: once to latch the unit's identity
// before the log is erased, and once after the user brings the device back, to
// prove it is the same unit. Listing it once would misdescribe the single most
// important step in the workflow.
func scanFrames() ([][]byte, error) {
	erase, err := pdoEraseFrame()
	if err != nil {
		return nil, err
	}
	frames := [][]byte{
		proto.Read(proto.CmdFirmwareVersion), // gate on firmware >= 5.0.0
		proto.Read(proto.CmdSerialNumber),    // latch the expected serial
		erase,
		proto.Read(proto.CmdSerialNumber), // after reconnect: same unit?
	}
	return append(frames, pdoReadFrames()...), nil
}

// readSerialRetrying reads the serial number under SPEC.md §9.2's retry policy.
//
// It retries ONLY when the serial could not be read at all: a transport or
// timeout error, or a response that sanitises to fewer than four characters and
// so cannot identify a unit (SPEC.md §6.4). A serial that reads cleanly is
// returned at once, whatever its value. That distinction is the whole point.
// The equality check the caller then makes is the hard invariant of §9.2 -- the
// unit whose log was erased must be the unit read back -- and retrying past a
// readable mismatch would quietly convert "this is a different VFLEX" into
// "keep asking until one of them agrees", which is the one weakening this
// workflow cannot survive: the decoded capture would describe the wrong unit's
// power source and nothing downstream would ever notice.
//
// Being patient about an unreadable serial costs nothing and buys a lot. The
// reconnect happens at the one point in the workflow where the user cannot
// cheaply retry: they have already unplugged the device, carried it to a
// charger, waited, and plugged it back in. A just-enumerated unit answering
// slowly is the normal case there, not an exotic one, and the protocol has no
// NACK, so a single timeout is routine rather than diagnostic (SPEC.md §5.2).
// Aborting the whole scan on one dropped frame makes the user redo the walk.
func readSerialRetrying(ctx context.Context, read func(context.Context) (string, error),
	attempts int, delay time.Duration) (string, error) {
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := ctxwait.Sleep(ctx, delay); err != nil {
				return "", err
			}
		}
		serial, err := read(ctx)
		switch {
		case err != nil && session.PermanentErr(err):
			// The link itself is gone (unplug, closed session, dead context).
			// Every further attempt fails identically and instantly, so the
			// 6 x 300 ms patience budget -- which exists for a slow-answering
			// just-enumerated unit (SPEC.md §9.2), not a dead one -- would
			// only delay telling the user the scan is over. Same
			// classification as the session's own retry loops.
			return "", fmt.Errorf("no usable serial number: %w", err)
		case err != nil:
			last = err
		case !proto.SerialUsable(serial):
			last = fmt.Errorf("the serial number read back as %q, which is too short to identify this unit", serial)
		default:
			return serial, nil
		}
	}
	return "", fmt.Errorf("no usable serial number after %d attempts %s apart: %w", attempts, delay, last)
}

type scanOpts struct {
	wait       time.Duration
	settle     time.Duration
	noPrompt   bool
	haveTarget bool
	voltageV   float64
	currentA   float64
}

func (a *App) runScan(ctx context.Context, f Formatter, o scanOpts) error {
	// The interactive handover ends at a.pause, which refuses outright when
	// stdin is not a terminal (interlock 7, SPEC.md §13.7). That refusal has to
	// happen HERE, before phase 1, because phase 1 finishes by erasing the
	// device's capture log -- and SPEC.md §9.2 offers no way to get one back
	// short of carrying the unit to the source and scanning it again. Reaching
	// the check afterwards meant `gflex scan </dev/null`, or the same command
	// from cron or CI, destroyed a capture the user cared about and then told
	// them they had mistyped the command line. --yes does not help either:
	// pause is not a confirmation and has no --yes path.
	//
	// Nothing about the condition needs the device. stdinIsTTY is a local ioctl
	// on our own stdin (see tty.go), so it is answerable before a single frame
	// is sent -- which is the same ordering every §13 interlock already uses,
	// where App.apply runs ahead of App.connect.
	if !o.noPrompt && !a.stdinIsTTY() {
		return codedf(ExitUsage, "the scan has to ask you to unplug the VFLEX and plug it back in, but "+
			"stdin is not a terminal;\n  use --no-prompt (optionally with --wait) to run this unattended. "+
			"Nothing has been sent to the device.")
	}

	// --- phase 1: gate on firmware, latch the serial, erase ------------------
	c, err := a.connect(ctx, f)
	if err != nil {
		return err
	}

	version, err := requirePDOFirmware(ctx, c.Session)
	if err != nil {
		c.Close()
		return err
	}

	// The latch read is not retried. It happens before the user has done
	// anything, so a failure here costs them one `gflex scan` and nothing else;
	// the read worth being patient about is the one after the handover.
	expectedSerial, err := c.Session.SerialNumber(ctx)
	if err != nil {
		c.Close()
		return fmt.Errorf("reading the serial number: %w", err)
	}
	if !proto.SerialUsable(expectedSerial) {
		c.Close()
		return codedf(ExitFailure, "the serial number read back as %q, which is too short to identify this unit", expectedSerial)
	}

	if err := c.Session.ClearPDOLog(ctx); err != nil {
		c.Close()
		return fmt.Errorf("erasing the PDO log: %w", err)
	}
	f.Diag("scan started on serial %s (firmware %s); capture log erased", expectedSerial, version)

	// The device has to be unplugged from here, so the session must go first.
	if err := c.Close(); err != nil {
		f.Diag("warning: closing the device: %v", err)
	}

	// --- phase 2: the user attaches the unit to the source under test --------
	if err := a.scanHandover(ctx, f, o, waitForDevice); err != nil {
		// Everything from here on is past the erase, so an abort leaves the
		// unit holding nothing. Say so plainly rather than leaving the user to
		// infer it from the Diag line above: the only recovery SPEC.md §9.2
		// describes is to attach the unit to the source and scan again.
		f.Diag("the capture log was already erased, so this unit now holds no capture; " +
			"re-run `gflex scan` to take a fresh one")
		return err
	}

	// --- phase 3: reconnect, verify the serial, download ---------------------
	c2, err := a.connect(ctx, f)
	if err != nil {
		return err
	}
	defer c2.Close()

	serial, err := readSerialRetrying(ctx, c2.Session.SerialNumber, scanSerialAttempts, scanSerialRetryDelay)
	if err != nil {
		return fmt.Errorf("re-reading the serial number after reconnect: %w", err)
	}
	// The hard invariant of SPEC.md §9.2, and deliberately absolute: a serial
	// that reads cleanly and differs ends the scan here. Nothing above softens
	// it -- the retry covers only the case where no serial could be read at all.
	if serial != expectedSerial {
		return codedf(ExitFailure, "%s\n  expected %q, found %q", msgSerialMismatch, expectedSerial, serial)
	}

	blob, err := a.downloadPDOLog(ctx, c2)
	if err != nil {
		return err
	}
	log, err := pdo.Parse(blob)
	if err != nil {
		return fmt.Errorf("decoding the PDO log: %w", err)
	}

	result := scanResult{SerialNum: serial, FirmwareID: version, Log: log}
	if o.haveTarget {
		m := log.Evaluate(o.voltageV, o.currentA)
		result.Match = &m
	}
	f.Document(result)

	f.KV("serial_num", "serial", serial, serial)
	emitPDOLog(f, log, blob, false, false)

	if result.Match != nil {
		emitMatch(f, *result.Match, o.voltageV, o.currentA)
	}
	return nil
}

// scanHandover covers the part of the workflow that happens off-host: the user
// takes the unit to the charger under test and brings it back. waitDev
// abstracts waitForDevice so both halves are testable without hardware.
func (a *App) scanHandover(ctx context.Context, f Formatter, o scanOpts,
	waitDev func(context.Context, bool, time.Duration) error) error {
	f.Diag("")
	f.Diag("Now, on the VFLEX:")
	f.Diag("  1. unplug it from this computer")
	f.Diag("  2. plug it into the USB-C PD source you want to characterise")
	f.Diag("  3. leave it there for about 5 seconds, until the LED settles green or red")
	f.Diag("  4. plug it back into this computer")
	f.Diag("")

	if o.noPrompt {
		return a.scanAwaitHandover(ctx, f, o, waitDev)
	}

	if err := a.pause(ctx, "Press Enter once the VFLEX is plugged back into this computer..."); err != nil {
		return err
	}
	// Interactive mode takes the user's Enter as the handover signal, and the
	// rawmidi node is consulted only where it can corroborate that. Under
	// --transport usb it cannot: midiPresenceMeaningful (firmware.go) records
	// why -- usbmidi.Open detached snd-usb-audio to claim the MIDI interface
	// (SPEC.md §4.2), and on a headless box the driver may never have been
	// loaded at all -- so the node may be absent no matter what the user just
	// plugged in. Waiting on it there aborts a scan at its very last step,
	// after the log has already been erased and the trip to the charger made,
	// for a signal that was never going to arrive. What actually protects the
	// result is phase 3: the reconnect, and the serial that must match the
	// unit whose log was erased (SPEC.md §9.2's hard invariant). Node presence
	// was never that check, so skipping it costs no integrity -- a device that
	// is not back simply fails the reconnect a moment later, with the
	// transport's own error rather than a misleading one about visibility.
	if !a.midiPresenceMeaningful() {
		f.Diag("note: on --transport %s the MIDI port is not a usable presence signal, so the "+
			"reconnect and serial check below are what establish the unit is back.", a.Transport)
		return nil
	}
	// On rawmidi presence does track the device, and ALSA needs a moment to
	// publish the node after enumeration, so wait out that race rather than
	// letting the reconnect fail on it. This is the sibling of
	// scanReattachGrace and deliberately longer: that one bounds a local
	// driver rebind, this one a fresh USB enumeration the user has just
	// initiated by hand.
	if err := waitDev(ctx, true, scanReplugGrace); err != nil {
		return fmt.Errorf("the VFLEX is not visible again: %w", err)
	}
	return nil
}

// scanReplugGrace bounds the interactive wait for the rawmidi node to appear
// after the user says they have plugged the unit back in (see scanHandover).
const scanReplugGrace = 10 * time.Second

// scanReattachGrace bounds the wait for the rawmidi node to reappear after a
// --transport usb session releases the MIDI interface (see scanAwaitHandover).
// Rebinding snd-usb-audio is a local kernel/udev operation, not a device
// action, so a healthy system is done in well under a second; the bound only
// has to be generous relative to that, not to the user's walk to the charger.
const scanReattachGrace = 5 * time.Second

// scanAwaitHandover is the --no-prompt half of scanHandover: watch the rawmidi
// node's presence for the unplug and the replug. waitDev abstracts
// waitForDevice so the sequencing is testable without hardware.
//
// The presence checks here are exactly what midiPresenceMeaningful (see
// firmware.go) warns about. Under --transport usb the phase-1 session claimed
// the MIDI interface with a kernel-driver detach (SPEC.md §4.2), so at this
// point the node's absence tracks THIS PROCESS, not the device: reading it as
// "the user unplugged" would sail through the settle while c.Close's reattach
// brings the node back, reconnect to the never-moved unit, match its serial,
// and report the just-erased log as a completed scan of the charger -- wrong
// data the user then acts on. The interactive path has no such problem, since
// the user tells us when each physical step has happened.
//
// So when presence is not currently meaningful, first wait -- bounded by
// scanReattachGrace -- for the node to REAPPEAR: Close handed the interface
// back to snd-usb-audio, and the node returning is the proof that presence
// tracks the device again, which makes the subsequent departure watch
// trustworthy. If it does not return, presence cannot distinguish anything on
// this system (snd-usb-audio may simply not be loaded -- the headless case in
// midiPresenceMeaningful's comment -- or the unit left the bus before the
// rebind finished) and the scan refuses rather than guesses: the §9.2 workflow
// exists to keep the decoded capture honest, and a wrong guess here fabricates
// one. The message points at interactive mode, which stays available and, by
// the same reasoning, does not consult the node on this transport at all (see
// scanHandover) -- so the advice leads somewhere that works rather than back
// into the same dead end.
func (a *App) scanAwaitHandover(ctx context.Context, f Formatter, o scanOpts,
	waitDev func(context.Context, bool, time.Duration) error) error {
	if !a.midiPresenceMeaningful() {
		f.Diag("waiting up to %s for the MIDI port to reappear before watching the handover...", scanReattachGrace)
		if err := waitDev(ctx, true, scanReattachGrace); err != nil {
			if ctx.Err() != nil {
				return err
			}
			return codedf(ExitFailure,
				"the VFLEX's MIDI port did not reappear within %s of releasing the USB interface, so "+
					"its presence cannot be used to watch the unplug/replug on --transport %s (the "+
					"snd-usb-audio driver may not be loaded on this system). Run the scan without "+
					"--no-prompt: interactive mode sequences the handover from your keypress and "+
					"does not consult the MIDI port on this transport, so it works here.",
				scanReattachGrace, a.Transport)
		}
	}
	f.Diag("waiting up to %s for the VFLEX to disconnect...", o.wait)
	if err := waitDev(ctx, false, o.wait); err != nil {
		return err
	}
	if err := ctxwait.Sleep(ctx, o.settle); err != nil {
		return err
	}
	f.Diag("waiting up to %s for the VFLEX to come back...", o.wait)
	return waitDev(ctx, true, o.wait)
}

// emitMatch renders the compatibility verdict from pdo.Evaluate.
func emitMatch(f Formatter, m pdo.Match, voltageV, currentA float64) {
	f.Note("")
	verdict := "NO"
	if m.OK {
		verdict = "yes"
	}
	f.KV("match_ok", "target achievable", m.OK,
		fmt.Sprintf("%s   (%s V at %s A)", verdict, trimFloat(voltageV, 3), trimFloat(currentA, 3)))
	if m.Mode != "" {
		f.KV("match_mode", "negotiated as", m.Mode, m.Mode)
	}
	if m.MaxCurrentA > 0 {
		// The row carries the conservative number alone. What the source
		// declared, and why the figure is lower, is said by pdo.finish's own
		// cable-bound note two lines below -- it is appended to Messages on
		// every path that sets DeclaredMaxCurrentA, which is exactly the
		// condition this row used to re-derive. Saying it here as well printed
		// one fact twice in two wordings, which is two claims to a reader; the
		// pdo package exported SPRAVSAssumptionClause rather than let a
		// disclosure be spelled out differently in two places, and this is the
		// same rule applied to the other one.
		f.KV("match_max_current_a", "available current", m.MaxCurrentA, trimFloat(m.MaxCurrentA, 2)+" A")
	}
	for _, msg := range m.Messages {
		f.Note("  %s", msg)
	}
	// Caveats qualify a verdict the user is about to act on -- most importantly
	// a recorded EPR cable failure, which means the scan itself may not have
	// seen the source's true capability. They reach --json through the embedded
	// Match; without this they would never reach a human.
	for _, c := range m.Caveats {
		f.Note("  caveat: %s", c)
	}
	if m.OK && voltageV > float64(proto.EPRThresholdMv)/1000 {
		f.Note("")
		f.Note("%s", eprWarning)
	}
}
