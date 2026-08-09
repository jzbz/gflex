package pdo

import (
	"fmt"
	"math"
	"strings"
	"text/tabwriter"
)

// String renders the log as the capability table produced by Summary.
func (l *Log) String() string { return l.Summary() }

// Summary renders the log as a human-readable capability table, grouped
// SPR/EPR x fixed/variable, preceded by the header diagnostics.
//
// The grouping follows SPEC.md §9.4: a fixed PDO is EPR when its voltage exceeds
// 20 V; an EPR AVS APDO is EPR variable; PPS and SPR AVS are SPR variable.
// Battery and Variable PDOs — which the vendor app discards entirely — are
// grouped by their maximum voltage.
func (l *Log) Summary() string {
	if l == nil {
		return "no PDO log\n"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "target %d mV, measured %d mV, %d PDO(s) received, selected PDO id %d\n",
		l.TargetVoltageMv, l.MeasuredVoltageMv, l.NPDOsReceived, l.SelectedPDOID)
	fmt.Fprintf(&sb, "flags 0x%04X, flags2 0x%04X, EPR cable fail: %s\n",
		l.Flags, l.Flags2, yesNo(l.EPRCableFail))
	if l.EPRCableFail {
		sb.WriteString("  the source could not enter Extended Power Range; an eMarker-equipped 5 A EPR cable is required above 20 V\n")
	}
	sb.WriteString("\n")

	// tabwriter aligns each run of tab-separated lines; a heading line without
	// tabs terminates the run, which is exactly the per-section behaviour wanted.
	w := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	for _, s := range sections {
		var rows []PDO
		for _, p := range l.PDOs {
			if s.member(p) {
				rows = append(rows, p)
			}
		}
		// The four SPR/EPR x fixed/variable sections always appear, so the
		// table has the same shape for every source and an absent capability
		// is visible. The trailing diagnostic section appears only when it has
		// something to report.
		if len(rows) == 0 && s.optional {
			continue
		}
		fmt.Fprintf(w, "%s\n", s.title)
		if len(rows) == 0 {
			fmt.Fprintf(w, "  (none)\n")
			continue
		}
		for _, p := range rows {
			fmt.Fprintf(w, "  #%d\t%s\t%s\t%s\t%s%s\n",
				p.Index, p.Kind.Label(), p.voltageDesc(), p.currentDesc(), p.RawHex(), invalidNote(p))
		}
	}
	// Flush cannot fail against a strings.Builder, whose Write never errors.
	_ = w.Flush()

	// The "?" in an SPR AVS row's voltage column is the only figure in this
	// table that did not come off the wire, and a bare question mark is not a
	// disclosure. Spell it out, so nothing here can be read as scanned data when
	// it is USB-PD 3.2 speaking (SPEC.md §9.4). Written after the Flush so it
	// cannot widen the table's columns.
	if l.hasKind(KindSPRAVS) {
		fmt.Fprintf(&sb, "\n  ? the SPR AVS %s output range is assumed, not scanned: %s\n",
			formatRange(SPRAVSMinVoltageV, SPRAVSMaxVoltageV), sprAVSAssumptionClause)
	}
	return sb.String()
}

// hasKind reports whether the log holds an object of the given class.
func (l *Log) hasKind(k Kind) bool {
	for _, p := range l.PDOs {
		if p.Kind == k {
			return true
		}
	}
	return false
}

// section is one group of the capability table. optional sections are omitted
// entirely when empty.
type section struct {
	title    string
	optional bool
	member   func(PDO) bool
}

var sections = []section{
	{title: "SPR fixed", member: func(p PDO) bool { return p.Kind == KindFixed && !p.EPR }},
	{title: "SPR variable", member: func(p PDO) bool {
		switch p.Kind {
		case KindPPS, KindSPRAVS:
			return true
		case KindBattery, KindVariable:
			return !p.EPR
		}
		return false
	}},
	{title: "EPR fixed", member: func(p PDO) bool { return p.Kind == KindFixed && p.EPR }},
	{title: "EPR variable", member: func(p PDO) bool {
		switch p.Kind {
		case KindEPRAVS:
			return true
		case KindBattery, KindVariable:
			return p.EPR
		}
		return false
	}},
	{title: "Unrecognised", optional: true, member: func(p PDO) bool { return p.Kind == KindUnknown }},
}

// String renders one PDO on a single line, for -v output and error messages.
func (p PDO) String() string {
	return fmt.Sprintf("#%d %s %s %s (%s)%s",
		p.Index, p.Kind, p.voltageDesc(), p.currentDesc(), p.RawHex(), invalidNote(p))
}

// RawHex renders the undecoded PDO word the way USB-PD documentation does.
// The JSON form keeps Raw as a number; this is for human output.
func (p PDO) RawHex() string { return fmt.Sprintf("0x%08X", p.Raw) }

// voltageDesc renders the voltage or voltage range appropriate to the class.
func (p PDO) voltageDesc() string {
	switch p.Kind {
	case KindFixed:
		return fmt.Sprintf("%.2f V", p.VoltageV)
	case KindSPRAVS:
		// The wire carries no range for SPR AVS; this is the USB-PD assumption.
		return fmt.Sprintf("%.1f-%.1f V?", SPRAVSMinVoltageV, SPRAVSMaxVoltageV)
	case KindUnknown:
		return "-"
	default:
		return fmt.Sprintf("%.2f-%.2f V", p.MinVoltageV, p.MaxVoltageV)
	}
}

// currentDesc renders the current or power limit appropriate to the class.
//
// Every figure goes through reportable rather than being read straight out of
// the field, for the reason reportable exists: decodePDO already bounded what it
// stored, but a PDO assembled anywhere else — by hand in a test, by a future
// decoder, by another package — would otherwise print a current no cable can
// carry, and print it with no disclosure attached, since Declared* would be
// empty. This is a rendering path, so it is exactly where such a figure would be
// believed. declaredNote appends the source's claim wherever the ceiling bit, so
// the bound is visible instead of silent.
func (p PDO) currentDesc() string {
	switch p.Kind {
	case KindBattery:
		return fmt.Sprintf("%.2f W", p.MaxPowerW)
	case KindEPRAVS:
		return fmt.Sprintf("%d W", p.PDPWatts)
	case KindSPRAVS:
		a15, d15 := reportable(p.MaxCurrent15VA, p.DeclaredMaxCurrent15VA)
		a20, d20 := reportable(p.MaxCurrent20VA, p.DeclaredMaxCurrent20VA)
		return fmt.Sprintf("%.2f A @15 V / %.2f A @20 V%s",
			a15, a20, declaredNote(math.Max(d15, d20)))
	case KindUnknown:
		return "-"
	default:
		a, d := reportable(p.MaxCurrentA, p.DeclaredMaxCurrentA)
		return fmt.Sprintf("%.2f A%s", a, declaredNote(d))
	}
}

// declaredNote marks a current the cable ceiling reduced; declared is the
// advertised figure, zero when nothing was reduced. For an SPR AVS APDO only the
// aggregate is named: which band was reduced is in the JSON form, and the
// undecoded word is on the same table row for anyone who wants to check the
// arithmetic.
func declaredNote(declared float64) string {
	if declared <= 0 {
		return ""
	}
	return fmt.Sprintf("  [source declares %.2f A; no cable carries over %.2f A]",
		declared, MaxCableCurrentA)
}

func invalidNote(p PDO) string {
	if p.Valid {
		return ""
	}
	if p.Kind == KindEPRAVS {
		// This is the promotion path for Log.EPRCableFail; say so where the
		// user sees it rather than only in the header.
		return "  [invalid - EPR cable fault]"
	}
	return "  [invalid]"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
