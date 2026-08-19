package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jzbz/gflex/internal/transport/rawmidi"
	"github.com/jzbz/gflex/internal/usbfs"
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

// TestSelectUSBRefRefusesAmbiguousPort is the regression test for openUSB
// taking the FIRST device matchesUSBPort said yes to.
//
// matchesUSBPort ends in a suffix match and accepts a bare address, so with
// two VFLEX units attached an imprecise --port can designate both -- here "7"
// matches addr 7 on two different buses. The old loop opened whichever
// usbfs.Enumerate sorted first and wrote the voltage to that unit's rail,
// silently; the pick had nothing to do with which unit the user meant.
// rawmidi.Select refuses its ambiguous case, and this must too, naming every
// candidate so the user can pass a full device path instead.
func TestSelectUSBRefRefusesAmbiguousPort(t *testing.T) {
	refs := []usbfs.DeviceRef{
		{Path: "/dev/bus/usb/001/007", Bus: 1, Addr: 7, VendorID: 0x37BF, ProductID: 0x0001},
		{Path: "/dev/bus/usb/003/007", Bus: 3, Addr: 7, VendorID: 0x37BF, ProductID: 0x0001},
	}
	_, ok, err := selectUSBRef(refs, "7")
	if err == nil {
		t.Fatalf("--port \"7\" matched two devices and was not refused (ok=%v)", ok)
	}
	if ok {
		t.Error("ok=true alongside an ambiguity error; the caller would open a device anyway")
	}
	for _, want := range []string{"/dev/bus/usb/001/007", "/dev/bus/usb/003/007", "--port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error does not mention %q:\n%v", want, err)
		}
	}
	if code := ExitCode(err); code != ExitNoDevice {
		t.Errorf("ExitCode = %d, want ExitNoDevice (%d): rawmidi ambiguity classifies there too", code, ExitNoDevice)
	}
}

// A --port precise enough to designate one unit keeps working: the unique
// match is returned for opening.
func TestSelectUSBRefUniqueMatchProceeds(t *testing.T) {
	refs := []usbfs.DeviceRef{
		{Path: "/dev/bus/usb/001/007", Bus: 1, Addr: 7},
		{Path: "/dev/bus/usb/003/012", Bus: 3, Addr: 12},
	}
	ref, ok, err := selectUSBRef(refs, "001:007")
	if err != nil || !ok {
		t.Fatalf("a unique match must proceed, got ok=%v err=%v", ok, err)
	}
	if ref.Path != "/dev/bus/usb/001/007" {
		t.Errorf("selected %s, want /dev/bus/usb/001/007", ref.Path)
	}
}

// No match is not an error here: openUSB owns the not-found report, with its
// searched/found/fixes body.
func TestSelectUSBRefNoMatch(t *testing.T) {
	refs := []usbfs.DeviceRef{{Path: "/dev/bus/usb/001/007", Bus: 1, Addr: 7}}
	_, ok, err := selectUSBRef(refs, "/dev/bus/usb/002/004")
	if err != nil {
		t.Fatalf("no match must not be an ambiguity error: %v", err)
	}
	if ok {
		t.Error("ok=true for a port matching nothing")
	}
}
