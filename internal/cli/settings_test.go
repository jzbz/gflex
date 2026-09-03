package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/session"
	"github.com/jzbz/gflex/internal/transport/fake"
)

// TestParseAuthLockLevelIsStrictDecimal is the regression test for parsing the
// auth lock level with strconv's base 0, which is Go literal syntax: a leading
// zero meant octal, so "010" wrote level 8, and "0x10" and "1_0" -- the exact
// shapes numberProblem in parse.go rejects for voltages -- were accepted. On
// the least understood command in the protocol, whose non-zero levels may be
// irreversible (SPEC.md §6.3, §14.8), the byte written must be beyond doubt
// the number the user typed.
func TestParseAuthLockLevelIsStrictDecimal(t *testing.T) {
	cases := []struct {
		arg  string
		want int
		ok   bool
	}{
		{"0", 0, true},
		{"10", 10, true},
		{" 10 ", 10, true},
		// The finding's case: decimal 10, and NEVER octal 8.
		{"010", 10, true},
		{"255", 255, true},
		// Out-of-byte and negative values parse here; CheckAuthLock owns the
		// 0-255 refusal so range enforcement lives in one place (SPEC.md §13).
		{"256", 256, true},
		{"-1", -1, true},
		// Go literal shapes are refused outright, mirroring parse.go's grammar.
		{"0x10", 0, false},
		{"1_0", 0, false},
		{"1e1", 0, false},
		{"1.5", 0, false},
		{"", 0, false},
		// Wider than 32 bits fails the parse instead of wrapping into a
		// plausible-looking level on any platform.
		{"4294967306", 0, false},
	}
	for _, tc := range cases {
		got, err := parseAuthLockLevel(tc.arg)
		if tc.ok {
			if err != nil {
				t.Errorf("parseAuthLockLevel(%q): unexpected error %v", tc.arg, err)
				continue
			}
			if got != tc.want {
				t.Errorf("parseAuthLockLevel(%q) = %d, want %d", tc.arg, got, tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("parseAuthLockLevel(%q) = %d, want a refusal", tc.arg, got)
			continue
		}
		if code := ExitCode(err); code != ExitUsage {
			t.Errorf("parseAuthLockLevel(%q): ExitCode = %d, want ExitUsage (%d)", tc.arg, code, ExitUsage)
		}
	}
}

// The values that parse but do not fit the wire byte are refused by the
// interlock, so nothing between parse and refusal can truncate them.
func TestAuthLockOutOfByteLevelsAreRefusedDownstream(t *testing.T) {
	for _, arg := range []string{"256", "-1"} {
		level, err := parseAuthLockLevel(arg)
		if err != nil {
			t.Fatalf("parseAuthLockLevel(%q): %v", arg, err)
		}
		if d := CheckAuthLock(level); d.Refused == "" {
			t.Errorf("CheckAuthLock(%d) did not refuse; %q would reach the wire truncated", level, arg)
		}
	}
}

// TestAuthLockSetLeadingZeroIsDecimal drives the real command tree: `authlock
// set 010 --dry-run` must list a write frame carrying level 10, and under no
// interpretation level 8. This is the whole-path guarantee the unit test above
// cannot give: the parsed value is what reaches the frame builder.
func TestAuthLockSetLeadingZeroIsDecimal(t *testing.T) {
	clearGflexEnv(t)
	var stdout, stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr, stdin: strings.NewReader("")}
	root := NewRootCommand(app)
	root.SetArgs([]string{"authlock", "set", "010", "--dry-run"})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("`authlock set 010 --dry-run`: %v", err)
	}
	want, err := proto.Write(proto.CmdAuthLock, []byte{10})
	if err != nil {
		t.Fatalf("building the expected frame: %v", err)
	}
	if !strings.Contains(stdout.String(), proto.Hex(want)) {
		t.Errorf("dry run does not list the level-10 write frame %s:\n%s", proto.Hex(want), stdout.String())
	}
	octal, err := proto.Write(proto.CmdAuthLock, []byte{8})
	if err != nil {
		t.Fatalf("building the octal frame: %v", err)
	}
	if strings.Contains(stdout.String(), proto.Hex(octal)) {
		t.Errorf("dry run lists the OCTAL reading of \"010\" (level 8): %s\n%s", proto.Hex(octal), stdout.String())
	}
}

// mustFrame builds the frame a dry run is expected to list, or to leave out.
func mustFrame(t *testing.T, cmd proto.Cmd, payload []byte) string {
	t.Helper()
	fr, err := proto.Write(cmd, payload)
	if err != nil {
		t.Fatalf("building the %v frame: %v", cmd, err)
	}
	return proto.Hex(fr)
}

// TestSettingWriteFlagsAreStrictDecimal is the same regression as
// TestAuthLockSetLeadingZeroIsDecimal, for the flags that had the defect after
// `authlock set` was fixed. pflag's IntVar and Int32Var call
// strconv.ParseInt(s, 0, ...), so --nominal, --sag, --offset and --scale
// reached base 0 -- Go literal syntax -- without a conversion in this package
// ever spelling it out. "0750" was octal 488, and every one of these values is
// written to non-volatile memory with no prompt (tolerance) or a prompt that
// --yes removes (calibrate).
//
// Both halves are asserted, and reverting the fix flips both: the dry run
// would list the octal frame and not the decimal one. The negative assertion
// earns its place by naming the failure -- a red test reads "the dry run
// listed 488 mV" rather than only "750 mV is missing".
func TestSettingWriteFlagsAreStrictDecimal(t *testing.T) {
	clearGflexEnv(t)
	cases := []struct {
		name    string
		args    []string
		want    []string
		notWant []string
	}{
		{
			name: "tolerance nominal",
			args: []string{"tolerance", "set", "--nominal", "0750", "--dry-run"},
			want: []string{mustFrame(t, proto.CmdVToleranceNominalMv, proto.EncodeU16(750))},
			// Octal 750 is 488 mV: inside every bound the code checks.
			notWant: []string{mustFrame(t, proto.CmdVToleranceNominalMv, proto.EncodeU16(488))},
		},
		{
			name:    "tolerance sag",
			args:    []string{"tolerance", "set", "--sag", "010", "--dry-run"},
			want:    []string{mustFrame(t, proto.CmdVToleranceSagPerMa, proto.EncodeU16(10))},
			notWant: []string{mustFrame(t, proto.CmdVToleranceSagPerMa, proto.EncodeU16(8))},
		},
		{
			name: "calibrate offset and scale",
			args: []string{"calibrate", "adc", "--offset", "010", "--scale", "020", "--dry-run"},
			want: []string{
				mustFrame(t, proto.CmdVMeasureADCOffset, proto.EncodeI32(10)),
				mustFrame(t, proto.CmdVMeasureADCScale, proto.EncodeI32(20)),
			},
			notWant: []string{
				mustFrame(t, proto.CmdVMeasureADCOffset, proto.EncodeI32(8)),
				mustFrame(t, proto.CmdVMeasureADCScale, proto.EncodeI32(16)),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			app := &App{stdout: &stdout, stderr: &stderr, stdin: strings.NewReader("")}
			root := NewRootCommand(app)
			root.SetArgs(tc.args)
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("`gflex %s`: %v", strings.Join(tc.args, " "), err)
			}
			for _, want := range tc.want {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("dry run does not list the decimal write frame %s:\n%s", want, stdout.String())
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(stdout.String(), bad) {
					t.Errorf("dry run lists the OCTAL reading as frame %s:\n%s", bad, stdout.String())
				}
			}
		})
	}
}

// The Go-literal shapes are usage errors on these flags too, so a script
// cannot feed a hex or underscored value past cobra into non-volatile memory.
func TestSettingWriteFlagsRefuseGoLiteralShapes(t *testing.T) {
	clearGflexEnv(t)
	for _, flag := range []struct{ cmd, name string }{
		{"tolerance set", "--nominal"},
		{"tolerance set", "--sag"},
		{"calibrate adc", "--offset"},
		{"calibrate adc", "--scale"},
	} {
		for _, arg := range []string{"0x10", "1_0"} {
			args := append(strings.Fields(flag.cmd), flag.name, arg)
			// calibrate's dry run needs both flags; the other one stays
			// plain decimal so only the shape under test can fail.
			switch flag.name {
			case "--offset":
				args = append(args, "--scale", "0")
			case "--scale":
				args = append(args, "--offset", "0")
			}
			args = append(args, "--dry-run")

			var stdout, stderr bytes.Buffer
			app := &App{stdout: &stdout, stderr: &stderr, stdin: strings.NewReader("")}
			root := NewRootCommand(app)
			root.SetArgs(args)

			err := root.ExecuteContext(context.Background())
			if err == nil {
				t.Errorf("`gflex %s` was accepted; stdout:\n%s", strings.Join(args, " "), stdout.String())
				continue
			}
			if code := ExitCode(err); code != ExitUsage {
				t.Errorf("`gflex %s`: ExitCode = %d, want ExitUsage (%d): %v",
					strings.Join(args, " "), code, ExitUsage, err)
			}
		}
	}
}

// applyButDoNotAnswer makes cmd behave like a device that received a write,
// applied it, and whose acknowledgement was then lost.
//
// fake.Fault{Drop: true} deliberately does not stage this -- its own
// documentation says so: Drop loses the REQUEST, so nothing is applied. The
// host cannot tell the two apart, because there is no NACK anywhere in this
// protocol (SPEC.md §5.2), and that is exactly why the advisory under test has
// to assume the dangerous one. onWrite, when non-nil, runs after the write has
// landed and before the silence.
func applyButDoNotAnswer(dev *fake.Device, cmd proto.Cmd, onWrite func()) {
	dev.SetHandler(cmd, func(f proto.Frame) []byte {
		if !f.Write {
			v, _ := dev.Register(cmd)
			return v
		}
		dev.StoreRegister(cmd, f.Payload)
		if onWrite != nil {
			onWrite()
		}
		return nil // the write landed; the acknowledgement did not
	})
}

// TestUnansweredVoltageWriteSaysTheRailMayBeLive covers the failure the
// protocol produces routinely: the write frame is transmitted in full, the
// device applies it, and the echo is lost (SPEC.md §5.2, §14.15). The command
// still fails -- the device's state is unknown, not known-good -- but it must
// not leave the user believing the rail is where it was, which is how a 5 V
// pedal ends up on a 12 V rail.
func TestUnansweredVoltageWriteSaysTheRailMayBeLive(t *testing.T) {
	dev := fake.NewTypical()
	applyButDoNotAnswer(dev, proto.CmdVoltageMv, nil)
	tr := newFakeTree(t, dev)

	err := tr.run(t, "voltage", "set", "12", "--yes", "--timeout=200ms")
	if err == nil {
		t.Fatal("`voltage set 12 --yes` reported success with nothing acknowledged")
	}
	// Assert the fake really did apply it first: otherwise a double that
	// dropped the write would let this test pass while describing nothing.
	if stored, ok := dev.Register(proto.CmdVoltageMv); !ok ||
		!bytes.Equal(stored, proto.EncodeU16(12000)) {
		t.Fatalf("device voltage register = %x, want the applied %x", stored, proto.EncodeU16(12000))
	}
	for _, want := range []string{"transmitted in full", "gflex voltage get"} {
		if !strings.Contains(tr.stderr.String(), want) {
			t.Errorf("stderr does not say the write may already have been applied (%q):\n%s",
				want, tr.stderr.String())
		}
	}
}

// The same write cut short by a Ctrl-C or a SIGTERM, which
// signal.NotifyContext turns into a cancelled context (root.go). This is the
// case that has to be said on stderr rather than in the error: Execute prints
// nothing but "gflex: interrupted" for anything chained to context.Canceled, so
// a message folded into the error chain reaches nobody, and "interrupted" reads
// as "nothing happened" while the rail is live.
func TestInterruptedVoltageWriteSaysTheRailMayBeLive(t *testing.T) {
	dev := fake.NewTypical()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The signal lands in the window between the last MIDI message and the
	// echo: the frame is on the wire, so the device has it.
	applyButDoNotAnswer(dev, proto.CmdVoltageMv, cancel)
	tr := newFakeTree(t, dev)

	err := tr.runContext(t, ctx, "voltage", "set", "12", "--yes")
	if err == nil {
		t.Fatal("`voltage set 12 --yes` reported success after being interrupted")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to carry context.Canceled", err)
	}
	if stored, ok := dev.Register(proto.CmdVoltageMv); !ok ||
		!bytes.Equal(stored, proto.EncodeU16(12000)) {
		t.Fatalf("device voltage register = %x, want the applied %x", stored, proto.EncodeU16(12000))
	}
	if !strings.Contains(tr.stderr.String(), "transmitted in full") {
		t.Errorf("an interrupted write said nothing about the rail:\n%s", tr.stderr.String())
	}
}

// The exclusion that makes the advisory worth trusting. A write whose SEND
// failed did not land: the frame stopped at the message that could not be
// written, so it is truncated and the device's receive state machine drops it
// for want of an end-of-frame marker (SPEC.md §3.3). Saying "the device may
// have applied it" there would be a false alarm on the one message that has to
// stay trustworthy, which is why session.ErrUnacknowledged is not attached to
// that leg -- and why this advisory keys on the sentinel rather than on "the
// write returned an error".
func TestOnlyAnUnacknowledgedWriteSaysItMayHaveLanded(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		wantAdvisory bool
	}{
		{
			"a send that never left the host",
			fmt.Errorf("CMD_VOLTAGE_MV: send failed (tx 04 92 2e e0): %w", errors.New("no such device")),
			false,
		},
		{
			"a transmitted write that went unanswered",
			fmt.Errorf("write voltage 12000 mV: %w: %w", session.ErrUnacknowledged, session.ErrTimeout),
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			warnUnacknowledged(newFormatter(false, &stdout, &stderr), tc.err,
				"output voltage write", "gflex voltage get")
			if got := strings.Contains(stderr.String(), "transmitted in full"); got != tc.wantAdvisory {
				t.Errorf("advisory printed = %v, want %v; stderr: %q", got, tc.wantAdvisory, stderr.String())
			}
		})
	}
}

// TestSettingDryRunsListTheWholeExchange holds the four commands that write a
// setting to interlock 8 of SPEC.md §13: --dry-run prints the frames a command
// would send, and is refused only where a frame cannot be known without reading
// the device. Every frame these four issue is a constant, so there was no
// excuse for listing only the write -- `voltage set 12 --dry-run` showed one
// frame while the command sends three, and the two it hid are the window read
// interlock 1 depends on and the read-back that verifies the value. An operator
// auditing the command before running it on a rig reads this list and stops.
//
// Derived rather than asserted, the way TestInfoDryRunMatchesWhatInfoSends is:
// each command runs for real against a fake device, and the frames the device
// received are compared with what the same command's own --dry-run lists.
func TestSettingDryRunsListTheWholeExchange(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"voltage set", []string{"voltage", "set", "12", "--yes"}},
		{"current set", []string{"current", "set", "3", "--yes"}},
		// A narrowing, so the run needs nothing but the flags themselves.
		{"vlimit set", []string{"vlimit", "set", "--low", "3300mV", "--high", "12000mV", "--yes"}},
		{"calibrate adc", []string{"calibrate", "adc", "--offset", "10", "--scale", "20", "--yes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dev := fake.NewTypical()
			live := newFakeTree(t, dev)
			if err := live.run(t, tc.args...); err != nil {
				t.Fatalf("`gflex %s`: %v\n%s", strings.Join(tc.args, " "), err, live.stderr.String())
			}

			dry := newFakeTree(t, fake.NewTypical())
			if err := dry.run(t, append(append([]string{}, tc.args...), "--dry-run", "--json")...); err != nil {
				t.Fatalf("`gflex %s --dry-run`: %v", strings.Join(tc.args, " "), err)
			}
			var listing struct {
				Frames []struct {
					Frame string `json:"frame"`
				} `json:"frames"`
			}
			if err := json.Unmarshal(dry.stdout.Bytes(), &listing); err != nil {
				t.Fatalf("decoding the dry-run listing: %v\n%s", err, dry.stdout.String())
			}

			sent := dev.SentHex()
			listed := make([]string, len(listing.Frames))
			for i, fr := range listing.Frames {
				listed[i] = fr.Frame
			}
			if len(sent) != len(listed) {
				t.Fatalf("the command sent %d frames and --dry-run lists %d\n  sent:   %v\n  listed: %v",
					len(sent), len(listed), sent, listed)
			}
			for i := range sent {
				if sent[i] != listed[i] {
					t.Errorf("frame %d: sent %s, --dry-run lists %s", i, sent[i], listed[i])
				}
			}
		})
	}
}

// The other half of interlock 8's honesty: a --dry-run reads nothing, so it must
// not report a read that failed. "user voltage limits could not be read"
// described a failure that never happened, on the one output the spec calls a
// safety property.
func TestVoltageSetDryRunDoesNotClaimAFailedRead(t *testing.T) {
	tr := newFakeTree(t, fake.NewTypical())
	if err := tr.run(t, "voltage", "set", "12", "--dry-run"); err != nil {
		t.Fatalf("`voltage set 12 --dry-run`: %v", err)
	}
	if strings.Contains(tr.stderr.String(), "could not be read") {
		t.Errorf("a dry run reports a limit read that was never attempted:\n%s", tr.stderr.String())
	}
	if !strings.Contains(tr.stderr.String(), "were not read") {
		t.Errorf("a dry run does not say the limits went unread:\n%s", tr.stderr.String())
	}
}

// echoAndKeepTheOldValue makes cmd acknowledge a write, echo it, and keep what
// it already held: the validate-and-discard shape SPEC.md §14.4 measured, which
// is what any write the device does not commit looks like from the host. There
// is no NACK to say so (SPEC.md §5.2), so the read-back is the only thing that
// can notice.
func echoAndKeepTheOldValue(dev *fake.Device, cmd proto.Cmd) {
	dev.SetHandler(cmd, func(f proto.Frame) []byte {
		if f.Write {
			return append([]byte{}, f.Payload...)
		}
		v, _ := dev.Register(cmd)
		return v
	})
}

// TestVLimitSetComparesItsReadBack holds `vlimit set` to the claim the README
// and SPEC.md §17 (row 6.5) make for it: the paths that can damage a load verify
// by read-back. Reading the pair back and printing it without comparing it is
// not verification -- the command exited 0 and printed the OLD window, which is
// the guard rail interlock 1 checks every later `voltage set` against, so a
// script chaining `vlimit set --high 5 && voltage set 12` would believe in a
// 5 V ceiling that was never stored.
func TestVLimitSetComparesItsReadBack(t *testing.T) {
	dev := fake.NewTypical() // window 3300-48000 mV
	echoAndKeepTheOldValue(dev, proto.CmdUserVLimit)
	tr := newFakeTree(t, dev)

	if err := tr.run(t, "vlimit", "set", "--low", "3300mV", "--high", "5000mV"); err != nil {
		t.Fatalf("`vlimit set --low 3300mV --high 5000mV`: %v", err)
	}
	if !strings.Contains(tr.stderr.String(), "read back [3300, 48000] mV after writing [3300, 5000] mV") {
		t.Errorf("a window the device did not keep was reported without a word:\n%s", tr.stderr.String())
	}
}

// The same for `current set`, which reads back for the same reason.
func TestCurrentSetComparesItsReadBack(t *testing.T) {
	dev := fake.NewTypical() // 5000 mA
	echoAndKeepTheOldValue(dev, proto.CmdCurrentLimitMa)
	tr := newFakeTree(t, dev)

	if err := tr.run(t, "current", "set", "4.9"); err != nil {
		t.Fatalf("`current set 4.9`: %v", err)
	}
	if !strings.Contains(tr.stderr.String(), "read back 5000 mA after writing 4900 mA") {
		t.Errorf("a current limit the device did not keep was reported without a word:\n%s",
			tr.stderr.String())
	}
}

// And when the read-back cannot be had at all, what is printed is the request,
// so it says so. `voltage set` and `current set` both annotate that case; the
// window -- the one value here that is itself a safety interlock -- did not.
func TestVLimitSetAnnotatesAPairItCouldNotReadBack(t *testing.T) {
	dev := fake.NewTypical()
	var reads int
	dev.SetHandler(proto.CmdUserVLimit, func(f proto.Frame) []byte {
		if f.Write {
			dev.StoreRegister(proto.CmdUserVLimit, f.Payload)
			return append([]byte{}, f.Payload...)
		}
		// The first read is interlock 3's, and it is answered; the read-back
		// after the write is not.
		reads++
		if reads > 1 {
			return nil
		}
		v, _ := dev.Register(proto.CmdUserVLimit)
		return v
	})
	tr := newFakeTree(t, dev)

	if err := tr.run(t, "vlimit", "set", "--low", "3300mV", "--high", "5000mV", "--timeout=200ms"); err != nil {
		t.Fatalf("`vlimit set` with an unanswered read-back: %v", err)
	}
	if !strings.Contains(tr.stdout.String(), "(written, not read back)") {
		t.Errorf("the requested window is presented as the device's answer:\n%s", tr.stdout.String())
	}
}

// answerWritesOnly makes cmd store and echo a write while leaving every read
// unanswered, which is how a single dropped response frame looks from the host
// (SPEC.md §5.2): the setting still works, the question about it does not.
func answerWritesOnly(dev *fake.Device, cmd proto.Cmd) {
	dev.SetHandler(cmd, func(f proto.Frame) []byte {
		if f.Write {
			dev.StoreRegister(cmd, f.Payload)
			return append([]byte{}, f.Payload...)
		}
		return nil
	})
}

// TestCalibrateADCFillsInAnOmittedFlagFromItsOwnRead: `calibrate adc --offset 5`
// needs the current SCALE to fill in the flag it was not given, and it had that
// value in hand -- the scale read succeeded. Failing the whole command because
// the unrelated offset read timed out threw away a perfectly good answer, on a
// path whose reads are two independent commands with no NACK between them.
func TestCalibrateADCFillsInAnOmittedFlagFromItsOwnRead(t *testing.T) {
	dev := fake.NewTypical()
	dev.StoreRegister(proto.CmdVMeasureADCScale, proto.EncodeI32(7))
	answerWritesOnly(dev, proto.CmdVMeasureADCOffset)
	tr := newFakeTree(t, dev)

	if err := tr.run(t, "calibrate", "adc", "--offset", "5", "--yes", "--timeout=200ms"); err != nil {
		t.Fatalf("`calibrate adc --offset 5 --yes` with the offset read unanswered: %v", err)
	}
	if stored, ok := dev.Register(proto.CmdVMeasureADCOffset); !ok ||
		!bytes.Equal(stored, proto.EncodeI32(5)) {
		t.Errorf("device ADC offset = %x, want %x", stored, proto.EncodeI32(5))
	}
	if stored, ok := dev.Register(proto.CmdVMeasureADCScale); !ok ||
		!bytes.Equal(stored, proto.EncodeI32(7)) {
		t.Errorf("device ADC scale = %x, want the 7 it already held", stored)
	}
	// And the term nobody asked to change is not written back at all: the value
	// came from the device one round trip ago, so rewriting it can only spend a
	// non-volatile write, and would make a wrong read permanent.
	if tr.wrote(t, proto.CmdVMeasureADCScale) {
		t.Errorf("the scale was rewritten with the value it already held; frames: %v",
			cmdNames(dev.Sent()))
	}
	if !strings.Contains(tr.stdout.String(), "(unchanged)") {
		t.Errorf("the kept term is presented as one this command wrote:\n%s", tr.stdout.String())
	}
}

// And the read with no consumer left is not issued at all. With both flags
// given, the scale read exists only to print the previous pair beside the
// offset in interlock 5's restore command (SPEC.md §13.5) -- which cannot be
// printed once the offset read has failed. Issuing it anyway costs a round trip,
// and a second full --timeout on a unit that has stopped answering.
func TestCalibrateADCSkipsTheReadWithNoConsumer(t *testing.T) {
	dev := fake.NewTypical()
	answerWritesOnly(dev, proto.CmdVMeasureADCOffset)
	tr := newFakeTree(t, dev)

	if err := tr.run(t, "calibrate", "adc", "--offset", "5", "--scale", "7", "--yes", "--timeout=200ms"); err != nil {
		t.Fatalf("`calibrate adc --offset 5 --scale 7 --yes`: %v", err)
	}
	for _, fr := range dev.Sent() {
		parsed, err := proto.Parse(fr)
		if err != nil {
			t.Fatalf("the device received an unparseable frame %s: %v", proto.Hex(fr), err)
		}
		if parsed.Cmd == proto.CmdVMeasureADCScale && !parsed.Write {
			t.Errorf("the ADC scale was read with nothing left to read it for; frames: %v",
				cmdNames(dev.Sent()))
		}
	}
}

// TestReadBackIsMarkedInJSON pins the machine-readable half of the annotation
// the human output has carried all along. `current set --json` printed the same
// current_limit_ma field whether the value was the device's read-back or only
// the request whose read-back never came, so a script had nothing to key on --
// and the read-back is what SPEC.md §17 (row 6.5) spends a round trip on.
func TestReadBackIsMarkedInJSON(t *testing.T) {
	readBack := func(t *testing.T, out []byte) (bool, bool) {
		t.Helper()
		var doc struct {
			ReadBack *bool `json:"read_back"`
		}
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("decoding the JSON result: %v\n%s", err, out)
		}
		if doc.ReadBack == nil {
			return false, false
		}
		return *doc.ReadBack, true
	}

	t.Run("verified", func(t *testing.T) {
		tr := newFakeTree(t, fake.NewTypical())
		if err := tr.run(t, "current", "set", "3", "--json"); err != nil {
			t.Fatalf("`current set 3 --json`: %v", err)
		}
		verified, ok := readBack(t, tr.stdout.Bytes())
		if !ok || !verified {
			t.Errorf("a value the device reported is not marked as read back: %s", tr.stdout.String())
		}
	})

	t.Run("unverified", func(t *testing.T) {
		dev := fake.NewTypical()
		answerWritesOnly(dev, proto.CmdCurrentLimitMa)
		tr := newFakeTree(t, dev)
		if err := tr.run(t, "current", "set", "3", "--json", "--timeout=200ms"); err != nil {
			t.Fatalf("`current set 3 --json` with the read-back unanswered: %v", err)
		}
		verified, ok := readBack(t, tr.stdout.Bytes())
		if !ok || verified {
			t.Errorf("a value that was never read back is presented as confirmed: %s", tr.stdout.String())
		}
	})
}

// TestLEDSetReportsWhatItWrote pins the annotation that keeps `led set`'s
// output honest. The command does not spend a round trip reading the setting
// back, so the line it prints is the request; without "(written)" it is
// indistinguishable from `voltage set`'s line, which is a read-back. A
// scratchpad-flagged write is acknowledged and echoed and still never commits
// (SPEC.md §14.4), so the difference is real, not pedantic.
func TestLEDSetReportsWhatItWrote(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)
	if err := tr.run(t, "led", "set", "off"); err != nil {
		t.Fatalf("`led set off`: %v", err)
	}
	if !tr.wrote(t, proto.CmdDisableLEDDuringOp) {
		t.Fatalf("`led set off` never reached the device:\n%s", tr.stdout.String())
	}
	var line string
	for _, l := range strings.Split(tr.stdout.String(), "\n") {
		if strings.Contains(l, "led always on") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("`led set off` printed no led line:\n%s", tr.stdout.String())
	}
	if !strings.Contains(line, "(written)") {
		t.Errorf("`led set` presents an unverified write as confirmed state: %q", line)
	}
}

// TestLEDColorSendsTheVendorFrame pins command 13 byte for byte.
//
// This is the one command whose only source is the vendor's published library
// rather than the shipped application or a measurement (SPEC.md §14.11): the
// colour goes in the middle byte and the four around it are sent unchanged
// because that is what the vendor sends. A test that only checked "a write for
// command 13 arrived" would let somebody tidy those four away, and there is no
// read side and no hardware here to notice.
func TestLEDColorSendsTheVendorFrame(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)
	if err := tr.run(t, "led", "color", "magenta"); err != nil {
		t.Fatalf("`led color magenta`: %v", err)
	}

	var frame []byte
	for _, fr := range dev.Sent() {
		parsed, err := proto.Parse(fr)
		if err != nil {
			t.Fatalf("the device received an unparseable frame %s: %v", proto.Hex(fr), err)
		}
		if parsed.Cmd == proto.CmdFlashLEDSeqAdvanced {
			frame = fr
		}
	}
	if frame == nil {
		t.Fatalf("`led color magenta` never reached the device:\n%s", tr.stdout.String())
	}
	// 07 8d 0a 01 06 02 00: length, command 13 with the write bit, then
	// [10, 1, magenta=6, 2, 0].
	if got := proto.Hex(frame); got != "07 8d 0a 01 06 02 00" {
		t.Errorf("frame = %s, want 07 8d 0a 01 06 02 00", got)
	}
	// Written, not read back -- there is no read side at all for this command.
	if !strings.Contains(tr.stdout.String(), "(written)") {
		t.Errorf("`led color` presents an unverified write as confirmed state:\n%s", tr.stdout.String())
	}
}

// An unknown colour is a usage error, and the refusal lists the eight that work
// rather than leaving the caller to guess at a vocabulary it cannot read
// anywhere else.
func TestLEDColorRefusesAnUnknownColour(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev)
	err := tr.run(t, "led", "color", "chartreuse")
	if err == nil {
		t.Fatal("`led color chartreuse` was accepted")
	}
	if code := ExitCode(err); code != ExitUsage {
		t.Errorf("ExitCode = %d, want ExitUsage (%d): %v", code, ExitUsage, err)
	}
	for _, want := range []string{"magenta", "cyan", "off"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not list %q as a valid colour:\n%v", want, err)
		}
	}
	if tr.wrote(t, proto.CmdFlashLEDSeqAdvanced) {
		t.Error("an unknown colour still reached the device")
	}
}

// The Go-literal shapes are usage errors through the real tree as well, so a
// script cannot feed them past cobra.
func TestAuthLockSetRefusesGoLiteralShapes(t *testing.T) {
	clearGflexEnv(t)
	for _, arg := range []string{"0x10", "1_0"} {
		var stdout, stderr bytes.Buffer
		app := &App{stdout: &stdout, stderr: &stderr, stdin: strings.NewReader("")}
		root := NewRootCommand(app)
		root.SetArgs([]string{"authlock", "set", arg, "--dry-run"})

		err := root.ExecuteContext(context.Background())
		if err == nil {
			t.Errorf("`authlock set %s` was accepted; stdout:\n%s", arg, stdout.String())
			continue
		}
		if code := ExitCode(err); code != ExitUsage {
			t.Errorf("`authlock set %s`: ExitCode = %d, want ExitUsage (%d): %v", arg, code, ExitUsage, err)
		}
	}
}
