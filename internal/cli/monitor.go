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
			"This is a bring-up tool. Two of the open questions in SPEC.md §14 are answerable\n" +
			"with it and nothing else: whether the device ever sends unsolicited frames (§14.14)\n" +
			"and whether it echoes the write and scratchpad flag bits, which the vendor client\n" +
			"masks off before it can observe them (§14.13).\n\n" +
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
	// Surfacing them is most of the point of this command: they are the
	// evidence that would settle SPEC.md §14.13 and §14.14 on real hardware.
	//
	// The hook runs on the framer's reader goroutine, so it must be installed
	// before Start and must not block. drops is buffered and the send is
	// non-blocking for exactly that reason.
	drops := make(chan monitorDrop, 32)
	fr.SetDropHook(func(reason string, buffered []byte) {
		select {
		case drops <- monitorDrop{at: time.Now(), reason: reason, buffered: buffered}:
		default: // reader is behind; a lost drop notice must not stall decoding
		}
	})
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
	enc := json.NewEncoder(a.stdout)

	for {
		select {
		case <-ctx.Done():
			// A --for deadline is a normal end, not a failure.
			if duration > 0 && ctx.Err() == context.DeadlineExceeded {
				return nil
			}
			return nil
		case frame, ok := <-fr.Frames():
			if !ok {
				return nil
			}
			a.printMonitorFrame(enc, time.Now(), "rx", frame, nil)
		case err, ok := <-fr.Errors():
			if !ok {
				return nil
			}
			a.printMonitorFrame(enc, time.Now(), "err", nil, err)
		case d := <-drops:
			// A frame the decoder refused to dispatch. See SetDropHook above.
			a.printMonitorFrame(enc, d.at, "drop", d.buffered, errors.New(d.reason))
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

// monitorDrop is one frame the decoder discarded, carried from the framer's
// reader goroutine to the monitor loop.
type monitorDrop struct {
	at       time.Time
	reason   string
	buffered []byte
}
