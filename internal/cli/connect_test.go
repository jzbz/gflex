package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jzbz/gflex/internal/transport/rawmidi"
)

// TestWarnSolePortFallback is the regression test for a flag nobody read.
//
// Discovery falls back to the only rawmidi port on the system when nothing
// identifies a VFLEX -- neither the USB vendor ID nor the "vflex" substring in
// the port name (SPEC.md §3.4). rawmidi.PortInfo.Fallback is set precisely so
// the CLI can say so, and the CLI said nothing. On a machine whose one MIDI
// device is a synthesizer, that meant opening the synth and writing protocol
// frames at it while every message above described the session as if it were a
// VFLEX.
func TestWarnSolePortFallback(t *testing.T) {
	var buf bytes.Buffer
	app := &App{stderr: &buf}
	app.warnSolePortFallback(rawmidi.PortInfo{
		Path:     "/dev/snd/midiC0D0",
		Card:     0,
		Device:   0,
		Name:     "Prophet Rev2",
		Fallback: true,
	})

	got := buf.String()
	if got == "" {
		t.Fatal("a sole-port fallback produced no warning at all")
	}
	for _, want := range []string{
		"warning",
		"/dev/snd/midiC0D0", // which port is about to be written to
		"Prophet Rev2",      // and what it calls itself
		"--port",            // how to take control of the choice
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warning does not mention %q:\n%s", want, got)
		}
	}
}

// A positively identified port is the ordinary case and must stay silent: a
// warning printed every run is a warning nobody reads.
func TestWarnSolePortFallbackSilentWhenIdentified(t *testing.T) {
	var buf bytes.Buffer
	app := &App{stderr: &buf}
	app.warnSolePortFallback(rawmidi.PortInfo{
		Path:     "/dev/snd/midiC2D0",
		Name:     "VFLEX MIDI 1",
		VendorID: 0x37BF,
		IsVFlex:  true,
	})
	if got := buf.String(); got != "" {
		t.Errorf("an identified port must not warn, got:\n%s", got)
	}
}

// The warning is written before any command runs, so a nil stderr -- an App
// built by a test that only exercises pure helpers -- must not panic the
// process on the way to the device.
func TestWarnSolePortFallbackToleratesNilStderr(t *testing.T) {
	app := &App{}
	app.warnSolePortFallback(rawmidi.PortInfo{Path: "/dev/snd/midiC0D0", Fallback: true})
}
