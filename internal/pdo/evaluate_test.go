package pdo

import (
	"strings"
	"testing"
)

// checkMatch asserts the whole Match at once.
func checkMatch(t *testing.T, got Match, wantOK bool, wantMode string, wantA float64) {
	t.Helper()
	if got.OK != wantOK {
		t.Errorf("OK = %v, want %v (messages: %v)", got.OK, wantOK, got.Messages)
	}
	if got.Mode != wantMode {
		t.Errorf("Mode = %q, want %q (messages: %v)", got.Mode, wantMode, got.Messages)
	}
	if !nearly(got.MaxCurrentA, wantA) {
		t.Errorf("MaxCurrentA = %v, want %v (messages: %v)", got.MaxCurrentA, wantA, got.Messages)
	}
}

// hasMessage reports whether any message contains sub.
func hasMessage(m Match, sub string) bool {
	for _, s := range m.Messages {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func wantMessage(t *testing.T, m Match, sub string) {
	t.Helper()
	if !hasMessage(m, sub) {
		t.Errorf("no message containing %q; got %v", sub, m.Messages)
	}
}

func hasCaveat(m Match, tag string) bool {
	for _, c := range m.Caveats {
		if c == tag {
			return true
		}
	}
	return false
}

func wantCaveat(t *testing.T, m Match, tag string) {
	t.Helper()
	if !hasCaveat(m, tag) {
		t.Errorf("caveat %q missing; got %v (messages: %v)", tag, m.Caveats, m.Messages)
	}
}

func wantNoCaveat(t *testing.T, m Match, tag string) {
	t.Helper()
	if hasCaveat(m, tag) {
		t.Errorf("caveat %q present but not warranted; got %v (messages: %v)", tag, m.Caveats, m.Messages)
	}
}

// ---------------------------------------------------------------------------
// SPR: fixed
// ---------------------------------------------------------------------------

func TestEvaluateSPRFixed(t *testing.T) {
	// An ordinary 60 W charger: 5/9/12/15/20 V fixed plus a 3 A PPS range.
	l := simpleLog(t, fixed5V3A, fixed9V3A, fixed12V3A, fixed15V3A, fixed20V5A, pps3311V3A)

	m := l.Evaluate(9, 2)
	checkMatch(t, m, true, ModeSPRFixed, 3)
	wantMessage(t, m, "fixed 9 V PDO offers 3.00 A")

	// 20 V is the SPR/EPR boundary and stays in the SPR branch.
	checkMatch(t, l.Evaluate(20, 5), true, ModeSPRFixed, 5)
}

func TestEvaluateSPRFixedCurrentShort(t *testing.T) {
	// Fixed 9 V offers 3 A, PPS also offers only 3 A, so there is nothing to
	// upgrade to: the mode stands but the request is not achievable.
	l := simpleLog(t, fixed9V3A, pps3311V3A)
	m := l.Evaluate(9, 3.5)
	checkMatch(t, m, false, ModeSPRFixed, 3)
	if !m.CurrentShort() {
		t.Error("CurrentShort() = false; voltage was reachable")
	}
	wantMessage(t, m, "requested 3.50 A exceeds the 3.00 A available at 9 V")
}

func TestEvaluateSPRUpgradeToPPS(t *testing.T) {
	// Fixed 9 V can only do 1.5 A; PPS covers 9 V at 3 A and the request needs
	// more than the fixed object can give, so the PPS upgrade fires.
	l := simpleLog(t, fixed5V3A, fixed9V15A, pps3311V3A)
	m := l.Evaluate(9, 2)
	checkMatch(t, m, true, ModeUpgradeSPRPPSMoreCurrent, 3)
	wantMessage(t, m, "PPS 3.3-11.0 V supplies 3.00 A")

	// The same source with a request the fixed PDO already satisfies must NOT
	// upgrade: SPEC.md §9.5 gates the upgrade on fixed < I.
	checkMatch(t, l.Evaluate(9, 1), true, ModeSPRFixed, 1.5)

	// And when PPS offers no more than fixed, no upgrade either.
	l2 := simpleLog(t, fixed9V3A, pps3311V2A)
	checkMatch(t, l2.Evaluate(9, 5), false, ModeSPRFixed, 3)
}

// ---------------------------------------------------------------------------
// SPR: PPS and SPR AVS
// ---------------------------------------------------------------------------

func TestEvaluateSPRPPSOnly(t *testing.T) {
	// No fixed 9 V object at all: PPS carries the request.
	l := simpleLog(t, fixed5V3A, pps3311V3A)
	m := l.Evaluate(9, 2)
	checkMatch(t, m, true, ModeSPRPPS, 3)
	wantMessage(t, m, "no fixed 9 V PDO offered")

	// A non-standard voltage inside the PPS range works too, and says why no
	// fixed object was even looked for.
	m = l.Evaluate(7.4, 1)
	checkMatch(t, m, true, ModeSPRPPS, 3)
	if hasMessage(m, "no fixed 7.4 V PDO offered") {
		t.Errorf("7.4 V is not a standard fixed voltage; message is misleading: %v", m.Messages)
	}
}

func TestEvaluateSPRUpgradeToAVS(t *testing.T) {
	// PPS fits at 2 A; SPR AVS offers 5 A. SPEC.md §9.5 upgrades unconditionally
	// here - there is no "only when PPS is short" gate on this branch.
	l := simpleLog(t, fixed5V3A, pps3311V2A, sprAVS)
	m := l.Evaluate(10, 4)
	checkMatch(t, m, true, ModeUpgradeSPRAVSMoreCurrent, 5)
	wantMessage(t, m, "SPR AVS supplies more: 5.00 A")

	// Upgrade fires even when PPS alone would have satisfied the request.
	m = l.Evaluate(10, 1)
	checkMatch(t, m, true, ModeUpgradeSPRAVSMoreCurrent, 5)

	// If SPR AVS offers no more than PPS, stay on PPS.
	l2 := simpleLog(t, pps3311V3A, sprAVS2A)
	checkMatch(t, l2.Evaluate(10, 1), true, ModeSPRPPS, 3)
}

func TestEvaluateSPRAVSOnly(t *testing.T) {
	l := simpleLog(t, fixed5V3A, sprAVS)

	// 18 V is in the upper band, so the 15-20 V limit applies -- NOT the
	// larger of the two. This assertion previously demanded 5 A, locking in an
	// over-report: the object offers 5.00 A only below 15 V and 3.25 A above.
	m := l.Evaluate(18, 4)
	checkMatch(t, m, false, ModeSPRAVS, 3.25)
	wantMessage(t, m, "assumed 9.0-20.0 V output range")
	wantMessage(t, m, "exceeds the 3.25 A available at 18 V")

	// The same object in the lower band does offer 5 A.
	m = l.Evaluate(12, 4)
	checkMatch(t, m, true, ModeSPRAVS, 5)

	// Below the assumed SPR AVS floor it cannot help.
	m = l.Evaluate(7, 1)
	checkMatch(t, m, false, ModeNone, 0)
	wantMessage(t, m, "SPR AVS covers 9.0-20.0 V only")
}

// ---------------------------------------------------------------------------
// SPR: failures
// ---------------------------------------------------------------------------

func TestEvaluateSPRNoFixedNoRange(t *testing.T) {
	l := simpleLog(t, fixed5V3A)
	m := l.Evaluate(12, 1)
	checkMatch(t, m, false, ModeNone, 0)
	wantMessage(t, m, "no fixed 12 V PDO offered")
	wantMessage(t, m, "the source offers no PPS range")
	wantMessage(t, m, "the source offers no SPR AVS range")
	if m.CurrentShort() {
		t.Error("CurrentShort() = true when the voltage itself is unreachable")
	}
}

func TestEvaluateSPRPPSRangeOnlyMessage(t *testing.T) {
	// The exact message shape called out in the brief.
	l := simpleLog(t, fixed5V3A, pps3311V3A)
	m := l.Evaluate(12, 1)
	checkMatch(t, m, false, ModeNone, 0)
	wantMessage(t, m, "PPS covers 3.3-11.0 V only")
	wantMessage(t, m, "no fixed 12 V PDO offered")
}

func TestEvaluateSPRNonStandardVoltage(t *testing.T) {
	l := simpleLog(t, fixed5V3A, fixed9V3A)
	m := l.Evaluate(13.5, 1)
	checkMatch(t, m, false, ModeNone, 0)
	wantMessage(t, m, "13.5 V is not a standard fixed PD voltage")
}

func TestEvaluateIgnoresInvalidFixed(t *testing.T) {
	// A 3.3 V fixed PDO fails the vendor's >= 5 V filter, and a 9 V PDO with
	// zero current fails the > 0 A filter; neither may be matched.
	l := simpleLog(t, fixed33V3A, fixed9V0A)
	checkMatch(t, l.Evaluate(3.3, 1), false, ModeNone, 0)
	checkMatch(t, l.Evaluate(9, 0.1), false, ModeNone, 0)
}

func TestEvaluatePicksHighestCurrentFixed(t *testing.T) {
	// A malformed source advertising 9 V twice is judged by its better object.
	l := simpleLog(t, fixed9V15A, fixed9V3A)
	checkMatch(t, l.Evaluate(9, 3), true, ModeSPRFixed, 3)
}

// ---------------------------------------------------------------------------
// EPR
// ---------------------------------------------------------------------------

func TestEvaluateEPRFixed(t *testing.T) {
	l := simpleLog(t, fixed5V3A, fixed20V5A, fixed28V5A)
	m := l.Evaluate(28, 4)
	checkMatch(t, m, true, ModeEPRFixed, 5)
	wantMessage(t, m, "EPR fixed 28 V PDO offers 5.00 A")
	wantMessage(t, m, "eMarker-equipped 5 A EPR cable")
}

func TestEvaluateEPRUpgradeToAVS(t *testing.T) {
	// Fixed 28 V gives 3 A; the 140 W AVS range gives 140/28 = 5 A.
	l := simpleLog(t, fixed5V3A, fixed28V3A, eprAVS140W)
	m := l.Evaluate(28, 4)
	checkMatch(t, m, true, ModeUpgradeEPRAVSMoreCurrent, 5)
	wantMessage(t, m, "the fixed PDO cannot supply the requested 4.00 A")

	// The fixed object already satisfies 2 A, so no upgrade (SPEC.md §9.5).
	checkMatch(t, l.Evaluate(28, 2), true, ModeEPRFixed, 3)

	// An 84 W AVS gives exactly 3 A at 28 V - no more than fixed - so no
	// upgrade even though the request is short.
	l2 := simpleLog(t, fixed28V3A, eprAVS84W)
	checkMatch(t, l2.Evaluate(28, 4), false, ModeEPRFixed, 3)
}

func TestEvaluateEPRAVSOnly(t *testing.T) {
	l := simpleLog(t, fixed5V3A, eprAVS140W)
	m := l.Evaluate(28, 4)
	checkMatch(t, m, true, ModeEPRAVS, 5)
	wantMessage(t, m, "no fixed 28 V PDO offered")
	wantMessage(t, m, "EPR AVS range 15.0-28.0 V (140 W) supplies 5.00 A at 28 V")

	// Power-limited, not cable-limited: 140 W at 24 V would be 5.83 A but no
	// cable is rated above 5 A.
	m = l.Evaluate(24, 5)
	checkMatch(t, m, true, ModeEPRAVS, 5)
	wantMessage(t, m, "no USB-C cable is rated above 5.00 A")

	// Genuinely power-limited: 140 W at 27 V is 5.19 A -> capped to 5.
	// Use the 240 W part at 48 V instead, which is 5 A exactly by budget.
	l2 := simpleLog(t, eprAVS240W)
	checkMatch(t, l2.Evaluate(48, 5), true, ModeEPRAVS, 5)
	// 240 W at 40 V is 6 A by budget, capped by the cable.
	m = l2.Evaluate(40, 5.5)
	checkMatch(t, m, false, ModeEPRAVS, 5)
	wantMessage(t, m, "requested 5.50 A exceeds the 5.00 A available at 40 V")
}

func TestEvaluateEPRAVSPowerLimited(t *testing.T) {
	// 84 W at 28 V is 3 A: below the cable cap, so the budget binds.
	l := simpleLog(t, eprAVS84W)
	m := l.Evaluate(28, 4)
	checkMatch(t, m, false, ModeEPRAVS, 3)
	wantMessage(t, m, "requested 4.00 A exceeds the 3.00 A available at 28 V")
	if hasMessage(m, "no USB-C cable is rated") {
		t.Errorf("cable-cap message emitted when the power budget bound: %v", m.Messages)
	}
}

func TestEvaluateEPRRequiredOnSPROnlySource(t *testing.T) {
	// The headline failure: an ordinary SPR charger asked for 28 V.
	l := simpleLog(t, fixed5V3A, fixed9V3A, fixed12V3A, fixed20V5A, pps3311V3A)
	m := l.Evaluate(28, 3)
	checkMatch(t, m, false, ModeNone, 0)
	wantMessage(t, m, eprRequiredMessage)
	if hasMessage(m, "eMarker-equipped 5 A EPR cable; a fast-blinking") {
		t.Errorf("cable advisory emitted for an unachievable request: %v", m.Messages)
	}
}

func TestEvaluateEPRSupportedButWrongVoltage(t *testing.T) {
	// The source is EPR-capable (48 V fixed) but has neither a 28 V object nor
	// an AVS range: a different diagnosis from "not EPR".
	l := simpleLog(t, fixed5V3A, fixed48V5A)
	m := l.Evaluate(28, 3)
	checkMatch(t, m, false, ModeNone, 0)
	wantMessage(t, m, "no fixed 28 V PDO offered")
	wantMessage(t, m, "the source offers no EPR AVS range")
	if hasMessage(m, eprRequiredMessage) {
		t.Errorf("EPR-capable source reported as non-EPR: %v", m.Messages)
	}
}

func TestEvaluateEPRAVSOutOfRange(t *testing.T) {
	l := simpleLog(t, fixed5V3A, eprAVS140W)
	m := l.Evaluate(36, 3) // above the AVS ceiling, no fixed 36 V object
	checkMatch(t, m, false, ModeNone, 0)
	wantMessage(t, m, "EPR AVS covers 15.0-28.0 V only")
	wantMessage(t, m, "no fixed 36 V PDO offered")
}

func TestEvaluateEPRNonStandardVoltage(t *testing.T) {
	l := simpleLog(t, fixed48V5A)
	m := l.Evaluate(24, 3)
	checkMatch(t, m, false, ModeNone, 0)
	wantMessage(t, m, "24 V is not a standard fixed PD voltage")
}

func TestEvaluateEPRCableFaultSurfaced(t *testing.T) {
	// An invalid EPR AVS APDO both promotes EPRCableFail and leaves the source
	// with no usable adjustable range.
	l, err := Parse(buildLog(28000, 0, 2, 1, 0, 0, fixed5V3A, eprAVSBad))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !l.EPRCableFail {
		t.Fatal("EPRCableFail not promoted")
	}
	m := l.Evaluate(28, 3)
	checkMatch(t, m, false, ModeNone, 0)
	wantMessage(t, m, "EPR AVS PDO failed validation")
	wantMessage(t, m, "eMarker-equipped 5 A EPR cable and rescan")
}

func TestEvaluateEPRCableFaultOnSPROnlySource(t *testing.T) {
	// flags2 says the cable failed, and no EPR capability was recorded as a
	// result. Point at the cable rather than declaring the charger inadequate.
	l, err := Parse(buildLog(28000, 0, 1, 1, 0, Flag2EPRCableFail, fixed5V3A))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := l.Evaluate(28, 3)
	checkMatch(t, m, false, ModeNone, 0)
	wantMessage(t, m, eprRequiredMessage)
	wantMessage(t, m, "the source may in fact be EPR-capable")
}

// ---------------------------------------------------------------------------
// Guards and helpers
// ---------------------------------------------------------------------------

func TestEvaluateBadInput(t *testing.T) {
	l := simpleLog(t, fixed9V3A)
	for _, v := range []float64{0, -1} {
		m := l.Evaluate(v, 1)
		checkMatch(t, m, false, ModeNone, 0)
		wantMessage(t, m, "requested voltage must be")
	}
	m := l.Evaluate(9, -1)
	checkMatch(t, m, false, ModeNone, 0)
	wantMessage(t, m, "requested current must be")

	var nilLog *Log
	m = nilLog.Evaluate(9, 1)
	checkMatch(t, m, false, ModeNone, 0)
	wantMessage(t, m, "run a power supply scan first")
}

func TestEvaluateZeroCurrentRequest(t *testing.T) {
	// Asking "can it reach 9 V at all" must succeed without a shortfall.
	l := simpleLog(t, fixed9V3A)
	checkMatch(t, l.Evaluate(9, 0), true, ModeSPRFixed, 3)
}

func TestEvaluateExactCurrentBoundary(t *testing.T) {
	// 3.00 A requested against a 3.00 A object must be OK; float noise from the
	// 0.01 scale factor must not tip it over.
	l := simpleLog(t, fixed9V3A)
	checkMatch(t, l.Evaluate(9, 3), true, ModeSPRFixed, 3)
	checkMatch(t, l.Evaluate(9, 3.01), false, ModeSPRFixed, 3)
}

func TestEvaluateIgnoresBatteryAndVariable(t *testing.T) {
	// Battery and Variable PDOs are decoded and reported but, as in the vendor
	// app, are not used to satisfy a request: the firmware never selects them.
	l := simpleLog(t, battery, variablePDO)
	checkMatch(t, l.Evaluate(9, 1), false, ModeNone, 0)
	if len(l.PDOs) != 2 {
		t.Fatalf("got %d PDOs, want 2 (they must still be decoded)", len(l.PDOs))
	}
	if !l.PDOs[0].Valid || !l.PDOs[1].Valid {
		t.Error("battery/variable PDOs were not decoded as valid")
	}
}

func TestKnownFixedVoltages(t *testing.T) {
	got := KnownFixedVoltages()
	want := []float64{5, 9, 12, 15, 20, 28, 36, 48}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// The returned slice must be a copy.
	got[0] = 999
	if KnownFixedVoltages()[0] != 5 {
		t.Error("KnownFixedVoltages returns the backing array")
	}
	for _, v := range want {
		if !IsKnownFixedVoltage(v) {
			t.Errorf("IsKnownFixedVoltage(%v) = false", v)
		}
	}
	for _, v := range []float64{0, 3.3, 7.4, 13.5, 24, 50} {
		if IsKnownFixedVoltage(v) {
			t.Errorf("IsKnownFixedVoltage(%v) = true", v)
		}
	}
}

func TestEvaluateFullModeCoverage(t *testing.T) {
	// Every mode the contract names must be reachable.
	seen := map[string]bool{}
	record := func(m Match) { seen[m.Mode] = true }

	record(simpleLog(t, fixed28V5A).Evaluate(28, 1))             // epr_fixed
	record(simpleLog(t, eprAVS140W).Evaluate(28, 1))             // epr_avs
	record(simpleLog(t, fixed28V3A, eprAVS140W).Evaluate(28, 4)) // upgrade_epr_avs
	record(simpleLog(t, fixed9V3A).Evaluate(9, 1))               // spr_fixed
	record(simpleLog(t, pps3311V3A).Evaluate(9, 1))              // spr_pps
	record(simpleLog(t, sprAVS).Evaluate(18, 1))                 // spr_avs
	record(simpleLog(t, fixed9V15A, pps3311V3A).Evaluate(9, 2))  // upgrade_spr_pps
	record(simpleLog(t, pps3311V2A, sprAVS).Evaluate(10, 1))     // upgrade_spr_avs
	record(simpleLog(t, fixed5V3A).Evaluate(12, 1))              // none

	for _, mode := range []string{
		ModeNone, ModeEPRFixed, ModeEPRAVS, ModeUpgradeEPRAVSMoreCurrent,
		ModeSPRFixed, ModeSPRPPS, ModeSPRAVS,
		ModeUpgradeSPRPPSMoreCurrent, ModeUpgradeSPRAVSMoreCurrent,
	} {
		if !seen[mode] {
			t.Errorf("mode %q never produced", mode)
		}
	}
}

// TestSPRAVSBandAwareCurrent is a regression test: PDO.MaxCurrentA is the max
// of the SPR AVS APDO's two band limits, and Evaluate used to report it at any
// voltage. That over-reports available current in the weaker band, which is the
// direction that damages hardware.
func TestSPRAVSBandAwareCurrent(t *testing.T) {
	// 3.25 A in the 15-20 V band (bits 19:10 = 325), 5.00 A in the 9-15 V band.
	p := PDO{Kind: KindSPRAVS, MaxCurrent20VA: 3.25, MaxCurrent15VA: 5.00, MaxCurrentA: 5.00}
	for _, tc := range []struct {
		v    float64
		want float64
	}{
		{9, 5.00}, {12, 5.00}, {15, 5.00}, // lower band, boundary inclusive
		{15.1, 3.25}, {18, 3.25}, {20, 3.25}, // upper band
	} {
		if got := p.CurrentAt(tc.v); got != tc.want {
			t.Errorf("CurrentAt(%g) = %g, want %g", tc.v, got, tc.want)
		}
	}
	// Every other kind is unaffected.
	f := PDO{Kind: KindFixed, VoltageV: 9, MaxCurrentA: 3}
	if got := f.CurrentAt(9); got != 3 {
		t.Errorf("fixed CurrentAt = %g, want 3", got)
	}
}

// ---------------------------------------------------------------------------
// Regression: the cable current ceiling applies to every kind, not just EPR AVS
// ---------------------------------------------------------------------------

// TestEvaluateFixedAboveCableLimit is the headline case. A fixed PDO's current
// field is ten bits of 10 mA, so a malformed or hostile source can advertise
// 10.23 A; that value used to flow into Match.MaxCurrentA unbounded, and a
// request for 6 A came back "yes" against a cable that melts at 5.
func TestEvaluateFixedAboveCableLimit(t *testing.T) {
	l := simpleLog(t, fixed5V3A, fixed9V1023A)

	m := l.Evaluate(9, 6)
	checkMatch(t, m, false, ModeSPRFixed, MaxCableCurrentA)
	wantCaveat(t, m, CaveatCableCurrentBound)
	wantMessage(t, m, "the PDO advertises 10.23 A")
	wantMessage(t, m, "no USB-C cable is rated above 5.00 A")
	wantMessage(t, m, "requested 6.00 A exceeds the 5.00 A available at 9 V")
	if !nearly(m.DeclaredMaxCurrentA, 10.23) {
		t.Errorf("DeclaredMaxCurrentA = %v, want 10.23: the claim must survive into --json", m.DeclaredMaxCurrentA)
	}
	// Everything at or below the ceiling still answers normally.
	checkMatch(t, l.Evaluate(9, 5), true, ModeSPRFixed, MaxCableCurrentA)
}

// TestEvaluateEPRFixedAboveCableLimit is the same defect above 20 V, where it is
// doubly wrong: EPR operation presupposes an eMarked 5 A cable, so 10.23 A is
// not merely unachievable but excluded by the mode the source is being asked to
// enter.
func TestEvaluateEPRFixedAboveCableLimit(t *testing.T) {
	l := simpleLog(t, fixed5V3A, fixed28V1023A)
	m := l.Evaluate(28, 6)
	checkMatch(t, m, false, ModeEPRFixed, MaxCableCurrentA)
	wantCaveat(t, m, CaveatCableCurrentBound)
	wantMessage(t, m, "EPR fixed 28 V PDO offers 5.00 A")
	wantMessage(t, m, "eMarker-equipped 5 A EPR cable")
}

// TestEvaluateNeverReportsMoreThanACableCarries closes the class rather than the
// instance: whatever kind carries the verdict and whatever the request, the
// current reported must be one a cable can deliver. A new mode that skipped the
// bound would fail here.
func TestEvaluateNeverReportsMoreThanACableCarries(t *testing.T) {
	logs := map[string][]uint32{
		"fixed":            {fixed9V1023A, fixed28V1023A},
		"variable":         {variable1023A},
		"pps":              {pps3311V635A},
		"spr avs":          {sprAVS1023A},
		"pps then spr avs": {pps3311V635A, sprAVS1023A},
		"fixed then pps":   {fixed9V15A, pps3311V635A},
		"epr avs":          {eprAVS240W},
		"fixed then epr":   {fixed28V3A, eprAVS240W},
		"everything at once": {fixed5V3A, fixed9V1023A, fixed28V1023A, pps3311V635A,
			sprAVS1023A, eprAVS240W, variable1023A, battery},
	}
	voltages := []float64{3.3, 5, 9, 10, 12, 15, 15.1, 18, 20, 21, 24, 28, 36, 48}
	for name, words := range logs {
		t.Run(name, func(t *testing.T) {
			l := simpleLog(t, words...)
			for _, v := range voltages {
				for _, i := range []float64{0, 1, 3, 5, 10} {
					m := l.Evaluate(v, i)
					if m.MaxCurrentA > MaxCableCurrentA+cmpEps {
						t.Fatalf("Evaluate(%v, %v) reports %v A, above the %v A ceiling (mode %q)",
							v, i, m.MaxCurrentA, MaxCableCurrentA, m.Mode)
					}
					if m.OK && i > MaxCableCurrentA+cmpEps {
						t.Fatalf("Evaluate(%v, %v) = OK: no cable carries %v A", v, i, i)
					}
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Regression: the assumed SPR AVS range must be disclosed wherever it decides
// ---------------------------------------------------------------------------

// TestSPRAVSAssumptionDisclosedOnEveryDecidingPath covers all three places the
// 9-20 V assumption turns into an answer. Only the first used to admit it, so a
// user upgraded onto SPR AVS, or refused because of its assumed floor, was told
// a spec document's opinion as if it were their charger's.
func TestSPRAVSAssumptionDisclosedOnEveryDecidingPath(t *testing.T) {
	cases := []struct {
		name     string
		log      *Log
		v, i     float64
		wantMode string
	}{
		{"spr_avs alone", simpleLog(t, fixed5V3A, sprAVS), 18, 1, ModeSPRAVS},
		{"pps upgraded to spr_avs", simpleLog(t, pps3311V2A, sprAVS), 10, 1, ModeUpgradeSPRAVSMoreCurrent},
		{"refused below the assumed floor", simpleLog(t, fixed5V3A, sprAVS), 7, 1, ModeNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.log.Evaluate(tc.v, tc.i)
			if m.Mode != tc.wantMode {
				t.Fatalf("Mode = %q, want %q (messages: %v)", m.Mode, tc.wantMode, m.Messages)
			}
			wantCaveat(t, m, CaveatSPRAVSAssumedRange)
			wantMessage(t, m, "carries no voltage range on the wire")
			wantMessage(t, m, "9.0-20.0 V")
		})
	}

	// And it must not be claimed where no SPR AVS object decided anything.
	for _, m := range []Match{
		simpleLog(t, fixed9V3A).Evaluate(9, 1),
		simpleLog(t, pps3311V3A).Evaluate(9, 1),
		simpleLog(t, eprAVS140W).Evaluate(28, 1),
	} {
		wantNoCaveat(t, m, CaveatSPRAVSAssumedRange)
	}
}

// ---------------------------------------------------------------------------
// Regression: a recorded EPR cable failure qualifies a positive verdict too
// ---------------------------------------------------------------------------

// TestEPRCableFailCaveatOnPositiveVerdict pins that Log.EPRCableFail reaches the
// success path. It was surfaced only in failure messages, so the one moment the
// user was about to act on "yes, 28 V at 3 A works" was the one moment nothing
// mentioned that the scan itself ran over a cable that could not enter EPR and
// may therefore have seen an incomplete picture.
func TestEPRCableFailCaveatOnPositiveVerdict(t *testing.T) {
	t.Run("flags2 bit with an EPR fixed verdict", func(t *testing.T) {
		l, err := Parse(buildLog(28000, 0, 2, 1, 0, Flag2EPRCableFail, fixed5V3A, fixed28V5A))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		m := l.Evaluate(28, 3)
		checkMatch(t, m, true, ModeEPRFixed, 5)
		wantCaveat(t, m, CaveatEPRCableFail)
		wantMessage(t, m, "recorded an EPR cable failure")
		wantMessage(t, m, "rescan to confirm")
	})

	t.Run("promoted by an invalid EPR AVS APDO", func(t *testing.T) {
		// The second source of the flag (SPEC.md §9.4) must qualify the verdict
		// exactly as the flags2 bit does.
		l, err := Parse(buildLog(28000, 0, 3, 1, 0, 0, fixed5V3A, fixed28V5A, eprAVSBad))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if !l.EPRCableFail {
			t.Fatal("EPRCableFail not promoted")
		}
		m := l.Evaluate(28, 3)
		checkMatch(t, m, true, ModeEPRFixed, 5)
		wantCaveat(t, m, CaveatEPRCableFail)
	})

	t.Run("EPR AVS answering an SPR request", func(t *testing.T) {
		// 18 V is below the EPR threshold, but the capability chosen is an EPR
		// object, so entering EPR is still required and the caveat still bites.
		l, err := Parse(buildLog(18000, 0, 2, 1, 0, Flag2EPRCableFail, fixed5V3A, eprAVS140W))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		m := l.Evaluate(18, 3)
		checkMatch(t, m, true, ModeEPRAVS, 5)
		wantCaveat(t, m, CaveatEPRCableFail)
		wantMessage(t, m, "required even below 20 V")
	})

	t.Run("not claimed without cause", func(t *testing.T) {
		// No recorded failure: no caveat.
		wantNoCaveat(t, simpleLog(t, fixed28V5A).Evaluate(28, 3), CaveatEPRCableFail)
		// Recorded failure, but a purely SPR verdict that never needs EPR.
		l, err := Parse(buildLog(9000, 0, 2, 1, 0, Flag2EPRCableFail, fixed5V3A, fixed9V3A))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		m := l.Evaluate(9, 3)
		checkMatch(t, m, true, ModeSPRFixed, 3)
		wantNoCaveat(t, m, CaveatEPRCableFail)
		if hasMessage(m, "recorded an EPR cable failure") {
			t.Errorf("EPR cable advice on a 9 V SPR verdict: %v", m.Messages)
		}
	})
}

// ---------------------------------------------------------------------------
// Regression: pick the EPR AVS object that explains the failure
// ---------------------------------------------------------------------------

// TestEPRAVSFailureExplanationIsInformative pins the preference order. Taking
// the first EPR AVS APDO in log order reported "the EPR AVS PDO failed
// validation" from a log that also held a perfectly good 15.0-28.0 V range,
// pointing the user at their cable when the real answer was the 28 V ceiling.
func TestEPRAVSFailureExplanationIsInformative(t *testing.T) {
	t.Run("valid beats invalid whatever the order", func(t *testing.T) {
		l := simpleLog(t, fixed5V3A, eprAVSBad, eprAVS140W)
		m := l.Evaluate(36, 3)
		checkMatch(t, m, false, ModeNone, 0)
		wantMessage(t, m, "EPR AVS covers 15.0-28.0 V only")
		if hasMessage(m, "failed validation") {
			t.Errorf("a valid range existed but the invalid APDO explained the failure: %v", m.Messages)
		}
	})

	t.Run("closest range wins among valid ones", func(t *testing.T) {
		// 15.0-20.0 V @ 100 W first, 15.0-28.0 V @ 140 W second. At 36 V the
		// second is the binding limit and the more useful thing to name.
		eprAVS15to20 := uint32(3)<<30 | uint32(1)<<28 | uint32(200)<<17 | uint32(150)<<8 | 100
		l := simpleLog(t, eprAVS15to20, eprAVS140W)
		m := l.Evaluate(36, 1)
		checkMatch(t, m, false, ModeNone, 0)
		wantMessage(t, m, "EPR AVS covers 15.0-28.0 V only")
	})

	t.Run("only an invalid object to report", func(t *testing.T) {
		l := simpleLog(t, fixed5V3A, eprAVSBad)
		m := l.Evaluate(28, 3)
		checkMatch(t, m, false, ModeNone, 0)
		wantMessage(t, m, "failed validation")
	})
}

// TestEvaluateEPRAVSCoversSPRRequest is a regression test for the branch split:
// Evaluate partitioned candidates by the requested voltage, so an EPR AVS range
// starting at 15 V was invisible to an 18 V request and the tool confidently
// reported "not achievable" from a log that plainly covered it.
func TestEvaluateEPRAVSCoversSPRRequest(t *testing.T) {
	// A real 140 W EPR charger: fixed 5 V plus EPR AVS 15.0-28.0 V @ 140 W.
	eprAVS15to28 := uint32(3)<<30 | uint32(1)<<28 | uint32(280)<<17 | uint32(150)<<8 | 140
	l := simpleLog(t, fixed5V3A, eprAVS15to28)

	m := l.Evaluate(18, 1)
	if !m.OK {
		t.Fatalf("18 V reported unachievable from a log containing a 15-28 V EPR AVS range: %v", m.Messages)
	}
	if m.Mode != ModeEPRAVS {
		t.Errorf("Mode = %q, want %q", m.Mode, ModeEPRAVS)
	}
	wantMessage(t, m, "EPR AVS range")
}

// TestEvaluateEPRAVSUpgradeWhenAWeakerSPRObjectCovers is the other half of the
// same deviation. Consulting the EPR side only when NOTHING in the SPR classes
// covers the request left the rule half applied: a 140 W charger advertising
// fixed 15 V 3 A beside an EPR AVS 15.0-28.0 V range answered "no, 3.00 A" at
// 15 V and "yes, 5.00 A" at 18 V from one and the same scan, purely because
// 18 V happened to have no fixed object. All three SPR arms could pick the
// weaker object, so all three are covered here.
func TestEvaluateEPRAVSUpgradeWhenAWeakerSPRObjectCovers(t *testing.T) {
	cases := []struct {
		name string
		log  *Log
		v    float64
	}{
		// An Apple 140 W adapter's shape: the fixed object at 15 V is the weak one.
		{"fixed", simpleLog(t, fixed5V3A, fixed9V3A, fixed15V3A, fixed20V5A, fixed28V5A, eprAVS140W), 15},
		{"pps", simpleLog(t, fixed5V3A, pps3321V3A, eprAVS140W), 18},
		{"spr avs", simpleLog(t, fixed5V3A, sprAVS, eprAVS140W), 18},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.log.Evaluate(tc.v, 4)
			checkMatch(t, m, true, ModeUpgradeEPRAVSMoreCurrent, 5)
			wantMessage(t, m, "the EPR AVS range 15.0-28.0 V (140 W) supplies 5.00 A")
			// The capability is an EPR object below 20 V, so the cable
			// requirement follows it.
			wantMessage(t, m, "required even below 20 V")
		})
	}

	// An SPR object that can do the job keeps the verdict: the upgrade is gated
	// on the shortfall, exactly as the EPR branch gates its own.
	l := simpleLog(t, fixed5V3A, fixed15V3A, eprAVS140W)
	m := l.Evaluate(15, 3)
	checkMatch(t, m, true, ModeSPRFixed, 3)
	if hasMessage(m, "eMarker") {
		t.Errorf("an SPR verdict acquired an EPR cable requirement it does not need: %v", m.Messages)
	}

	// Moving off an SPR AVS object must not take its disclosure with it: the
	// message that object left behind still quotes the assumed 9-20 V range.
	m = cases[2].log.Evaluate(18, 4)
	wantCaveat(t, m, CaveatSPRAVSAssumedRange)
	wantMessage(t, m, "carries no voltage range on the wire")
}

// TestEvaluateEPRCableFaultOnAnSPRRangeRefusal is the SPR mirror of
// TestEvaluateEPRCableFaultOnSPROnlySource. A refusal at 15 V or above is
// exactly the refusal a cable that could not enter EPR produces, since an EPR
// AVS range routinely starts at 15 V and would have covered the request; the SPR
// refusal path explained itself purely in terms of fixed/PPS/SPR AVS objects and
// never mentioned the fault the same scan recorded.
func TestEvaluateEPRCableFaultOnAnSPRRangeRefusal(t *testing.T) {
	// A 140 W charger scanned through a non-eMarked cable: EPR entry failed, so
	// it advertised only its SPR objects.
	l, err := Parse(buildLog(18000, 0, 4, 1, 0, Flag2EPRCableFail,
		fixed5V3A, fixed9V3A, fixed15V3A, fixed20V5A))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	m := l.Evaluate(18, 3)
	checkMatch(t, m, false, ModeNone, 0)
	wantCaveat(t, m, CaveatEPRCableFail)
	wantMessage(t, m, "an EPR AVS range (15 V and up) may have been hidden")

	// Below 15 V no EPR AVS object could have covered the request whatever the
	// cable did, so the fault explains nothing and must not be cited.
	m = l.Evaluate(11, 3)
	checkMatch(t, m, false, ModeNone, 0)
	wantNoCaveat(t, m, CaveatEPRCableFail)
	if hasMessage(m, "may have been hidden") {
		t.Errorf("cable fault blamed for an 11 V refusal it cannot explain: %v", m.Messages)
	}
}

// ---------------------------------------------------------------------------
// Regression: a power-limited PPS range is not credited with its full current
// ---------------------------------------------------------------------------

// TestPPSPowerLimitedBoundsTheVerdict pins the verdict half of bit 27. The
// decode ignored it, so a 45 W charger's 3.3-11 V / 5 A range was reported as
// 5 A at every voltage in it and `scan --voltage 11 --current 4.5` answered
// "yes" against a source that can supply 4.09 A there — a 22% over-report in the
// direction that destroys hardware.
func TestPPSPowerLimitedBoundsTheVerdict(t *testing.T) {
	// The 45 W shape: 5/9/15 V at 3 A, 20 V at 2.25 A, PPS 3.3-11 V at 5 A with
	// the Power Limited bit set.
	l := simpleLog(t, fixed5V3A, fixed9V3A, fixed15V3A, fixed20V225A, pps3311V5APL)

	m := l.Evaluate(11, 4.5)
	checkMatch(t, m, false, ModeSPRPPS, 4.09)
	wantCaveat(t, m, CaveatPPSPowerLimited)
	wantMessage(t, m, "45 W permits 4.09 A at 11 V")
	wantMessage(t, m, "inferred from the source's own fixed PDOs, not scanned")
	wantMessage(t, m, "requested 4.50 A exceeds the 4.09 A available at 11 V")

	// The same word with bit 27 clear says nothing about a budget, so the
	// advertised figure stands and nothing is disclosed.
	plain := simpleLog(t, fixed5V3A, fixed9V3A, fixed15V3A, fixed20V225A, pps3311V5A)
	m = plain.Evaluate(11, 4.5)
	checkMatch(t, m, true, ModeSPRPPS, 5)
	wantNoCaveat(t, m, CaveatPPSPowerLimited)

	// Where the budget does not bite, the bit costs the answer nothing and
	// saying so would be noise: 45 W allows 9 A at 5 V.
	m = l.Evaluate(5, 3)
	checkMatch(t, m, true, ModeSPRFixed, 3)
	wantNoCaveat(t, m, CaveatPPSPowerLimited)

	// With nothing to infer a budget from, the advertised figure is all there
	// is — which is exactly when the user needs telling that it may be
	// optimistic.
	guess := simpleLog(t, fixed5V3A, pps3311V5APL)
	m = guess.Evaluate(11, 4.5)
	checkMatch(t, m, true, ModeSPRPPS, 5)
	wantCaveat(t, m, CaveatPPSPowerLimited)
	wantMessage(t, m, "do not say what it is")
	wantMessage(t, m, "Source_Capabilities_Extended")
}

// TestEvaluatePPSCoversAboveTheEPRThreshold is the EPR-side mirror of the same
// SPEC.md §17 deviation: the vendor partitions candidates by the requested
// voltage, so an above-20 V request never sees an SPR object. A PPS APDO's
// max-voltage field is 8 bits of 100 mV and decodes to 25.5 V, so the class can
// reach past the threshold even though it is an SPR class, and the branch that
// checks for it before declaring the voltage unreachable had no test of its own
// — every other PPS fixture in this file tops out at 11 V.
func TestEvaluatePPSCoversAboveTheEPRThreshold(t *testing.T) {
	l := simpleLog(t, fixed5V3A, pps3321V5A)

	m := l.Evaluate(21, 3)
	checkMatch(t, m, true, ModeSPRPPS, 5)
	wantMessage(t, m, "no EPR capability reaches 21 V, but the PPS range 3.3-21.0 V covers it and supplies 5.00 A")
	// The request is still above 20 V, so the cable advisory finish attaches to
	// every EPR-voltage verdict must survive the SPR-class capability.
	wantMessage(t, m, "eMarker-equipped 5 A EPR cable")

	// One tenth of a volt further and nothing covers it: the branch answers only
	// for what the decoded range actually reaches, and hands back to the EPR
	// failure explanation otherwise.
	m = l.Evaluate(21.1, 3)
	checkMatch(t, m, false, ModeNone, 0)
	wantMessage(t, m, eprRequiredMessage)
}
