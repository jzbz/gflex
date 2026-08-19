package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/framer"
	"github.com/jzbz/gflex/internal/proto"
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
