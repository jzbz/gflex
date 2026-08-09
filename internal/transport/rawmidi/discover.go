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
type PortInfo struct {
	Path   string `json:"path"`   // /dev/snd/midiC1D0
	Card   int    `json:"card"`   // ALSA card number, -1 if unknown
	Device int    `json:"device"` // rawmidi device number on that card, -1 if unknown
	Name   string `json:"name"`   // ALSA port/card name, best effort

	// VendorID is 0 when the port could not be traced back to a USB device.
	VendorID uint16 `json:"vendor_id"`
	// ProductID is the USB product ID. Recording it is the point of the
	// VID-anchored scan: the VFLEX's PID is not known from any vendor artifact
	// (SPEC.md §14.1), so whatever a real unit reports here is new information.
	ProductID uint16 `json:"product_id"`

	IsVFlex bool `json:"is_vflex"`

	// SysPath is the sysfs directory of the owning USB device, when known.
	SysPath string `json:"sys_path,omitempty"`

	// Fallback marks a port that was selected only because it was the single
	// rawmidi port on the system, with nothing identifying it as a VFLEX. The
	// vendor app has the same fallback (SPEC.md §3.4); the CLI should warn
	// rather than silently program an unknown device.
	Fallback bool `json:"sole_port_fallback,omitempty"`
}

// USBID renders the USB ids as udev writes them, e.g. "37bf:800f", or "" when
// the port could not be traced to a USB device.
func (p PortInfo) USBID() string {
	if p.VendorID == 0 && p.ProductID == 0 {
		return ""
	}
	return fmt.Sprintf("%04x:%04x", p.VendorID, p.ProductID)
}

// classify applies the vendor app's identification rule, widened by the USB
// vendor ID. The app matches only on a lowercase substring of the port name
// (SPEC.md §3.4) and the name a VFLEX actually advertises is unknown, so the
// vendor ID -- which is authoritative -- is checked first and the substring is
// reproduced verbatim as a fallback.
func (p *PortInfo) classify() {
	p.IsVFlex = p.VendorID == proto.VendorID ||
		strings.Contains(strings.ToLower(p.Name), proto.PortNameMatch)
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
// nodes underneath. This is strictly better than the vendor app's name match,
// which cannot work reliably because the advertised name is unknown. The second
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
func (s *scanner) portName(card, device int) string {
	if b, err := os.ReadFile(filepath.Join(s.procAsound,
		fmt.Sprintf("card%d", card), fmt.Sprintf("midi%d", device))); err == nil {
		if line := firstLine(b); line != "" {
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
			if line := firstLine(b); line != "" {
				return line
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(s.procAsound, "cards")); err == nil {
		if name, ok := parseProcAsoundCards(b)[card]; ok {
			return name
		}
	}
	return ""
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
// failing that, and only if the system has exactly one rawmidi port at all,
// that port is used with PortInfo.Fallback set so the caller can warn -- this
// mirrors the vendor app's sole-port fallback (SPEC.md §3.4).
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

	var vflex []PortInfo
	for _, p := range ports {
		if p.IsVFlex {
			vflex = append(vflex, p)
		}
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
		// Sole-port fallback, as the vendor app does. Flagged, not silent:
		// nothing here says the device is a VFLEX.
		p := ports[0]
		p.Fallback = true
		return p, nil
	default:
		return PortInfo{}, fmt.Errorf("%w and none identifies as a VFLEX: %s. Pass --port to choose one",
			ErrAmbiguous, summarise(ports))
	}
}

// summarise renders a candidate list for an error message.
func summarise(ports []PortInfo) string {
	if len(ports) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		if p.Name != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", p.Path, p.Name))
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
