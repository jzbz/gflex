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
			"A port is marked as a VFLEX when its USB parent has vendor 0x37BF. The product ID\n" +
			"is not matched: application mode reports 0x800F (SPEC.md §14.1, measured), but the\n" +
			"bootloader-mode PID is still unmeasured (SPEC.md §14.16), so the vendor ID is the\n" +
			"only thing that identifies the device in both modes.",
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
			Path:      p.Path,
			Card:      p.Card,
			Device:    p.Device,
			Name:      p.Name,
			VendorID:  hexID(p.VendorID),
			ProductID: hexID(p.ProductID),
			IsVFlex:   p.IsVFlex,
		})
		// p.Name is device-supplied and goes into the table unquoted. That is
		// safe at the source rather than here: rawmidi's portName filters every
		// candidate through printableName, so anything outside printable ASCII
		// -- the escapes and newlines that would let a name forge a row -- is
		// gone before this ever sees it. Re-quoting here would only make the
		// common case harder to read.
		rows = append(rows, []string{p.Path, fmt.Sprintf("%d:%d", p.Card, p.Device), p.Name, idPair(p), vflexMark(p.IsVFlex)})
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

func vflexMark(ok bool) string {
	if ok {
		return "yes"
	}
	return ""
}
