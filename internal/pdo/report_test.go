package pdo

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Regression: vendor strings are reproduced verbatim, capitalisation included
// ---------------------------------------------------------------------------

// TestVendorStringsMatchTheSpec pins the three strings this package copies from
// the vendor's application rather than writing itself. They exist to be searched
// for (SPEC.md §9.6), so a difference of one letter's case makes them useless:
// the user who pastes what the vendor app printed finds nothing. The literals
// below are the spec's, transcribed character for character from §9.5 and §9.6 —
// including "vFlex", whose odd capitalisation is what shows the transcription is
// faithful rather than approximate.
func TestVendorStringsMatchTheSpec(t *testing.T) {
	if eprRequiredMessage != "Power source with Extended Power Range is Required" {
		t.Errorf("EPR-required message = %q; SPEC.md §9.5 has a capital R in \"is Required\"", eprRequiredMessage)
	}
	if ErrShortLog.Error() != "Invalid PDO log length" {
		t.Errorf("ErrShortLog = %q, want the SPEC.md §9.6 wording", ErrShortLog)
	}
	const wantEmpty = "No PDO data captured (log is empty). Unplug vFlex from phone, " +
		"plug into a USB-C PD charger (e.g. MacBook charger) for ~10s, then reconnect and retry."
	if ErrEmptyLog.Error() != wantEmpty {
		t.Errorf("ErrEmptyLog = %q,\nwant %q", ErrEmptyLog, wantEmpty)
	}

	// And it must reach the user through Evaluate, not merely exist.
	l := simpleLog(t, fixed5V3A, fixed9V3A, fixed20V5A)
	m := l.Evaluate(28, 1)
	if !hasMessage(m, "Extended Power Range is Required") {
		t.Errorf("the vendor's sentence did not reach the verdict: %v", m.Messages)
	}
	for _, s := range m.Messages {
		if strings.Contains(s, "Extended Power Range is required") {
			t.Errorf("lowercase %q: users search for the vendor's own capitalisation", s)
		}
	}
}

// ---------------------------------------------------------------------------
// Regression: an SPR AVS failure must name what actually refused the request
// ---------------------------------------------------------------------------

// sprAVSUpperBandOnly is an SPR AVS APDO with 3.25 A in the 15-20 V band and
// nothing at all in the 9-15 V band: bits 19:10 = 325, bits 9:0 = 0. It is a
// valid object (SPEC.md §9.4 needs only one band non-zero), and it cannot supply
// a single milliamp below 15 V.
const sprAVSUpperBandOnly uint32 = 3<<30 | 2<<28 | 325<<10

// sprAVSLowerBandOnly is the mirror image: 5.00 A below 15 V, nothing above.
const sprAVSLowerBandOnly uint32 = 3<<30 | 2<<28 | 500

// TestSPRAVSFailureExplanationIsInformative is the SPR half of the "explain the
// failure with the object that explains it" defect. Asked for 12 V from a source
// whose SPR AVS APDO has an empty 9-15 V band, the answer used to be "SPR AVS
// covers 9.0-20.0 V only" — a statement that both fails to explain the refusal
// and contradicts itself, since 12 V is inside 9-20 V.
func TestSPRAVSFailureExplanationIsInformative(t *testing.T) {
	t.Run("empty band, not the range, is named", func(t *testing.T) {
		l := simpleLog(t, fixed5V3A, sprAVSUpperBandOnly)
		m := l.Evaluate(12, 1)
		checkMatch(t, m, false, ModeNone, 0)
		wantMessage(t, m, "declares no current in the 9.0-15.0 V band")
		wantMessage(t, m, "it offers 3.25 A in the 15.0-20.0 V band only")
		wantCaveat(t, m, CaveatSPRAVSAssumedRange)
		if hasMessage(m, "SPR AVS covers 9.0-20.0 V only") {
			t.Errorf("12 V is inside the quoted range; the message contradicts itself: %v", m.Messages)
		}
	})

	t.Run("the mirror image above the split", func(t *testing.T) {
		l := simpleLog(t, fixed5V3A, sprAVSLowerBandOnly)
		m := l.Evaluate(18, 1)
		checkMatch(t, m, false, ModeNone, 0)
		wantMessage(t, m, "declares no current in the 15.0-20.0 V band")
		wantMessage(t, m, "it offers 5.00 A in the 9.0-15.0 V band only")
	})

	t.Run("out of the assumed range still quotes the range", func(t *testing.T) {
		// 7 V is below the assumed floor, so there the range really is the
		// obstacle and the old wording is the right one.
		l := simpleLog(t, fixed5V3A, sprAVS)
		m := l.Evaluate(7, 1)
		checkMatch(t, m, false, ModeNone, 0)
		wantMessage(t, m, "SPR AVS covers 9.0-20.0 V only")
		wantMessage(t, m, "assumed rather than scanned")
		wantCaveat(t, m, CaveatSPRAVSAssumedRange)
	})

	t.Run("the capable object speaks for the source", func(t *testing.T) {
		// Two APDOs, the useless one first. The verdict is still "no" at 12 V,
		// but the message must come from the object with capability to describe.
		bothBandsWeakUpper := uint32(3)<<30 | uint32(2)<<28 | uint32(200)<<10 // 2.00 A @20 V, 0 @15 V
		l := simpleLog(t, bothBandsWeakUpper, sprAVSUpperBandOnly)
		m := l.Evaluate(12, 1)
		checkMatch(t, m, false, ModeNone, 0)
		wantMessage(t, m, "it offers 3.25 A in the 15.0-20.0 V band only")
		if hasMessage(m, "it offers 2.00 A in") {
			t.Errorf("the weaker APDO explained the failure: %v", m.Messages)
		}
	})

	t.Run("an APDO with nothing in either band cites no range", func(t *testing.T) {
		// Invalid by SPEC.md §9.4, so the assumed range never enters into the
		// refusal and must not be blamed for it.
		l := &Log{PDOs: []PDO{{Index: 0, Kind: KindSPRAVS, Valid: false}}}
		m := l.Evaluate(12, 1)
		checkMatch(t, m, false, ModeNone, 0)
		wantMessage(t, m, "declares no current in either band")
		wantNoCaveat(t, m, CaveatSPRAVSAssumedRange)
	})

	t.Run("no SPR AVS object at all", func(t *testing.T) {
		m := simpleLog(t, fixed5V3A).Evaluate(12, 1)
		wantMessage(t, m, "the source offers no SPR AVS range")
		wantNoCaveat(t, m, CaveatSPRAVSAssumedRange)
	})

	t.Run("the caveat follows the message, not the object", func(t *testing.T) {
		// Two objects decodePDO cannot produce, to pin that the tag tracks what
		// was actually said. Neither refusal owes anything to the assumed range,
		// so neither may claim the assumption decided it.
		for _, tc := range []struct {
			name string
			p    PDO
			want string
		}{
			{"valid but empty in both bands",
				PDO{Kind: KindSPRAVS, Valid: true},
				"declares no current in either band"},
			{"invalid despite a band limit",
				PDO{Kind: KindSPRAVS, Valid: false, MaxCurrent15VA: 3},
				"failed validation"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				m := (&Log{PDOs: []PDO{tc.p}}).Evaluate(12, 1)
				checkMatch(t, m, false, ModeNone, 0)
				wantMessage(t, m, tc.want)
				wantNoCaveat(t, m, CaveatSPRAVSAssumedRange)
			})
		}
	})
}

// TestEPRAVSFailureNeverContradictsItself is the EPR counterpart of the case
// above. eprAVSExplaining ranks by distance to v, so the object it returns can be
// one whose range contains v; saying "EPR AVS covers 15.0-28.0 V only" of a 24 V
// request would be nonsense. Unreachable through Parse today (validity requires
// pdpW > 0, and PDP/V is then always positive), which is precisely why it is
// worth pinning: a Log assembled anywhere else must still get a coherent answer.
func TestEPRAVSFailureNeverContradictsItself(t *testing.T) {
	l := &Log{PDOs: []PDO{{
		Index: 0, Kind: KindEPRAVS, Valid: true, EPR: true,
		MinVoltageV: 15, MaxVoltageV: 28, PDPWatts: 0,
	}}}
	m := l.Evaluate(24, 1)
	checkMatch(t, m, false, ModeNone, 0)
	wantMessage(t, m, "does reach 24 V")
	if hasMessage(m, "covers 15.0-28.0 V only") {
		t.Errorf("24 V is inside the quoted range; the message contradicts itself: %v", m.Messages)
	}
	// The ordinary out-of-range case is untouched.
	m = simpleLog(t, eprAVS140W).Evaluate(36, 1)
	wantMessage(t, m, "EPR AVS covers 15.0-28.0 V only")
}

// ---------------------------------------------------------------------------
// Regression: the cable ceiling is central, on every path out of the package
// ---------------------------------------------------------------------------

// amps matches a current figure in rendered text: "5.00 A", "10.23 A", "5 A".
var amps = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*A\b`)

// overCeiling returns the current figures in s that exceed MaxCableCurrentA.
func overCeiling(s string) []float64 {
	var out []float64
	for _, m := range amps.FindAllStringSubmatch(s, -1) {
		a, err := strconv.ParseFloat(m[1], 64)
		if err == nil && a > MaxCableCurrentA+cmpEps {
			out = append(out, a)
		}
	}
	return out
}

// disclosed reports whether s is one of the sanctioned shapes in which a figure
// above the ceiling may appear: the user's own request echoed back, or the
// source's claim named as a claim (cableBoundNote, and the bracketed note the
// capability table appends in internal/cli). Anything else is this package
// telling a user that a current no cable carries is available.
func disclosed(s string) bool {
	for _, marker := range []string{"requested", "advertises", "would allow", "source declares"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// TestNoOutputReportsMoreCurrentThanACableCarries closes the loop that
// TestEvaluateNeverReportsMoreThanACableCarries opens. That test checks one
// field; this one checks everything this package hands out — every exported
// current field, PDO.CurrentAt at every voltage, and every Match message —
// against MaxCableCurrentA. A new code path that read a stored field instead of
// going through boundCurrent would fail here even if it never touched
// Match.MaxCurrentA. The rendered capability table is not this package's to
// check: it is built in internal/cli, and the same invariant is asserted there.
//
// The logs are deliberately hostile: every class has a current field wide enough
// to express a figure no cable can carry (SPEC.md §9.4 field widths), and the
// vendor's application reports whatever it is given.
func TestNoOutputReportsMoreCurrentThanACableCarries(t *testing.T) {
	logs := map[string][]uint32{
		"fixed":            {fixed9V1023A, fixed28V1023A},
		"variable":         {variable1023A},
		"pps":              {pps3311V635A},
		"spr avs":          {sprAVS1023A},
		"epr avs":          {eprAVS240W},
		"spr avs one band": {sprAVSUpperBandOnly, sprAVSLowerBandOnly},
		"everything at once": {fixed5V3A, fixed9V1023A, fixed28V1023A, pps3311V635A,
			sprAVS1023A, eprAVS240W, variable1023A, battery, augReserved},
	}
	voltages := []float64{3.3, 5, 7, 9, 10, 12, 15, 15.1, 18, 20, 21, 24, 28, 36, 48}
	currents := []float64{0, 1, 3, 5, 6, 10.23}

	for name, words := range logs {
		t.Run(name, func(t *testing.T) {
			l := simpleLog(t, words...)

			for _, p := range l.PDOs {
				// Every exported current field, and the accessor, are bounded;
				// only the Declared* fields may exceed the ceiling, which is
				// their entire purpose.
				for field, got := range map[string]float64{
					"MaxCurrentA":    p.MaxCurrentA,
					"MaxCurrent15VA": p.MaxCurrent15VA,
					"MaxCurrent20VA": p.MaxCurrent20VA,
				} {
					if got > MaxCableCurrentA+cmpEps {
						t.Errorf("PDO #%d %v: %s = %v, above the %v A ceiling",
							p.Index, p.Kind, field, got, MaxCableCurrentA)
					}
				}
				for _, v := range voltages {
					if a := p.CurrentAt(v); a > MaxCableCurrentA+cmpEps {
						t.Errorf("PDO #%d %v: CurrentAt(%v) = %v, above the ceiling", p.Index, p.Kind, v, a)
					}
				}
			}

			for _, v := range voltages {
				for _, i := range currents {
					m := l.Evaluate(v, i)
					if m.MaxCurrentA > MaxCableCurrentA+cmpEps {
						t.Fatalf("Evaluate(%v, %v): MaxCurrentA = %v, above the ceiling", v, i, m.MaxCurrentA)
					}
					if m.OK && i > MaxCableCurrentA+cmpEps {
						t.Fatalf("Evaluate(%v, %v) = OK: no cable carries %v A", v, i, i)
					}
					// A bounded verdict must carry both halves of the
					// disclosure, not just the reduced number.
					if m.DeclaredMaxCurrentA > 0 {
						if !hasCaveat(m, CaveatCableCurrentBound) {
							t.Errorf("Evaluate(%v, %v) declares %v A without the %s caveat",
								v, i, m.DeclaredMaxCurrentA, CaveatCableCurrentBound)
						}
						if !hasMessage(m, "no USB-C cable is rated above") {
							t.Errorf("Evaluate(%v, %v) bounded silently: %v", v, i, m.Messages)
						}
					}
					for _, s := range m.Messages {
						for _, a := range overCeiling(s) {
							if !disclosed(s) {
								t.Errorf("Evaluate(%v, %v) message offers %v A with no disclosure: %q", v, i, a, s)
							}
						}
					}
				}
			}
		})
	}
}

// TestHandBuiltPDONeverEscapesTheCeiling is the same guarantee for an object
// that never went through decodePDO. PDO's fields are exported, so a caller can
// build one; the ceiling has to be applied where the value is used, not only
// where it is decoded, or the "central clamp" is central to the decoder alone.
func TestHandBuiltPDONeverEscapesTheCeiling(t *testing.T) {
	hand := []PDO{
		{Index: 0, Kind: KindFixed, Valid: true, VoltageV: 9, MaxCurrentA: 42},
		{Index: 1, Kind: KindPPS, Valid: true, MinVoltageV: 5, MaxVoltageV: 11, MaxCurrentA: 6.35},
		{Index: 2, Kind: KindSPRAVS, Valid: true, MaxCurrent15VA: 99, MaxCurrent20VA: 7, MaxCurrentA: 99},
		{Index: 3, Kind: KindVariable, Valid: true, MinVoltageV: 5, MaxVoltageV: 12, MaxCurrentA: 10.23},
	}
	l := &Log{NPDOsReceived: uint8(len(hand)), PDOs: hand}

	for _, p := range hand {
		for _, v := range []float64{5, 9, 12, 18, 20} {
			if a := p.CurrentAt(v); a > MaxCableCurrentA+cmpEps {
				t.Errorf("PDO #%d %v: CurrentAt(%v) = %v, above the ceiling", p.Index, p.Kind, v, a)
			}
		}
	}
	for _, v := range []float64{5, 9, 12, 18, 20} {
		m := l.Evaluate(v, 6)
		if m.MaxCurrentA > MaxCableCurrentA+cmpEps {
			t.Errorf("Evaluate(%v, 6): MaxCurrentA = %v, above the ceiling", v, m.MaxCurrentA)
		}
		if m.OK {
			t.Errorf("Evaluate(%v, 6) = OK: no cable carries 6 A", v)
		}
		// Reducing the figure is only half of it: the hand-built claim must
		// survive as a claim, or the verdict is quietly conservative and a user
		// comparing it against the charger's own label has nothing to go on.
		// Every one of these five requests lands on an object the ceiling bit.
		if m.DeclaredMaxCurrentA <= MaxCableCurrentA {
			t.Errorf("Evaluate(%v, 6): DeclaredMaxCurrentA = %v, the hand-built claim was dropped rather than disclosed",
				v, m.DeclaredMaxCurrentA)
		}
		if !hasCaveat(m, CaveatCableCurrentBound) {
			t.Errorf("Evaluate(%v, 6) bounded a hand-built current without the %s caveat: %v",
				v, CaveatCableCurrentBound, m.Caveats)
		}
	}
}

// TestBoundCurrentIsTheOnlyGate documents what the ceiling is, so that a change
// to MaxCableCurrentA or to boundCurrent's contract shows up as a failure here
// rather than as a wrong number on a power rail.
func TestBoundCurrentIsTheOnlyGate(t *testing.T) {
	for _, tc := range []struct {
		in                float64
		wantUse, wantDecl float64
	}{
		{0, 0, 0},
		{-1, 0, 0},
		{math.NaN(), 0, 0},
		{3, 3, 0},
		{MaxCableCurrentA, MaxCableCurrentA, 0},
		{10.23, MaxCableCurrentA, 10.23},
	} {
		use, decl := boundCurrent(tc.in)
		if !nearly(use, tc.wantUse) || !nearly(decl, tc.wantDecl) {
			t.Errorf("boundCurrent(%v) = (%v, %v), want (%v, %v)", tc.in, use, decl, tc.wantUse, tc.wantDecl)
		}
	}
	// Infinity is the one input that must not survive as a comparison-defeating
	// value: a NaN or +Inf current compares false against every limit. The
	// usable half is clamped, like any figure over the ceiling — it is not
	// collapsed to zero the way NaN is. The declared half is the unbounded claim
	// by construction, so +Inf reaches it verbatim; pinned here because that is
	// the one place a non-finite number still leaves this function, and a caller
	// marshalling it would be the one to find out.
	if use, decl := boundCurrent(math.Inf(1)); use != MaxCableCurrentA || !math.IsInf(decl, 1) {
		t.Errorf("boundCurrent(+Inf) = (%v, %v), want (%v, +Inf)", use, decl, MaxCableCurrentA)
	}
}
