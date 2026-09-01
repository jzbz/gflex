package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jzbz/gflex/internal/proto"
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
