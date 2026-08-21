package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jzbz/gflex/internal/proto"
)

func newRawCommand(app *App) *cobra.Command {
	var (
		noACK  bool
		anyLen bool
	)
	cmd := &cobra.Command{
		Use:   "raw <hex...>",
		Short: "Send a frame verbatim and print the response",
		Long: "raw sends the bytes you give it as a protocol frame. The bytes are the whole\n" +
			"frame including the two-byte preamble:\n\n" +
			"  gflex raw 02 08          read the serial number\n" +
			"  gflex raw 0x04 0x92 0x2E 0xE0    write 12000 mV\n\n" +
			"Separators are ignored, so \"0208\", \"02 08\" and \"02:08\" are the same frame.\n\n" +
			"This is the escape hatch for the parts of the protocol nobody has characterised:\n" +
			"the four reserved codes, the two LED commands, the encrypt challenge and the host\n" +
			"mode flag (SPEC.md §14.5-§14.7). It also reaches the scratchpad flag, which the\n" +
			"vendor app never sets and which one unit measured as validate-and-discard: the\n" +
			"write is acknowledged and echoed back but never committed, so it looks like a\n" +
			"success and stores nothing (SPEC.md §14.4). Anything beyond a plain documented\n" +
			"read is confirmed first.\n\n" +
			"There is no error response anywhere in this protocol: a frame the device does not\n" +
			"like produces silence, which arrives as a timeout.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				raw, err := parseHexBytes(args)
				if err != nil {
					return err
				}
				if len(raw) < proto.PreambleLen {
					return codedf(ExitUsage, "a frame needs at least %d bytes ([length, command]), got %d",
						proto.PreambleLen, len(raw))
				}
				// Parsed before the length gate so the gate can read the
				// declared length off the frame rather than re-deriving it
				// from raw[0]. Parse's only failure is the short-frame case,
				// already refused above, so nothing is lost by moving it up.
				parsed, err := proto.Parse(raw)
				if err != nil {
					return codedf(ExitUsage, "cannot parse the frame: %v", err)
				}

				// Frame.DeclaredValid is deliberately NOT the test here. It
				// admits a declared length shorter than the buffer, which is a
				// legitimate thing to RECEIVE -- a well-formed frame followed
				// by padding (SPEC.md §5.2). What goes out has no padding, so
				// byte[0] must equal the whole frame, which is the tighter
				// question DeclaredLen answers directly.
				if parsed.DeclaredLen != len(raw) {
					if !anyLen {
						return codedf(ExitUsage,
							"the length byte is 0x%02x (%d) but you gave %d bytes.\n"+
								"  byte[0] must equal the total frame length. Pass --any-length --no-ack to send\n"+
								"  it anyway (the device's receive path drops frames whose declared length does\n"+
								"  not fit, so nothing would come back).",
							raw[0], raw[0], len(raw))
					}
					// A response is matched by command code, and a frame the
					// device drops produces no response at all -- so a
					// deliberately malformed length can only be sent
					// fire-and-forget. Round-tripping it through DoRaw would
					// silently rebuild a well-formed length byte.
					if !noACK {
						return codedf(ExitUsage,
							"--any-length needs --no-ack: a frame with a mismatched length byte is dropped by\n"+
								"  the receiver, so waiting for a response would only time out")
					}
				}

				if app.DryRun {
					if err := app.applyDryRun(f, CheckRawFrame(parsed)); err != nil {
						return err
					}
					return app.dryRun(f, raw)
				}
				if err := app.apply(ctx, f, CheckRawFrame(parsed)); err != nil {
					return err
				}

				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()

				f.KV("sent", "sent", proto.Hex(raw), fmt.Sprintf("%s   %s", proto.Hex(raw), describeFrame(parsed)))

				if noACK {
					// The one command that genuinely has no response is the
					// jump to the bootloader: the device disconnects instead
					// (SPEC.md §6.1).
					if err := c.Session.SendNoACK(ctx, raw); err != nil {
						return fmt.Errorf("sending the frame: %w", err)
					}
					f.KV("acked", "response", false, "not awaited (--no-ack)")
					return nil
				}

				resp, err := c.Session.DoRaw(ctx, parsed.Cmd, parsed.Payload, parsed.Write, parsed.Scratchpad)
				if err != nil {
					return fmt.Errorf("sending %s: %w", parsed.Cmd, err)
				}
				emitRawResponse(f, resp)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&noACK, "no-ack", false, "send and return immediately without waiting for a response")
	cmd.Flags().BoolVar(&anyLen, "any-length", false, "do not require byte[0] to equal the frame length")
	return cmd
}

// emitRawResponse renders a response frame both raw and decoded.
func emitRawResponse(f Formatter, resp proto.Frame) {
	f.KV("cmd", "command", resp.Cmd.String(), fmt.Sprintf("%s (%d)", resp.Cmd, uint8(resp.Cmd)))
	f.KV("write", "write flag", resp.Write, fmt.Sprintf("%t", resp.Write))
	f.KV("scratchpad", "scratchpad flag", resp.Scratchpad, fmt.Sprintf("%t", resp.Scratchpad))
	f.KV("payload", "payload", proto.Hex(resp.Payload), payloadDisplay(resp.Payload))
	f.KV("payload_len", "payload length", len(resp.Payload), fmt.Sprintf("%d", len(resp.Payload)))
	// The one unit measured CLEARS both flag bits on the way back: `tx 04 92 13
	// 88` came home as `rx 04 12 13 88` (SPEC.md §14.13), which is why masking
	// the received command byte is required rather than merely defensive. A set
	// bit here therefore contradicts that measurement -- a different unit, a
	// different firmware -- and is worth saying out loud. The vendor client
	// masks before any comparison and so could never have seen it either way.
	if resp.Write || resp.Scratchpad {
		f.Note("")
		f.Note("The device echoed a flag bit in the command byte. The one unit measured cleared")
		f.Note("both bits, so this contradicts SPEC.md §14.13 and is worth recording.")
	}
}

func payloadDisplay(p []byte) string {
	if len(p) == 0 {
		return "(empty)"
	}
	out := proto.Hex(p)
	if s := proto.DecodeString(p); s != "" && len(s) >= len(p)/2 {
		out += fmt.Sprintf("   %q", s)
	}
	if len(p) >= 2 {
		if v, err := proto.DecodeU16(p); err == nil {
			out += fmt.Sprintf("   u16=%d", v)
		}
	}
	if len(p) >= 4 {
		if v, err := proto.DecodeI32(p); err == nil {
			out += fmt.Sprintf("   i32=%d", v)
		}
	}
	return out
}

// describeFrame is the one-line human summary of an outbound frame.
func describeFrame(fr proto.Frame) string {
	s := fr.Cmd.String()
	switch {
	case fr.Write:
		s += " write"
	case len(fr.Payload) == 0:
		s += " read"
	}
	if fr.Scratchpad {
		s += " +scratchpad"
	}
	if fr.Cmd.Undocumented() {
		s += "  [undocumented]"
	}
	return s
}
