package pdo

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Hand-built PDO words.
//
// Every constant below was derived from the field layouts in SPEC.md §9.4 and
// cross-checked by decoding it back. Note that the value offered in the task
// brief for a 9 V 3 A fixed PDO, 0x0002D0BC, is wrong: its low ten bits are
// 0x0BC = 188, i.e. 1.88 A. 3 A is 300 = 0x12C, so the correct word is
// 0x0002D12C, used here (and tested explicitly in TestFixed9V3AWord).
// ---------------------------------------------------------------------------
const (
	fixed5V3A   uint32 = 0x0001912C
	fixed9V3A   uint32 = 0x0002D12C
	fixed9V15A  uint32 = 0x0002D096 // 9 V @ 1.50 A
	fixed12V3A  uint32 = 0x0003C12C
	fixed15V3A  uint32 = 0x0004B12C
	fixed20V5A  uint32 = 0x000641F4
	fixed28V5A  uint32 = 0x0008C1F4 // EPR
	fixed28V3A  uint32 = 0x0008C12C // EPR
	fixed48V5A  uint32 = 0x000F01F4 // EPR
	fixed33V3A  uint32 = 0x0001092C // 3.3 V - below the 5 V floor, invalid
	fixed9V0A   uint32 = 0x0002D000 // zero current, invalid
	battery     uint32 = 0x4F00F0B4 // 3-12 V, 45 W
	variablePDO uint32 = 0x8F01912C // 5-12 V, 3 A
	pps3311V3A  uint32 = 0xC0DC213C // PPS 3.3-11.0 V, 3.00 A
	pps3311V2A  uint32 = 0xC0DC2128 // PPS 3.3-11.0 V, 2.00 A
	pps3321V5A  uint32 = 0xC1A42164 // PPS 3.3-21.0 V, 5.00 A
	eprAVS140W  uint32 = 0xD230968C // EPR AVS 15.0-28.0 V, 140 W
	eprAVS84W   uint32 = 0xD2309654 // EPR AVS 15.0-28.0 V, 84 W  (3 A at 28 V)
	eprAVS240W  uint32 = 0xD3C096F0 // EPR AVS 15.0-48.0 V, 240 W (cable-capped)
	eprAVSBad   uint32 = 0xD2309600 // 15.0-28.0 V, 0 W -> invalid -> cable fault
	sprAVS      uint32 = 0xE00515F4 // 3.25 A @20 V, 5.00 A @15 V
	sprAVS2A    uint32 = 0xE00320C8 // 2.00 A at both points
	augReserved uint32 = 0xF0001234 // subtype 3
)

// Objects that advertise more current than any cable can carry. Every current
// field in USB-PD is wide enough to express one, so a malformed or hostile
// source can produce these and the decode must bound them (MaxCableCurrentA).
const (
	fixed9V1023A  uint32 = 0x0002D3FF // 9 V, field 1023 = 10.23 A
	fixed28V1023A uint32 = 0x0008C3FF // 28 V, 10.23 A - doubly impossible: EPR needs a 5 A cable
	variable1023A uint32 = 0x8F0193FF // 5-12 V, 10.23 A
	pps3311V635A  uint32 = 0xC0DC217F // PPS 3.3-11.0 V, field 127 = 6.35 A
	sprAVS1023A   uint32 = 0xE00517FF // 3.25 A @20 V, 10.23 A @15 V
)

// buildLog assembles a 90-byte little-endian blob.
func buildLog(target, measured uint16, n, sel uint8, flags, flags2 uint16, pdos ...uint32) []byte {
	b := make([]byte, LogBytes)
	binary.LittleEndian.PutUint16(b[0:2], target)
	binary.LittleEndian.PutUint16(b[2:4], measured)
	b[4] = n
	b[5] = sel
	binary.LittleEndian.PutUint16(b[6:8], flags)
	binary.LittleEndian.PutUint16(b[8:10], flags2)
	for i, p := range pdos {
		if i >= MaxPDOs {
			break
		}
		binary.LittleEndian.PutUint32(b[HeaderBytes+i*4:HeaderBytes+i*4+4], p)
	}
	return b
}

// simpleLog builds a log declaring exactly the PDOs given.
func simpleLog(t *testing.T, pdos ...uint32) *Log {
	t.Helper()
	l, err := Parse(buildLog(9000, 9010, uint8(len(pdos)), 1, 0, 0, pdos...))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return l
}

func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func mustPDO(t *testing.T, l *Log, i int) PDO {
	t.Helper()
	if i >= len(l.PDOs) {
		t.Fatalf("want at least %d PDOs, got %d", i+1, len(l.PDOs))
	}
	return l.PDOs[i]
}

// ---------------------------------------------------------------------------
// Header and framing
// ---------------------------------------------------------------------------

func TestParseHeaderLittleEndian(t *testing.T) {
	// 0x2EE0 = 12000 mV. Written little-endian, a big-endian reader would see
	// 0xE02E = 57390, so this pins the endianness (SPEC.md §9.3).
	b := buildLog(12000, 11980, 1, 2, 0x1234, 0x0000, fixed12V3A)
	if b[0] != 0xE0 || b[1] != 0x2E {
		t.Fatalf("test blob is not little-endian: % X", b[0:2])
	}
	l, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if l.TargetVoltageMv != 12000 {
		t.Errorf("TargetVoltageMv = %d, want 12000", l.TargetVoltageMv)
	}
	if l.MeasuredVoltageMv != 11980 {
		t.Errorf("MeasuredVoltageMv = %d, want 11980", l.MeasuredVoltageMv)
	}
	if l.NPDOsReceived != 1 {
		t.Errorf("NPDOsReceived = %d, want 1", l.NPDOsReceived)
	}
	if l.SelectedPDOID != 2 {
		t.Errorf("SelectedPDOID = %d, want 2", l.SelectedPDOID)
	}
	if l.Flags != 0x1234 {
		t.Errorf("Flags = 0x%04X, want 0x1234", l.Flags)
	}
	if l.EPRCableFail {
		t.Error("EPRCableFail set with flags2 == 0 and no bad EPR AVS")
	}
}

func TestParseShortBlob(t *testing.T) {
	for _, n := range []int{0, 1, 89} {
		_, err := Parse(make([]byte, n))
		if !errors.Is(err, ErrShortLog) {
			t.Errorf("Parse(%d bytes) error = %v, want ErrShortLog", n, err)
		}
		if err != nil && !strings.Contains(err.Error(), "≥90") {
			t.Errorf("Parse(%d bytes) message %q does not reproduce SPEC.md §9.6", n, err)
		}
	}
}

func TestParseAllZeroBlob(t *testing.T) {
	// SPEC.md §9.6: an all-zero blob means the unit never saw a charger.
	_, err := Parse(make([]byte, LogBytes))
	if !errors.Is(err, ErrEmptyLog) {
		t.Fatalf("Parse(zeroes) error = %v, want ErrEmptyLog", err)
	}
	if !strings.Contains(err.Error(), "No PDO data captured") {
		t.Errorf("message %q does not reproduce the vendor string", err)
	}
	// 96 bytes of zeroes (the raw chunk concatenation) must fail the same way.
	if _, err := Parse(make([]byte, 96)); !errors.Is(err, ErrEmptyLog) {
		t.Errorf("Parse(96 zeroes) error = %v, want ErrEmptyLog", err)
	}
}

func TestParseAcceptsOverlongBlob(t *testing.T) {
	// The download concatenates twelve 8-byte chunks = 96 bytes; the tail is
	// padding and must be ignored, not rejected.
	b := append(buildLog(5000, 5000, 1, 1, 0, 0, fixed5V3A), 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
	l, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(l.PDOs) != 1 {
		t.Fatalf("got %d PDOs, want 1", len(l.PDOs))
	}
}

func TestNPDOsBounding(t *testing.T) {
	// nPdosReceived is a byte; a corrupt value must not read past the array.
	all := make([]uint32, MaxPDOs)
	for i := range all {
		all[i] = fixed5V3A
	}
	b := buildLog(5000, 5000, 255, 1, 0, 0, all...)
	l, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(l.PDOs) != MaxPDOs {
		t.Errorf("got %d PDOs, want %d", len(l.PDOs), MaxPDOs)
	}

	// And a count lower than the number of words present truncates.
	b = buildLog(5000, 5000, 2, 1, 0, 0, fixed5V3A, fixed9V3A, fixed12V3A, fixed15V3A, fixed20V5A)
	l, err = Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(l.PDOs) != 2 {
		t.Fatalf("got %d PDOs, want 2", len(l.PDOs))
	}
	if !nearly(l.PDOs[1].VoltageV, 9) {
		t.Errorf("second PDO = %v, want 9 V", l.PDOs[1].VoltageV)
	}
}

func TestZeroWordsSkipped(t *testing.T) {
	l, err := Parse(buildLog(9000, 9000, 4, 1, 0, 0, fixed5V3A, 0, fixed9V3A, 0))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(l.PDOs) != 2 {
		t.Fatalf("got %d PDOs, want 2", len(l.PDOs))
	}
	// Index is the slot, not the position in the returned slice.
	if l.PDOs[0].Index != 0 || l.PDOs[1].Index != 2 {
		t.Errorf("indices = %d,%d, want 0,2", l.PDOs[0].Index, l.PDOs[1].Index)
	}
}

// ---------------------------------------------------------------------------
// Fixed PDOs
// ---------------------------------------------------------------------------

func TestFixed9V3AWord(t *testing.T) {
	// Verify the arithmetic of the constant itself, and record that the brief's
	// 0x0002D0BC is a different (1.88 A) PDO.
	if got := (fixed9V3A >> 10) & 0x3FF; got != 180 {
		t.Errorf("voltage field = %d, want 180 (9 V / 50 mV)", got)
	}
	if got := fixed9V3A & 0x3FF; got != 300 {
		t.Errorf("current field = %d, want 300 (3 A / 10 mA)", got)
	}
	const brief uint32 = 0x0002D0BC
	if brief&0x3FF == 300 {
		t.Fatal("0x0002D0BC unexpectedly encodes 3 A")
	}
	if got := 0.01 * float64(brief&0x3FF); !nearly(got, 1.88) {
		t.Errorf("0x0002D0BC current = %v A, want 1.88", got)
	}
}

func TestDecodeFixed(t *testing.T) {
	tests := []struct {
		name    string
		raw     uint32
		voltage float64
		current float64
		valid   bool
		epr     bool
	}{
		{"5V 3A", fixed5V3A, 5, 3, true, false},
		{"9V 3A", fixed9V3A, 9, 3, true, false},
		{"9V 1.5A", fixed9V15A, 9, 1.5, true, false},
		{"12V 3A", fixed12V3A, 12, 3, true, false},
		{"15V 3A", fixed15V3A, 15, 3, true, false},
		{"20V 5A is still SPR", fixed20V5A, 20, 5, true, false},
		{"28V 5A is EPR", fixed28V5A, 28, 5, true, true},
		{"48V 5A is EPR", fixed48V5A, 48, 5, true, true},
		{"3.3V below the 5V floor", fixed33V3A, 3.3, 3, false, false},
		{"9V zero current", fixed9V0A, 9, 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := decodePDO(0, tt.raw)
			if p.Kind != KindFixed {
				t.Fatalf("Kind = %v, want fixed", p.Kind)
			}
			if !nearly(p.VoltageV, tt.voltage) {
				t.Errorf("VoltageV = %v, want %v", p.VoltageV, tt.voltage)
			}
			if !nearly(p.MaxCurrentA, tt.current) {
				t.Errorf("MaxCurrentA = %v, want %v", p.MaxCurrentA, tt.current)
			}
			if p.Valid != tt.valid {
				t.Errorf("Valid = %v, want %v", p.Valid, tt.valid)
			}
			if p.EPR != tt.epr {
				t.Errorf("EPR = %v, want %v", p.EPR, tt.epr)
			}
			if p.Raw != tt.raw {
				t.Errorf("Raw = 0x%08X, want 0x%08X", p.Raw, tt.raw)
			}
		})
	}
}

func TestRoundingToTwoDecimals(t *testing.T) {
	// 0.05*66 is 3.3000000000000003 in binary floating point; without round2
	// that is the figure the PPS APDO below hands out, and it prints and
	// serialises as noise. The fixed PDO is the control: 0.05*180 happens to be
	// exactly 9, so that assertion holds with or without the rounding and only
	// the PPS one below can fail.
	p := decodePDO(0, fixed9V3A)
	if p.VoltageV != 9.0 {
		t.Errorf("VoltageV = %.20g, want exactly 9", p.VoltageV)
	}
	q := decodePDO(0, pps3311V3A)
	if q.MinVoltageV != 3.3 {
		t.Errorf("MinVoltageV = %.20g, want exactly 3.3", q.MinVoltageV)
	}
}

// ---------------------------------------------------------------------------
// Battery and Variable - decoded here, discarded by the vendor app
// ---------------------------------------------------------------------------

func TestDecodeBattery(t *testing.T) {
	p := decodePDO(3, battery)
	if p.Kind != KindBattery {
		t.Fatalf("Kind = %v, want battery", p.Kind)
	}
	if !nearly(p.MinVoltageV, 3) || !nearly(p.MaxVoltageV, 12) {
		t.Errorf("range = %v-%v V, want 3-12", p.MinVoltageV, p.MaxVoltageV)
	}
	if !nearly(p.MaxPowerW, 45) {
		t.Errorf("MaxPowerW = %v, want 45", p.MaxPowerW)
	}
	if p.PDPWatts != 0 {
		t.Errorf("PDPWatts = %d, want 0 (whole-watt field is EPR AVS only)", p.PDPWatts)
	}
	if !p.Valid || p.EPR {
		t.Errorf("Valid=%v EPR=%v, want true/false", p.Valid, p.EPR)
	}

	// Zero power fails validation.
	if q := decodePDO(0, battery&^uint32(0x3FF)); q.Valid {
		t.Error("battery with 0 W is valid")
	}
}

func TestDecodeVariable(t *testing.T) {
	p := decodePDO(4, variablePDO)
	if p.Kind != KindVariable {
		t.Fatalf("Kind = %v, want variable", p.Kind)
	}
	if !nearly(p.MinVoltageV, 5) || !nearly(p.MaxVoltageV, 12) {
		t.Errorf("range = %v-%v V, want 5-12", p.MinVoltageV, p.MaxVoltageV)
	}
	if !nearly(p.MaxCurrentA, 3) {
		t.Errorf("MaxCurrentA = %v, want 3", p.MaxCurrentA)
	}
	if !p.Valid || p.EPR {
		t.Errorf("Valid=%v EPR=%v, want true/false", p.Valid, p.EPR)
	}

	// A variable supply reaching above 20 V is grouped as EPR.
	// 29:20 = 48/0.05 = 960 = 0x3C0, 19:10 = 100 (5 V), 9:0 = 300 (3 A).
	hi := uint32(2)<<30 | 0x3C0<<20 | 100<<10 | 300
	if q := decodePDO(0, hi); !q.EPR || !nearly(q.MaxVoltageV, 48) {
		t.Errorf("48 V variable: EPR=%v max=%v, want true/48", q.EPR, q.MaxVoltageV)
	}
}

// ---------------------------------------------------------------------------
// Augmented PDOs
// ---------------------------------------------------------------------------

func TestDecodePPS(t *testing.T) {
	p := decodePDO(5, pps3311V3A)
	if p.Kind != KindPPS {
		t.Fatalf("Kind = %v, want pps", p.Kind)
	}
	if !nearly(p.MinVoltageV, 3.3) || !nearly(p.MaxVoltageV, 11) {
		t.Errorf("range = %v-%v V, want 3.3-11", p.MinVoltageV, p.MaxVoltageV)
	}
	if !nearly(p.MaxCurrentA, 3) {
		t.Errorf("MaxCurrentA = %v, want 3 (60 units x 50 mA)", p.MaxCurrentA)
	}
	if !p.Valid {
		t.Error("Valid = false")
	}
	if p.EPR {
		t.Error("EPR = true; PPS is SPR-only (SPEC.md §9.4)")
	}

	// The 8-bit max-voltage field tops out at 25.5 V; 21 V is the real limit
	// for PPS and must decode cleanly.
	q := decodePDO(0, pps3321V5A)
	if !nearly(q.MaxVoltageV, 21) || !nearly(q.MaxCurrentA, 5) {
		t.Errorf("pps3321V5A = %v-%v V %v A, want 3.3-21.0 V 5 A", q.MinVoltageV, q.MaxVoltageV, q.MaxCurrentA)
	}
}

func TestDecodePPSValidity(t *testing.T) {
	tests := []struct {
		name  string
		raw   uint32
		valid bool
	}{
		{"ok", pps3311V3A, true},
		{"zero max voltage", uint32(3)<<30 | 33<<8 | 60, false},
		{"zero min voltage", uint32(3)<<30 | 110<<17 | 60, false},
		{"max below min", uint32(3)<<30 | 33<<17 | 110<<8 | 60, false},
		{"zero current", uint32(3)<<30 | 110<<17 | 33<<8, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if p := decodePDO(0, tt.raw); p.Valid != tt.valid {
				t.Errorf("Valid = %v, want %v (%+v)", p.Valid, tt.valid, p)
			}
		})
	}
}

func TestDecodeEPRAVS(t *testing.T) {
	p := decodePDO(6, eprAVS140W)
	if p.Kind != KindEPRAVS {
		t.Fatalf("Kind = %v, want epr_avs", p.Kind)
	}
	if !nearly(p.MinVoltageV, 15) || !nearly(p.MaxVoltageV, 28) {
		t.Errorf("range = %v-%v V, want 15-28", p.MinVoltageV, p.MaxVoltageV)
	}
	if p.PDPWatts != 140 {
		t.Errorf("PDPWatts = %d, want 140", p.PDPWatts)
	}
	if !nearly(p.MaxPowerW, 140) {
		t.Errorf("MaxPowerW = %v, want 140", p.MaxPowerW)
	}
	if !p.Valid || !p.EPR {
		t.Errorf("Valid=%v EPR=%v, want true/true", p.Valid, p.EPR)
	}

	// The 9-bit max-voltage field must reach 48 V (0x1E0 = 480 x 0.1).
	hi := uint32(3)<<30 | uint32(1)<<28 | 480<<17 | 150<<8 | 240
	if q := decodePDO(0, hi); !nearly(q.MaxVoltageV, 48) {
		t.Errorf("MaxVoltageV = %v, want 48 (9-bit field)", q.MaxVoltageV)
	}
}

func TestEPRAVSInvalidPromotesCableFail(t *testing.T) {
	// SPEC.md §9.4: an EPR_AVS APDO that fails validation is itself a report of
	// an EPR cable fault, independent of flags2 bit 3.
	l, err := Parse(buildLog(28000, 0, 2, 1, 0, 0x0000, fixed5V3A, eprAVSBad))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if l.Flags2&Flag2EPRCableFail != 0 {
		t.Fatal("test blob accidentally set flags2 bit 3")
	}
	if !l.EPRCableFail {
		t.Error("EPRCableFail = false; an invalid EPR AVS APDO must promote it")
	}
	if p := mustPDO(t, l, 1); p.Valid {
		t.Error("the 0 W EPR AVS APDO is marked valid")
	}

	// A valid one must not set it.
	l2, err := Parse(buildLog(28000, 0, 2, 1, 0, 0, fixed5V3A, eprAVS140W))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if l2.EPRCableFail {
		t.Error("EPRCableFail = true for a valid EPR AVS APDO")
	}
}

func TestFlags2CableFailBit(t *testing.T) {
	l, err := Parse(buildLog(28000, 0, 1, 1, 0, Flag2EPRCableFail, fixed5V3A))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !l.EPRCableFail {
		t.Error("EPRCableFail = false with flags2 bit 3 set")
	}
	// Neighbouring bits must not trip it.
	l2, _ := Parse(buildLog(28000, 0, 1, 1, 0, 0x0004|0x0010, fixed5V3A))
	if l2.EPRCableFail {
		t.Error("EPRCableFail set by bits other than 3")
	}
}

func TestDecodeSPRAVS(t *testing.T) {
	p := decodePDO(7, sprAVS)
	if p.Kind != KindSPRAVS {
		t.Fatalf("Kind = %v, want spr_avs", p.Kind)
	}
	if !nearly(p.MaxCurrent20VA, 3.25) {
		t.Errorf("MaxCurrent20VA = %v, want 3.25", p.MaxCurrent20VA)
	}
	if !nearly(p.MaxCurrent15VA, 5) {
		t.Errorf("MaxCurrent15VA = %v, want 5", p.MaxCurrent15VA)
	}
	// MaxCurrentA is only the summary aggregate for tables and ranking; a
	// verdict at a specific voltage goes through CurrentAt, which is
	// band-aware. See TestSPRAVSFieldOrderIsLoadBearing for the ordering.
	if !nearly(p.MaxCurrentA, 5) {
		t.Errorf("MaxCurrentA = %v, want 5 (max of the two)", p.MaxCurrentA)
	}
	if !p.Valid || p.EPR {
		t.Errorf("Valid=%v EPR=%v, want true/false", p.Valid, p.EPR)
	}
}

// TestSPRAVSFieldOrderIsLoadBearing pins the SPR AVS band-to-bit assignment.
//
// This test replaces TestSPRAVSFieldOrderIsSwapSafe, which asserted the
// opposite: that swapping the two current fields "does not change any outcome"
// because every consumer took max() of the pair. That claim was deliberately
// abandoned when PDO.CurrentAt became band-aware (SPEC.md §17): reporting the
// strong band's current in the weak band over-states what the source can
// deliver, which is the direction that damages hardware. The ordering now
// matches published USB-PD 3.2 — bits 19:10 bound the 15-20 V band, bits 9:0
// the 9-15 V band (SPEC.md §17, "Resolved") — and a reorder of the decode
// WOULD change verdicts, so this test exists to make that reorder fail.
func TestSPRAVSFieldOrderIsLoadBearing(t *testing.T) {
	// sprAVS: 3.25 A in bits 19:10 (15-20 V band), 5.00 A in bits 9:0
	// (9-15 V band).
	p := decodePDO(0, sprAVS)
	if !nearly(p.MaxCurrent20VA, 3.25) || !nearly(p.MaxCurrent15VA, 5) {
		t.Fatalf("decode: 20V band = %v, 15V band = %v; want 3.25 / 5", p.MaxCurrent20VA, p.MaxCurrent15VA)
	}
	// The verdict path must use the band the requested voltage falls in.
	// A swapped decode would invert these two assertions.
	if got := p.CurrentAt(18); !nearly(got, 3.25) {
		t.Errorf("CurrentAt(18 V) = %v, want 3.25 (upper band); a field swap over-reports by 54%%", got)
	}
	if got := p.CurrentAt(12); !nearly(got, 5) {
		t.Errorf("CurrentAt(12 V) = %v, want 5.00 (lower band)", got)
	}
	// And a genuinely swapped word must produce the mirrored verdicts, so the
	// decode cannot be "fixed" back to swap-neutrality without failing here.
	swapped := uint32(3)<<30 | uint32(2)<<28 | 500<<10 | 325
	q := decodePDO(0, swapped)
	if got := q.CurrentAt(18); !nearly(got, 5) {
		t.Errorf("swapped word CurrentAt(18 V) = %v, want 5.00", got)
	}
}

func TestDecodeSPRAVSValidity(t *testing.T) {
	both0 := uint32(3)<<30 | uint32(2)<<28
	if p := decodePDO(0, both0); p.Valid {
		t.Error("SPR AVS with both currents zero is valid")
	}
	only15 := uint32(3)<<30 | uint32(2)<<28 | 300
	if p := decodePDO(0, only15); !p.Valid {
		t.Error("SPR AVS with only the 15 V current set is invalid")
	}
	only20 := uint32(3)<<30 | uint32(2)<<28 | 300<<10
	if p := decodePDO(0, only20); !p.Valid {
		t.Error("SPR AVS with only the 20 V current set is invalid")
	}
}

func TestDecodeAugmentedReserved(t *testing.T) {
	p := decodePDO(8, augReserved)
	if p.Kind != KindUnknown {
		t.Fatalf("Kind = %v, want unknown", p.Kind)
	}
	if p.Valid {
		t.Error("Valid = true for a reserved APDO subtype")
	}
	if p.Raw != augReserved {
		t.Errorf("Raw = 0x%08X, want 0x%08X", p.Raw, augReserved)
	}
	// A reserved APDO must not be mistaken for an EPR AVS cable fault.
	l := simpleLog(t, augReserved)
	if l.EPRCableFail {
		t.Error("EPRCableFail promoted by a reserved APDO subtype")
	}
}

func TestEPRAVSValidityMatrix(t *testing.T) {
	base := func(maxV, minV, pdp uint32) uint32 {
		return uint32(3)<<30 | uint32(1)<<28 | maxV<<17 | minV<<8 | pdp
	}
	tests := []struct {
		name  string
		raw   uint32
		valid bool
	}{
		{"ok", base(280, 150, 140), true},
		{"zero pdp", base(280, 150, 0), false},
		{"zero min", base(280, 0, 140), false},
		{"zero max", base(0, 150, 140), false},
		{"max below min", base(150, 200, 140), false}, // 15.0 V max, 20.0 V min
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if p := decodePDO(0, tt.raw); p.Valid != tt.valid {
				t.Errorf("Valid = %v, want %v (%+v)", p.Valid, tt.valid, p)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The cable current ceiling
// ---------------------------------------------------------------------------

// TestDecodeBoundsCurrentByCable is the decode half of the regression: no class
// may store a current above MaxCableCurrentA, and every class has a field wide
// enough to express one. Before the fix only the EPR AVS power budget was
// bounded, so a fixed PDO whose ten-bit current field held 1023 decoded to
// 10.23 A and that figure reached Match.MaxCurrentA and the summary table
// untouched.
func TestDecodeBoundsCurrentByCable(t *testing.T) {
	tests := []struct {
		name     string
		raw      uint32
		kind     Kind
		declared float64
	}{
		{"fixed 9 V", fixed9V1023A, KindFixed, 10.23},
		{"fixed 28 V", fixed28V1023A, KindFixed, 10.23},
		{"variable", variable1023A, KindVariable, 10.23},
		{"pps", pps3311V635A, KindPPS, 6.35},
		{"spr avs", sprAVS1023A, KindSPRAVS, 10.23},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := decodePDO(0, tt.raw)
			if p.Kind != tt.kind {
				t.Fatalf("Kind = %v, want %v", p.Kind, tt.kind)
			}
			if !nearly(p.MaxCurrentA, MaxCableCurrentA) {
				t.Errorf("MaxCurrentA = %v, want %v (bounded)", p.MaxCurrentA, MaxCableCurrentA)
			}
			if !nearly(p.DeclaredMaxCurrentA, tt.declared) {
				t.Errorf("DeclaredMaxCurrentA = %v, want %v (the claim must not be lost)",
					p.DeclaredMaxCurrentA, tt.declared)
			}
			if !p.CableBound() {
				t.Error("CableBound() = false")
			}
			if !p.Valid {
				t.Error("Valid = false; bounding must not invalidate an otherwise sound PDO")
			}
			// Whatever the class, asking what it supplies must not exceed the
			// ceiling at any voltage.
			for _, v := range []float64{3.3, 5, 9, 12, 15, 15.1, 18, 20, 28, 48} {
				if a := p.CurrentAt(v); a > MaxCableCurrentA+1e-9 {
					t.Errorf("CurrentAt(%v) = %v, above the %v A ceiling", v, a, MaxCableCurrentA)
				}
			}
		})
	}
}

// TestSPRAVSBandsBoundedIndependently pins that the two band limits are bounded
// one at a time, so a monstrous figure in one band cannot raise the other and
// the per-band claim is still recorded.
func TestSPRAVSBandsBoundedIndependently(t *testing.T) {
	p := decodePDO(0, sprAVS1023A)
	if !nearly(p.MaxCurrent15VA, 5) {
		t.Errorf("MaxCurrent15VA = %v, want 5 (bounded from 10.23)", p.MaxCurrent15VA)
	}
	if !nearly(p.DeclaredMaxCurrent15VA, 10.23) {
		t.Errorf("DeclaredMaxCurrent15VA = %v, want 10.23", p.DeclaredMaxCurrent15VA)
	}
	if !nearly(p.MaxCurrent20VA, 3.25) {
		t.Errorf("MaxCurrent20VA = %v, want 3.25 (untouched)", p.MaxCurrent20VA)
	}
	if p.DeclaredMaxCurrent20VA != 0 {
		t.Errorf("DeclaredMaxCurrent20VA = %v, want 0 (that band was not bounded)", p.DeclaredMaxCurrent20VA)
	}
	// The band split still decides which limit applies.
	if a := p.CurrentAt(12); !nearly(a, 5) {
		t.Errorf("CurrentAt(12) = %v, want 5", a)
	}
	if a := p.CurrentAt(18); !nearly(a, 3.25) {
		t.Errorf("CurrentAt(18) = %v, want 3.25", a)
	}
}

// TestBoundCurrentUnaffectedBelowCeiling guards against the bound quietly
// changing an ordinary value: under-reporting is safe but still wrong.
func TestBoundCurrentUnaffectedBelowCeiling(t *testing.T) {
	for _, raw := range []uint32{fixed5V3A, fixed9V3A, fixed20V5A, fixed28V5A, pps3311V3A, sprAVS} {
		p := decodePDO(0, raw)
		if p.CableBound() {
			t.Errorf("0x%08X reported as bounded (declared %v)", raw, p.DeclaredMaxCurrentA)
		}
		if p.DeclaredMaxCurrentA != 0 {
			t.Errorf("0x%08X: DeclaredMaxCurrentA = %v, want 0 when nothing was bounded", raw, p.DeclaredMaxCurrentA)
		}
	}
	// A PDO built by hand, bypassing decodePDO, must not escape the ceiling
	// either: CurrentAt re-applies it (see reportable).
	hand := PDO{Kind: KindFixed, VoltageV: 9, MaxCurrentA: 42}
	if a := hand.CurrentAt(9); !nearly(a, MaxCableCurrentA) {
		t.Errorf("hand-built PDO CurrentAt = %v, want %v", a, MaxCableCurrentA)
	}
}

// ---------------------------------------------------------------------------
// Kind naming
// ---------------------------------------------------------------------------

func TestKindString(t *testing.T) {
	want := map[Kind]string{
		KindFixed: "fixed", KindBattery: "battery", KindVariable: "variable",
		KindPPS: "pps", KindEPRAVS: "epr_avs", KindSPRAVS: "spr_avs", KindUnknown: "unknown",
	}
	for k, s := range want {
		if k.String() != s {
			t.Errorf("Kind(%d).String() = %q, want %q", int(k), k.String(), s)
		}
		if k.Label() == "" {
			t.Errorf("Kind(%d).Label() is empty", int(k))
		}
	}
	if got := Kind(99).String(); got != "kind(99)" {
		t.Errorf("Kind(99).String() = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Negotiation flags
// ---------------------------------------------------------------------------

// TestStatusDecodesEveryNamedBitExactlyOnce walks the flag vocabulary itself:
// for each bit, a log with only that bit set must light exactly that field and
// name exactly that label.
//
// Driving the test from the same table the code uses would be circular for the
// mask values, and is not for the wiring: the table maps a mask to a field
// accessor and a label, and the failure this catches is two entries pointing at
// one field -- a copy-paste that silently drops a bit and doubles another.
// Nothing outside the table could notice that, and eighteen hand-written cases
// would be the same copy-paste with a second chance to make it.
func TestStatusDecodesEveryNamedBitExactlyOnce(t *testing.T) {
	for _, group := range []struct {
		name  string
		bits  []statusBit
		build func(mask uint16) []byte
		label func(uint16) []string
	}{
		{"flags", flagBits,
			func(m uint16) []byte { return buildLog(5000, 5000, 1, 1, m, 0, fixed5V3A) },
			FlagLabels},
		{"flags2", flag2Bits,
			func(m uint16) []byte { return buildLog(5000, 5000, 1, 1, 0, m, fixed5V3A) },
			Flag2Labels},
	} {
		for _, b := range group.bits {
			t.Run(group.name+"/"+b.label, func(t *testing.T) {
				l, err := Parse(group.build(b.mask))
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if !*b.field(&l.Status) {
					t.Errorf("%s bit %#04x (%s) did not set its field", group.name, b.mask, b.label)
				}
				// Exactly one field, so no two entries share an accessor.
				var lit int
				for _, other := range append(append([]statusBit{}, flagBits...), flag2Bits...) {
					if *other.field(&l.Status) {
						lit++
					}
				}
				if lit != 1 {
					t.Errorf("%s bit %#04x (%s) set %d fields, want exactly 1", group.name, b.mask, b.label, lit)
				}
				if got := group.label(b.mask); len(got) != 1 || got[0] != b.label {
					t.Errorf("labels for %#04x = %v, want [%q]", b.mask, got, b.label)
				}
			})
		}
	}
}

// A bit nobody has a name for is reported, not dropped. Two of the sixteen bits
// of flags2 and fourteen of flags are unaccounted for, and a firmware that
// starts setting one is exactly the thing this tool should show rather than
// quietly discard.
func TestFlagLabelsReportUnnamedBits(t *testing.T) {
	got := Flag2Labels(Flag2EPRCableFail | 0x4000)
	want := []string{"epr cable fail", "unknown 0x4000"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Flag2Labels = %v, want %v", got, want)
	}
	if labels := FlagLabels(0); len(labels) != 0 {
		t.Errorf("FlagLabels(0) = %v, want nothing", labels)
	}
}

// Log.EPRCableFail is the union of the flags2 bit and the computed source (an
// EPR AVS APDO that failed validation); Status.EPRCableFail is the bit alone.
// They disagree on purpose exactly when the word is silent and the APDO is not,
// and a reader of either needs the other to stay put.
func TestStatusEPRCableFailIsTheBitAloneNotTheUnion(t *testing.T) {
	// eprAVSBad declares 0 W, so it fails validation; the flag word is clear.
	l, err := Parse(buildLog(28000, 0, 2, 1, 0, 0, fixed5V3A, eprAVSBad))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !l.EPRCableFail {
		t.Error("Log.EPRCableFail is false; a failed EPR AVS APDO is its second source (SPEC.md §9.4)")
	}
	if l.Status.EPRCableFail {
		t.Error("Status.EPRCableFail is true with flags2 bit 3 clear; it must report the bit, not the union")
	}
}

// ---------------------------------------------------------------------------
// Fixed PDO capability bits
// ---------------------------------------------------------------------------

// EPR-capable and peak current are decoded for Fixed PDOs and are nil for every
// other class, where those bit positions mean something else entirely. nil
// rather than false is the point: "not EPR capable" and "this class has no such
// bit" are different statements.
func TestFixedPDOCapabilityBits(t *testing.T) {
	// Bit 23 and bits 21:20, laid on the 20 V 5 A fixed word above. Both sit
	// clear of the voltage (19:10) and current (9:0) fields, so the decoded
	// figures must not move either.
	const eprCapable, peakLevel2 uint32 = 1 << 23, 2 << 20

	t.Run("fixed carries both", func(t *testing.T) {
		l := simpleLog(t, fixed20V5A|eprCapable|peakLevel2)
		p := l.PDOs[0]
		if !nearly(p.VoltageV, 20) || !nearly(p.MaxCurrentA, 5) {
			t.Errorf("capability bits disturbed the value fields: %v V, %v A", p.VoltageV, p.MaxCurrentA)
		}
		if p.EPRCapable == nil || !*p.EPRCapable {
			t.Errorf("EPRCapable = %v, want true for a PDO with bit 23 set", p.EPRCapable)
		}
		if p.PeakCurrent == nil || *p.PeakCurrent != 2 {
			t.Errorf("PeakCurrent = %v, want 2 from bits 21:20", p.PeakCurrent)
		}
	})
	t.Run("fixed without the bits says so", func(t *testing.T) {
		p := simpleLog(t, fixed20V5A).PDOs[0]
		if p.EPRCapable == nil || *p.EPRCapable {
			t.Errorf("EPRCapable = %v, want a non-nil false: the object carries the bit and it is clear", p.EPRCapable)
		}
		if p.PeakCurrent == nil || *p.PeakCurrent != 0 {
			t.Errorf("PeakCurrent = %v, want a non-nil 0", p.PeakCurrent)
		}
	})
	t.Run("other classes carry neither", func(t *testing.T) {
		for _, raw := range []uint32{pps3321V5A, battery, eprAVS140W, sprAVS} {
			p := simpleLog(t, raw).PDOs[0]
			if p.EPRCapable != nil || p.PeakCurrent != nil {
				t.Errorf("%s PDO reports epr_capable=%v peak_current=%v; those bits are not its fields",
					p.Kind, p.EPRCapable, p.PeakCurrent)
			}
		}
	})
}
