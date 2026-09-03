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

// The bits of the two flag words, SPEC.md §9.3.
//
// The vocabulary comes from the vendor's published library, which names all
// eighteen; the shipped application this protocol was otherwise recovered from
// consumes only Flag2EPRCableFail and discards the rest. Nothing here is
// measured -- see the note on Status.
const (
	// Bits of Log.Flags.
	FlagPDRequestAccepted      uint16 = 0x0001
	FlagPDRequestRejected      uint16 = 0x0002
	FlagVoltageWithinTolerance uint16 = 0x0004
	FlagWebUSBConnection       uint16 = 0x0008

	// Bits of Log.Flags2.
	Flag2SPRInitPDOsReceived        uint16 = 0x0001
	Flag2SPRPSReady                 uint16 = 0x0002
	Flag2NonEPRPS                   uint16 = 0x0004
	Flag2EPRCableFail               uint16 = 0x0008
	Flag2NonEPRPSReady              uint16 = 0x0010
	Flag2NonEPRPSReject             uint16 = 0x0020
	Flag2EPRAvailable               uint16 = 0x0040
	Flag2EPREnterRequest            uint16 = 0x0080
	Flag2EPREnterRequestAck         uint16 = 0x0100
	Flag2EPREntered                 uint16 = 0x0200
	Flag2EPRRejected                uint16 = 0x0400
	Flag2EPRFirstPDOsChunkReceived  uint16 = 0x0800
	Flag2EPRSecondPDOsChunkReceived uint16 = 0x1000
	Flag2EPRPSReady                 uint16 = 0x2000
)

// Status is the decoded negotiation state carried by the two flag words.
//
// It is a record of how the last power negotiation went, not a capability: the
// PDOs say what the source offers, these bits say what happened when the device
// asked for something. That makes them the first thing to look at when a scan
// came back with a target voltage the source plainly advertises -- a rejected
// request, an EPR entry the source refused, a cable that could not carry EPR.
//
// # Provenance
//
// The names are the vendor's own, from its published library
// (tundra-labs/lib.vflex.app), corroborated between that library and the vendor
// web tool in the same repository. None of them is measured, and only
// EPRCableFail has ever been observed to do anything here. A bit whose label
// turns out to be wrong is wrong in the output too, which is why Log.Flags and
// Log.Flags2 are still reported raw beside this: the words are the evidence,
// this is the reading of them.
//
// # EPRCableFail
//
// This field is bit 3 alone. Log.EPRCableFail is the union of that bit with the
// second, computed source -- an EPR AVS APDO that failed validation -- and it is
// the one Evaluate consults. The two therefore disagree, on purpose, exactly
// when the flag word is silent and the APDO is not.
type Status struct {
	PDRequestAccepted      bool `json:"pd_request_accepted"`
	PDRequestRejected      bool `json:"pd_request_rejected"`
	VoltageWithinTolerance bool `json:"voltage_within_tolerance"`
	WebUSBConnection       bool `json:"webusb_connection"`

	SPRInitPDOsReceived        bool `json:"spr_init_pdos_received"`
	SPRPSReady                 bool `json:"spr_ps_ready"`
	NonEPRPS                   bool `json:"non_epr_ps"`
	EPRCableFail               bool `json:"epr_cable_fail"`
	NonEPRPSReady              bool `json:"non_epr_ps_ready"`
	NonEPRPSReject             bool `json:"non_epr_ps_reject"`
	EPRAvailable               bool `json:"epr_available"`
	EPREnterRequest            bool `json:"epr_enter_request"`
	EPREnterRequestAck         bool `json:"epr_enter_request_ack"`
	EPREntered                 bool `json:"epr_entered"`
	EPRRejected                bool `json:"epr_rejected"`
	EPRFirstPDOsChunkReceived  bool `json:"epr_first_pdos_chunk_received"`
	EPRSecondPDOsChunkReceived bool `json:"epr_second_pdos_chunk_received"`
	EPRPSReady                 bool `json:"epr_ps_ready"`
}

// statusBit ties one bit to its name and to the field it decodes into.
type statusBit struct {
	mask  uint16
	label string
	field func(*Status) *bool
}

// The flag vocabulary, once. A bit named in the output but missing from Status,
// or decoded into Status but never named, is the drift this table exists to
// make impossible; both listings below are generated from it. Wire order, so
// that a rendering of the set bits reads in the order the device laid them out.
var (
	flagBits = []statusBit{
		{FlagPDRequestAccepted, "pd request accepted", func(s *Status) *bool { return &s.PDRequestAccepted }},
		{FlagPDRequestRejected, "pd request rejected", func(s *Status) *bool { return &s.PDRequestRejected }},
		{FlagVoltageWithinTolerance, "voltage within tolerance", func(s *Status) *bool { return &s.VoltageWithinTolerance }},
		{FlagWebUSBConnection, "webusb connection", func(s *Status) *bool { return &s.WebUSBConnection }},
	}
	flag2Bits = []statusBit{
		{Flag2SPRInitPDOsReceived, "spr init pdos received", func(s *Status) *bool { return &s.SPRInitPDOsReceived }},
		{Flag2SPRPSReady, "spr ps ready", func(s *Status) *bool { return &s.SPRPSReady }},
		{Flag2NonEPRPS, "non-epr ps", func(s *Status) *bool { return &s.NonEPRPS }},
		{Flag2EPRCableFail, "epr cable fail", func(s *Status) *bool { return &s.EPRCableFail }},
		{Flag2NonEPRPSReady, "non-epr ps ready", func(s *Status) *bool { return &s.NonEPRPSReady }},
		{Flag2NonEPRPSReject, "non-epr ps reject", func(s *Status) *bool { return &s.NonEPRPSReject }},
		{Flag2EPRAvailable, "epr available", func(s *Status) *bool { return &s.EPRAvailable }},
		{Flag2EPREnterRequest, "epr enter request", func(s *Status) *bool { return &s.EPREnterRequest }},
		{Flag2EPREnterRequestAck, "epr enter request ack", func(s *Status) *bool { return &s.EPREnterRequestAck }},
		{Flag2EPREntered, "epr entered", func(s *Status) *bool { return &s.EPREntered }},
		{Flag2EPRRejected, "epr rejected", func(s *Status) *bool { return &s.EPRRejected }},
		{Flag2EPRFirstPDOsChunkReceived, "epr 1st pdo chunk received", func(s *Status) *bool { return &s.EPRFirstPDOsChunkReceived }},
		{Flag2EPRSecondPDOsChunkReceived, "epr 2nd pdo chunk received", func(s *Status) *bool { return &s.EPRSecondPDOsChunkReceived }},
		{Flag2EPRPSReady, "epr ps ready", func(s *Status) *bool { return &s.EPRPSReady }},
	}
)

// decodeStatus reads both flag words into their named fields.
func decodeStatus(flags, flags2 uint16) Status {
	var s Status
	for _, group := range []struct {
		word uint16
		bits []statusBit
	}{{flags, flagBits}, {flags2, flag2Bits}} {
		for _, b := range group.bits {
			if group.word&b.mask != 0 {
				*b.field(&s) = true
			}
		}
	}
	return s
}

// FlagLabels names the bits set in the flags word, and Flag2Labels those in
// flags2, both in wire order.
//
// A bit with no name is reported as "unknown 0x…" rather than dropped. Fourteen
// of sixteen bits of flags2 are spoken for and two of flags are, so an unnamed
// bit is either a firmware newer than the vocabulary or a misreading of the
// word -- and either one is worth seeing rather than hiding.
func FlagLabels(flags uint16) []string { return labelBits(flags, flagBits) }

// Flag2Labels is FlagLabels for the second word.
func Flag2Labels(flags2 uint16) []string { return labelBits(flags2, flag2Bits) }

func labelBits(word uint16, bits []statusBit) []string {
	var out []string
	named := uint16(0)
	for _, b := range bits {
		named |= b.mask
		if word&b.mask != 0 {
			out = append(out, b.label)
		}
	}
	for i := range 16 {
		mask := uint16(1) << i
		if word&mask != 0 && named&mask == 0 {
			out = append(out, fmt.Sprintf("unknown 0x%04x", mask))
		}
	}
	return out
}

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
//	pps       MinVoltageV, MaxVoltageV, MaxCurrentA, PPSPowerLimited, PPSBudgetW
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

	// PPSPowerLimited is bit 27 of an SPR PPS APDO, the USB-PD "PPS Power
	// Limited" bit: the source cannot hold this APDO's Maximum Current across
	// the whole of its voltage range, and what it can supply at Vout is
	// min(maxI, PDP/Vout). The USB-PD power rules make such an APDO the norm
	// rather than an oddity — a 45 W source advertises 3.3-11 V at 5 A
	// (55 W > 45 W) and a 65 W one 3.3-21 V at 3.25 A (68 W > 65 W) — so
	// crediting a PPS range with its Maximum Current at the top of the range
	// over-reports on most real chargers.
	//
	// PPSBudgetW is the power budget that bound applies, in whole watts. It is
	// INFERRED, not decoded: a source's PDP travels in
	// Source_Capabilities_Extended, which the VFLEX never captures
	// (SPEC.md §9.3). See applyPPSPowerBudget for where the inference comes
	// from and when it is declined; it is zero unless PPSPowerLimited is set and
	// the fixed PDOs said something worth acting on. Every verdict that rests on
	// either field discloses it (CaveatPPSPowerLimited).
	PPSPowerLimited bool `json:"pps_power_limited,omitempty"`
	PPSBudgetW      int  `json:"pps_budget_w,omitempty"`

	MaxCurrent15VA float64 `json:"max_current_15v_a,omitempty"`
	MaxCurrent20VA float64 `json:"max_current_20v_a,omitempty"`

	// The per-band equivalents of DeclaredMaxCurrentA, set on the same terms.
	DeclaredMaxCurrent15VA float64 `json:"declared_max_current_15v_a,omitempty"`
	DeclaredMaxCurrent20VA float64 `json:"declared_max_current_20v_a,omitempty"`

	// MaxPowerW is the power budget as a float: the Battery PDO's 250 mW-unit
	// field (which PDPWatts cannot represent) and, for symmetry, the EPR AVS
	// PDP. This field has no counterpart in the vendor app.
	MaxPowerW float64 `json:"max_power_w,omitempty"`

	// EPRCapable is bit 23 of a Fixed Supply PDO: the source can operate in the
	// Extended Power Range. USB-PD requires every EPR-capable source to set it
	// on all of its fixed objects, so the mandatory 5 V object answers "does
	// this charger do EPR at all" directly -- where Evaluate otherwise has to
	// infer it from which object classes turned up.
	//
	// PeakCurrent is bits 21:20 of the same object: how far above its rated
	// current the source will go, briefly, and for how long. It is an index
	// into a USB-PD table of overload profiles, not a current; 0 means no
	// overload capability. It is reported as the raw index because the table it
	// indexes has not been transcribed here.
	//
	// Both are pointers because both are defined only for Fixed Supply PDOs.
	// nil says "this class does not carry the field", which false and 0 cannot:
	// a source that is not EPR-capable and one whose PDO has no such bit are
	// different statements, and squashing them would put the second in the
	// output as though it were the first.
	//
	// The other six flags a Fixed PDO carries (dual-role power and data, USB
	// suspend, unconstrained power, USB communications capable, unchunked
	// extended messages) are deliberately NOT decoded: the vendor's library and
	// the published USB-PD field order disagree about which bit is which, and
	// nothing here has been checked against a charger whose capabilities are
	// known independently. Raw carries them for whoever settles it
	// (SPEC.md §14.18).
	EPRCapable  *bool `json:"epr_capable,omitempty"`
	PeakCurrent *int  `json:"peak_current,omitempty"`
}

// Log is a decoded PDO capture.
type Log struct {
	TargetVoltageMv   uint16 `json:"target_voltage_mv"`
	MeasuredVoltageMv uint16 `json:"measured_voltage_mv"`
	NPDOsReceived     uint8  `json:"n_pdos_received"`
	SelectedPDOID     uint8  `json:"selected_pdo_id"`
	Flags             uint16 `json:"flags"`
	Flags2            uint16 `json:"flags2"`
	// Status is Flags and Flags2 decoded into named bits. The words above stay
	// beside it because they are the evidence and this is the reading.
	Status Status `json:"status"`
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
	l.Status = decodeStatus(l.Flags, l.Flags2)
	l.EPRCableFail = l.Flags2&Flag2EPRCableFail != 0

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
	// A power-limited PPS APDO is bounded by a budget that lives in the other
	// objects, so it can only be applied once the whole array is decoded.
	l.applyPPSPowerBudget()
	return l, nil
}

// applyPPSPowerBudget copies the source's inferred SPR power budget onto every
// PPS APDO that declared itself power limited.
//
// Such an APDO cannot hold its Maximum Current across its range: what the source
// supplies at Vout is min(maxI, PDP/Vout). The PDP itself is NOT in this blob —
// it travels in Source_Capabilities_Extended, which the VFLEX never captures
// (SPEC.md §9.3) — so it is inferred here from the fixed PDOs, which the USB-PD
// power rules derive from the source's rating: a 45 W source advertises 5 V 3 A,
// 9 V 3 A, 15 V 3 A and 20 V 2.25 A, and 20 × 2.25 = 45. Only SPR objects count,
// since PPS is an SPR class and an EPR source's higher fixed PDOs describe a
// budget its PPS range cannot draw on.
//
// Where the cable ceiling reduced a fixed PDO's current, the declared figure is
// what the budget is built from: a malformed 10.23 A object clamped to 5 A would
// otherwise understate the source and over-tighten every PPS answer.
//
// A source whose only SPR fixed object is the mandatory 5 V one yields nothing
// worth acting on — every source has that object, so 15 W says as much about
// this one as about any other — and the budget is left at zero. currentAt then
// reports the advertised current unchanged and the verdict discloses the bit
// instead of quietly bounding a figure on 15 W of guesswork.
func (l *Log) applyPPSPowerBudget() {
	budget := 0.0
	informative := false
	for _, p := range l.PDOs {
		if p.Kind != KindFixed || !p.Valid || p.VoltageV > EPRThresholdV {
			continue
		}
		a := p.MaxCurrentA
		if p.DeclaredMaxCurrentA > a {
			a = p.DeclaredMaxCurrentA
		}
		if w := p.VoltageV * a; w > budget {
			budget = w
		}
		if p.VoltageV > 5 {
			informative = true
		}
	}
	// Whole watts, rounded down, matching the EPR AVS PDP field's resolution and
	// erring the only safe way.
	w := int(math.Floor(budget))
	if !informative || w <= 0 {
		return
	}
	for i := range l.PDOs {
		if l.PDOs[i].Kind == KindPPS && l.PDOs[i].PPSPowerLimited {
			l.PDOs[i].PPSBudgetW = w
		}
	}
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
	// Bit 23 and bits 21:20. These two are the only capability fields of a
	// Fixed PDO that the vendor's library and the published USB-PD layout agree
	// on; see the PDO struct for why the other six are left in Raw
	// (SPEC.md §9.4).
	eprCapable := (raw>>23)&1 != 0
	peakCurrent := int((raw >> 20) & 3)
	p.EPRCapable, p.PeakCurrent = &eprCapable, &peakCurrent
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
		// Bit 27, "PPS Power Limited": the Maximum Current above is not held
		// across the whole range. SPEC.md §9.4 does not list this bit — the
		// vendor's library does not decode it either — and ignoring it credits
		// most real chargers with a current they will not supply near the top of
		// their range, which is the direction that destroys hardware. See the
		// PDO struct and applyPPSPowerBudget.
		p.PPSPowerLimited = (raw>>27)&1 != 0
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
// both that the verdict was reduced and what the source had claimed. NaN, zero
// and negative inputs collapse to zero rather than propagating: a corrupt log
// must not produce a NaN that compares false against every limit. +Inf is not
// collapsed — it is over the ceiling, so it takes the clamp like any other
// over-large figure and the declared half then carries +Inf verbatim. Nothing
// decoded can reach that: every current Parse produces is a bounded bit-field
// times a finite scale (SPEC.md §9.4), so only a hand-built PDO can supply one.
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
// exactly representable in binary floating point, so a decoded figure can land
// beside the value the source meant rather than on it: the 3.3 V floor of a PPS
// APDO arrives as field 66, and 0.05*66 evaluates to 3.3000000000000003, which
// prints as noise in the table and serialises as noise in --json. It is not
// about comparisons — every voltage test in this package carries cmpEps
// (evaluate.go) — but about the numbers handed out. Every decoded voltage and
// current goes through this.
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// floorToWireStep rounds a DERIVED current down to the 10 mA wire resolution.
//
// round2 is exact for anything decoded — every current field is an integer
// multiple of 10 or 50 mA — but the currents this package computes rather than
// reads are quotients: an EPR AVS budget's PDP/V, a power-limited PPS APDO's
// PDP/V. Rounding a quotient to nearest moves it UP by as much as 5 mA, and the
// result is not display-only: it is what the verdict compares the request
// against (evaluate.go, finish). A 140 W budget at 36 V is 3.8889 A and was
// answering "yes" to a request for 3.89 A. Small, and the wrong direction, which
// is the one the package undertakes never to take.
//
// The epsilon is load-bearing in the other direction: an exact two-decimal
// quotient is not always exactly representable — 23 W / 20 V evaluates to
// 1.1499999999999999 — and a bare floor would hand back a full 10 mA the source
// really does offer.
func floorToWireStep(a float64) float64 { return math.Floor(a*100+cmpEps) / 100 }

// SPRAVSBandSplitV is the boundary between the SPR AVS APDO's two current
// bands: the 9-15 V band and the 15-20 V band (USB-PD 3.2).
const SPRAVSBandSplitV = 15.0

// CurrentAt reports the current this PDO can supply at v volts, in amps.
//
// It is the only correct way to ask what a PDO offers, and every verdict in this
// package goes through it. Reading a value field instead is wrong for three of
// the six classes and dangerous for all of them:
//
//   - An SPR AVS APDO carries two band-specific limits and the applicable one
//     depends on v. MaxCurrentA is the larger, so an APDO offering 5.00 A below
//     15 V and 3.25 A above it would claim 5 A at 18 V — a 54% over-report in
//     the direction that damages hardware.
//   - An EPR AVS APDO carries no current at all, only a power budget
//     (SPEC.md §9.4); the current it can supply is PDP/V and falls as V rises.
//   - A PPS APDO that sets the Power Limited bit holds its Maximum Current only
//     where its power budget allows: a 45 W source advertising 3.3-11 V at 5 A
//     supplies 4.09 A at 11 V, not 5.
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
		return boundCurrent(floorToWireStep(float64(p.PDPWatts) / v))
	case KindPPS:
		// An APDO that sets the Power Limited bit does not hold its Maximum
		// Current across the range: what the source supplies at v is
		// min(maxI, PDP/v). The budget is inferred from the fixed PDOs rather
		// than scanned (applyPPSPowerBudget), so it is applied only where it
		// reduces the answer — it can never raise one — and every verdict it
		// decides says so (CaveatPPSPowerLimited). Where no budget could be
		// inferred the advertised figure stands and the disclosure carries the
		// whole weight.
		usable, declared = reportable(p.MaxCurrentA, p.DeclaredMaxCurrentA)
		if p.PPSPowerLimited && p.PPSBudgetW > 0 && v > 0 {
			if a := floorToWireStep(float64(p.PPSBudgetW) / v); a < usable {
				usable = a
			}
		}
		return usable, declared
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
