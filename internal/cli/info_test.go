package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/session"
	"github.com/jzbz/gflex/internal/transport/fake"
)

// newFakeSession builds a session over a healthy in-memory VFLEX, paced as fast
// as the framer allows so a test pays no per-message delay at all (SPEC.md
// §3.1) -- neither the 1 ms default nor the vendor's 20 ms.
func newFakeSession(t *testing.T, dev *fake.Device) *session.Session {
	t.Helper()
	s := session.New(dev.Transport(), session.Options{
		ByteDelay: time.Nanosecond,
		Timeout:   2 * time.Second,
	})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// cmdNames renders a frame list as command names, so a failure says which
// command is in the wrong place rather than printing two columns of hex.
func cmdNames(frames [][]byte) []string {
	out := make([]string, 0, len(frames))
	for _, fr := range frames {
		if parsed, err := proto.Parse(fr); err == nil {
			out = append(out, parsed.Cmd.String())
			continue
		}
		out = append(out, proto.Hex(fr))
	}
	return out
}

// TestInfoDryRunMatchesWhatInfoSends is the regression test for --dry-run drift.
//
// Interlock 8 of SPEC.md §13 promises that --dry-run prints the exact frames a
// command would send. The listing had drifted from session.Info in two ways: a
// plain `info` advertised CMD_VTOLERANCE_NOMINAL_MV, which session.Info reads
// only under --all, and the --all commands were listed in a different order
// from the order they go out in. A safety promise that is only mostly true is
// worse than one documented as approximate -- someone auditing what a command
// will do reads this list and stops.
//
// The two lists cannot be merged, because the real one is the control flow of
// session.Info and there is nothing to export. So this test derives it: it
// drives a fake device through session.Info and compares the frames the device
// actually received against infoReadCmds. Changing session.Info without
// changing the listing now fails here.
func TestInfoDryRunMatchesWhatInfoSends(t *testing.T) {
	for _, all := range []bool{false, true} {
		name := "info"
		if all {
			name = "info --all"
		}
		t.Run(name, func(t *testing.T) {
			dev := fake.NewTypical()
			defer dev.Close()
			s := newFakeSession(t, dev)

			if _, err := s.Info(context.Background(), all); err != nil {
				t.Fatalf("session.Info(all=%v): %v", all, err)
			}

			got, want := dev.Sent(), infoReadFrames(all)
			if len(got) != len(want) {
				t.Fatalf("session.Info sent %d frames, --dry-run lists %d\n  sent:   %v\n  listed: %v",
					len(got), len(want), cmdNames(got), cmdNames(want))
			}
			for i := range want {
				if !bytes.Equal(got[i], want[i]) {
					t.Errorf("frame %d: session.Info sent %s (%s), --dry-run lists %s (%s)",
						i, proto.Hex(got[i]), cmdNames(got[i : i+1])[0],
						proto.Hex(want[i]), cmdNames(want[i : i+1])[0])
				}
			}
		})
	}
}

// The specific claim that was false: a plain `info` never reads the tolerance
// terms, so it must not advertise them.
func TestPlainInfoDoesNotAdvertiseAllOnlyCommands(t *testing.T) {
	allOnly := map[proto.Cmd]bool{
		proto.CmdVToleranceNominalMv: true,
		proto.CmdVToleranceSagPerMa:  true,
		proto.CmdChipUUID:            true,
		proto.CmdHardwareID:          true,
		proto.CmdMfgDate:             true,
		proto.CmdAuthLock:            true,
		proto.CmdVMeasureADCOffset:   true,
		proto.CmdVMeasureADCScale:    true,
		proto.CmdVMeasure:            true,
	}
	for _, c := range infoReadCmds(false) {
		if allOnly[c] {
			t.Errorf("plain `info --dry-run` lists %s, which session.Info issues only under --all", c)
		}
	}
	// And --all must list every one of them, or the listing understates the
	// traffic instead.
	listed := make(map[proto.Cmd]bool)
	for _, c := range infoReadCmds(true) {
		listed[c] = true
	}
	for c := range allOnly {
		if !listed[c] {
			t.Errorf("`info --all --dry-run` omits %s", c)
		}
	}
}

// TestInfoAllFootnoteNamesTheVendorReadFields is the regression test for a
// footnote that pointed at a position in the output instead of naming a set.
//
// "Fields above the LED setting are read by the vendor app too" was false for
// three of the fields it covered: chip uuid, hardware id and mfg date are
// commands 9, 10 and 12, which the vendor app never issues (SPEC.md §6.4), and
// they print with the other identity strings at the top of the block. So the
// one line telling a user which readings the vendor app corroborates told them
// the opposite for the three fields most likely to be missing.
//
// The list is derived from infoReadCmds(false) -- the commands a plain `info`
// issues, which is exactly the vendor-read set -- so adding a command to that
// list without saying so in the note fails here.
func TestInfoAllFootnoteNamesTheVendorReadFields(t *testing.T) {
	// The wording each vendor-read command appears under in the note.
	labels := map[proto.Cmd]string{
		proto.CmdSerialNumber:       "serial",
		proto.CmdFirmwareVersion:    "firmware",
		proto.CmdVoltageMv:          "output voltage",
		proto.CmdCurrentLimitMa:     "current limit",
		proto.CmdUserVLimit:         "voltage limits",
		proto.CmdDisableLEDDuringOp: "LED setting",
	}

	// An empty DeviceInfo prints no rows at all, so what comes back is the
	// footnote and nothing else -- a label that happens to name a printed row
	// cannot satisfy the assertions below.
	var out, errBuf bytes.Buffer
	f := newFormatter(false, &out, &errBuf)
	emitDeviceInfo(f, &proto.DeviceInfo{}, true)
	if err := f.Flush(); err != nil {
		t.Fatalf("flushing the formatter: %v", err)
	}
	note := out.String()

	for _, c := range infoReadCmds(false) {
		label, ok := labels[c]
		if !ok {
			t.Errorf("a plain `info` reads %s, which the footnote has no wording for", c)
			continue
		}
		if !strings.Contains(note, label) {
			t.Errorf("the footnote does not name %s (%q) as read by the vendor app:\n%s", c, label, note)
		}
	}
	// And the three the vendor app never issues must not be claimed for it.
	for _, absent := range []string{"chip uuid", "hardware id", "mfg date"} {
		if strings.Contains(note, absent) {
			t.Errorf("the footnote claims the vendor app reads %q:\n%s", absent, note)
		}
	}
}
