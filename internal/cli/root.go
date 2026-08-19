// Package cli implements the gflex command tree: flag handling, output
// formatting, the safety interlocks of SPEC.md §13, and the wiring from each
// subcommand down to the session, transport and bootloader packages.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jzbz/gflex/internal/framer"
	"github.com/jzbz/gflex/internal/proto"
)

// App holds the global flag state and the process's standard streams. It is
// created once by Execute and threaded through every subcommand, so that
// nothing in this package needs package-level mutable state.
type App struct {
	// Port is the --port value: a device node path, or a name substring to
	// match against the discovered ports.
	Port string
	// Transport selects the link: "rawmidi" (default) or "usb".
	Transport string
	// AsJSON is --json.
	AsJSON bool
	// Timeout is the per-command response timeout (--timeout).
	Timeout time.Duration
	// ByteDelay is the inter-MIDI-message delay (--byte-delay).
	ByteDelay time.Duration
	// Verbose enables hex TX/RX tracing on stderr (-v).
	Verbose bool
	// DryRun prints the frames a command would send and sends nothing.
	DryRun bool
	// Yes pre-answers every interlock confirmation.
	Yes bool

	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader

	// testTransport, when non-nil, replaces the link openTransport would have
	// opened. It is a test seam and nothing in the shipped tree ever sets it:
	// Execute builds the App with the field zero, and it is unexported so no
	// embedder can reach it either.
	//
	// It exists because the SPEC.md §13 interlocks are wired in, not just
	// implemented. Every Check* function is table-tested, but the tests could
	// only reach them directly; deleting an `app.apply(...)` call from a
	// command -- the line that turns a Decision into a refusal -- failed
	// nothing, so the safety contract for attached hardware rested on review
	// alone. With this seam a test drives the real cobra tree against
	// transport/fake and asserts on the frames the device did or did not
	// receive, which is the only evidence that says the interlock is armed.
	// Same seam pattern the bootloader package uses for updateFirmware.
	testTransport func(ctx context.Context) (proto.Transport, string, error)

	// prompt is created lazily and reused so that a wizard asking two
	// questions does not lose input buffered by the first read.
	prompt *bufio.Reader
}

// Transport names accepted by --transport.
const (
	transportRawMIDI = "rawmidi"
	transportUSB     = "usb"
)

// Execute runs the gflex command tree and returns the process exit code.
// It never calls os.Exit itself, so that main stays a one-liner and tests can
// drive the whole tree in-process.
func Execute() int {
	app := &App{stdout: os.Stdout, stderr: os.Stderr, stdin: os.Stdin}
	root := NewRootCommand(app)

	// Ctrl-C during a firmware flash or a PDO download must unwind through the
	// normal error path so that deferred Close calls release the device.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	code := ExitCode(err)
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "gflex: interrupted")
		return code
	}
	fmt.Fprintf(os.Stderr, "gflex: %v\n", err)
	if hint := exitHint(code); hint != "" && !suppressHint(err) {
		fmt.Fprintf(os.Stderr, "\n%s\n", hint)
	}
	return code
}

// NewRootCommand builds the command tree against app. Exported so that tests
// and any future embedder can drive it with their own streams.
func NewRootCommand(app *App) *cobra.Command {
	root := &cobra.Command{
		Use:   "gflex",
		Short: "Program a Werewolf VFLEX USB-C Power Delivery voltage adapter",
		Long: "gflex programs a Werewolf VFLEX over its USB-MIDI interface.\n\n" +
			"The VFLEX is an inline USB-C Power Delivery adapter: it negotiates a voltage you\n" +
			"choose from a PD source and presents it on the X-Connector. Everything this tool\n" +
			"writes is stored in non-volatile memory and survives a power cycle.\n\n" +
			"Values are millivolts and milliamps on the wire. Where a command takes a voltage\n" +
			"or a current you may write \"12\", \"12V\", \"12000mV\" or \"9.5\"; a bare number is\n" +
			"read as volts (amps for a current).",
		Version:       versionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
		// Without a subcommand, print help rather than failing obscurely. With
		// one we do not recognise, reject it: setting root.Args below means
		// nothing else will.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return codedf(ExitUsage, "unknown command %q for %q", args[0], cmd.CommandPath())
		},
	}

	// Take the arguments ourselves, which is what makes the branch above
	// load-bearing rather than dead: cobra rejects an unknown command only in
	// legacyArgs, and Find calls that solely when Args is nil. With Args set,
	// `gflex bogus` reaches RunE, and without the branch it would print nothing
	// and exit 0 -- the same "reported success having done nothing" failure the
	// group helper at the bottom of this file exists to prevent.
	//
	// The wording is deliberately identical to legacyArgs'; what differs is
	// that this is a CodedError. legacyArgs returns a bare error that ExitCode
	// can only recognise by matching cobra's phrasing, which a release is free
	// to change (see commandLineErrorPrefixes in exit.go). Same exit code
	// today, held by construction rather than by a string.
	root.Args = cobra.ArbitraryArgs

	root.SetOut(app.stdout)
	root.SetErr(app.stderr)

	// Any flag parsing failure is a usage error, not a generic one.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return coded(ExitUsage, err)
	})

	pf := root.PersistentFlags()
	pf.StringVar(&app.Port, "port", "", "device node path or port-name substring (see `gflex devices`)")
	pf.StringVar(&app.Transport, "transport", transportRawMIDI, "link to the device: rawmidi|usb")
	pf.BoolVar(&app.AsJSON, "json", false, "emit a single JSON object on stdout; diagnostics go to stderr")
	pf.DurationVar(&app.Timeout, "timeout", proto.DefaultTimeout, "per-command response timeout")
	pf.DurationVar(&app.ByteDelay, "byte-delay", proto.ByteDelay,
		"delay between MIDI messages; the vendor app uses 20ms, whether the device needs it is unknown")
	pf.BoolVarP(&app.Verbose, "verbose", "v", false, "trace TX/RX frames as hex on stderr")
	pf.BoolVar(&app.DryRun, "dry-run", false, "print the frames and MIDI bytes that would be sent, and send nothing")
	pf.BoolVarP(&app.Yes, "yes", "y", false, "assume yes for every safety confirmation (SPEC.md §13)")

	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		return app.applyEnv(cmd)
	}

	root.AddCommand(
		newDevicesCommand(app),
		newInfoCommand(app),
		newVoltageCommand(app),
		newCurrentCommand(app),
		newVLimitCommand(app),
		newToleranceCommand(app),
		newMeasureCommand(app),
		newCalibrateCommand(app),
		newLEDCommand(app),
		newAuthLockCommand(app),
		newScanCommand(app),
		newPDOCommand(app),
		newFirmwareCommand(app),
		newRawCommand(app),
		newMonitorCommand(app),
		newInstallUdevCommand(app),
		newVersionCommand(app),
	)
	return root
}

// applyEnv fills in global flags from GFLEX_* environment variables for any
// flag the user did not set explicitly. Precedence is flag > env > default,
// matching SPEC.md §11.
func (a *App) applyEnv(cmd *cobra.Command) error {
	f := cmd.Root().PersistentFlags()
	get := func(name, env string) (string, bool) {
		if f.Changed(name) {
			return "", false
		}
		v, ok := os.LookupEnv(env)
		return v, ok && v != ""
	}
	if v, ok := get("port", "GFLEX_PORT"); ok {
		a.Port = v
	}
	if v, ok := get("transport", "GFLEX_TRANSPORT"); ok {
		a.Transport = v
	}
	if v, ok := get("timeout", "GFLEX_TIMEOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return codedf(ExitUsage, "GFLEX_TIMEOUT=%q: %v", v, err)
		}
		a.Timeout = d
	}
	if v, ok := get("byte-delay", "GFLEX_BYTE_DELAY"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return codedf(ExitUsage, "GFLEX_BYTE_DELAY=%q: %v", v, err)
		}
		a.ByteDelay = d
	}
	if v, ok := get("json", "GFLEX_JSON"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return codedf(ExitUsage, "GFLEX_JSON=%q: %v", v, err)
		}
		a.AsJSON = b
	}
	if v, ok := get("verbose", "GFLEX_VERBOSE"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return codedf(ExitUsage, "GFLEX_VERBOSE=%q: %v", v, err)
		}
		a.Verbose = b
	}
	switch a.Transport {
	case transportRawMIDI, transportUSB:
	default:
		return codedf(ExitUsage, "unknown --transport %q: want %q or %q", a.Transport, transportRawMIDI, transportUSB)
	}
	if a.Timeout <= 0 {
		return codedf(ExitUsage, "--timeout must be positive, got %s", a.Timeout)
	}
	if a.ByteDelay < 0 {
		return codedf(ExitUsage, "--byte-delay cannot be negative, got %s", a.ByteDelay)
	}
	if a.ByteDelay == 0 {
		// session.Options treats a zero ByteDelay as "unset" and substitutes the
		// vendor's 20 ms, so accepting 0 here would silently do the opposite of
		// what the user asked. Whether the device needs the delay at all is an
		// open question (SPEC.md §14.15), so this is a setting people will
		// genuinely want to drive to nothing -- point them at how.
		return codedf(ExitUsage, "--byte-delay 0 is indistinguishable from unset and would be "+
			"treated as the %s default; use a small positive value such as 1ns to pace as fast as possible",
			proto.ByteDelay)
	}
	return nil
}

// newFormatter builds the output formatter for this invocation.
func (a *App) newFormatter() Formatter {
	return newFormatter(a.AsJSON, a.stdout, a.stderr)
}

// run is the shared body of every subcommand: build a formatter, run fn, and
// flush only on success so that a failed command never emits a half-built JSON
// object on stdout.
func (a *App) run(cmd *cobra.Command, fn func(ctx context.Context, f Formatter) error) error {
	f := a.newFormatter()
	if err := fn(cmd.Context(), f); err != nil {
		return err
	}
	return f.Flush()
}

// ---------------------------------------------------------------------------
// Confirmation
// ---------------------------------------------------------------------------

// apply runs the shared interlock handling for a Decision: refuse, warn, then
// confirm. Every dangerous command funnels through here so the behaviour is
// identical across the tree.
func (a *App) apply(ctx context.Context, f Formatter, d Decision) error {
	if !d.OK() {
		return refused(d.Refused)
	}
	for _, w := range d.Warnings {
		f.Diag("warning: %s", w)
	}
	if !d.Confirm {
		return nil
	}
	return a.confirm(ctx, d.Prompt)
}

// confirm asks the user to approve an operation.
//
// Interlock 7 of SPEC.md §13: with no terminal on stdin there is nobody to ask,
// so the operation is refused rather than assumed. --yes is the only way
// through in a script.
func (a *App) confirm(ctx context.Context, question string) error {
	if a.Yes {
		return nil
	}
	if !a.stdinIsTTY() {
		return refused(fmt.Sprintf("%s\n  stdin is not a terminal, so this cannot be confirmed interactively.\n"+
			"  Pass --yes to proceed without a prompt", question))
	}
	if a.prompt == nil {
		a.prompt = bufio.NewReader(a.stdin)
	}
	// The prompt goes to stderr so that --json stdout stays clean even when a
	// command both prompts and prints.
	fmt.Fprintf(a.stderr, "%s [y/N] ", question)
	line, err := a.readLine(ctx)
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return refused("declined at the prompt")
}

// readLine reads one line from stdin, abandoning the wait if ctx is cancelled.
//
// The read itself has to happen on its own goroutine. signal.NotifyContext has
// removed the default SIGINT disposition for the whole process, and a plain
// bufio read is not interruptible -- Go retries it across EINTR -- so a Ctrl-C
// at a prompt would otherwise be absorbed completely: the context is cancelled,
// nobody is watching it, and the terminal is held indefinitely. Every safety
// confirmation in SPEC.md §13 goes through here, as does the scan wizard's
// handover, which has no timeout at all.
//
// The goroutine outlives a cancelled call, blocked on a read that will never be
// consumed. That is deliberate and bounded: cancellation means the process is
// on its way out, and the alternative (closing os.Stdin) would break any later
// prompt in the same run.
func (a *App) readLine(ctx context.Context) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := a.prompt.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case <-ctx.Done():
		fmt.Fprintln(a.stderr)
		return "", ctx.Err()
	case r := <-ch:
		if r.err != nil && r.line == "" {
			return "", refused("no answer read from stdin")
		}
		return r.line, nil
	}
}

// pause waits for the user to press Enter. Used by the scan wizard, where the
// question is not yes/no but "tell me when you have done it".
func (a *App) pause(ctx context.Context, question string) error {
	if !a.stdinIsTTY() {
		return codedf(ExitUsage, "%s\n  stdin is not a terminal; use --no-prompt (optionally with --wait) "+
			"to run this unattended", question)
	}
	if a.prompt == nil {
		a.prompt = bufio.NewReader(a.stdin)
	}
	fmt.Fprintf(a.stderr, "%s ", question)
	if _, err := a.readLine(ctx); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Dry run
// ---------------------------------------------------------------------------

// dryRunFrame is one frame a command would have sent.
type dryRunFrame struct {
	Cmd     string `json:"cmd"`
	Code    uint8  `json:"code"`
	Write   bool   `json:"write"`
	Frame   string `json:"frame"`
	MIDI    string `json:"midi"`
	Payload string `json:"payload,omitempty"`
}

// dryRun renders the frames a command would have sent and returns without
// touching the device.
//
// Interlock 8 of SPEC.md §13 requires both representations: the protocol frame
// and the MIDI byte stream it becomes. The MIDI form is what actually goes on
// the wire and is where the surprises live — every protocol byte becomes a
// Note-On carrying its high nibble as the note and its low nibble as the
// velocity, so a byte ending in 0 emits a velocity-0 Note-On (SPEC.md §3.1,
// §3.2).
func (a *App) dryRun(f Formatter, frames ...[]byte) error {
	items := make([]dryRunFrame, 0, len(frames))
	for _, fr := range frames {
		item := dryRunFrame{
			Frame: proto.Hex(fr),
			MIDI:  proto.Hex(framer.EncodeMIDI(fr)),
		}
		if parsed, err := proto.Parse(fr); err == nil {
			item.Cmd = parsed.Cmd.String()
			item.Code = uint8(parsed.Cmd)
			item.Write = parsed.Write
			item.Payload = proto.Hex(parsed.Payload)
		}
		items = append(items, item)
	}

	f.KV("dry_run", "dry run", true, "nothing was sent to the device")
	rows := make([][]string, 0, len(items)*2)
	for _, it := range items {
		name := it.Cmd
		if it.Write {
			name += " (write)"
		}
		rows = append(rows,
			[]string{name, "frame", it.Frame},
			[]string{"", "midi", it.MIDI},
		)
	}
	f.Table("frames", "would send:", items, []string{"COMMAND", "", "BYTES"}, rows)
	return nil
}

// group configures a command that exists only to hold subcommands.
//
// Cobra's default for a non-runnable command with leftover arguments is to
// print help and return nil, so `gflex voltage sett 12` exits 0 having written
// nothing. For a tool whose job is keeping a power rail inside a safe range,
// "I did nothing and reported success" is the wrong failure mode: a script
// guarded with `|| exit 1` sails past the typo believing the rail was
// reprogrammed, and then attaches a load to whatever voltage was there before.
//
// It also breaks the --json contract, because cobra writes the help text to
// stdout where the result object is supposed to be (SPEC.md §11).
func group(cmd *cobra.Command) *cobra.Command {
	// Take the args ourselves so we can phrase the error, rather than letting
	// cobra's legacyArgs silently accept them.
	cmd.Args = cobra.ArbitraryArgs
	cmd.RunE = func(c *cobra.Command, args []string) error {
		var sub []string
		for _, child := range c.Commands() {
			if !child.Hidden {
				sub = append(sub, child.Name())
			}
		}
		list := strings.Join(sub, ", ")
		if len(args) == 0 {
			return codedf(ExitUsage, "%s needs a subcommand (%s)", c.CommandPath(), list)
		}
		return codedf(ExitUsage, "unknown subcommand %q for %q (expected one of: %s)",
			args[0], c.CommandPath(), list)
	}
	return cmd
}
