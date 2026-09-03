package pdo

import (
	"fmt"
	"math"
	"strconv"
)

// Match is the result of evaluating a requested operating point against a
// scanned source.
type Match struct {
	// OK reports full achievability: a mode was found AND it can supply the
	// requested current.
	OK bool `json:"ok"`
	// Mode names the capability that would be used, or "" if the requested
	// voltage cannot be reached at all. See the Mode* constants.
	Mode string `json:"mode"`
	// MaxCurrentA is the current the chosen mode can supply at the requested
	// voltage, bounded by MaxCableCurrentA. Zero when Mode is "".
	MaxCurrentA float64 `json:"max_current_a"`
	// DeclaredMaxCurrentA is what the chosen capability advertises, set only
	// when MaxCableCurrentA reduced MaxCurrentA below it.
	DeclaredMaxCurrentA float64 `json:"declared_max_current_a,omitempty"`
	// Caveats tags anything that qualifies the verdict: a term that came from a
	// specification rather than from this scan, a scan-time fault that may have
	// hidden capability, or a figure the cable ceiling reduced. Every tag has a
	// human-readable counterpart in Messages; this field exists so that a
	// --json consumer does not have to parse prose to discover that a "yes"
	// came with conditions. See the Caveat* constants.
	Caveats []string `json:"caveats,omitempty"`
	// Messages explains the outcome in terms a user can act on.
	Messages []string `json:"messages,omitempty"`

	// chosen is the capability the verdict rests on, and chosenDeclaredA what
	// that capability advertised before bounding. Recorded by pick and consumed
	// by finish, which is where every caveat common to all branches is applied.
	// Unexported: plumbing, not output.
	chosen          PDO
	chosenDeclaredA float64
	// quotedSPRAVS records that an SPR AVS object was picked at some point, even
	// if a later upgrade moved the verdict off it. The message that object left
	// behind still quotes the assumed 9-20 V range, and the disclosure follows
	// the note rather than the object.
	quotedSPRAVS bool
}

// Modes reported by Evaluate. The "upgrade_" modes mean a higher-current
// capability was selected in preference to the one that would otherwise have
// been used, because the first could not meet the requested current.
const (
	ModeNone                     = ""
	ModeEPRFixed                 = "epr_fixed"
	ModeEPRAVS                   = "epr_avs"
	ModeUpgradeEPRAVSMoreCurrent = "upgrade_epr_avs_more_current"
	ModeSPRFixed                 = "spr_fixed"
	ModeSPRPPS                   = "spr_pps"
	ModeSPRAVS                   = "spr_avs"
	ModeUpgradeSPRPPSMoreCurrent = "upgrade_spr_pps_more_current"
	ModeUpgradeSPRAVSMoreCurrent = "upgrade_spr_avs_more_current"
)

// Caveat tags reported in Match.Caveats.
const (
	// CaveatSPRAVSAssumedRange means the verdict turns on the assumed SPR AVS
	// output range. An SPR AVS APDO carries no voltage range on the wire at all
	// (SPEC.md §9.4), so SPRAVSMinVoltageV..SPRAVSMaxVoltageV is USB-PD 3.2
	// speaking, not this source. A user acting on "yes, 18 V works" is entitled
	// to know which half of that came from their charger. The band split that
	// divides the two current limits (SPRAVSBandSplitV) comes from the same
	// place, so a refusal that turns on which band the request falls in is
	// tagged too.
	CaveatSPRAVSAssumedRange = "spr_avs_range_assumed"

	// CaveatEPRCableFail means the scan recorded an EPR cable failure
	// (SPEC.md §9.3 flags2 bit 3, or an EPR AVS APDO that failed validation),
	// so the capture may not show what the source can really do in Extended
	// Power Range. It qualifies a positive verdict as much as a negative one:
	// the scan was working with a cable that could not enter EPR.
	CaveatEPRCableFail = "epr_cable_fail_recorded"

	// CaveatCableCurrentBound means MaxCurrentA is the cable/hardware ceiling
	// rather than what the capability advertises; DeclaredMaxCurrentA holds the
	// advertised figure.
	CaveatCableCurrentBound = "current_bounded_by_cable"

	// CaveatPPSPowerLimited means the verdict rests on a PPS APDO that sets the
	// USB-PD Power Limited bit (SPEC.md §9.4 does not list it; see
	// decodeAugmented): the source does not hold that APDO's Maximum Current
	// across its whole range, and what it can supply at the requested voltage is
	// bounded by a power budget this scan cannot read. Where the fixed PDOs
	// support an inference the bound is applied and the figure reported is the
	// smaller one; where they do not, the advertised figure stands and may be
	// optimistic. Either way part of the answer comes from a specification
	// rather than from the capture, which is the disclosure
	// CaveatSPRAVSAssumedRange makes for the SPR AVS range.
	CaveatPPSPowerLimited = "pps_power_limited"
)

// cmpEps absorbs floating-point noise in voltage and current comparisons. All
// decoded values are already rounded to two decimals (round2), so anything this
// small is representation error rather than a real difference.
const cmpEps = 1e-6

// knownFixedVoltages are the fixed voltages a USB-PD source may advertise:
// 5/9/12/15/20 V in SPR and 28/36/48 V in EPR.
//
// Unlike the vendor application, this package does not gate on the list — a
// fixed PDO found in the scan is matched whatever its voltage, because the scan
// is ground truth and the list is not. The list is used only to explain a
// failure ("13.5 V is not a standard fixed PD voltage") rather than to cause one.
var knownFixedVoltages = [...]float64{5, 9, 12, 15, 20, 28, 36, 48}

// KnownFixedVoltages returns the standard USB-PD fixed voltages, in volts.
func KnownFixedVoltages() []float64 {
	out := make([]float64, len(knownFixedVoltages))
	copy(out, knownFixedVoltages[:])
	return out
}

// IsKnownFixedVoltage reports whether v is one of the standard USB-PD fixed
// supply voltages.
func IsKnownFixedVoltage(v float64) bool {
	for _, k := range knownFixedVoltages {
		if math.Abs(v-k) < cmpEps {
			return true
		}
	}
	return false
}

// CurrentShort reports whether the requested voltage is reachable but the
// requested current is not. It is the discriminator between "wrong voltage" and
// "not enough amps" without parsing Messages.
func (m Match) CurrentShort() bool { return m.Mode != ModeNone && !m.OK }

// Evaluate reports whether the scanned source can deliver voltageV at currentA.
//
// The algorithm is SPEC.md §9.5, applied to the PDOs this device actually
// scanned rather than to the vendor's cloud model of the charger. Requests above
// EPRThresholdV take the Extended Power Range branch; everything else takes the
// Standard Power Range branch.
//
// A non-empty Mode with OK false means the voltage is reachable but the current
// is not; Mode is "" only when the voltage itself cannot be produced.
func (l *Log) Evaluate(voltageV, currentA float64) Match {
	if l == nil {
		return Match{Messages: []string{"no PDO log available; run a power supply scan first"}}
	}
	if voltageV <= 0 || math.IsNaN(voltageV) || math.IsInf(voltageV, 0) {
		return Match{Messages: []string{"requested voltage must be a positive number of volts"}}
	}
	if currentA < 0 || math.IsNaN(currentA) || math.IsInf(currentA, 0) {
		return Match{Messages: []string{"requested current must be a non-negative number of amps"}}
	}
	if voltageV > EPRThresholdV {
		return l.evaluateEPR(voltageV, currentA)
	}
	return l.evaluateSPR(voltageV, currentA)
}

// evaluateEPR handles requests above 20 V. SPEC.md §9.5, EPR branch.
func (l *Log) evaluateEPR(v, i float64) Match {
	var m Match

	fixed, hasFixed := l.fixedAt(v)
	fixedA := fixed.CurrentAt(v)
	avs, hasAVS := l.eprAVSCovering(v)
	avsA := avs.CurrentAt(v)
	if hasAVS && avsA <= 0 {
		hasAVS = false
	}

	switch {
	case hasFixed && fixedA > 0:
		m.pick(ModeEPRFixed, fixed, v)
		m.Messages = append(m.Messages,
			fmt.Sprintf("EPR fixed %s PDO offers %s", formatV(v), formatA(fixedA)))
		// Upgrade only when the fixed object cannot meet the request and the
		// adjustable one genuinely offers more; otherwise prefer fixed, which
		// needs no PPS/AVS request loop from the firmware.
		if hasAVS && avsA > fixedA+cmpEps && fixedA+cmpEps < i {
			m.pick(ModeUpgradeEPRAVSMoreCurrent, avs, v)
			m.Messages = append(m.Messages,
				fmt.Sprintf("the fixed PDO cannot supply the requested %s; the EPR AVS range %s (%d W) supplies %s at %s",
					formatA(i), formatRange(avs.MinVoltageV, avs.MaxVoltageV), avs.PDPWatts, formatA(avsA), formatV(v)))
		}

	case hasAVS:
		m.pick(ModeEPRAVS, avs, v)
		if IsKnownFixedVoltage(v) {
			m.Messages = append(m.Messages, fmt.Sprintf("no fixed %s PDO offered", formatV(v)))
		}
		m.Messages = append(m.Messages,
			fmt.Sprintf("EPR AVS range %s (%d W) supplies %s at %s",
				formatRange(avs.MinVoltageV, avs.MaxVoltageV), avs.PDPWatts, formatA(avsA), formatV(v)))

	default:
		// Mirror of the SPR branch: a PPS range can decode up to 25.5 V, so it
		// may cover a request just above the 20 V boundary even though PPS is
		// an SPR object class. Check before declaring the voltage unreachable.
		if pps, ok := l.ppsCovering(v); ok && pps.CurrentAt(v) > 0 {
			ppsA := m.pick(ModeSPRPPS, pps, v)
			m.Messages = append(m.Messages,
				fmt.Sprintf("no EPR capability reaches %s, but the PPS range %s covers it and supplies %s",
					formatV(v), formatRange(pps.MinVoltageV, pps.MaxVoltageV), formatA(ppsA)))
			break
		}
		l.appendEPRFailure(&m, v)
	}

	l.finish(&m, v, i)
	return m
}

// eprRequiredMessage is the vendor's own sentence for "this charger is not
// EPR-capable", transcribed in SPEC.md §9.5. It is reproduced verbatim —
// capital R and all — for the same reason as the §9.6 parse errors in pdo.go:
// what a user pastes into a search box is the string the vendor's app printed,
// and a case difference defeats the whole point of copying it. That the spec's
// transcription is faithful rather than sloppy is visible in the next section,
// where ErrEmptyLog's "vFlex" keeps the vendor's own odd capitalisation.
//
// It is the only vendor-authored sentence in this file; every other message here
// is our prose and stays in the lowercase Go style.
const eprRequiredMessage = "Power source with Extended Power Range is Required"

// appendEPRFailure explains why no EPR capability could produce v.
func (l *Log) appendEPRFailure(m *Match, v float64) {
	if !l.hasEPRCapability() {
		// The headline case: an ordinary SPR-only charger.
		m.Messages = append(m.Messages, eprRequiredMessage)
		if l.EPRCableFail {
			m.addCaveat(CaveatEPRCableFail)
			m.Messages = append(m.Messages, "the scan also reported an EPR cable failure: fit an eMarker-equipped 5 A EPR cable and rescan, the source may in fact be EPR-capable")
		}
		return
	}

	// The source does support EPR, so name the specific shortfall.
	switch {
	case !IsKnownFixedVoltage(v):
		m.Messages = append(m.Messages, fmt.Sprintf("%s is not a standard fixed PD voltage (28/36/48 V in EPR); only an adjustable supply can produce it", formatV(v)))
	default:
		m.Messages = append(m.Messages, fmt.Sprintf("no fixed %s PDO offered", formatV(v)))
	}

	if avs, ok := l.eprAVSExplaining(v); ok {
		switch {
		case !avs.Valid:
			m.Messages = append(m.Messages, "the source's EPR AVS PDO failed validation, so its adjustable range is unusable")
		case rangeDistance(avs, v) > cmpEps:
			m.Messages = append(m.Messages, fmt.Sprintf("EPR AVS covers %s only", formatRange(avs.MinVoltageV, avs.MaxVoltageV)))
		default:
			// The range does contain v, so the range is not the obstacle and
			// "covers 15.0-28.0 V only" of a 24 V request would contradict
			// itself. Reaching here means the object yields no current at v,
			// which for an EPR AVS APDO means an empty power budget.
			m.Messages = append(m.Messages, fmt.Sprintf("the EPR AVS range %s does reach %s, but its %d W budget leaves no current to draw there",
				formatRange(avs.MinVoltageV, avs.MaxVoltageV), formatV(v), avs.PDPWatts))
		}
	} else {
		m.Messages = append(m.Messages, "the source offers no EPR AVS range")
	}
	if l.EPRCableFail {
		m.addCaveat(CaveatEPRCableFail)
		m.Messages = append(m.Messages, "the scan reported an EPR cable failure: fit an eMarker-equipped 5 A EPR cable and rescan")
	}
}

// evaluateSPR handles requests at or below 20 V. SPEC.md §9.5, SPR branch.
func (l *Log) evaluateSPR(v, i float64) Match {
	var m Match

	fixed, hasFixed := l.fixedAt(v)
	fixedA := fixed.CurrentAt(v)
	pps, hasPPS := l.ppsCovering(v)
	ppsA := pps.CurrentAt(v)
	avs, hasAVS := l.sprAVSCovering(v)
	avsA := avs.CurrentAt(v)

	switch {
	case hasFixed && fixedA > 0:
		m.pick(ModeSPRFixed, fixed, v)
		m.Messages = append(m.Messages,
			fmt.Sprintf("fixed %s PDO offers %s", formatV(v), formatA(fixedA)))
		if hasPPS && ppsA > fixedA+cmpEps && fixedA+cmpEps < i {
			m.pick(ModeUpgradeSPRPPSMoreCurrent, pps, v)
			m.Messages = append(m.Messages,
				fmt.Sprintf("the fixed PDO cannot supply the requested %s; PPS %s supplies %s",
					formatA(i), formatRange(pps.MinVoltageV, pps.MaxVoltageV), formatA(ppsA)))
		}

	case hasPPS && ppsA > 0:
		m.pick(ModeSPRPPS, pps, v)
		if IsKnownFixedVoltage(v) {
			m.Messages = append(m.Messages, fmt.Sprintf("no fixed %s PDO offered", formatV(v)))
		}
		m.Messages = append(m.Messages,
			fmt.Sprintf("PPS %s supplies %s at %s", formatRange(pps.MinVoltageV, pps.MaxVoltageV), formatA(ppsA), formatV(v)))
		// Unlike the fixed->PPS upgrade above, SPEC.md §9.5 applies no
		// "only if PPS is short of the request" condition here: any SPR AVS
		// offering more current wins. The switch of capability also switches
		// the verdict onto the assumed SPR AVS range, which finish discloses.
		if hasAVS && avsA > ppsA+cmpEps {
			m.pick(ModeUpgradeSPRAVSMoreCurrent, avs, v)
			m.Messages = append(m.Messages,
				fmt.Sprintf("SPR AVS supplies more: %s", formatA(avsA)))
		}

	case hasAVS && avsA > 0:
		m.pick(ModeSPRAVS, avs, v)
		if IsKnownFixedVoltage(v) {
			m.Messages = append(m.Messages, fmt.Sprintf("no fixed %s PDO offered", formatV(v)))
		}
		m.Messages = append(m.Messages,
			fmt.Sprintf("SPR AVS supplies %s at %s", formatA(avsA), formatV(v)))

	default:
		// Nothing in the SPR classes covers v. Before declaring failure, look
		// at the EPR side: an EPR AVS range routinely starts at 15 V, so it can
		// cover an SPR-range request. Partitioning candidates by the *requested*
		// voltage would hide it and produce a confident wrong answer -- "18 V is
		// not achievable" from a log that plainly contains a 15-28 V range.
		if eavs, ok := l.eprAVSCovering(v); ok {
			if a := eavs.CurrentAt(v); a > 0 {
				m.pick(ModeEPRAVS, eavs, v)
				m.Messages = append(m.Messages,
					fmt.Sprintf("no SPR capability reaches %s, but the EPR AVS range %s (%d W) covers it and supplies %s",
						formatV(v), formatRange(eavs.MinVoltageV, eavs.MaxVoltageV), eavs.PDPWatts, formatA(a)))
				break
			}
		}
		l.appendSPRFailure(&m, v)
	}

	// The same look at the EPR side, for the case where an SPR object did cover
	// v but cannot supply the request. SPEC.md §17 (row 9.5) states the rule as
	// "any object whose decoded range covers the request is considered", and the
	// default arm above was applying it only where nothing else covered v at
	// all: a 140 W charger advertising fixed 15 V 3 A alongside an EPR AVS
	// 15.0-28.0 V range answered "no, 3.00 A" at 15 V and "yes, 5.00 A" at 18 V
	// from one and the same scan, because 18 V happened to have no fixed object.
	// Applied once, after the switch, because all three arms could pick the
	// weaker object; the condition is the EPR branch's own (evaluateEPR above) —
	// upgrade only when what was picked is short of the request AND the
	// adjustable object genuinely offers more — so a plain SPR object still wins
	// whenever it can do the job, and no verdict acquires an EPR cable
	// requirement it did not need.
	if m.Mode != ModeNone && m.chosen.Kind != KindEPRAVS && m.MaxCurrentA+cmpEps < i {
		if eavs, ok := l.eprAVSCovering(v); ok {
			if a := eavs.CurrentAt(v); a > m.MaxCurrentA+cmpEps {
				m.pick(ModeUpgradeEPRAVSMoreCurrent, eavs, v)
				m.Messages = append(m.Messages,
					fmt.Sprintf("no SPR capability supplies the requested %s at %s; the EPR AVS range %s (%d W) supplies %s",
						formatA(i), formatV(v), formatRange(eavs.MinVoltageV, eavs.MaxVoltageV), eavs.PDPWatts, formatA(a)))
			}
		}
	}

	l.finish(&m, v, i)
	return m
}

// appendSPRFailure explains why no SPR capability could produce v.
func (l *Log) appendSPRFailure(m *Match, v float64) {
	if !IsKnownFixedVoltage(v) {
		m.Messages = append(m.Messages, fmt.Sprintf("%s is not a standard fixed PD voltage (5/9/12/15/20 V in SPR); only a PPS or AVS source can produce it", formatV(v)))
	} else {
		m.Messages = append(m.Messages, fmt.Sprintf("no fixed %s PDO offered", formatV(v)))
	}

	ppsSeen := false
	for _, p := range l.PDOs {
		if p.Kind == KindPPS && p.Valid {
			ppsSeen = true
			m.Messages = append(m.Messages, fmt.Sprintf("PPS covers %s only", formatRange(p.MinVoltageV, p.MaxVoltageV)))
		}
	}
	if !ppsSeen {
		m.Messages = append(m.Messages, "the source offers no PPS range")
	}

	if avs, ok := l.sprAVSExplaining(v); ok {
		note, assumed := sprAVSFailureNote(avs, v)
		if assumed {
			// This is the third place the assumed range decides an answer, and
			// the answer here is "no": the refusal rests on a range no APDO ever
			// transmitted, or on the band split that comes with it, rather than
			// on anything the scan saw (SPEC.md §9.4). The tag follows the note
			// rather than the object, so a refusal that owes nothing to the
			// assumption -- an APDO with no current at all -- does not claim it.
			m.addCaveat(CaveatSPRAVSAssumedRange)
		}
		m.Messages = append(m.Messages, note)
	} else {
		m.Messages = append(m.Messages, "the source offers no SPR AVS range")
	}

	// A refusal from 15 V up is precisely the refusal a cable fault can cause:
	// an EPR AVS range routinely starts at 15 V and covers the SPR band from
	// there (SPEC.md §17, row 9.5), and a scan that could not enter Extended
	// Power Range may simply never have seen it. Its EPR counterpart says this
	// on both of its branches (appendEPRFailure); finish cannot, because a
	// refusal never reaches the block that would.
	//
	// Gated on the voltage, and not merely on the flag: below 15 V no EPR AVS
	// object could have covered the request whatever the cable did, so citing it
	// there would invent a diagnosis the log does not support.
	if l.EPRCableFail && v+cmpEps >= eprAVSLowestMinVoltageV {
		m.addCaveat(CaveatEPRCableFail)
		m.Messages = append(m.Messages, "the scan also reported an EPR cable failure: an EPR AVS range (15 V and up) may have been hidden; fit an eMarker-equipped 5 A EPR cable and rescan")
	}
}

// eprAVSLowestMinVoltageV is the lowest voltage an EPR AVS range can start at.
// USB-PD puts the floor of the EPR AVS operating range at 15 V, which is what
// makes such a range the usual answer to a 15-20 V request and the thing a
// failed EPR entry is most likely to have hidden.
const eprAVSLowestMinVoltageV = 15.0

// sprAVSFailureNote says why the SPR AVS APDO that best describes this source
// cannot produce v, and reports whether the answer leans on the assumed range or
// band split so the caller can tag it.
//
// Which shape applies matters: "SPR AVS covers 9.0-20.0 V only" is the right
// answer for a 7 V request and a self-contradiction for a 12 V one. An SPR AVS
// APDO carries no range on the wire, only two band-specific current limits
// (SPEC.md §9.4), and either band may be zero — so a source that refuses 12 V
// usually refuses it because its 9-15 V band is empty, not because 12 V is
// outside anything.
func sprAVSFailureNote(p PDO, v float64) (note string, assumed bool) {
	lower, upper := sprAVSBands(p)

	switch {
	case lower <= 0 && upper <= 0:
		// Nothing in either band, which is exactly what makes such an APDO
		// invalid (SPEC.md §9.4). The assumed range never enters into the
		// refusal, so do not cite it as though it were the obstacle.
		return fmt.Sprintf("the source's SPR AVS APDO declares no current in either band, so it offers nothing at %s", formatV(v)), false

	case !p.Valid:
		// Current in a band but marked invalid: not something decodePDO can
		// produce, so say only what can be relied on.
		return "the source's SPR AVS APDO failed validation, so nothing it declares can be relied on", false

	case v+cmpEps < SPRAVSMinVoltageV || v-cmpEps > SPRAVSMaxVoltageV:
		return fmt.Sprintf("SPR AVS covers %s only, and that range is assumed rather than scanned: %s",
			formatRange(SPRAVSMinVoltageV, SPRAVSMaxVoltageV), SPRAVSAssumptionClause), true

	default:
		// v is inside the assumed range, so the range is not what refused it:
		// the band containing v declares no current. Name the other band, which
		// is the actionable part — a user asking for 12 V from an APDO with only
		// a 15-20 V band can ask for 18 V instead.
		empty, other, otherA := sprAVSBandDesc(v, lower, upper)
		return fmt.Sprintf("the SPR AVS APDO declares no current in the %s band, so it cannot supply %s; it offers %s in the %s band only, and those bands are assumed rather than scanned: %s",
			empty, formatV(v), formatA(otherA), other, SPRAVSAssumptionClause), true
	}
}

// sprAVSBands returns the APDO's two band limits, lower (9-15 V) first, each
// re-bounded by MaxCableCurrentA so that a PDO which did not come from
// decodePDO cannot put an unbounded figure into a message; see reportable.
func sprAVSBands(p PDO) (lower, upper float64) {
	lower, _ = reportable(p.MaxCurrent15VA, p.DeclaredMaxCurrent15VA)
	upper, _ = reportable(p.MaxCurrent20VA, p.DeclaredMaxCurrent20VA)
	return lower, upper
}

// sprAVSBandDesc names the band containing v and the other band, with the other
// band's current. The split is SPRAVSBandSplitV, matching PDO.currentAt.
func sprAVSBandDesc(v, lower, upper float64) (containing, other string, otherA float64) {
	loBand := formatRange(SPRAVSMinVoltageV, SPRAVSBandSplitV)
	hiBand := formatRange(SPRAVSBandSplitV, SPRAVSMaxVoltageV)
	if v > SPRAVSBandSplitV {
		return hiBand, loBand, lower
	}
	return loBand, hiBand, upper
}

// pick records the capability a branch selected and returns the current usable
// at v.
//
// Every branch of Evaluate must set its mode through pick. It is the one place a
// PDO becomes a verdict, so a branch cannot report a current that skipped
// PDO.CurrentAt's bounding, and the caveats that must accompany a given kind of
// capability are attached by finish from what pick recorded rather than by each
// branch remembering to add them.
func (m *Match) pick(mode string, p PDO, v float64) float64 {
	usable, declared := p.currentAt(v)
	m.Mode = mode
	m.MaxCurrentA = usable
	m.chosen = p
	m.chosenDeclaredA = declared
	if p.Kind == KindSPRAVS {
		m.quotedSPRAVS = true
	}
	return usable
}

// addCaveat records a tag once.
func (m *Match) addCaveat(tag string) {
	for _, c := range m.Caveats {
		if c == tag {
			return
		}
	}
	m.Caveats = append(m.Caveats, tag)
}

// finish turns the capability a branch picked into the verdict.
//
// Everything that must hold for EVERY answer lives here rather than in the
// branches: the current ceiling, the disclosures that qualify a "yes", and the
// shortfall message. A branch that forgets one of them cannot exist, which is
// the point — the two review findings this replaces were both a single branch
// missing a bound or a caveat the others had.
//
// SPEC.md §9.5 decides voltage achievability only; reporting the current
// shortfall separately is what makes the result actionable.
func (l *Log) finish(m *Match, v, i float64) {
	if m.Mode == ModeNone {
		m.OK = false
		m.MaxCurrentA = 0
		m.DeclaredMaxCurrentA = 0
		return
	}

	// Backstop. pick already went through PDO.currentAt, so this normally
	// changes nothing; it is here so that a future branch assigning MaxCurrentA
	// directly still cannot report a current no cable can carry.
	bounded, declared := boundCurrent(m.MaxCurrentA)
	m.MaxCurrentA = bounded
	if declared == 0 {
		declared = m.chosenDeclaredA
	}
	if declared > m.MaxCurrentA+cmpEps {
		m.DeclaredMaxCurrentA = declared
		m.addCaveat(CaveatCableCurrentBound)
		m.Messages = append(m.Messages, cableBoundNote(m.chosen, declared))
	}

	// An SPR AVS APDO carries no voltage range on the wire (SPEC.md §9.4), so
	// any verdict resting on one rests partly on USB-PD 3.2 rather than on the
	// scan. Disclosed here so that all three deciding paths -- spr_avs,
	// upgrade_spr_avs_more_current, and the refusal in appendSPRFailure -- say
	// it, instead of only the first. quotedSPRAVS keeps it attached when a later
	// upgrade moved the verdict off the SPR AVS object: the message it left
	// above still quotes what the assumed range offers at v.
	if m.chosen.Kind == KindSPRAVS || m.quotedSPRAVS {
		m.addCaveat(CaveatSPRAVSAssumedRange)
		m.Messages = append(m.Messages, sprAVSAssumptionNote(v))
	}

	// A PPS APDO that declared itself power limited holds its Maximum Current
	// only where the source's budget allows, and that budget is not in the log.
	// Said here rather than in the branches for the same reason as the rest of
	// this function -- spr_pps and upgrade_spr_pps_more_current are two paths to
	// one object, and a disclosure only one of them made would be worse than
	// none.
	if m.chosen.Kind == KindPPS && m.chosen.PPSPowerLimited {
		if note, disclose := ppsPowerLimitedNote(m.chosen, v, m.MaxCurrentA); disclose {
			m.addCaveat(CaveatPPSPowerLimited)
			m.Messages = append(m.Messages, note)
		}
	}

	// EPR operation, whether because the request itself is above 20 V or
	// because the capability chosen for a lower request is an EPR object.
	if v > EPRThresholdV || m.chosen.EPR {
		if v > EPRThresholdV {
			// SPEC.md §13.9: anything above 20 V needs an eMarked EPR cable, and
			// a fast-blinking red LED is the device telling the user it does not
			// have one (SPEC.md §1.1).
			m.Messages = append(m.Messages, eprCableAdvisory)
		} else {
			// Below 20 V but on an EPR object: a source offers those only once
			// it is in Extended Power Range, so the cable requirement follows
			// the capability rather than the output voltage.
			m.Messages = append(m.Messages, eprObjectCableAdvisory)
		}
		if l.EPRCableFail {
			// The failure paths already say this; the success path used to drop
			// it, which is precisely where the user is about to act on "yes".
			m.addCaveat(CaveatEPRCableFail)
			m.Messages = append(m.Messages, eprCableFailCaveat)
		}
	}

	m.OK = m.MaxCurrentA+cmpEps >= i
	if !m.OK {
		m.Messages = append(m.Messages,
			fmt.Sprintf("requested %s exceeds the %s available at %s", formatA(i), formatA(m.MaxCurrentA), formatV(v)))
	}
}

// Advisories attached by finish. All are about EPR operation, which the device
// signals with a fast-blinking red LED when the cable cannot support it.
const (
	eprCableAdvisory = "output above 20 V requires an eMarker-equipped 5 A EPR cable; " +
		"a fast-blinking red LED means the cable cannot support it"
	eprObjectCableAdvisory = "this is an EPR capability, which a source offers only once it is in " +
		"Extended Power Range, so an eMarker-equipped 5 A EPR cable is required even below 20 V; " +
		"a fast-blinking red LED means the cable cannot support it"
	eprCableFailCaveat = "this scan recorded an EPR cable failure, so it may not have seen everything " +
		"the source can do in EPR: the verdict above rests only on what was captured with a cable " +
		"that could not enter Extended Power Range. Fit an eMarker-equipped 5 A EPR cable and rescan to confirm"
)

// cableBoundNote explains a reported current that is the cable ceiling rather
// than what the capability advertises.
func cableBoundNote(p PDO, declared float64) string {
	if p.Kind == KindEPRAVS {
		return fmt.Sprintf("the %d W budget alone would allow %s here, but no USB-C cable is rated above %s",
			p.PDPWatts, formatA(declared), formatA(MaxCableCurrentA))
	}
	return fmt.Sprintf("the PDO advertises %s, but no USB-C cable is rated above %s and the VFLEX passes no more than that, so %s is the most that can be drawn",
		formatA(declared), formatA(MaxCableCurrentA), formatA(MaxCableCurrentA))
}

// sprAVSAssumptionNote states, for a verdict that rests on an SPR AVS APDO, how
// much of the answer came from the scan and how much from the specification.
func sprAVSAssumptionNote(v float64) string {
	return fmt.Sprintf("the assumed %s output range is not scanned data: %s, so reaching %s is what USB-PD 3.2 says such a source does rather than something this scan observed",
		formatRange(SPRAVSMinVoltageV, SPRAVSMaxVoltageV), SPRAVSAssumptionClause, formatV(v))
}

// ppsPowerLimitedNote says what a PPS APDO's Power Limited bit cost this
// verdict, and reports whether there is anything to disclose at all.
//
// There are two things to say and they are different answers. With a budget
// inferred from the fixed PDOs, the reported current may be smaller than the
// APDO advertises and the user is owed the arithmetic and its provenance. With
// no budget to infer, nothing was reduced and the advertised figure is the one
// reported — which is exactly when it may be optimistic, so the disclosure
// matters most. Where a budget was inferred and does not bite at v, the answer
// owes the bit nothing and saying so would be noise.
func ppsPowerLimitedNote(p PDO, v, reported float64) (note string, disclose bool) {
	advertised, _ := reportable(p.MaxCurrentA, p.DeclaredMaxCurrentA)
	switch {
	case p.PPSBudgetW <= 0:
		return fmt.Sprintf("this PPS APDO sets the USB-PD Power Limited bit, so the source does not hold %s across the whole %s range; %s, and this source's fixed PDOs do not say what it is, so %s may be optimistic near the top of the range",
			formatA(advertised), formatRange(p.MinVoltageV, p.MaxVoltageV), ppsBudgetClause, formatA(reported)), true
	case reported+cmpEps < advertised:
		return fmt.Sprintf("this PPS APDO sets the USB-PD Power Limited bit, so its %s applies only where the source's power budget allows: %d W permits %s at %s. That budget is inferred from the source's own fixed PDOs, not scanned — %s",
			formatA(advertised), p.PPSBudgetW, formatA(reported), formatV(v), ppsBudgetClause), true
	default:
		return "", false
	}
}

// ppsBudgetClause is the shared half-sentence for why a PPS power budget is
// never read from the scan, so the two shapes of the disclosure above agree
// about what is missing and why.
const ppsBudgetClause = "a source's PDP travels in Source_Capabilities_Extended, which this scan does not capture (SPEC.md §9.3)"

// SPRAVSAssumptionClause is the shared half-sentence, so the disclosure reads
// identically wherever the assumption decides an answer. Exported because the
// capability table is rendered outside this package and the "?" it puts on an
// SPR AVS voltage range has to be explained in these same words: a mark whose
// meaning is spelled out differently in two places is two claims, not one.
const SPRAVSAssumptionClause = "an SPR AVS APDO carries no voltage range on the wire, only its two current limits (SPEC.md §9.4)"

// ---------------------------------------------------------------------------
// Capability lookups. Each returns the most capable matching object, so a source
// advertising several overlapping ranges is judged by its best one.
// ---------------------------------------------------------------------------

// fixedAt returns the valid fixed PDO whose voltage is exactly v.
func (l *Log) fixedAt(v float64) (PDO, bool) {
	var best PDO
	found := false
	for _, p := range l.PDOs {
		if p.Kind != KindFixed || !p.Valid {
			continue
		}
		if math.Abs(p.VoltageV-v) > cmpEps {
			continue
		}
		if !found || p.CurrentAt(v) > best.CurrentAt(v) {
			best, found = p, true
		}
	}
	return best, found
}

// ppsCovering returns the valid SPR PPS APDO whose range contains v.
func (l *Log) ppsCovering(v float64) (PDO, bool) {
	return l.bestCovering(KindPPS, v, func(p PDO) bool {
		return v+cmpEps >= p.MinVoltageV && v-cmpEps <= p.MaxVoltageV
	})
}

// eprAVSCovering returns the valid EPR AVS APDO whose range contains v.
func (l *Log) eprAVSCovering(v float64) (PDO, bool) {
	return l.bestCovering(KindEPRAVS, v, func(p PDO) bool {
		return v+cmpEps >= p.MinVoltageV && v-cmpEps <= p.MaxVoltageV
	})
}

// sprAVSCovering returns the valid SPR AVS APDO usable at v.
//
// An SPR AVS APDO carries no voltage range on the wire, so the USB-PD 3.2 range
// is assumed; see SPRAVSMinVoltageV.
func (l *Log) sprAVSCovering(v float64) (PDO, bool) {
	return l.bestCovering(KindSPRAVS, v, func(PDO) bool {
		return v+cmpEps >= SPRAVSMinVoltageV && v-cmpEps <= SPRAVSMaxVoltageV
	})
}

// bestCovering returns the valid PDO of the given kind that covers v and offers
// the most current.
func (l *Log) bestCovering(k Kind, v float64, covers func(PDO) bool) (PDO, bool) {
	var best PDO
	found := false
	for _, p := range l.PDOs {
		if p.Kind != k || !p.Valid || !covers(p) {
			continue
		}
		// PDO.CurrentAt ranks every kind correctly at v: band-aware for SPR AVS,
		// PDP/v for the power-limited EPR AVS, and bounded for all of them.
		if !found || p.CurrentAt(v) > best.CurrentAt(v) {
			best, found = p, true
		}
	}
	return best, found
}

// hasEPRCapability reports whether the source advertised anything in the
// Extended Power Range: a fixed PDO above 20 V, or an EPR AVS APDO. An invalid
// EPR AVS APDO still counts — the source intended EPR and something (usually the
// cable) prevented it, which is a different diagnosis from an SPR-only charger.
func (l *Log) hasEPRCapability() bool {
	for _, p := range l.PDOs {
		if p.Kind == KindEPRAVS {
			return true
		}
		if p.Kind == KindFixed && p.Valid && p.EPR {
			return true
		}
	}
	return false
}

// eprAVSExplaining returns the EPR AVS APDO that best explains why v could not
// be produced, valid or not.
//
// Taking simply the first one buries the answer: a log holding an invalid APDO
// ahead of a valid 15.0-28.0 V one reports "the EPR AVS PDO failed validation"
// and never mentions the range that does exist, so a user asking for 36 V is
// told to change cables when the real answer is that 28 V is the ceiling.
//
// Preference order: a valid object over an invalid one, since only a valid one
// carries a usable range; then the one whose range comes closest to v, since
// that is the limit the user has to work around; then the larger power budget.
func (l *Log) eprAVSExplaining(v float64) (PDO, bool) {
	var best PDO
	found := false
	for _, p := range l.PDOs {
		if p.Kind != KindEPRAVS {
			continue
		}
		if !found || betterEPRAVSExplanation(p, best, v) {
			best, found = p, true
		}
	}
	return best, found
}

// betterEPRAVSExplanation reports whether cand explains a failure at v better
// than best does.
func betterEPRAVSExplanation(cand, best PDO, v float64) bool {
	if cand.Valid != best.Valid {
		return cand.Valid
	}
	if !cand.Valid {
		// One failed APDO says exactly as much as another: the cable is the
		// diagnosis either way.
		return false
	}
	dc, db := rangeDistance(cand, v), rangeDistance(best, v)
	if math.Abs(dc-db) > cmpEps {
		return dc < db
	}
	return cand.PDPWatts > best.PDPWatts
}

// rangeDistance is how far v lies outside p's advertised range, in volts, and
// zero inside it.
func rangeDistance(p PDO, v float64) float64 {
	switch {
	case v < p.MinVoltageV:
		return p.MinVoltageV - v
	case v > p.MaxVoltageV:
		return v - p.MaxVoltageV
	}
	return 0
}

// sprAVSExplaining returns the SPR AVS APDO that best explains why v could not
// be produced, valid or not.
//
// The SPR counterpart of eprAVSExplaining, and the same defect: taking the first
// object in log order lets a useless one speak for a source that also advertised
// a capable one. It bites differently here, because an SPR AVS APDO has no range
// to quote — what it has is two band-specific current limits, either of which may
// be zero (SPEC.md §9.4). An APDO whose 9-15 V band is empty refuses a 12 V
// request, and the old message, "SPR AVS covers 9.0-20.0 V only", was then simply
// false: 12 V is inside that range. Picking the object with capability, and
// describing the band that actually refused, is what makes the message
// actionable.
//
// Preference order: valid over invalid, since only a valid object has a band
// worth describing; then the most current at v; then the most current in either
// band, so the source is represented by its best object.
func (l *Log) sprAVSExplaining(v float64) (PDO, bool) {
	var best PDO
	found := false
	for _, p := range l.PDOs {
		if p.Kind != KindSPRAVS {
			continue
		}
		if !found || betterSPRAVSExplanation(p, best, v) {
			best, found = p, true
		}
	}
	return best, found
}

// betterSPRAVSExplanation reports whether cand explains a failure at v better
// than best does.
func betterSPRAVSExplanation(cand, best PDO, v float64) bool {
	if cand.Valid != best.Valid {
		return cand.Valid
	}
	if !cand.Valid {
		// One APDO with no current in either band says as much as another.
		return false
	}
	// CurrentAt is band-aware, so this ranks by what each object offers at the
	// requested voltage; on the failure path that is normally zero for all of
	// them, which is exactly why the tie-break below exists.
	if ca, cb := cand.CurrentAt(v), best.CurrentAt(v); math.Abs(ca-cb) > cmpEps {
		return ca > cb
	}
	candLo, candHi := sprAVSBands(cand)
	bestLo, bestHi := sprAVSBands(best)
	return math.Max(candLo, candHi) > math.Max(bestLo, bestHi)
}

// ---------------------------------------------------------------------------
// Formatting helpers for the verdict's prose.
// ---------------------------------------------------------------------------

// formatV renders a requested or fixed voltage with no trailing zeros: "9 V",
// "13.5 V", "3.3 V".
func formatV(v float64) string {
	return strconv.FormatFloat(math.Round(v*1000)/1000, 'g', -1, 64) + " V"
}

// formatA renders a current to two decimals, matching the wire resolution.
func formatA(a float64) string { return fmt.Sprintf("%.2f A", a) }

// formatRange renders an adjustable range at the 100 mV resolution those PDOs
// actually carry: "3.3-11.0 V".
func formatRange(lo, hi float64) string { return fmt.Sprintf("%.1f-%.1f V", lo, hi) }
