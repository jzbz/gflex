package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/jzbz/gflex/internal/framer"
	"github.com/jzbz/gflex/internal/proto"
)

// monitorEvent is one traced frame, for --json output.
type monitorEvent struct {
	Time    string `json:"time"`
	Dir     string `json:"dir"`
	Cmd     string `json:"cmd"`
	Code    uint8  `json:"code"`
	Write   bool   `json:"write"`
	Frame   string `json:"frame"`
	Payload string `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
}

func newMonitorCommand(app *App) *cobra.Command {
	var duration time.Duration
	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Print decoded protocol frames as they arrive",
		Long: "monitor opens the transport and prints every frame the device sends, decoded and\n" +
			"timestamped. It sends nothing itself.\n\n" +
			"This is a bring-up tool, and it did its first job: SPEC.md §14.14 and §14.13 were\n" +
			"settled with it and nothing else -- the device sends nothing unsolicited, and it\n" +
			"clears the write and scratchpad flag bits rather than echoing them. What is left\n" +
			"is watching for frames the decoder discards, which nothing else surfaces at all\n" +
			"(SPEC.md §3.3), and confirming either answer on a second unit or firmware.\n\n" +
			"Only the receive direction is visible here, because the ALSA rawmidi node does not\n" +
			"loop back what another process writes. To see both directions of a real exchange,\n" +
			"run the command you want to watch with -v, which traces the session's own frames.\n\n" +
			"With --json this streams one object per line rather than a single document.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app.DryRun {
				f := app.newFormatter()
				f.Note("monitor sends nothing, so --dry-run has nothing to show.")
				return f.Flush()
			}
			// monitor streams rather than producing a result, so it writes
			// directly instead of going through the buffering formatter.
			return app.runMonitor(cmd.Context(), duration)
		},
	}
	cmd.Flags().DurationVar(&duration, "for", 0, "stop after this long (default: run until interrupted)")
	return cmd
}

func (a *App) runMonitor(ctx context.Context, duration time.Duration) error {
	t, desc, err := a.openTransport(ctx)
	if err != nil {
		return err
	}

	// The framer is used directly rather than through a session: a session
	// serialises command traffic through one pending slot, which is exactly
	// what we do not want when the point is to see whatever arrives.
	//
	// The framer takes ownership of the transport, so closing it is the only
	// close needed here.
	fr := framer.New(t, a.ByteDelay)

	// Frames the decoder discards -- an impossible declared length, a mid-frame
	// resync, an accumulator overflow -- never reach Frames() or Errors(). The
	// vendor client drops them silently and the pending command just times out
	// five seconds later with no diagnostic, which SPEC.md §3.3 asks us to fix.
	// Surfacing them is most of the point of this command: a malformed frame is
	// the one class of device output nothing else in the tool can see, and it is
	// what §14.13 and §14.14 were watched for on hardware.
	drops := monitorDrops(fr)
	fr.Start()
	defer fr.Close()

	if duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}

	fmt.Fprintf(a.stderr, "monitoring %s -- press Ctrl-C to stop\n", desc)
	if !a.AsJSON {
		fmt.Fprintf(a.stdout, "%-12s %-3s %-34s %s\n", "TIME", "DIR", "FRAME", "DECODED")
	}
	return a.monitorLoop(ctx, fr.Frames(), fr.Errors(), drops)
}

// monitorLoop services the three receive channels until the framer's have both
// closed, then reports the terminal transport error, if one arrived.
//
// A channel that reports !ok is nil-ed, not returned on. The framer closes
// frames and errs one after the other when its reader exits, so for a moment
// one is closed while the other still holds data; a receive from a closed
// channel is always ready, so a select that returned on the first !ok chose at
// random between "this channel is exhausted" and "the other still has the
// goods". That coin flip cost real evidence twice over: on an unplug the
// terminal ENODEV sat in errs while frames closed first, so about half of
// unplugs exited 0 printing nothing -- and in the mirror case up to 16
// buffered decoded frames were abandoned, which on this command are the whole
// record of what the device sent -- the observations that settled SPEC.md
// §14.13 and §14.14 arrived exactly this way. session.go
// documents the same pattern at its drain sites ("a closed channel is not a
// reason to stop"); this is that pattern with the drop hook serviced alongside.
//
// The terminal error is returned as well as printed: the framer stops reading
// on it, so the monitor is over whether the user wanted it or not, and a real
// transport failure must not exit 0. Context ends -- the --for deadline or
// Ctrl-C -- stay a normal nil end as before, unless an error had already been
// received by then.
func (a *App) monitorLoop(ctx context.Context, frames <-chan []byte, errs <-chan error, drops chan monitorDrop) error {
	enc := json.NewEncoder(a.stdout)
	var terminal error
	for frames != nil || errs != nil {
		select {
		case <-ctx.Done():
			a.drainMonitorDrops(enc, drops)
			return terminal
		case frame, ok := <-frames:
			if !ok {
				frames = nil
				continue
			}
			a.printMonitorFrame(enc, time.Now(), "rx", frame, nil)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			a.printMonitorFrame(enc, time.Now(), "err", nil, err)
			terminal = err
		case d := <-drops:
			// A frame the decoder refused to dispatch. See SetDropHook above.
			a.printMonitorFrame(enc, d.at, "drop", d.buffered, errors.New(d.reason))
		}
	}
	// Both framer channels are closed, so the reader has exited and no drop
	// hook can fire again: whatever sits in drops now is all there will ever
	// be. Print it rather than abandon it -- a drop notice is exactly the kind
	// of evidence this command exists to surface.
	a.drainMonitorDrops(enc, drops)
	return terminal
}

// drainMonitorDrops prints every drop notice already buffered, without
// blocking. See monitorLoop for when this is complete versus best-effort.
func (a *App) drainMonitorDrops(enc *json.Encoder, drops chan monitorDrop) {
	for {
		select {
		case d := <-drops:
			a.printMonitorFrame(enc, d.at, "drop", d.buffered, errors.New(d.reason))
		default:
			return
		}
	}
}

func (a *App) printMonitorFrame(enc *json.Encoder, at time.Time, dir string, frame []byte, ferr error) {
	ev := monitorEvent{Time: at.Format(time.RFC3339Nano), Dir: dir, Frame: proto.Hex(frame)}
	decoded := ""
	if ferr != nil {
		ev.Error = ferr.Error()
		decoded = ferr.Error()
	} else if parsed, err := proto.Parse(frame); err == nil {
		ev.Cmd = parsed.Cmd.String()
		ev.Code = uint8(parsed.Cmd)
		ev.Write = parsed.Write
		ev.Payload = proto.Hex(parsed.Payload)
		decoded = describeFrame(parsed)
		if len(parsed.Payload) > 0 {
			decoded += "  " + payloadDisplay(parsed.Payload)
		}
	}
	if a.AsJSON {
		_ = enc.Encode(ev) // a closed stdout is not worth aborting a monitor for
		return
	}
	fmt.Fprintf(a.stdout, "%-12s %-3s %-34s %s\n", at.Format("15:04:05.000"), dir, proto.Hex(frame), decoded)
}

// monitorDrops installs the drop hook on fr and returns the channel it feeds.
//
// It exists as a named function rather than as five lines inside runMonitor
// because it is the whole of a SPEC.md §17 deliberate divergence -- the vendor
// drops malformed frames silently, this reports them -- and §17 asks that each
// of those carry a regression test. runMonitor's own transport cannot be faked,
// but this can: TestMonitorDropHookIsWiredToTheFramer drives a real Framer over
// a malformed stream and waits on the returned channel, so deleting the
// SetDropHook call, or the framer's delegation to the decoder, fails a test
// instead of quietly restoring the vendor behaviour.
//
// Call it before fr.Start(). The hook runs on the framer's reader goroutine and
// the decoder's hook field is not lock-guarded, so installing it after Start
// both races that reader and misses every drop until it lands. The hook must
// not block either, which is why the channel is buffered and the send is
// non-blocking.
func monitorDrops(fr *framer.Framer) chan monitorDrop {
	drops := make(chan monitorDrop, 32)
	fr.SetDropHook(func(reason string, buffered []byte) {
		select {
		case drops <- monitorDrop{at: time.Now(), reason: reason, buffered: buffered}:
		default: // reader is behind; a lost drop notice must not stall decoding
		}
	})
	return drops
}

// monitorDrop is one frame the decoder discarded, carried from the framer's
// reader goroutine to the monitor loop.
type monitorDrop struct {
	at       time.Time
	reason   string
	buffered []byte
}
