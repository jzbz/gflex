package rawmidi

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
)

// Describe renders a port as one human-readable line for "gflex devices".
//
//	/dev/snd/midiC2D0  card 2 device 0  "VFLEX MIDI 1"  usb 37bf:800f  [VFLEX]
func (p PortInfo) Describe() string {
	var b strings.Builder
	b.WriteString(p.Path)
	b.WriteString("  card ")
	b.WriteString(numOrDash(p.Card))
	b.WriteString(" device ")
	b.WriteString(numOrDash(p.Device))
	if p.Name != "" {
		fmt.Fprintf(&b, "  %q", p.Name)
	}
	if id := p.USBID(); id != "" {
		b.WriteString("  usb ")
		b.WriteString(id)
	}
	if p.IsVFlex {
		b.WriteString("  [VFLEX]")
	}
	if p.Fallback {
		b.WriteString("  [sole port -- identity unconfirmed]")
	}
	return b.String()
}

// Table renders a set of ports as an aligned table with a header, for the
// human-readable form of "gflex devices". The result ends in a newline; an
// empty set renders as a single explanatory line.
func Table(ports []PortInfo) string {
	if len(ports) == 0 {
		return "no ALSA rawmidi ports found\n"
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PATH\tCARD\tDEVICE\tNAME\tUSB\tVFLEX")
	for _, p := range ports {
		usb := p.USBID()
		if usb == "" {
			usb = "-"
		}
		name := p.Name
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Path, numOrDash(p.Card), numOrDash(p.Device), name, usb, yesNo(p.IsVFlex))
	}
	// tabwriter.Flush only fails if the underlying writer does; a
	// strings.Builder never does.
	_ = w.Flush()
	return b.String()
}

func numOrDash(n int) string {
	if n < 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
