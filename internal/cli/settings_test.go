package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jzbz/gflex/internal/proto"
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
