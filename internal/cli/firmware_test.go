package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/bootloader"
	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/session"
	"github.com/jzbz/gflex/internal/transport/fake"
	"github.com/jzbz/gflex/internal/usbfs"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// newFlashTestApp builds an App with buffered streams. stderr is not a
// terminal, so progress rendering is off, which is what a test wants.
func newFlashTestApp() (*App, *bytes.Buffer, *bytes.Buffer) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	app := &App{
		Transport: transportRawMIDI,
		Timeout:   proto.DefaultTimeout,
		stdout:    out,
		stderr:    errOut,
		// These tests exercise the paths that run after a port has been opened,
		// and the port openRawMIDI opens in the ordinary case is one the vendor
		// ID identified -- which is what makes the ALSA node a usable presence
		// signal (see midiPresenceMeaningful).
		midiPortVIDConfirmed: true,
	}
	return app, out, errOut
}

// flashTestImage is a one-page image with a declared CRC.
func flashTestImage() *bootloader.Firmware {
	return &bootloader.Firmware{
		Pages:    [][]byte{bytes.Repeat([]byte{0xA5}, bootloader.DefaultPageSize)},
		Version:  "5.1.0",
		CRC:      0x5A,
		CRCKnown: true,
	}
}

type updateFn = func(context.Context, *usbfs.Device, usbfs.Interface, *bootloader.Firmware,
	bootloader.UpdateOptions) (*bootloader.UpdateResult, error)

func stubUpdate(t *testing.T, fn updateFn) {
	t.Helper()
	prev := updateFirmware
	updateFirmware = fn
	t.Cleanup(func() { updateFirmware = prev })
}

func stubFetch(t *testing.T, fn func(context.Context, string, string, time.Duration) (*bootloader.Firmware, error)) {
	t.Helper()
	prev := fetchFirmware
	fetchFirmware = fn
	t.Cleanup(func() { fetchFirmware = prev })
}

// baseOpts is the flash option set with nothing overridden.
func baseOpts() flashOpts {
	return flashOpts{crc: -1, fetchTimeout: bootloader.DefaultFetchTimeout}
}

type connectFn = func(context.Context) (*usbfs.Device, usbfs.Interface, error)

// stubConnect substitutes the bootloader connect seam for a stub that ignores
// the retry options, which is what all but the fail-fast tests care about.
func stubConnect(t *testing.T, fn connectFn) {
	t.Helper()
	stubConnectWithOptions(t, func(ctx context.Context, _ bootloader.ConnectOptions) (*usbfs.Device, usbfs.Interface, error) {
		return fn(ctx)
	})
}

// stubConnectWithOptions is stubConnect for a test that needs to see which
// retry policy the CLI asked for.
func stubConnectWithOptions(t *testing.T, fn func(context.Context, bootloader.ConnectOptions) (*usbfs.Device, usbfs.Interface, error)) {
	t.Helper()
	prev := connectBootloader
	connectBootloader = fn
	t.Cleanup(func() { connectBootloader = prev })
}

// newReplaySession builds a session over an in-memory VFLEX for the post-update
// replay tests. The timeout is short on purpose: these tests drop a command to
// make a replay step fail, and the drop is paid for in full at every faulted
// command (SPEC.md §5.2 -- the protocol has no NACK, so a refusal is a timeout).
func newReplaySession(t *testing.T, dev *fake.Device) *session.Session {
	t.Helper()
	s := session.New(dev.Transport(), session.Options{
		ByteDelay: time.Nanosecond,
		Timeout:   150 * time.Millisecond,
	})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// countDeviceReads counts the read frames for cmd that actually reached the
// device. Not what was printed: how many times the command asked, which is what
// a protocol that charges --byte-delay per message (SPEC.md §3.1) bills for.
func countDeviceReads(t *testing.T, dev *fake.Device, cmd proto.Cmd) int {
	t.Helper()
	n := 0
	for _, fr := range dev.Sent() {
		parsed, err := proto.Parse(fr)
		if err != nil {
			t.Fatalf("the device received an unparseable frame %s: %v", proto.Hex(fr), err)
		}
		if !parsed.Write && parsed.Cmd == cmd {
			n++
		}
	}
	return n
}

// containsLine reports whether any line contains sub.
func containsLine(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// firmware version
// ---------------------------------------------------------------------------

// TestFirmwareVersionAsksTheDeviceOnce pins the single read. The whole body of
// this command is one CMD_FIRMWARE_VERSION exchange, and it used to issue two:
// once to print the string, and again inside Session.FirmwareAtLeast, whose
// first act is another read. At any per-message pacing (SPEC.md §3.1) that
// doubled the command's device time, and against a unit that has stopped
// answering it doubled the wait to two full --timeout periods. --dry-run has
// always advertised the one-frame shape; this makes the live path agree.
//
// Reverting to FirmwareAtLeast makes the count 2 and fails here.
func TestFirmwareVersionAsksTheDeviceOnce(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)

	if err := tr.run(t, "firmware", "version"); err != nil {
		t.Fatalf("firmware version: %v", err)
	}
	if got := countDeviceReads(t, dev, proto.CmdFirmwareVersion); got != 1 {
		t.Errorf("the device saw %d CMD_FIRMWARE_VERSION reads, want exactly 1", got)
	}
	out := tr.stdout.String()
	if !strings.Contains(out, fake.TypicalFirmware) {
		t.Errorf("the version was not printed:\n%s", out)
	}
	if strings.Contains(out, msgFirmwareTooOld) {
		t.Errorf("firmware %s was reported as too old:\n%s", fake.TypicalFirmware, out)
	}
}

// TestFirmwareVersionNotesFirmwareTooOld: the note is the only reason the
// version is compared at all -- the PD capability scan is hard-gated on 5.0.0
// (SPEC.md §9) -- so it has to survive the comparison moving off the device and
// into session.VersionAtLeast, still on the one read.
//
// It also pins the behaviour change that came with the single read. The old
// form discarded the second read's error (`err == nil && !ok`), so a dropped
// response -- routine in a protocol with no NACK (SPEC.md §5.2) -- silently
// omitted this note from a unit that had already reported a 4.x version. There
// is no second read left to fail.
func TestFirmwareVersionNotesFirmwareTooOld(t *testing.T) {
	dev := fake.NewTypical()
	dev.SetResponse(proto.CmdFirmwareVersion, []byte("4.9.2"))
	tr := newFakeTree(t, dev)

	if err := tr.run(t, "firmware", "version"); err != nil {
		t.Fatalf("firmware version: %v", err)
	}
	out := tr.stdout.String()
	if !strings.Contains(out, "4.9.2") {
		t.Errorf("the version was not printed:\n%s", out)
	}
	// The vendor's own wording, verbatim: users search for it (SPEC.md §9.6).
	if !strings.Contains(out, msgFirmwareTooOld) {
		t.Errorf("firmware 4.9.2 was not flagged as too old for the scan:\n%s", out)
	}
	if got := countDeviceReads(t, dev, proto.CmdFirmwareVersion); got != 1 {
		t.Errorf("the device saw %d CMD_FIRMWARE_VERSION reads, want exactly 1", got)
	}
}

// ---------------------------------------------------------------------------
// firmware fetch
// ---------------------------------------------------------------------------

// TestFetchDryRunLeavesTheSerialWithNobody pins interlock 8 of SPEC.md §13 on
// the one command that ignored it. `firmware fetch` read the serial off the
// unit, opened a TLS WebSocket to the vendor, handed the serial over, pulled
// the image down and wrote -o -- all under a flag whose help says it sends
// nothing. The frame it sends is a fixed CMD_SERIAL_NUMBER read, so §13.8's
// only escape ("the frame cannot be known without first reading the device",
// which is why `flash --fetch` refuses the combination) does not apply: the
// frame is printed and the rest of the command does not happen.
func TestFetchDryRunLeavesTheSerialWithNobody(t *testing.T) {
	stubFetch(t, func(context.Context, string, string, time.Duration) (*bootloader.Firmware, error) {
		t.Error("--dry-run contacted the vendor service")
		return flashTestImage(), nil
	})
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)
	out := filepath.Join(t.TempDir(), "image.json")

	if err := tr.run(t, "--dry-run", "firmware", "fetch", "-o", out); err != nil {
		t.Fatalf("`--dry-run firmware fetch`: %v", err)
	}
	if frames := dev.Sent(); len(frames) != 0 {
		t.Errorf("--dry-run sent %v to the device", cmdNames(frames))
	}
	if _, err := os.Stat(out); err == nil {
		t.Errorf("--dry-run wrote %s", out)
	}
	// The frame it would have sent still has to be printed, or "sends nothing"
	// has quietly become "does nothing".
	if said := tr.stdout.String(); !strings.Contains(said, proto.CmdSerialNumber.String()) {
		t.Errorf("--dry-run printed no frame table:\n%s", said)
	}
}

// TestRawFetchWithoutAnOutputIsRefusedBeforeTheSerialIsRead: --raw needs -o,
// and both halves are on the command line, so nothing has to happen first to
// find that out. The refusal used to come after the serial had been read off
// the unit, handed to the vendor and paid for with the whole download -- every
// side effect the combination is refused for, performed and then discarded.
func TestRawFetchWithoutAnOutputIsRefusedBeforeTheSerialIsRead(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)

	err := tr.run(t, "firmware", "fetch", "--raw")
	if err == nil {
		t.Fatal("`firmware fetch --raw` without -o was accepted")
	}
	if code := ExitCode(err); code != ExitUsage {
		t.Errorf("ExitCode = %d, want ExitUsage (%d): %v", code, ExitUsage, err)
	}
	if frames := dev.Sent(); len(frames) != 0 {
		t.Errorf("a usage error was answered only after reading the device: %v", cmdNames(frames))
	}
}

// TestAFetchFailureIsNotReportedAsADeviceTimeout: the vendor service is not the
// device. Every way a download runs out of time carries a shape isTimeout
// matches -- the budget expires as context.DeadlineExceeded, a stalled dial as
// a net.Error whose Timeout() is true -- so wrapping it bare exited 5, which
// README's table defines as "Timed out waiting for the device", under the hint
// that says the device did not answer and offers -v to trace MIDI traffic. The
// serial read had already succeeded by then; no device was involved at all.
func TestAFetchFailureIsNotReportedAsADeviceTimeout(t *testing.T) {
	const endpoint = "wss://lab.example/bootloader"
	stubFetch(t, func(context.Context, string, string, time.Duration) (*bootloader.Firmware, error) {
		return nil, fmt.Errorf("bootloader: fetching firmware: %w", context.DeadlineExceeded)
	})
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)

	err := tr.run(t, "firmware", "fetch", "--ws-url", endpoint)
	if err == nil {
		t.Fatal("an unreachable vendor service was reported as success")
	}
	if code := ExitCode(err); code == ExitTimeout {
		t.Errorf("a network failure exited ExitTimeout (%d), which is the device's timeout", ExitTimeout)
	} else if code != ExitFailure {
		t.Errorf("ExitCode = %d, want ExitFailure (%d): %v", code, ExitFailure, err)
	}
	if !suppressHint(err) {
		t.Error("the per-code hint was not suppressed; it diagnoses a device that was never asked anything")
	}
	msg := err.Error()
	for _, want := range []string{endpoint, "--fetch-timeout", "not the device"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure does not point at the network (missing %q): %s", want, msg)
		}
	}
}

// The same wording has to reach `flash --fetch`, which fails a layer lower.
func TestAFetchFailureInsideAFlashIsAlsoANetworkFailure(t *testing.T) {
	stubFetch(t, func(context.Context, string, string, time.Duration) (*bootloader.Firmware, error) {
		return nil, fmt.Errorf("bootloader: fetching firmware: %w", context.DeadlineExceeded)
	})
	app, _, _ := newFlashTestApp()
	o := baseOpts()
	o.fetch = true
	o.wsURL = bootloader.DefaultWSURL

	_, err := app.loadFirmware(context.Background(), o, "VFX-0001")
	if err == nil {
		t.Fatal("a failed download was reported as a loaded image")
	}
	if code := ExitCode(err); code != ExitFailure {
		t.Errorf("ExitCode = %d, want ExitFailure (%d): %v", code, ExitFailure, err)
	}
}

// ---------------------------------------------------------------------------
// firmware bootloader
// ---------------------------------------------------------------------------

// TestFirmwareBootloaderJumpIsGatedByTheConfirmation is the wiring test for the
// same action SPEC.md §13.10 makes `raw 02 14` confirm, reached by its own
// command. The confirmation here is a direct app.confirm rather than an
// app.apply(CheckFlash(...)), so interlock_test.go's tables cannot see it, and
// nothing else drove the command: deleting the confirm block left the whole
// suite green while `gflex firmware bootloader </dev/null` dropped a unit off
// the bus mid-session, where only a firmware flash or a power cycle reaches it.
func TestFirmwareBootloaderJumpIsGatedByTheConfirmation(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)

	err := tr.run(t, "firmware", "bootloader")
	if err == nil {
		t.Fatal("`firmware bootloader` jumped the unit with nobody to confirm it")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if tr.sent(t, proto.CmdJumpAppToBootloader) {
		t.Fatalf("the jump frame reached the device despite the refusal; frames: %v", cmdNames(dev.Sent()))
	}
}

// Its positive control: with --yes the same jump goes out, so the refusal above
// came from the interlock and not from the plumbing. --transport usb is what
// keeps this quick -- the evidence report then prints its "cannot be confirmed
// on this transport" note instead of polling sysfs for a disconnect that this
// fake device is in no position to perform.
func TestFirmwareBootloaderJumpProceedsWithYes(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)

	if err := tr.run(t, "firmware", "bootloader", "--yes", "--transport", "usb"); err != nil {
		t.Fatalf("`firmware bootloader --yes`: %v", err)
	}
	if !tr.sent(t, proto.CmdJumpAppToBootloader) {
		t.Errorf("--yes did not reach the device; frames: %v", cmdNames(dev.Sent()))
	}
}

// ---------------------------------------------------------------------------
// the update sequence lives in the bootloader package, not here
// ---------------------------------------------------------------------------

// TestRunUpdateDelegatesAndWaitsForNothing pins the fix for the duplicated
// update orchestration. The CLI's own copy applied POST_FLASH_DELAY_MS a second
// time on top of the one Flasher.Flash already performs, so a flash waited 4 s
// where SPEC.md §10.1 specifies 2 s. Nothing in the CLI may sit on a timer
// around the update any more: the delays belong to the package that owns the
// sequence.
func TestRunUpdateDelegatesAndWaitsForNothing(t *testing.T) {
	var (
		calls int
		got   bootloader.UpdateOptions
	)
	stubUpdate(t, func(_ context.Context, _ *usbfs.Device, _ usbfs.Interface, fw *bootloader.Firmware,
		opts bootloader.UpdateOptions,
	) (*bootloader.UpdateResult, error) {
		calls++
		got = opts
		if fw == nil {
			t.Error("no firmware image handed to the update")
		}
		return &bootloader.UpdateResult{Serial: "VFX-0001", CRC: 0x5A, CRCChecked: true, JumpedToApp: true}, nil
	})

	app, _, _ := newFlashTestApp()
	f := app.newFormatter()
	start := time.Now()
	res, err := app.runUpdate(context.Background(), f, nil, usbfs.Interface{},
		flashTestImage(), baseOpts(), "VFX-0001")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if calls != 1 {
		t.Errorf("bootloader.Update called %d times, want exactly 1", calls)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("runUpdate spent %s of its own; the settle delays belong to the bootloader package", elapsed)
	}
	if got.ExpectSerial != "VFX-0001" {
		t.Errorf("ExpectSerial = %q, want the serial read in application mode", got.ExpectSerial)
	}
	if got.SkipJump {
		t.Error("SkipJump set: the CLI asks for the jump back to the application")
	}
	if got.Force {
		t.Error("Force set without --force: an unverifiable image would be started without consent")
	}
	if !res.CRCChecked || res.CRC != 0x5A {
		t.Errorf("result not passed through: %+v", res)
	}
}

// TestForceNeverSkipsVerification guards both directions of the --force
// mapping at once, because each has failed on its own.
//
// It must not become SkipVerify: --force exists to flash an image that carries
// no CRC (CheckFlash refuses that image otherwise), and SkipVerify would skip
// verification on an image that does declare one -- the dangerous direction,
// because an unverified image is then jumped into.
//
// It must become Force: the earlier version of this test asserted only the
// first half, and the missing second assertion is exactly how the flag came to
// be mapped to nothing at all -- a raw .bin with --force passed CheckFlash,
// jumped the unit into the bootloader, and was then refused there with
// ErrUnverifiable telling the user to set the flag they had already passed.
func TestForceNeverSkipsVerification(t *testing.T) {
	var got bootloader.UpdateOptions
	stubUpdate(t, func(_ context.Context, _ *usbfs.Device, _ usbfs.Interface, _ *bootloader.Firmware,
		opts bootloader.UpdateOptions,
	) (*bootloader.UpdateResult, error) {
		got = opts
		return &bootloader.UpdateResult{}, nil
	})

	app, _, _ := newFlashTestApp()
	o := baseOpts()
	o.force = true
	if _, err := app.runUpdate(context.Background(), app.newFormatter(), nil, usbfs.Interface{},
		flashTestImage(), o, "VFX-0001"); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if got.SkipVerify {
		t.Error("--force became SkipVerify: an image that declares a CRC would be flashed unverified")
	}
	if !got.Force {
		t.Error("--force never reached bootloader.UpdateOptions.Force: a raw .bin flash jumps the " +
			"unit into the bootloader and is then refused there with ErrUnverifiable")
	}
}

// TestForcedRawBinReachesTheFlasher is the end-to-end shape of the same fix: a
// real raw .bin through the real loader, against a stub that reproduces the
// bootloader's own verification-policy gate (bootloader.update step 1: an
// image with no CRC that will be jumped into needs Force, SPEC.md §13
// interlock 6). Before the fix this refused with ErrUnverifiable -- after the
// jump, so the unit sat stranded in bootloader mode -- and --recover --force
// dead-ended identically.
func TestForcedRawBinReachesTheFlasher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fw.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0x22}, 2*bootloader.DefaultPageSize), 0o600); err != nil {
		t.Fatal(err)
	}
	var flashed bool
	stubUpdate(t, func(_ context.Context, _ *usbfs.Device, _ usbfs.Interface, fw *bootloader.Firmware,
		opts bootloader.UpdateOptions,
	) (*bootloader.UpdateResult, error) {
		crcKnown := fw.CRCKnown || opts.CRC != nil
		if !crcKnown && !opts.SkipJump && !opts.Force {
			return &bootloader.UpdateResult{}, fmt.Errorf(
				"%w: the image declares no CRC, so a successful flash cannot be confirmed",
				bootloader.ErrUnverifiable)
		}
		flashed = true
		return &bootloader.UpdateResult{Serial: "VFX-0001", Unverified: true, JumpedToApp: true}, nil
	})

	app, _, _ := newFlashTestApp()
	o := baseOpts()
	o.path = path
	o.force = true
	fw, err := app.loadFirmware(context.Background(), o, "")
	if err != nil {
		t.Fatalf("loadFirmware: %v", err)
	}
	if fw.CRCKnown {
		t.Fatal("a raw .bin claimed a CRC; the test needs the unverifiable path")
	}
	res, err := app.runUpdate(context.Background(), app.newFormatter(), nil, usbfs.Interface{},
		fw, o, "VFX-0001")
	if err != nil {
		t.Fatalf("a raw .bin with --force was refused after the jump: %v", err)
	}
	if !flashed {
		t.Fatal("the flasher was never reached")
	}
	if !res.Unverified {
		t.Errorf("result not passed through: %+v", res)
	}
}

// TestCRCMismatchSentinelSurvives is the regression test for the sentinel the
// duplicated copy swallowed: it compared CRCs itself, so callers could never
// match bootloader.ErrCRCMismatch. The CLI must add its own guidance without
// breaking errors.Is.
func TestCRCMismatchSentinelSurvives(t *testing.T) {
	stubUpdate(t, func(_ context.Context, _ *usbfs.Device, _ usbfs.Interface, _ *bootloader.Firmware,
		_ bootloader.UpdateOptions,
	) (*bootloader.UpdateResult, error) {
		return &bootloader.UpdateResult{}, fmt.Errorf(
			"%w: device reports 0x12, image declares 0x34", bootloader.ErrCRCMismatch)
	})

	app, _, _ := newFlashTestApp()
	o := baseOpts()
	o.path = "/tmp/fw.bin"
	o.pageSize = 320
	o.crc = 0x30
	_, err := app.runUpdate(context.Background(), app.newFormatter(), nil, usbfs.Interface{},
		flashTestImage(), o, "VFX-0001")
	if err == nil {
		t.Fatal("a CRC mismatch was not reported as an error")
	}
	if !errors.Is(err, bootloader.ErrCRCMismatch) {
		t.Errorf("errors.Is(err, ErrCRCMismatch) = false; the sentinel was lost: %v", err)
	}
	if code := ExitCode(err); code != ExitFailure {
		t.Errorf("exit code %d, want ExitFailure (%d)", code, ExitFailure)
	}
	msg := err.Error()
	// The geometry flags are part of "how to resume" and not decoration. The
	// line used to name the image alone, and a raw .bin resumed that way is
	// refused for carrying no CRC -- with advice to add --force, which then
	// re-splits it at the 512-byte default and flashes it unverified at a
	// geometry the user never asked for (SPEC.md §10.2, §14.12).
	for _, want := range []string{"--recover", "/tmp/fw.bin", "--page-size 320", "--crc 0x30"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure does not say how to resume (missing %q): %s", want, msg)
		}
	}
}

// TestTheResumeCommandKeepsEveryFlagThatDecidesWhatIsFlashed covers the same
// property for the flags the CRC-mismatch case does not carry, and its
// converse: a flag left at its default must not appear, or the line stops being
// something to paste and starts being something to edit.
func TestTheResumeCommandKeepsEveryFlagThatDecidesWhatIsFlashed(t *testing.T) {
	app, _, _ := newFlashTestApp()
	app.Transport = transportUSB
	o := baseOpts()
	o.path = "fw.bin"
	o.pageSize = 320
	o.crc = 0x30
	o.force = true
	o.ackFirst = true

	got := app.resumeCommand(o)
	for _, want := range []string{
		"--transport usb", "firmware flash --recover", "fw.bin",
		"--page-size 320", "--crc 0x30", "--force", "--ack-mode", "--yes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the resume command drops %q: %s", want, got)
		}
	}

	app.Transport = transportRawMIDI
	o = baseOpts()
	o.fetch = true
	o.wsURL = "wss://lab.example/bootloader"
	o.fetchTimeout = 90 * time.Second
	got = app.resumeCommand(o)
	for _, want := range []string{"--fetch", "--ws-url wss://lab.example/bootloader", "--fetch-timeout 1m30s"} {
		if !strings.Contains(got, want) {
			t.Errorf("the resume command drops %q, so the retry would go somewhere else: %s", want, got)
		}
	}
	// The defaults are the tool's, not the user's, and restating them adds
	// nothing a reader has to check.
	o = baseOpts()
	o.path = "fw.json"
	o.wsURL = bootloader.DefaultWSURL
	got = app.resumeCommand(o)
	for _, unwanted := range []string{"--transport", "--ws-url", "--fetch-timeout", "--crc", "--page-size", "--force"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the resume command states the default %s: %s", unwanted, got)
		}
	}
}

// TestSerialMismatchPromisesNothingWasFlashed: identity is checked before the
// first page is written, so this is the one failure that can say so.
func TestSerialMismatchPromisesNothingWasFlashed(t *testing.T) {
	stubUpdate(t, func(_ context.Context, _ *usbfs.Device, _ usbfs.Interface, _ *bootloader.Firmware,
		_ bootloader.UpdateOptions,
	) (*bootloader.UpdateResult, error) {
		return &bootloader.UpdateResult{}, fmt.Errorf(
			"%w: expected %q, bootloader reports %q", bootloader.ErrSerialMismatch, "VFX-0001", "VFX-0002")
	})

	app, _, _ := newFlashTestApp()
	_, err := app.runUpdate(context.Background(), app.newFormatter(), nil, usbfs.Interface{},
		flashTestImage(), baseOpts(), "VFX-0001")
	if err == nil {
		t.Fatal("a serial mismatch was not reported as an error")
	}
	if !errors.Is(err, bootloader.ErrSerialMismatch) {
		t.Errorf("errors.Is(err, ErrSerialMismatch) = false: %v", err)
	}
	if !strings.Contains(err.Error(), "Nothing has been flashed") {
		t.Errorf("the message does not say nothing was flashed: %v", err)
	}
	// "Nothing has been flashed" is true and, on its own, misleading: without
	// --recover this error is only reachable after phase 1 jumped the addressed
	// unit and watched it leave the bus, so a literal retry cannot find it and
	// fails in phase 1 as a missing device. SPEC.md §13.6 wants the state said.
	for _, want := range []string{"bootloader mode", "re-flashable", "--recover"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not say where the jumped unit is (missing %q): %v", want, err)
		}
	}

	// Under --recover no jump was issued by this command, so the sentence would
	// be someone else's news.
	o := baseOpts()
	o.recover = true
	_, err = app.runUpdate(context.Background(), app.newFormatter(), nil, usbfs.Interface{},
		flashTestImage(), o, "VFX-0001")
	if err == nil {
		t.Fatal("a serial mismatch under --recover was not reported as an error")
	}
	if strings.Contains(err.Error(), "The unit this command jumped") {
		t.Errorf("--recover claimed a jump it never made: %v", err)
	}
}

// TestAnInterruptedFlashSaysWhereTheUnitIs is the counterpart of
// TestInterruptionAfterASuccessfulFlashReportsSuccess for the window before
// CMD_BOOTLOAD_END -- the whole multi-second stream, where a Ctrl-C is most
// likely to land and where the image may be half written. Execute prints
// nothing but "gflex: interrupted" for an error chained to context.Canceled, so
// the §13.6 guidance in the returned error's text reached nobody: the unit sat
// in the bootloader with no MIDI interface, and every later gflex command
// reported a missing device with hints about udev rules and busy ports.
func TestAnInterruptedFlashSaysWhereTheUnitIs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stubUpdate(t, func(_ context.Context, _ *usbfs.Device, _ usbfs.Interface, _ *bootloader.Firmware,
		_ bootloader.UpdateOptions,
	) (*bootloader.UpdateResult, error) {
		// What Flasher.Flash returns from its pause points when the context
		// ends mid-stream: ctxwait.Sleep's own error, bare.
		cancel()
		return &bootloader.UpdateResult{}, ctx.Err()
	})

	app, _, errOut := newFlashTestApp()
	f := app.newFormatter()
	o := baseOpts()
	o.path = "/tmp/fw.json"
	_, err := app.runUpdate(ctx, f, nil, usbfs.Interface{}, flashTestImage(), o, "VFX-0001")
	if err == nil {
		t.Fatal("an interrupted update did not report an error")
	}
	err = app.bootloaderPhaseFailure(f, o, err)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("the interruption's own error was lost from the chain: %v", err)
	}
	said := errOut.String()
	for _, want := range []string{
		"bootloader mode",        // where the unit is
		"CMD_BOOTLOAD_END",       // and why it is still there
		"re-flashable",           // SPEC.md §13 interlock 6, in as many words
		"--recover /tmp/fw.json", // and the command that resumes
	} {
		if !strings.Contains(said, want) {
			t.Errorf("an interrupted flash said nothing about %q: %q", want, said)
		}
	}
}

// The other half: a failure that already explains itself must not be narrated
// twice. Execute prints those, so the guidance would appear once on stderr and
// again under "gflex:", which is how a message stops being read.
func TestAFailureThatExplainsItselfIsNotNarratedTwice(t *testing.T) {
	app, _, errOut := newFlashTestApp()
	o := baseOpts()
	o.path = "/tmp/fw.json"
	err := app.updateError(fmt.Errorf("%w: device reports 0x12, image declares 0x34",
		bootloader.ErrCRCMismatch), o)

	if got := app.bootloaderPhaseFailure(app.newFormatter(), o, err); got != err {
		t.Errorf("the original error was replaced: %v", got)
	}
	if said := errOut.String(); said != "" {
		t.Errorf("a self-explanatory failure was also narrated on stderr: %q", said)
	}
}

// ---------------------------------------------------------------------------
// --fetch budget
// ---------------------------------------------------------------------------

// TestFetchUsesTheDownloadBudgetNotTheCommandTimeout pins SPEC.md §10.3. The
// fetch used to be bounded by --timeout, which is the per-COMMAND response
// timeout of the MIDI protocol (5 s by default) and has nothing to do with an
// HTTP/WebSocket download -- and it made bootloader.DefaultFetchTimeout dead.
func TestFetchUsesTheDownloadBudgetNotTheCommandTimeout(t *testing.T) {
	var gotTimeout time.Duration
	stubFetch(t, func(_ context.Context, _, _ string, timeout time.Duration) (*bootloader.Firmware, error) {
		gotTimeout = timeout
		return flashTestImage(), nil
	})

	app, _, _ := newFlashTestApp()
	app.Timeout = 5 * time.Second
	o := baseOpts()
	o.fetch = true
	o.wsURL = bootloader.DefaultWSURL
	if _, err := app.loadFirmware(context.Background(), o, "VFX-0001"); err != nil {
		t.Fatalf("loadFirmware: %v", err)
	}
	if gotTimeout == app.Timeout {
		t.Fatalf("the fetch was bounded by --timeout (%s); that is the MIDI response timeout", app.Timeout)
	}
	if gotTimeout != bootloader.DefaultFetchTimeout {
		t.Errorf("fetch budget = %s, want bootloader.DefaultFetchTimeout (%s)",
			gotTimeout, bootloader.DefaultFetchTimeout)
	}
}

// TestFetchTimeoutFlagDefault checks the flag a user overrides it with.
func TestFetchTimeoutFlagDefault(t *testing.T) {
	app, _, _ := newFlashTestApp()
	fl := newFirmwareFlashCommand(app).Flags().Lookup("fetch-timeout")
	if fl == nil {
		t.Fatal("no --fetch-timeout flag")
	}
	if want := bootloader.DefaultFetchTimeout.String(); fl.DefValue != want {
		t.Errorf("--fetch-timeout default %q, want %q", fl.DefValue, want)
	}
}

// ---------------------------------------------------------------------------
// --crc
// ---------------------------------------------------------------------------

// TestCRCOverrideIsAppliedToTheImage: the update has no other channel for an
// expected CRC, and CRCKnown is what decides whether verification runs at all.
// A raw .bin declares none, so without --crc it must stay unverifiable rather
// than silently verifying against 0x00.
func TestCRCOverrideIsAppliedToTheImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fw.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0x11}, bootloader.DefaultPageSize), 0o600); err != nil {
		t.Fatal(err)
	}
	app, _, _ := newFlashTestApp()

	o := baseOpts()
	o.path = path
	fw, err := app.loadFirmware(context.Background(), o, "")
	if err != nil {
		t.Fatalf("loadFirmware: %v", err)
	}
	if fw.CRCKnown {
		t.Error("a raw .bin was reported as carrying a CRC")
	}

	o.crc = 0x00 // a legitimate expected value, and the one a bool would lose
	fw, err = app.loadFirmware(context.Background(), o, "")
	if err != nil {
		t.Fatalf("loadFirmware with --crc: %v", err)
	}
	if !fw.CRCKnown || fw.CRC != 0x00 {
		t.Errorf("--crc 0 did not make the image verifiable: CRCKnown=%v CRC=0x%02x", fw.CRCKnown, fw.CRC)
	}
}

// TestReplacingADeclaredCRCSaysSo: --crc also overrides the value an image
// declares, which is deliberate -- it is the only way to re-flash an image
// whose declared CRC is known wrong -- but it is also the one route by which an
// image that failed verification can be walked through to CMD_BOOTLOAD_END on
// the next run. The mismatch prints the byte the device computed, and typing
// that back in makes the comparison compare the device with itself, after which
// the run reports the image as verified. Silence there is what makes it look
// like verification; a raw .bin, which declares nothing to replace, must stay
// quiet.
func TestReplacingADeclaredCRCSaysSo(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "fw.json")
	if err := os.WriteFile(jsonPath,
		[]byte(`{"app_bin": [[1,2,3,4,5,6,7,8]], "app_version": "5.1.0", "crc": 52}`), 0o600); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(dir, "fw.bin")
	if err := os.WriteFile(binPath, bytes.Repeat([]byte{0x11}, bootloader.DefaultPageSize), 0o600); err != nil {
		t.Fatal(err)
	}

	app, _, errOut := newFlashTestApp()
	o := baseOpts()
	o.path = jsonPath
	o.crc = 0x12
	fw, err := app.loadFirmware(context.Background(), o, "")
	if err != nil {
		t.Fatalf("loadFirmware: %v", err)
	}
	if fw.CRC != 0x12 {
		t.Errorf("--crc did not take: expected CRC 0x%02x", fw.CRC)
	}
	said := errOut.String()
	for _, want := range []string{"0x12", "0x34"} {
		if !strings.Contains(said, want) {
			t.Errorf("the replacement did not name %s: %q", want, said)
		}
	}

	errOut.Reset()
	o.path = binPath
	if _, err := app.loadFirmware(context.Background(), o, ""); err != nil {
		t.Fatalf("loadFirmware: %v", err)
	}
	if said := errOut.String(); said != "" {
		t.Errorf("a raw .bin declares no CRC, so there is nothing to warn about: %q", said)
	}
}

// ---------------------------------------------------------------------------
// --page-size
// ---------------------------------------------------------------------------

// TestPageSizeReachesTheLoader: the flag is half the guard against a wrongly
// split raw image (the other half is the loader's geometry validation), and a
// wrong split can flash and even verify cleanly (SPEC.md §10.2, §14.12), so it
// has to actually arrive in bootloader.LoadOptions.PageSize -- and the default
// must stay DefaultPageSize when it is unset.
func TestPageSizeReachesTheLoader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fw.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0x33}, 256), 0o600); err != nil {
		t.Fatal(err)
	}
	app, _, _ := newFlashTestApp()
	o := baseOpts()
	o.path = path

	fw, err := app.loadFirmware(context.Background(), o, "")
	if err != nil {
		t.Fatalf("loadFirmware: %v", err)
	}
	if fw.PageSize() != bootloader.DefaultPageSize || len(fw.Pages) != 1 {
		t.Errorf("with --page-size unset: %d pages of %d bytes, want 1 of the %d-byte default",
			len(fw.Pages), fw.PageSize(), bootloader.DefaultPageSize)
	}

	o.pageSize = 128
	fw, err = app.loadFirmware(context.Background(), o, "")
	if err != nil {
		t.Fatalf("loadFirmware with --page-size 128: %v", err)
	}
	if fw.PageSize() != 128 || len(fw.Pages) != 2 {
		t.Errorf("with --page-size 128: %d pages of %d bytes, want 2 of 128", len(fw.Pages), fw.PageSize())
	}
}

// TestPageSizeOnAJSONImageIsAUsageError: the object payload takes its split
// from its own page_size field and ignores LoadOptions.PageSize entirely, so
// accepting the flag there would silently discard the one flag whose purpose is
// geometry safety. It is refused as a usage error instead. (The refusal is
// deliberately wider than that -- it covers the bare-array shape too, where the
// loader would honour the flag for a flat image; see loadFirmware for why the
// sniff does not try to tell those apart.)
func TestPageSizeOnAJSONImageIsAUsageError(t *testing.T) {
	// Leading whitespace on purpose: format detection tolerates it
	// (bootloader.ParseImage trims before sniffing), so the refusal must too.
	img := "\n\t {\"app_bin\": [[1, 2, 3, 4, 5, 6, 7, 8]], \"app_version\": \"5.1.0\", \"crc\": 9}"
	path := filepath.Join(t.TempDir(), "fw.json")
	if err := os.WriteFile(path, []byte(img), 0o600); err != nil {
		t.Fatal(err)
	}
	app, _, _ := newFlashTestApp()
	o := baseOpts()
	o.path = path

	// Without the flag the same payload loads normally.
	if _, err := app.loadFirmware(context.Background(), o, ""); err != nil {
		t.Fatalf("the JSON payload does not load at all: %v", err)
	}

	o.pageSize = 128
	_, err := app.loadFirmware(context.Background(), o, "")
	if err == nil {
		t.Fatal("--page-size on a JSON image was silently ignored")
	}
	if code := ExitCode(err); code != ExitUsage {
		t.Errorf("exit code %d, want ExitUsage (%d)", code, ExitUsage)
	}
	if !strings.Contains(err.Error(), "page split") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestPageSizeInvalidValueSurfacesTheLoadersError: the geometry rules live in
// the bootloader package alone -- the CLI must not duplicate them -- and its
// error, which names the exact rule violated, has to reach the user verbatim.
func TestPageSizeInvalidValueSurfacesTheLoadersError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fw.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0x44}, 200), 0o600); err != nil {
		t.Fatal(err)
	}
	app, _, _ := newFlashTestApp()
	o := baseOpts()
	o.path = path
	o.pageSize = 100 // not divisible by ChunksPerPage

	_, err := app.loadFirmware(context.Background(), o, "")
	if err == nil {
		t.Fatal("a page size the bootloader cannot chunk was accepted")
	}
	if !errors.Is(err, bootloader.ErrBadPageLength) {
		t.Errorf("the loader's sentinel was lost: %v", err)
	}
	if !strings.Contains(err.Error(), "not divisible by") {
		t.Errorf("the loader's own wording was not surfaced verbatim: %v", err)
	}
}

// TestPageSizeFlagUsageErrors drives the real cobra command for the two
// refusals that happen before any file or device is touched: --page-size with
// --fetch (the fetched payload carries its own split, SPEC.md §10.3), and a
// negative size (which the library would silently treat as "unset" -- the one
// direction an explicit geometry instruction must never take).
func TestPageSizeFlagUsageErrors(t *testing.T) {
	run := func(t *testing.T, args ...string) error {
		t.Helper()
		app, _, _ := newFlashTestApp()
		cmd := newFirmwareFlashCommand(app)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		return cmd.ExecuteContext(context.Background())
	}

	t.Run("with --fetch", func(t *testing.T) {
		err := run(t, "--fetch", "--page-size", "512")
		if err == nil {
			t.Fatal("--page-size with --fetch was silently accepted")
		}
		if code := ExitCode(err); code != ExitUsage {
			t.Errorf("exit code %d, want ExitUsage (%d)", code, ExitUsage)
		}
		// "own page split" pins the message to the new check rather than to
		// cobra's unknown-flag complaint, which also exits ExitUsage.
		if !strings.Contains(err.Error(), "own page split") {
			t.Errorf("the refusal does not say why: %v", err)
		}
	})

	t.Run("negative", func(t *testing.T) {
		err := run(t, "fw.bin", "--page-size=-8")
		if err == nil {
			t.Fatal("a negative --page-size was silently accepted")
		}
		if code := ExitCode(err); code != ExitUsage {
			t.Errorf("exit code %d, want ExitUsage (%d)", code, ExitUsage)
		}
		if !strings.Contains(err.Error(), "--page-size must be positive") {
			t.Errorf("the refusal does not name the flag: %v", err)
		}
	})
}

// TestPageSizeIsPlainDecimal pins the base the flag is parsed in. It was
// registered with pflag's IntVar, which calls strconv.ParseInt(s, 0, ...) --
// Go literal syntax -- so `--page-size 0200` meant octal 128. Both 128 and 200
// are geometries the loader accepts, so nothing downstream could catch the
// substitution, on the one flag whose wrong value can flash and even verify
// cleanly (SPEC.md §10.2, §14.12). Restoring IntVar makes the page count 4 in
// the first subtest and accepts the hex spelling in the second.
// --crc is compared, never applied, so a misparse does not fail loudly: it can
// match what the device answers for a reason the user never intended. pflag's
// base-0 integer flag read `--crc 017` as 15.
func TestCRCIsNotSilentlyOctal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fw.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0x77}, 512), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("a leading zero is refused rather than read as octal", func(t *testing.T) {
		tr := newFakeTree(t, fake.NewTypical())
		err := tr.run(t, "firmware", "flash", path, "--crc", "017", "--dry-run")
		if err == nil {
			t.Fatal("--crc 017 was accepted; it would have meant 15")
		}
		if code := ExitCode(err); code != ExitUsage {
			t.Errorf("exit code %d, want ExitUsage (%d)", code, ExitUsage)
		}
		if !strings.Contains(err.Error(), "not octal") {
			t.Errorf("the refusal does not say what was wrong: %v", err)
		}
	})

	t.Run("decimal and explicit hex both reach the loader", func(t *testing.T) {
		for _, tc := range []struct {
			arg  string
			want int
		}{
			{"90", 90},
			{"0x5a", 90},
			{"0X5A", 90},
			{"0", 0},
			{"255", 255},
		} {
			tr := newFakeTree(t, fake.NewTypical())
			if err := tr.run(t, "firmware", "flash", path, "--crc", tc.arg, "--dry-run", "--json"); err != nil {
				t.Fatalf("--crc %s: %v\n%s", tc.arg, err, tr.stderr.String())
			}
			var doc struct {
				CRC *int `json:"crc"`
			}
			if err := json.Unmarshal(tr.stdout.Bytes(), &doc); err != nil {
				t.Fatalf("--crc %s: the dry run did not emit a JSON object (%v):\n%s", tc.arg, err, tr.stdout.String())
			}
			if doc.CRC == nil {
				t.Fatalf("--crc %s: the dry run reported no expected CRC:\n%s", tc.arg, tr.stdout.String())
			}
			if *doc.CRC != tc.want {
				t.Errorf("--crc %s reached the loader as %d, want %d", tc.arg, *doc.CRC, tc.want)
			}
		}
	})

	t.Run("out of range is refused", func(t *testing.T) {
		tr := newFakeTree(t, fake.NewTypical())
		err := tr.run(t, "firmware", "flash", path, "--crc", "256", "--dry-run")
		if err == nil {
			t.Fatal("--crc 256 was accepted")
		}
		if code := ExitCode(err); code != ExitUsage {
			t.Errorf("exit code %d, want ExitUsage (%d)", code, ExitUsage)
		}
	})
}

func TestPageSizeIsPlainDecimal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fw.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0x77}, 400), 0o600); err != nil {
		t.Fatal(err)
	}

	// --dry-run reaches the loader and stops before any device is opened;
	// --crc gives the raw image the CRC CheckFlash insists on before it will
	// agree to a flash at all.
	t.Run("a leading zero is not octal", func(t *testing.T) {
		tr := newFakeTree(t, fake.NewTypical())
		if err := tr.run(t, "firmware", "flash", path,
			"--page-size", "0200", "--crc", "90", "--dry-run", "--json"); err != nil {
			t.Fatalf("dry-run flash: %v\n%s", err, tr.stderr.String())
		}
		var doc struct {
			Pages int `json:"pages"`
		}
		if err := json.Unmarshal(tr.stdout.Bytes(), &doc); err != nil {
			t.Fatalf("the dry run did not emit a JSON object (%v):\n%s", err, tr.stdout.String())
		}
		// 400 bytes at 200 is 2 pages; read as octal 128 it would be 4.
		if doc.Pages != 2 {
			t.Errorf("pages = %d, want 2: --page-size 0200 was not read as decimal 200", doc.Pages)
		}
	})

	t.Run("hex is refused", func(t *testing.T) {
		tr := newFakeTree(t, fake.NewTypical())
		err := tr.run(t, "firmware", "flash", path,
			"--page-size", "0x200", "--crc", "90", "--dry-run")
		if err == nil {
			t.Fatal("--page-size 0x200 was accepted")
		}
		if code := ExitCode(err); code != ExitUsage {
			t.Errorf("exit code %d, want ExitUsage (%d)", code, ExitUsage)
		}
		if !strings.Contains(err.Error(), "not a plain decimal number") {
			t.Errorf("the refusal does not say what was wrong: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// what a jump can actually be said to prove
// ---------------------------------------------------------------------------

// TestUnconfirmedJumpNoteDoesNotGuess is the regression test for the false
// reassurance: "the unit should still be in application mode" was printed
// whenever any VFLEX port was still visible, including when the visible port
// belonged to a second unit and ours had in fact jumped.
func TestUnconfirmedJumpNoteDoesNotGuess(t *testing.T) {
	const ours = "VFX-0001"

	same := unconfirmedJumpNote(ours, ours)
	if !strings.Contains(same, "application mode") || !strings.Contains(same, ours) {
		t.Errorf("the unit that answered as ours is not identified: %s", same)
	}

	other := unconfirmedJumpNote(ours, "VFX-0002")
	if strings.Contains(other, "still answering in application mode") {
		t.Errorf("a different unit's port was read as proof ours did not jump: %s", other)
	}
	for _, want := range []string{"VFX-0002", ours, "--recover"} {
		if !strings.Contains(other, want) {
			t.Errorf("the second-unit case does not mention %q: %s", want, other)
		}
	}

	unknown := unconfirmedJumpNote(ours, "")
	if !strings.Contains(unknown, "unknown") {
		t.Errorf("an unidentifiable port should be reported as unknown, not guessed: %s", unknown)
	}
}

// TestJumpIsNotClaimedConfirmedOnUSBTransport pins the fix for a proof that did
// not prove anything: the jump is confirmed by watching the MIDI port leave the
// bus, but --transport usb detaches the kernel MIDI driver itself, so the port
// is absent either way (SPEC.md §4.2). The flash continues -- the serial check
// in the bootloader is what guards the write -- but the user is told plainly
// that nothing was confirmed.
func TestJumpIsNotClaimedConfirmedOnUSBTransport(t *testing.T) {
	app, _, errOut := newFlashTestApp()
	app.Transport = transportUSB
	if app.midiPresenceMeaningful() {
		t.Fatal("the ALSA node is treated as meaningful on --transport usb")
	}
	watched := func(context.Context, bool, time.Duration) error {
		t.Error("the ALSA node was polled on a transport whose answer means nothing")
		return nil
	}
	if err := app.confirmJump(context.Background(), app.newFormatter(), "VFX-0001", watched); err != nil {
		t.Fatalf("confirmJump aborted the update on a transport that simply cannot observe it: %v", err)
	}
	said := errOut.String()
	if !strings.Contains(said, "cannot be confirmed") {
		t.Errorf("the unconfirmable jump was not reported as such: %q", said)
	}

	errOut.Reset()
	app.Transport = transportUSB
	app.reportJumpEvidence(context.Background(), app.newFormatter(), watched)
	if !strings.Contains(errOut.String(), "NOT be confirmed") {
		t.Errorf("`firmware bootloader` implied a confirmation it never had: %q", errOut.String())
	}
}

// TestAnInterruptedDisconnectWaitIsNotReportedAsATimeout: waitForDevice returns
// the context's own error when a Ctrl-C lands in the poll, and both callers
// treated every error as the three-second timeout. On the flash path that
// produced a message claiming an observation that was never made, promising
// nothing had been flashed, and then reconnecting with the dead context purely
// to print "could not identify the VFLEX still on the bus: context canceled" --
// for a unit that by then had almost certainly jumped. On the `firmware
// bootloader` path it produced the same false three-second claim.
//
// The jump frame goes out before either wait begins, so neither outcome is a
// failed jump: confirmJump hands the cancellation back with its chain intact,
// and reportJumpEvidence stays a note.
func TestAnInterruptedDisconnectWaitIsNotReportedAsATimeout(t *testing.T) {
	interrupted := func(context.Context, bool, time.Duration) error {
		return context.Canceled
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	app, _, errOut := newFlashTestApp()
	err := app.confirmJump(ctx, app.newFormatter(), "VFX-0001", interrupted)
	if err == nil {
		t.Fatal("an interrupted confirmation was reported as a confirmed jump")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the interruption's own error was lost from the chain: %v", err)
	}
	if msg := err.Error(); strings.Contains(msg, "did not disconnect") {
		t.Errorf("a Ctrl-C was reported as a jump that did not take: %s", msg)
	}
	if said := errOut.String(); strings.Contains(said, "could not identify") {
		t.Errorf("the dead context was used for a diagnostic read: %q", said)
	}

	// The timeout it really is must keep its own wording, or the branch above
	// would have swallowed the case it exists to distinguish.
	errOut.Reset()
	timedOut := func(context.Context, bool, time.Duration) error {
		return codedf(ExitFailure, "timed out after %s waiting for the VFLEX to disconnect", disconnectTimeout)
	}
	err = app.confirmJump(context.Background(), app.newFormatter(), "VFX-0001", timedOut)
	if err == nil || !strings.Contains(err.Error(), "did not disconnect") {
		t.Errorf("a genuine timeout no longer says the jump was unconfirmed: %v", err)
	}

	errOut.Reset()
	app.reportJumpEvidence(ctx, app.newFormatter(), interrupted)
	said := errOut.String()
	if strings.Contains(said, "still visible") {
		t.Errorf("`firmware bootloader` claimed a %s observation it never made: %q", disconnectTimeout, said)
	}
	if !strings.Contains(said, "interrupted") {
		t.Errorf("the interruption was not reported at all: %q", said)
	}
}

// ---------------------------------------------------------------------------
// opening the bootloader interface: exactly one retry loop
// ---------------------------------------------------------------------------

// TestBootloaderConnectIsNotWrappedInASecondRetryLoop is the regression test for
// two retry budgets stacked on each other. bootloader.Connect retries for
// bootloader.ConnectRetryWindow itself and burns that whole window before it
// ever returns an error; the CLI wrapped it in a further 15 s / 500 ms loop, so
// the real wait was one window plus another full pass, and the failure quoted
// two different budgets in one message. Nothing here may call Connect more than
// once or sit on a timer of its own.
func TestBootloaderConnectIsNotWrappedInASecondRetryLoop(t *testing.T) {
	var calls int
	stubConnect(t, func(context.Context) (*usbfs.Device, usbfs.Interface, error) {
		calls++
		return nil, usbfs.Interface{}, errors.New("bootloader: no device with vendor 0x37BF is attached")
	})

	app, _, errOut := newFlashTestApp()
	start := time.Now()
	_, _, err := openBootloaderInterface(context.Background(), app.newFormatter(), false)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a failed connect was not reported as an error")
	}
	if calls != 1 {
		t.Errorf("bootloader.Connect called %d times; it owns the retry loop, so the CLI calls it once", calls)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("openBootloaderInterface waited %s of its own; the retry window belongs to Connect", elapsed)
	}
	if code := ExitCode(err); code != ExitNoDevice {
		t.Errorf("exit code %d, want ExitNoDevice (%d)", code, ExitNoDevice)
	}
	msg := err.Error()
	if !strings.Contains(msg, "0x37BF") {
		t.Errorf("the underlying reason was dropped: %s", msg)
	}
	if !strings.Contains(msg, "install-udev") {
		t.Errorf("the fix for a raw-USB interface is missing: %s", msg)
	}
	// The CLI must not quote a window it does not own: Connect names its own in
	// the error being wrapped, and two numbers in one message is the confusion
	// this fix removes.
	if strings.Contains(msg, "after 15s") {
		t.Errorf("the CLI still advertises a retry budget of its own: %s", msg)
	}
	if !strings.Contains(errOut.String(), bootloader.ConnectRetryWindow.String()) {
		t.Errorf("the user was not told how long the silent wait lasts: %q", errOut.String())
	}
}

// TestBootloaderConnectInterruptionIsNotReportedAsAMissingDevice: Connect
// returns the context's own error when a Ctrl-C lands in the middle of its
// wait. Dressing that up as ExitNoDevice would tell a user their unit had
// vanished when in fact they interrupted the flash themselves.
func TestBootloaderConnectInterruptionIsNotReportedAsAMissingDevice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stubConnect(t, func(c context.Context) (*usbfs.Device, usbfs.Interface, error) {
		cancel()
		return nil, usbfs.Interface{}, c.Err()
	})

	app, _, _ := newFlashTestApp()
	_, _, err := openBootloaderInterface(ctx, app.newFormatter(), false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the interruption was swallowed: %v", err)
	}
	if code := ExitCode(err); code == ExitNoDevice {
		t.Error("an interrupted connect was reported as a missing device")
	}
}

// TestBootloaderOpenFailuresKeepTheirClass: the bootloader is raw usbfs on
// /dev/bus/usb, which is exactly what `gflex install-udev` exists for, so the
// missing-rule case is the ordinary one here -- and it exited 3, "no device
// found", while the same cause on `gflex --transport usb info` exits 6. README
// calls the codes stable enough to branch on, so a script that runs
// install-udev on 6 never fired. Two things caused it, and both are fixed here:
// %v flattened the chain so no sentinel survived, and an explicit CodedError
// wins over every classification ExitCode would have made anyway.
func TestBootloaderOpenFailuresKeepTheirClass(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode int
		wantIs   error
		udev     bool // whether a udev rule is the fix
	}{
		{
			name: "a missing udev rule",
			err: fmt.Errorf("bootloader: opening /dev/bus/usb/001/004: %w",
				&usbfs.Error{Op: "open", Path: "/dev/bus/usb/001/004", Errno: syscall.EACCES,
					Class: usbfs.ErrPermission}),
			wantCode: ExitPermission,
			wantIs:   usbfs.ErrPermission,
			udev:     true,
		},
		{
			name: "an interface someone else holds",
			err: fmt.Errorf("bootloader: claiming interface 0: %w",
				&usbfs.Error{Op: "claim", Errno: syscall.EBUSY, Class: usbfs.ErrBusy}),
			wantCode: ExitBusy,
			wantIs:   usbfs.ErrBusy,
		},
		{
			// --recover against a unit that never left its application. The
			// open worked, so a udev rule is not the fix, and the package's own
			// message already names both ways forward.
			name:     "a unit still running its application",
			err:      fmt.Errorf("bootloader: %w: /dev/bus/usb/001/004 is running its application", bootloader.ErrApplicationMode),
			wantCode: ExitFailure,
			wantIs:   bootloader.ErrApplicationMode,
		},
		{
			name:     "no VFLEX at all",
			err:      errors.New("bootloader: no device with vendor 0x37BF is attached"),
			wantCode: ExitNoDevice,
			udev:     true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubConnect(t, func(context.Context) (*usbfs.Device, usbfs.Interface, error) {
				return nil, usbfs.Interface{}, tc.err
			})
			app, _, _ := newFlashTestApp()
			_, _, err := openBootloaderInterface(context.Background(), app.newFormatter(), false)
			if err == nil {
				t.Fatal("a failed connect was not reported as an error")
			}
			if code := ExitCode(err); code != tc.wantCode {
				t.Errorf("exit code %d, want %d: %v", code, tc.wantCode, err)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("the %v sentinel did not survive the wrap: %v", tc.wantIs, err)
			}
			if got := strings.Contains(err.Error(), "install-udev"); got != tc.udev {
				t.Errorf("udev advice present = %v, want %v: %v", got, tc.udev, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SPEC.md §10.4: the forced vlimit rewrite on a major 4 -> 5 jump
// ---------------------------------------------------------------------------

// TestVLimitRewriteIsForcedOnlyOnAKnownCrossing pins the decision that used to
// be unreachable: PostUpdateInitForce's forceVLimit parameter had no caller
// passing true, so SPEC.md §10.4's "or on a major 4 -> 5 jump" was unimplemented.
//
// The direction of the uncertainty is the point of this table. A forced rewrite
// installs the 3300/48000 default, the widest window there is, over whatever the
// user chose -- so it happens only when both versions are known and the crossing
// is established. Anything unreadable or unparseable on either side means "do
// not force", and the plausibility check inside PostUpdateInitForce still
// rewrites an erased window on its own.
func TestVLimitRewriteIsForcedOnlyOnAKnownCrossing(t *testing.T) {
	for _, tc := range []struct {
		before, after string
		want          bool
		why           string
	}{
		{"4.9.2", "5.0.0", true, "the crossing SPEC.md §10.4 names"},
		{"4.0.0", "6.1.0", true, "4 -> 6 crosses the same boundary"},
		{"v4.9", "FW 5.0.0-rc1", true, "the vendor's parse takes the decimal runs (SPEC.md §10.3)"},
		{"4.9.2", "4.9.3", false, "no major change"},
		{"5.0.1", "5.1.0", false, "already past the boundary"},
		{"5.1.0", "4.9.0", false, "a downgrade does not cross upwards"},
		{"", "5.0.0", false, "the version before the flash is unknown (--recover, or a failed read)"},
		{"4.9.2", "", false, "the version after the flash could not be read"},
		{"unknown", "5.0.0", false, "nothing numeric to compare"},
		{"4.9.2", "no digits here", false, "nothing numeric to compare"},
		{"", "", false, "neither side known"},
	} {
		if got := crossesToMajor5(tc.before, tc.after); got != tc.want {
			t.Errorf("crossesToMajor5(%q, %q) = %v, want %v -- %s", tc.before, tc.after, got, tc.want, tc.why)
		}
	}
}

// TestForcedVLimitDecisionIsAnnounced: both the rewrite and the case where the
// rule could not be evaluated are stated on stderr. Silently widening the guard
// rail, or silently not applying a mandatory step, are both things a user has to
// be able to see afterwards.
func TestForcedVLimitDecisionIsAnnounced(t *testing.T) {
	app, _, errOut := newFlashTestApp()
	if !decideForcedVLimit(app.newFormatter(), "4.9.2", "5.0.0") {
		t.Fatal("a 4 -> 5 crossing did not force the vlimit rewrite")
	}
	said := errOut.String()
	for _, want := range []string{"4.9.2", "5.0.0", "§10.4", "3300", "48000"} {
		if !strings.Contains(said, want) {
			t.Errorf("the forced rewrite was not explained (missing %q): %q", want, said)
		}
	}

	errOut.Reset()
	if decideForcedVLimit(app.newFormatter(), "", "5.0.0") {
		t.Fatal("an unknown pre-flash version forced the rewrite; unknown must mean 'do not force'")
	}
	if said := errOut.String(); !strings.Contains(said, "unknown") || !strings.Contains(said, "vlimit get") {
		t.Errorf("an unevaluated §10.4 rule was not reported: %q", said)
	}

	errOut.Reset()
	if decideForcedVLimit(app.newFormatter(), "5.0.0", "5.0.1") {
		t.Fatal("a same-major update forced the rewrite")
	}
	if said := errOut.String(); said != "" {
		t.Errorf("an ordinary update printed a §10.4 note it had no reason to: %q", said)
	}
}

// TestForcedVLimitReachesTheDevice drives the real session against an in-memory
// VFLEX, so the wiring is checked end to end rather than at the boundary: with
// the flag off a plausible window the flash left intact is kept, and with it on
// the defaults are written over it.
func TestForcedVLimitReachesTheDevice(t *testing.T) {
	// A narrow-ish window that VLimitPlausible accepts, so nothing but the force
	// flag can cause it to be rewritten.
	kept := proto.EncodeVLimit(4000, 12000)

	t.Run("not forced", func(t *testing.T) {
		dev := fake.NewTypical()
		defer dev.Close()
		dev.StoreRegister(proto.CmdUserVLimit, kept)
		app, _, _ := newFlashTestApp()

		rep := replaySettings(context.Background(), app.newFormatter(), newReplaySession(t, dev), false)
		if containsLine(rep.restored, "set vlimit") {
			t.Errorf("a plausible window was rewritten without being asked: %v", rep.restored)
		}
		if got, _ := dev.Register(proto.CmdUserVLimit); !bytes.Equal(got, kept) {
			t.Errorf("vlimit register = % X, want % X (the user's window, untouched)", got, kept)
		}
	})

	t.Run("forced", func(t *testing.T) {
		dev := fake.NewTypical()
		defer dev.Close()
		dev.StoreRegister(proto.CmdUserVLimit, kept)
		app, _, _ := newFlashTestApp()

		rep := replaySettings(context.Background(), app.newFormatter(), newReplaySession(t, dev), true)
		if !containsLine(rep.restored, "set vlimit") {
			t.Errorf("the forced rewrite never happened: %v", rep.restored)
		}
		want := proto.EncodeVLimit(proto.DefaultVLimitLowMv, proto.DefaultVLimitHighMv)
		if got, _ := dev.Register(proto.CmdUserVLimit); !bytes.Equal(got, want) {
			t.Errorf("vlimit register = % X, want the % X defaults of SPEC.md §10.4", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// honest reporting when the flash worked and the replay did not
// ---------------------------------------------------------------------------

// TestPartialReplayIsReportedAsPartial is the regression test for a flash that
// verified being reported as if it had not.
//
// The §10.4 replay is error-tolerant: a step that fails is narrated through the
// log callback and the sequence carries on. Collecting those lines into one
// list called "restored" counted the failures as successes, and left the user
// with no way to see which setting is now sitting at whatever the flash left it
// as. Because the classification reads the session's own wording, this test
// drives the real session so it cannot drift away from it unnoticed.
func TestPartialReplayIsReportedAsPartial(t *testing.T) {
	dev := fake.NewTypical()
	defer dev.Close()
	// The device stops answering the current-limit write, which on a protocol
	// with no NACK is what a refusal looks like too (SPEC.md §5.2).
	dev.SetFault(proto.CmdCurrentLimitMa, fake.Fault{Drop: true})

	app, _, errOut := newFlashTestApp()
	rep := replaySettings(context.Background(), app.newFormatter(), newReplaySession(t, dev), false)

	if !containsLine(rep.failed, "current limit") {
		t.Errorf("the step that failed is not reported as failed: %v", rep.failed)
	}
	if containsLine(rep.restored, "current limit") {
		t.Errorf("a setting that was never written is counted as restored: %v", rep.restored)
	}
	if containsLine(rep.restored, "failed") {
		t.Errorf("a failure line was counted as a restored setting: %v", rep.restored)
	}
	if !containsLine(rep.restored, "set authlock 0") {
		t.Errorf("the steps that did take are not reported: %v", rep.restored)
	}
	if !rep.incomplete() {
		t.Error("a replay with a failed step did not report itself as incomplete")
	}

	// And the wording: firmware good, settings bad, and no invitation to reflash.
	errOut.Reset()
	reportReplayIncomplete(app.newFormatter(), rep, &bootloader.UpdateResult{CRC: 0x5A, CRCChecked: true})
	said := errOut.String()
	for _, want := range []string{"SUCCEEDED", "CRC verified", "current limit", "§10.4", "Do NOT re-flash", "info --all"} {
		if !strings.Contains(said, want) {
			t.Errorf("the partial-success report is missing %q: %q", want, said)
		}
	}
}

// TestPartialReplayDoesNotClaimAnUnverifiedImageWasVerified: --force flashes an
// image that carries no CRC, and nothing checked what landed (SPEC.md §10.2,
// §14.12). The reassurance that the firmware is fine must not overstate what was
// established.
func TestPartialReplayDoesNotClaimAnUnverifiedImageWasVerified(t *testing.T) {
	app, _, errOut := newFlashTestApp()
	rep := replayReport{failed: []string{"set current limit 5000 mA failed: response timeout exceeded"}}
	reportReplayIncomplete(app.newFormatter(), rep, &bootloader.UpdateResult{Unverified: true})
	said := errOut.String()
	if strings.Contains(said, "CRC verified") {
		t.Errorf("an unverified flash was reported as verified: %q", said)
	}
	if !strings.Contains(said, "no CRC") {
		t.Errorf("the report does not say the image was never verified: %q", said)
	}
}

// TestCompleteReplayReportsNothingExtra: the partial-success wording exists for
// the partial case only. Printing it after a clean run would train the reader to
// skip it.
func TestCompleteReplayReportsNothingExtra(t *testing.T) {
	dev := fake.NewTypical()
	defer dev.Close()
	app, _, errOut := newFlashTestApp()

	rep := replaySettings(context.Background(), app.newFormatter(), newReplaySession(t, dev), false)
	if len(rep.failed) != 0 {
		t.Fatalf("a healthy device failed a replay step: %v", rep.failed)
	}
	if rep.incomplete() {
		t.Error("a complete replay reported itself as incomplete")
	}
	errOut.Reset()
	reportReplayIncomplete(app.newFormatter(), rep, &bootloader.UpdateResult{CRCChecked: true})
	if said := errOut.String(); said != "" {
		t.Errorf("a complete replay printed a partial-success warning: %q", said)
	}
}

// TestInterruptionAfterASuccessfulFlashReportsSuccess is the regression test
// for the two bare returns between CMD_BOOTLOAD_END and the §10.4 replay. A
// Ctrl-C landing in the post-jump settle used to surface as nothing but
// "gflex: interrupted" (Execute prints only that line for an error chained to
// context.Canceled, and Flush -- with the buffered crc line -- runs only on
// success), which reads as a failed update and sends someone to re-flash
// firmware that verified. So the succeeded wording, the CRC evidence and the
// manual §10.4 instructions must reach stderr immediately, before the error is
// returned.
func TestInterruptionAfterASuccessfulFlashReportsSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stubUpdate(t, func(_ context.Context, _ *usbfs.Device, _ usbfs.Interface, _ *bootloader.Firmware,
		_ bootloader.UpdateOptions,
	) (*bootloader.UpdateResult, error) {
		// The update finishes -- CRC verified, CMD_BOOTLOAD_END sent -- and
		// the user's Ctrl-C lands immediately after, in the post-jump settle.
		cancel()
		return &bootloader.UpdateResult{Serial: "VFX-0001", CRC: 0x5A, CRCChecked: true, JumpedToApp: true}, nil
	})

	app, _, errOut := newFlashTestApp()
	f := app.newFormatter()
	res, err := app.runUpdate(ctx, f, nil, usbfs.Interface{}, flashTestImage(), baseOpts(), "VFX-0001")
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}

	err = app.awaitApplicationReturn(ctx, f, baseOpts(), res)
	if err == nil {
		t.Fatal("an interrupted post-jump wait did not report an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the interruption's own error was lost from the chain: %v", err)
	}
	if code := ExitCode(err); code == ExitOK {
		t.Error("an interrupted command must still exit non-zero")
	}
	said := errOut.String()
	for _, want := range []string{
		"SUCCEEDED",        // (a) the update itself worked
		"0x5A",             // the crc-verified evidence, since Flush never runs
		"Do NOT re-flash",  // and the instruction that follows from (a)
		"§10.4",            // (b) what was skipped: the settings replay...
		"firmware version", // ...and the version read-back
		"info --all",       // (b) how to check by hand
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the post-success interruption report is missing %q: %q", want, said)
		}
	}
}

// TestPostFlashFailureKeepsTheFailuresIdentity: the rewording must not eat the
// original error -- a slow hub still exits ExitNoDevice so scripts can branch
// on it, the error text itself also names the success (for the paths where
// Execute prints it), and the generic no-device hint is suppressed, because
// "check the cable" guidance under a successful update reads as a failed one.
func TestPostFlashFailureKeepsTheFailuresIdentity(t *testing.T) {
	app, _, errOut := newFlashTestApp()
	cause := codedf(ExitNoDevice, "no device with vendor 0x37BF came back on the USB bus within 15s after the jump")
	err := app.postFlashFailure(app.newFormatter(), baseOpts(),
		&bootloader.UpdateResult{CRC: 0x5A, CRCChecked: true, JumpedToApp: true},
		"waiting for the unit to come back in application mode", cause)

	if err == nil {
		t.Fatal("a post-flash failure was not reported as an error")
	}
	if code := ExitCode(err); code != ExitNoDevice {
		t.Errorf("exit code %d, want ExitNoDevice (%d): the failure class must survive the rewording", code, ExitNoDevice)
	}
	if !strings.Contains(err.Error(), "SUCCEEDED") {
		t.Errorf("the returned error does not name the success: %v", err)
	}
	if !suppressHint(err) {
		t.Error("the generic hint was not suppressed; it would diagnose a missing device after a successful update")
	}
	if said := errOut.String(); !strings.Contains(said, "CRC verified") && !strings.Contains(said, "verified CRC") {
		t.Errorf("the crc-verified evidence was not printed before returning: %q", said)
	}
}

// TestPostFlashFailureDoesNotClaimAnUnverifiedImageWasVerified: --force
// flashes an image that carries no CRC, and nothing checked what landed
// (SPEC.md §10.2, §14.12); the reassurance must not overstate what was
// established, exactly as reportReplayIncomplete's wording is pinned not to.
func TestPostFlashFailureDoesNotClaimAnUnverifiedImageWasVerified(t *testing.T) {
	app, _, errOut := newFlashTestApp()
	_ = app.postFlashFailure(app.newFormatter(), baseOpts(),
		&bootloader.UpdateResult{Unverified: true, JumpedToApp: true},
		"reconnecting to the unit", errors.New("no device"))
	said := errOut.String()
	if strings.Contains(said, "CRC verified") || strings.Contains(said, "verified CRC") {
		t.Errorf("an unverified flash was reported as verified: %q", said)
	}
	if !strings.Contains(said, "no CRC") {
		t.Errorf("the report does not say the image was never verified: %q", said)
	}
	// The honesty above ended one line too early: "Do NOT re-flash the unit for
	// this" rests on a unit whose firmware is known good, which is precisely
	// what --force did not establish. A unit that does not come back after
	// being started on an unverified image is the case where the "still
	// re-flashable" reassurance stops applying on its own, and re-flashing is
	// then the fix rather than the mistake (SPEC.md §10.5, §13.6).
	if strings.Contains(said, "Do NOT re-flash") {
		t.Errorf("an unverified image that did not come back was protected from a re-flash: %q", said)
	}
	for _, want := range []string{bootloaderLEDNote, "--recover"} {
		if !strings.Contains(said, want) {
			t.Errorf("the report does not say how to get an unresponsive unit back (missing %q): %q",
				want, said)
		}
	}
}

// TestAnUndeliveredJumpIsNotReportedAsAStartedImage: postFlashFailure's opening
// line asserts that the jump was sent, and the field that records the send is
// UpdateResult.JumpedToApp. Asserting it on any other authority is how a unit
// sitting in the bootloader comes to be described to its owner as running the
// new image, with "Do NOT re-flash" forbidding the one action that would get it
// out (SPEC.md §10.5).
func TestAnUndeliveredJumpIsNotReportedAsAStartedImage(t *testing.T) {
	app, _, errOut := newFlashTestApp()
	o := baseOpts()
	o.path = "/tmp/fw.json"
	err := app.postFlashFailure(app.newFormatter(), o,
		&bootloader.UpdateResult{CRC: 0x5A, CRCChecked: true},
		"waiting out the post-jump settle", errors.New("interrupted"))

	said := errOut.String()
	if strings.Contains(said, "Do NOT re-flash") {
		t.Errorf("a unit still in the bootloader was told not to be re-flashed: %q", said)
	}
	for _, want := range []string{"CMD_BOOTLOAD_END", bootloaderLEDNote, "re-flashable", "--recover /tmp/fw.json"} {
		if !strings.Contains(said, want) {
			t.Errorf("the report does not say the unit never left the bootloader (missing %q): %q",
				want, said)
		}
	}
	if err == nil || strings.Contains(err.Error(), "SUCCEEDED") {
		t.Errorf("the returned error claims a completed update: %v", err)
	}
}

// ---------------------------------------------------------------------------
// progress rendering
// ---------------------------------------------------------------------------

// TestProgressWriter checks the two things the renderer has to get right: the
// phases that carry no chunk must not print one, and a log line interleaved
// with progress must not land on top of a half-drawn line.
func TestProgressWriter(t *testing.T) {
	var buf bytes.Buffer
	p := &progressWriter{w: &buf}

	p.clear() // nothing pending: must not emit a blank line
	if buf.Len() != 0 {
		t.Errorf("clear() with no pending progress wrote %q", buf.String())
	}

	p.report(bootloader.Progress{Phase: bootloader.PhaseSerial, Chunk: -1, TotalPages: 4})
	if s := buf.String(); strings.Contains(s, "-1") {
		t.Errorf("a phase with no chunk printed one: %q", s)
	}

	buf.Reset()
	p.report(bootloader.Progress{Phase: bootloader.PhaseChunk, Page: 0, TotalPages: 4, Chunk: 0, TotalChunks: 8})
	if s := buf.String(); !strings.Contains(s, "page 1/4") || !strings.Contains(s, "chunk 1/8") {
		t.Errorf("chunk progress is not one-based: %q", s)
	}
	p.clear()
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Error("clear() did not end the pending progress line")
	}
}

// TestFetchedImageSaveIsAtomic covers the success half of `firmware fetch -o`:
// the file lands with its full contents, is readable by the flash path that
// will later be handed it, carries 0644 (os.CreateTemp opens 0600, so the
// explicit Chmod is load-bearing), and leaves no temporary file behind.
func TestFetchedImageSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "image.json")

	app, _, _ := newFlashTestApp()
	fw := flashTestImage()
	if err := app.reportFetched(app.newFormatter(), fw, path); err != nil {
		t.Fatalf("reportFetched: %v", err)
	}

	// The point of saving is that `firmware flash <file>` can read it back.
	back, err := bootloader.LoadFile(path)
	if err != nil {
		t.Fatalf("the saved image does not load: %v", err)
	}
	if len(back.Pages) != len(fw.Pages) || back.PageSize() != fw.PageSize() {
		t.Errorf("round trip gave %d pages of %d bytes, want %d of %d",
			len(back.Pages), back.PageSize(), len(fw.Pages), fw.PageSize())
	}
	if !back.CRCKnown || back.CRC != fw.CRC {
		t.Errorf("round trip gave crc %#02x (known=%v), want %#02x", back.CRC, back.CRCKnown, fw.CRC)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %04o, want 0644 -- a firmware image is not a secret", perm)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory holds %v, want only the image", names)
	}
}

// TestASavedImageRoundTripsWhereBase64WouldNot pins the page encoding. Handing
// [][]byte to encoding/json renders each page as base64, and the loader's rule
// is that a string of nothing but hex digits IS hex -- which base64 can be. A
// page of zero bytes whose length is a multiple of 3 encodes to an unpadded run
// of 'A': all hex digits, even length, and read back as two thirds as many
// 0xAA bytes. The 320-byte pages the vendor service sends today always carry
// '=' padding and hide it; 240, 480 and 960 are equally valid eight-chunk
// geometries and do not.
//
// The two failures it produced are both here: a mixed image the tool could not
// read back at all, and an all-zero image that loaded cleanly as different
// bytes at a different geometry -- and would then have been flashed.
func TestASavedImageRoundTripsWhereBase64WouldNot(t *testing.T) {
	const pageSize = 480 // 8 chunks of 60 bytes, and divisible by 3

	for _, tc := range []struct {
		name  string
		pages [][]byte
	}{
		{
			name:  "one zero-filled page among others",
			pages: [][]byte{bytes.Repeat([]byte{0xA5}, pageSize), make([]byte, pageSize)},
		},
		{
			name:  "an image that is entirely zero",
			pages: [][]byte{make([]byte, pageSize), make([]byte, pageSize)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "image.json")
			app, _, _ := newFlashTestApp()
			fw := &bootloader.Firmware{Pages: tc.pages, Version: "5.1.0", CRC: 0x5A, CRCKnown: true}
			if err := app.reportFetched(app.newFormatter(), fw, path); err != nil {
				t.Fatalf("reportFetched: %v", err)
			}

			back, err := bootloader.LoadFile(path)
			if err != nil {
				t.Fatalf("the saved image does not load: %v", err)
			}
			if len(back.Pages) != len(fw.Pages) || back.PageSize() != pageSize {
				t.Fatalf("round trip gave %d pages of %d bytes, want %d of %d",
					len(back.Pages), back.PageSize(), len(fw.Pages), pageSize)
			}
			for i := range fw.Pages {
				if !bytes.Equal(back.Pages[i], fw.Pages[i]) {
					t.Errorf("page %d came back as different bytes (first %#02x, want %#02x)",
						i, back.Pages[i][0], fw.Pages[i][0])
				}
			}
		})
	}
}

// The half that matters: a save that fails must leave whatever was at that path
// alone. os.WriteFile truncates in place, so the old behaviour committed a
// zero-length file before writing a byte -- and this path is a trust input to
// `firmware flash` later, where a truncated-but-parseable image is one with
// pages missing, which can still flash and verify cleanly (SPEC.md §10.2).
//
// The failure is injected by taking write permission off the directory, which
// stops the temporary file from being created but leaves the destination file
// itself writable -- exactly the case an in-place os.WriteFile would sail
// through, rewriting the file and failing this test's first assertion.
func TestFetchedImageSaveLeavesThePreviousFileIntactWhenItFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the failure cannot be injected")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "image.json")
	const old = "{\"app_bin\": [[1, 2, 3, 4]], \"app_version\": \"5.0.0\"}\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	app, _, _ := newFlashTestApp()
	if err := app.reportFetched(app.newFormatter(), flashTestImage(), path); err == nil {
		t.Fatal("reportFetched reported success with an unwritable directory")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != old {
		t.Errorf("the previously saved image was damaged by a failed write:\n%q", got)
	}
}

// TestBootloaderFailFastOnlyUnderRecover pins which side of the flash asks
// Connect to give up early on a unit still running the application.
//
// Under --recover no jump was issued: the user is asserting the unit is already
// in the bootloader, so an application-mode answer is settled and re-asking for
// the full window only delays the message saying so. On the ordinary path a
// jump has just gone out and that same refusal is expected while the unit
// resets, so failing fast there would abandon a flash about to succeed.
func TestBootloaderFailFastOnlyUnderRecover(t *testing.T) {
	for _, tt := range []struct {
		name    string
		recover bool
		want    bool
	}{
		{"recover asserts nothing is in flight", true, true},
		{"a jump was just issued", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got bootloader.ConnectOptions
			stubConnectWithOptions(t, func(_ context.Context, opts bootloader.ConnectOptions) (*usbfs.Device, usbfs.Interface, error) {
				got = opts
				return nil, usbfs.Interface{}, errors.New("stub: no bootloader")
			})
			app, _, _ := newFlashTestApp()
			_, _, err := openBootloaderInterface(context.Background(), app.newFormatter(), tt.recover)
			if err == nil {
				t.Fatal("openBootloaderInterface succeeded against a stub that always fails")
			}
			if got.FailFastOnApplicationMode != tt.want {
				t.Errorf("FailFastOnApplicationMode = %v, want %v", got.FailFastOnApplicationMode, tt.want)
			}
		})
	}
}

// TestPresenceIsNotMeaningfulOnAPortOnlyThisRunIdentified pins the second half
// of midiPresenceMeaningful.
//
// devicePresent asks rawmidi.Discover, which classifies a port by vendor ID.
// The weaker tiers of SPEC.md §3.4 -- a name substring, or the sole port on the
// system -- belong to the selection openRawMIDI made and mean nothing to
// Discover, so a unit reached that way reads as absent the whole time it is
// attached. Treating the node as evidence there makes a jump look confirmed on
// an observation never made, and makes the post-flash wait for the unit's
// return one that can never be satisfied.
func TestPresenceIsNotMeaningfulOnAPortOnlyThisRunIdentified(t *testing.T) {
	tests := []struct {
		name      string
		transport string
		confirmed bool
		want      bool
	}{
		{"rawmidi, vendor ID identified the port", transportRawMIDI, true, true},
		{"rawmidi, matched by name or taken as the only port", transportRawMIDI, false, false},
		{"usb detaches the kernel driver either way", transportUSB, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := &App{Transport: tt.transport, midiPortVIDConfirmed: tt.confirmed}
			if got := app.midiPresenceMeaningful(); got != tt.want {
				t.Errorf("midiPresenceMeaningful() = %v, want %v", got, tt.want)
			}
		})
	}
}
