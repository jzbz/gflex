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
// power cut does to this process partway through. Two syncs are what make the
// last of those true rather than merely likely: one for the contents, one for
// the directory entry the rename creates.
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
	if err = os.Rename(name, path); err != nil {
		return err
	}
	// And sync the directory, because the rename is not covered by the sync
	// above: fsync commits a file's contents, not the directory entry that
	// names it. Without this the promise at the top of this function stops one
	// step short of a power cut -- the bytes are on the disk under a name the
	// directory may still not carry, so what comes back is the old file, or the
	// temporary name, or neither. Opening the directory read-only and syncing
	// it is what makes the rename itself durable.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err = dir.Sync(); err != nil {
		dir.Close()
		return err
	}
	return dir.Close()
}
