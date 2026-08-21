package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jzbz/gflex/internal/pdo"
	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/transport/fake"
)

// TestRequirePDOFirmwareRefusesOldFirmware is the regression test for the gate
// that `pdo dump` and `pdo clear` did not have.
//
// SPEC.md §9 hard-gates the capture log on firmware 5.0.0, and only `scan`
// checked. On a 4.x unit the other two reached the device and then failed the
// way an unsupported command always fails in a protocol with no NACK: a bare
// five-second timeout (SPEC.md §5.2), which reads as a broken cable rather than
// as an old firmware.
func TestRequirePDOFirmwareRefusesOldFirmware(t *testing.T) {
	dev := fake.NewTypical()
	defer dev.Close()
	dev.SetResponse(proto.CmdFirmwareVersion, []byte("4.2.0"))
	s := newFakeSession(t, dev)

	version, err := requirePDOFirmware(context.Background(), s)
	if err == nil {
		t.Fatal("firmware 4.2.0 must be refused")
	}
	if got := ExitCode(err); got != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d)", got, ExitRefused)
	}
	// The vendor's own wording, verbatim: users search for it (SPEC.md §9.6).
	if !strings.Contains(err.Error(), msgFirmwareTooOld) {
		t.Errorf("refusal %q does not carry the vendor's message", err)
	}
	// And it must say what this unit reported, or the message is not actionable.
	if !strings.Contains(err.Error(), "4.2.0") {
		t.Errorf("refusal %q does not name the version found", err)
	}
	if version != "4.2.0" {
		t.Errorf("version = %q, want %q", version, "4.2.0")
	}
}

func TestRequirePDOFirmwareAcceptsSupportedFirmware(t *testing.T) {
	dev := fake.NewTypical()
	defer dev.Close()
	s := newFakeSession(t, dev)

	version, err := requirePDOFirmware(context.Background(), s)
	if err != nil {
		t.Fatalf("firmware %s should pass the gate: %v", fake.TypicalFirmware, err)
	}
	if version != fake.TypicalFirmware {
		t.Errorf("version = %q, want %q", version, fake.TypicalFirmware)
	}
}

// The gate is a frame like any other, so interlock 8 of SPEC.md §13 requires
// --dry-run to show it. Its presence at the head of both listings is also the
// visible trace that the two subcommands go through the gate at all.
func TestPDODryRunLeadsWithTheFirmwareGate(t *testing.T) {
	dump := cmdNames(pdoDumpFrames())
	if len(dump) != 1+pdoChunks {
		t.Fatalf("`pdo dump --dry-run` lists %d frames, want 1 gate + %d chunk reads: %v",
			len(dump), pdoChunks, dump)
	}
	if dump[0] != proto.CmdFirmwareVersion.String() {
		t.Errorf("`pdo dump --dry-run` starts with %s, want %s", dump[0], proto.CmdFirmwareVersion)
	}

	clearFrames, err := pdoClearFrames()
	if err != nil {
		t.Fatalf("pdoClearFrames: %v", err)
	}
	clear := cmdNames(clearFrames)
	if len(clear) != 2 {
		t.Fatalf("`pdo clear --dry-run` lists %d frames, want the gate and the erase: %v", len(clear), clear)
	}
	if clear[0] != proto.CmdFirmwareVersion.String() {
		t.Errorf("`pdo clear --dry-run` starts with %s, want %s", clear[0], proto.CmdFirmwareVersion)
	}
	erase, perr := proto.Parse(clearFrames[1])
	if perr != nil {
		t.Fatalf("parsing the erase frame: %v", perr)
	}
	if erase.Cmd != proto.CmdPDOLog || !erase.Write || len(erase.Payload) != 0 {
		t.Errorf("erase frame = %s, want the 02 91 write with an empty payload", proto.Hex(clearFrames[1]))
	}
}

// The chunk reads are the download itself: twelve sequential indices, 03 11 kk
// (SPEC.md §9.1). Everything above appends to this list, so a mistake here
// would show up in all three commands at once.
func TestPDOReadFramesAreTwelveSequentialChunks(t *testing.T) {
	frames := pdoReadFrames()
	if len(frames) != pdoChunks {
		t.Fatalf("got %d chunk frames, want %d", len(frames), pdoChunks)
	}
	for i, fr := range frames {
		parsed, err := proto.Parse(fr)
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		if parsed.Cmd != proto.CmdPDOLog || parsed.Write {
			t.Errorf("chunk %d = %s, want a CMD_PDO_LOG read", i, proto.Hex(fr))
		}
		if len(parsed.Payload) != 1 || int(parsed.Payload[0]) != i {
			t.Errorf("chunk %d carries index %v, want [%d]", i, parsed.Payload, i)
		}
	}
}

// TestPDOCurrentRendering pins what the capability table shows.
//
// Three properties, each of which was wrong or missing before: a Battery PDO
// renders its power budget rather than a blank cell (the vendor app drops the
// class; decoding it is deliberate), a cable-bound current names what the
// source declared so an apparently under-powered supply is explained, and an
// SPR AVS row shows both band limits because the applicable one depends on the
// output voltage.
func TestPDOCurrentRendering(t *testing.T) {
	tests := []struct {
		name string
		p    pdo.PDO
		want []string // substrings that must appear
		not  []string // substrings that must not
	}{
		{
			name: "battery shows watts, not a blank cell",
			p:    pdo.PDO{Kind: pdo.KindBattery, MaxPowerW: 27.5, Valid: true},
			want: []string{"27.5 W"},
		},
		{
			name: "fixed within the cable rating carries no note",
			// DeclaredMaxCurrentA stays zero unless the clamp actually bit;
			// CableBound() keys off exactly that.
			p:    pdo.PDO{Kind: pdo.KindFixed, MaxCurrentA: 3, Valid: true},
			want: []string{"3 A"},
			not:  []string{"declares"},
		},
		{
			name: "cable-bound fixed names the declared figure",
			p: pdo.PDO{
				Kind: pdo.KindFixed, MaxCurrentA: pdo.MaxCableCurrentA,
				DeclaredMaxCurrentA: 10.23, Valid: true,
			},
			want: []string{"5 A", "declares 10.23 A"},
		},
		{
			name: "spr avs shows both bands",
			p: pdo.PDO{
				Kind: pdo.KindSPRAVS, MaxCurrent15VA: 5, MaxCurrent20VA: 3.25,
				MaxCurrentA: 5, Valid: true,
			},
			want: []string{"5 A @15V", "3.25 A @20V"},
		},
		{
			name: "epr avs shows its power budget",
			p:    pdo.PDO{Kind: pdo.KindEPRAVS, PDPWatts: 140, Valid: true},
			want: []string{"140 W"},
		},
		{
			name: "unknown class is marked, not blank",
			p:    pdo.PDO{Kind: pdo.KindUnknown},
			want: []string{"-"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pdoCurrent(tc.p)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("pdoCurrent = %q, want it to contain %q", got, w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("pdoCurrent = %q, want it NOT to contain %q", got, n)
				}
			}
		})
	}
}

// TestPDORangeSPRAVSIsTheAssumedRange is a regression test: this rendered a
// hardcoded "15 V / 20 V", which are the boundaries of the two CURRENT bands,
// printed where a voltage range belongs. It understated the assumed floor (9 V,
// not 15 V) and presented a USB-PD assumption as scanned data. An SPR AVS APDO
// carries no voltage range on the wire at all (SPEC.md §9.4).
func TestPDORangeSPRAVSIsTheAssumedRange(t *testing.T) {
	got := pdoRange(pdo.PDO{Kind: pdo.KindSPRAVS, MaxCurrent15VA: 5, MaxCurrent20VA: 3.25, Valid: true})
	for _, want := range []string{"9", "20", "?"} {
		if !strings.Contains(got, want) {
			t.Errorf("pdoRange(spr_avs) = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "15") {
		t.Errorf("pdoRange(spr_avs) = %q; 15 V is a current-band boundary, not the range floor", got)
	}
	// It must agree with the constants the evaluator uses for the same decision.
	if !strings.Contains(got, trimFloat(pdo.SPRAVSMinVoltageV, 1)) {
		t.Errorf("pdoRange(spr_avs) = %q, disagrees with pdo.SPRAVSMinVoltageV", got)
	}
}

// renderPDOLog runs a log through emitPDOLog in human mode and returns stdout.
// setDoc is false because the JSON document is not what these tests are about;
// what reaches a human is.
func renderPDOLog(t *testing.T, log *pdo.Log) string {
	t.Helper()
	var out, errBuf bytes.Buffer
	f := newFormatter(false, &out, &errBuf)
	emitPDOLog(f, log, nil, false, false)
	if err := f.Flush(); err != nil {
		t.Fatalf("flushing the formatter: %v", err)
	}
	return out.String()
}

// TestPDOTableDisclosesTheAssumedSPRAVSRange holds the capability table to the
// disclosure rule internal/pdo states for this one figure.
//
// An SPR AVS APDO carries no voltage range on the wire at all -- only its two
// band current limits (SPEC.md §9.4) -- so the 9-20 V this tool prints is
// USB-PD 3.2 speaking, not the scan. Every other cell in that column is scanned
// data, which is exactly what makes a bare "?" insufficient: a reader has no
// way to tell that one figure apart from the measurements around it. The mark
// therefore has to be spelled out, in the pdo package's own words so the two
// statements of it cannot drift.
//
// This test used to live against a renderer with no production caller, and the
// shipped table -- the one `pdo dump` and `scan` reach -- printed the bare mark.
// Delete the f.Note block in emitPDOLog and this fails.
func TestPDOTableDisclosesTheAssumedSPRAVSRange(t *testing.T) {
	log := &pdo.Log{
		NPDOsReceived: 2,
		PDOs: []pdo.PDO{
			{Index: 0, Kind: pdo.KindFixed, VoltageV: 5, MaxCurrentA: 3, Valid: true},
			{Index: 1, Kind: pdo.KindSPRAVS, MaxCurrent15VA: 5, MaxCurrent20VA: 3.25,
				MaxCurrentA: 5, Valid: true},
		},
	}
	got := renderPDOLog(t, log)

	if !strings.Contains(got, pdo.SPRAVSAssumptionClause) {
		t.Errorf("the table marks the SPR AVS range \"?\" but never explains it; "+
			"want the package's own clause %q in:\n%s", pdo.SPRAVSAssumptionClause, got)
	}
	// And the disclosure must name the range the mark sits on, or it explains
	// the wrong number.
	for _, want := range []string{trimFloat(pdo.SPRAVSMinVoltageV, 1), trimFloat(pdo.SPRAVSMaxVoltageV, 1), "assumed"} {
		if !strings.Contains(got, want) {
			t.Errorf("the assumed-range note does not mention %q:\n%s", want, got)
		}
	}
}

// The negative half: a log with no SPR AVS object has nothing assumed in it, so
// the note must not appear. A disclosure printed unconditionally is noise, and
// noise is how a real one stops being read.
func TestPDOTableOmitsTheAssumptionWhenNothingIsAssumed(t *testing.T) {
	log := &pdo.Log{
		NPDOsReceived: 2,
		PDOs: []pdo.PDO{
			{Index: 0, Kind: pdo.KindFixed, VoltageV: 5, MaxCurrentA: 3, Valid: true},
			{Index: 1, Kind: pdo.KindPPS, MinVoltageV: 3.3, MaxVoltageV: 11, MaxCurrentA: 3, Valid: true},
		},
	}
	if got := renderPDOLog(t, log); strings.Contains(got, pdo.SPRAVSAssumptionClause) {
		t.Errorf("a log with no SPR AVS object still carries the assumption note:\n%s", got)
	}
}

// TestPDOTableDisclosesACableBoundCurrent is the table-level half of the other
// safety property this package's renderer owes the user.
//
// Every current in the table has already been bounded by pdo.MaxCableCurrentA,
// because over-reporting available current is the one direction that destroys
// hardware. Clamping silently would be its own hazard, though: a source that
// advertises 10.23 A would appear in the table as a 5 A supply with nothing to
// say the figure had been reduced, and a user comparing the table against the
// charger's own label would conclude the scan had misread it. Delete
// pdoDeclaredNote's call site and this fails.
func TestPDOTableDisclosesACableBoundCurrent(t *testing.T) {
	log := &pdo.Log{
		NPDOsReceived: 1,
		PDOs: []pdo.PDO{{
			Index: 0, Kind: pdo.KindFixed, VoltageV: 20,
			MaxCurrentA: pdo.MaxCableCurrentA, DeclaredMaxCurrentA: 10.23, Valid: true,
		}},
	}
	got := renderPDOLog(t, log)

	if !strings.Contains(got, trimFloat(pdo.MaxCableCurrentA, 2)+" A") {
		t.Errorf("the table does not report the bounded current at all:\n%s", got)
	}
	for _, want := range []string{"declares", "10.23"} {
		if !strings.Contains(got, want) {
			t.Errorf("a current the cable ceiling reduced was clamped silently; want %q in:\n%s", want, got)
		}
	}
}

// And the mirror: a current the ceiling never touched must not claim it was
// reduced. DeclaredMaxCurrentA is set only when the clamp actually bit, so this
// pins that the note keys off that and not off the class.
func TestPDOTableDoesNotInventACableBound(t *testing.T) {
	log := &pdo.Log{
		NPDOsReceived: 1,
		PDOs:          []pdo.PDO{{Index: 0, Kind: pdo.KindFixed, VoltageV: 9, MaxCurrentA: 3, Valid: true}},
	}
	if got := renderPDOLog(t, log); strings.Contains(got, "declares") {
		t.Errorf("a current within the cable rating was reported as bounded:\n%s", got)
	}
}
