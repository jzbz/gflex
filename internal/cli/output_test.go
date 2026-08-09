package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jzbz/gflex/internal/proto"
)

func u16p(v uint16) *uint16 { return &v }
func boolp(v bool) *bool    { return &v }

// The JSON document must be exactly one object, on stdout, with nothing else
// mixed in: `gflex --json info | jq` has to work unconditionally.
func TestJSONOutputIsASingleObject(t *testing.T) {
	var out, errOut bytes.Buffer
	f := newFormatter(true, &out, &errOut)

	f.KV("voltage_mv", "output voltage", uint16(12000), "12000 mV (12 V)")
	f.KV("current_limit_ma", "current limit", uint16(5000), "5000 mA (5 A)")
	f.Note("this prose must not reach stdout")
	f.Diag("neither must this diagnostic")
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a single JSON object: %v\n%s", err, out.String())
	}
	if len(got) != 2 {
		t.Errorf("got %d fields, want 2: %v", len(got), got)
	}
	if got["voltage_mv"] != float64(12000) {
		t.Errorf("voltage_mv = %v, want 12000", got["voltage_mv"])
	}
	if strings.Contains(out.String(), "must not reach stdout") {
		t.Error("Note leaked into stdout")
	}
	if !strings.Contains(errOut.String(), "must not reach stdout") {
		t.Error("Note did not reach stderr")
	}
	if !strings.Contains(errOut.String(), "neither must this diagnostic") {
		t.Error("Diag did not reach stderr")
	}
}

// Values must be emitted in wire units. A JSON consumer reading voltage_mv
// gets millivolts, never volts.
func TestJSONValuesAreWireUnits(t *testing.T) {
	var out, errOut bytes.Buffer
	f := newFormatter(true, &out, &errOut)
	f.KV("voltage_mv", "output voltage", uint16(9500), "9500 mV (9.5 V)")
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	var got struct {
		VoltageMv int `json:"voltage_mv"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.VoltageMv != 9500 {
		t.Errorf("voltage_mv = %d, want 9500 (millivolts, not volts)", got.VoltageMv)
	}
}

// Keys come out in insertion order, not alphabetically: encoding/json sorts map
// keys, which would scatter related fields.
func TestJSONKeyOrderIsInsertionOrder(t *testing.T) {
	var out, errOut bytes.Buffer
	f := newFormatter(true, &out, &errOut)
	f.KV("zeta", "z", 1, "1")
	f.KV("alpha", "a", 2, "2")
	f.KV("mu", "m", 3, "3")
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	s := out.String()
	iz, ia, im := strings.Index(s, "zeta"), strings.Index(s, "alpha"), strings.Index(s, "mu")
	if !(iz < ia && ia < im) {
		t.Errorf("keys are not in insertion order:\n%s", s)
	}
}

// info --json must be the DeviceInfo struct itself, so the field names come
// from the shared model and cannot drift from SPEC.md §8.
func TestInfoJSONUsesDeviceInfoFieldNames(t *testing.T) {
	var out, errOut bytes.Buffer
	f := newFormatter(true, &out, &errOut)

	info := &proto.DeviceInfo{
		SerialNum:      "VF001234",
		FirmwareID:     "5.0.1",
		VoltageMv:      u16p(12000),
		CurrentLimitMa: u16p(5000),
		VLimitLowMv:    u16p(3300),
		VLimitHighMv:   u16p(48000),
		LEDAlwaysOn:    boolp(true),
	}
	emitDeviceInfo(f, info, false)
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a single JSON object: %v\n%s", err, out.String())
	}
	for _, key := range []string{
		"serial_num", "fw_id", "voltage_mv", "current_limit_ma",
		"vlimit_low_mv", "vlimit_high_mv", "led_always_on",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing field %q in %v", key, got)
		}
	}
	// Unread fields must be absent rather than zero: a nil pointer means "not
	// read", and reporting 0 mV would be a lie.
	for _, key := range []string{"uuid", "hw_id", "mfg_date", "vmeasure_raw_adc", "authlock_level"} {
		if _, ok := got[key]; ok {
			t.Errorf("field %q should be omitted when it was not read", key)
		}
	}
	if got["voltage_mv"] != float64(12000) {
		t.Errorf("voltage_mv = %v, want 12000", got["voltage_mv"])
	}
}

// pdo dump --json must be the decoded log at the top level.
func TestPDOJSONShape(t *testing.T) {
	var out, errOut bytes.Buffer
	f := newFormatter(true, &out, &errOut)
	f.Document(pdoDumpJSON{Log: nil, Raw: "01 02"})
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not a single JSON object: %v\n%s", err, out.String())
	}
	if got["raw"] != "01 02" {
		t.Errorf("raw = %v, want %q", got["raw"], "01 02")
	}
}

// The human formatter aligns a run of key/value entries against each other.
func TestHumanOutputIsAligned(t *testing.T) {
	var out, errOut bytes.Buffer
	f := newFormatter(false, &out, &errOut)
	f.KV("serial_num", "serial", "VF001234", "VF001234")
	f.KV("voltage_mv", "output voltage", uint16(12000), "12000 mV (12 V)")
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), out.String())
	}
	if strings.Index(lines[0], "VF001234") != strings.Index(lines[1], "12000 mV") {
		t.Errorf("values are not aligned:\n%s", out.String())
	}
}

// A JSON-only field (a paired value with no label) must not leave a blank line
// in the human block.
func TestHumanOutputSkipsJSONOnlyFields(t *testing.T) {
	var out, errOut bytes.Buffer
	f := newFormatter(false, &out, &errOut)
	f.KV("vlimit_low_mv", "voltage limits", uint16(3300), "3300 - 48000 mV")
	f.KV("vlimit_high_mv", "", uint16(48000), "")
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := strings.Count(strings.TrimRight(out.String(), "\n"), "\n") + 1; got != 1 {
		t.Errorf("got %d lines, want 1:\n%q", got, out.String())
	}
}

func TestHumanTableRendersHeadersAndRows(t *testing.T) {
	var out, errOut bytes.Buffer
	f := newFormatter(false, &out, &errOut)
	f.Table("pdos", "capabilities", nil,
		[]string{"#", "TYPE", "VOLTAGE"},
		[][]string{{"0", "fixed", "5 V"}, {"1", "pps", "3.3 - 11 V"}})
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	s := out.String()
	for _, want := range []string{"capabilities", "TYPE", "fixed", "3.3 - 11 V"} {
		if !strings.Contains(s, want) {
			t.Errorf("table output missing %q:\n%s", want, s)
		}
	}
}

// An empty table says so rather than printing nothing at all.
func TestHumanEmptyTable(t *testing.T) {
	var out, errOut bytes.Buffer
	f := newFormatter(false, &out, &errOut)
	f.Table("midi_ports", "MIDI ports", nil, []string{"PATH"}, nil)
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !strings.Contains(out.String(), "(none)") {
		t.Errorf("an empty table should say so:\n%s", out.String())
	}
}

// Document must win over accumulated KV fields in JSON mode.
func TestDocumentOverridesKVFields(t *testing.T) {
	var out, errOut bytes.Buffer
	f := newFormatter(true, &out, &errOut)
	f.KV("ignored", "ignored", 1, "1")
	f.Document(map[string]string{"only": "this"})
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["ignored"]; ok {
		t.Errorf("KV fields should not survive Document: %v", got)
	}
	if got["only"] != "this" {
		t.Errorf("got %v, want the document", got)
	}
}

// An empty result is still a valid object.
func TestJSONEmptyResultIsAnObject(t *testing.T) {
	var out, errOut bytes.Buffer
	f := newFormatter(true, &out, &errOut)
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "{}" {
		t.Errorf("got %q, want %q", got, "{}")
	}
}

func TestDryRunPrintsFrameAndMIDI(t *testing.T) {
	var out, errOut bytes.Buffer
	app := &App{stdout: &out, stderr: &errOut}
	f := newFormatter(false, &out, &errOut)

	// Set voltage to 12.000 V, the golden vector from SPEC.md §15.
	frame, err := proto.Write(proto.CmdVoltageMv, proto.EncodeU16(12000))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := app.dryRun(f, frame); err != nil {
		t.Fatalf("dryRun: %v", err)
	}
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "04 92 2e e0") {
		t.Errorf("dry run did not print the protocol frame:\n%s", s)
	}
	// Interlock 8 requires the MIDI stream too, because that is what actually
	// goes on the wire.
	if !strings.Contains(s, "80 00 00") || !strings.Contains(s, "a0 00 00") {
		t.Errorf("dry run did not print the MIDI byte stream:\n%s", s)
	}
	if !strings.Contains(s, "CMD_VOLTAGE_MV") {
		t.Errorf("dry run did not name the command:\n%s", s)
	}
}
