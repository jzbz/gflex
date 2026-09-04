package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/jzbz/gflex/internal/bootloader"
	"github.com/jzbz/gflex/internal/session"
	"github.com/jzbz/gflex/internal/transport/rawmidi"
	"github.com/jzbz/gflex/internal/usbfs"
)

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil", err: nil, want: ExitOK},
		{name: "plain error", err: errors.New("something broke"), want: ExitFailure},
		{name: "explicit code", err: codedf(ExitUsage, "bad flag"), want: ExitUsage},
		{name: "interlock refusal", err: refused("too much voltage"), want: ExitRefused},
		{
			name: "wrapped explicit code",
			err:  fmt.Errorf("while doing the thing: %w", codedf(ExitNoDevice, "not found")),
			want: ExitNoDevice,
		},
		{
			name: "EBUSY from an exclusively held rawmidi node",
			err:  &fs.PathError{Op: "open", Path: "/dev/snd/midiC1D0", Err: syscall.EBUSY},
			want: ExitBusy,
		},
		{
			name: "EACCES with no udev rule",
			err:  &fs.PathError{Op: "open", Path: "/dev/bus/usb/001/007", Err: syscall.EACCES},
			want: ExitPermission,
		},
		{name: "fs.ErrPermission", err: fmt.Errorf("open: %w", fs.ErrPermission), want: ExitPermission},
		{name: "context deadline", err: fmt.Errorf("waiting: %w", context.DeadlineExceeded), want: ExitTimeout},
		{name: "os deadline", err: fmt.Errorf("read: %w", os.ErrDeadlineExceeded), want: ExitTimeout},
		{name: "cancelled", err: context.Canceled, want: ExitFailure},
		// The session layer reproduces the vendor's own wording and does not
		// wrap a context deadline; its sentinel is what makes it a timeout.
		{name: "session timeout sentinel", err: session.ErrTimeout, want: ExitTimeout},
		// The other direction: an error is not a timeout because its text says
		// so. Classification is by sentinel now, not by substring.
		{
			name: "a message that merely mentions a timeout is not one",
			err:  errors.New("could not parse the timeout in the config file"),
			want: ExitFailure,
		},
		{
			name: "an explicit code beats the sentinel underneath it",
			err:  coded(ExitNoDevice, &fs.PathError{Op: "open", Path: "/dev/snd/midiC1D0", Err: syscall.EBUSY}),
			want: ExitNoDevice,
		},
		// ESHUTDOWN reaches this switch only when nothing wrapped it: a usbfs
		// failure arrives as a usbfs.Error whose Class is already ErrNoDevice,
		// so what this covers is the bare errno a rawmidi write to a node the
		// kernel has swapped for the disconnected file operations would return.
		// Both sibling classifiers -- session.deviceGone and usbfs.classify --
		// count it as a device that went away, and this is where the agreement
		// between the three is pinned.
		{name: "a bare ESHUTDOWN", err: syscall.ESHUTDOWN, want: ExitNoDevice},
		// The two the set deliberately stops short of. A missing firmware image
		// is by far the commonest ENOENT at this level and stays a plain
		// failure, and os.ErrClosed is this tool closing its own port.
		{name: "ENOENT is not a device that went away", err: syscall.ENOENT, want: ExitFailure},
		{name: "a port this tool closed is not a device that went away", err: os.ErrClosed, want: ExitFailure},
		// The transport fstats the descriptor it opened and refuses a regular
		// file. Whichever of the two checks catches it -- the stat on the
		// --port name or this one -- the command line named the wrong object.
		{
			name: "a --port path the transport found was a regular file",
			err:  fmt.Errorf("opening /home/jz/notes.txt: %w", rawmidi.ErrNotADevice),
			want: ExitUsage,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExitCode(tt.err); got != tt.want {
				t.Errorf("ExitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// TestExitCodeTimeoutSentinels is the regression test for classifying timeouts
// by substring.
//
// ExitCode used to fall back to `strings.Contains(err.Error(), "timeout")`.
// Only the session layer phrases it that way; usbfs says "transfer timed out"
// and the bootloader says "timed out waiting for acknowledgement", so both
// missed and exited 1. Those are the two paths where the distinction carries
// the most weight -- a stalled flash leaves a unit that is still in bootloader
// mode and still re-flashable (SPEC.md §10.5), and a caller that sees a generic
// failure has no way to tell that from a dead device.
//
// Each error below is wrapped, because that is how it reaches ExitCode in
// practice, and wrapping is exactly what a text test cannot be trusted through.
func TestExitCodeTimeoutSentinels(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "session response timeout",
			err:  fmt.Errorf("CMD_VOLTAGE_MV: %w after 5s (tx 02 12)", session.ErrTimeout),
		},
		{
			name: "usbfs transfer timeout",
			err:  fmt.Errorf("bulk transfer on endpoint 0x81: %w", usbfs.ErrTimeout),
		},
		{
			name: "bootloader acknowledgement timeout",
			err:  fmt.Errorf("committing page 12: %w", bootloader.ErrACKTimeout),
		},
		{
			name: "raw ETIMEDOUT from a syscall",
			err:  fmt.Errorf("ioctl USBDEVFS_BULK: %w", syscall.ETIMEDOUT),
		},
		{
			name: "an os deadline, which reports itself through Timeout()",
			err:  fmt.Errorf("read /dev/snd/midiC1D0: %w", os.ErrDeadlineExceeded),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExitCode(tt.err); got != ExitTimeout {
				t.Errorf("ExitCode(%v) = %d, want ExitTimeout (%d)", tt.err, got, ExitTimeout)
			}
		})
	}
}

// TestCobraUsageErrorsExitTwo drives a real cobra tree so that the messages
// under test are cobra's and pflag's own, not copies of them. If a release
// rephrases one, this fails rather than the exit code silently regressing to 1.
//
// SPEC.md §11 gives a wrong command line its own exit code. Before this, only
// flag-parse failures reached it, through the root command's FlagErrorFunc;
// everything cobra rejects afterwards -- an argument count, a stray argument
// after a subcommand, a missing required flag -- arrived as a bare error and
// exited 1, which claims the device failed when the user simply mistyped.
func TestCobraUsageErrorsExitTwo(t *testing.T) {
	newTree := func() *cobra.Command {
		nop := func(*cobra.Command, []string) error { return nil }
		root := &cobra.Command{Use: "vflex", SilenceUsage: true, SilenceErrors: true, RunE: nop}
		root.PersistentFlags().Duration("timeout", 5*time.Second, "")
		root.AddCommand(
			&cobra.Command{Use: "info", Args: cobra.NoArgs, RunE: nop},
			&cobra.Command{Use: "set", Args: cobra.ExactArgs(1), RunE: nop},
			&cobra.Command{Use: "raw", Args: cobra.MinimumNArgs(1), RunE: nop},
		)
		req := &cobra.Command{Use: "vlimit", Args: cobra.NoArgs, RunE: nop}
		req.Flags().String("low", "", "")
		if err := req.MarkFlagRequired("low"); err != nil {
			t.Fatalf("MarkFlagRequired: %v", err)
		}
		root.AddCommand(req)
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		return root
	}

	cases := []struct {
		name string
		args []string
	}{
		{"stray argument after a subcommand", []string{"info", "bogus"}},
		{"too few positional arguments", []string{"set"}},
		{"too many positional arguments", []string{"set", "12", "13"}},
		{"minimum arity not met", []string{"raw"}},
		{"unknown flag", []string{"--bogus"}},
		{"flag missing its value", []string{"--timeout"}},
		{"unparseable flag value", []string{"--timeout", "not-a-duration"}},
		{"unknown shorthand flag", []string{"-Z"}},
		{"required flag not set", []string{"vlimit"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newTree()
			root.SetArgs(tc.args)
			err := root.Execute()
			if err == nil {
				t.Fatalf("cobra accepted %q; the case no longer tests anything", tc.args)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Errorf("ExitCode(%q) = %d, want ExitUsage (%d)\n  message: %v",
					tc.args, got, ExitUsage, err)
			}
		})
	}

	// The same, through the tree the binary actually builds.
	for _, args := range [][]string{{"info", "bogus"}, {"voltage", "set"}} {
		t.Run("real tree "+strings.Join(args, " "), func(t *testing.T) {
			app := &App{stdout: io.Discard, stderr: io.Discard, stdin: strings.NewReader("")}
			root := NewRootCommand(app)
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs(args)
			err := root.Execute()
			if err == nil {
				t.Fatalf("vflex %v was accepted", args)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Errorf("ExitCode for `vflex %s` = %d, want ExitUsage (%d)\n  message: %v",
					strings.Join(args, " "), got, ExitUsage, err)
			}
		})
	}
}

// A device-side failure must not be mistaken for a mistyped command line just
// because its wording brushes past one of the templates.
func TestDeviceErrorsAreNotUsageErrors(t *testing.T) {
	for _, err := range []error{
		syscall.EINVAL, // renders as exactly "invalid argument"
		errors.New("decoding the PDO log: invalid argument count in the blob"),
		fmt.Errorf("reading device info: %w", errors.New("unknown command code 42 in the response")),
	} {
		if got := ExitCode(err); got == ExitUsage {
			t.Errorf("ExitCode(%v) = ExitUsage; a device error was read as a command-line error", err)
		}
	}
}

// Every distinct failure class must have its own code, or scripts cannot branch
// on them.
func TestExitCodesAreDistinct(t *testing.T) {
	codes := map[int]string{
		ExitOK:         "ok",
		ExitFailure:    "failure",
		ExitUsage:      "usage",
		ExitNoDevice:   "no device",
		ExitBusy:       "busy",
		ExitTimeout:    "timeout",
		ExitPermission: "permission",
		ExitRefused:    "refused",
	}
	if len(codes) != 8 {
		t.Fatalf("exit codes collide: %v", codes)
	}
	for _, want := range []int{0, 1, 2, 3, 4, 5, 6, 7} {
		if _, ok := codes[want]; !ok {
			t.Errorf("no exit code with value %d", want)
		}
	}
}

func TestCodedErrorUnwraps(t *testing.T) {
	base := errors.New("root cause")
	err := coded(ExitBusy, fmt.Errorf("opening: %w", base))
	if !errors.Is(err, base) {
		t.Error("CodedError must not hide the error it wraps")
	}
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != ExitBusy {
		t.Errorf("errors.As did not recover the exit code")
	}
	if coded(ExitBusy, nil) != nil {
		t.Error("coded(nil) must stay nil")
	}
}

// A refusal must state that nothing was written; that sentence is the whole
// contract of the interlocks.
func TestRefusedSaysNothingWasWritten(t *testing.T) {
	err := refused("out of range")
	if got := err.Error(); !strings.Contains(got, "nothing was written") {
		t.Errorf("refusal %q must say nothing was written", got)
	}
}
