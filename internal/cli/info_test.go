package cli

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/session"
	"github.com/jzbz/gflex/internal/transport/fake"
)

// newFakeSession builds a session over a healthy in-memory VFLEX, paced as fast
// as the framer allows so a test does not pay the vendor's 20 ms per MIDI
// message (SPEC.md §3.1).
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
