package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/transport/fake"
)

// ---------------------------------------------------------------------------
// Interlock wiring
// ---------------------------------------------------------------------------
//
// interlock_test.go proves the Check* functions decide correctly. These tests
// prove the commands ASK them. That is a separate property, and it was
// previously held by nothing: deleting the `app.apply(ctx, f, CheckVoltage(...))`
// line from `voltage set` -- the line that turns a Decision into a refusal --
// left the whole suite green while a 48 V write sailed past a 12 V window onto
// someone's pedal. SPEC.md §13 is a contract about what reaches the wire, so
// the assertions here are about frames the fake device did and did not receive.
//
// Everything above the byte link is the real thing: the cobra tree, the flag
// parsing, App.apply, the session and its framing. Only the transport is
// substituted, through the App.testTransport seam (see root.go).

// fakeTree drives the real command tree against an in-memory VFLEX.
type fakeTree struct {
	app    *App
	dev    *fake.Device
	stdout bytes.Buffer
	stderr bytes.Buffer
}

// newFakeTree wires dev into an App. stdin is deliberately not a terminal: that
// is the state a script runs in, and interlock 7 (SPEC.md §13.7) turns every
// confirmation into a refusal there unless --yes is passed, which is what makes
// "no write reached the device" a stable assertion.
func newFakeTree(t *testing.T, dev *fake.Device) *fakeTree {
	t.Helper()
	clearGflexEnv(t)
	tr := &fakeTree{dev: dev}
	tr.app = &App{
		stdin: strings.NewReader(""),
		testTransport: func(context.Context) (proto.Transport, string, error) {
			return dev.Transport(), fake.TransportName, nil
		},
	}
	tr.app.stdout = &tr.stdout
	tr.app.stderr = &tr.stderr
	t.Cleanup(func() { _ = dev.Close() })
	return tr
}

// run executes one invocation, as `gflex` would. --byte-delay is pinned to the
// smallest positive value so a test pays no pacing per MIDI message (SPEC.md
// §3.1) -- not the 1 ms default and not the 20 ms the vendor client uses.
// Zero would be read as "use the default" (SPEC.md §11), and the CLI refuses it
// for exactly that reason.
func (tr *fakeTree) run(t *testing.T, args ...string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return tr.runContext(t, ctx, args...)
}

// runContext is run with the caller's own context, for the tests that have to
// end one mid-command: a Ctrl-C or a SIGTERM lands on the context Execute
// derives from signal.NotifyContext (root.go), and the window it can land in is
// exactly the window a write's acknowledgement lives in.
func (tr *fakeTree) runContext(t *testing.T, ctx context.Context, args ...string) error {
	t.Helper()
	root := NewRootCommand(tr.app)
	root.SetOut(&tr.stdout)
	root.SetErr(&tr.stderr)
	root.SetArgs(append(append([]string{}, args...), "--byte-delay=1ns"))
	return root.ExecuteContext(ctx)
}

// wrote reports whether a write frame for cmd reached the device. This is the
// question the interlocks exist to answer: not what was printed, but what the
// hardware was told to do.
func (tr *fakeTree) wrote(t *testing.T, cmd proto.Cmd) bool {
	t.Helper()
	for _, fr := range tr.dev.Sent() {
		parsed, err := proto.Parse(fr)
		if err != nil {
			t.Fatalf("the device received an unparseable frame %s: %v", proto.Hex(fr), err)
		}
		if parsed.Write && parsed.Cmd == cmd {
			return true
		}
	}
	return false
}

// sent is wrote's sibling for the commands that are disruptive without carrying
// the write flag. CMD_JUMP_APP_TO_BOOTLOADER is the case SPEC.md §13.10 singles
// out: a plain read frame that drops the device off the bus, so "did a write
// reach it" is the wrong question to ask about it entirely.
func (tr *fakeTree) sent(t *testing.T, cmd proto.Cmd) bool {
	t.Helper()
	for _, fr := range tr.dev.Sent() {
		parsed, err := proto.Parse(fr)
		if err != nil {
			t.Fatalf("the device received an unparseable frame %s: %v", proto.Hex(fr), err)
		}
		if parsed.Cmd == cmd {
			return true
		}
	}
	return false
}

// TestVoltageSetRefusesOutsideTheDeviceWindow is the wiring test for interlock
// 1 of SPEC.md §13: the configured window is an unconditional bound on a
// voltage write, and --yes cannot discard it -- that flag is the routine
// scripting path, and only the self-describing --ignore-device-limits opens
// this gate.
func TestVoltageSetRefusesOutsideTheDeviceWindow(t *testing.T) {
	dev := fake.NewTypical()
	// A window an owner would set for a 12 V pedal.
	dev.SetRegister(proto.CmdUserVLimit, proto.EncodeVLimit(5000, 12000))
	tr := newFakeTree(t, dev)

	err := tr.run(t, "voltage", "set", "20", "--yes")
	if err == nil {
		t.Fatal("`voltage set 20 --yes` was accepted against a [5000, 12000] mV window")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if !strings.Contains(err.Error(), "outside this unit's configured voltage limits") {
		t.Errorf("refusal does not name the window it applied:\n%v", err)
	}
	// The load-bearing assertion: nothing was written to the rail.
	if tr.wrote(t, proto.CmdVoltageMv) {
		t.Fatalf("20 V reached the device despite the refusal; frames: %v", cmdNames(dev.Sent()))
	}
}

// The same interlock in its other direction: a window that cannot be read is a
// refusal, not a fallback to the hardware envelope (SPEC.md §13.1, §17). A
// single dropped frame must not silently downgrade a 12 V ceiling to 48 V --
// and the protocol has no NACK, so a dropped frame is an ordinary transient
// (SPEC.md §5.2), not an exotic condition.
func TestVoltageSetRefusesWhenTheWindowCannotBeRead(t *testing.T) {
	dev := fake.NewTypical()
	dev.SetFault(proto.CmdUserVLimit, fake.Fault{Drop: true})
	tr := newFakeTree(t, dev)

	// 12 V is inside the hardware envelope and inside the unit's real window,
	// so only the unreadable window can refuse it.
	err := tr.run(t, "voltage", "set", "12", "--yes", "--timeout=100ms")
	if err == nil {
		t.Fatal("`voltage set 12 --yes` was accepted with the voltage window unreadable")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	for _, want := range []string{"could not be read", "--ignore-device-limits"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q:\n%v", want, err)
		}
	}
	if tr.wrote(t, proto.CmdVoltageMv) {
		t.Fatalf("12 V reached the device with no window to check it against; frames: %v", cmdNames(dev.Sent()))
	}
}

// TestVoltageSetIsGatedByTheConfirmation is the wiring test for interlocks 2
// and 7 of SPEC.md §13: anything above the 5 V factory default is confirmed
// every time, and with no terminal on stdin there is nobody to ask, so the
// write is refused before it happens.
//
// Every other dangerous command has held this property here for a while --
// authlock, vlimit, calibrate and firmware flash each have their own case
// below -- while `voltage set`, the command the seam was built for, did not.
// Replacing its `app.apply(ctx, f, CheckVoltage(req))` with the applyDryRun
// two lines away, which honours a refusal and ignores a confirmation, left the
// whole suite green: `echo | gflex voltage set 48` would have put 48 V on the
// rail with no prompt and no --yes.
func TestVoltageSetIsGatedByTheConfirmation(t *testing.T) {
	dev := fake.NewTypical() // window 3300-48000 mV, output at the factory 5000 mV
	tr := newFakeTree(t, dev)

	err := tr.run(t, "voltage", "set", "12")
	if err == nil {
		t.Fatal("`voltage set 12` wrote 12 V with nobody to confirm it")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if !strings.Contains(err.Error(), "stdin is not a terminal") {
		t.Errorf("the refusal does not say why nobody could be asked:\n%v", err)
	}
	if tr.wrote(t, proto.CmdVoltageMv) {
		t.Fatalf("12 V reached the device despite the refusal; frames: %v", cmdNames(dev.Sent()))
	}
	if stored, ok := dev.Register(proto.CmdVoltageMv); !ok ||
		!bytes.Equal(stored, proto.EncodeU16(proto.DefaultVoltageMv)) {
		t.Errorf("device voltage register = %x, want it left at the factory %x",
			stored, proto.EncodeU16(proto.DefaultVoltageMv))
	}
}

// TestVoltageSetHonoursASubSixVoltCeiling drives the §17 row 2 regression shape
// through `voltage set` itself rather than through CheckVoltage alone.
//
// A [3300, 5000] window is the most protective configuration in the product and
// the one an owner with a 5 V pedal would set, but it is also the shape
// session.VLimitPlausible rejects, since that predicate asks the vendor's
// post-flash "was this erased?" question (low >= 3000, high >= 6000). Interlock
// 1 once reached for it here, which discarded the ceiling entirely and let 48 V
// through on a unit limited to 5 V. TestNarrowWindowIsHonoured pins the
// decision; this pins the classification that feeds it, which is where the
// wrong predicate was called.
func TestVoltageSetHonoursASubSixVoltCeiling(t *testing.T) {
	// One device per invocation: the session closes the transport when the
	// command ends, and the fake's transport is the device.
	narrowed := func(t *testing.T) (*fake.Device, *fakeTree) {
		t.Helper()
		dev := fake.NewTypical()
		dev.SetRegister(proto.CmdUserVLimit, proto.EncodeVLimit(3300, 5000))
		return dev, newFakeTree(t, dev)
	}

	dev, tr := narrowed(t)
	err := tr.run(t, "voltage", "set", "9", "--yes")
	if err == nil {
		t.Fatal("`voltage set 9 --yes` was accepted against a [3300, 5000] mV window")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if tr.wrote(t, proto.CmdVoltageMv) {
		t.Fatalf("9 V reached a unit limited to 5 V; frames: %v", cmdNames(dev.Sent()))
	}

	// The other half: a window that narrow must still permit what it covers, or
	// "refuses everything" would pass the assertion above for the wrong reason.
	dev, tr = narrowed(t)
	if err := tr.run(t, "voltage", "set", "5", "--yes"); err != nil {
		t.Fatalf("`voltage set 5 --yes` inside the [3300, 5000] mV window failed: %v", err)
	}
	if !tr.wrote(t, proto.CmdVoltageMv) {
		t.Errorf("5 V never reached the device; frames: %v", cmdNames(dev.Sent()))
	}
}

// The positive control for the three tests above. Without it a broken seam -- a
// transport that never opens, say -- would make every refusal test pass for
// entirely the wrong reason.
func TestVoltageSetInsideTheWindowReachesTheDevice(t *testing.T) {
	dev := fake.NewTypical() // window 3300-48000 mV
	tr := newFakeTree(t, dev)

	if err := tr.run(t, "voltage", "set", "12", "--yes"); err != nil {
		t.Fatalf("`voltage set 12 --yes` inside the window failed: %v", err)
	}
	want, err := proto.Write(proto.CmdVoltageMv, proto.EncodeU16(12000))
	if err != nil {
		t.Fatalf("building the expected frame: %v", err)
	}
	var found bool
	for _, fr := range dev.Sent() {
		if bytes.Equal(fr, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("the 12000 mV write %s never reached the device; frames: %v", proto.Hex(want), dev.SentHex())
	}
	if stored, ok := dev.Register(proto.CmdVoltageMv); !ok || !bytes.Equal(stored, proto.EncodeU16(12000)) {
		t.Errorf("device voltage register = %x, want %x", stored, proto.EncodeU16(12000))
	}
}

// TestAuthLockSetIsGatedByTheConfirmation is the wiring test for interlocks 4
// and 7 of SPEC.md §13. `authlock set` confirms every write, so on a non-TTY
// stdin without --yes it must refuse before the write -- which matters more
// here than anywhere else, because a non-zero level has no documented effect
// and there may be no way back (SPEC.md §6.3, §14.8).
func TestAuthLockSetIsGatedByTheConfirmation(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)

	err := tr.run(t, "authlock", "set", "1")
	if err == nil {
		t.Fatal("`authlock set 1` wrote a possibly irreversible level with nobody to confirm it")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if tr.wrote(t, proto.CmdAuthLock) {
		t.Fatalf("the auth lock was written despite the refusal; frames: %v", cmdNames(dev.Sent()))
	}
	// The AUTHLOCK read payload is [0x16, level]: the command code echoed a
	// second time, then the level (SPEC.md §14.8, measured `tx 02 16` ->
	// `rx 04 16 16 00`). The level lives at index 1, which is what the fake
	// stores and what the vendor client always read.
	level, ok := dev.Register(proto.CmdAuthLock)
	if !ok || len(level) < 2 {
		t.Fatalf("device auth lock register = %v, want the two-byte [echo, level] read payload", level)
	}
	if level[1] != proto.AuthLockUnlocked {
		t.Errorf("device auth lock level = %d, want it left at %d (register %v)",
			level[1], proto.AuthLockUnlocked, level)
	}
}

// Its positive control: with --yes the same command goes through, so the
// refusal above came from the interlock and not from the plumbing.
func TestAuthLockSetProceedsWithYes(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)

	if err := tr.run(t, "authlock", "set", "1", "--yes"); err != nil {
		t.Fatalf("`authlock set 1 --yes`: %v", err)
	}
	if !tr.wrote(t, proto.CmdAuthLock) {
		t.Errorf("--yes did not reach the device; frames: %v", cmdNames(dev.Sent()))
	}
}

// TestRawBootloaderJumpIsGatedByTheConfirmation is the wiring test for the case
// SPEC.md §13.10 singles out, and the one `gflex raw` cannot survive getting
// wrong: `raw 02 14` is a plain READ frame with a documented command code, so
// none of CheckRawFrame's generic reasons (write, undocumented, unknown,
// scratchpad) fires on it. Only the disruptive-command switch does, and if that
// switch went missing the frame would go out unchallenged and strand the unit
// in the bootloader, where nothing in this tool but `firmware flash` can reach
// it.
func TestRawBootloaderJumpIsGatedByTheConfirmation(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)

	err := tr.run(t, "raw", "02", "14")
	if err == nil {
		t.Fatal("`raw 02 14` sent the device into the bootloader with nobody to confirm it")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if tr.sent(t, proto.CmdJumpAppToBootloader) {
		t.Fatalf("the jump frame reached the device despite the refusal; frames: %v", cmdNames(dev.Sent()))
	}
}

// Its positive control: with --yes the same frame goes out, so the refusal
// above came from the interlock and not from the plumbing. --no-ack is how the
// jump is sent for real -- the device disconnects instead of answering
// (SPEC.md §6.1), which is why the fake drops it too -- so this is the
// invocation a user would actually type.
func TestRawBootloaderJumpProceedsWithYes(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)

	if err := tr.run(t, "raw", "02", "14", "--yes", "--no-ack"); err != nil {
		t.Fatalf("`raw 02 14 --yes --no-ack`: %v", err)
	}
	if !tr.sent(t, proto.CmdJumpAppToBootloader) {
		t.Errorf("--yes did not reach the device; frames: %v", cmdNames(dev.Sent()))
	}
}

// TestArgumentOnlyRefusalsOpenNoDevice pins where an argument-only refusal
// happens: before anything is read. 60 V is outside the hardware
// envelope and an inverted window cannot bound anything, so no window the unit
// might report changes either answer -- and being told to find a device first,
// only to hear the number was never in range, is a worse answer as well as a
// slower one.
//
// Asserted on the wire rather than on timing: not one frame goes out.
func TestArgumentOnlyRefusalsOpenNoDevice(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"a voltage above the hardware maximum", []string{"voltage", "set", "60", "--yes"}},
		{"a window that cannot bound anything", []string{"vlimit", "set", "--low", "12000mV", "--high", "9000mV", "--yes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dev := fake.NewTypical()
			tr := newFakeTree(t, dev)

			err := tr.run(t, tc.args...)
			if err == nil {
				t.Fatalf("`gflex %s` was accepted", strings.Join(tc.args, " "))
			}
			if code := ExitCode(err); code != ExitRefused {
				t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
			}
			if frames := dev.Sent(); len(frames) != 0 {
				t.Errorf("the device was asked %v before the refusal", cmdNames(frames))
			}
		})
	}
}

// TestRawWriteIsGatedByTheConfirmation is the wiring test for the generic WRITE
// reason of SPEC.md §13.10, which every other raw case here reaches only
// through a more specific arm. `raw 03 96 07` is an auth lock write: a
// documented, known command code with no switch arm of its own, so the reason
// that names it a write is the only thing between a non-TTY invocation and a
// level whose effect is undocumented and may not be reversible (SPEC.md §6.3,
// §14.8).
func TestRawWriteIsGatedByTheConfirmation(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)

	err := tr.run(t, "raw", "03", "96", "07")
	if err == nil {
		t.Fatal("`raw 03 96 07` wrote an auth lock level with nobody to confirm it")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if tr.wrote(t, proto.CmdAuthLock) {
		t.Fatalf("the auth lock was written despite the refusal; frames: %v", cmdNames(dev.Sent()))
	}
}

// TestVLimitWideningIsGatedByTheConfirmation is the wiring test for interlock 3
// of SPEC.md §13. Widening the window removes the guard rail interlock 1
// depends on, so it must be confirmed -- and on a non-TTY that is a refusal
// before the write.
func TestVLimitWideningIsGatedByTheConfirmation(t *testing.T) {
	dev := fake.NewTypical()
	// A window an owner would set for a 12 V pedal, so that raising the ceiling
	// to 20 V is unambiguously a widening.
	dev.SetRegister(proto.CmdUserVLimit, proto.EncodeVLimit(5000, 12000))
	tr := newFakeTree(t, dev)

	err := tr.run(t, "vlimit", "set", "--low", "5000mV", "--high", "20000mV")
	if err == nil {
		t.Fatal("`vlimit set` widened the window with nobody to confirm it")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if tr.wrote(t, proto.CmdUserVLimit) {
		t.Fatalf("the window was widened despite the refusal; frames: %v", cmdNames(dev.Sent()))
	}
	if stored, ok := dev.Register(proto.CmdUserVLimit); !ok ||
		!bytes.Equal(stored, proto.EncodeVLimit(5000, 12000)) {
		t.Errorf("device window = %x, want the original %x", stored, proto.EncodeVLimit(5000, 12000))
	}
}

// TestCalibrateADCIsGatedByTheConfirmation is the wiring test for interlock 5.
// A wrong calibration makes every subsequent voltage reading silently wrong,
// which defeats interlock 1 by corrupting the evidence it relies on -- so
// CheckCalibrate confirms unconditionally and this path must never write on a
// non-TTY without --yes.
func TestCalibrateADCIsGatedByTheConfirmation(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)

	err := tr.run(t, "calibrate", "adc", "--offset", "40", "--scale", "9")
	if err == nil {
		t.Fatal("`calibrate adc` rewrote the calibration with nobody to confirm it")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if tr.wrote(t, proto.CmdVMeasureADCOffset) || tr.wrote(t, proto.CmdVMeasureADCScale) {
		t.Fatalf("the calibration was rewritten despite the refusal; frames: %v", cmdNames(dev.Sent()))
	}
}

// TestFirmwareFlashIsGatedByTheConfirmation is the wiring test for interlock 6,
// and the highest-stakes of the set: the `app.apply(CheckFlash(...))` line is
// the only thing standing between a scripted `firmware flash` and
// Session.JumpToBootloader, after which the unit is off the bus and only
// `--recover` can reach it.
//
// --crc is what makes this test measure the confirmation rather than the
// no-CRC refusal: a raw .bin carries no CRC of its own, and CheckFlash refuses
// an unverifiable image outright before it ever gets as far as confirming.
func TestFirmwareFlashIsGatedByTheConfirmation(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)
	path := filepath.Join(t.TempDir(), "image.bin")
	if err := os.WriteFile(path, make([]byte, 512), 0o600); err != nil {
		t.Fatalf("writing the test image: %v", err)
	}

	err := tr.run(t, "firmware", "flash", path, "--crc", "0")
	if err == nil {
		t.Fatal("`firmware flash` started an update with nobody to confirm it")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if tr.sent(t, proto.CmdJumpAppToBootloader) {
		t.Fatalf("the device was sent into the bootloader despite the refusal; frames: %v",
			cmdNames(dev.Sent()))
	}
}

// TestInterlockWarningsReachStderr pins the other half of App.apply, and of its
// dry-run twin: a Decision's warnings are printed, not merely computed.
//
// Both loops could be deleted without a test noticing, which would take the
// SPEC.md §13.9 EPR advisory off screen -- the one that says a request above
// 20 V needs an eMarker-equipped 5 A cable and an EPR-capable source, and that
// a fast-blinking red LED means exactly that combination is missing. The third
// case is a dry run on purpose: applyDryRun keeps its own copy of the loop and
// is the path with no device to answer for it.
func TestInterlockWarningsReachStderr(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fake.Device)
		args  []string
		want  string
	}{
		{
			name: "a voltage above the EPR threshold",
			args: []string{"voltage", "set", "28", "--yes"},
			want: "eMarker",
		},
		{
			name: "a window raised above it",
			setup: func(d *fake.Device) {
				d.SetRegister(proto.CmdUserVLimit, proto.EncodeVLimit(3300, 12000))
			},
			args: []string{"vlimit", "set", "--high", "48", "--yes"},
			want: "eMarker",
		},
		{
			name: "a raw voltage write, which opens no device at all",
			args: []string{"raw", "04", "92", "2e", "e0", "--dry-run"},
			want: "bypassing every range check",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dev := fake.NewTypical()
			if tc.setup != nil {
				tc.setup(dev)
			}
			tr := newFakeTree(t, dev)
			if err := tr.run(t, tc.args...); err != nil {
				t.Fatalf("`gflex %s`: %v", strings.Join(tc.args, " "), err)
			}
			if !strings.Contains(tr.stderr.String(), "warning:") ||
				!strings.Contains(tr.stderr.String(), tc.want) {
				t.Errorf("stderr carries no warning mentioning %q:\n%s", tc.want, tr.stderr.String())
			}
		})
	}
}

// The seam must be invisible to the shipped tree: an App built the way Execute
// builds one opens a real transport, so no production path can reach a fake.
//
// It has to be the App Execute really builds. A test that writes its own
// &App{...} literal and finds testTransport nil has proved that Go zeroes an
// omitted field, which holds whatever this package looks like -- so newApp
// exists (root.go) to give the assertion something to bite on. NewRootCommand
// is driven too, because installing the override while wiring up the tree would
// be just as much a leak as setting it in the constructor.
func TestShippedAppCarriesNoTransportSeam(t *testing.T) {
	app := newApp()
	if app.testTransport != nil {
		t.Fatal("the App Execute builds carries a transport override")
	}
	_ = NewRootCommand(app)
	if app.testTransport != nil {
		t.Fatal("NewRootCommand installed a transport override")
	}
}

// The positive control for interlock 3, and the direction the refusal test
// above cannot see. Deleting `vlimit set`'s app.apply call fails that test, but
// a seam that refuses unconditionally -- a previous pair read wrongly, an
// always-false OK() -- passes it, while the documented scripting path is
// broken and every `vlimit set --yes` in a provisioning script refuses.
func TestVLimitWideningProceedsWithYes(t *testing.T) {
	dev := fake.NewTypical()
	dev.SetRegister(proto.CmdUserVLimit, proto.EncodeVLimit(5000, 12000))
	tr := newFakeTree(t, dev)

	if err := tr.run(t, "vlimit", "set", "--low", "5000mV", "--high", "20000mV", "--yes"); err != nil {
		t.Fatalf("`vlimit set --low 5000mV --high 20000mV --yes`: %v", err)
	}
	// HIGH goes before LOW on the wire in both directions (SPEC.md §6.2), which
	// EncodeVLimit is what holds; comparing the stored bytes checks the order as
	// well as the values.
	want := proto.EncodeVLimit(5000, 20000)
	if stored, ok := dev.Register(proto.CmdUserVLimit); !ok || !bytes.Equal(stored, want) {
		t.Errorf("device window = %x, want %x", stored, want)
	}
}

// The positive control for interlock 5. The stderr assertion is the second half
// of it: CheckCalibrate's whole reason for reading the previous pair first is
// the ready-made restore command, which App.apply prints through the warning
// loop before --yes short-circuits the confirmation -- so a run that wrote the
// new values while losing the way back would pass a write-only assertion.
func TestCalibrateADCProceedsWithYes(t *testing.T) {
	dev := fake.NewTypical() // factory calibration: offset 0, scale 0
	tr := newFakeTree(t, dev)

	if err := tr.run(t, "calibrate", "adc", "--offset", "40", "--scale", "9", "--yes"); err != nil {
		t.Fatalf("`calibrate adc --offset 40 --scale 9 --yes`: %v", err)
	}
	// Signed 32-bit, big-endian (SPEC.md §6.5).
	for _, tc := range []struct {
		cmd  proto.Cmd
		want []byte
	}{
		{proto.CmdVMeasureADCOffset, proto.EncodeI32(40)},
		{proto.CmdVMeasureADCScale, proto.EncodeI32(9)},
	} {
		if stored, ok := dev.Register(tc.cmd); !ok || !bytes.Equal(stored, tc.want) {
			t.Errorf("device %s = %x, want %x", tc.cmd, stored, tc.want)
		}
	}
	restore := fmt.Sprintf("gflex calibrate adc --offset %d --scale %d --yes",
		proto.DefaultADCOffset, proto.DefaultADCScale)
	if !strings.Contains(tr.stderr.String(), restore) {
		t.Errorf("stderr does not carry the restore command %q:\n%s", restore, tr.stderr.String())
	}
}

// TestCurrentSetRefusesAboveThePassThroughMaximum is the wiring test `current
// set` never had: the interlock could be unhooked entirely with the suite
// green.
//
// CheckCurrent is the only thing standing between a typo and a request the
// hardware cannot pass -- and the only thing that catches the 16-bit wrap that
// turns `current set 70` into a 4464 mA write, which is a plausible number no
// later check can question.
func TestCurrentSetRefusesAboveThePassThroughMaximum(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)

	err := tr.run(t, "current", "set", "6", "--yes")
	if err == nil {
		t.Fatal("`current set 6 --yes` was accepted above the 5000 mA pass-through maximum")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if tr.wrote(t, proto.CmdCurrentLimitMa) {
		t.Fatalf("6000 mA reached the device despite the refusal; frames: %v", cmdNames(dev.Sent()))
	}

	// And the value it exists to permit still goes through, so the assertion
	// above cannot be satisfied by a command that refuses everything.
	dev = fake.NewTypical()
	tr = newFakeTree(t, dev)
	if err := tr.run(t, "current", "set", "3", "--yes"); err != nil {
		t.Fatalf("`current set 3 --yes`: %v", err)
	}
	if stored, ok := dev.Register(proto.CmdCurrentLimitMa); !ok ||
		!bytes.Equal(stored, proto.EncodeU16(3000)) {
		t.Errorf("device current limit = %x, want %x", stored, proto.EncodeU16(3000))
	}
}

// --ignore-device-limits is the documented override of SPEC.md §13.1, and the
// only way through for a unit whose window genuinely cannot be read. The flag
// was bound to a variable that no test ever passed on a command line, so the
// assignment into VoltageRequest could be dropped and the override would become
// a silent no-op: every affected user would be left with a refusal and a flag
// that does nothing.
//
// --timeout is pinned short because the point of the test is the dropped read,
// and the default response timeout would be paid in real time on every run.
func TestIgnoreDeviceLimitsCarriesTheWriteThrough(t *testing.T) {
	dev := fake.NewTypical()
	dev.SetFault(proto.CmdUserVLimit, fake.Fault{Drop: true})
	tr := newFakeTree(t, dev)

	if err := tr.run(t, "voltage", "set", "12", "--yes", "--ignore-device-limits", "--timeout=100ms"); err != nil {
		t.Fatalf("`voltage set 12 --yes --ignore-device-limits`: %v", err)
	}
	if stored, ok := dev.Register(proto.CmdVoltageMv); !ok ||
		!bytes.Equal(stored, proto.EncodeU16(12000)) {
		t.Errorf("device voltage register = %x, want %x", stored, proto.EncodeU16(12000))
	}
}
