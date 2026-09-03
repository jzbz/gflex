package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/framer"
	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/usbfs"
)

// TestMonitorLoopAlwaysReportsTheTerminalError is the regression test for
// returning on the first channel that reported !ok.
//
// The channels here are hand-built in exactly the state the framer leaves
// behind after an unplug: the reader has exited, both channels are closed, the
// decoded frames and the terminal read error are still buffered. A receive
// from a closed channel is always ready -- buffered values first, then !ok --
// so the old select chose uniformly among ready cases and returned nil on the
// first !ok it drew: the terminal error could be left unread ("exit 0 printing
// nothing"), buffered frames could be abandoned (bring-up evidence for
// SPEC.md §14.13/§14.14), and even when the error WAS printed the loop still
// returned nil, so a real transport failure always exited 0. The iterations
// cover the select's orderings; run with -count=50 for more. session.go
// documents the correct pattern at its drain sites ("a closed channel is not
// a reason to stop"); monitorLoop now follows it.
func TestMonitorLoopAlwaysReportsTheTerminalError(t *testing.T) {
	frame := proto.Read(proto.CmdSerialNumber)
	const wantFrames = 3

	for i := 0; i < 40; i++ {
		frames := make(chan []byte, 16)
		for j := 0; j < wantFrames; j++ {
			frames <- frame
		}
		close(frames)
		errs := make(chan error, 4)
		errs <- errors.New("device unplugged (ENODEV)")
		close(errs)

		var out bytes.Buffer
		app := &App{stdout: &out, stderr: io.Discard}
		err := app.monitorLoop(context.Background(), frames, errs, make(chan monitorDrop, 4))

		if err == nil {
			t.Fatalf("iteration %d: the terminal transport error was swallowed; monitor would exit 0", i)
		}
		if !strings.Contains(err.Error(), "ENODEV") {
			t.Fatalf("iteration %d: returned error %v does not carry the transport failure", i, err)
		}
		if !strings.Contains(out.String(), "ENODEV") {
			t.Errorf("iteration %d: the terminal error was returned but never printed", i)
		}
		if got := strings.Count(out.String(), proto.Hex(frame)); got != wantFrames {
			t.Fatalf("iteration %d: %d of %d buffered frames were printed; none may be abandoned:\n%s",
				i, got, wantFrames, out.String())
		}
	}
}

// scriptedTransport hands out its reads one per ReadMIDI call, then fails
// every subsequent read with a terminal error, as an unplugged device does.
type scriptedTransport struct {
	mu    sync.Mutex
	reads [][]byte
	err   error
}

func (s *scriptedTransport) ReadMIDI(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reads) > 0 {
		n := copy(p, s.reads[0])
		s.reads = s.reads[1:]
		return n, nil
	}
	return 0, s.err
}

func (s *scriptedTransport) WriteMIDI([]byte) error { return nil }
func (s *scriptedTransport) Name() string           { return "scripted" }
func (s *scriptedTransport) Close() error           { return nil }

// The same property over a real framer, end to end: frames arrive, the
// transport dies, and monitorLoop both prints everything and reports the
// failure. Close is deliberately not called until the loop has returned --
// the loop returning is itself the proof that both channels closed, so the
// reader has exited and the error cannot have been suppressed as
// close-induced.
func TestMonitorLoopOverARealFramer(t *testing.T) {
	frame := proto.Read(proto.CmdSerialNumber)
	encoded := framer.EncodeMIDI(frame)
	st := &scriptedTransport{
		reads: [][]byte{encoded, encoded, encoded},
		err:   errors.New("device unplugged (ENODEV)"),
	}
	fr := framer.New(st, time.Nanosecond)
	fr.Start()

	var out bytes.Buffer
	app := &App{stdout: &out, stderr: io.Discard}
	err := app.monitorLoop(context.Background(), fr.Frames(), fr.Errors(), make(chan monitorDrop, 4))
	if cerr := fr.Close(); cerr != nil {
		t.Fatalf("closing the framer: %v", cerr)
	}

	if err == nil {
		t.Fatal("the terminal transport error was swallowed; monitor would exit 0")
	}
	if !strings.Contains(err.Error(), "ENODEV") {
		t.Fatalf("returned error %v does not carry the transport failure", err)
	}
	if got := strings.Count(out.String(), proto.Hex(frame)); got != 3 {
		t.Errorf("%d of 3 frames were printed:\n%s", got, out.String())
	}
}

// Drop notices buffered behind the last frame are evidence too -- for this
// command, most of the point (SPEC.md §3.3, §14.13, §14.14) -- so shutdown
// drains them instead of abandoning them.
func TestMonitorLoopDrainsDropsOnShutdown(t *testing.T) {
	frames := make(chan []byte)
	errs := make(chan error, 1)
	errs <- errors.New("device unplugged (ENODEV)")
	close(frames)
	close(errs)

	var out bytes.Buffer
	app := &App{stdout: &out, stderr: io.Discard}
	drops := make(chan monitorDrop, 4)
	drops <- monitorDrop{at: time.Now(), reason: "length byte exceeds accumulator", buffered: []byte{0x02}}

	if err := app.monitorLoop(context.Background(), frames, errs, drops); err == nil {
		t.Fatal("the terminal error was lost")
	}
	if !strings.Contains(out.String(), "length byte exceeds accumulator") {
		t.Errorf("a pending drop notice was abandoned at shutdown:\n%s", out.String())
	}
}

// A clean closure with no error -- a quiet end, not an unplug -- stays a
// normal exit 0: only a real transport failure may fail the command.
func TestMonitorLoopCleanShutdownIsNotAnError(t *testing.T) {
	frames := make(chan []byte)
	errs := make(chan error)
	close(frames)
	close(errs)

	app := &App{stdout: io.Discard, stderr: io.Discard}
	if err := app.monitorLoop(context.Background(), frames, errs, make(chan monitorDrop, 1)); err != nil {
		t.Fatalf("a clean shutdown returned %v", err)
	}
}

// TestMonitorDropHookIsWiredToTheFramer is the regression test SPEC.md §17 owes
// the drop-hook divergence.
//
// §17 records it deliberately: the vendor client discards a malformed frame in
// silence and the pending command just times out five seconds later with no
// diagnostic, and this tool reports it instead, surfaced by `gflex monitor`
// (SPEC.md §3.3). The whole divergence is one SetDropHook call, which reads
// like optional instrumentation -- delete it, or reorder it after fr.Start(),
// and every test still passed while the command silently went back to the
// vendor behaviour.
//
// So this drives a real Framer over a stream whose end-of-frame marker arrives
// with a length byte the accumulator cannot satisfy: two buffered bytes
// declaring an eight-byte frame. That is dropped by the decoder, never reaches
// Frames(), and is visible only through the hook -- so the notice arriving on
// the channel proves the whole chain, monitorDrops -> Framer.SetDropHook ->
// Decoder.SetDropHook, not just the top of it.
func TestMonitorDropHookIsWiredToTheFramer(t *testing.T) {
	// 0x08 declares eight bytes; only these two are buffered when the
	// end-of-frame marker arrives.
	malformed := framer.EncodeMIDI([]byte{0x08, 0x01})
	st := &scriptedTransport{
		reads: [][]byte{malformed},
		err:   errors.New("device unplugged (ENODEV)"),
	}
	fr := framer.New(st, time.Nanosecond)
	drops := monitorDrops(fr)
	fr.Start()
	defer func() { _ = fr.Close() }()

	select {
	case d := <-drops:
		if !strings.Contains(d.reason, "declared frame length") {
			t.Errorf("drop reason %q does not say why the frame was discarded", d.reason)
		}
		if len(d.buffered) == 0 {
			t.Error("the drop notice carries no buffered bytes, so the operator sees no evidence")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a malformed frame produced no drop notice; monitor is back to the vendor's silent discard")
	}
}

// The frames a drop notice describes never reach Frames(), which is the reason
// the hook exists at all: without it that byte stream is indistinguishable from
// a device that said nothing.
func TestMonitorDroppedFramesNeverReachTheFrameChannel(t *testing.T) {
	malformed := framer.EncodeMIDI([]byte{0x08, 0x01})
	st := &scriptedTransport{
		reads: [][]byte{malformed},
		err:   errors.New("device unplugged (ENODEV)"),
	}
	fr := framer.New(st, time.Nanosecond)
	drops := monitorDrops(fr)
	fr.Start()

	var out bytes.Buffer
	app := &App{stdout: &out, stderr: io.Discard}
	if err := app.monitorLoop(context.Background(), fr.Frames(), fr.Errors(), drops); err == nil {
		t.Fatal("the terminal transport error was swallowed")
	}
	if cerr := fr.Close(); cerr != nil {
		t.Fatalf("closing the framer: %v", cerr)
	}
	if !strings.Contains(out.String(), "drop") {
		t.Errorf("the monitor printed no drop line for a malformed frame:\n%s", out.String())
	}
}

// idleTransport reads nothing until it is closed, and reports err from Close --
// the shape of a usbfs transport whose interface was released while
// snd-usb-audio declined to rebind (SPEC.md §4.2).
type idleTransport struct {
	closed chan struct{}
	once   sync.Once
	err    error
}

func (t *idleTransport) ReadMIDI([]byte) (int, error) { <-t.closed; return 0, io.EOF }
func (t *idleTransport) WriteMIDI([]byte) error       { return nil }
func (t *idleTransport) Name() string                 { return "idle" }
func (t *idleTransport) Close() error {
	t.once.Do(func() { close(t.closed) })
	return t.err
}

// TestMonitorReportsAnUnreboundDriver covers the one close error that has to be
// printed rather than discarded, on the one command that does not close through
// conn.
//
// monitor drives the framer directly, so conn.Close -- whose whole purpose is
// this warning -- never runs for it, while `monitor --transport usb` is exactly
// what the tool tells a user to run when another MIDI client holds the rawmidi
// node (SPEC.md §4.1). Without this the monitor ended at exit 0 saying nothing,
// the ALSA node was gone until replug, and the next ordinary command reported
// no device with nothing connecting the two.
func TestMonitorReportsAnUnreboundDriver(t *testing.T) {
	tr := &idleTransport{
		closed: make(chan struct{}),
		err:    fmt.Errorf("releasing interface 1: %w", usbfs.ErrDriverNotRebound),
	}
	var out, errOut bytes.Buffer
	app := &App{stdout: &out, stderr: &errOut, ByteDelay: time.Nanosecond}
	app.testTransport = func(context.Context) (proto.Transport, string, error) {
		return tr, "usb:test", nil
	}

	if err := app.runMonitor(context.Background(), 20*time.Millisecond); err != nil {
		t.Fatalf("runMonitor: %v", err)
	}
	got := errOut.String()
	for _, want := range []string{"warning", "unplugged and plugged back in", "--transport usb"} {
		if !strings.Contains(got, want) {
			t.Errorf("the monitor's close said nothing about %q:\n%s", want, got)
		}
	}
}

// An ordinary close error stays as silent here as it does in conn.Close: a
// warning printed on every unremarkable close is one nobody reads by the time
// it matters.
func TestMonitorIsSilentAboutAnOrdinaryCloseError(t *testing.T) {
	tr := &idleTransport{closed: make(chan struct{}), err: errors.New("some other close failure")}
	var out, errOut bytes.Buffer
	app := &App{stdout: &out, stderr: &errOut, ByteDelay: time.Nanosecond}
	app.testTransport = func(context.Context) (proto.Transport, string, error) {
		return tr, "usb:test", nil
	}

	if err := app.runMonitor(context.Background(), 20*time.Millisecond); err != nil {
		t.Fatalf("runMonitor: %v", err)
	}
	if strings.Contains(errOut.String(), "warning") {
		t.Errorf("an ordinary close error was reported as the rebind failure:\n%s", errOut.String())
	}
}

// TestMonitorLoopDrainsWhatIsBufferedWhenTheContextEnds is the regression test
// for the --for deadline landing on a burst.
//
// The framer keeps decoding into its 16-slot buffer until runMonitor's deferred
// Close, and select picks uniformly among ready cases, so once the context was
// done each iteration was a coin flip between printing what had arrived and
// abandoning it -- on the command whose whole job is to be the record of what
// the device sent (SPEC.md §14.13, §14.14). The terminal error was the worse
// half: an unplug that raced the deadline left ENODEV unread in errs and the
// command exited 0 with nothing printed. The iterations cover the orderings.
func TestMonitorLoopDrainsWhatIsBufferedWhenTheContextEnds(t *testing.T) {
	frame := proto.Read(proto.CmdSerialNumber)
	const wantFrames = 3

	for i := 0; i < 40; i++ {
		frames := make(chan []byte, 16)
		for j := 0; j < wantFrames; j++ {
			frames <- frame
		}
		errs := make(chan error, 4)
		errs <- errors.New("device unplugged (ENODEV)")
		drops := make(chan monitorDrop, 4)
		drops <- monitorDrop{at: time.Now(), reason: "length byte exceeds accumulator", buffered: []byte{0x02}}

		// Neither framer channel is closed: the reader is still running, which
		// is the state a --for expiry or a Ctrl-C actually finds them in.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var out bytes.Buffer
		app := &App{stdout: &out, stderr: io.Discard}
		err := app.monitorLoop(ctx, frames, errs, drops)

		if err == nil {
			t.Fatalf("iteration %d: a transport failure waiting at the deadline exited 0", i)
		}
		if !strings.Contains(err.Error(), "ENODEV") {
			t.Fatalf("iteration %d: returned error %v does not carry the transport failure", i, err)
		}
		if got := strings.Count(out.String(), proto.Hex(frame)); got != wantFrames {
			t.Fatalf("iteration %d: %d of %d decoded frames were printed:\n%s", i, got, wantFrames, out.String())
		}
		if !strings.Contains(out.String(), "length byte exceeds accumulator") {
			t.Errorf("iteration %d: a pending drop notice was abandoned:\n%s", i, out.String())
		}
	}
}
