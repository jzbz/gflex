package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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
	// stderr is where Close reports the one failure the caller cannot.
	//
	// Every command closes with `defer c.Close()` and discards the result, and
	// that is right: a close error must never surface as a command failure,
	// because the command already succeeded (SPEC.md §17, on ErrReadBack). The
	// cost of that convention is that a close error nobody reads is a close
	// error nobody sees, which is fine for every close error but one.
	stderr io.Writer
}

// Close releases the device. The session owns the framer, which owns the
// transport, so this is the only close the caller needs.
//
// It writes one diagnostic straight to stderr rather than relying on the return
// value, because usbfs.ErrDriverNotRebound is not a report about this command:
// it says the kernel driver this process displaced to use --transport usb did
// not come back, so the user's ALSA MIDI port is gone until they replug the
// device, and every later gflex run on the default transport will fail to find
// it. Returning that to a caller who discards close errors -- which all of them
// do, correctly -- would leave the user with a broken device and no explanation
// (SPEC.md §4.2).
func (c *conn) Close() error {
	if c == nil || c.Session == nil {
		return nil
	}
	err := c.Session.Close()
	warnDriverNotRebound(c.stderr, err)
	return err
}

// warnDriverNotRebound prints the rebind warning for a close error that carries
// usbfs.ErrDriverNotRebound, and says nothing for anything else.
//
// It is a free function rather than part of conn.Close because `monitor` does
// not build a conn: it drives the framer directly and closes it with the
// transport underneath, so on --transport usb it could detach snd-usb-audio,
// fail to get it back and say nothing at all -- the one outcome conn.Close
// exists to report. Two call sites, one wording: a warning about a device the
// user now has to unplug is not a sentence to keep two copies of.
func warnDriverNotRebound(w io.Writer, err error) {
	if err == nil || w == nil || !errors.Is(err, usbfs.ErrDriverNotRebound) {
		return
	}
	fmt.Fprintf(w,
		"warning: the kernel driver this process detached to claim the device did not rebind.\n"+
			"  The ALSA MIDI port is gone until the device is unplugged and plugged back in, so\n"+
			"  commands without --transport usb will not find it (SPEC.md §4.2).\n"+
			"  %v\n", err)
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
	return &conn{Session: s, Desc: desc, stderr: a.stderr}, nil
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
		// The seam stands in for the ordinary case: a rawmidi port identified
		// by the vendor ID. Recording that here rather than in each test keeps
		// midiPresenceMeaningful answering as it does in production, and a test
		// that wants one of §3.4's weaker tiers clears the field itself.
		a.midiPortVIDConfirmed = a.Transport != transportUSB
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
		// --port says "yes, that one, I mean it" about IDENTIFICATION -- it
		// overrides the vendor ID, the name match and the sole-port fallback
		// alike (SPEC.md §3.4). It does not say the path is a device node, and
		// nothing else asks: rawmidi.Open is a plain O_RDWR open, so a regular
		// file named by mistake -- a stale GFLEX_PORT, a shell completion that
		// landed beside the node -- is opened and then WRITTEN to. The framer's
		// first SendFrame stamps MIDI messages over the start of the file and
		// the reader's EOF tears the session down a moment later, with nothing
		// on stderr saying the file was modified. Every other path into this
		// package refuses to write frames at something it cannot identify or
		// warns loudly first; this is that guard for the one path that skips
		// discovery entirely.
		//
		// Only a positive answer refuses. A path that does not exist falls
		// through untouched, so rawmidi.Open keeps classifying ENOENT and EACCES
		// the way it does today.
		if fi, serr := os.Stat(a.Port); serr == nil && fi.Mode()&os.ModeCharDevice == 0 {
			return nil, "", codedf(ExitUsage,
				"--port %s is not a character device; expected an ALSA rawmidi node such as "+
					"/dev/snd/midiC1D0 (see gflex devices)", a.Port)
		}
		t, err := openWaitingForACL(ctx, func() (proto.Transport, error) {
			p, oerr := rawmidi.Open(a.Port)
			if oerr != nil {
				return nil, oerr
			}
			return p, nil
		})
		if err != nil {
			return nil, "", a.transportError(ctx, fmt.Errorf("opening %s: %w", a.Port, err))
		}
		// --port skips discovery, so nothing has yet said what this node is.
		// Ask afterwards, best effort: it is the difference between "port
		// /dev/snd/midiC1D0" and a line naming the card and vendor, and it is
		// also what tells midiPresenceMeaningful whether rawmidi.Discover will
		// recognise this unit again (SPEC.md §3.4). A failure here changes
		// nothing -- the port is already open and the user asserted it.
		desc := a.Port
		if ports, derr := rawmidi.Discover(); derr == nil {
			for _, p := range ports {
				if p.Path == a.Port {
					a.midiPortVIDConfirmed = p.VendorID == proto.VendorID
					desc = describePort(p)
					break
				}
			}
		}
		return t, desc, nil
	}
	var info rawmidi.PortInfo
	t, err := openWaitingForACL(ctx, func() (proto.Transport, error) {
		p, i, oerr := rawmidi.OpenAuto(a.Port)
		info = i
		if oerr != nil {
			return nil, oerr
		}
		return p, nil
	})
	if err != nil {
		return nil, "", a.transportError(ctx, err)
	}
	a.warnSolePortFallback(info)
	a.warnNameOnlyMatch(info)
	// Only the vendor ID carries over to rawmidi.Discover; the name match and
	// the sole-port fallback are tiers of this selection alone. See
	// midiPresenceMeaningful.
	a.midiPortVIDConfirmed = info.VendorID == proto.VendorID
	return t, describePort(info), nil
}

// The window openWaitingForACL waits out, and how often it looks.
const (
	rawmidiACLGrace = time.Second
	rawmidiACLPoll  = 100 * time.Millisecond
)

// openWaitingForACL runs open, retrying a permission failure for a short
// bounded grace.
//
// devtmpfs creates /dev/snd/midiC*D* when the device registers; systemd-udevd
// applies the uaccess ACL that makes it readable by the seat user some
// milliseconds later. On a systemd host that ACL is the whole of the access
// (the shipped rule leaves its SUBSYSTEM=="sound" line commented out, SPEC.md
// §4.4), so anything that opens the node the instant it appears races it.
// `scan --no-prompt` does exactly that: it polls presence every 250 ms and
// reconnects on the first positive with no human delay in between, and so does
// the reconnect after a firmware flash. Losing the race costs a capture whose
// log has already been erased -- another walk to the charger (SPEC.md §9.2) --
// and reports it as EACCES, whose hint sends the user to install a udev rule
// they already have.
//
// Only EACCES/EPERM is retried, and deliberately not ENOENT: outside the replug
// window a missing node is the ordinary no-device case, by far the commonest
// failure this tool has, and making every one of those wait a second to fix the
// rarest is the wrong trade. Once the grace expires the error is returned
// unchanged, so transportError classifies it exactly as before.
func openWaitingForACL(ctx context.Context, open func() (proto.Transport, error)) (proto.Transport, error) {
	deadline := time.Now().Add(rawmidiACLGrace)
	for {
		t, err := open()
		if err == nil || !isPermissionDenied(err) || !time.Now().Before(deadline) {
			return t, err
		}
		select {
		case <-ctx.Done():
			// The open error, not ctx.Err(): the caller classifies what it is
			// given, and "permission denied" is what actually happened.
			return nil, err
		case <-time.After(rawmidiACLPoll):
		}
	}
}

// isPermissionDenied reports whether err is the kernel refusing an open for lack
// of permission, in any of the three shapes the layers below produce it.
func isPermissionDenied(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}

// warnNameOnlyMatch tells the user when the port was identified as a VFLEX by
// its advertised name alone, with no USB vendor ID behind the claim.
//
// classify now trusts the vendor ID exclusively wherever there is one, and
// reaches for the name substring only when discovery could not trace the port to
// a USB device at all -- which is SPEC.md §3.4's precedence: vendor ID first,
// name second, sole port last. Keeping that second tier is right, because a port
// whose sysfs walk failed is still probably the device. But it is a materially
// weaker claim than the one the vendor ID makes, and at the point of use the two
// were indistinguishable: IsVFlex read true either way.
//
// The name is evidence, not an identifier. "Werewolf VFLEX" is what one unit's
// firmware advertised (SPEC.md §14.2); it is not a protocol constant, no
// document promises a second revision keeps it, and nothing stops an unrelated
// device from spelling "vflex" in its own port name. So this says which tier the
// identification came from rather than presenting both as the same fact --
// exactly the distinction warnSolePortFallback draws one tier further down.
func (a *App) warnNameOnlyMatch(p rawmidi.PortInfo) {
	// Fallback ports are warned about by warnSolePortFallback, which says
	// strictly more; a port cannot need both messages.
	if !p.IsVFlex || p.VendorID != 0 || p.Fallback || a.stderr == nil {
		return
	}
	fmt.Fprintf(a.stderr,
		"warning: this port was identified as a VFLEX by its name alone:\n"+
			"  %s\n"+
			"  Discovery could not trace it back to a USB device, so the vendor ID never\n"+
			"  confirmed it. Run `gflex devices` to see what was found, or pass --port.\n",
		describePort(p))
}

// warnSolePortFallback tells the user when discovery could not identify a VFLEX
// and used the only MIDI port on the system instead.
//
// rawmidi.PortInfo.Fallback exists for exactly this and nothing else, and until
// now nobody read it. The fallback itself is worth keeping -- the vendor app has
// it too, and the name a unit advertises is firmware-dependent evidence rather
// than an identifier: one unit measured as "Werewolf VFLEX" (SPEC.md §14.2), so
// the substring match of SPEC.md §3.4 does hit a real device, but a second
// firmware revision may name itself anything at all. On a machine with one MIDI
// device the sole-port fallback is therefore usually right. But
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
// escape route when another MIDI client -- a Chrome tab using Web MIDI, or
// PipeWire, JACK or a DAW -- holds the rawmidi node, which is
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
// Every match is collected before anything is opened. matchesUSBPort takes a
// bare address and a trailing part of a path, so with two VFLEX units attached
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
// pair, a bare address and a trailing part of the path all work, because
// `gflex devices` prints the first three and the fourth is what someone types
// when they have half of a path in front of them.
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
	// Anchored on the separator, so a partial path is a whole number of path
	// components and not any string the path happens to end with. Unanchored,
	// --port 3 matched /dev/bus/usb/011/013 -- and where the unit the user
	// meant is not on the bus, that is a lone match rather than the ambiguity
	// selectUSBRef refuses, so the write went to the other unit's rail with
	// nothing said. "013" and "011/013" still designate it; "3" no longer does.
	return strings.HasSuffix(ref.Path, "/"+port)
}

// transportError classifies a failure to open the device and attaches the
// guidance that actually resolves it.
func (a *App) transportError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, syscall.EBUSY) && a.Transport == transportUSB:
		// EBUSY means something different on each transport, and the generic
		// ExitBusy guidance is written for the other one: it explains ALSA's
		// per-direction exclusivity, points at /proc/asound/seq/clients, and
		// ends by recommending --transport usb -- which is the transport this
		// run is already on. Here the errno comes from USBDEVFS_DISCONNECT_CLAIM
		// with another usbfs holder in the way (a second `gflex --transport usb`,
		// or a WebUSB tab), and the usbfs error already carries its own
		// remediation, so the hint is suppressed rather than replaced.
		return codedSelfExplanatory(ExitBusy, "the USB interface is already claimed by another process: %w", err)
	case errors.Is(err, syscall.EBUSY):
		return coded(ExitBusy, fmt.Errorf("the device node is already open: %w", err))
	case isPermissionDenied(err):
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
	sb.WriteString("  2. busy -- another MIDI client holds the rawmidi node exclusively. A Chrome tab\n")
	sb.WriteString("     using Web MIDI is the likeliest one for this device, since the vendor ships a\n")
	sb.WriteString("     Chrome web app; PipeWire, JACK and DAWs do it too:\n")
	sb.WriteString("       cat /proc/asound/seq/clients   (a Web MIDI tab shows up as \"Chrome\")\n")
	sb.WriteString("       gflex --transport usb <command>   (fallback; may cost the MIDI port until replug)\n")
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
