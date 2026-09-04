package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jzbz/gflex/internal/pdo"
	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/session"
)

// pdoChunks is the number of 8-byte chunks the vendor client requests to cover
// the 90-byte log (SPEC.md §9.1). Twelve chunks yield 96 bytes; the first 90
// are the log.
//
// Defined in terms of the download rather than restated as 12, because this
// name drives the --dry-run listing and the progress denominator while
// session.FullPDOLog drives what the device is actually asked. Interlock 8 of
// SPEC.md §13 requires the listing to be the exact frames the command sends, and
// two independent 12s cannot hold that: `info` had precisely this drift, which
// is what TestInfoDryRunMatchesWhatInfoSends exists to prevent. Here the
// equality is held by construction instead.
const pdoChunks = session.PDOChunkCount

// requirePDOFirmware applies the hard gate SPEC.md §9 puts on the capture log:
// it exists only on firmware 5.0.0 and newer, and the vendor app refuses
// outright below that rather than trying and failing.
//
// Every command that touches the log goes through here -- `scan`, `pdo dump`
// and `pdo clear` alike. Only `scan` used to check, so on a 4.x unit the other
// two reached the device and then failed the way an absent command always fails
// in this protocol: a bare timeout, because there is no NACK (SPEC.md §5.2).
// "The device did not answer" is a diagnosis that sends the user looking at
// cables for a fault that is a firmware version.
//
// The refusal reproduces the vendor's own wording verbatim, scan-flavoured
// phrasing and all, because SPEC.md §9.6 lists it among the strings users will
// paste into a search box. The version this unit reported is appended so the
// message is actionable rather than merely correct.
func requirePDOFirmware(ctx context.Context, s *session.Session) (string, error) {
	ok, version, err := s.FirmwareAtLeast(ctx, 5, 0, 0)
	if err != nil {
		return "", fmt.Errorf("reading the firmware version: %w", err)
	}
	if !ok {
		return version, codedf(ExitRefused, "%s\n  this unit reports %q", msgFirmwareTooOld, version)
	}
	return version, nil
}

func newPDOCommand(app *App) *cobra.Command {
	cmd := group(&cobra.Command{
		Use:   "pdo",
		Short: "Read or erase the captured PD capability log",
		Long: "The VFLEX captures a 90-byte log of the Power Delivery capabilities advertised by\n" +
			"whatever source it was last attached to. `gflex scan` drives the whole capture;\n" +
			"these subcommands are the two halves of it.\n\n" +
			"The log blob is the only little-endian data in the entire protocol (SPEC.md §9.3).",
	})

	var raw bool
	dump := &cobra.Command{
		Use:   "dump",
		Short: "Download and decode the PD capability log",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				if app.DryRun {
					return app.dryRun(f, pdoDumpFrames()...)
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				if _, err := requirePDOFirmware(ctx, c.Session); err != nil {
					return err
				}
				blob, err := app.downloadPDOLog(ctx, c)
				if err != nil {
					return err
				}
				log, err := pdo.Parse(blob)
				if err != nil {
					return fmt.Errorf("decoding the PDO log: %w", err)
				}
				emitPDOLog(f, log, blob, raw, true)
				return nil
			})
		},
	}
	dump.Flags().BoolVar(&raw, "raw", false, "also emit the undecoded 90-byte blob as hex")
	cmd.AddCommand(dump)

	// `clear` erases non-volatile state that nothing can hand back, and it does
	// so with no confirmation. That is deliberate, and it was looked at again
	// rather than assumed.
	//
	// It is not a SPEC.md §13 interlock. That list is ten items and it is
	// closed, and every one of them is scoped to a value that can damage a load
	// or lock the owner out of their own device: the rail, the window that
	// bounds it, the calibration the window's evidence comes from, the auth
	// lock, a flash, and the raw escape hatch. Writing NVM is not by itself the
	// test -- `tolerance set`, `led set` and `led color` all write it and none
	// of them prompts, under the policy warnUnacknowledged states in
	// settings.go. A capture log is a measurement of somebody else's charger,
	// and the Note printed straight after this says how to take it again.
	//
	// What settles it is that `scan` already erases the same log on the way
	// past (scan.go), and `scan --no-prompt` does it unattended and silently --
	// the bigger, longer, more surprising erase of the two. Gating the small
	// deliberate one alone would refuse `pdo clear` on a non-TTY without --yes,
	// breaking exactly the scripted use the erase exists to serve, while the
	// scripted wizard went on erasing without a word. An interlock that stops
	// the careful spelling of an operation and waves through the automatic one
	// is not a safety property; it is a lottery. If a confirmation is ever
	// wanted here, it belongs on both, and on the same day.
	cmd.AddCommand(&cobra.Command{
		Use:   "clear",
		Short: "Erase the captured PD capability log",
		Long: "clear erases the log so the next attachment to a PD source captures a fresh one.\n" +
			"This is step 2 of the scan workflow; `gflex scan` does it for you.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				if app.DryRun {
					frames, err := pdoClearFrames()
					if err != nil {
						return err
					}
					return app.dryRun(f, frames...)
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				if _, err := requirePDOFirmware(ctx, c.Session); err != nil {
					return err
				}
				if err := c.Session.ClearPDOLog(ctx); err != nil {
					return fmt.Errorf("erasing the PDO log: %w", err)
				}
				f.KV("pdo_log_cleared", "pdo log", true, "erased")
				f.Note("")
				f.Note("Now unplug the VFLEX, attach it to the PD source you want to characterise for")
				f.Note("about 5 seconds (until the LED settles green or red), then run `gflex pdo dump`.")
				return nil
			})
		},
	})
	return cmd
}

// pdoReadFrames lists the twelve chunk reads a log download sends.
func pdoReadFrames() [][]byte {
	frames := make([][]byte, 0, pdoChunks)
	for i := 0; i < pdoChunks; i++ {
		fr, err := proto.Build(proto.CmdPDOLog, []byte{uint8(i)}, false, false)
		if err != nil {
			continue // unreachable: a one-byte payload always fits
		}
		frames = append(frames, fr)
	}
	return frames
}

// pdoEraseFrame is the log erase, 02 91: a write with an empty payload
// (SPEC.md §9.1).
func pdoEraseFrame() ([]byte, error) {
	return proto.Build(proto.CmdPDOLog, nil, true, false)
}

// pdoDumpFrames and pdoClearFrames are the --dry-run listings for the two
// subcommands, and they lead with the firmware read because requirePDOFirmware
// really does issue it first. Interlock 8 of SPEC.md §13 wants the exact
// frames, and the gate is a frame like any other.
func pdoDumpFrames() [][]byte {
	return append([][]byte{proto.Read(proto.CmdFirmwareVersion)}, pdoReadFrames()...)
}

func pdoClearFrames() ([][]byte, error) {
	erase, err := pdoEraseFrame()
	if err != nil {
		return nil, err
	}
	return [][]byte{proto.Read(proto.CmdFirmwareVersion), erase}, nil
}

// downloadPDOLog pulls the log with a progress indicator on stderr.
func (a *App) downloadPDOLog(ctx context.Context, c *conn) ([]byte, error) {
	progress := func(chunk, bytes int) {
		// Progress belongs on stderr in both modes: in JSON mode stdout must
		// carry nothing but the result object.
		fmt.Fprintf(a.stderr, "\rdownloading PDO log: chunk %d/%d, %d bytes", chunk+1, pdoChunks, bytes)
	}
	if a.AsJSON || !isTerminal(fdOf(a.stderr)) {
		progress = nil // no point animating into a pipe
	}
	blob, err := c.Session.FullPDOLog(ctx, progress)
	if progress != nil {
		fmt.Fprintln(a.stderr)
	}
	if err != nil {
		return nil, fmt.Errorf("downloading the PDO log: %w\n"+
			"  If the log is empty: unplug the VFLEX, plug it into a USB-C PD charger for ~10s,\n"+
			"  then reconnect and retry -- or use `gflex scan`, which walks through that", err)
	}
	return blob, nil
}

// fdOf recovers the file descriptor behind a writer, or an invalid one when the
// writer is not a file (a buffer in tests, a pipe).
func fdOf(w any) uintptr {
	type fder interface{ Fd() uintptr }
	if f, ok := w.(fder); ok {
		return f.Fd()
	}
	return ^uintptr(0)
}

// pdoDumpJSON embeds the decoded log so its field names appear verbatim at the
// top level, with the undecoded blob alongside when --raw was given.
type pdoDumpJSON struct {
	*pdo.Log
	Raw string `json:"raw,omitempty"`
}

// emitPDOLog renders a decoded PD capability log.
//
// setDoc controls whether the log becomes the JSON document. `pdo dump` wants
// that; `scan` wraps the log in a richer result and sets its own.
func emitPDOLog(f Formatter, log *pdo.Log, blob []byte, withRaw, setDoc bool) {
	if setDoc {
		doc := pdoDumpJSON{Log: log}
		if withRaw {
			doc.Raw = proto.Hex(blob)
		}
		f.Document(doc)
	}

	f.KV("target_voltage_mv", "target voltage", log.TargetVoltageMv, formatMv(log.TargetVoltageMv))
	f.KV("measured_voltage_mv", "measured voltage", log.MeasuredVoltageMv, formatMv(log.MeasuredVoltageMv))
	f.KV("n_pdos_received", "pdos received", log.NPDOsReceived, fmt.Sprintf("%d", log.NPDOsReceived))
	f.KV("selected_pdo_id", "selected pdo", log.SelectedPDOID, fmt.Sprintf("%d", log.SelectedPDOID))
	f.KV("flags", "flags", log.Flags, fmt.Sprintf("0x%04x", log.Flags))
	f.KV("flags2", "flags2", log.Flags2, fmt.Sprintf("0x%04x", log.Flags2))
	f.KV("epr_cable_fail", "epr cable fail", log.EPRCableFail, eprCableText(log.EPRCableFail))
	emitNegotiation(f, log)

	rows := make([][]string, 0, len(log.PDOs))
	for _, p := range log.PDOs {
		rows = append(rows, []string{
			fmt.Sprintf("%d", p.Index),
			p.Kind.String(),
			pdoRange(p),
			pdoCurrent(p),
			sprEPR(p.EPR),
			validMark(p.Valid),
			fmt.Sprintf("0x%08x", p.Raw),
		})
	}
	f.Table("pdos", "advertised capabilities", log.PDOs,
		[]string{"#", "TYPE", "VOLTAGE", "CURRENT/POWER", "RANGE", "VALID", "RAW"}, rows)

	// The "?" on an SPR AVS voltage range is the only figure in this table that
	// did not come off the wire, and a bare question mark is not a disclosure.
	// Spell it out, so nothing above can be read as scanned data when it is
	// USB-PD 3.2 speaking (SPEC.md §9.4). The wording comes from the pdo package
	// rather than being restated here: a mark whose meaning is spelled out
	// differently in two places is two claims, not one, and the same clause is
	// what pdo.Evaluate puts on a verdict that rests on the assumption.
	//
	// After the table on purpose, so it cannot widen a column.
	if hasSPRAVS(log) {
		f.Note("")
		f.Note("? the %s - %s V SPR AVS range is assumed, not scanned: %s",
			trimFloat(pdo.SPRAVSMinVoltageV, 1), trimFloat(pdo.SPRAVSMaxVoltageV, 1),
			pdo.SPRAVSAssumptionClause)
	}

	if withRaw {
		f.Note("")
		f.Note("raw blob (little-endian): %s", proto.Hex(blob))
	}
}

// emitNegotiation names the flag bits that are set, for the human path only.
//
// The two words are printed above as hex because they are the evidence; this is
// the reading of them, and it is the answer to "the charger advertises 28 V, so
// why did the scan not get it" -- a rejected request, an EPR entry the source
// refused, a cable that could not carry EPR. SPEC.md §9.3 asked for exactly
// this: the flags were parsed and discarded, and they are free diagnostics.
//
// One line, however long, rather than a wrapped block or a truncated list: the
// same call this file's table makes, for the same reason (see writeTable).
//
// It goes out as a KV with an empty key, which both formatters already treat as
// "human only" -- the JSON one skips an empty key outright. That is the right
// shape here rather than a Note: a Note would break the run of aligned KV rows
// above and land unindented in the middle of them, and JSON callers want
// log.Status, where every bit is named whether it is set or not, not a
// pre-joined sentence.
//
// An empty list is still printed. A log that parsed but negotiated nothing is a
// real state, and silence would read as "no flags section" rather than as the
// finding it is.
func emitNegotiation(f Formatter, log *pdo.Log) {
	labels := append(pdo.FlagLabels(log.Flags), pdo.Flag2Labels(log.Flags2)...)
	if len(labels) == 0 {
		f.KV("", "negotiation", nil, "nothing flagged -- neither word has a bit set")
		return
	}
	f.KV("", "negotiation", nil, strings.Join(labels, ", "))
}

// hasSPRAVS reports whether any decoded object is an SPR AVS APDO, which is the
// condition on the assumed-range disclosure above. Invalid objects count: the
// table prints them too, question mark and all.
func hasSPRAVS(log *pdo.Log) bool {
	for _, p := range log.PDOs {
		if p.Kind == pdo.KindSPRAVS {
			return true
		}
	}
	return false
}

func eprCableText(fail bool) string {
	if !fail {
		return "no"
	}
	return "YES -- an EPR-rated eMarker cable is missing or the source is not EPR-capable.\n" +
		"                    On the device this shows as a fast-blinking red LED (SPEC.md §1.1)."
}

// pdoRange renders a PDO's voltage as a point or a range, in the USB-PD units
// the decode produces.
func pdoRange(p pdo.PDO) string {
	switch p.Kind {
	case pdo.KindFixed:
		return fmt.Sprintf("%s V", trimFloat(p.VoltageV, 2))
	case pdo.KindPPS, pdo.KindEPRAVS:
		return fmt.Sprintf("%s - %s V", trimFloat(p.MinVoltageV, 2), trimFloat(p.MaxVoltageV, 2))
	case pdo.KindSPRAVS:
		// An SPR AVS APDO carries NO voltage range on the wire — only two
		// band-specific current limits (SPEC.md §9.4). The range below is the
		// USB-PD 3.2 assumption, and the trailing "?" marks it as such. This is
		// the only renderer of the capability table, so the convention is
		// established here; emitPDOLog above spells the mark out in a footnote,
		// because the mark alone is a hint and not a disclosure. The previous
		// "15 V / 20 V" here was neither: those are
		// the boundaries of the two CURRENT bands, printed where a voltage range
		// belongs, which both understated the low end (the assumed floor is 9 V)
		// and presented an assumption as if it had been scanned.
		return fmt.Sprintf("%s - %s V?", trimFloat(pdo.SPRAVSMinVoltageV, 1), trimFloat(pdo.SPRAVSMaxVoltageV, 1))
	case pdo.KindBattery, pdo.KindVariable:
		if p.MaxVoltageV > 0 || p.MinVoltageV > 0 {
			return fmt.Sprintf("%s - %s V", trimFloat(p.MinVoltageV, 2), trimFloat(p.MaxVoltageV, 2))
		}
	}
	return ""
}

// pdoCurrent renders the current or power term appropriate to the PDO type.
//
// The current fields have already been bounded by pdo.MaxCableCurrentA, so this
// shows what a load may actually draw rather than what the source claims;
// pdoDeclaredNote appends the claim wherever the two differ, so the bound is
// visible instead of silent.
func pdoCurrent(p pdo.PDO) string {
	switch p.Kind {
	case pdo.KindBattery:
		// Battery PDOs carry a power budget, not a current. The vendor app
		// discards this class entirely; decoding it is a deliberate improvement
		// (SPEC.md §9.4), which is wasted if the table then renders it blank.
		return fmt.Sprintf("%s W", trimFloat(p.MaxPowerW, 2))
	case pdo.KindEPRAVS:
		return fmt.Sprintf("%d W", p.PDPWatts)
	case pdo.KindSPRAVS:
		// Both bands are shown because the applicable limit depends on the
		// output voltage: bits 19:10 bound 15-20 V and bits 9:0 bound 9-15 V,
		// matching USB-PD 3.2. A verdict uses the band-specific figure via
		// PDO.CurrentAt; reporting the larger at every voltage would over-state
		// what the source can deliver.
		return fmt.Sprintf("%s A @15V / %s A @20V%s",
			trimFloat(p.MaxCurrent15VA, 2), trimFloat(p.MaxCurrent20VA, 2), pdoDeclaredNote(p))
	case pdo.KindPPS:
		// A PPS APDO that sets the Power Limited bit cannot hold its Maximum
		// Current across its range: at Vout the source supplies
		// min(maxI, PDP/Vout), which PDO.CurrentAt applies and a verdict
		// therefore reports. The table would otherwise show the advertised
		// figure unqualified and disagree with the verdict beneath it -- and it
		// is the advertised figure that is optimistic, so the disagreement
		// would read the wrong way round. The budget is inferred from the
		// source's fixed PDOs rather than scanned (SPEC.md §9.4), so the note
		// says which case it is.
		cur := fmt.Sprintf("%s A%s", trimFloat(p.MaxCurrentA, 2), pdoDeclaredNote(p))
		switch {
		case !p.PPSPowerLimited:
			return cur
		case p.PPSBudgetW > 0:
			return fmt.Sprintf("%s [power limited; source budget ~%d W]", cur, p.PPSBudgetW)
		default:
			return cur + " [power limited; source budget unknown]"
		}
	case pdo.KindUnknown:
		return "-"
	default:
		if p.MaxCurrentA > 0 {
			return fmt.Sprintf("%s A%s", trimFloat(p.MaxCurrentA, 2), pdoDeclaredNote(p))
		}
	}
	return ""
}

// pdoDeclaredNote marks a current the cable ceiling reduced, so a source that
// looks under-powered in the table is explained rather than merely quiet.
func pdoDeclaredNote(p pdo.PDO) string {
	if !p.CableBound() {
		return ""
	}
	return fmt.Sprintf("  [source declares %s A; no cable carries over %s A]",
		trimFloat(p.DeclaredMaxCurrentA, 2), trimFloat(pdo.MaxCableCurrentA, 2))
}

func sprEPR(epr bool) string {
	if epr {
		return "EPR"
	}
	return "SPR"
}

func validMark(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}
