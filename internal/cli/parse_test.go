package cli

import (
	"strings"
	"testing"
)

func TestParseVoltage(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		// A bare number is volts: "set it to 12" is how people talk about
		// this device.
		{in: "12", want: 12000},
		{in: "12V", want: 12000},
		{in: "12v", want: 12000},
		{in: "12 V", want: 12000},
		{in: " 12V ", want: 12000},
		{in: "9.5", want: 9500},
		{in: "9.5V", want: 9500},
		{in: "3.3", want: 3300},
		// 12.3 V is 12299.999... in binary floating point; rounding, not
		// truncation, is what puts it on 12300.
		{in: "12.3", want: 12300},
		{in: "0.05", want: 50},
		// Explicit millivolts bypass the x1000.
		{in: "12000mV", want: 12000},
		{in: "12000mv", want: 12000},
		{in: "12000 mV", want: 12000},
		{in: "5000MV", want: 5000},
		{in: "0", want: 0},
		// A bare number that looks like millivolts is still volts; the range
		// interlock is what rejects it, with a hint.
		{in: "12000", want: 12000000},
		{in: "48", want: 48000},
		// A sign is part of the grammar. A negative voltage is nonsense to the
		// device, but saying so is the range interlock's job, not the parser's,
		// and it says so far better than "cannot parse" would.
		{in: "+12", want: 12000},
		{in: "-12", want: -12000},
		// Digits on either side of the point alone are unambiguous.
		{in: ".5", want: 500},
		{in: "12.", want: 12000},
		// Bad input.
		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: "V", wantErr: true},
		{in: "mV", wantErr: true},
		{in: "twelve", wantErr: true},
		{in: "12x", wantErr: true},
		{in: "12 volts", wantErr: true},
		{in: "1,2", wantErr: true},
		{in: "NaN", wantErr: true},
		{in: "Inf", wantErr: true},
		{in: "1e400", wantErr: true},
		{in: ".", wantErr: true},
		{in: "+", wantErr: true},
		{in: "1.2.3", wantErr: true},
		{in: "1 2", wantErr: true},
		// Enormous but syntactically valid: the digits overflow a float64, so
		// ParseFloat still gets to reject something.
		{in: "1" + strings.Repeat("0", 400), wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseVoltage(tt.in)
		switch {
		case tt.wantErr && err == nil:
			t.Errorf("ParseVoltage(%q) = %d, want an error", tt.in, got)
		case !tt.wantErr && err != nil:
			t.Errorf("ParseVoltage(%q) failed: %v", tt.in, err)
		case !tt.wantErr && got != tt.want:
			t.Errorf("ParseVoltage(%q) = %d mV, want %d mV", tt.in, got, tt.want)
		}
	}
}

func TestParseVoltageErrorsAreUsageErrors(t *testing.T) {
	_, err := ParseVoltage("banana")
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := ExitCode(err); got != ExitUsage {
		t.Errorf("ExitCode = %d, want ExitUsage (%d)", got, ExitUsage)
	}
}

// TestParseRejectsGoLiteralSyntax is the regression test for handing the whole
// command line to strconv.ParseFloat.
//
// ParseFloat implements Go's float literal grammar, not the one the help text
// promises, so every input below used to be accepted and to produce the
// millivolt value named beside it. None of them is a plausible thing to have
// meant, and all of them are one keystroke from something that is -- which is
// exactly why they are dangerous: the interlocks of SPEC.md §13 see an integer
// millivolt count with no way to tell that it is not the number the user typed.
// A silent 12 V where 1.2 V was meant is a destroyed pedal.
func TestParseRejectsGoLiteralSyntax(t *testing.T) {
	tests := []struct {
		in        string
		wouldHave int    // what ParseFloat used to make of it
		reason    string // the error must name what was wrong
	}{
		{in: "1_2", wouldHave: 12000, reason: "digit separators"},
		{in: "12_000mV", wouldHave: 12000, reason: "digit separators"},
		{in: "1e1", wouldHave: 10000, reason: "exponent notation"},
		{in: "1E1V", wouldHave: 10000, reason: "exponent notation"},
		{in: "0x1p4", wouldHave: 16000, reason: "unexpected 'x'"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseVoltage(tt.in)
			if err == nil {
				t.Fatalf("ParseVoltage(%q) = %d mV, want an error (this is the old %d mV behaviour)",
					tt.in, got, tt.wouldHave)
			}
			if !strings.Contains(err.Error(), tt.reason) {
				t.Errorf("ParseVoltage(%q) error = %q, want it to mention %q", tt.in, err, tt.reason)
			}
			// The error has to say what to type instead, not merely refuse.
			if !strings.Contains(err.Error(), "12000mV") {
				t.Errorf("ParseVoltage(%q) error = %q, want an example of the accepted form", tt.in, err)
			}
			if code := ExitCode(err); code != ExitUsage {
				t.Errorf("ParseVoltage(%q) exit code = %d, want ExitUsage (%d)", tt.in, code, ExitUsage)
			}
		})
	}
	// The same syntax on a current, since both share parseScaled.
	if got, err := ParseCurrent("3_0"); err == nil {
		t.Errorf("ParseCurrent(%q) = %d mA, want an error", "3_0", got)
	}
}

func TestParseCurrent(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "3", want: 3000},
		{in: "3A", want: 3000},
		{in: "3a", want: 3000},
		{in: "3.0", want: 3000},
		{in: "3000mA", want: 3000},
		{in: "3000ma", want: 3000},
		{in: "3000 mA", want: 3000},
		{in: "5", want: 5000},
		{in: "0.5", want: 500},
		{in: "1.5A", want: 1500},
		{in: "", wantErr: true},
		{in: "A", wantErr: true},
		{in: "three", wantErr: true},
		{in: "3amps", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseCurrent(tt.in)
		switch {
		case tt.wantErr && err == nil:
			t.Errorf("ParseCurrent(%q) = %d, want an error", tt.in, got)
		case !tt.wantErr && err != nil:
			t.Errorf("ParseCurrent(%q) failed: %v", tt.in, err)
		case !tt.wantErr && got != tt.want:
			t.Errorf("ParseCurrent(%q) = %d mA, want %d mA", tt.in, got, tt.want)
		}
	}
}

func TestParseHexBytes(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    []byte
		wantErr bool
	}{
		{name: "spaced", in: []string{"02", "08"}, want: []byte{0x02, 0x08}},
		{name: "joined", in: []string{"0208"}, want: []byte{0x02, 0x08}},
		{name: "prefixed", in: []string{"0x02", "0x08"}, want: []byte{0x02, 0x08}},
		{name: "colons", in: []string{"02:08"}, want: []byte{0x02, 0x08}},
		{name: "set voltage", in: []string{"04", "92", "2E", "E0"}, want: []byte{0x04, 0x92, 0x2E, 0xE0}},
		{name: "lowercase", in: []string{"04", "92", "2e", "e0"}, want: []byte{0x04, 0x92, 0x2E, 0xE0}},
		{name: "odd digits", in: []string{"020"}, wantErr: true},
		{name: "not hex", in: []string{"zz"}, wantErr: true},
		{name: "empty", in: []string{""}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHexBytes(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseHexBytes(%v) = % x, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHexBytes(%v) failed: %v", tt.in, err)
			}
			if string(got) != string(tt.want) {
				t.Errorf("parseHexBytes(%v) = % x, want % x", tt.in, got, tt.want)
			}
		})
	}
}

// TestParseDecimalInt covers the grammar every device-write integer flag goes
// through. The case that matters is "0750": base 0 -- what strconv and pflag
// both default to -- reads it as octal 488, and 488 mV is an unremarkable
// value that no interlock can question.
func TestParseDecimalInt(t *testing.T) {
	tests := []struct {
		in   string
		bits int
		want int64
		ok   bool
	}{
		{in: "0", bits: 32, want: 0, ok: true},
		{in: "750", bits: 32, want: 750, ok: true},
		{in: " 750 ", bits: 32, want: 750, ok: true},
		// The finding's case: decimal 750, and never octal 488.
		{in: "0750", bits: 32, want: 750, ok: true},
		{in: "010", bits: 32, want: 10, ok: true},
		// A sign parses; the callers own the bound, so a negative offset is
		// legitimate here and an out-of-range tolerance is refused downstream.
		{in: "-2147483648", bits: 32, want: -2147483648, ok: true},
		{in: "+750", bits: 32, want: 750, ok: true},
		// Go literal shapes are refused outright.
		{in: "0x10", bits: 32, ok: false},
		{in: "1_0", bits: 32, ok: false},
		{in: "1e1", bits: 32, ok: false},
		{in: "7.5", bits: 32, ok: false},
		{in: "", bits: 32, ok: false},
		{in: "seven", bits: 32, ok: false},
		// Wider than the caller's conversion fails the parse rather than
		// wrapping into a plausible-looking value.
		{in: "2147483648", bits: 32, ok: false},
	}
	for _, tt := range tests {
		got, err := parseDecimalInt(tt.in, "--nominal", "0..65535", tt.bits)
		if tt.ok {
			if err != nil {
				t.Errorf("parseDecimalInt(%q) failed: %v", tt.in, err)
				continue
			}
			if got != tt.want {
				t.Errorf("parseDecimalInt(%q) = %d, want %d", tt.in, got, tt.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("parseDecimalInt(%q) = %d, want a refusal", tt.in, got)
			continue
		}
		if code := ExitCode(err); code != ExitUsage {
			t.Errorf("parseDecimalInt(%q): ExitCode = %d, want ExitUsage (%d)", tt.in, code, ExitUsage)
		}
	}
}

func TestFormatters(t *testing.T) {
	if got, want := formatMv(12000), "12000 mV (12 V)"; got != want {
		t.Errorf("formatMv(12000) = %q, want %q", got, want)
	}
	if got, want := formatMv(9500), "9500 mV (9.5 V)"; got != want {
		t.Errorf("formatMv(9500) = %q, want %q", got, want)
	}
	if got, want := formatMa(5000), "5000 mA (5 A)"; got != want {
		t.Errorf("formatMa(5000) = %q, want %q", got, want)
	}
	if got, want := trimFloat(3.3, 3), "3.3"; got != want {
		t.Errorf("trimFloat(3.3) = %q, want %q", got, want)
	}
	if got, want := trimFloat(48, 3), "48"; got != want {
		t.Errorf("trimFloat(48) = %q, want %q", got, want)
	}
}
