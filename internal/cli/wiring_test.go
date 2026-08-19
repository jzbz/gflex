package cli

import (
	"bytes"
	"context"
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
// smallest positive value so a test does not pay the vendor's 20 ms per MIDI
// message (SPEC.md §3.1); zero would be read as "use the default" (SPEC.md §11).
func (tr *fakeTree) run(t *testing.T, args ...string) error {
	t.Helper()
	root := NewRootCommand(tr.app)
	root.SetOut(&tr.stdout)
	root.SetErr(&tr.stderr)
	root.SetArgs(append(append([]string{}, args...), "--byte-delay=1ns"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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

// The positive control for both tests above. Without it a broken seam -- a
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
	if level, ok := dev.Register(proto.CmdAuthLock); !ok || level[0] != proto.AuthLockUnlocked {
		t.Errorf("device auth lock = %v, want it left at %d", level, proto.AuthLockUnlocked)
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

// The seam must be invisible to the shipped tree: an App built the way Execute
// builds one opens a real transport, so no production path can reach a fake.
func TestTestTransportSeamIsUnsetByDefault(t *testing.T) {
	app := &App{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, stdin: strings.NewReader("")}
	if app.testTransport != nil {
		t.Error("a freshly built App carries a transport override")
	}
}
