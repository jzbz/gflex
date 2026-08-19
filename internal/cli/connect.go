package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/session"
	"github.com/jzbz/gflex/internal/transport/rawmidi"
	"github.com/jzbz/gflex/internal/transport/usbmidi"
	"github.com/jzbz/gflex/internal/usbfs"
)

// conn is an open session plus a human description of what was opened.
type conn struct {
	// Session is the protocol layer. Closing it closes the framer and the
	// underlying transport with it.
	Session *session.Session
	// Desc identifies the endpoint for diagnostics, e.g. a device node path.
	Desc string
}

// Close releases the device. The session owns the framer, which owns the
// transport, so this is the only close the caller needs.
func (c *conn) Close() error {
	if c == nil || c.Session == nil {
		return nil
	}
	return c.Session.Close()
}

// connect resolves the transport, builds a session and returns it ready to use.
// Every command that talks to hardware goes through here, so --port,
// --transport, --timeout, --byte-delay and -v behave identically everywhere.
//
// The caller must defer Close.
func (a *App) connect(ctx context.Context, f Formatter) (*conn, error) {
	t, desc, err := a.openTransport(ctx)
	if err != nil {
		return nil, err
	}
	opts := session.Options{
		ByteDelay: a.ByteDelay,
		Timeout:   a.Timeout,
	}
	if a.Verbose {
		opts.Trace = a.traceFunc(f)
	}
	s := session.New(t, opts)
	if a.Verbose {
		f.Diag("connected: %s", desc)
	}
	return &conn{Session: s, Desc: desc}, nil
}

// traceFunc builds the -v frame tracer. Frames are printed as raw hex plus the
// decoded command name, which is what a bring-up session actually needs (both
// directions are visible here because the session sees its own writes).
func (a *App) traceFunc(f Formatter) func(dir string, frame []byte) {
	return func(dir string, frame []byte) {
		f.Diag("%s", traceLine(time.Now(), dir, frame))
	}
}

// traceLine renders one traced frame.
func traceLine(at time.Time, dir string, frame []byte) string {
	label := ""
	if parsed, err := proto.Parse(frame); err == nil {
		label = " " + parsed.Cmd.String()
		if parsed.Write {
			label += " write"
		}
		if parsed.Scratchpad {
			label += " scratchpad"
		}
	}
	return fmt.Sprintf("%s %s %s%s", at.Format("15:04:05.000"), dir, proto.Hex(frame), label)
}

// openTransport opens the byte-level link selected by --transport and --port.
func (a *App) openTransport(ctx context.Context) (proto.Transport, string, error) {
	// The test seam (see App.testTransport). Consulted here rather than in
	// connect so that everything above the byte link -- the session, its
	// timeouts, the tracer, the interlocks the commands run first -- is the
	// real thing under test.
	if a.testTransport != nil {
		return a.testTransport(ctx)
	}
	switch a.Transport {
	case transportUSB:
		return a.openUSB(ctx)
	default:
		return a.openRawMIDI(ctx)
	}
}

// openRawMIDI opens the ALSA rawmidi node.
//
// A --port beginning with "/" is taken as a device node path and opened
// directly; anything else is a name hint handed to the discovery logic, which
// anchors on the USB vendor ID first and falls back to the vendor app's
// case-insensitive "vflex" substring match (SPEC.md §3.4).
func (a *App) openRawMIDI(ctx context.Context) (proto.Transport, string, error) {
	if strings.HasPrefix(a.Port, "/") {
		t, err := rawmidi.Open(a.Port)
		if err != nil {
			return nil, "", a.transportError(ctx, fmt.Errorf("opening %s: %w", a.Port, err))
		}
		return t, a.Port, nil
	}
	t, info, err := rawmidi.OpenAuto(a.Port)
	if err != nil {
		return nil, "", a.transportError(ctx, err)
	}
	a.warnSolePortFallback(info)
	return t, describePort(info), nil
}

// warnSolePortFallback tells the user when discovery could not identify a VFLEX
// and used the only MIDI port on the system instead.
//
// rawmidi.PortInfo.Fallback exists for exactly this and nothing else, and until
// now nobody read it. The fallback itself is worth keeping -- the vendor app has
// it too, and the port name a VFLEX actually advertises is unknown (SPEC.md
// §3.4, §14.2), so on a machine with one MIDI device it is usually right. But
// "usually right" is the whole problem: on a machine whose one MIDI device is a
// synthesizer, this path opens the synth and starts writing protocol frames at
// it, and every layer above here goes on to describe what it is doing as if it
// were talking to a VFLEX. Silence is what makes that indistinguishable from
// success.
//
// It writes straight to stderr rather than through a Formatter because the
// transport is opened below the layer that has one, and diagnostics go to
// stderr in both output modes anyway (see Formatter.Diag), so --json stdout
// stays a clean object either way.
func (a *App) warnSolePortFallback(p rawmidi.PortInfo) {
	if !p.Fallback || a.stderr == nil {
		return
	}
	fmt.Fprintf(a.stderr,
		"warning: no VFLEX was identified; using the only MIDI port on this system:\n"+
			"  %s\n"+
			"  Nothing about that port says it is a VFLEX -- not the USB vendor ID, not the port\n"+
			"  name. If it is a synthesizer or an audio interface, protocol frames are about to\n"+
			"  be written to it. Run `gflex devices` to see what was found, or pass --port.\n",
		describePort(p))
}

// openUSB opens the device through usbfs, bypassing ALSA entirely. This is the
// escape route when PipeWire, JACK or a DAW holds the rawmidi node, which is
// opened exclusively per direction (SPEC.md §4.1).
func (a *App) openUSB(ctx context.Context) (proto.Transport, string, error) {
	if a.Port == "" {
		t, ref, err := usbmidi.OpenAuto()
		if err != nil {
			return nil, "", a.transportError(ctx, err)
		}
		return t, describeUSB(ref), nil
	}
	refs, err := usbfs.Enumerate(proto.VendorID)
	if err != nil {
		return nil, "", a.transportError(ctx, err)
	}
	ref, ok, err := selectUSBRef(refs, a.Port)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", codedf(ExitNoDevice, "no USB device matching --port %q\n%s", a.Port, a.searchReport(ctx))
	}
	t, err := usbmidi.Open(ref)
	if err != nil {
		return nil, "", a.transportError(ctx, fmt.Errorf("opening %s: %w", ref.Path, err))
	}
	return t, describeUSB(ref), nil
}

// selectUSBRef applies --port to an enumerated device list: the unique match,
// or ok=false when nothing matched (the caller owns the not-found report).
//
// Every match is collected before anything is opened. matchesUSBPort ends in a
// suffix match and also takes a bare address, so with two VFLEX units attached
// an imprecise --port ("3" matching addr 3 on both buses) can designate both --
// and taking whichever usbfs.Enumerate happens to sort first would silently
// write the voltage to the wrong unit's rail. rawmidi.Select already refuses
// its ambiguous case for exactly this reason; this mirrors its wording, and
// carries ExitNoDevice -- the code rawmidi's ErrAmbiguous ends up with when it
// passes through transportError.
func selectUSBRef(refs []usbfs.DeviceRef, port string) (usbfs.DeviceRef, bool, error) {
	var matches []usbfs.DeviceRef
	for _, ref := range refs {
		if matchesUSBPort(ref, port) {
			matches = append(matches, ref)
		}
	}
	switch len(matches) {
	case 0:
		return usbfs.DeviceRef{}, false, nil
	case 1:
		return matches[0], true, nil
	}
	parts := make([]string, len(matches))
	for i, m := range matches {
		parts[i] = describeUSB(m)
	}
	return usbfs.DeviceRef{}, false, codedf(ExitNoDevice,
		"several USB devices match --port %q: %s. Pass --port with a device path",
		port, strings.Join(parts, ", "))
}

// matchesUSBPort reports whether --port designates ref. A path, a "bus:addr"
// pair and a bare address all work, because `gflex devices` prints all three.
func matchesUSBPort(ref usbfs.DeviceRef, port string) bool {
	if port == ref.Path || port == ref.SysPath {
		return true
	}
	if port == fmt.Sprintf("%d:%d", ref.Bus, ref.Addr) || port == fmt.Sprintf("%03d:%03d", ref.Bus, ref.Addr) {
		return true
	}
	if n, err := strconv.Atoi(port); err == nil && n == ref.Addr {
		return true
	}
	return strings.HasSuffix(ref.Path, port)
}

// transportError classifies a failure to open the device and attaches the
// guidance that actually resolves it.
func (a *App) transportError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, syscall.EBUSY):
		return coded(ExitBusy, fmt.Errorf("the device node is already open: %w", err))
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return coded(ExitPermission, err)
	}
	return codedf(ExitNoDevice, "%v\n%s", err, a.searchReport(ctx))
}

// searchReport builds the "no device found" body: what was searched, what was
// actually there, and the three fixes that resolve almost every case.
//
// Discovery errors are reported inline rather than replacing the report — a
// permission error on one path is itself the most useful clue.
func (a *App) searchReport(_ context.Context) string {
	var sb strings.Builder

	sb.WriteString("\nsearched:\n")
	fmt.Fprintf(&sb, "  ALSA rawmidi nodes under /dev/snd, matching USB vendor 0x%04X first,\n", proto.VendorID)
	fmt.Fprintf(&sb, "    then any port name containing %q (case-insensitive)\n", proto.PortNameMatch)
	fmt.Fprintf(&sb, "  USB devices under /dev/bus/usb with vendor 0x%04X\n", proto.VendorID)

	sb.WriteString("\nfound:\n")
	ports, perr := rawmidi.Discover()
	switch {
	case perr != nil:
		fmt.Fprintf(&sb, "  MIDI ports: could not enumerate: %v\n", perr)
	case len(ports) == 0:
		sb.WriteString("  MIDI ports: none\n")
	default:
		for _, p := range ports {
			mark := ""
			if !p.IsVFlex {
				mark = "   (not a VFLEX)"
			}
			fmt.Fprintf(&sb, "  MIDI port:  %s%s\n", describePort(p), mark)
		}
	}
	refs, uerr := usbfs.Enumerate(proto.VendorID)
	switch {
	case uerr != nil:
		fmt.Fprintf(&sb, "  USB devices: could not enumerate: %v\n", uerr)
	case len(refs) == 0:
		fmt.Fprintf(&sb, "  USB devices: none with vendor 0x%04X\n", proto.VendorID)
	default:
		for _, r := range refs {
			fmt.Fprintf(&sb, "  USB device: %s\n", describeUSB(r))
		}
	}

	sb.WriteString("\nmost likely fixes:\n")
	sb.WriteString("  1. permissions -- the node exists but is not readable by you:\n")
	sb.WriteString("       sudo gflex install-udev      (then unplug and replug the VFLEX)\n")
	sb.WriteString("  2. busy -- PipeWire, JACK or a DAW holds the rawmidi node exclusively:\n")
	sb.WriteString("       gflex --transport usb <command>\n")
	sb.WriteString("  3. wrong port -- more than one candidate, or an unrecognised port name:\n")
	sb.WriteString("       gflex devices                (then pass --port with one of the paths)\n")
	return sb.String()
}

// describePort renders a rawmidi port for humans.
func describePort(p rawmidi.PortInfo) string {
	var sb strings.Builder
	sb.WriteString(p.Path)
	if p.Name != "" {
		fmt.Fprintf(&sb, "  %q", p.Name)
	}
	if p.VendorID != 0 {
		fmt.Fprintf(&sb, "  %04x:%04x", p.VendorID, p.ProductID)
	}
	return sb.String()
}

// describeUSB renders a usbfs device reference for humans.
func describeUSB(r usbfs.DeviceRef) string {
	return fmt.Sprintf("%s  bus %03d addr %03d  %04x:%04x", r.Path, r.Bus, r.Addr, r.VendorID, r.ProductID)
}

// ---------------------------------------------------------------------------
// Presence polling
// ---------------------------------------------------------------------------

// devicePresent reports whether a VFLEX MIDI port is currently visible.
//
// Presence is judged on the USB vendor ID rather than the port name. The vendor
// app matches names only, which is why its firmware-update jump can report
// failure on a unit whose port name lacks "vflex" even though the jump
// succeeded (SPEC.md §3.4); anchoring on the VID avoids that bug entirely.
func devicePresent() bool {
	ports, err := rawmidi.Discover()
	if err != nil {
		return false
	}
	for _, p := range ports {
		if p.IsVFlex {
			return true
		}
	}
	return false
}

// usbPresent reports whether any device with the Tundra Labs vendor ID is on
// the bus. In bootloader mode there is no MIDI interface at all, so this is the
// only presence signal that works across a mode switch.
func usbPresent() bool {
	refs, err := usbfs.Enumerate(proto.VendorID)
	return err == nil && len(refs) > 0
}

// waitForDevice polls until the MIDI port's presence matches want, or the
// timeout expires. The poll interval is short enough to feel immediate and long
// enough not to spin on the sysfs walk.
func waitForDevice(ctx context.Context, want bool, timeout time.Duration) error {
	const poll = 250 * time.Millisecond
	deadline := time.Now().Add(timeout)
	for {
		if devicePresent() == want {
			return nil
		}
		if time.Now().After(deadline) {
			if want {
				return codedf(ExitNoDevice, "timed out after %s waiting for the VFLEX to reappear", timeout)
			}
			return codedf(ExitFailure, "timed out after %s waiting for the VFLEX to disconnect", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// sleepCtx sleeps for d unless the context is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
