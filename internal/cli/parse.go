package cli

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The wire is millivolts and milliamps, everywhere, in both directions
// (SPEC.md §6.5). Parsing converts once, on the way in; formatting converts
// once, on the way out. Nothing in between ever sees volts or amps.

// ParseVoltage converts a user-supplied voltage into millivolts.
//
// Accepted forms:
//
//	"12"        12 V   -> 12000 mV
//	"12V"       12 V   -> 12000 mV
//	"9.5"       9.5 V  ->  9500 mV
//	"12000mV"          -> 12000 mV
//	"12000 mv"         -> 12000 mV
//
// A bare number is always volts. That is the reading that matches how people
// talk about this device ("set it to 12"), and the alternative — guessing from
// magnitude — would silently turn a typo into a 12-fold over-volt. A bare
// number large enough to be a plausible millivolt value is therefore rejected
// downstream by the range interlock with a hint, not reinterpreted here.
//
// The number itself is a plain decimal and nothing else: no underscores, no
// exponent, no hexadecimal, no "Inf". See numberProblem for why.
//
// The result may be negative or exceed 65535; range enforcement belongs to the
// interlocks in interlock.go so that it stays testable and appears in exactly
// one place.
func ParseVoltage(s string) (int, error) {
	return parseScaled(s, "voltage", "V", "mV")
}

// ParseCurrent converts a user-supplied current into milliamps.
//
// Accepted forms: "3", "3A", "3.0", "3000mA", "3000 ma". A bare number is amps,
// for the same reason as ParseVoltage.
func ParseCurrent(s string) (int, error) {
	return parseScaled(s, "current", "A", "mA")
}

// parseScaled implements the shared "<number><unit>" grammar. baseUnit is the
// large unit (volts, amps) and milliUnit the wire unit; a bare number is read as
// the base unit and multiplied by 1000.
func parseScaled(s, what, baseUnit, milliUnit string) (int, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, codedf(ExitUsage, "empty %s value: expected e.g. 12, 12%s or 12000%s", what, baseUnit, milliUnit)
	}

	num := trimmed
	scale := 1000.0 // bare number and the base unit are both x1000 into wire units
	lower := strings.ToLower(trimmed)
	switch {
	case strings.HasSuffix(lower, strings.ToLower(milliUnit)):
		num = trimmed[:len(trimmed)-len(milliUnit)]
		scale = 1
	case strings.HasSuffix(lower, strings.ToLower(baseUnit)):
		num = trimmed[:len(trimmed)-len(baseUnit)]
	}
	num = strings.TrimSpace(num)
	if num == "" {
		return 0, codedf(ExitUsage, "missing number in %s value %q", what, s)
	}

	if why := numberProblem(num); why != "" {
		return 0, codedf(ExitUsage, "cannot parse %s %q: %s; expected e.g. 12, 12%s or 12000%s",
			what, s, why, baseUnit, milliUnit)
	}

	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		// The syntax has already been vetted, so the only failure ParseFloat
		// has left is magnitude: a decimal with more digits than a float64 can
		// carry an exponent for, which overflows to ±Inf or underflows to 0.
		return 0, codedf(ExitUsage, "%s %q is out of any plausible range", what, s)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		// Unreachable through the grammar above, and kept precisely because
		// this is the one value that would defeat everything downstream: every
		// comparison against NaN is false, so it would pass the int32 bound
		// below and then the range interlock of SPEC.md §13.1 as well, and
		// int(NaN) is implementation-defined. Cheaper than reasoning about it
		// again after the next edit.
		return 0, codedf(ExitUsage, "cannot parse %s %q: not a finite number", what, s)
	}

	// Round rather than truncate: 12.3 V is 12299.999... in binary floating
	// point and must land on 12300 mV, not 12299.
	scaled := math.Round(v * scale)
	if scaled > math.MaxInt32 || scaled < math.MinInt32 {
		return 0, codedf(ExitUsage, "%s %q is out of any plausible range", what, s)
	}
	return int(scaled), nil
}

// numberProblem describes what is wrong with num as a plain decimal number, or
// returns "" if it is one.
//
// The grammar is an optional sign, decimal digits and at most one decimal
// point — exactly what the help text promises and what a user actually types.
// It is deliberately much narrower than strconv.ParseFloat, which also accepts
// Go literal syntax: underscore separators, hexadecimal floats, exponents,
// "Inf" and "NaN". Every one of those turns a fat-fingered command line into a
// plausible-looking rail voltage — "1_2" is 12 V, "0x1p4" is 16 V, "1e1" is
// 10 V — and does it silently, because by the time the value reaches
// CheckVoltage it is an integer millivolt count indistinguishable from one the
// user meant. The interlocks of SPEC.md §13 bound the number; only this can
// question whether it is the number that was typed.
//
// Exponents are rejected along with the rest, which is a judgement call: 1e1 is
// unambiguous to Go. But nobody writes a supply voltage that way, an exponent
// letter sits next to the unit suffixes this grammar also has to carry, and
// every value it could express has a plain-decimal spelling. Weigh a user who
// has to retype 1e1 as 10 against a stray "e" scaling a rail by ten with no
// visible decimal point, on a tool whose one job is not to destroy hardware.
func numberProblem(num string) string {
	rest := num
	if rest[0] == '+' || rest[0] == '-' { // num is non-empty; the caller checked
		rest = rest[1:]
	}
	digits, points := 0, 0
	for _, r := range rest {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '.':
			points++
			if points > 1 {
				return "more than one decimal point"
			}
		case r == '_':
			return "digit separators are not accepted (write 12000, not 12_000)"
		case r == 'e' || r == 'E':
			return "exponent notation is not accepted (write 10, not 1e1)"
		default:
			return fmt.Sprintf("unexpected %q in the number", r)
		}
	}
	if digits == 0 {
		return "no digits"
	}
	return ""
}

// parseDecimalInt parses a user-supplied integer as plain decimal, and nothing
// else. what names the quantity in the message, and bound describes the limit
// the caller cares about for the one failure strconv can diagnose by itself.
//
// It exists because the obvious alternative is strconv.ParseInt(s, 0, …), and
// that is exactly what pflag's IntVar and Int32Var call for a flag: base 0 is
// Go literal syntax, so a leading zero means octal, "0x…" means hex and "_" is
// a digit separator. `authlock set` shipped with that spelling and "010" wrote
// level 8; the flags that write the tolerance and the ADC calibration shipped
// with it too, reached through pflag rather than through a conversion written
// here, so the same defect was invisible in this file. Every one of those
// values lands in non-volatile memory, and the reinterpreted number sits well
// inside every bound the interlocks of SPEC.md §13 check -- 0750 is 488, which
// is a perfectly ordinary millivolt count -- so nothing downstream can catch
// it. numberProblem above carries the full argument for why the accepted
// grammar is this narrow; it is not reused here only because it admits a
// decimal point, which none of these wire fields can carry.
//
// Range policy stays with the callers. bits is conversion safety rather than a
// bound: it makes the caller's conversion to int or int32 exact, so an enormous
// typo fails the parse instead of wrapping into a plausible-looking value.
func parseDecimalInt(arg, what, bound string, bits int) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(arg), 10, bits)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return 0, codedf(ExitUsage, "%s %q is far outside %s", what, arg, bound)
		}
		return 0, codedf(ExitUsage,
			"cannot parse %s %q: not a plain decimal number (write 10, not 0x0a or 1_0)", what, arg)
	}
	return v, nil
}

// parseCRCByte parses --crc, the one numeric flag in the tool where hex is the
// natural spelling: the value is the byte an image ships alongside itself, and
// every artifact that carries one prints it as hex.
//
// So unlike parseDecimalInt this accepts a base prefix -- but only an explicit
// one. A bare leading zero is refused rather than read as octal, which is what
// pflag's base-0 integer flag did here: `--crc 017` meant 15. That matters more
// on this flag than the arithmetic suggests. A CRC is not applied, it is
// compared, so a value that quietly means a different number than the one typed
// does not fail loudly -- it can match what the device answers, for a reason the
// user never intended, and walk an image they meant to reject through to
// CMD_BOOTLOAD_END (SPEC.md §10.4). The device's CRC algorithm is unknown
// (SPEC.md §14.12), so nothing downstream can recompute the value and notice.
func parseCRCByte(arg string) (int, error) {
	s := strings.TrimSpace(arg)
	base := 10
	switch {
	case strings.HasPrefix(s, "0x"), strings.HasPrefix(s, "0X"):
		base, s = 16, s[2:]
	case len(s) > 1 && s[0] == '0':
		bare := strings.TrimLeft(s, "0")
		return 0, codedf(ExitUsage, "cannot parse --crc %q: a leading zero is not octal here; "+
			"write %s for decimal or 0x%s for hex", arg, bare, bare)
	}
	v, err := strconv.ParseUint(s, base, 8)
	if err != nil {
		return 0, codedf(ExitUsage,
			"cannot parse --crc %q: want a byte value 0..255, as decimal or with an explicit 0x prefix", arg)
	}
	return int(v), nil
}

// parseHexBytes parses the arguments of `gflex raw` into a byte slice.
//
// The arguments are joined and then stripped of the separators people naturally
// type, so "02 08", "0208", "0x02 0x08" and "02:08" are all the same frame.
func parseHexBytes(args []string) ([]byte, error) {
	joined := strings.Join(args, "")
	var sb strings.Builder
	sb.Grow(len(joined))
	for i := 0; i < len(joined); i++ {
		c := joined[i]
		switch {
		case c == ' ' || c == ',' || c == ':' || c == '-' || c == '_' || c == '\t':
			continue
		case c == '0' && i+1 < len(joined) && (joined[i+1] == 'x' || joined[i+1] == 'X'):
			i++ // skip the "0x" prefix of a byte literal
			continue
		}
		sb.WriteByte(c)
	}
	h := sb.String()
	if h == "" {
		return nil, codedf(ExitUsage, "no hex bytes given")
	}
	if len(h)%2 != 0 {
		return nil, codedf(ExitUsage, "hex input has an odd number of digits (%d): every byte needs two", len(h))
	}
	out := make([]byte, len(h)/2)
	for i := range out {
		v, err := strconv.ParseUint(h[i*2:i*2+2], 16, 8)
		if err != nil {
			return nil, codedf(ExitUsage, "invalid hex byte %q at offset %d", h[i*2:i*2+2], i)
		}
		out[i] = byte(v)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Display formatting. Presentation only: no caller may feed these back into a
// wire value.
// ---------------------------------------------------------------------------

// formatMv renders millivolts for humans, keeping the wire value visible.
func formatMv(mv uint16) string {
	return fmt.Sprintf("%d mV (%s V)", mv, trimFloat(float64(mv)/1000, 3))
}

// formatMa renders milliamps for humans, keeping the wire value visible.
func formatMa(ma uint16) string {
	return fmt.Sprintf("%d mA (%s A)", ma, trimFloat(float64(ma)/1000, 3))
}

// formatMvInt renders a millivolt quantity that has not yet been range-checked
// and so may not fit in a uint16.
func formatMvInt(mv int) string {
	return fmt.Sprintf("%d mV (%s V)", mv, trimFloat(float64(mv)/1000, 3))
}

// formatMaInt renders a milliamp quantity that has not yet been range-checked.
func formatMaInt(ma int) string {
	return fmt.Sprintf("%d mA (%s A)", ma, trimFloat(float64(ma)/1000, 3))
}

// trimFloat formats v with at most prec decimals and no trailing zeros.
func trimFloat(v float64, prec int) string {
	s := strconv.FormatFloat(v, 'f', prec, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s
}
