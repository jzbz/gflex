package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/transport/rawmidi"
	"github.com/jzbz/gflex/internal/usbfs"
)

// midiPortJSON is the machine-readable form of a discovered rawmidi port. The
// transport structs carry no JSON tags, so the names live here.
type midiPortJSON struct {
	Path      string `json:"path"`
	Card      int    `json:"card"`
	Device    int    `json:"device"`
	Name      string `json:"name,omitempty"`
	VendorID  string `json:"vendor_id,omitempty"`
	ProductID string `json:"product_id,omitempty"`
	IsVFlex   bool   `json:"is_vflex"`
	// IdentifiedBy names the evidence behind IsVFlex: "vendor_id" or "name".
	// The two tiers are not the same claim (see warnNameOnlyMatch in
	// connect.go), and a consumer that reads is_vflex alone cannot tell them
	// apart -- a port whose sysfs walk failed reports true with no vendor_id.
	IdentifiedBy string `json:"identified_by,omitempty"`
}

// usbDeviceJSON is the machine-readable form of a usbfs device reference.
type usbDeviceJSON struct {
	Path      string `json:"path"`
	Bus       int    `json:"bus"`
	Addr      int    `json:"addr"`
	VendorID  string `json:"vendor_id"`
	ProductID string `json:"product_id"`
	SysPath   string `json:"sys_path,omitempty"`
}

func newDevicesCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "List candidate MIDI ports and USB devices",
		Long: "devices lists every ALSA rawmidi port on the system and every USB device carrying\n" +
			"the Tundra Labs vendor ID, so you can tell which one to pass to --port.\n\n" +
			"A port is marked as a VFLEX when its USB parent has vendor 0x37BF, or -- only when\n" +
			"it could not be traced to a USB device at all -- when its own name contains \"vflex\"\n" +
			"(SPEC.md §3.4). The VFLEX column says which of the two it was, and the USB ID column\n" +
			"is empty for the second.\n\n" +
			"The product ID is not matched: application mode reports 0x800F (SPEC.md §14.1,\n" +
			"measured), but the bootloader-mode PID is still unmeasured (SPEC.md §14.16), so the\n" +
			"vendor ID is the only thing that identifies the device in both modes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				return app.listDevices(ctx, f)
			})
		},
	}
	return cmd
}

func (a *App) listDevices(_ context.Context, f Formatter) error {
	ports, perr := rawmidi.Discover()
	if perr != nil {
		f.Diag("warning: could not enumerate MIDI ports: %v", perr)
	}
	items := make([]midiPortJSON, 0, len(ports))
	rows := make([][]string, 0, len(ports))
	for _, p := range ports {
		items = append(items, midiPortJSON{
			Path:         p.Path,
			Card:         p.Card,
			Device:       p.Device,
			Name:         p.Name,
			VendorID:     hexID(p.VendorID),
			ProductID:    hexID(p.ProductID),
			IsVFlex:      p.IsVFlex,
			IdentifiedBy: identifiedBy(p),
		})
		// p.Name is device-supplied and goes into the table unquoted. That is
		// safe at the source rather than here: rawmidi's portName filters every
		// candidate through printableName, so anything outside printable ASCII
		// -- the escapes and newlines that would let a name forge a row -- is
		// gone before this ever sees it. Re-quoting here would only make the
		// common case harder to read.
		rows = append(rows, []string{p.Path, fmt.Sprintf("%d:%d", p.Card, p.Device), p.Name, idPair(p), vflexMark(p)})
	}
	f.Table("midi_ports", "MIDI ports", items,
		[]string{"PATH", "CARD:DEV", "NAME", "USB ID", "VFLEX"}, rows)

	refs, uerr := usbfs.Enumerate(proto.VendorID)
	if uerr != nil {
		f.Diag("warning: could not enumerate USB devices: %v", uerr)
	}
	uitems := make([]usbDeviceJSON, 0, len(refs))
	urows := make([][]string, 0, len(refs))
	for _, r := range refs {
		uitems = append(uitems, usbDeviceJSON{
			Path:      r.Path,
			Bus:       r.Bus,
			Addr:      r.Addr,
			VendorID:  hexID(r.VendorID),
			ProductID: hexID(r.ProductID),
			SysPath:   r.SysPath,
		})
		urows = append(urows, []string{
			r.Path,
			fmt.Sprintf("%03d:%03d", r.Bus, r.Addr),
			fmt.Sprintf("%04x:%04x", r.VendorID, r.ProductID),
		})
	}
	f.Table("usb_devices", fmt.Sprintf("USB devices with vendor 0x%04X", proto.VendorID), uitems,
		[]string{"PATH", "BUS:ADDR", "USB ID"}, urows)

	if len(ports) == 0 && len(refs) == 0 {
		f.Note("")
		f.Note("Nothing found. If the VFLEX is plugged in, this is usually a permissions problem:")
		f.Note("  sudo gflex install-udev      (then unplug and replug the device)")
	}
	return nil
}

// hexID renders a USB ID as a 0x-prefixed string, or "" when unknown.
func hexID(v uint16) string {
	if v == 0 {
		return ""
	}
	return fmt.Sprintf("0x%04x", v)
}

func idPair(p rawmidi.PortInfo) string {
	if p.VendorID == 0 {
		return ""
	}
	return fmt.Sprintf("%04x:%04x", p.VendorID, p.ProductID)
}

// vflexMark renders the VFLEX column, keeping the two tiers of SPEC.md §3.4
// apart.
//
// classify falls back to the "vflex" name substring only when it could not
// trace the port to a USB device at all, and an unqualified "yes" presented
// that as the same fact as a vendor-ID match. It is not: the name is
// firmware-dependent evidence -- one unit advertised "Werewolf VFLEX" (SPEC.md
// §14.2), no document promises a second revision keeps it, and nothing stops an
// unrelated device from spelling it -- which is why the connect path warns when
// it opens such a port (warnNameOnlyMatch). Here the empty USB ID column was
// the only tell, and it reads as "unknown" rather than as "unconfirmed".
func vflexMark(p rawmidi.PortInfo) string {
	switch {
	case !p.IsVFlex:
		return ""
	case p.VendorID == 0:
		return "yes (name only)"
	}
	return "yes"
}

// identifiedBy is vflexMark's answer for a consumer rather than a reader.
func identifiedBy(p rawmidi.PortInfo) string {
	switch {
	case !p.IsVFlex:
		return ""
	case p.VendorID == 0:
		return "name"
	}
	return "vendor_id"
}
