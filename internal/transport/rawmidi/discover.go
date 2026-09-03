package rawmidi

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jzbz/gflex/internal/proto"
)

// PortInfo describes one candidate ALSA rawmidi port.
//
// Card and Device are -1 when they could not be derived from the node name,
// which only happens for a caller-supplied path that is not a midiC<n>D<m>
// node (a /dev/snd/by-path alias, say).
//
// The fields carry no JSON tags because nothing marshals this type:
// "gflex devices --json" declares its own struct, since presentation belongs
// above the transport rather than inside it.
type PortInfo struct {
	Path   string // /dev/snd/midiC1D0
	Card   int    // ALSA card number, -1 if unknown
	Device int    // rawmidi device number on that card, -1 if unknown
	Name   string // ALSA port/card name, best effort

	// VendorID is 0 when the port could not be traced back to a USB device.
	VendorID uint16
	// ProductID is the USB product ID. A VFLEX in application mode reports
	// 0x800F (SPEC.md §14.1, measured); the bootloader-mode PID is still
	// unmeasured, so nothing here matches on it -- the vendor ID is what
	// identifies the device in both modes.
	ProductID uint16

	IsVFlex bool

	// SysPath is the sysfs directory of the owning USB device, when known.
	SysPath string

	// Fallback marks a port that was selected only because it was the single
	// rawmidi port on the system, with nothing identifying it as a VFLEX. The
	// vendor app has the same fallback (SPEC.md §3.4); the CLI should warn
	// rather than silently program an unknown device.
	Fallback bool
}

// classify decides whether a port is a VFLEX.
//
// The USB vendor ID is authoritative, so when it is known it is the whole
// answer: a port whose USB parent is somebody else's is not a VFLEX however it
// names itself. Only a port with no USB parent at all -- a virtual card, or one
// whose sysfs walk failed -- falls back to the vendor app's identification
// rule, a plain lowercase substring of the port name (SPEC.md §3.4).
//
// The substring does match a real unit ("Werewolf VFLEX", SPEC.md §14.2), but
// it is evidence of a different quality: the name is firmware-dependent and
// anything may call itself a VFLEX. Select therefore also prefers a
// VID-confirmed port over a name-only one, which is the precedence SPEC.md §3.4
// prescribes -- vendor ID first, name substring second, sole port last.
func (p *PortInfo) classify() {
	if p.VendorID != 0 {
		p.IsVFlex = p.VendorID == proto.VendorID
		return
	}
	p.IsVFlex = strings.Contains(strings.ToLower(p.Name), proto.PortNameMatch)
}

// scanner holds the filesystem roots discovery reads. Tests point them at a
// fixture tree; nothing outside the package can change the real ones.
type scanner struct {
	sysUSB     string // /sys/bus/usb/devices
	sysSound   string // /sys/class/sound
	procAsound string // /proc/asound
	devSnd     string // /dev/snd
}

func defaultScanner() *scanner {
	return &scanner{
		sysUSB:     "/sys/bus/usb/devices",
		sysSound:   "/sys/class/sound",
		procAsound: "/proc/asound",
		devSnd:     "/dev/snd",
	}
}

// midiGlobs locate a rawmidi node relative to a USB device's sysfs directory.
// snd-usb-audio creates the sound class directory under the *interface* it
// binds (<dev>/<dev>:1.<n>/sound/card<N>/midiC<N>D<M>), but the directory can
// also sit directly under the device, so both depths are searched.
var midiGlobs = []string{
	"sound/card*/midiC*D*",
	"*/sound/card*/midiC*D*",
}

// maxSysfsWalk bounds the upward walk from a sound card to its USB parent.
// card -> sound -> interface -> device is three levels; the slack covers hubs
// and future layouts without ever escaping into an unbounded loop.
const maxSysfsWalk = 8

// Discover enumerates every ALSA rawmidi port on the system.
//
// Two passes are merged. The first is anchored on the USB vendor ID: it walks
// /sys/bus/usb/devices looking for idVendor == 37bf and then finds the rawmidi
// nodes underneath. This is better than the vendor app's name match even now
// that the advertised name is known ("Werewolf VFLEX", SPEC.md §14.2): that is
// one unit's firmware, the name is not a protocol constant, and a name match
// cannot tell a VFLEX from anything else that spells "vflex". The second
// pass enumerates /dev/snd directly so that non-VFLEX ports (and a VFLEX whose
// sysfs layout we failed to walk) still appear in "gflex devices".
//
// Ports are returned sorted by card then device. A missing /dev/snd yields an
// empty list rather than an error: it means no sound devices, not a fault.
func Discover() ([]PortInfo, error) { return defaultScanner().discover() }

func (s *scanner) discover() ([]PortInfo, error) {
	byPath := make(map[string]*PortInfo)
	var order []string

	// First writer wins: the vendor-anchored pass knows more than the /dev/snd
	// sweep, so it runs first and its entries are not overwritten.
	add := func(p PortInfo) {
		if _, dup := byPath[p.Path]; dup {
			return
		}
		cp := p
		byPath[p.Path] = &cp
		order = append(order, p.Path)
	}

	// Pass 1: vendor-ID anchored. Best effort -- a failure here only costs us
	// the USB identification, since pass 2 finds the node anyway.
	for _, p := range s.discoverByVendor() {
		add(p)
	}

	// Pass 2: every rawmidi node the kernel exposes.
	entries, err := os.ReadDir(s.devSnd)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("rawmidi: reading %s: %w", s.devSnd, err)
	}
	for _, e := range entries {
		card, dev, ok := parseMidiNode(e.Name())
		if !ok {
			continue
		}
		add(PortInfo{Path: filepath.Join(s.devSnd, e.Name()), Card: card, Device: dev})
	}

	out := make([]PortInfo, 0, len(order))
	for _, path := range order {
		p := byPath[path]
		s.enrich(p)
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Card != out[j].Card {
			return out[i].Card < out[j].Card
		}
		if out[i].Device != out[j].Device {
			return out[i].Device < out[j].Device
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// enrich fills in whatever the discovering pass did not know: the port name,
// the USB ids (walked up from the sound card), and the VFLEX verdict.
func (s *scanner) enrich(p *PortInfo) {
	if p.Name == "" && p.Card >= 0 {
		p.Name = s.portName(p.Card, p.Device)
	}
	if p.VendorID == 0 && p.Card >= 0 {
		if vid, pid, sysPath, ok := s.usbIDsForCard(p.Card); ok {
			p.VendorID, p.ProductID = vid, pid
			if p.SysPath == "" {
				p.SysPath = sysPath
			}
		}
	}
	p.classify()
}

// discoverByVendor walks /sys/bus/usb/devices for VFLEX units and reports the
// rawmidi nodes they own.
func (s *scanner) discoverByVendor() []PortInfo {
	entries, err := os.ReadDir(s.sysUSB)
	if err != nil {
		return nil
	}
	var out []PortInfo
	for _, e := range entries {
		devDir := filepath.Join(s.sysUSB, e.Name())
		// Interface directories (1-1:1.0) live in the same directory but own no
		// idVendor; readHexID fails on them, which skips them for free.
		vid, ok := readHexID(filepath.Join(devDir, "idVendor"))
		if !ok || vid != proto.VendorID {
			continue
		}
		pid, _ := readHexID(filepath.Join(devDir, "idProduct"))
		for _, g := range midiGlobs {
			matches, err := filepath.Glob(filepath.Join(devDir, g))
			if err != nil {
				continue // only ErrBadPattern, impossible for these constants
			}
			for _, m := range matches {
				card, dev, ok := parseMidiNode(filepath.Base(m))
				if !ok {
					continue
				}
				out = append(out, PortInfo{
					Path:      filepath.Join(s.devSnd, filepath.Base(m)),
					Card:      card,
					Device:    dev,
					VendorID:  vid,
					ProductID: pid,
					SysPath:   devDir,
				})
			}
		}
	}
	return out
}

// portName resolves a human-readable name for a rawmidi port, best effort.
//
// The first source is the one the vendor app's substring test would see: the
// kernel's rawmidi device name, which /proc/asound/card<N>/midi<D> prints on
// its first line. The remaining sources name the card rather than the port, so
// they are only used when that file is absent.
//
// Every candidate goes through printableName. The kernel builds these names
// from the device's own USB string descriptors, so they are device-supplied in
// exactly the sense the identity strings are (SPEC.md §17) -- this was the one
// such string in the tree that no filter was applied to.
func (s *scanner) portName(card, device int) string {
	if b, err := os.ReadFile(filepath.Join(s.procAsound,
		fmt.Sprintf("card%d", card), fmt.Sprintf("midi%d", device))); err == nil {
		if line := printableName(firstLine(b)); line != "" {
			return line
		}
	}
	// The sound class node's "device" link points at the card directory, which
	// owns the short ALSA id ("Generic", "VFLEX"). Both spellings are tried
	// because the link target has moved between kernel versions.
	node := fmt.Sprintf("midiC%dD%d", card, device)
	for _, rel := range []string{
		filepath.Join(node, "device", "id"),
		filepath.Join(node, "device", "..", "id"),
		filepath.Join(fmt.Sprintf("card%d", card), "id"),
	} {
		if b, err := os.ReadFile(filepath.Join(s.sysSound, rel)); err == nil {
			if line := printableName(firstLine(b)); line != "" {
				return line
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(s.procAsound, "cards")); err == nil {
		if name, ok := parseProcAsoundCards(b)[card]; ok {
			if name := printableName(name); name != "" {
				return name
			}
		}
	}
	return ""
}

// printableName applies the tree's device-string discipline to an ALSA port
// name: everything outside printable ASCII is dropped, then the result is
// trimmed.
//
// It defers to proto.DecodeString rather than restating the rule, because the
// rule must not drift between the two: an identity string read over the wire
// and a port name read out of procfs are the same class of data, both chosen
// by the device. The objection that kept internal/bootloader from reusing
// DecodeString -- that converting an unbounded host-side string to []byte to
// borrow eight lines could copy megabytes -- does not apply here, where the
// input is one line of a kernel-generated file.
//
// What this defends against is an ESC introducing an ANSI control sequence in
// a USB string descriptor: the name is printed to a terminal by "gflex devices"
// and by the candidate lists in the errors below, and a device that can clear
// the screen can hide what discovery actually found.
func printableName(s string) string {
	return proto.DecodeString([]byte(s))
}

// usbIDsForCard walks up from a sound card's sysfs node until it finds the
// directory that owns idVendor, i.e. the USB device behind the card. This is
// the reverse of discoverByVendor and exists so that "gflex devices" can show
// the vendor of every port, not just of VFLEX units.
func (s *scanner) usbIDsForCard(card int) (vid, pid uint16, sysPath string, ok bool) {
	dir, err := filepath.EvalSymlinks(filepath.Join(s.sysSound, fmt.Sprintf("card%d", card)))
	if err != nil {
		return 0, 0, "", false
	}
	for i := 0; i < maxSysfsWalk; i++ {
		if v, ok := readHexID(filepath.Join(dir, "idVendor")); ok {
			p, _ := readHexID(filepath.Join(dir, "idProduct"))
			return v, p, dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return 0, 0, "", false
}

// ---------------------------------------------------------------------------
// Selection
// ---------------------------------------------------------------------------

// OpenAuto picks a port and opens it.
//
// A hint beginning with "/" is taken as a device path and used as given.
// Any other non-empty hint is a case-insensitive substring matched against the
// port name and the node name. With no hint the single VFLEX-looking port wins;
// failing that, and only if the system has exactly one rawmidi port at all and
// that port's USB vendor is unknown, it is used with PortInfo.Fallback set so
// the caller can warn -- this mirrors the vendor app's sole-port fallback
// (SPEC.md §3.4), minus the ports sysfs has already identified as somebody
// else's.
func OpenAuto(hint string) (proto.Transport, PortInfo, error) {
	return defaultScanner().openAuto(hint)
}

func (s *scanner) openAuto(hint string) (proto.Transport, PortInfo, error) {
	ports, err := s.discover()
	if err != nil {
		return nil, PortInfo{}, err
	}
	pi, err := Select(ports, hint)
	if err != nil {
		return nil, PortInfo{}, err
	}
	// A path hint can name a port discovery did not list; fill in what we can
	// so the caller still gets a useful description.
	s.enrich(&pi)

	t, err := Open(pi.Path)
	if err != nil {
		return nil, pi, err
	}
	return t, pi, nil
}

// Select applies the port-choosing policy to an already-discovered list. It
// performs no I/O, which makes the policy testable without hardware; OpenAuto
// is Discover plus Select plus Open.
//
// The path branch below serves direct callers of this package. The CLI resolves
// a "/" --port to Open itself and never reaches OpenAuto with one, deliberately:
// discovery walks sysfs and can fail for reasons that have nothing to do with
// the node the user named, and an explicitly named node has to keep opening
// through that.
func Select(ports []PortInfo, hint string) (PortInfo, error) {
	if strings.HasPrefix(hint, "/") {
		for _, p := range ports {
			if p.Path == hint {
				return p, nil
			}
		}
		// Not enumerated: trust the user. Opening it will produce a precise
		// errno-derived error if it really is not there.
		p := PortInfo{Path: hint, Card: -1, Device: -1}
		if card, dev, ok := parseMidiNode(filepath.Base(hint)); ok {
			p.Card, p.Device = card, dev
		}
		return p, nil
	}

	if hint != "" {
		want := strings.ToLower(hint)
		var matches []PortInfo
		for _, p := range ports {
			if strings.Contains(strings.ToLower(p.Name), want) ||
				strings.Contains(strings.ToLower(filepath.Base(p.Path)), want) {
				matches = append(matches, p)
			}
		}
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
			return PortInfo{}, fmt.Errorf("%w matching %q. Present: %s",
				ErrNotFound, hint, summarise(ports))
		default:
			return PortInfo{}, fmt.Errorf("%w matching %q: %s. Pass --port with a device path",
				ErrAmbiguous, hint, summarise(matches))
		}
	}

	// Vendor ID first, name substring second: the precedence SPEC.md §3.4
	// prescribes and classify records. A port confirmed by the USB vendor ID
	// outranks one identified only by its name, so a second card that happens
	// to spell "vflex" -- a loopback given that ALSA id, a sibling product --
	// cannot turn an unambiguous VFLEX into ErrAmbiguous, and cannot be chosen
	// over it either.
	var vflex, confirmed []PortInfo
	for _, p := range ports {
		if !p.IsVFlex {
			continue
		}
		vflex = append(vflex, p)
		if p.VendorID == proto.VendorID {
			confirmed = append(confirmed, p)
		}
	}
	if len(confirmed) > 0 {
		vflex = confirmed
	}
	switch {
	case len(vflex) == 1:
		return vflex[0], nil
	case len(vflex) > 1:
		return PortInfo{}, fmt.Errorf("%w look like a VFLEX: %s. Pass --port to choose one",
			ErrAmbiguous, summarise(vflex))
	case len(ports) == 0:
		return PortInfo{}, fmt.Errorf("%w. Check the cable, then \"gflex devices\"; "+
			"if the unit is in bootloader mode (slow blinking white LED) it exposes no MIDI port", ErrNoPorts)
	case len(ports) == 1:
		p := ports[0]
		// The sole-port fallback exists for the case where the tool does not
		// know what the port is, which is not the same as knowing it is not a
		// VFLEX. Discovery has already walked sysfs for every port, so a
		// non-zero vendor ID that is not Tundra Labs' is a positive
		// identification of somebody else's device -- and taking it would open
		// a synthesizer and start writing protocol frames at it. Refuse; --port
		// remains the way to say "yes, that one, I mean it".
		if p.VendorID != 0 && p.VendorID != proto.VendorID {
			return PortInfo{}, fmt.Errorf("%w: the only MIDI port on this system is %s, whose USB "+
				"vendor is 0x%04x rather than 0x%04x. Pass --port %s to use it anyway",
				ErrNotFound, summarise(ports), p.VendorID, proto.VendorID, p.Path)
		}
		// Sole-port fallback, as the vendor app does (SPEC.md §3.4). Flagged,
		// not silent: nothing here says the device is a VFLEX.
		p.Fallback = true
		return p, nil
	default:
		return PortInfo{}, fmt.Errorf("%w and none identifies as a VFLEX: %s. Pass --port to choose one",
			ErrAmbiguous, summarise(ports))
	}
}

// summarise renders a candidate list for an error message.
//
// The name is quoted, as every other renderer of it quotes it (describePort in
// internal/cli). portName has already stripped anything unprintable, so this is
// belt and braces against a Name set by some other route; it also keeps the
// comma-separated list unambiguous when a device names itself "a, b".
func summarise(ports []PortInfo) string {
	if len(ports) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		if p.Name != "" {
			parts = append(parts, fmt.Sprintf("%s (%q)", p.Path, p.Name))
			continue
		}
		parts = append(parts, p.Path)
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// Parsing helpers
// ---------------------------------------------------------------------------

// parseMidiNode splits an ALSA rawmidi node name, "midiC1D0", into its card and
// device numbers.
func parseMidiNode(base string) (card, device int, ok bool) {
	const prefix = "midiC"
	if !strings.HasPrefix(base, prefix) {
		return 0, 0, false
	}
	rest := base[len(prefix):]
	i := strings.IndexByte(rest, 'D')
	if i <= 0 {
		return 0, 0, false
	}
	card, ok = atoiStrict(rest[:i])
	if !ok {
		return 0, 0, false
	}
	device, ok = atoiStrict(rest[i+1:])
	if !ok {
		return 0, 0, false
	}
	return card, device, true
}

// atoiStrict parses an unsigned decimal run, rejecting signs and empty input
// that strconv.Atoi would otherwise accept or mis-handle.
func atoiStrict(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// readHexID reads a sysfs idVendor/idProduct file. udev and sysfs both spell
// these as bare lowercase hex with no 0x prefix (SPEC.md §4.4).
func readHexID(path string) (uint16, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(firstLine(b), 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(v), true
}

// parseProcAsoundCards parses /proc/asound/cards, whose entries look like
//
//	1 [Generic_1      ]: HDA-Intel - HD-Audio Generic
//	                     HD-Audio Generic at 0xa0440000 irq 171
//
// It returns the descriptive text after "]:" per card number, falling back to
// the bracketed ALSA id when that text is empty.
func parseProcAsoundCards(b []byte) map[int]string {
	out := make(map[int]string)
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		lb := strings.IndexByte(line, '[')
		rb := strings.IndexByte(line, ']')
		if lb <= 0 || rb < lb {
			continue // continuation line, or not a card header
		}
		num, ok := atoiStrict(strings.TrimSpace(line[:lb]))
		if !ok {
			continue
		}
		id := strings.TrimSpace(line[lb+1 : rb])
		desc := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line[rb+1:]), ":"))
		if desc == "" {
			desc = id
		}
		if _, dup := out[num]; !dup {
			out[num] = desc
		}
	}
	return out
}

// firstLine returns the first line of a sysfs/procfs file, trimmed.
func firstLine(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}
