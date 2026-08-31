package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/session"
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

// A name-only identification is a weaker claim than a vendor-ID one, and until
// classify stopped OR-ing the two together they were indistinguishable at the
// point of use. The warning is what makes the tier visible; without it a port
// that merely spells "vflex" is opened exactly as silently as a confirmed unit.
func TestWarnNameOnlyMatch(t *testing.T) {
	var buf bytes.Buffer
	app := &App{stderr: &buf}
	app.warnNameOnlyMatch(rawmidi.PortInfo{
		Path:    "/dev/snd/midiC1D0",
		Card:    1,
		Device:  0,
		Name:    "vflex clone",
		IsVFlex: true,
		// No VendorID: discovery never traced this port to a USB device, so
		// the name substring is the only thing that identified it.
	})

	got := buf.String()
	if got == "" {
		t.Fatal("a name-only identification produced no warning at all")
	}
	for _, want := range []string{
		"warning",
		"/dev/snd/midiC1D0",
		"vflex clone",
		"name alone",
		"--port",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warning does not mention %q:\n%s", want, got)
		}
	}
}

// The vendor ID is the strong identification, and it is the overwhelmingly
// common case: warning on it would train the user to ignore the message that
// matters. A sole-port fallback stays silent here too, because
// warnSolePortFallback already says strictly more about it.
func TestWarnNameOnlyMatchSilentWhenConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name string
		port rawmidi.PortInfo
	}{
		{"vendor id confirmed it", rawmidi.PortInfo{
			Path: "/dev/snd/midiC1D0", Name: "Werewolf VFLEX", IsVFlex: true, VendorID: proto.VendorID,
		}},
		{"sole-port fallback is another function's message", rawmidi.PortInfo{
			Path: "/dev/snd/midiC1D0", Name: "Prophet Rev2", Fallback: true,
		}},
		{"not a VFLEX at all", rawmidi.PortInfo{
			Path: "/dev/snd/midiC1D0", Name: "Prophet Rev2",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			app := &App{stderr: &buf}
			app.warnNameOnlyMatch(tc.port)
			if got := buf.String(); got != "" {
				t.Errorf("expected silence, got:\n%s", got)
			}
		})
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

// TestMatchesUSBPortAnchorsItsSuffixOnASeparator is the regression test for
// --port picking a unit the user did not name.
//
// The suffix arm existed so half a path works, but it was unanchored: "3" is a
// suffix of /dev/bus/usb/011/013, so it designated address 13. Where the unit
// the user meant is also attached, selectUSBRef sees two matches and refuses;
// where it is not -- the script pinned --port 3, that unit re-enumerated or was
// left unplugged -- the stray match is the only one, so it was selected without
// a word and the voltage went to the other unit's rail.
func TestMatchesUSBPortAnchorsItsSuffixOnASeparator(t *testing.T) {
	ref := usbfs.DeviceRef{Path: "/dev/bus/usb/011/013", SysPath: "/sys/bus/usb/devices/11-2", Bus: 11, Addr: 13}

	for _, port := range []string{"/dev/bus/usb/011/013", "/sys/bus/usb/devices/11-2",
		"11:13", "011:013", "13", "013", "011/013", "usb/011/013"} {
		if !matchesUSBPort(ref, port) {
			t.Errorf("--port %q no longer designates %s; every form gflex devices prints must keep working", port, ref.Path)
		}
	}
	for _, port := range []string{"3", "1/013", "13/013", "0"} {
		if matchesUSBPort(ref, port) {
			t.Errorf("--port %q matched %s on a partial path component", port, ref.Path)
		}
	}
}

// The consequence, at the layer that opens the device: a --port that names no
// attached unit must find nothing, not fall through onto whichever path happens
// to end with those characters. openUSB owns the not-found report, so no match
// is ok=false with no error.
func TestSelectUSBRefDoesNotFallThroughToAnUnnamedUnit(t *testing.T) {
	refs := []usbfs.DeviceRef{
		{Path: "/dev/bus/usb/001/004", Bus: 1, Addr: 4},
		{Path: "/dev/bus/usb/011/013", Bus: 11, Addr: 13},
	}
	ref, ok, err := selectUSBRef(refs, "3")
	if err != nil {
		t.Fatalf("selectUSBRef: %v", err)
	}
	if ok {
		t.Errorf("--port 3 selected %s; no attached unit is at address 3", ref.Path)
	}
}

// closeErrTransport is a transport whose only interesting behaviour is the
// error it returns from Close.
type closeErrTransport struct{ err error }

func (closeErrTransport) WriteMIDI([]byte) error         { return nil }
func (closeErrTransport) ReadMIDI(p []byte) (int, error) { return 0, nil }
func (closeErrTransport) Name() string                   { return "test" }
func (t closeErrTransport) Close() error                 { return t.err }

// The rebind diagnostic has to reach stderr from Close itself, because every
// command closes with `defer c.Close()` and discards the error -- so a fix that
// only returns it is a fix nobody ever sees. This is the test that would fail if
// the reporting in conn.Close were deleted while the sentinel kept propagating.
func TestConnCloseReportsAnUnreboundDriver(t *testing.T) {
	var buf bytes.Buffer
	tr := closeErrTransport{err: fmt.Errorf("releasing interface 1: %w", usbfs.ErrDriverNotRebound)}
	c := &conn{
		Session: session.New(tr, session.Options{}),
		Desc:    "test",
		stderr:  &buf,
	}
	if err := c.Close(); !errors.Is(err, usbfs.ErrDriverNotRebound) {
		t.Fatalf("Close() = %v, want it to wrap usbfs.ErrDriverNotRebound", err)
	}
	got := buf.String()
	if got == "" {
		t.Fatal("Close() reported nothing; a discarded error is a diagnostic the user never sees")
	}
	for _, want := range []string{"warning", "unplug", "--transport usb"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not mention %q:\n%s", want, got)
		}
	}
}

// An ordinary close error must stay silent: a close failure is not a command
// failure, and a warning printed on every unremarkable close is one nobody
// reads by the time it matters.
func TestConnCloseIsSilentForOrdinaryErrors(t *testing.T) {
	var buf bytes.Buffer
	c := &conn{
		Session: session.New(closeErrTransport{err: errors.New("some other close failure")}, session.Options{}),
		Desc:    "test",
		stderr:  &buf,
	}
	_ = c.Close()
	if got := buf.String(); got != "" {
		t.Errorf("expected silence for an ordinary close error, got:\n%s", got)
	}
}
