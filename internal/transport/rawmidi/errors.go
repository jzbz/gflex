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
		// O_RDWR, so either direction being taken yields EBUSY. On a desktop
		// the holder is almost always the session sound server.
		return fmt.Errorf("%w: %s is already open. ALSA rawmidi allows one client per direction, "+
			"so another MIDI client is holding it (PipeWire/WirePlumber, JACK, or a DAW). "+
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
