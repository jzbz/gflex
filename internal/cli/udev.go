package cli

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// udevRules is the rule file from SPEC.md §4.4.
//
// It matches on the vendor ID alone, and that is still right, but not for the
// reason first given here: the product ID is no longer unknown. It was measured
// as 0x800F on hardware, and the vendor's own udev rule had that value right
// even though it appears nowhere in the vendor application (SPEC.md §14.1).
// What §14.1 answers is application mode only; question 1 asked for both modes,
// and the bootloader's PID is still unmeasured (§14.16 is open). A rule naming
// the application PID would therefore stop granting access at exactly the
// moment a firmware flash needs it. Matching on the VID alone also mirrors the
// vendor app's own WebUSB filter, which names no PID at all.
//
// The file is duplicated at packaging/udev/70-gflex.rules because go:embed
// cannot reach outside this directory; TestPackagedUdevRuleMatchesTheEmbeddedOne
// is what keeps the two byte-identical.
//
//go:embed 70-gflex.rules
var udevRules string

// udevRulesPath is where a manually installed rule belongs. Distribution
// packages should ship theirs under /usr/lib/udev/rules.d instead, so that a
// user's copy here still wins.
const udevRulesPath = "/etc/udev/rules.d/70-gflex.rules"

// geteuid is os.Geteuid, indirected so that the privilege gate below can be
// driven from a test.
//
// The gate is the first thing an unprivileged caller meets, so without this
// seam every assertion about what happens *after* it -- the refusal to
// overwrite somebody's hand-edited rule, above all -- could only be made by
// calling a helper directly, which is exactly the kind of test that keeps
// passing after the call to the helper is deleted. Nothing in the shipped tree
// assigns to it.
var geteuid = os.Geteuid

func newInstallUdevCommand(app *App) *cobra.Command {
	var printOnly bool
	cmd := &cobra.Command{
		Use:   "install-udev",
		Short: "Install the udev rule that grants access to the device",
		Long: "install-udev writes " + udevRulesPath + ", reloads the udev rules and\n" +
			"triggers them.\n\n" +
			"On a systemd distribution the rule is only needed for the direct-USB transport and\n" +
			"the bootloader: /usr/lib/udev/rules.d/70-uaccess.rules already grants the seat user\n" +
			"an ACL on every /dev/snd node, which covers the default rawmidi path. There is no\n" +
			"equivalent generic rule for USB (SPEC.md §4.4).\n\n" +
			"Use --print to inspect the rule, or to redirect it somewhere else yourself.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if printOnly {
				_, err := fmt.Fprint(app.stdout, udevRules)
				return err
			}
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				return app.installUdev(ctx, f, udevRulesPath)
			})
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "write the rule to stdout instead of installing it")
	return cmd
}

// installUdev installs the embedded rule at path.
//
// The destination is a parameter rather than the constant so that the write
// path is reachable from a test: the shipped path is under /etc, and a test
// that had to be root to exercise the overwrite guard would never be run.
func (a *App) installUdev(ctx context.Context, f Formatter, path string) error {
	// --dry-run is answered before the privilege check, not after. Nothing in
	// this branch touches the filesystem or spawns udevadm, and install-udev is
	// precisely the command whose dry run an unprivileged user wants: it is how
	// they see the rule and the destination before deciding to type sudo.
	// SPEC.md §13.8 withholds a dry run only where the frame cannot be known
	// without first reading the device, which does not describe this command.
	if a.DryRun {
		f.KV("dry_run", "dry run", true, "nothing was written")
		f.KV("path", "would write", path, path)
		if geteuid() != 0 {
			f.Note("Writing it for real needs root: sudo gflex install-udev")
		}
		f.Note("")
		f.Note("%s", udevRules)
		return nil
	}

	if geteuid() != 0 {
		// The message below already names the fix, so the generic
		// "run install-udev" hint would only repeat it.
		return codedSelfExplanatory(ExitPermission,
			"installing a udev rule needs root.\n\n"+
				"  Re-run as:   sudo gflex install-udev\n"+
				"  Or inspect the rule first, and place it yourself:\n"+
				"      gflex install-udev --print | sudo tee %s\n"+
				"      sudo udevadm control --reload-rules && sudo udevadm trigger",
			path)
	}

	existing, err := os.ReadFile(path)
	unchanged := err == nil && string(existing) == udevRules

	// A file that exists but does not match the embedded copy is somebody's
	// edit, not our own leftover: the rule ships with two commented-out
	// fallbacks (plugdev, audio) that a headless or non-systemd host is meant
	// to uncomment, and SPEC.md §4.4 says the group should be a packaging
	// variable. Replacing that silently destroys the only copy. Confirmation
	// goes through the same helper as the SPEC.md §13 interlocks, so --yes
	// works in a script and a non-interactive run is refused rather than
	// assumed.
	if err == nil && !unchanged {
		q := fmt.Sprintf("%s already exists and differs from the rule gflex installs.\n"+
			"  Overwriting it discards those edits (compare with: gflex install-udev --print).\n"+
			"  Overwrite it?", path)
		if err := a.confirm(ctx, q); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if !unchanged {
		if err := writeRuleFile(path); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}

	f.KV("path", "rule file", path, path)
	if unchanged {
		f.KV("written", "written", false, "already up to date")
	} else {
		f.KV("written", "written", true, "yes")
	}

	reloaded := runUdevadm(ctx, f, "control", "--reload-rules")
	triggered := runUdevadm(ctx, f, "trigger")
	f.KV("reloaded", "rules reloaded", reloaded, boolWord(reloaded))
	f.KV("triggered", "rules triggered", triggered, boolWord(triggered))

	f.Note("")
	if reloaded && triggered {
		f.Note("Unplug and replug the VFLEX for the new permissions to apply to it, then run:")
	} else {
		f.Note("udevadm did not run cleanly. Reload manually, then replug the VFLEX:")
		f.Note("  sudo udevadm control --reload-rules && sudo udevadm trigger")
	}
	f.Note("  gflex devices")
	return nil
}

// writeRuleFile installs the embedded rule at path.
//
// The write is atomic (writefile.go) because this file lives in a directory
// udev re-reads on every `udevadm control --reload-rules`, and a rule truncated
// partway through an install looks installed while granting no access at all.
// 0644 rather than the 0600 a temporary file is created with: the installed
// rule is world-readable, matching every other file in /etc/udev/rules.d.
func writeRuleFile(path string) error {
	return writeFileAtomic(path, []byte(udevRules), 0o644)
}

// udevadmPaths are the standard locations for udevadm, in the order they are
// tried.
//
// Resolution is deliberately not left to exec.LookPath. runUdevadm only ever
// runs behind the euid-0 check in installUdev, and a bare command name is
// resolved against the PATH inherited from the caller -- so on a sudo
// configuration without `Defaults secure_path`, a directory the invoking user
// controls would decide which binary root executes. The arguments are already
// compile-time literals; this makes the executable one too.
//
// On a merged-/usr system the last two entries are symlinks to the first two.
// Both spellings are listed because the split-/usr systems that still exist put
// udevadm only in /sbin.
var udevadmPaths = []string{
	"/usr/bin/udevadm",
	"/usr/sbin/udevadm",
	"/bin/udevadm",
	"/sbin/udevadm",
}

// udevadmPath returns the first entry of udevadmPaths that exists as a regular
// file.
func udevadmPath() (string, error) {
	for _, p := range udevadmPaths {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no udevadm in %s", strings.Join(udevadmPaths, ", "))
}

// runUdevadm runs one udevadm subcommand, reporting failure as a diagnostic
// rather than an error: the rule file is written either way, and a manual
// reload is a one-liner.
func runUdevadm(ctx context.Context, f Formatter, args ...string) bool {
	bin, err := udevadmPath()
	if err != nil {
		f.Diag("warning: %v", err)
		return false
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.Diag("warning: udevadm %v failed: %v", args, err)
		if len(out) > 0 {
			f.Diag("  %s", out)
		}
		return false
	}
	return true
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
