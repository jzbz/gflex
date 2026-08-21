package cli

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Build stamps. These are the only package-level variables in the package and
// exist because -ldflags is the only way to inject them:
//
//	go build -ldflags "-X github.com/jzbz/gflex/internal/cli.Version=1.0.0"
//
// When they are not set, the values are recovered from the module's build info.
var (
	// Version is the release version.
	Version = ""
	// Commit is the VCS revision the binary was built from.
	Commit = ""
	// BuildDate is the build timestamp.
	BuildDate = ""
)

func newVersionCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the gflex version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(_ context.Context, f Formatter) error {
				v, commit, date := buildStamps()
				// The label names the program, not the device. These stamps
				// are gflex's own build identity; the attached unit's firmware
				// version is a different value read over the wire by
				// `gflex firmware version` (SPEC.md §6.4), and labelling this
				// line "vflex" invited the two to be read as one.
				f.KV("version", "gflex", v, v)
				if commit != "" {
					f.KV("commit", "commit", commit, commit)
				}
				if date != "" {
					f.KV("build_date", "built", date, date)
				}
				f.KV("go_version", "go", runtime.Version(), runtime.Version())
				f.KV("platform", "platform", runtime.GOOS+"/"+runtime.GOARCH, runtime.GOOS+"/"+runtime.GOARCH)
				return nil
			})
		},
	}
}

// buildStamps resolves the version, revision and build date, preferring
// -ldflags values and falling back to the Go build info embedded in every
// module binary.
func buildStamps() (version, commit, date string) {
	version, commit, date = Version, Commit, BuildDate
	info, ok := debug.ReadBuildInfo()
	if !ok {
		if version == "" {
			version = "dev"
		}
		return version, commit, date
	}
	if version == "" {
		version = info.Main.Version
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if commit == "" {
				commit = s.Value
			}
		case "vcs.time":
			if date == "" {
				date = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" && commit != "" {
				commit += " (dirty)"
			}
		}
	}
	if version == "" || version == "(devel)" {
		version = "dev"
	}
	return version, commit, date
}

// versionString is the one-line form used in help output and diagnostics.
func versionString() string {
	v, commit, _ := buildStamps()
	if commit == "" {
		return v
	}
	return fmt.Sprintf("%s (%s)", v, commit)
}
