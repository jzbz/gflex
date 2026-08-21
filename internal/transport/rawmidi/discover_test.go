package rawmidi

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// Fixture construction. Everything below runs against a fake /sys, /proc and
// /dev tree under t.TempDir(); no real device is ever opened.
// ---------------------------------------------------------------------------

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(link), err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

// fixture builds a small but realistic view of the kernel's filesystems:
//
//	card0  non-VFLEX USB audio device, sound/ two levels down
//	card1  a rawmidi port with no USB parent, named "...vFlex..." in
//	       /proc/asound/cards -- the vendor app's name-substring case
//	card2  VFLEX, reached through the /sys/bus/usb/devices symlink, sound/ under
//	       the interface directory, named by /proc/asound/card2/midi0
//	card3  VFLEX, sound/ directly under the device directory, named by the ALSA
//	       card id in /sys/class/sound
func fixture(t *testing.T) *scanner {
	t.Helper()
	root := t.TempDir()
	s := &scanner{
		sysUSB:     filepath.Join(root, "sys", "bus", "usb", "devices"),
		sysSound:   filepath.Join(root, "sys", "class", "sound"),
		procAsound: filepath.Join(root, "proc", "asound"),
		devSnd:     filepath.Join(root, "dev", "snd"),
	}
	devices := filepath.Join(root, "sys", "devices")

	// --- card2: VFLEX, real sysfs shape (bus entry is a symlink into /sys/devices)
	vflexDev := filepath.Join(devices, "pci0000:00", "usb5", "5-1.3")
	writeFile(t, filepath.Join(vflexDev, "idVendor"), "37bf\n")
	writeFile(t, filepath.Join(vflexDev, "idProduct"), "800f\n")
	iface := filepath.Join(vflexDev, "5-1.3:1.0")
	mkdirAll(t, filepath.Join(iface, "sound", "card2", "midiC2D0"))
	symlink(t, vflexDev, filepath.Join(s.sysUSB, "5-1.3"))
	// The interface also appears at the top level of the bus directory and must
	// be skipped: it owns no idVendor.
	symlink(t, iface, filepath.Join(s.sysUSB, "5-1.3:1.0"))
	symlink(t, filepath.Join(iface, "sound", "card2"), filepath.Join(s.sysSound, "card2"))
	writeFile(t, filepath.Join(s.procAsound, "card2", "midi0"),
		"VFLEX MIDI 1\n\nOutput\n  Tx bytes     : 0\n")

	// --- card3: VFLEX with the sound directory directly under the device
	vflex2 := filepath.Join(s.sysUSB, "6-2")
	writeFile(t, filepath.Join(vflex2, "idVendor"), "37BF\n") // uppercase tolerated
	writeFile(t, filepath.Join(vflex2, "idProduct"), "0001\n")
	mkdirAll(t, filepath.Join(vflex2, "sound", "card3", "midiC3D0"))
	// Named through /sys/class/sound/midiC3D0/device/id.
	writeFile(t, filepath.Join(vflex2, "sound", "card3", "id"), "VFX3\n")
	symlink(t, filepath.Join(vflex2, "sound", "card3"),
		filepath.Join(s.sysSound, "midiC3D0", "device"))

	// --- card0: an ordinary USB audio interface, not a VFLEX
	otherDev := filepath.Join(devices, "pci0000:00", "usb3", "3-4")
	writeFile(t, filepath.Join(otherDev, "idVendor"), "0b05\n")
	writeFile(t, filepath.Join(otherDev, "idProduct"), "1a30\n")
	otherIface := filepath.Join(otherDev, "3-4:1.2")
	mkdirAll(t, filepath.Join(otherIface, "sound", "card0", "midiC0D0"))
	symlink(t, otherDev, filepath.Join(s.sysUSB, "3-4"))
	symlink(t, filepath.Join(otherIface, "sound", "card0"), filepath.Join(s.sysSound, "card0"))

	// --- /proc/asound/cards, the last-resort name source
	writeFile(t, filepath.Join(s.procAsound, "cards"),
		" 0 [Generic        ]: HDA-Intel - HD-Audio Generic\n"+
			"                      HD-Audio Generic at 0xa0448000 irq 166\n"+
			" 1 [Loopback       ]: USB-Audio - vFlex Audio Widget\n"+
			" 2 [VFLEX          ]: USB-Audio - VFLEX\n"+
			" 3 [VFX3           ]: USB-Audio - VFLEX\n")

	// --- /dev/snd, including nodes that must be ignored
	for _, n := range []string{"midiC0D0", "midiC1D0", "midiC2D0", "midiC3D0",
		"controlC0", "pcmC0D0p", "seq", "timer", "midiCxDy"} {
		writeFile(t, filepath.Join(s.devSnd, n), "")
	}
	return s
}

func TestDiscover(t *testing.T) {
	s := fixture(t)
	ports, err := s.discover()
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(ports) != 4 {
		t.Fatalf("got %d ports, want 4: %v", len(ports), ports)
	}

	want := []PortInfo{
		{Path: filepath.Join(s.devSnd, "midiC0D0"), Card: 0, Device: 0,
			Name: "HDA-Intel - HD-Audio Generic", VendorID: 0x0b05, ProductID: 0x1a30, IsVFlex: false},
		{Path: filepath.Join(s.devSnd, "midiC1D0"), Card: 1, Device: 0,
			Name: "USB-Audio - vFlex Audio Widget", IsVFlex: true},
		{Path: filepath.Join(s.devSnd, "midiC2D0"), Card: 2, Device: 0,
			Name: "VFLEX MIDI 1", VendorID: 0x37bf, ProductID: 0x800f, IsVFlex: true},
		{Path: filepath.Join(s.devSnd, "midiC3D0"), Card: 3, Device: 0,
			Name: "VFX3", VendorID: 0x37bf, ProductID: 0x0001, IsVFlex: true},
	}
	for i, w := range want {
		g := ports[i]
		if g.Path != w.Path || g.Card != w.Card || g.Device != w.Device {
			t.Errorf("port %d: got %s card %d dev %d, want %s card %d dev %d",
				i, g.Path, g.Card, g.Device, w.Path, w.Card, w.Device)
		}
		if g.Name != w.Name {
			t.Errorf("port %d name: got %q, want %q", i, g.Name, w.Name)
		}
		if g.VendorID != w.VendorID || g.ProductID != w.ProductID {
			t.Errorf("port %d ids: got %04x:%04x, want %04x:%04x",
				i, g.VendorID, g.ProductID, w.VendorID, w.ProductID)
		}
		if g.IsVFlex != w.IsVFlex {
			t.Errorf("port %d IsVFlex: got %v, want %v", i, g.IsVFlex, w.IsVFlex)
		}
	}
	// The VID-anchored pass must record where it found the unit, since the
	// product ID it reports is the answer to SPEC.md §14.1.
	if ports[2].SysPath != filepath.Join(s.sysUSB, "5-1.3") {
		t.Errorf("SysPath: got %q", ports[2].SysPath)
	}
}

func TestDiscoverIgnoresMissingRoots(t *testing.T) {
	s := &scanner{
		sysUSB:     filepath.Join(t.TempDir(), "absent-sys"),
		sysSound:   filepath.Join(t.TempDir(), "absent-class"),
		procAsound: filepath.Join(t.TempDir(), "absent-proc"),
		devSnd:     filepath.Join(t.TempDir(), "absent-dev"),
	}
	ports, err := s.discover()
	if err != nil {
		t.Fatalf("missing roots must not be an error, got %v", err)
	}
	if len(ports) != 0 {
		t.Fatalf("got %d ports, want 0", len(ports))
	}
}

// A VFLEX whose node is listed in sysfs but whose /dev/snd entry is missing is
// still reported, so the CLI can explain what happened rather than say "no
// device".
func TestDiscoverSysfsOnlyPortStillListed(t *testing.T) {
	s := fixture(t)
	if err := os.Remove(filepath.Join(s.devSnd, "midiC2D0")); err != nil {
		t.Fatal(err)
	}
	ports, err := s.discover()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range ports {
		if p.Card == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("card 2 dropped when /dev/snd node was absent: %v", ports)
	}
}

func TestPortNamePrefersRawmidiName(t *testing.T) {
	s := fixture(t)
	if got := s.portName(2, 0); got != "VFLEX MIDI 1" {
		t.Errorf("card2: got %q, want the /proc/asound/card2/midi0 name", got)
	}
	// No /proc/asound/card3/midi0: falls through to the sysfs ALSA card id.
	if got := s.portName(3, 0); got != "VFX3" {
		t.Errorf("card3: got %q, want %q", got, "VFX3")
	}
	// Neither source: falls through to /proc/asound/cards.
	if got := s.portName(1, 0); got != "USB-Audio - vFlex Audio Widget" {
		t.Errorf("card1: got %q", got)
	}
	if got := s.portName(9, 0); got != "" {
		t.Errorf("unknown card: got %q, want empty", got)
	}
}

// The kernel builds these names from the device's own USB string descriptors,
// so a hostile unit chooses them; they get the same printable-ASCII treatment
// as every other device-supplied string (SPEC.md §17, "identity strings
// ... sanitised"). Each of portName's three sources is checked, because the
// filter has to sit on all of them or the device just picks another one.
func TestPortNameStripsControlBytes(t *testing.T) {
	s := fixture(t)
	writeFile(t, filepath.Join(s.procAsound, "card2", "midi0"),
		"VFLEX\x1b[2J MIDI 1\n\nOutput\n")
	writeFile(t, filepath.Join(s.sysSound, "midiC3D0", "device", "id"), "V\x07FX3\n")
	writeFile(t, filepath.Join(s.procAsound, "cards"),
		" 1 [Loopback       ]: USB-Audio - vFlex\x1b]0;x\x07 Widget\n")

	for _, c := range []struct {
		card int
		want string
	}{
		// Only the ESC and BEL bytes go; the rest of a sequence survives as the
		// literal text it is, which is harmless and keeps the name recognisable.
		{2, "VFLEX[2J MIDI 1"},
		{3, "VFX3"},
		{1, "USB-Audio - vFlex]0;x Widget"},
	} {
		got := s.portName(c.card, 0)
		if got != c.want {
			t.Errorf("portName(%d, 0) = %q, want %q", c.card, got, c.want)
		}
	}
}

// A name that is nothing but control bytes must not shadow a usable name from
// the next source down: the filter emptying a string is the same situation as
// the file being absent.
func TestPortNameFallsThroughWhenNothingPrintableSurvives(t *testing.T) {
	s := fixture(t)
	writeFile(t, filepath.Join(s.procAsound, "card2", "midi0"), "\x1b\x07\x00\n")
	if got := s.portName(2, 0); got != "USB-Audio - VFLEX" {
		t.Errorf("portName(2, 0) = %q, want the /proc/asound/cards name", got)
	}
}

func TestUSBIDsForCard(t *testing.T) {
	s := fixture(t)
	vid, pid, sysPath, ok := s.usbIDsForCard(0)
	if !ok {
		t.Fatal("expected to walk up from card0 to its USB parent")
	}
	if vid != 0x0b05 || pid != 0x1a30 {
		t.Errorf("got %04x:%04x, want 0b05:1a30", vid, pid)
	}
	if !strings.HasSuffix(sysPath, "3-4") {
		t.Errorf("sysPath %q does not name the USB device directory", sysPath)
	}
	if _, _, _, ok := s.usbIDsForCard(1); ok {
		t.Error("card1 has no USB parent; expected no ids")
	}
}

func TestParseMidiNode(t *testing.T) {
	cases := []struct {
		in        string
		card, dev int
		wantOK    bool
	}{
		{"midiC1D0", 1, 0, true},
		{"midiC0D0", 0, 0, true},
		{"midiC12D3", 12, 3, true},
		{"midiCxDy", 0, 0, false},
		{"midiC1", 0, 0, false},
		{"midiCD0", 0, 0, false},
		{"midiC1D", 0, 0, false},
		{"midiC-1D0", 0, 0, false},
		{"controlC1", 0, 0, false},
		{"pcmC0D3p", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		card, dev, ok := parseMidiNode(c.in)
		if ok != c.wantOK || (ok && (card != c.card || dev != c.dev)) {
			t.Errorf("parseMidiNode(%q) = %d,%d,%v; want %d,%d,%v",
				c.in, card, dev, ok, c.card, c.dev, c.wantOK)
		}
	}
}

func TestParseProcAsoundCards(t *testing.T) {
	in := " 0 [Generic        ]: HDA-Intel - HD-Audio Generic\n" +
		"                      HD-Audio Generic at 0xa0448000 irq 166\n" +
		" 1 [VFLEX          ]:\n" +
		"--- no soundcards ---\n"
	got := parseProcAsoundCards([]byte(in))
	if got[0] != "HDA-Intel - HD-Audio Generic" {
		t.Errorf("card 0: %q", got[0])
	}
	// Empty description falls back to the bracketed ALSA id.
	if got[1] != "VFLEX" {
		t.Errorf("card 1: %q", got[1])
	}
	if len(got) != 2 {
		t.Errorf("got %d cards, want 2: %v", len(got), got)
	}
}

func TestReadHexID(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ok"), "37bf\n")
	writeFile(t, filepath.Join(dir, "upper"), "37BF")
	writeFile(t, filepath.Join(dir, "junk"), "not-hex\n")
	writeFile(t, filepath.Join(dir, "wide"), "137bf\n")

	if v, ok := readHexID(filepath.Join(dir, "ok")); !ok || v != 0x37bf {
		t.Errorf("ok: %04x %v", v, ok)
	}
	if v, ok := readHexID(filepath.Join(dir, "upper")); !ok || v != 0x37bf {
		t.Errorf("upper: %04x %v", v, ok)
	}
	if _, ok := readHexID(filepath.Join(dir, "junk")); ok {
		t.Error("junk parsed")
	}
	if _, ok := readHexID(filepath.Join(dir, "wide")); ok {
		t.Error("value wider than 16 bits parsed")
	}
	if _, ok := readHexID(filepath.Join(dir, "absent")); ok {
		t.Error("missing file parsed")
	}
}

// ---------------------------------------------------------------------------
// Selection policy
// ---------------------------------------------------------------------------

func mkPort(path, name string, vflex bool) PortInfo {
	card, dev, _ := parseMidiNode(filepath.Base(path))
	return PortInfo{Path: path, Card: card, Device: dev, Name: name, IsVFlex: vflex}
}

func TestSelect(t *testing.T) {
	vflex := mkPort("/dev/snd/midiC2D0", "VFLEX MIDI 1", true)
	other := mkPort("/dev/snd/midiC0D0", "HD-Audio Generic", false)
	second := mkPort("/dev/snd/midiC3D0", "VFLEX MIDI 1", true)

	t.Run("single vflex among many", func(t *testing.T) {
		got, err := Select([]PortInfo{other, vflex}, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Path != vflex.Path || got.Fallback {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("two vflex ports are ambiguous", func(t *testing.T) {
		_, err := Select([]PortInfo{vflex, second}, "")
		if !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("got %v, want ErrAmbiguous", err)
		}
		// The message has to name the candidates or --port is unusable.
		if !strings.Contains(err.Error(), vflex.Path) || !strings.Contains(err.Error(), second.Path) {
			t.Errorf("error does not list candidates: %v", err)
		}
	})

	// mkPort leaves VendorID 0: the sysfs walk found no USB parent, so the tool
	// genuinely does not know what this port is. That is the only case the
	// fallback is for.
	t.Run("sole port fallback is flagged", func(t *testing.T) {
		got, err := Select([]PortInfo{other}, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Path != other.Path {
			t.Fatalf("got %s", got.Path)
		}
		if !got.Fallback {
			t.Error("Fallback must be set so the CLI can warn the port is unidentified")
		}
	})

	// A port sysfs has traced to another vendor is not unknown, though, and the
	// fallback must not take it: doing so writes VFLEX protocol frames at
	// somebody's keyboard. --port is the way to insist.
	t.Run("sole port of a different vendor is refused", func(t *testing.T) {
		foreign := PortInfo{Path: "/dev/snd/midiC0D0", Card: 0, Device: 0,
			Name: "Keystation 49", VendorID: 0x0763, ProductID: 0x0192}
		foreign.classify()
		_, err := Select([]PortInfo{foreign}, "")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
		for _, want := range []string{"0x0763", foreign.Path, "--port"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not mention %q: %v", want, err)
			}
		}
	})

	// The vendor ID is authoritative and a name is not, so the confirmed port
	// wins outright rather than the pair being called ambiguous (SPEC.md §3.4).
	t.Run("vendor id outranks a name-only match", func(t *testing.T) {
		confirmed := PortInfo{Path: "/dev/snd/midiC2D0", Card: 2, Device: 0,
			Name: "Werewolf VFLEX", VendorID: 0x37bf, ProductID: 0x800f}
		nameOnly := PortInfo{Path: "/dev/snd/midiC1D0", Card: 1, Device: 0,
			Name: "vFlex Audio Widget"}
		confirmed.classify()
		nameOnly.classify()
		if !nameOnly.IsVFlex {
			t.Fatal("a port with no USB parent must still be identified by name")
		}
		got, err := Select([]PortInfo{nameOnly, confirmed}, "")
		if err != nil {
			t.Fatalf("got %v, want the VID-confirmed port", err)
		}
		if got.Path != confirmed.Path {
			t.Errorf("got %s, want %s", got.Path, confirmed.Path)
		}
	})

	t.Run("several unidentified ports refuse to guess", func(t *testing.T) {
		_, err := Select([]PortInfo{other, mkPort("/dev/snd/midiC1D0", "Keystation", false)}, "")
		if !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("got %v, want ErrAmbiguous", err)
		}
	})

	t.Run("no ports", func(t *testing.T) {
		_, err := Select(nil, "")
		if !errors.Is(err, ErrNoPorts) {
			t.Fatalf("got %v, want ErrNoPorts", err)
		}
	})

	t.Run("path hint known", func(t *testing.T) {
		got, err := Select([]PortInfo{other, vflex}, "/dev/snd/midiC2D0")
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "VFLEX MIDI 1" {
			t.Errorf("known path should keep its metadata, got %+v", got)
		}
	})

	t.Run("path hint unknown is trusted", func(t *testing.T) {
		got, err := Select([]PortInfo{other}, "/dev/snd/midiC7D1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Path != "/dev/snd/midiC7D1" || got.Card != 7 || got.Device != 1 {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("path hint that is not a rawmidi node", func(t *testing.T) {
		got, err := Select(nil, "/dev/snd/by-path/pci-0000:00-usb")
		if err != nil {
			t.Fatal(err)
		}
		if got.Card != -1 || got.Device != -1 {
			t.Errorf("unknown card/device must be -1, got %+v", got)
		}
	})

	t.Run("name substring hint", func(t *testing.T) {
		got, err := Select([]PortInfo{other, vflex}, "hd-audio")
		if err != nil {
			t.Fatal(err)
		}
		if got.Path != other.Path {
			t.Errorf("got %s", got.Path)
		}
	})

	t.Run("node name hint", func(t *testing.T) {
		got, err := Select([]PortInfo{other, vflex}, "midiC2D0")
		if err != nil {
			t.Fatal(err)
		}
		if got.Path != vflex.Path {
			t.Errorf("got %s", got.Path)
		}
	})

	t.Run("hint matching nothing", func(t *testing.T) {
		_, err := Select([]PortInfo{other}, "nope")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("hint matching several", func(t *testing.T) {
		_, err := Select([]PortInfo{vflex, second}, "vflex")
		if !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("got %v, want ErrAmbiguous", err)
		}
	})
}

// The name match reproduces the vendor app's plain, unanchored, case-insensitive
// substring test (SPEC.md §3.4), but only for a port with no USB parent: a known
// vendor ID decides the question by itself, in both directions.
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		vid  uint16
		want bool
	}{
		{"VFLEX MIDI 1", 0, true},
		{"my vflex thing", 0, true},
		{"Werewolf VFlex", 0, true},
		{"Keystation 49", 0, false},
		{"", 0x37bf, true},
		{"Keystation 49", 0x37bf, true},
		{"", 0, false},
		// Sysfs says this is an M-Audio device. Whatever it calls itself, it is
		// not a VFLEX, and the name must not be able to say otherwise.
		{"vFlex Audio Widget", 0x0763, false},
	}
	for _, c := range cases {
		p := PortInfo{Name: c.name, VendorID: c.vid}
		p.classify()
		if p.IsVFlex != c.want {
			t.Errorf("classify(%q, %04x) = %v, want %v", c.name, c.vid, p.IsVFlex, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Error classification
// ---------------------------------------------------------------------------

func TestClassifyOpenError(t *testing.T) {
	const path = "/dev/snd/midiC2D0"
	cases := []struct {
		errno    error
		sentinel error
		mentions []string
	}{
		{unix.EBUSY, ErrBusy, []string{"PipeWire", "--transport usb", path}},
		{unix.EACCES, ErrPermission, []string{"gflex install-udev", path}},
		{unix.EPERM, ErrPermission, []string{"gflex install-udev"}},
		{unix.ENOENT, ErrNotFound, []string{"gflex devices"}},
		{unix.ENODEV, ErrNotFound, []string{"gflex devices"}},
	}
	for _, c := range cases {
		err := classifyOpenError(path, &fs.PathError{Op: "open", Path: path, Err: c.errno})
		if !errors.Is(err, c.sentinel) {
			t.Errorf("%v: got %v, want %v", c.errno, err, c.sentinel)
			continue
		}
		if !errors.Is(err, c.errno) {
			t.Errorf("%v: errno lost from the chain: %v", c.errno, err)
		}
		for _, m := range c.mentions {
			if !strings.Contains(err.Error(), m) {
				t.Errorf("%v: message does not mention %q: %v", c.errno, m, err)
			}
		}
	}

	// Anything unrecognised is still wrapped, but claims no diagnosis.
	err := classifyOpenError(path, &fs.PathError{Op: "open", Path: path, Err: unix.EIO})
	for _, s := range []error{ErrBusy, ErrPermission, ErrNotFound} {
		if errors.Is(err, s) {
			t.Errorf("EIO misclassified as %v", s)
		}
	}
	if !errors.Is(err, unix.EIO) {
		t.Errorf("EIO lost from the chain: %v", err)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestOpenMissingNode(t *testing.T) {
	// A path under t.TempDir(), so no real device is touched.
	_, err := Open(filepath.Join(t.TempDir(), "midiC9D9"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

func TestSummarise(t *testing.T) {
	if got := summarise(nil); got != "(none)" {
		t.Errorf("got %q", got)
	}
	got := summarise([]PortInfo{
		{Path: "/dev/snd/midiC0D0", Name: "A"},
		{Path: "/dev/snd/midiC1D0"},
	})
	want := fmt.Sprintf("%s, %s", `/dev/snd/midiC0D0 ("A")`, "/dev/snd/midiC1D0")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A Name that reached PortInfo by some route other than portName -- a caller
// building one by hand -- must still not be able to write an escape sequence to
// the terminal through an error message. That is what the %q is for.
func TestSummariseEscapesControlBytes(t *testing.T) {
	got := summarise([]PortInfo{{Path: "/dev/snd/midiC0D0", Name: "A\x1b[2J"}})
	for i := 0; i < len(got); i++ {
		if got[i] < 0x20 {
			t.Fatalf("summarise emitted a control byte %#02x: %q", got[i], got)
		}
	}
}
