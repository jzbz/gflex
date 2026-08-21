package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// withEuid points the privilege gate in installUdev at a fixed value for the
// duration of one test. See the comment on geteuid for why the seam exists.
func withEuid(t *testing.T, uid int) {
	t.Helper()
	saved := geteuid
	geteuid = func() int { return uid }
	t.Cleanup(func() { geteuid = saved })
}

// TestPackagedUdevRuleMatchesTheEmbeddedOne is the guard that makes the
// duplication safe.
//
// The rule exists twice on purpose -- go:embed cannot reach outside
// internal/cli, and SPEC.md §4.4 commits the project to shipping
// packaging/udev/70-gflex.rules for distribution packagers -- and until this
// test existed nothing at all noticed if the two diverged. Divergence would be
// invisible and nasty: distribution users would get a rule different from the
// one `gflex install-udev` writes, on a file whose whole job is granting
// access to the device.
func TestPackagedUdevRuleMatchesTheEmbeddedOne(t *testing.T) {
	packaged, err := os.ReadFile(filepath.Join("..", "..", "packaging", "udev", "70-gflex.rules"))
	if err != nil {
		t.Fatalf("reading the packaged rule: %v", err)
	}
	if len(packaged) == 0 {
		t.Fatal("packaging/udev/70-gflex.rules is empty")
	}
	if string(packaged) != udevRules {
		t.Errorf("packaging/udev/70-gflex.rules has drifted from the copy install-udev embeds.\n"+
			"packaged:\n%s\nembedded:\n%s", packaged, udevRules)
	}
}

// TestInstallUdevDryRunNeedsNoRoot pins the ordering of the two branches at the
// top of installUdev.
//
// The privilege check used to come first, so `gflex install-udev --dry-run` --
// the one dry run that touches nothing at all, and the one an unprivileged user
// most wants, since it is how they read the rule before typing sudo -- exited 6
// without printing anything.
func TestInstallUdevDryRunNeedsNoRoot(t *testing.T) {
	withEuid(t, 1000)

	var out, errOut bytes.Buffer
	app := &App{DryRun: true, stdout: &out, stderr: &errOut}
	f := newFormatter(false, &out, &errOut)
	if err := app.installUdev(context.Background(), f, udevRulesPath); err != nil {
		t.Fatalf("installUdev --dry-run as an ordinary user: %v", err)
	}
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		udevRulesPath,            // where it would go
		`SUBSYSTEM=="usb"`,       // and what it would contain
		"needs root: sudo gflex", // and that the real run is not free
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run output does not mention %q:\n%s", want, got)
		}
	}
}

// The privilege check still has to guard the real thing: a dry run being free
// must not make the write free.
func TestInstallUdevRefusesToWriteWithoutRoot(t *testing.T) {
	withEuid(t, 1000)

	path := filepath.Join(t.TempDir(), "70-gflex.rules")
	app := &App{stdout: io.Discard, stderr: io.Discard}
	f := newFormatter(false, io.Discard, io.Discard)
	err := app.installUdev(context.Background(), f, path)
	if err == nil {
		t.Fatal("installUdev as an ordinary user succeeded")
	}
	if code := ExitCode(err); code != ExitPermission {
		t.Errorf("ExitCode = %d, want ExitPermission (%d): %v", code, ExitPermission, err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Errorf("%s was written despite the refusal", path)
	}
}

// TestInstallUdevRefusesToOverwriteACustomisedRule is the regression test for a
// silent data loss.
//
// The rule ships with two commented-out fallbacks a headless or non-systemd
// host is meant to uncomment (SPEC.md §4.4), so an edited /etc copy is expected
// rather than exotic. install-udev used to compute `unchanged` and then
// overwrite anything that was not, with no prompt, no backup and no flag --
// there was no way to reinstall the rule after a firmware upgrade without
// destroying the local edit.
func TestInstallUdevRefusesToOverwriteACustomisedRule(t *testing.T) {
	withEuid(t, 0)

	path := filepath.Join(t.TempDir(), "70-gflex.rules")
	custom := udevRules + "\nSUBSYSTEM==\"usb\", ATTR{idVendor}==\"37bf\", GROUP=\"plugdev\"\n"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	// stdin is a bytes.Reader, so it is not a terminal: interlock 7 of
	// SPEC.md §13 refuses rather than prompting nobody.
	app := &App{stdout: io.Discard, stderr: io.Discard, stdin: bytes.NewReader(nil)}
	f := newFormatter(false, io.Discard, io.Discard)
	err := app.installUdev(context.Background(), f, path)
	if err == nil {
		t.Fatal("installUdev overwrote a customised rule without asking")
	}
	if code := ExitCode(err); code != ExitRefused {
		t.Errorf("ExitCode = %d, want ExitRefused (%d): %v", code, ExitRefused, err)
	}
	if msg := err.Error(); !strings.Contains(msg, path) || !strings.Contains(msg, "--yes") {
		t.Errorf("the refusal names neither the file nor the way through:\n%s", msg)
	}

	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != custom {
		t.Errorf("the customised rule was modified:\n%s", after)
	}
}

// TestWriteRuleFileIsAtomic covers the success half of the temp-file-and-rename
// write: exact bytes, 0644 (os.CreateTemp opens 0600, so the explicit Chmod is
// load-bearing), and no temporary file left behind.
func TestWriteRuleFileIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "70-gflex.rules")
	if err := writeRuleFile(path); err != nil {
		t.Fatalf("writeRuleFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != udevRules {
		t.Errorf("written content differs from the embedded rule:\n%s", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %04o, want 0644 -- udev's own tooling has to be able to read it", perm)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory holds %v, want only the rule file", names)
	}
}

// The half that matters: a write that fails must leave the previous rule
// intact. os.WriteFile truncates in place, so the old behaviour committed an
// empty file before writing a byte -- and /etc/udev/rules.d is re-read on every
// `udevadm control --reload-rules`, which means a truncated rule looks
// installed and grants nothing.
//
// The failure is injected by taking write permission off the directory, which
// stops the temporary file from being created but leaves the destination file
// itself writable -- exactly the case an in-place os.WriteFile would sail
// through, rewriting the file and failing this test's first assertion.
func TestWriteRuleFileLeavesTheOldRuleIntactWhenItFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the failure cannot be injected")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "70-gflex.rules")
	const old = "# a rule somebody is relying on\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if err := writeRuleFile(path); err == nil {
		t.Fatal("writeRuleFile reported success with an unwritable directory")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != old {
		t.Errorf("the previous rule was damaged by a failed write:\n%q", got)
	}
}

// TestUdevadmIsNotResolvedThroughPATH covers the one part of this root-only
// code path that was not fixed at compile time.
//
// runUdevadm executes behind the euid-0 check, and a bare command name is
// resolved through the PATH inherited from the caller -- so on a sudo without
// `Defaults secure_path`, a directory the invoking user controls would pick the
// binary root runs.
func TestUdevadmIsNotResolvedThroughPATH(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "udevadm")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	// Without this the test could pass for the wrong reason: if the shadow were
	// not actually reachable through PATH, resolving through PATH would not
	// have found it either and the assertion below would prove nothing.
	if p, err := exec.LookPath("udevadm"); err != nil || p != fake {
		t.Fatalf("PATH shadow not in place: LookPath = %q, %v", p, err)
	}

	got, err := udevadmPath()
	if got == fake {
		t.Errorf("udevadmPath returned the PATH shadow %q", got)
	}
	if err == nil && !slices.Contains(udevadmPaths, got) {
		t.Errorf("udevadmPath returned %q, which is not one of %v", got, udevadmPaths)
	}
	if err != nil {
		// A machine with no udevadm at all is a legitimate outcome; what
		// matters is that the shadow was refused, which is asserted above.
		t.Logf("no udevadm in the standard locations: %v", err)
	}
}
