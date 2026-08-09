package cli

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

// udevRules is the rule file from SPEC.md §4.4.
//
// It matches on the vendor ID alone, because the product ID is genuinely
// unknown: it appears nowhere in the vendor application, and the only PID ever
// published for this device comes from the vendor's own udev rule, which
// targets the wrong subsystem entirely and hedges about the value (SPEC.md
// §4.4, §14.1).
//
//go:embed 70-gflex.rules
var udevRules string

// udevRulesPath is where a manually installed rule belongs. Distribution
// packages should ship theirs under /usr/lib/udev/rules.d instead, so that a
// user's copy here still wins.
const udevRulesPath = "/etc/udev/rules.d/70-gflex.rules"

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
				return app.installUdev(ctx, f)
			})
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "write the rule to stdout instead of installing it")
	return cmd
}

func (a *App) installUdev(ctx context.Context, f Formatter) error {
	if os.Geteuid() != 0 {
		// The message below already names the fix, so the generic
		// "run install-udev" hint would only repeat it.
		return codedSelfExplanatory(ExitPermission,
			"installing a udev rule needs root.\n\n"+
				"  Re-run as:   sudo gflex install-udev\n"+
				"  Or inspect the rule first, and place it yourself:\n"+
				"      gflex install-udev --print | sudo tee %s\n"+
				"      sudo udevadm control --reload-rules && sudo udevadm trigger",
			udevRulesPath)
	}

	if a.DryRun {
		f.KV("dry_run", "dry run", true, "nothing was written")
		f.KV("path", "would write", udevRulesPath, udevRulesPath)
		f.Note("")
		f.Note("%s", udevRules)
		return nil
	}

	existing, err := os.ReadFile(udevRulesPath)
	unchanged := err == nil && string(existing) == udevRules

	if err := os.MkdirAll(filepath.Dir(udevRulesPath), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(udevRulesPath), err)
	}
	if !unchanged {
		if err := os.WriteFile(udevRulesPath, []byte(udevRules), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", udevRulesPath, err)
		}
	}

	f.KV("path", "rule file", udevRulesPath, udevRulesPath)
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

// runUdevadm runs one udevadm subcommand, reporting failure as a diagnostic
// rather than an error: the rule file is written either way, and a manual
// reload is a one-liner.
func runUdevadm(ctx context.Context, f Formatter, args ...string) bool {
	cmd := exec.CommandContext(ctx, "udevadm", args...)
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
