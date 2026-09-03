package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/pdo"
	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/session"
	"github.com/jzbz/gflex/internal/transport/fake"
)

// serialReader is a scripted CMD_SERIAL_NUMBER, counting how many times it was
// asked so a test can prove the retry did or did not happen.
type serialReader struct {
	replies []struct {
		serial string
		err    error
	}
	calls int
}

func (r *serialReader) read(context.Context) (string, error) {
	i := r.calls
	r.calls++
	if i >= len(r.replies) {
		i = len(r.replies) - 1 // the last reply repeats forever
	}
	return r.replies[i].serial, r.replies[i].err
}

func scripted(pairs ...[2]any) *serialReader {
	r := &serialReader{}
	for _, p := range pairs {
		serial, _ := p[0].(string)
		err, _ := p[1].(error)
		r.replies = append(r.replies, struct {
			serial string
			err    error
		}{serial, err})
	}
	return r
}

// TestScanSerialPolicyMatchesSpec pins the numbers themselves. SPEC.md §9.2
// specifies six attempts 300 ms apart for the serial reads that bracket a scan,
// and the whole point of the fix is that the CLI now honours them.
func TestScanSerialPolicyMatchesSpec(t *testing.T) {
	if scanSerialAttempts != 6 {
		t.Errorf("scanSerialAttempts = %d, SPEC.md §9.2 says 6", scanSerialAttempts)
	}
	if scanSerialRetryDelay != 300*time.Millisecond {
		t.Errorf("scanSerialRetryDelay = %s, SPEC.md §9.2 says 300ms", scanSerialRetryDelay)
	}
}

// TestReadSerialRetryingSurvivesASlowReconnect is the regression test.
//
// The reconnect read used to be a single attempt, so one dropped frame aborted
// the scan at the point where the user had already unplugged the device, walked
// it to a charger, waited and plugged it back in. A just-enumerated unit
// answering slowly is the normal case there, and the protocol has no NACK, so a
// lone timeout says nothing at all (SPEC.md §5.2).
func TestReadSerialRetryingSurvivesASlowReconnect(t *testing.T) {
	r := scripted(
		[2]any{"", session.ErrTimeout},
		[2]any{"", session.ErrTimeout},
		[2]any{"VF001234", nil},
	)
	got, err := readSerialRetrying(context.Background(), r.read, scanSerialAttempts, time.Microsecond)
	if err != nil {
		t.Fatalf("two timeouts then a good read should succeed, got %v", err)
	}
	if got != "VF001234" {
		t.Errorf("serial = %q, want %q", got, "VF001234")
	}
	if r.calls != 3 {
		t.Errorf("read %d times, want 3 (it must stop as soon as a serial is readable)", r.calls)
	}
}

// A response that sanitises to fewer than four characters identifies nothing
// (SPEC.md §6.4), so it counts as "could not read it" and is retried too --
// a settling unit answering with padding is the same event as a timeout.
func TestReadSerialRetryingRetriesAnUnusableSerial(t *testing.T) {
	r := scripted(
		[2]any{"", nil},
		[2]any{"VF0", nil},
		[2]any{"VF001234", nil},
	)
	got, err := readSerialRetrying(context.Background(), r.read, scanSerialAttempts, time.Microsecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "VF001234" || r.calls != 3 {
		t.Errorf("serial = %q after %d reads, want %q after 3", got, r.calls, "VF001234")
	}
}

// TestReadSerialRetryingNeverRetriesPastAReadableSerial is the invariant half,
// and it is the more important of the two.
//
// The serial equality check is the hard invariant of SPEC.md §9.2: the unit
// whose log was erased must be the unit read back. A retry that kept going
// after a perfectly readable serial would convert "this is a different VFLEX"
// into "ask again until one of them agrees" -- and the scan would then decode
// one unit's capture and label it with another's, with nothing downstream able
// to tell. So the first readable answer is final, even when a later attempt
// would have returned the serial the caller is hoping for.
func TestReadSerialRetryingNeverRetriesPastAReadableSerial(t *testing.T) {
	r := scripted(
		[2]any{"OTHER999", nil},
		[2]any{"VF001234", nil}, // must never be reached
	)
	got, err := readSerialRetrying(context.Background(), r.read, scanSerialAttempts, time.Microsecond)
	if err != nil {
		t.Fatalf("a readable serial is not an error: %v", err)
	}
	if got != "OTHER999" {
		t.Fatalf("serial = %q, want %q: the mismatching unit must be reported, not retried away", got, "OTHER999")
	}
	if r.calls != 1 {
		t.Errorf("read %d times, want 1: retrying past a readable serial would soften the §9.2 invariant", r.calls)
	}
}

// Exhausting the attempts is a failure, not a silent empty serial: an empty
// string compared against the latched one would be reported as a mismatch,
// which blames the wrong thing.
func TestReadSerialRetryingGivesUp(t *testing.T) {
	r := scripted([2]any{"", session.ErrTimeout})
	got, err := readSerialRetrying(context.Background(), r.read, 4, time.Microsecond)
	if err == nil {
		t.Fatalf("expected failure after 4 unreadable attempts, got %q", got)
	}
	if r.calls != 4 {
		t.Errorf("read %d times, want 4", r.calls)
	}
	if !errors.Is(err, session.ErrTimeout) {
		t.Errorf("the last cause must survive for ExitCode to classify it; got %v", err)
	}
	if got != "" {
		t.Errorf("serial = %q on failure, want empty", got)
	}
}

// A cancelled context stops the retry rather than sitting out the remaining
// attempts: Ctrl-C during a scan has to unwind so the deferred Close releases
// the device.
func TestReadSerialRetryingHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := scripted([2]any{"", session.ErrTimeout})
	if _, err := readSerialRetrying(ctx, r.read, scanSerialAttempts, time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if r.calls != 1 {
		t.Errorf("read %d times after cancellation, want 1", r.calls)
	}
}

// TestScanDryRunListsBothSerialReads holds `scan --dry-run` to interlock 8: the
// workflow of SPEC.md §9.2 reads the serial twice, once to latch it and once to
// check it, and a listing that shows one misdescribes the step the whole
// workflow turns on.
func TestScanDryRunListsBothSerialReads(t *testing.T) {
	frames, err := scanFrames()
	if err != nil {
		t.Fatalf("scanFrames: %v", err)
	}
	got := cmdNames(frames)
	want := []string{
		proto.CmdFirmwareVersion.String(),
		proto.CmdSerialNumber.String(),
		proto.CmdPDOLog.String(), // erase
		proto.CmdSerialNumber.String(),
	}
	if len(got) != len(want)+pdoChunks {
		t.Fatalf("scan lists %d frames, want %d preamble + %d chunk reads: %v",
			len(got), len(want), pdoChunks, got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("frame %d = %s, want %s\n  full listing: %v", i, got[i], w, got)
		}
	}
	// The erase really is a write; a read of command 17 is a chunk request.
	erase, err := proto.Parse(frames[2])
	if err != nil {
		t.Fatalf("parsing the erase frame: %v", err)
	}
	if !erase.Write || len(erase.Payload) != 0 {
		t.Errorf("erase frame = %s, want a write with an empty payload (02 91)", proto.Hex(frames[2]))
	}
}

// ---------------------------------------------------------------------------
// The --no-prompt handover watch
// ---------------------------------------------------------------------------

// handoverWaiter is a scripted waitForDevice, recording the sequence of
// presence edges scanAwaitHandover asked for.
type handoverWaiter struct {
	calls []struct {
		want    bool
		timeout time.Duration
	}
	// respond decides call i's outcome; nil means every wait succeeds.
	respond func(i int, want bool) error
}

func (w *handoverWaiter) wait(_ context.Context, want bool, timeout time.Duration) error {
	i := len(w.calls)
	w.calls = append(w.calls, struct {
		want    bool
		timeout time.Duration
	}{want, timeout})
	if w.respond != nil {
		return w.respond(i, want)
	}
	return nil
}

// TestScanNoPromptUSBWaitsForReappearanceFirst is the regression test for the
// handover watch trusting the rawmidi node under --transport usb.
//
// The phase-1 session claims the MIDI interface with a kernel-driver detach
// (SPEC.md §4.2), so at handover time the node's absence tracks this process,
// not the device. The old sequence began by waiting for the node to VANISH; it
// was already gone, so the "unplug" was detected instantly, the settle passed
// while Close's reattach brought the node back, the serial matched the
// never-moved unit, and the just-erased log was reported as a completed scan
// (SPEC.md §9.2's hard invariant defeated without a single check failing).
// The watch must instead begin by waiting for the node to REAPPEAR, so that a
// later absence is a genuine departure.
func TestScanNoPromptUSBWaitsForReappearanceFirst(t *testing.T) {
	app := &App{Transport: transportUSB}
	f := newFormatter(false, io.Discard, io.Discard)
	o := scanOpts{noPrompt: true, wait: time.Minute}

	w := &handoverWaiter{}
	if err := app.scanAwaitHandover(context.Background(), f, o, w.wait); err != nil {
		t.Fatalf("a clean handover failed: %v", err)
	}
	if len(w.calls) != 3 {
		t.Fatalf("waited %d times, want 3 (reappear, depart, return): %+v", len(w.calls), w.calls)
	}
	if !w.calls[0].want {
		t.Fatal("the first wait was for departure: the node is absent because THIS process detached " +
			"the driver, so that reads our own detach as the user's unplug")
	}
	if w.calls[0].timeout >= o.wait {
		t.Errorf("the reappear gate waited %s; it must be bounded well below --wait (%s), rebinding is local",
			w.calls[0].timeout, o.wait)
	}
	if w.calls[1].want || !w.calls[2].want {
		t.Errorf("after the gate the watch must be depart-then-return, got %+v", w.calls[1:])
	}
}

// If the node never reappears, presence proves nothing on this system -- the
// driver may not be loaded at all -- and the scan must refuse rather than
// guess: a wrong guess here fabricates a capture. The message points at the
// interactive mode that still works.
func TestScanNoPromptUSBRefusesWhenNodeNeverReturns(t *testing.T) {
	app := &App{Transport: transportUSB}
	f := newFormatter(false, io.Discard, io.Discard)
	o := scanOpts{noPrompt: true, wait: time.Minute}

	w := &handoverWaiter{respond: func(i int, _ bool) error {
		if i == 0 {
			return codedf(ExitNoDevice, "timed out after %s waiting for the VFLEX to reappear", scanReattachGrace)
		}
		return nil
	}}
	err := app.scanAwaitHandover(context.Background(), f, o, w.wait)
	if err == nil {
		t.Fatal("the reappear gate timed out and the handover proceeded anyway")
	}
	if len(w.calls) != 1 {
		t.Errorf("waited %d times after the gate failed, want 1: presence must not be consulted further", len(w.calls))
	}
	for _, want := range []string{"--no-prompt", "--transport usb"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q:\n%v", want, err)
		}
	}
}

// A cancelled context during the gate is Ctrl-C, not a driver problem, and
// must unwind as itself so Execute reports "interrupted".
func TestScanNoPromptUSBGatePropagatesCancellation(t *testing.T) {
	app := &App{Transport: transportUSB}
	f := newFormatter(false, io.Discard, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w := &handoverWaiter{respond: func(int, bool) error { return ctx.Err() }}
	err := app.scanAwaitHandover(ctx, f, scanOpts{noPrompt: true, wait: time.Minute}, w.wait)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled undisguised", err)
	}
}

// Under the default rawmidi transport nothing in this process hides the node,
// so the watch stays exactly what it was: depart, settle, return.
func TestScanNoPromptRawMIDIUnchanged(t *testing.T) {
	app := &App{Transport: transportRawMIDI, midiPortVIDConfirmed: true}
	f := newFormatter(false, io.Discard, io.Discard)
	o := scanOpts{noPrompt: true, wait: time.Minute}

	w := &handoverWaiter{}
	if err := app.scanAwaitHandover(context.Background(), f, o, w.wait); err != nil {
		t.Fatalf("a clean rawmidi handover failed: %v", err)
	}
	if len(w.calls) != 2 || w.calls[0].want || !w.calls[1].want {
		t.Errorf("rawmidi watch = %+v, want exactly [depart, return]: presence is already meaningful there", w.calls)
	}
}

// ---------------------------------------------------------------------------
// The interactive handover
// ---------------------------------------------------------------------------

// interactiveApp returns an App whose stdin is a real terminal with a keypress
// already waiting, so scanHandover's pause returns as if the user had answered.
// Interlock 7 (SPEC.md §13.7) refuses on a non-TTY before reading anything, so
// a pty is the only honest way to reach the code after the prompt.
func interactiveApp(t *testing.T, transport string) *App {
	t.Helper()
	master, slave := openPTY(t)
	if _, err := master.WriteString("\n"); err != nil {
		t.Fatalf("writing the keypress to the terminal: %v", err)
	}
	// midiPortVIDConfirmed models the ordinary case these tests are about: the
	// port openRawMIDI opened was identified by the vendor ID, so the ALSA node
	// is a usable presence signal (see midiPresenceMeaningful). Under
	// --transport usb it is not, and that is decided by the transport.
	return &App{
		Transport:            transport,
		stdout:               io.Discard,
		stderr:               io.Discard,
		stdin:                slave,
		midiPortVIDConfirmed: true,
	}
}

// TestScanInteractiveUSBDoesNotWaitOnThePort is the regression test for the
// interactive handover consulting the rawmidi node under --transport usb.
//
// The --no-prompt path was gated on midiPresenceMeaningful; this one was not,
// and waited on the node unconditionally after the keypress. Under that
// transport the node cannot be trusted to appear at all -- this process
// detached snd-usb-audio to claim the interface (SPEC.md §4.2), and on a
// headless box the driver may never be loaded -- so the wait aborted the scan
// at its final step, after the log had been erased and the trip to the charger
// made. The --no-prompt refusal even sends the user here. The keypress IS the
// handover signal in this mode; the serial match after the reconnect is what
// keeps the capture honest (SPEC.md §9.2).
func TestScanInteractiveUSBDoesNotWaitOnThePort(t *testing.T) {
	app := interactiveApp(t, transportUSB)
	f := newFormatter(false, io.Discard, io.Discard)

	// Any consultation of the node fails, as it does on the host this bug was
	// found on: if the handover asks, the scan dies.
	w := &handoverWaiter{respond: func(int, bool) error {
		return codedf(ExitNoDevice, "timed out after %s waiting for the VFLEX to reappear", scanReplugGrace)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := app.scanHandover(ctx, f, scanOpts{}, w.wait); err != nil {
		t.Fatalf("the interactive handover failed on a signal that cannot arrive here: %v", err)
	}
	if len(w.calls) != 0 {
		t.Errorf("presence was consulted %d time(s) on --transport usb: %+v", len(w.calls), w.calls)
	}
}

// Under rawmidi presence does track the device, so the grace period for ALSA to
// publish the node after the user's replug stays exactly as it was.
func TestScanInteractiveRawMIDIStillWaitsForTheNode(t *testing.T) {
	app := interactiveApp(t, transportRawMIDI)
	f := newFormatter(false, io.Discard, io.Discard)

	w := &handoverWaiter{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := app.scanHandover(ctx, f, scanOpts{}, w.wait); err != nil {
		t.Fatalf("a clean interactive handover failed: %v", err)
	}
	if len(w.calls) != 1 || !w.calls[0].want {
		t.Fatalf("rawmidi handover = %+v, want exactly one wait for the node to appear", w.calls)
	}
	if w.calls[0].timeout != scanReplugGrace {
		t.Errorf("grace = %s, want scanReplugGrace (%s)", w.calls[0].timeout, scanReplugGrace)
	}
}

// And on rawmidi a node that never appears still stops the scan before the
// reconnect, with the wording that says what was actually observed.
func TestScanInteractiveRawMIDIReportsAMissingNode(t *testing.T) {
	app := interactiveApp(t, transportRawMIDI)
	f := newFormatter(false, io.Discard, io.Discard)

	w := &handoverWaiter{respond: func(int, bool) error {
		return codedf(ExitNoDevice, "timed out after %s waiting for the VFLEX to reappear", scanReplugGrace)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := app.scanHandover(ctx, f, scanOpts{}, w.wait)
	if err == nil {
		t.Fatal("the node never came back and the handover reported success")
	}
	if !strings.Contains(err.Error(), "not visible again") {
		t.Errorf("error %q does not say the unit was not seen again", err)
	}
}

// A dead link is not a slow unit. The 6 x 300 ms patience exists for a
// just-enumerated device answering slowly (SPEC.md §9.2); once the transport
// itself is gone every attempt fails identically and instantly, so retrying
// only delays telling the user the scan is over. Same classification as the
// session's own retry loops (session.PermanentErr).
func TestReadSerialRetryingFailsFastOnADeadLink(t *testing.T) {
	r := scripted(
		[2]any{"", session.ErrTransportClosed},
		[2]any{"VF001234", nil}, // must never be reached
	)
	_, err := readSerialRetrying(context.Background(), r.read, scanSerialAttempts, time.Hour)
	if !errors.Is(err, session.ErrTransportClosed) {
		t.Fatalf("error = %v, want ErrTransportClosed", err)
	}
	if r.calls != 1 {
		t.Errorf("read %d times, want 1: a permanent failure earns no retry", r.calls)
	}
}

// ---------------------------------------------------------------------------
// The terminal precondition
// ---------------------------------------------------------------------------

// TestScanRefusesBeforeErasingWhenStdinIsNotATerminal is the regression test for
// the ordering of the interactive scan's terminal check.
//
// Phase 1 used to run unconditionally: connect, gate on firmware, latch the
// serial, and then erase the capture log -- and only afterwards did the wizard
// reach pause, whose first act is to refuse when stdin is not a terminal. So
// `gflex scan </dev/null`, or the same command from cron or a pipeline, destroyed
// whatever capture the unit was holding and then told the user they had mistyped
// the command line. SPEC.md §9.2 has no recovery for that short of carrying the
// unit back to the source; --yes does not help either, because pause is not a
// confirmation.
//
// The condition needs no device at all -- stdinIsTTY is a local ioctl -- so the
// refusal belongs ahead of every frame, which is what this asserts: the log the
// fake was holding is still there, byte for byte.
func TestScanRefusesBeforeErasingWhenStdinIsNotATerminal(t *testing.T) {
	dev := fake.NewTypical()
	tr := newFakeTree(t, dev) // stdin is deliberately not a terminal
	before, ok := dev.Register(proto.CmdPDOLog)
	if !ok {
		t.Fatal("the fake is not holding a capture to lose")
	}
	before = append([]byte{}, before...)

	err := tr.run(t, "scan")
	if err == nil {
		t.Fatal("`gflex scan` ran the interactive workflow with no terminal to prompt at")
	}
	if code := ExitCode(err); code != ExitUsage {
		t.Errorf("ExitCode = %d, want ExitUsage (%d): %v", code, ExitUsage, err)
	}
	if !strings.Contains(err.Error(), "--no-prompt") {
		t.Errorf("error %q does not say how to run this unattended", err)
	}
	// The load-bearing assertions, in the order they would fail: no frame at
	// all, and in particular no erase.
	if len(dev.Sent()) != 0 {
		t.Errorf("the device was talked to before the terminal check; frames: %v", cmdNames(dev.Sent()))
	}
	if after, ok := dev.Register(proto.CmdPDOLog); !ok || !bytes.Equal(after, before) {
		t.Fatalf("the capture log was erased before the command refused: %x -> %x", before, after)
	}
}

// TestScanVerdictStatesTheCableCeilingOnce holds one fact to one wording.
//
// The verdict's current row used to re-derive the cable bound and append its
// own parenthetical, two lines above the note pdo.finish attaches to
// Match.Messages on exactly the same condition -- so a scan against a source
// advertising more than 5 A said the same thing twice, in two phrasings, in one
// block. A reader has no way to tell a restatement from a second claim about a
// second limit. The pdo package exported SPRAVSAssumptionClause rather than let
// a disclosure be spelled two ways; the cable ceiling is the other disclosure
// and gets the same treatment.
func TestScanVerdictStatesTheCableCeilingOnce(t *testing.T) {
	log := &pdo.Log{
		NPDOsReceived: 1,
		PDOs: []pdo.PDO{{
			Index: 0, Kind: pdo.KindFixed, VoltageV: 9,
			MaxCurrentA: pdo.MaxCableCurrentA, DeclaredMaxCurrentA: 10.23, Valid: true,
		}},
	}
	m := log.Evaluate(9, 4)
	if len(m.Messages) == 0 {
		t.Fatal("pdo.Evaluate attached no cable-bound note, so there is nothing to duplicate")
	}

	var out, errBuf bytes.Buffer
	f := newFormatter(false, &out, &errBuf)
	emitMatch(f, m, 9, 4)
	if err := f.Flush(); err != nil {
		t.Fatalf("flushing the formatter: %v", err)
	}
	got := out.String()

	if n := strings.Count(got, "no USB-C cable"); n != 1 {
		t.Errorf("the cable ceiling is stated %d times in one verdict, want once:\n%s", n, got)
	}
	// And it is still stated: dropping the row's parenthetical must not take
	// the disclosure with it, or a 140 W supply reads as under-powered for no
	// visible reason.
	if !strings.Contains(got, "10.23") {
		t.Errorf("the verdict reports a reduced current without saying what the source declared:\n%s", got)
	}
}
