// Package pdo decodes the VFLEX's 90-byte USB Power Delivery capability log and
// evaluates whether the scanned power source can actually supply a requested
// voltage and current.
//
// Everything in this package is pure computation: no device, no I/O, no context.
// A caller obtains the blob with session.FullPDOLog and hands it to Parse.
//
// # Endianness
//
// The PDO log is LITTLE-ENDIAN. It is the only little-endian data in the entire
// VFLEX system — every other multi-byte scalar in the protocol is big-endian
// (SPEC.md §5.1, §9.3). The header fields and all twenty PDO words are read with
// binary.LittleEndian here, deliberately and exclusively.
//
// # Relationship to the vendor application
//
// Two decoding behaviours are deliberately better than the vendor app's:
//
//   - The vendor silently discards Battery (type 1) and Variable (type 2) PDOs.
//     This package decodes them, classifies them and reports them; see
//     decodeBattery / decodeVariable.
//   - The vendor's compatibility check runs against a cloud "power source"
//     record; Evaluate runs against the PDOs the device actually scanned, which
//     cannot disagree with the hardware in front of the user.
//   - The vendor reports whatever current a PDO claims. Every current this
//     package reports is bounded by MaxCableCurrentA first; see boundCurrent.
//
// See SPEC.md §9.3 (blob layout), §9.4 (PDO decode) and §9.5 (compatibility).
package pdo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Blob geometry, from SPEC.md §9.3.
const (
	// LogBytes is the length of the PDO log. The device returns twelve 8-byte
	// chunks (96 bytes); the first 90 are the log and the remainder is padding.
	LogBytes = 90
	// HeaderBytes is the fixed header preceding the PDO array.
	HeaderBytes = 10
	// MaxPDOs is the number of uint32 slots in the array. A USB-PD source may
	// advertise at most 7 SPR + 6 EPR objects, so the array is never full.
	MaxPDOs = 20
)

// FlagEPRCableFail is bit 3 of the flags2 word: the device could not establish
// Extended Power Range operation, in practice because the attached cable is not
// an eMarked 5 A EPR cable. SPEC.md §9.3.
//
// Note that this is only one of two sources for the condition: an EPR_AVS APDO
// that fails validation sets Log.EPRCableFail as well (SPEC.md §9.4).
const FlagEPRCableFail uint16 = 0x0008

// Voltage boundaries used to classify and evaluate capabilities.
const (
	// EPRThresholdV is the SPR/EPR boundary. At or below 20 V the source is
	// operating in Standard Power Range; above it, Extended Power Range and an
	// eMarked 5 A cable are required (SPEC.md §9.4, §13.9).
	EPRThresholdV = 20.0

	// SPRAVSMinVoltageV and SPRAVSMaxVoltageV bound the SPR Adjustable Voltage
	// Supply output range.
	//
	// Unlike PPS and EPR AVS, an SPR AVS APDO carries no voltage range on the
	// wire at all — only two current limits (SPEC.md §9.4). The range therefore
	// comes from USB-PD 3.2, not from the scan, and is applied as an assumption
	// when deciding whether an SPR AVS supply can reach a requested voltage.
	SPRAVSMinVoltageV = 9.0
	SPRAVSMaxVoltageV = 20.0

	// MaxCableCurrentA is the highest current this package will report as
	// available, whatever a PDO claims. Two independent limits put the ceiling
	// at 5 A, and the lower of the two would win in any case:
	//
	//   - No USB-C cable is rated above 5 A, and above 20 V USB-PD requires an
	//     eMarked 5 A EPR cable by definition (SPEC.md §1.1, §13.9).
	//   - The VFLEX itself passes at most 5 A (SPEC.md §1, matching the
	//     firmware's DEFAULT_CURRENT_LIMIT_MA = 5000).
	//
	// It bounds far more than the EPR AVS power budget it was introduced for.
	// The Fixed and Variable current fields are ten bits of 10 mA, so they
	// decode up to 10.23 A; the PPS field is seven bits of 50 mA, up to 6.35 A;
	// each SPR AVS band limit is ten bits of 10 mA again (SPEC.md §9.4). A
	// malformed or hostile source can therefore advertise a current no cable
	// could carry, and over-reporting available current is the one direction
	// that destroys hardware. boundCurrent is the single place the ceiling is
	// applied, and every stored and derived current in this package goes
	// through it.
	MaxCableCurrentA = 5.0
)

// Parse errors. The message text of these two reproduces SPEC.md §9.6 verbatim,
// including its capitalisation, because those strings are what the vendor app
// shows and what users will search for. That is a deliberate departure from the
// usual Go convention of lowercase, unpunctuated error strings.
var (
	// ErrShortLog means fewer than LogBytes bytes were supplied.
	ErrShortLog = errors.New("Invalid PDO log length")
	// ErrEmptyLog means the blob is present but entirely zero: the device was
	// never attached to a PD source between the erase and the read-back.
	ErrEmptyLog = errors.New("No PDO data captured (log is empty). Unplug vFlex from phone, plug into a USB-C PD charger (e.g. MacBook charger) for ~10s, then reconnect and retry.")
)

// Kind is the decoded class of a Power Data Object.
type Kind int

// PDO classes. The first three come from the two-bit type field; the last three
// from the two-bit subtype field of an Augmented PDO (SPEC.md §9.4).
const (
	KindFixed    Kind = iota // type 0: a single fixed voltage
	KindBattery              // type 1: a battery range, ignored by the vendor app
	KindVariable             // type 2: a variable range, ignored by the vendor app
	KindPPS                  // type 3 subtype 0: SPR Programmable Power Supply
	KindEPRAVS               // type 3 subtype 1: EPR Adjustable Voltage Supply
	KindSPRAVS               // type 3 subtype 2: SPR Adjustable Voltage Supply
	KindUnknown              // type 3 subtype 3: reserved / not defined
)

var kindNames = map[Kind]string{
	KindFixed:    "fixed",
	KindBattery:  "battery",
	KindVariable: "variable",
	KindPPS:      "pps",
	KindEPRAVS:   "epr_avs",
	KindSPRAVS:   "spr_avs",
	KindUnknown:  "unknown",
}

var kindLabels = map[Kind]string{
	KindFixed:    "Fixed",
	KindBattery:  "Battery",
	KindVariable: "Variable",
	KindPPS:      "PPS",
	KindEPRAVS:   "EPR AVS",
	KindSPRAVS:   "SPR AVS",
	KindUnknown:  "Unknown",
}

// String returns the machine-readable name of the class, suitable for --json.
func (k Kind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// Label returns the human-readable name of the class, for table output.
func (k Kind) Label() string {
	if s, ok := kindLabels[k]; ok {
		return s
	}
	return k.String()
}

// MarshalText makes Kind serialise as its String form in JSON.
func (k Kind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// PDO is one decoded Power Data Object.
//
// Which value fields are populated depends on Kind:
//
//	fixed     VoltageV, MaxCurrentA
//	battery   MinVoltageV, MaxVoltageV, MaxPowerW
//	variable  MinVoltageV, MaxVoltageV, MaxCurrentA
//	pps       MinVoltageV, MaxVoltageV, MaxCurrentA
//	epr_avs   MinVoltageV, MaxVoltageV, PDPWatts, MaxPowerW
//	spr_avs   MaxCurrent15VA, MaxCurrent20VA, MaxCurrentA (the larger of the two)
//	unknown   nothing
//
// Every current field holds the value a load may actually draw: the decoded
// figure bounded by MaxCableCurrentA. Where that bound bit, the corresponding
// Declared* field carries what the source advertised, so the conservative number
// is the one a caller reads by default and nothing is hidden.
//
// Raw is always the undecoded little-endian word, so the CLI can dump it and a
// future reader can re-decode a field this package got wrong.
type PDO struct {
	// Index is the slot the object occupied in the log's 20-entry array,
	// counted from 0. It is not necessarily the USB-PD "object position", which
	// is 1-based; the relationship to Log.SelectedPDOID is unverified.
	Index int    `json:"index"`
	Raw   uint32 `json:"raw"`
	Kind  Kind   `json:"kind"`
	// Valid reports whether the object passed the vendor's plausibility filter
	// for its class. Invalid objects are kept and reported rather than dropped.
	Valid bool `json:"valid"`
	// EPR reports whether the object belongs to the Extended Power Range group.
	EPR bool `json:"epr"`

	VoltageV    float64 `json:"voltage_v,omitempty"`
	MinVoltageV float64 `json:"min_voltage_v,omitempty"`
	MaxVoltageV float64 `json:"max_voltage_v,omitempty"`
	MaxCurrentA float64 `json:"max_current_a,omitempty"`

	// DeclaredMaxCurrentA is the current the object advertises on the wire, and
	// is set ONLY when that exceeds MaxCableCurrentA and MaxCurrentA was
	// therefore reduced. A non-zero value is thus both the disclosure and the
	// "this was bounded" flag; see CableBound.
	DeclaredMaxCurrentA float64 `json:"declared_max_current_a,omitempty"`

	// PDPWatts is the EPR AVS power budget, in whole watts as carried on the
	// wire. It is zero for every other class.
	PDPWatts int `json:"pdp_watts,omitempty"`

	MaxCurrent15VA float64 `json:"max_current_15v_a,omitempty"`
	MaxCurrent20VA float64 `json:"max_current_20v_a,omitempty"`

	// The per-band equivalents of DeclaredMaxCurrentA, set on the same terms.
	DeclaredMaxCurrent15VA float64 `json:"declared_max_current_15v_a,omitempty"`
	DeclaredMaxCurrent20VA float64 `json:"declared_max_current_20v_a,omitempty"`

	// MaxPowerW is the power budget as a float: the Battery PDO's 250 mW-unit
	// field (which PDPWatts cannot represent) and, for symmetry, the EPR AVS
	// PDP. This field has no counterpart in the vendor app.
	MaxPowerW float64 `json:"max_power_w,omitempty"`
}

// Log is a decoded PDO capture.
type Log struct {
	TargetVoltageMv   uint16 `json:"target_voltage_mv"`
	MeasuredVoltageMv uint16 `json:"measured_voltage_mv"`
	NPDOsReceived     uint8  `json:"n_pdos_received"`
	SelectedPDOID     uint8  `json:"selected_pdo_id"`
	Flags             uint16 `json:"flags"`
	Flags2            uint16 `json:"flags2"`
	// EPRCableFail is the union of two independent signals: bit 3 of Flags2,
	// and an EPR_AVS APDO that failed validation (SPEC.md §9.4).
	EPRCableFail bool  `json:"epr_cable_fail"`
	PDOs         []PDO `json:"pdos"`
}

// Parse decodes a PDO log blob.
//
// The blob must be at least LogBytes long; anything beyond that (the device
// returns 96 bytes in twelve chunks) is ignored. An all-zero blob is rejected
// with ErrEmptyLog, whose message is the vendor's own guidance for the case
// where the unit was never attached to a charger between erase and read-back.
func Parse(b []byte) (*Log, error) {
	if len(b) < LogBytes {
		return nil, fmt.Errorf("%w: expected ≥%d bytes, got %d", ErrShortLog, LogBytes, len(b))
	}
	blob := b[:LogBytes]
	if allZero(blob) {
		return nil, ErrEmptyLog
	}

	// Little-endian throughout — see the package comment and SPEC.md §9.3.
	l := &Log{
		TargetVoltageMv:   binary.LittleEndian.Uint16(blob[0:2]),
		MeasuredVoltageMv: binary.LittleEndian.Uint16(blob[2:4]),
		NPDOsReceived:     blob[4],
		SelectedPDOID:     blob[5],
		Flags:             binary.LittleEndian.Uint16(blob[6:8]),
		Flags2:            binary.LittleEndian.Uint16(blob[8:10]),
	}
	l.EPRCableFail = l.Flags2&FlagEPRCableFail != 0

	// Only the first nPdosReceived slots hold data; the rest of the array is
	// stale or never written. Bound it at MaxPDOs so a corrupt count cannot
	// read past the header+array region.
	n := int(l.NPDOsReceived)
	if n > MaxPDOs {
		n = MaxPDOs
	}
	l.PDOs = make([]PDO, 0, n)
	for i := range n {
		off := HeaderBytes + i*4
		raw := binary.LittleEndian.Uint32(blob[off : off+4])
		if raw == 0 {
			continue // a zero word is an empty slot, not a PDO
		}
		p := decodePDO(i, raw)
		// An EPR_AVS APDO that fails validation is itself evidence of an EPR
		// cable fault, independent of the flags2 bit (SPEC.md §9.4).
		if p.Kind == KindEPRAVS && !p.Valid {
			l.EPRCableFail = true
		}
		l.PDOs = append(l.PDOs, p)
	}
	return l, nil
}

// decodePDO decodes one 32-bit Power Data Object. SPEC.md §9.4.
func decodePDO(index int, raw uint32) PDO {
	p := PDO{Index: index, Raw: raw}
	switch (raw >> 30) & 3 {
	case 0:
		decodeFixed(&p, raw)
	case 1:
		decodeBattery(&p, raw)
	case 2:
		decodeVariable(&p, raw)
	default:
		decodeAugmented(&p, raw)
	}
	return p
}

// decodeFixed decodes a type-0 Fixed Supply PDO.
//
// The field scale is the same in SPR and EPR: 50 mV for voltage and 10 mA for
// current. Only the resulting voltage decides which range the object belongs to.
func decodeFixed(p *PDO, raw uint32) {
	p.Kind = KindFixed
	p.VoltageV = round2(0.05 * float64((raw>>10)&0x3FF))
	// Ten bits of 10 mA reach 10.23 A, which no cable and no VFLEX can carry;
	// see MaxCableCurrentA. Above 20 V it is doubly impossible, since EPR
	// operation presupposes a 5 A eMarked cable.
	p.MaxCurrentA, p.DeclaredMaxCurrentA = boundCurrent(0.01 * float64(raw&0x3FF))
	// The vendor's filter. 5 V is the mandatory first object of every source,
	// so a fixed PDO below it is a misparse rather than a real capability.
	p.Valid = p.VoltageV >= 5 && p.MaxCurrentA > 0
	p.EPR = p.VoltageV > EPRThresholdV
}

// decodeBattery decodes a type-1 Battery Supply PDO.
//
// The vendor application discards Battery PDOs without looking at them. We
// decode them instead: a battery source is a real thing to encounter and the
// user is better served by "this source offers a 3-12 V battery supply we do
// not attempt to use" than by silence.
//
// Layout: max voltage 29:20 and min voltage 19:10 in 50 mV units, max power 9:0
// in 250 mW units. The 250 mW resolution is why MaxPowerW exists — PDPWatts is
// a whole-watt field and cannot carry it.
func decodeBattery(p *PDO, raw uint32) {
	p.Kind = KindBattery
	p.MaxVoltageV = round2(0.05 * float64((raw>>20)&0x3FF))
	p.MinVoltageV = round2(0.05 * float64((raw>>10)&0x3FF))
	p.MaxPowerW = round2(0.25 * float64(raw&0x3FF))
	p.Valid = p.MinVoltageV > 0 && p.MaxVoltageV > 0 && p.MaxVoltageV >= p.MinVoltageV && p.MaxPowerW > 0
	p.EPR = p.MaxVoltageV > EPRThresholdV
}

// decodeVariable decodes a type-2 Variable Supply (non-battery) PDO.
//
// Also discarded by the vendor application, and also decoded here for the same
// reason. Layout: max voltage 29:20 and min voltage 19:10 in 50 mV units, max
// current 9:0 in 10 mA units.
func decodeVariable(p *PDO, raw uint32) {
	p.Kind = KindVariable
	p.MaxVoltageV = round2(0.05 * float64((raw>>20)&0x3FF))
	p.MinVoltageV = round2(0.05 * float64((raw>>10)&0x3FF))
	p.MaxCurrentA, p.DeclaredMaxCurrentA = boundCurrent(0.01 * float64(raw&0x3FF))
	p.Valid = p.MinVoltageV > 0 && p.MaxVoltageV > 0 && p.MaxVoltageV >= p.MinVoltageV && p.MaxCurrentA > 0
	p.EPR = p.MaxVoltageV > EPRThresholdV
}

// decodeAugmented decodes a type-3 Augmented PDO, dispatching on bits 29:28.
func decodeAugmented(p *PDO, raw uint32) {
	switch (raw >> 28) & 3 {
	case 0: // SPR Programmable Power Supply
		p.Kind = KindPPS
		p.MaxVoltageV = round2(0.1 * float64((raw>>17)&0xFF))
		p.MinVoltageV = round2(0.1 * float64((raw>>8)&0xFF))
		// Seven bits of 50 mA reach 6.35 A; see MaxCableCurrentA.
		p.MaxCurrentA, p.DeclaredMaxCurrentA = boundCurrent(0.05 * float64(raw&0x7F))
		p.Valid = p.MinVoltageV > 0 && p.MaxVoltageV > 0 &&
			p.MaxVoltageV >= p.MinVoltageV && p.MaxCurrentA > 0
		// PPS is defined only within the Standard Power Range.
		p.EPR = false

	case 1: // EPR Adjustable Voltage Supply
		p.Kind = KindEPRAVS
		// Note the wider max-voltage field: 9 bits, because EPR AVS reaches
		// 48 V where PPS stops at 21 V.
		p.MaxVoltageV = round2(0.1 * float64((raw>>17)&0x1FF))
		p.MinVoltageV = round2(0.1 * float64((raw>>8)&0xFF))
		p.PDPWatts = int(raw & 0xFF) // whole watts
		p.MaxPowerW = float64(p.PDPWatts)
		p.Valid = p.MinVoltageV > 0 && p.MaxVoltageV > 0 &&
			p.MaxVoltageV >= p.MinVoltageV && p.PDPWatts > 0
		p.EPR = true

	case 2: // SPR Adjustable Voltage Supply
		p.Kind = KindSPRAVS
		// Two BAND-SPECIFIC limits, matching the USB-PD 3.2 SPR AVS APDO:
		// bits 19:10 bound the 15-20 V band and bits 9:0 the 9-15 V band.
		//
		// MaxCurrentA is the larger of the two purely so that summaries and
		// rankings have one number to show. It is NOT the current available at
		// a given voltage — use CurrentAt for that. Reporting the strong band's
		// limit while operating in the weak band over-reports capability, which
		// is the dangerous direction for a tool that drives a power rail.
		//
		// Each band field is ten bits of 10 mA and so is bounded individually;
		// the aggregates are then formed from the bounded values, which keeps
		// MaxCurrentA <= MaxCableCurrentA by construction.
		p.MaxCurrent20VA, p.DeclaredMaxCurrent20VA = boundCurrent(0.01 * float64((raw>>10)&0x3FF))
		p.MaxCurrent15VA, p.DeclaredMaxCurrent15VA = boundCurrent(0.01 * float64(raw&0x3FF))
		p.MaxCurrentA = math.Max(p.MaxCurrent15VA, p.MaxCurrent20VA)
		p.DeclaredMaxCurrentA = math.Max(p.DeclaredMaxCurrent15VA, p.DeclaredMaxCurrent20VA)
		p.Valid = p.MaxCurrent15VA > 0 || p.MaxCurrent20VA > 0
		p.EPR = false

	default: // subtype 3 is reserved; nothing can be said about it
		p.Kind = KindUnknown
		p.Valid = false
	}
}

// allZero reports whether every byte of b is zero.
func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// boundCurrent applies the MaxCableCurrentA ceiling to one decoded or derived
// current. It is the single place a current in this package becomes a number
// anything else may report.
//
// usable is what a load may actually draw. declared is the unbounded figure and
// is non-zero ONLY when the ceiling bit, so callers can test one value to learn
// both that the verdict was reduced and what the source had claimed. Anything
// non-finite or non-positive collapses to zero rather than propagating: a
// corrupt log must not produce a NaN that compares false against every limit.
func boundCurrent(a float64) (usable, declared float64) {
	if math.IsNaN(a) || a <= 0 {
		return 0, 0
	}
	if a > MaxCableCurrentA {
		return MaxCableCurrentA, round2(a)
	}
	return round2(a), 0
}

// round2 rounds to two decimal places.
//
// Necessary because none of the wire scale factors (0.05, 0.01, 0.1, 0.25) is
// exactly representable in binary floating point: 0.05*180 evaluates to
// 9.000000000000002, which then fails an == comparison against a requested 9 V
// and prints as noise. Every decoded voltage and current goes through this.
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// SPRAVSBandSplitV is the boundary between the SPR AVS APDO's two current
// bands: the 9-15 V band and the 15-20 V band (USB-PD 3.2).
const SPRAVSBandSplitV = 15.0

// CurrentAt reports the current this PDO can supply at v volts, in amps.
//
// It is the only correct way to ask what a PDO offers, and every verdict in this
// package goes through it. Reading a value field instead is wrong for two of the
// six classes and dangerous for all of them:
//
//   - An SPR AVS APDO carries two band-specific limits and the applicable one
//     depends on v. MaxCurrentA is the larger, so an APDO offering 5.00 A below
//     15 V and 3.25 A above it would claim 5 A at 18 V — a 54% over-report in
//     the direction that damages hardware.
//   - An EPR AVS APDO carries no current at all, only a power budget
//     (SPEC.md §9.4); the current it can supply is PDP/V and falls as V rises.
//   - Any class can decode to a current no cable carries, so the answer is
//     bounded by MaxCableCurrentA.
func (p PDO) CurrentAt(v float64) float64 {
	a, _ := p.currentAt(v)
	return a
}

// CableBound reports whether MaxCableCurrentA reduced any current this object
// advertises, i.e. whether a Declared* field is populated.
func (p PDO) CableBound() bool { return p.DeclaredMaxCurrentA > 0 }

// currentAt is CurrentAt plus the disclosure: declared is what the source
// advertised at this operating point, non-zero only when the cable ceiling
// reduced the answer.
func (p PDO) currentAt(v float64) (usable, declared float64) {
	switch p.Kind {
	case KindEPRAVS:
		// Watts, not amps (SPEC.md §9.4). PDP/V is unbounded as V falls: a
		// 140 W budget at 21 V computes to 6.67 A.
		if p.PDPWatts <= 0 || v <= 0 {
			return 0, 0
		}
		return boundCurrent(float64(p.PDPWatts) / v)
	case KindSPRAVS:
		if v > SPRAVSBandSplitV {
			return reportable(p.MaxCurrent20VA, p.DeclaredMaxCurrent20VA)
		}
		return reportable(p.MaxCurrent15VA, p.DeclaredMaxCurrent15VA)
	default:
		return reportable(p.MaxCurrentA, p.DeclaredMaxCurrentA)
	}
}

// reportable re-applies the ceiling to a stored field.
//
// decodePDO already bounded every current it stores, so this is normally a
// no-op. It exists so that a PDO which did not come from decodePDO — one built
// by hand in a test, assembled by a future decoder, or filled in by another
// package — still cannot get an unbounded figure out of this package.
func reportable(stored, declared float64) (usable, decl float64) {
	usable, decl = boundCurrent(stored)
	if decl == 0 {
		decl = declared
	}
	return usable, decl
}
