package pdo

import (
	"math"
	"testing"
)

// budgetAt is the current p's power budget allows at v, or 0 where the class
// carries no budget. Two classes do: the EPR AVS PDP read off the wire, and the
// SPR power budget inferred for a PPS APDO that declared itself power limited.
func budgetAt(p PDO, v float64) float64 {
	if v <= 0 {
		return 0
	}
	switch {
	case p.Kind == KindEPRAVS && p.PDPWatts > 0:
		return float64(p.PDPWatts) / v
	case p.Kind == KindPPS && p.PPSPowerLimited && p.PPSBudgetW > 0:
		return float64(p.PPSBudgetW) / v
	}
	return 0
}

// FuzzParse asserts that no byte sequence can panic the decoder or produce a
// log that violates its own invariants. The blob comes off a USB link from
// firmware whose behaviour is only partially known, so it must be treated as
// untrusted input.
func FuzzParse(f *testing.F) {
	f.Add(buildLog(9000, 9010, 6, 1, 0, 0,
		fixed5V3A, fixed9V3A, fixed12V3A, pps3311V3A, eprAVS140W, sprAVS))
	f.Add(buildLog(11000, 11000, 5, 1, 0, 0,
		fixed5V3A, fixed9V3A, fixed15V3A, fixed20V225A, pps3311V5APL))
	f.Add(buildLog(0, 0, 255, 255, 0xFFFF, 0xFFFF, augReserved, battery, variablePDO))
	f.Add(make([]byte, LogBytes))
	f.Add([]byte{1, 2, 3})

	f.Fuzz(func(t *testing.T, b []byte) {
		l, err := Parse(b)
		if err != nil {
			if l != nil {
				t.Fatalf("Parse returned both a log and an error %v", err)
			}
			return
		}
		if len(l.PDOs) > MaxPDOs {
			t.Fatalf("decoded %d PDOs, maximum is %d", len(l.PDOs), MaxPDOs)
		}
		for _, p := range l.PDOs {
			if p.Index < 0 || p.Index >= MaxPDOs {
				t.Fatalf("PDO index %d out of range", p.Index)
			}
			if p.Raw == 0 {
				t.Fatal("a zero word was decoded instead of skipped")
			}
			for _, v := range []float64{
				p.VoltageV, p.MinVoltageV, p.MaxVoltageV, p.MaxCurrentA,
				p.MaxCurrent15VA, p.MaxCurrent20VA, p.MaxPowerW,
			} {
				if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
					t.Fatalf("non-finite or negative decoded value %v in %+v", v, p)
				}
			}
			// No stored current may exceed what a cable can carry, and the
			// declared figure exists only where the bound actually bit.
			for _, c := range []struct{ have, declared float64 }{
				{p.MaxCurrentA, p.DeclaredMaxCurrentA},
				{p.MaxCurrent15VA, p.DeclaredMaxCurrent15VA},
				{p.MaxCurrent20VA, p.DeclaredMaxCurrent20VA},
			} {
				if c.have > MaxCableCurrentA+cmpEps {
					t.Fatalf("decoded current %v exceeds the %v A ceiling: %+v", c.have, MaxCableCurrentA, p)
				}
				if c.declared != 0 && c.declared <= c.have {
					t.Fatalf("declared %v does not exceed the reported %v: %+v", c.declared, c.have, p)
				}
			}
			for _, v := range []float64{3.3, 5, 9, 15, 15.1, 20, 28, 48} {
				if a := p.CurrentAt(v); math.IsNaN(a) || a > MaxCableCurrentA+cmpEps {
					t.Fatalf("CurrentAt(%v) = %v: %+v", v, a, p)
				}
				// A power budget is the other ceiling, and it is derived by
				// division rather than read off the wire, so a rounding that went
				// to nearest could put the answer above it by a few milliamps.
				if budget := budgetAt(p, v); budget > 0 {
					if a := p.CurrentAt(v); a > budget+cmpEps && a < MaxCableCurrentA {
						t.Fatalf("CurrentAt(%v) = %v, above the %v A its power budget allows: %+v", v, a, budget, p)
					}
				}
			}
			if p.Valid && p.Kind == KindUnknown {
				t.Fatalf("reserved APDO subtype marked valid: %+v", p)
			}
			// An invalid EPR AVS object must always have raised the flag.
			if p.Kind == KindEPRAVS && !p.Valid && !l.EPRCableFail {
				t.Fatalf("invalid EPR AVS did not promote EPRCableFail: %+v", p)
			}
		}

		// Evaluate must be total: no panic, and a self-consistent result for
		// any request, including ones no source could satisfy.
		for _, q := range [][2]float64{{0, 0}, {5, 3}, {9, 5}, {13.5, 1}, {20, 5}, {28, 5}, {48, 5}, {1e9, 1e9}} {
			m := l.Evaluate(q[0], q[1])
			if m.Mode == ModeNone && (m.OK || m.MaxCurrentA != 0) {
				t.Fatalf("Evaluate(%v,%v) = %+v: no mode but OK/current set", q[0], q[1], m)
			}
			if m.OK && m.MaxCurrentA+cmpEps < q[1] {
				t.Fatalf("Evaluate(%v,%v) = %+v: OK with insufficient current", q[0], q[1], m)
			}
			// The invariant that matters most: no request, from no log,
			// however malformed, may be answered with a current no cable
			// can carry. Over-reporting is the direction that melts things.
			if m.MaxCurrentA > MaxCableCurrentA+cmpEps {
				t.Fatalf("Evaluate(%v,%v) = %+v: above the %v A ceiling", q[0], q[1], m, MaxCableCurrentA)
			}
			if m.DeclaredMaxCurrentA != 0 && m.DeclaredMaxCurrentA <= m.MaxCurrentA {
				t.Fatalf("Evaluate(%v,%v) = %+v: declared current does not exceed the reported one", q[0], q[1], m)
			}
			if len(m.Messages) == 0 {
				t.Fatalf("Evaluate(%v,%v) = %+v: no explanation", q[0], q[1], m)
			}
		}
	})
}
