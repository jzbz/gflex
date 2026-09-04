package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

// TestPortPathThatIsNotADeviceNodeIsRefused is the regression test for what
// --port does not mean.
//
// It overrides IDENTIFICATION -- the vendor ID, the name match, the sole-port
// fallback (SPEC.md §3.4) -- and nothing else asked whether the path was a
// device node at all. rawmidi.Open was a plain O_RDWR open, so a stale
// GFLEX_PORT or a shell completion that landed beside the node opened a regular
// file and the framer's first frame went into it, overwriting the beginning of
// somebody's file with 80 00 00 90 ... and then failing with EOF.
//
// Two checks refuse it now, one on the name and one on the descriptor, and both
// subtests below assert the same three things: exit 2, the one refusal wording,
// and -- the assertion that matters -- the file back byte for byte.
func TestPortPathThatIsNotADeviceNodeIsRefused(t *testing.T) {
	const content = "a file of somebody's, and not a MIDI port at all.\n"
	newFile := func(t *testing.T) (dir, path string) {
		t.Helper()
		dir = t.TempDir()
		path = filepath.Join(dir, "notes.txt")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir, path
	}
	refused := func(t *testing.T, path string, err error) {
		t.Helper()
		if code := ExitCode(err); code != ExitUsage {
			t.Errorf("ExitCode = %d, want ExitUsage (%d): %v", code, ExitUsage, err)
		}
		for _, want := range []string{path, "character device", "/dev/snd/"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not mention %q:\n%v", want, err)
			}
		}
		after, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if string(after) != content {
			t.Errorf("the file was written to:\n%q", after)
		}
	}

	t.Run("the stat on the name", func(t *testing.T) {
		_, path := newFile(t)
		app := &App{Port: path, stdout: io.Discard, stderr: io.Discard}
		tr, _, err := app.openRawMIDI(context.Background())
		if err == nil {
			_ = tr.Close()
			t.Fatal("openRawMIDI opened a regular file named by --port")
		}
		refused(t, path, err)
	})

	// The stat is positive-only on purpose -- a path it cannot answer for falls
	// through, so ENOENT stays a missing device -- and a name is not the object
	// anyway: it can be replaced between the stat and the open. What catches
	// that is rawmidi.Open's fstat on the descriptor it is already holding, and
	// the CLI has to render it as the same refusal, not as "no device found".
	//
	// A directory the process cannot search is the one shape of that window
	// which can be staged from inside this process: the stat fails EACCES and
	// says nothing, the open behind it is denied too, and openWaitingForACL
	// retries a denial for rawmidiACLGrace (TestOpenWaitsOutTheUdevACL), so
	// restoring the mode partway through lets the open land on the file the
	// stat never saw. The elapsed-time check is what proves it got there: the
	// stat guard would have answered in microseconds.
	t.Run("the transport's fstat on the descriptor", func(t *testing.T) {
		dir, path := newFile(t)
		if err := os.Chmod(dir, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })
		go func() {
			time.Sleep(2 * rawmidiACLPoll)
			os.Chmod(dir, 0o700)
		}()

		app := &App{Port: path, stdout: io.Discard, stderr: io.Discard}
		start := time.Now()
		tr, _, err := app.openRawMIDI(context.Background())
		if err == nil {
			_ = tr.Close()
			t.Fatal("openRawMIDI opened a regular file the stat could not see")
		}
		if elapsed := time.Since(start); elapsed < 2*rawmidiACLPoll {
			t.Fatalf("refused after %s, before the mode was restored: the stat answered and "+
				"the transport's check was never reached", elapsed)
		}
		refused(t, path, err)
	})
}

// A path that does not exist must still fall through to the transport, so the
// open error keeps classifying itself -- ENOENT is a missing device, not a
// malformed command line.
func TestPortPathThatDoesNotExistIsLeftToTheTransport(t *testing.T) {
	app := &App{Port: filepath.Join(t.TempDir(), "midiC9D9"), stdout: io.Discard, stderr: io.Discard}
	_, _, err := app.openRawMIDI(context.Background())
	if err == nil {
		t.Fatal("openRawMIDI succeeded on a path that does not exist")
	}
	if code := ExitCode(err); code == ExitUsage {
		t.Errorf("a missing node was reported as a command-line error: %v", err)
	}
}

// EBUSY means something different on each transport, and the generic ExitBusy
// guidance is written for only one of them: it explains ALSA's per-direction
// exclusivity, points at /proc/asound/seq/clients, and ends by recommending
// --transport usb. Under --transport usb the errno comes from
// USBDEVFS_DISCONNECT_CLAIM with another usbfs process in the way, and that
// advice recommends the transport the run is already on.
func TestABusyUSBInterfaceDoesNotGetTheALSAAdvice(t *testing.T) {
	busy := fmt.Errorf("claiming interface 1: %w", syscall.EBUSY)

	usb := (&App{Transport: transportUSB}).transportError(context.Background(), busy)
	if code := ExitCode(usb); code != ExitBusy {
		t.Errorf("ExitCode = %d, want ExitBusy (%d): %v", code, ExitBusy, usb)
	}
	if !suppressHint(usb) {
		t.Errorf("the ALSA hint is still printed for a usbfs claim:\n%v", usb)
	}
	if !strings.Contains(usb.Error(), "USB interface") {
		t.Errorf("the message does not say what is held:\n%v", usb)
	}

	// The rawmidi side keeps the hint, because that is where the advice applies.
	alsa := (&App{Transport: transportRawMIDI}).transportError(context.Background(), busy)
	if code := ExitCode(alsa); code != ExitBusy {
		t.Errorf("ExitCode = %d, want ExitBusy (%d): %v", code, ExitBusy, alsa)
	}
	if suppressHint(alsa) {
		t.Errorf("the rawmidi busy case lost its guidance:\n%v", alsa)
	}
}

// TestOpenWaitsOutTheUdevACL covers the window between a node appearing and
// being usable.
//
// devtmpfs creates /dev/snd/midiC*D* when the device registers and udev applies
// the uaccess ACL a moment later, so anything that opens on the first sight of
// the node races it -- `scan --no-prompt` polls presence every 250 ms and
// reconnects immediately, with the capture log already erased (SPEC.md §9.2).
func TestOpenWaitsOutTheUdevACL(t *testing.T) {
	denied := &fs.PathError{Op: "open", Path: "/dev/snd/midiC1D0", Err: syscall.EACCES}
	calls := 0
	tr, err := openWaitingForACL(context.Background(), func() (proto.Transport, error) {
		calls++
		if calls < 3 {
			return nil, denied
		}
		return closeErrTransport{}, nil
	})
	if err != nil {
		t.Fatalf("openWaitingForACL gave up on a permission denial that cleared: %v", err)
	}
	if tr == nil {
		t.Fatal("openWaitingForACL returned no transport and no error")
	}
	if calls != 3 {
		t.Errorf("open was called %d times, want 3", calls)
	}
}

// The other half, and the reason the retry is not general: a missing node is
// the ordinary no-device case, by far the commonest failure this tool has, and
// making every one of those wait out the grace would be the wrong trade.
func TestOpenDoesNotWaitOutAMissingNode(t *testing.T) {
	calls := 0
	start := time.Now()
	_, err := openWaitingForACL(context.Background(), func() (proto.Transport, error) {
		calls++
		return nil, &fs.PathError{Op: "open", Path: "/dev/snd/midiC1D0", Err: syscall.ENOENT}
	})
	if !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("openWaitingForACL returned %v, want the ENOENT unchanged", err)
	}
	if calls != 1 {
		t.Errorf("a missing node was retried (%d opens); every no-device run would pay the grace", calls)
	}
	if elapsed := time.Since(start); elapsed > rawmidiACLGrace {
		t.Errorf("a missing node took %s to report", elapsed)
	}
}
