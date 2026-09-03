package cli

import (
	"strings"
	"testing"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/transport/rawmidi"
)

// TestVFlexMarkKeepsTheIdentificationTiersApart pins what the VFLEX column
// claims.
//
// classify anchors on the USB vendor ID wherever there is one and falls back to
// the "vflex" name substring only when the port could not be traced to a USB
// device at all (SPEC.md §3.4). Both tiers used to render as an unqualified
// "yes", so a port identified by a string its own firmware chose was presented
// exactly as a unit the vendor ID had confirmed -- and the JSON consumer got
// is_vflex: true with no vendor_id to notice the difference by.
func TestVFlexMarkKeepsTheIdentificationTiersApart(t *testing.T) {
	cases := []struct {
		name string
		port rawmidi.PortInfo
		mark string
		by   string
	}{
		{
			name: "the vendor ID confirmed it",
			port: rawmidi.PortInfo{IsVFlex: true, VendorID: proto.VendorID, ProductID: 0x800F},
			mark: "yes",
			by:   "vendor_id",
		},
		{
			name: "the name is all there was",
			port: rawmidi.PortInfo{IsVFlex: true, Name: "vflex loop"},
			mark: "yes (name only)",
			by:   "name",
		},
		{
			name: "not a VFLEX",
			port: rawmidi.PortInfo{Name: "Prophet Rev2"},
			mark: "",
			by:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vflexMark(tc.port); got != tc.mark {
				t.Errorf("vflexMark = %q, want %q", got, tc.mark)
			}
			if got := identifiedBy(tc.port); got != tc.by {
				t.Errorf("identifiedBy = %q, want %q", got, tc.by)
			}
		})
	}
}

// The help is the one place a user is told what the mark means, and it stated a
// single vendor-ID rule that the code does not follow.
func TestDevicesHelpDescribesBothIdentificationTiers(t *testing.T) {
	long := newDevicesCommand(&App{}).Long
	for _, want := range []string{"0x37BF", "\"vflex\"", "§3.4"} {
		if !strings.Contains(long, want) {
			t.Errorf("the devices help does not mention %q:\n%s", want, long)
		}
	}
}
