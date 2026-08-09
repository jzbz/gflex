package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func stubConnect(t *testing.T, fn connectFn) {
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
	if !res.CRCChecked || res.CRC != 0x5A {
		t.Errorf("result not passed through: %+v", res)
	}
}

// TestForceNeverSkipsVerification guards the one mapping that must not be made.
// --force exists to flash an image that carries no CRC (CheckFlash refuses that
// image otherwise); it must never turn into SkipVerify, which would skip
// verification on an image that does declare one -- the dangerous direction,
// because an unverified image is then jumped into.
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
	o.path = "/tmp/fw.json"
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
	for _, want := range []string{"--recover", "/tmp/fw.json"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure does not say how to resume (missing %q): %s", want, msg)
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
	if err := app.confirmJump(context.Background(), app.newFormatter(), "VFX-0001"); err != nil {
		t.Fatalf("confirmJump aborted the update on a transport that simply cannot observe it: %v", err)
	}
	said := errOut.String()
	if !strings.Contains(said, "cannot be confirmed") {
		t.Errorf("the unconfirmable jump was not reported as such: %q", said)
	}

	errOut.Reset()
	app.Transport = transportUSB
	app.reportJumpEvidence(context.Background(), app.newFormatter())
	if !strings.Contains(errOut.String(), "NOT be confirmed") {
		t.Errorf("`firmware bootloader` implied a confirmation it never had: %q", errOut.String())
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
	_, _, err := openBootloaderInterface(context.Background(), app.newFormatter())
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
	_, _, err := openBootloaderInterface(ctx, app.newFormatter())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the interruption was swallowed: %v", err)
	}
	if code := ExitCode(err); code == ExitNoDevice {
		t.Error("an interrupted connect was reported as a missing device")
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
