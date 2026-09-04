package rawmidi

import (
	"errors"
	"fmt"
	"io/fs"

	"golang.org/x/sys/unix"
)

// Sentinel errors. The CLI maps these onto distinct exit codes (SPEC.md §11),
// so the classification must be stable; wrap them, never replace them.
var (
	// ErrBusy reports that the rawmidi node exists but is already open.
	// ALSA grants a rawmidi substream exclusively, per direction, so any other
	// MIDI client holding the port makes it unavailable to us (SPEC.md §4.1).
	ErrBusy = errors.New("rawmidi: port busy")

	// ErrPermission reports a udev/ACL problem opening the device node.
	ErrPermission = errors.New("rawmidi: permission denied")

	// ErrNotFound reports that the named port does not exist.
	ErrNotFound = errors.New("rawmidi: no such MIDI port")

	// ErrNotADevice reports that the path opened to a regular file rather than
	// to a device node. Only a regular file earns it: a regular file is the one
	// kind of object MIDI frames can silently corrupt, and Open refuses exactly
	// that and nothing else -- see the fstat in Open for why the check is not
	// "is this a character device".
	ErrNotADevice = errors.New("rawmidi: not a device node")

	// ErrNoPorts reports that the system exposes no ALSA rawmidi ports at all.
	ErrNoPorts = errors.New("rawmidi: no ALSA rawmidi ports found")

	// ErrAmbiguous reports that several ports matched and the caller must
	// disambiguate with --port.
	ErrAmbiguous = errors.New("rawmidi: several candidate MIDI ports")
)

// classifyOpenError turns an open(2) failure into one of the sentinels above,
// with advice a user can act on. The errno is preserved in the chain so callers
// can still inspect it.
func classifyOpenError(path string, err error) error {
	switch {
	case errors.Is(err, unix.EBUSY):
		// rawmidi substreams are opened exclusively per direction, and we open
		// O_RDWR, so either direction being taken yields EBUSY. Chrome heads the
		// holder list because the vendor ships a functionally identical web build
		// at vflex.app (SPEC.md §1) driven over Web MIDI, and comparing gflex
		// against that build is the likeliest reason for two clients to want the
		// port at once. On 2026-08-21 it was the actual holder on a second unit:
		// /proc/asound/seq/clients showed the VFLEX port connected to and from a
		// sequencer client named "Chrome", the kernel's snd_seq_midi having taken
		// the rawmidi node on its behalf. Naming only sound servers and DAWs sent
		// that user hunting software they were not running. The old "is already
		// open" clause makes room for the check: "another client already holds
		// it" says the same thing one sentence later.
		return fmt.Errorf("%w: %s. ALSA rawmidi allows one client per direction, so another MIDI "+
			"client already holds it -- a Chrome tab using Web MIDI (the vendor's web app at "+
			"vflex.app is one), PipeWire/WirePlumber, JACK, or a DAW. "+
			"/proc/asound/seq/clients lists what is connected to the port, and a Chrome tab "+
			"shows up there as a client named \"Chrome\". "+
			"Stop or disconnect that client, or retry with --transport usb: %w", ErrBusy, path, err)

	case errors.Is(err, fs.ErrPermission):
		// EACCES/EPERM. On systemd hosts 70-uaccess.rules already grants the
		// seat user an ACL on every /dev/snd node, so this normally means a
		// headless login, a service account, or a non-systemd distro.
		return fmt.Errorf("%w: cannot open %s. On a seat session udev grants access to /dev/snd "+
			"automatically; headless or non-systemd hosts need the packaged rule -- run "+
			"\"gflex install-udev\", or add this user to the audio group: %w", ErrPermission, path, err)

	case errors.Is(err, fs.ErrNotExist), errors.Is(err, unix.ENODEV), errors.Is(err, unix.ENXIO):
		return fmt.Errorf("%w: %s. The device may have been unplugged, or snd-usb-audio did not "+
			"claim its MIDI interface; run \"gflex devices\" to list what is present: %w",
			ErrNotFound, path, err)
	}
	return fmt.Errorf("rawmidi: open %s: %w", path, err)
}
