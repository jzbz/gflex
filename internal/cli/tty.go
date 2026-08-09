package cli

import (
	"golang.org/x/sys/unix"
)

// isTerminal reports whether fd refers to a terminal.
//
// TCGETS succeeds only on a character device with a line discipline, so a
// successful ioctl is the test. This is deliberately a two-line x/sys/unix call
// rather than a third-party dependency: the whole binary must build with
// CGO_ENABLED=0 and the dependency set is fixed.
func isTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	return err == nil
}

// stdinIsTTY reports whether the App's standard input is an interactive
// terminal.
//
// The safety interlocks in SPEC.md §13 hinge on this: a command that would
// normally prompt must refuse outright when stdin is a pipe or a file, so that
// a script cannot silently over-volt a load by answering a prompt it never saw.
//
// It asks a.stdin, not os.Stdin, because that is the reader the prompt will
// actually be answered from — every other stream in App is threaded the same
// way. Judging os.Stdin instead makes the two disagree for any embedder or test
// that supplies its own reader, and the disagreement is in the dangerous
// direction: the interlock would clear because the process was launched from a
// terminal while the answer came from somewhere else entirely.
//
// Anything that is not an *os.File cannot be a terminal, and fdOf hands those
// back as ^uintptr(0), which is not a valid descriptor, so the ioctl fails and
// the answer is "not a terminal". That is the direction interlock 7 requires
// (SPEC.md §13.7): a wrong "yes" here would turn a refusal into a prompt that
// nobody is there to answer, and the command would then hang or be answered by
// whatever bytes the pipe happens to carry.
func (a *App) stdinIsTTY() bool { return isTerminal(fdOf(a.stdin)) }
