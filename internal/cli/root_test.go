package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/transport/fake"
)

// clearGflexEnv neutralises any GFLEX_* the developer happens to export, so
// that a test driving the real tree is not judged by their shell. applyEnv
// treats an empty value as unset (SPEC.md §11), which is why this can use
// t.Setenv rather than unsetting and restoring by hand.
func clearGflexEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GFLEX_PORT", "GFLEX_TRANSPORT", "GFLEX_TIMEOUT",
		"GFLEX_BYTE_DELAY", "GFLEX_JSON", "GFLEX_VERBOSE",
		// The last two are not read by applyEnv and deliberately never will
		// be (SPEC.md §11). They are cleared anyway so that
		// TestNoEnvironmentVariableAnswersAConfirmation cannot be defeated by
		// a developer who happens to export one -- a test of an absence has to
		// start from a known absence.
		"GFLEX_YES", "GFLEX_DRY_RUN",
	} {
		t.Setenv(k, "")
	}
}

// TestEnvironmentLayerFillsUnsetFlags covers SPEC.md §11's precedence rule --
// flag > env > default -- which until now was held by review alone.
//
// Both halves matter and they fail differently. If applyEnv stopped reading the
// environment, a user's GFLEX_PORT would silently stop selecting their unit; if
// it stopped honouring f.Changed, an exported variable would silently override
// the flag the user just typed, which is the more dangerous direction.
func TestEnvironmentLayerFillsUnsetFlags(t *testing.T) {
	clearGflexEnv(t)
	t.Setenv("GFLEX_PORT", "/dev/snd/midiC9D0")
	t.Setenv("GFLEX_TRANSPORT", "usb")
	t.Setenv("GFLEX_TIMEOUT", "1500ms")
	t.Setenv("GFLEX_VERBOSE", "true")

	var stdout, stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr, stdin: strings.NewReader("")}
	root := NewRootCommand(app)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	// An explicit --timeout, so the same run proves both halves: the flag wins
	// over GFLEX_TIMEOUT while the three unset flags still take their values
	// from the environment. `version` reaches no device.
	root.SetArgs([]string{"version", "--timeout=9s"})

	if err := root.Execute(); err != nil {
		t.Fatalf("`gflex version`: %v", err)
	}
	if app.Port != "/dev/snd/midiC9D0" {
		t.Errorf("Port = %q, want it filled in from GFLEX_PORT", app.Port)
	}
	if app.Transport != transportUSB {
		t.Errorf("Transport = %q, want %q from GFLEX_TRANSPORT", app.Transport, transportUSB)
	}
	if !app.Verbose {
		t.Error("Verbose = false, want it set from GFLEX_VERBOSE")
	}
	if app.Timeout != 9*time.Second {
		t.Errorf("Timeout = %s, want the explicit --timeout=9s to beat GFLEX_TIMEOUT=1500ms", app.Timeout)
	}
}

// A malformed variable is a usage error, not a generic failure: the user's
// shell is wrong, not their device, and exit 2 is what says so.
func TestMalformedEnvironmentVariableIsAUsageError(t *testing.T) {
	clearGflexEnv(t)
	t.Setenv("GFLEX_TIMEOUT", "half a minute")

	var stdout, stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr, stdin: strings.NewReader("")}
	root := NewRootCommand(app)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"version"})

	err := root.Execute()
	if err == nil {
		t.Fatal("an unparseable GFLEX_TIMEOUT was accepted")
	}
	if code := ExitCode(err); code != ExitUsage {
		t.Errorf("ExitCode = %d, want ExitUsage (%d): %v", code, ExitUsage, err)
	}
	if !strings.Contains(err.Error(), "GFLEX_TIMEOUT") {
		t.Errorf("error %q does not name the variable at fault", err)
	}
}

// TestNoEnvironmentVariableAnswersAConfirmation pins an absence, and the
// absence is the safety decision (SPEC.md §11): --dry-run and --yes are the two
// global flags with no environment counterpart, because a GFLEX_YES left
// exported in a shell profile would pre-answer every §13 confirmation for
// months without anyone noticing.
//
// Adding `get("yes", "GFLEX_YES")` to applyEnv for symmetry is a two-line
// change that reads as tidying up, and until this test existed the whole suite
// stayed green when it was made. The assertion is deliberately the same one the
// other wiring tests use -- what did and did not reach the device -- rather
// than a check on the App field, because the field is not what does the damage.
func TestNoEnvironmentVariableAnswersAConfirmation(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev) // clears the GFLEX_* environment first
	t.Setenv("GFLEX_YES", "1")
	t.Setenv("GFLEX_DRY_RUN", "1")

	err := tr.run(t, "authlock", "set", "1")
	if err == nil {
		t.Fatal("an exported GFLEX_YES pre-answered a §13 confirmation")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if tr.wrote(t, proto.CmdAuthLock) {
		t.Fatalf("the auth lock was written from the environment; frames: %v", cmdNames(dev.Sent()))
	}
}

// openPTY returns the two ends of a fresh pseudo-terminal.
//
// The slave end is a real character device with a line discipline, so
// isTerminal answers yes for it. That is the only honest way to reach the
// prompt at all: interlock 7 (SPEC.md §13.7) refuses before reading anything
// when stdin is not a terminal, and that refusal is the point of the check, so
// it must not be stubbed out to make the happy path testable. x/sys/unix is
// already a dependency and this is the ordinary ptmx dance -- unlock, ask for
// the number, open the slave -- so no test-only hook has to exist in the
// production path either.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals available: %v", err)
	}
	fail := func(what string, err error) {
		m.Close()
		t.Skipf("%s: %v", what, err)
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		fail("cannot unlock a pseudo-terminal", err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		fail("cannot name a pseudo-terminal", err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		fail("cannot open the pseudo-terminal slave", err)
	}
	t.Cleanup(func() {
		s.Close()
		m.Close()
	})
	return m, s
}

// TestConfirmRefusesWhenStdinIsNotATerminal is the regression test for asking
// os.Stdin whether the *App's* stdin is a terminal.
//
// The two can disagree, and the disagreement is dangerous in one specific
// direction: a process launched from a terminal but reading its answers from
// somewhere else -- an embedder, a test, anything that supplies its own
// reader -- used to clear interlock 7 (SPEC.md §13.7) on the strength of the
// terminal it was launched from, then take "y" from a buffer nobody typed into.
// That is precisely the silent over-volt the interlock exists to prevent.
//
// Nothing may be consumed from stdin either: the refusal has to happen before
// the read, or a pipe carrying an unrelated "y" would answer the question.
func TestConfirmRefusesWhenStdinIsNotATerminal(t *testing.T) {
	in := bytes.NewBufferString("y\n")
	var stderr bytes.Buffer
	app := &App{stdout: io.Discard, stderr: &stderr, stdin: in}

	if app.stdinIsTTY() {
		t.Fatal("a bytes.Buffer is being reported as a terminal")
	}
	err := app.confirm(context.Background(), "Set 12000 mV?")
	if err == nil {
		t.Fatal("confirm approved the operation with no terminal to ask")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	for _, want := range []string{"Set 12000 mV?", "stdin is not a terminal", "--yes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	if in.Len() != len("y\n") {
		t.Errorf("confirm consumed %d bytes of stdin before refusing; the answer must not be read",
			len("y\n")-in.Len())
	}
	if stderr.Len() != 0 {
		t.Errorf("a prompt was written with no terminal to read it: %q", stderr.String())
	}
}

// TestConfirmIgnoresTheProcessTerminal is the sharp edge of the same
// regression, and the case the test above cannot reach: the process is attached
// to a terminal while the answers come from somewhere else entirely.
//
// Judging os.Stdin let that combination through -- the check passed on the
// strength of the terminal the process was launched from, and the "y" was then
// taken from a buffer nobody typed into, approving a write to a live rail. The
// pseudo-terminal here stands in for that terminal; only App's own stdin, the
// reader the answer would actually come from, is allowed to decide.
//
// Swapping the process-wide os.Stdin is safe because after this change nothing
// in the package reads it outside Execute, and no test here runs in parallel.
func TestConfirmIgnoresTheProcessTerminal(t *testing.T) {
	_, slave := openPTY(t)
	saved := os.Stdin
	os.Stdin = slave
	t.Cleanup(func() { os.Stdin = saved })

	in := bytes.NewBufferString("y\n")
	var stderr bytes.Buffer
	app := &App{stdout: io.Discard, stderr: &stderr, stdin: in}

	err := app.confirm(context.Background(), "Set 12000 mV?")
	if err == nil {
		t.Fatal("confirm took its answer from a buffer because the process had a terminal")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if in.Len() != len("y\n") {
		t.Error("the buffered answer was read even though it could not have been typed at the prompt")
	}
}

// TestConfirmYesNeedsNoTerminal covers the documented way through in a script:
// --yes answers ahead of time, so the terminal check never applies.
func TestConfirmYesNeedsNoTerminal(t *testing.T) {
	in := bytes.NewBufferString("n\n")
	var stderr bytes.Buffer
	app := &App{stdout: io.Discard, stderr: &stderr, stdin: in, Yes: true}

	if err := app.confirm(context.Background(), "Set 12000 mV?"); err != nil {
		t.Fatalf("confirm with --yes: %v", err)
	}
	if in.Len() != len("n\n") {
		t.Error("--yes read from stdin; the answer was already given on the command line")
	}
	if stderr.Len() != 0 {
		t.Errorf("--yes printed a prompt: %q", stderr.String())
	}
}

// TestConfirmOnATerminal drives the prompt itself, which the TTY check now
// makes reachable without a terminal on the test binary's own stdin.
//
// One App answers every question, as a real session does, because that is what
// the lazily-created bufio.Reader in App has to survive: a second read must not
// lose input the first one buffered.
func TestConfirmOnATerminal(t *testing.T) {
	master, slave := openPTY(t)
	var stderr bytes.Buffer
	app := &App{stdout: io.Discard, stderr: &stderr, stdin: slave}

	if !app.stdinIsTTY() {
		t.Fatal("a pseudo-terminal is not being recognised as a terminal")
	}

	cases := []struct {
		answer string
		wantOK bool
	}{
		{answer: "y\n", wantOK: true},
		{answer: "YES\n", wantOK: true},
		{answer: "  y  \n", wantOK: true},
		// Anything else is a refusal. The prompt says [y/N]: silence, a typo
		// and a deliberate "no" must all leave the rail alone.
		{answer: "n\n", wantOK: false},
		{answer: "\n", wantOK: false},
		{answer: "yeah ok\n", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.answer), func(t *testing.T) {
			stderr.Reset()
			if _, err := master.WriteString(tc.answer); err != nil {
				t.Fatalf("writing %q to the terminal: %v", tc.answer, err)
			}
			// A bounded context so that a prompt that never returns fails this
			// test instead of hanging until the package timeout.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := app.confirm(ctx, "Set 12000 mV?")
			if ctx.Err() != nil {
				t.Fatalf("confirm never read the answer %q", tc.answer)
			}
			switch {
			case tc.wantOK && err != nil:
				t.Errorf("answering %q was refused: %v", tc.answer, err)
			case !tc.wantOK && err == nil:
				t.Errorf("answering %q approved the operation", tc.answer)
			case !tc.wantOK && ExitCode(err) != ExitRefused:
				t.Errorf("answering %q gave exit code %d, want ExitRefused (%d)",
					tc.answer, ExitCode(err), ExitRefused)
			}
			// The question goes to stderr so that --json keeps stdout clean.
			if got := stderr.String(); got != "Set 12000 mV? [y/N] " {
				t.Errorf("prompt on stderr = %q, want %q", got, "Set 12000 mV? [y/N] ")
			}
		})
	}
}

// pause has the same terminal requirement as confirm but is not an interlock:
// there is nothing to refuse, so it is a usage error that names the flags for
// running unattended.
func TestPauseRefusesWhenStdinIsNotATerminal(t *testing.T) {
	in := bytes.NewBufferString("\n")
	var stderr bytes.Buffer
	app := &App{stdout: io.Discard, stderr: &stderr, stdin: in}

	err := app.pause(context.Background(), "Plug the supply in and press Enter.")
	if err == nil {
		t.Fatal("pause returned with no terminal to wait on")
	}
	if code := ExitCode(err); code != ExitUsage {
		t.Errorf("ExitCode = %d, want ExitUsage (%d): %v", code, ExitUsage, err)
	}
	if !strings.Contains(err.Error(), "--no-prompt") {
		t.Errorf("error %q does not say how to run this unattended", err)
	}
}

// TestPauseEOFIsAUsageErrorNotARefusal is the other half of the same
// distinction, reached from a real terminal.
//
// readLine used to hand back refused("no answer read from stdin") itself, and
// refused() appends the fixed sentence "nothing was written to the device"
// (exit.go) -- deliberately, because it is true for a §13 confirmation, which
// is always evaluated before the write it guards. pause is not a confirmation.
// Its only caller is the scan wizard's handover, which runs AFTER the capture
// log has been erased, so a Ctrl-D at that prompt printed a §13-shaped refusal
// whose central claim was false at exactly the moment the user needed to know
// their capture was gone.
func TestPauseEOFIsAUsageErrorNotARefusal(t *testing.T) {
	master, slave := openPTY(t)
	var stderr bytes.Buffer
	app := &App{stdout: io.Discard, stderr: &stderr, stdin: slave}
	// A literal EOT, which is what Ctrl-D sends: in canonical mode the line
	// discipline turns it into a zero-length read at the start of a line, so
	// the reader sees EOF while the descriptor stays a terminal. Closing the
	// master instead would hang the slave up, and the TTY check above would
	// then be what refused -- the test would pass without ever reaching the
	// branch it exists to cover.
	if _, err := master.WriteString("\x04"); err != nil {
		t.Fatalf("sending EOT to the terminal: %v", err)
	}
	if !app.stdinIsTTY() {
		t.Fatal("a pseudo-terminal is not being recognised as a terminal")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := app.pause(ctx, "Press Enter once the VFLEX is plugged back in...")
	if ctx.Err() != nil {
		t.Fatal("pause never noticed the end of input")
	}
	if err == nil {
		t.Fatal("pause returned success with no answer given")
	}
	if code := ExitCode(err); code != ExitUsage {
		t.Errorf("ExitCode = %d, want ExitUsage (%d): %v", code, ExitUsage, err)
	}
	// Names the branch, so a hung-up terminal taking the "not a terminal" path
	// cannot stand in for the one under test.
	if !strings.Contains(err.Error(), "no answer read from stdin") {
		t.Errorf("error %q is not the end-of-input branch", err)
	}
	if strings.Contains(err.Error(), "nothing was written to the device") {
		t.Errorf("pause claims nothing was written, but `scan` erases the capture log before "+
			"it prompts:\n%v", err)
	}
	if !strings.Contains(err.Error(), "--no-prompt") {
		t.Errorf("error %q does not say how to run this unattended", err)
	}
}

// The asymmetry is the point, so the interlock side is pinned too: the same EOF
// at a confirmation must still be a refusal, and must still say nothing was
// written -- which is true there. Without this, "fixing" the test above by
// making readLine never refuse would look like a pass.
func TestConfirmEOFStillRefuses(t *testing.T) {
	master, slave := openPTY(t)
	var stderr bytes.Buffer
	app := &App{stdout: io.Discard, stderr: &stderr, stdin: slave}
	if _, err := master.WriteString("\x04"); err != nil { // Ctrl-D, as above
		t.Fatalf("sending EOT to the terminal: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := app.confirm(ctx, "Set 12000 mV?")
	if ctx.Err() != nil {
		t.Fatal("confirm never noticed the end of input")
	}
	if err == nil {
		t.Fatal("confirm approved the operation with no answer given")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if !strings.Contains(err.Error(), "nothing was written to the device") {
		t.Errorf("refusal %q dropped the sentence that says the rail is untouched", err)
	}
}

// TestUnknownRootCommandIsRejected guards the branch in the root RunE.
//
// root.Args is cobra.ArbitraryArgs, which means cobra's own legacyArgs check
// never runs and nothing but that branch stands between a mistyped command and
// exit 0 with no output -- the failure mode a script guarded with `|| exit 1`
// sails straight past.
func TestUnknownRootCommandIsRejected(t *testing.T) {
	clearGflexEnv(t)
	var stdout, stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr, stdin: strings.NewReader("")}
	root := NewRootCommand(app)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"voltag", "12"}) // a plausible typo for `voltage`

	err := root.Execute()
	if err == nil {
		t.Fatalf("`gflex voltag 12` was accepted; stdout: %q", stdout.String())
	}
	if code := ExitCode(err); code != ExitUsage {
		t.Errorf("ExitCode = %d, want ExitUsage (%d): %v", code, ExitUsage, err)
	}
	if !strings.Contains(err.Error(), `unknown command "voltag"`) {
		t.Errorf("error = %q, want it to name the unknown command", err)
	}
	// It must carry its own exit code rather than rely on ExitCode matching
	// cobra's phrasing, which is a thing releases are free to change.
	var ce *CodedError
	if !errors.As(err, &ce) {
		t.Errorf("error %q is not a CodedError; it would be classified by its text", err)
	}
}

// The other half of the same branch: bare `gflex` is not an error, it is a
// request for help, and the help has to reach the App's stdout.
func TestRootWithNoArgumentsPrintsHelp(t *testing.T) {
	clearGflexEnv(t)
	var stdout, stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr, stdin: strings.NewReader("")}
	root := NewRootCommand(app)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	// An explicit empty slice, not nil: cobra falls back to os.Args[1:] when
	// args are nil, which under `go test` is a list of -test.* flags.
	root.SetArgs([]string{})

	if err := root.Execute(); err != nil {
		t.Fatalf("bare `gflex`: %v", err)
	}
	for _, want := range []string{"Usage:", "voltage", "firmware"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("help output does not mention %q:\n%s", want, stdout.String())
		}
	}
}
