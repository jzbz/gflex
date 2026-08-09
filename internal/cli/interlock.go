package cli

import (
	"fmt"

	"github.com/jzbz/gflex/internal/proto"
)

// The safety interlocks of SPEC.md §13.
//
// This device drives a power rail into someone's guitar pedal and the vendor
// application performs no range validation at all — `Math.round(1000*volts)`
// with no clamp, no minimum and no maximum (SPEC.md §6.5). Everything that
// stands between a typo and destroyed hardware lives in this file.
//
// Every function here is pure: it takes the state already read from the device
// plus the requested value and returns a decision. No I/O, no prompting, no
// process state. That is what makes the interlocks testable without hardware,
// and it keeps the "what is allowed" question in one readable place.

// maxWireValue is the largest value the 16-bit voltage and current fields can
// hold. The vendor app truncates silently above this, so 66000 mV would be
// written as 464 mV — a failure mode that is invisible until something is
// already broken.
const maxWireValue = 65535

// Decision is the outcome of an interlock evaluation.
type Decision struct {
	// Refused is a non-empty explanation when the operation must not proceed.
	Refused string
	// Confirm reports that the operation needs an interactive "yes", or --yes
	// when stdin is not a terminal.
	Confirm bool
	// Prompt is the question to put to the user when Confirm is set.
	Prompt string
	// Warnings are advisories to print before proceeding. They never block.
	Warnings []string
}

// OK reports whether the operation may proceed, possibly after confirmation.
func (d Decision) OK() bool { return d.Refused == "" }

// warn appends an advisory to the decision.
func (d *Decision) warn(format string, args ...any) {
	d.Warnings = append(d.Warnings, fmt.Sprintf(format, args...))
}

// eprWarning is the advisory required above 20 V by SPEC.md §13.9. The LED
// mapping comes from SPEC.md §1.1: fast-blinking red means exactly this.
const eprWarning = "above 20 V this needs an eMarker-equipped 5 A cable and an EPR-capable source.\n" +
	"  A fast-blinking red LED on the VFLEX means precisely that combination is missing."

// VoltageRequest is the state an output-voltage write is judged against.
type VoltageRequest struct {
	// Mv is the requested output voltage in millivolts, unclamped and
	// possibly out of range — that is what is being decided.
	Mv int
	// LimitLowMv and LimitHighMv are the user voltage limits read back from
	// the device with CMD_USER_VLIMIT. They are meaningful only when Limits
	// is LimitsValid.
	LimitLowMv, LimitHighMv uint16
	// Limits reports what is known about the device's configured window.
	Limits LimitState
	// IgnoreDeviceLimits is the explicit --ignore-device-limits override. It
	// is deliberately NOT --yes: SPEC.md §13.7 designates --yes as the routine
	// scripting path, so it must never be able to discard the owner's own
	// guard rail. Proceeding without a readable window takes a separate,
	// self-describing flag that nobody passes by habit.
	IgnoreDeviceLimits bool
}

// LimitState describes what the CMD_USER_VLIMIT read told us.
type LimitState int

const (
	// LimitsUnread means the read failed. The protocol has no NACK, so this is
	// an ordinary transient (SPEC.md §5.2), not an exotic condition.
	LimitsUnread LimitState = iota
	// LimitsValid means a usable window was read.
	LimitsValid
	// LimitsMalformed means the device answered but the pair cannot bound
	// anything: high does not exceed low.
	LimitsMalformed
)

// CheckVoltage applies interlocks 1, 2 and 9 of SPEC.md §13 to a voltage write.
//
// The user limits are checked first because they are the guard rail the owner
// configured for their own load; the documented 3300-48000 mV hardware envelope
// is a second, independent bound.
func CheckVoltage(r VoltageRequest) Decision {
	var d Decision

	switch {
	case r.Mv < 0:
		d.Refused = fmt.Sprintf("%s is negative", formatMvInt(r.Mv))
		return d
	case r.Mv > maxWireValue:
		// Interlock 1: the field is 16 bits wide, so the vendor app would
		// write this value wrapped and the device would happily obey.
		d.Refused = fmt.Sprintf(
			"%d mV does not fit the 16-bit wire field (max %d mV) and would silently wrap to %d mV.\n"+
				"  If you meant millivolts, that is above the %d mV hardware maximum anyway; "+
				"a bare number is read as volts, so use e.g. \"12\" or \"12000mV\"",
			r.Mv, maxWireValue, r.Mv&maxWireValue, proto.HardwareMaxVoltageMv)
		return d
	case r.Mv < int(proto.HardwareMinVoltageMv):
		d.Refused = fmt.Sprintf("%s is below the %s hardware minimum",
			formatMvInt(r.Mv), formatMv(proto.HardwareMinVoltageMv))
		return d
	case r.Mv > int(proto.HardwareMaxVoltageMv):
		d.Refused = fmt.Sprintf("%s is above the %s hardware maximum",
			formatMvInt(r.Mv), formatMv(proto.HardwareMaxVoltageMv))
		return d
	}

	// Interlock 1. SPEC.md §13.1 makes the device's own window an unconditional
	// bound: "read CMD_USER_VLIMIT first and refuse anything outside it". It
	// offers no fallback, and for good reason — the window is the one limit the
	// owner chose for their own load, so a narrow one is the case that matters
	// most, not a suspicious one. Anything less than a refusal here would mean
	// a single dropped frame silently downgrades a 5 V ceiling to 48 V.
	switch r.Limits {
	case LimitsValid:
		if r.Mv < int(r.LimitLowMv) || r.Mv > int(r.LimitHighMv) {
			d.Refused = fmt.Sprintf(
				"%s is outside this unit's configured voltage limits [%d, %d] mV.\n"+
					"  Widen them first with: gflex vlimit set --low <mV> --high <mV> --yes",
				formatMvInt(r.Mv), r.LimitLowMv, r.LimitHighMv)
			return d
		}
	case LimitsMalformed:
		if !r.IgnoreDeviceLimits {
			d.Refused = fmt.Sprintf(
				"this unit reports an unusable voltage window (low=%d high=%d mV), so the limit\n"+
					"  that protects your load cannot be applied. Repair it first with:\n"+
					"    gflex vlimit set --low 3300 --high 48000 --yes\n"+
					"  or, to write this voltage anyway against only the %d-%d mV hardware envelope:\n"+
					"    add --ignore-device-limits",
				r.LimitLowMv, r.LimitHighMv, proto.HardwareMinVoltageMv, proto.HardwareMaxVoltageMv)
			return d
		}
		d.warn("proceeding with an unusable device window (low=%d high=%d mV); only the documented %d-%d mV envelope was enforced",
			r.LimitLowMv, r.LimitHighMv, proto.HardwareMinVoltageMv, proto.HardwareMaxVoltageMv)
	default: // LimitsUnread
		if !r.IgnoreDeviceLimits {
			d.Refused = fmt.Sprintf(
				"this unit's configured voltage limits could not be read, so the limit that\n"+
					"  protects your load cannot be applied. Retry, or to write this voltage\n"+
					"  against only the %d-%d mV hardware envelope: add --ignore-device-limits",
				proto.HardwareMinVoltageMv, proto.HardwareMaxVoltageMv)
			return d
		}
		d.warn("user voltage limits could not be read; only the documented %d-%d mV envelope was enforced",
			proto.HardwareMinVoltageMv, proto.HardwareMaxVoltageMv)
	}

	if r.Mv > int(proto.EPRThresholdMv) {
		d.warn("%s", eprWarning)
	}

	// Interlock 2: anything above the 5 V factory default can destroy a load
	// that was only ever meant to see 9 V, so it is confirmed every time.
	if r.Mv > int(proto.DefaultVoltageMv) {
		d.Confirm = true
		d.Prompt = fmt.Sprintf("Set the output to %s? Anything connected will see this voltage.", formatMvInt(r.Mv))
	}
	return d
}

// CheckCurrent judges a current-limit write.
//
// This value is a PD negotiation request, not a measurement — the hardware has
// no current sensing at all (SPEC.md §6.5) — so the risk here is a wrapped
// 16-bit field rather than a destroyed load.
func CheckCurrent(ma int) Decision {
	var d Decision
	switch {
	case ma < 0:
		d.Refused = fmt.Sprintf("%s is negative", formatMaInt(ma))
	case ma > maxWireValue:
		d.Refused = fmt.Sprintf(
			"%d mA does not fit the 16-bit wire field (max %d mA) and would silently wrap to %d mA.\n"+
				"  A bare number is read as amps, so use e.g. \"3\" or \"3000mA\"",
			ma, maxWireValue, ma&maxWireValue)
	case ma > int(proto.HardwareMaxCurrentMa):
		d.Refused = fmt.Sprintf("%s is above the %s maximum pass-through current",
			formatMaInt(ma), formatMa(proto.HardwareMaxCurrentMa))
	case ma == 0:
		d.warn("0 mA requests no current at all; the source may refuse to negotiate")
	}
	return d
}

// VLimitRequest is the state a user-voltage-limit write is judged against.
type VLimitRequest struct {
	// NewLowMv and NewHighMv are the requested limits.
	NewLowMv, NewHighMv int
	// CurLowMv and CurHighMv are the limits currently stored on the device.
	CurLowMv, CurHighMv uint16
	// CurKnown is false when the current pair could not be read.
	CurKnown bool
}

// CheckVLimit applies interlock 3 of SPEC.md §13.
//
// Narrowing the window is always safe — it only adds protection. Widening it
// removes the guard rail that interlock 1 depends on, so it needs an explicit
// yes.
func CheckVLimit(r VLimitRequest) Decision {
	var d Decision

	switch {
	case r.NewLowMv < 0 || r.NewHighMv < 0:
		d.Refused = "voltage limits cannot be negative"
		return d
	case r.NewLowMv > maxWireValue || r.NewHighMv > maxWireValue:
		d.Refused = fmt.Sprintf("voltage limits must fit the 16-bit wire field (max %d mV); got low=%d high=%d",
			maxWireValue, r.NewLowMv, r.NewHighMv)
		return d
	case r.NewHighMv <= r.NewLowMv:
		d.Refused = fmt.Sprintf("high limit %s must be above the low limit %s",
			formatMvInt(r.NewHighMv), formatMvInt(r.NewLowMv))
		return d
	case r.NewLowMv < int(proto.HardwareMinVoltageMv):
		d.Refused = fmt.Sprintf("low limit %s is below the %s hardware minimum",
			formatMvInt(r.NewLowMv), formatMv(proto.HardwareMinVoltageMv))
		return d
	case r.NewHighMv > int(proto.HardwareMaxVoltageMv):
		d.Refused = fmt.Sprintf("high limit %s is above the %s hardware maximum",
			formatMvInt(r.NewHighMv), formatMv(proto.HardwareMaxVoltageMv))
		return d
	}

	if r.NewHighMv > int(proto.EPRThresholdMv) {
		d.warn("%s", eprWarning)
	}

	if !r.CurKnown {
		// Without the old pair we cannot tell widening from narrowing, so we
		// assume the dangerous case.
		d.Confirm = true
		d.Prompt = fmt.Sprintf("Set the voltage limits to [%d, %d] mV? The current limits could not be read, "+
			"so this may be widening them.", r.NewLowMv, r.NewHighMv)
		return d
	}

	widensLow := r.NewLowMv < int(r.CurLowMv)
	widensHigh := r.NewHighMv > int(r.CurHighMv)
	if widensLow || widensHigh {
		d.Confirm = true
		d.Prompt = fmt.Sprintf(
			"Widen the voltage limits from [%d, %d] to [%d, %d] mV? "+
				"This removes the guard rail that blocks an out-of-range `voltage set`.",
			r.CurLowMv, r.CurHighMv, r.NewLowMv, r.NewHighMv)
	}
	return d
}

// VLimitUsable reports whether a vlimit pair can bound anything at all.
//
// This is the ONLY test interlock 1 may apply to the device's window. A window
// is either usable as a bound or it is not; how narrow it is says nothing about
// whether to trust it. A [3300, 5000] pair — exactly what someone protecting a
// 5 V pedal would configure — is perfectly usable, and treating it as suspect
// would discard the strictest and most deliberate guard rail in the system.
//
// Do not confuse it with session.VLimitPlausible, which is a different question
// with a different answer: that one asks whether the pair still has the shape
// the vendor firmware expects after a flash (low >= 3000, high >= 6000), and it
// is the predicate the post-update routine uses to decide whether to rewrite
// the defaults (SPEC.md §6.5, §10.4). Interlock 1 once called it by mistake,
// which discarded every window with a ceiling below 6 V and let 48 V through on
// a unit its owner had limited to 5 V. There is deliberately only one copy of
// it now, in package session, so the two cannot be reached for interchangeably
// here.
func VLimitUsable(lowMv, highMv uint16) bool { return highMv > lowMv }

// CheckAuthLock applies interlock 4 of SPEC.md §13.
//
// The lock is the least understood command in the protocol: the write puts the
// level in the first payload byte while the vendor's reader takes the second,
// only level 0 ("unlocked") is named anywhere, and the read path has zero
// callers so it was never exercised (SPEC.md §6.3, §14.8). Writing a non-zero
// level is therefore an experiment, and is labelled as one.
func CheckAuthLock(level int) Decision {
	var d Decision
	if level < 0 || level > 255 {
		d.Refused = fmt.Sprintf("auth lock level %d is not a single byte (0-255)", level)
		return d
	}
	d.Confirm = true
	if level == int(proto.AuthLockUnlocked) {
		d.Prompt = "Write auth lock level 0 (unlocked)?"
		return d
	}
	d.warn("auth lock level %d has NO DOCUMENTED EFFECT.\n"+
		"  Only level 0 (unlocked) is defined anywhere in the vendor application; what other\n"+
		"  levels gate, and how to get back out of one, is unknown (SPEC.md §6.3, §14.8).\n"+
		"  There may be no way to undo this from this tool.", level)
	d.Prompt = fmt.Sprintf("Write auth lock level %d anyway, accepting that its effect is undocumented "+
		"and may not be reversible?", level)
	return d
}

// CheckCalibrate applies interlock 5 of SPEC.md §13.
//
// A wrong offset or scale makes every subsequent voltage reading silently
// wrong, which defeats interlock 1 by corrupting the evidence it relies on. The
// previous values are echoed back with a ready-made restore command.
func CheckCalibrate(offset, scale, prevOffset, prevScale int32, prevKnown bool) Decision {
	var d Decision
	if prevKnown {
		d.warn("previous calibration: offset=%d scale=%d\n"+
			"  restore it with: gflex calibrate adc --offset %d --scale %d --yes",
			prevOffset, prevScale, prevOffset, prevScale)
	} else {
		d.warn("the previous calibration could not be read, so it cannot be restored automatically.\n"+
			"  The factory default is offset=%d scale=%d (SPEC.md §6.5)",
			proto.DefaultADCOffset, proto.DefaultADCScale)
	}
	d.warn("a wrong calibration makes every voltage reading silently wrong, which defeats the\n" +
		"  range check on `voltage set`. The host has no calibration formula; the device computes\n" +
		"  the calibrated millivolts itself (SPEC.md §6.5).")
	d.Confirm = true
	d.Prompt = fmt.Sprintf("Write ADC calibration offset=%d scale=%d?", offset, scale)
	return d
}

// CheckFlash applies interlock 6 of SPEC.md §13 to the pre-flight state of a
// firmware update. crcKnown is false when the image carries no CRC to verify
// against, in which case force must be set to proceed.
func CheckFlash(pages int, version string, crcKnown, force bool) Decision {
	var d Decision
	if pages == 0 {
		d.Refused = "firmware image contains no pages"
		return d
	}
	if !crcKnown {
		if !force {
			d.Refused = "this firmware image carries no CRC, so a successful flash cannot be verified.\n" +
				"  Re-run with --force to flash it unverified, accepting that a corrupted image\n" +
				"  will not be detected before the device is told to jump to it"
			return d
		}
		d.warn("this image carries no CRC: verification will be SKIPPED and a corrupted flash\n" +
			"  will not be detected before the jump to the application.")
	}
	label := version
	if label == "" {
		label = "(unversioned)"
	}
	d.Confirm = true
	d.Prompt = fmt.Sprintf("Flash firmware %s (%d pages)? The device's settings will be erased and "+
		"restored afterwards.", label, pages)
	return d
}

// CheckRawFrame guards the raw escape hatch.
//
// Reads of documented commands are harmless. A write frame stores something in
// non-volatile memory with no range checking whatsoever, an undocumented
// command code does something nobody has characterised (SPEC.md §14.5), and the
// scratchpad flag is never set by the vendor app so its volatile-versus-
// committed meaning is unknown (SPEC.md §5.1).
func CheckRawFrame(f proto.Frame) Decision {
	var d Decision
	var reasons []string
	if f.Write {
		reasons = append(reasons, "it is a WRITE frame and no range checking is applied to a raw payload")
	}
	if f.Cmd.Undocumented() {
		reasons = append(reasons, fmt.Sprintf("%s has no documented payload format or effect (SPEC.md §14.5)", f.Cmd))
	}
	if !f.Cmd.Known() {
		reasons = append(reasons, fmt.Sprintf("command code %d is outside the known table", uint8(f.Cmd)))
	}
	if f.Scratchpad {
		reasons = append(reasons, "the scratchpad flag is set; the vendor app never sets it and its "+
			"volatile-vs-committed meaning is unknown (SPEC.md §5.1)")
	}
	// Some commands are disruptive with no write flag set, so the checks above
	// miss them entirely. `raw 02 14` is the clearest case: a plain read frame,
	// a documented command code, and it drops the device off the bus into the
	// bootloader where no other command in this tool can reach it.
	switch f.Cmd {
	case proto.CmdJumpAppToBootloader:
		reasons = append(reasons, "this disconnects the device into bootloader mode; "+
			"only `gflex firmware flash` can reach it afterwards, until it is power-cycled")
	case proto.CmdBootloadEnd, proto.CmdBootloaderWriteChunk,
		proto.CmdBootloaderCommitPage, proto.CmdBootloaderVerify:
		reasons = append(reasons, fmt.Sprintf(
			"%s is a bootloader command; in application mode its effect is undefined", f.Cmd))
	case proto.CmdVoltageMv:
		if f.Write {
			// Worth saying plainly: this is the one path to the rail that the
			// interlocks of SPEC.md §13 do not police.
			reasons = append(reasons, "this writes the output voltage directly, bypassing every "+
				"range check in `gflex voltage set` -- the value is not compared against the "+
				"unit's limit window or the hardware envelope")
		}
	}
	if len(reasons) == 0 {
		return d
	}
	for _, r := range reasons {
		d.warn("%s", r)
	}
	d.Confirm = true
	d.Prompt = fmt.Sprintf("Send %s verbatim?", f.Cmd)
	return d
}
