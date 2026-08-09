package cli

import (
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
