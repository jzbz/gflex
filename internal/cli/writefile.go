package cli

import (
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path by way of a temporary file in the same
// directory and a rename.
//
// os.WriteFile would truncate the destination in place, committing a
// zero-length file to the filesystem before any of the new bytes reach it, and
// it cleans up nothing on the error path. A rename within one directory is
// atomic, so a reader sees either the whole of the old file or the whole of the
// new one -- never the window in between, whatever a signal, a full disk or a
// power cut does to this process partway through.
//
// Both callers need that, for the same reason and with different consequences.
// The udev rule lands in a directory udev re-reads on every `udevadm control
// --reload-rules`, where a truncated rule looks installed and grants no access
// at all. A saved firmware image is later handed back to `firmware flash`,
// where a truncated-but-parseable JSON is an image with pages missing -- and
// one that can still flash and verify cleanly, since a wrong page split has no
// per-page equality constraint to trip over (SPEC.md §10.2).
//
// The temporary name is derived from the destination's own, so it sorts beside
// it and is recognisable if anything ever does leave one behind. The leading
// dot and the ".tmp" suffix are also what keep udev from loading a half-written
// rule: it matches *.rules, and this is never one until the rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(name)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		return err
	}
	// CreateTemp opens 0600, which is not what either destination wants.
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	// Sync before the rename: a file that exists only in the page cache is one
	// power cut away from being the zero-length file this function exists to
	// avoid, and the rename would have long since claimed otherwise.
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
