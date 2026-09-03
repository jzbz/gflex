package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jzbz/gflex/internal/proto"
)

func newInfoCommand(app *App) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "info",
		Short: "Read identity and settings from the device",
		Long: "info reads everything the vendor application reads: serial number, firmware\n" +
			"version, output voltage, current limit, user voltage limits and the LED setting.\n\n" +
			"--all additionally issues the commands the vendor app never sends: chip UUID,\n" +
			"hardware ID, manufacturing date, the auth lock, both tolerance terms, the ADC\n" +
			"calibration pair and a measurement. Every one of them answered on the single unit\n" +
			"bring-up reached (SPEC.md §14), which is one unit and not a guarantee, so failures\n" +
			"are still tolerated and simply leave the field out.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				if app.DryRun {
					return app.dryRun(f, infoReadFrames(all)...)
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()

				info, err := c.Session.Info(ctx, all)
				if err != nil {
					return fmt.Errorf("reading device info: %w", err)
				}
				emitDeviceInfo(f, info, all)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "also issue the commands the vendor app never sends (best-effort)")
	return cmd
}

// infoReadCmds is the command sequence `info` issues, in the order
// session.Info issues it. It is the single source for both the --dry-run
// listing and the test that holds it to session.Info's actual behaviour.
//
// Interlock 8 of SPEC.md §13 promises --dry-run shows the exact frames a
// command would send, and a safety property that is only mostly true is worse
// than one documented as approximate: this list previously advertised
// CMD_VTOLERANCE_NOMINAL_MV on a plain `info`, which session.Info reads only
// under --all, and ordered the --all commands differently from the order they
// actually go out in. The real list lives in session.Info and cannot be
// exported from there, so TestInfoDryRunMatchesWhatInfoSends drives a fake
// device through session.Info and compares the frames it received against this
// list. Changing one without the other now fails the build's tests rather than
// silently making the promise false again.
//
// One approximation remains and is inherent: CMD_VOLTAGE_MV is read with
// session.VoltageMv's ready-retry, so a device that answers 0 mV sends that
// frame more than once. The vendor's fixed ladder -- three attempts 300 ms
// apart, then an outer backoff chain -- is deliberately not what this does
// (SPEC.md §17): the first read goes out immediately and only a not-ready
// answer or a failed read starts an escalating backoff, 100 ms doubling to a
// 1 s cap, against session.DefaultReadyTimeout (10 s). That is a dozen or more
// frames on a unit that never settles, not three. The listing shows the
// healthy-device case, which is the one attempt every other command makes.
func infoReadCmds(all bool) []proto.Cmd {
	cmds := []proto.Cmd{
		proto.CmdSerialNumber,
		proto.CmdFirmwareVersion,
		proto.CmdVoltageMv,
		proto.CmdCurrentLimitMa,
		proto.CmdUserVLimit,
		proto.CmdDisableLEDDuringOp,
	}
	if !all {
		return cmds
	}
	return append(cmds,
		proto.CmdChipUUID,
		proto.CmdHardwareID,
		proto.CmdMfgDate,
		proto.CmdAuthLock,
		proto.CmdVToleranceNominalMv,
		proto.CmdVToleranceSagPerMa,
		proto.CmdVMeasureADCOffset,
		proto.CmdVMeasureADCScale,
		proto.CmdVMeasure,
	)
}

// infoReadFrames renders infoReadCmds as the read frames --dry-run prints.
func infoReadFrames(all bool) [][]byte {
	cmds := infoReadCmds(all)
	frames := make([][]byte, 0, len(cmds))
	for _, c := range cmds {
		frames = append(frames, proto.Read(c))
	}
	return frames
}

// emitDeviceInfo renders a DeviceInfo through the formatter.
//
// JSON output is the DeviceInfo struct itself, so the field names come straight
// from the shared model and cannot drift from SPEC.md §8. The human rendering
// is built separately from the same values, in a deliberate order.
func emitDeviceInfo(f Formatter, info *proto.DeviceInfo, all bool) {
	f.Document(info)

	kvString := func(key, label, v string) {
		if v != "" {
			f.KV(key, label, v, v)
		}
	}
	kvString("serial_num", "serial", info.SerialNum)
	kvString("fw_id", "firmware", info.FirmwareID)
	kvString("uuid", "chip uuid", info.UUID)
	kvString("hw_id", "hardware id", info.HardwareID)
	kvString("mfg_date", "mfg date", info.MfgDate)

	if v := info.VoltageMv; v != nil {
		f.KV("voltage_mv", "output voltage", *v, formatMv(*v))
	}
	if v := info.CurrentLimitMa; v != nil {
		// Never present this as a measurement: the hardware has no current
		// sensing at all, this is the value requested during PD negotiation
		// (SPEC.md §6.5).
		f.KV("current_limit_ma", "current limit", *v, formatMa(*v)+"  (negotiation request, not a measurement)")
	}
	if lo, hi := info.VLimitLowMv, info.VLimitHighMv; lo != nil && hi != nil {
		f.KV("vlimit_low_mv", "voltage limits", *lo, vlimitDisplay(*lo, *hi))
		f.KV("vlimit_high_mv", "", *hi, "")
	}
	if v := info.LEDAlwaysOn; v != nil {
		f.KV("led_always_on", "led always on", *v, onOff(*v))
	}
	if v := info.VToleranceNominalMv; v != nil {
		f.KV("vtolerance_nominal_mv", "tolerance (nominal)", *v, fmt.Sprintf("%d mV", *v))
	}
	if v := info.VToleranceSagPerMa; v != nil {
		// Units unknown; a literal mV-per-mA reading is dimensionally
		// implausible at integer resolution (SPEC.md §6.5, §14.9).
		f.KV("vtolerance_sag_per_ma", "tolerance (sag)", *v, fmt.Sprintf("%d  (raw; units unknown)", *v))
	}
	if v := info.VMeasureADCOffset; v != nil {
		f.KV("vmeasure_adc_offset", "adc offset", *v, fmt.Sprintf("%d", *v))
	}
	if v := info.VMeasureADCScale; v != nil {
		f.KV("vmeasure_adc_scale", "adc scale", *v, fmt.Sprintf("%d", *v))
	}
	if v := info.VMeasureRawADC; v != nil {
		f.KV("vmeasure_raw_adc", "measured (raw adc)", *v, fmt.Sprintf("%d counts", *v))
	}
	if v := info.VMeasureCalibratedMv; v != nil {
		f.KV("vmeasure_calibrated_mv", "measured voltage", *v, formatMv(*v))
	}
	if v := info.AuthLockLevel; v != nil {
		display := fmt.Sprintf("%d", *v)
		if len(info.AuthLockRaw) > 0 {
			// The layout is settled: a two-byte payload of [0x16, level], the
			// command code echoed a second time and then the level, so reading
			// payload[1] was never an off-by-one (SPEC.md §14.8). The raw bytes
			// stay on the line because levels above 0 are still untested and a
			// unit that answers differently should be visible, not silently
			// reduced to one number.
			display += fmt.Sprintf("   (raw payload %s; [code, level] per SPEC.md §14.8)",
				proto.Hex(info.AuthLockRaw))
		}
		f.KV("authlock_level", "auth lock", *v, display)
		if len(info.AuthLockRaw) > 0 {
			f.KV("authlock_raw", "", info.AuthLockRaw, "")
		}
	}

	if all {
		// The vendor-read set is named rather than pointed at. "Fields above the
		// LED setting" was false for three of the fields it covered: chip uuid,
		// hardware id and mfg date are commands 9, 10 and 12, which the vendor
		// app never issues (SPEC.md §6.4) and which infoReadCmds appends only
		// under --all -- yet they print with the identity strings at the top,
		// above the LED row. A list cannot drift out of step with the display
		// order the way a position can.
		f.Note("")
		f.Note("serial, firmware, output voltage, current limit, voltage limits and the LED setting")
		f.Note("are read by the vendor app too; every other field here is best-effort, and a missing")
		f.Note("one only means the firmware did not answer (SPEC.md §6.4).")
	}
}

func onOff(v bool) string {
	if v {
		return "on   (LED stays lit in the Power Good state)"
	}
	return "off  (LED suppressed only while solid green; every other state still lights)"
}
