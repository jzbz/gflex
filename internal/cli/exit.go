package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"github.com/jzbz/gflex/internal/bootloader"
	"github.com/jzbz/gflex/internal/session"
	"github.com/jzbz/gflex/internal/transport/rawmidi"
	"github.com/jzbz/gflex/internal/usbfs"
)

// Process exit codes. Every failure path maps onto exactly one of these so that
// scripts can branch on the failure class without parsing messages.
const (
	// ExitOK indicates the command completed successfully.
	ExitOK = 0
	// ExitFailure is the generic failure code: something went wrong that does
	// not fit any of the more specific classes below.
	ExitFailure = 1
	// ExitUsage indicates the command line itself was wrong: unknown flag,
	// unknown subcommand, bad argument count, unparseable value.
	ExitUsage = 2
	// ExitNoDevice indicates no VFLEX could be found on the selected transport.
	ExitNoDevice = 3
	// ExitBusy indicates the device node exists but is held by another process.
	// ALSA rawmidi is opened exclusively per direction, so a Chrome tab using Web
	// MIDI -- the vendor ships one (SPEC.md §1) -- or PipeWire, JACK or a
	// DAW holding the port produces this (SPEC.md §4.1).
	ExitBusy = 4
	// ExitTimeout indicates the device did not answer within the response
	// timeout. The protocol has no NACK, so every device-side refusal also
	// surfaces as a timeout (SPEC.md §5.2).
	ExitTimeout = 5
	// ExitPermission indicates the device node was found but could not be
	// opened for lack of permission; `gflex install-udev` is the usual fix.
	ExitPermission = 6
	// ExitRefused indicates a safety interlock in SPEC.md §13 blocked the
	// operation. Nothing was written to the device.
	ExitRefused = 7
)

// CodedError couples an error with the process exit code it should produce.
type CodedError struct {
	// Code is the exit status the process should terminate with.
	Code int
	// Err is the underlying error.
	Err error
	// NoHint suppresses the generic per-code guidance. Set it when the error
	// already says exactly what to do, so the user is not told twice.
	NoHint bool
}

// Error implements the error interface.
func (e *CodedError) Error() string { return e.Err.Error() }

// Unwrap exposes the wrapped error to errors.Is and errors.As.
func (e *CodedError) Unwrap() error { return e.Err }

// coded wraps err so that it terminates the process with the given exit code.
func coded(code int, err error) error {
	if err == nil {
		return nil
	}
	return &CodedError{Code: code, Err: err}
}

// codedf builds a CodedError from a format string.
func codedf(code int, format string, args ...any) error {
	return &CodedError{Code: code, Err: fmt.Errorf(format, args...)}
}

// codedSelfExplanatory is codedf for an error whose own text already tells the
// user what to do; the generic per-code hint is suppressed.
func codedSelfExplanatory(code int, format string, args ...any) error {
	return &CodedError{Code: code, Err: fmt.Errorf(format, args...), NoHint: true}
}

// refused builds the error returned when a safety interlock blocks an
// operation. The wording deliberately states that nothing was written.
//
// That sentence is a claim about the caller, and the callers have to keep it
// true. It holds for every SPEC.md §13 confirmation because each one is
// evaluated ahead of the write it guards -- App.apply runs before App.connect
// at each of its call sites. It does NOT hold for the scan wizard's handover
// prompt, which runs after the capture log has been erased, so that path
// reports a usage error instead; see errNoAnswer in root.go.
func refused(reason string) error {
	return codedf(ExitRefused, "refused: %s\nnothing was written to the device", reason)
}

// suppressHint reports whether err asked for the generic guidance to be left
// off.
func suppressHint(err error) bool {
	var ce *CodedError
	return errors.As(err, &ce) && ce.NoHint
}

// ExitCode maps an error onto one of the exit codes above.
//
// An explicit CodedError anywhere in the chain wins. Otherwise the error is
// classified by the sentinels the layers below surface: EBUSY from an
// exclusively-held rawmidi node, EACCES/EPERM from a missing udev rule, and the
// per-layer timeouts of isTimeout. Only cobra's complaints about the command
// line itself, which carry no sentinel at all, are recognised by text.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	switch {
	case errors.Is(err, syscall.EBUSY):
		return ExitBusy
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return ExitPermission
	case errors.Is(err, rawmidi.ErrNotADevice):
		// The transport fstat'd the descriptor it had just opened and found a
		// regular file. openRawMIDI stats the --port path first and refuses the
		// same thing with the same wording, but a name and a descriptor answer
		// two different questions and only the descriptor's answer is
		// authoritative; this is the code for the one that is. Either way the
		// command line named the wrong object, which is a usage error and not a
		// device that failed.
		return ExitUsage
	case errors.Is(err, usbfs.ErrNoDevice), errors.Is(err, syscall.ENODEV),
		errors.Is(err, syscall.ENXIO), errors.Is(err, syscall.ESHUTDOWN):
		// A device that vanished mid-operation is the same outcome to a script
		// as one that was never there. Without this it unwraps only to ENODEV,
		// which nothing below matches, and lands on the generic failure code.
		//
		// ESHUTDOWN is here for agreement rather than for a live bug: every
		// shipped path that can produce it goes through usbfs.Error, whose
		// Class is already usbfs.ErrNoDevice, so nothing currently arrives here
		// as a bare ESHUTDOWN. It is listed because both sibling classifiers --
		// session.deviceGone and usbfs.classify -- list it, and a set described
		// as the same set should not quietly differ from one.
		//
		// The set stops here on purpose, and the two obvious additions are
		// wrong. syscall.ENOENT is one of them: usbfs maps it to ErrNoDevice
		// for a /dev/bus/usb path that disappeared, but by this level the
		// commonest ENOENT by far is a firmware image the user misnamed, and
		// that must stay exit 1 (localFileError). os.ErrClosed is the other:
		// that is this tool closing its own port, not a device that went away.
		// session.deviceGone matches it because a send after Close means "stop
		// retrying", which is a statement about the session and not about the
		// hardware a script would go looking for.
		return ExitNoDevice
	case isTimeout(err):
		return ExitTimeout
	case errors.Is(err, context.Canceled):
		return ExitFailure
	case isCommandLineError(err):
		return ExitUsage
	}
	return ExitFailure
}

// isTimeout reports whether err is one of the waits this tool can run out of.
//
// Every layer has its own deadline and its own sentinel: the session's 5 s
// response timeout (SPEC.md §5.2), a usbfs transfer's ETIMEDOUT, and the
// bootloader's 15 s ACK wait (SPEC.md §7). Matching them by name is not a style
// preference. The test this replaced looked for the substring "timeout", and
// both usbfs and the bootloader say "timed out" instead, so a flash that
// stalled waiting for an acknowledgement and a usbfs transfer that never
// completed both exited 1 rather than 5 -- exactly the two cases where knowing
// it was a timeout matters most, because a stalled bootloader leaves a unit
// that is still re-flashable (SPEC.md §10.5) and the user needs to be told to
// retry rather than to conclude the device is dead.
//
// The substring test is gone rather than kept as a fallback. It is not a
// contract with anything: it under-matched every "timed out" phrasing while
// over-matching any unrelated error whose text happens to mention a timeout,
// and a misclassification in that direction sends the user to the "the device
// did not answer" hint for a fault that has nothing to do with the device.
// What replaces it is broader, not narrower: the three sentinels, both context
// deadlines, the raw errno, and the net/os Timeout() shape.
func isTimeout(err error) bool {
	if errors.Is(err, session.ErrTimeout) ||
		errors.Is(err, usbfs.ErrTimeout) ||
		errors.Is(err, bootloader.ErrACKTimeout) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	// net.Error and the os poller report themselves this way; it is a shape,
	// not a string, so it stays honest.
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

// commandLineErrorPrefixes are the message templates cobra and pflag produce
// for a malformed command line.
//
// Matching on text is what isTimeout above exists to avoid, so the exception
// needs its reason stated. Neither cobra nor pflag gives these errors a type or
// a sentinel, and the one hook either offers -- SetFlagErrorFunc, which the root
// command already uses to code flag-parse failures -- covers only ParseFlags.
// It does not cover Command.ValidateArgs, ValidateRequiredFlags or
// ValidateFlagGroups, which is where `gflex info bogus` and `gflex voltage set`
// with no argument end up. Those arrive as a bare errors.errorString, and every
// one of them means the command line was wrong: SPEC.md §11 gives that its own
// exit code, and 1 (something went wrong talking to the device) is the wrong
// answer for a typo.
//
// Each template is anchored at the start of the message, and includes cobra's
// opening quote wherever it quotes, so an error that merely contains one of
// these words cannot match: syscall.EINVAL renders as exactly "invalid
// argument" and must stay a generic failure.
var commandLineErrorPrefixes = []string{
	`unknown command "`,          // cobra: NoArgs, legacyArgs
	`invalid argument "`,         // cobra: OnlyValidArgs; pflag: an unparseable flag value
	"unknown flag: ",             // pflag
	"unknown shorthand flag: ",   // pflag
	"flag needs an argument: ",   // pflag
	"bad flag syntax: ",          // pflag
	"required flag(s) ",          // cobra: ValidateRequiredFlags
	"if any flags in the group ", // cobra: ValidateFlagGroups
}

// isCommandLineError reports whether err is cobra's or pflag's complaint about
// the command line rather than a failure to reach the device.
//
// Only the outermost message is examined. Nothing in this tool wraps a cobra
// error, so a match against an inner one would mean the text collided by
// accident. TestCobraUsageErrorsExitTwo drives a real cobra tree to produce
// every shape below rather than asserting on copied strings, so a cobra release
// that rephrases one fails the test instead of quietly regressing the code.
func isCommandLineError(err error) bool {
	msg := err.Error()
	for _, p := range commandLineErrorPrefixes {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	// The arity validators -- ExactArgs, MinimumNArgs, MaximumNArgs, RangeArgs
	// -- all end alike: "accepts 1 arg(s), received 0", "requires at least 1
	// arg(s), only received 0".
	return strings.Contains(msg, "arg(s), received ") ||
		strings.Contains(msg, "arg(s), only received ")
}

// exitHint returns extra guidance printed under the error message for the
// failure classes where the fix is not obvious from the error alone.
func exitHint(code int) string {
	switch code {
	case ExitBusy:
		return "the ALSA rawmidi node is opened exclusively per direction; a Chrome tab using\n" +
			"Web MIDI (the vendor's own web app is one), PipeWire, JACK or a DAW may be holding\n" +
			"it. Check with: cat /proc/asound/seq/clients\n" +
			"Closing that client is the better fix; --transport usb is the fallback, and on at\n" +
			"least one kernel it costs the ALSA MIDI port until the device is replugged (§4.2)."
	case ExitPermission:
		return "no permission to open the device node. Try: sudo gflex install-udev\n" +
			"then unplug and replug the VFLEX."
	case ExitTimeout:
		return "the device did not answer. The protocol has no error response, so a rejected or\n" +
			"unsupported command looks exactly like this. Try -v to see the raw traffic."
	}
	return ""
}
