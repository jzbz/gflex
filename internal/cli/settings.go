package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/session"
)

// applyDryRun evaluates an interlock decision when nothing will actually be
// sent. A refusal still refuses -- --dry-run is for seeing what a command would
// do, and a command that would be refused does nothing -- but there is nothing
// to confirm because there is nothing to confirm to.
func (a *App) applyDryRun(f Formatter, d Decision) error {
	if !d.OK() {
		return refused(d.Refused)
	}
	for _, w := range d.Warnings {
		f.Diag("warning: %s", w)
	}
	return nil
}

// ---------------------------------------------------------------------------
// voltage
// ---------------------------------------------------------------------------

func newVoltageCommand(app *App) *cobra.Command {
	cmd := group(&cobra.Command{
		Use:   "voltage",
		Short: "Read or set the output voltage",
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "get",
		Short: "Read the configured output voltage",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				if app.DryRun {
					return app.dryRun(f, proto.Read(proto.CmdVoltageMv))
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				mv, err := c.Session.VoltageMv(ctx)
				if err != nil {
					return fmt.Errorf("reading output voltage: %w", err)
				}
				f.KV("voltage_mv", "output voltage", mv, formatMv(mv))
				return nil
			})
		},
	})
	var ignoreLimits bool
	setCmd := &cobra.Command{
		Use:   "set <value>",
		Short: "Set the output voltage",
		Long: "set writes the output voltage, in millivolts on the wire.\n\n" +
			"Accepts \"12\", \"12V\", \"12000mV\" or \"9.5\"; a bare number is volts.\n\n" +
			"The user voltage limits are read first and the value is refused outside them, and\n" +
			"outside the documented 3300-48000 mV hardware envelope. Anything above 5 V is\n" +
			"confirmed interactively, or needs --yes (SPEC.md §13).\n\n" +
			"If those limits cannot be read, or the unit reports a window that cannot bound\n" +
			"anything, the write is refused rather than falling back to the hardware envelope:\n" +
			"the window is the limit you chose for your own load. --ignore-device-limits\n" +
			"overrides that deliberately; --yes does not.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				mv, err := ParseVoltage(args[0])
				if err != nil {
					return err
				}
				if app.DryRun {
					// No device is opened, so the window genuinely cannot be
					// read. Judge the value against the hardware envelope and
					// the 16-bit wrap check alone rather than refusing for a
					// limit read that was never going to happen.
					if err := app.applyDryRun(f, CheckVoltage(VoltageRequest{Mv: mv, IgnoreDeviceLimits: true})); err != nil {
						return err
					}
					frame, err := proto.Write(proto.CmdVoltageMv, proto.EncodeU16(uint16(mv)))
					if err != nil {
						return err
					}
					return app.dryRun(f, frame)
				}

				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()

				req := VoltageRequest{Mv: mv, IgnoreDeviceLimits: ignoreLimits}
				low, high, lerr := c.Session.VLimit(ctx)
				switch {
				case lerr != nil:
					f.Diag("warning: could not read the user voltage limits: %v", lerr)
					req.Limits = LimitsUnread
				case !VLimitUsable(low, high):
					// Only a pair that cannot bound anything is rejected. A
					// narrow window is the point of the feature, not a fault.
					req.LimitLowMv, req.LimitHighMv = low, high
					req.Limits = LimitsMalformed
				default:
					req.LimitLowMv, req.LimitHighMv, req.Limits = low, high, LimitsValid
				}
				if err := app.apply(ctx, f, CheckVoltage(req)); err != nil {
					return err
				}

				readback, err := c.Session.SetVoltageMv(ctx, uint16(mv))
				if errors.Is(err, session.ErrReadBack) {
					// The device acknowledged the write, so the rail is already
					// at the new voltage. Saying "setting output voltage failed"
					// here would be actively dangerous: the user would believe
					// the old value still stands and wire up accordingly.
					f.Diag("warning: %v", err)
					f.KV("voltage_mv", "output voltage", uint16(mv),
						formatMv(uint16(mv))+"  (written and acknowledged, not read back)")
					return nil
				}
				if err != nil {
					return fmt.Errorf("setting output voltage: %w", err)
				}
				f.KV("voltage_mv", "output voltage", readback, formatMv(readback))
				if int(readback) != mv {
					f.Diag("warning: read back %d mV after writing %d mV", readback, mv)
				}
				return nil
			})
		},
	}
	setCmd.Flags().BoolVar(&ignoreLimits, "ignore-device-limits", false,
		"write even though this unit's configured voltage window could not be read or is unusable")
	cmd.AddCommand(setCmd)
	return cmd
}

// ---------------------------------------------------------------------------
// current
// ---------------------------------------------------------------------------

func newCurrentCommand(app *App) *cobra.Command {
	cmd := group(&cobra.Command{
		Use:   "current",
		Short: "Read or set the current limit",
		Long: "The current limit is what the VFLEX requests during PD negotiation. The hardware\n" +
			"has no current sensing, so this is never a measurement (SPEC.md §6.5).",
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "get",
		Short: "Read the configured current limit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				if app.DryRun {
					return app.dryRun(f, proto.Read(proto.CmdCurrentLimitMa))
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				ma, err := c.Session.CurrentLimitMa(ctx)
				if err != nil {
					return fmt.Errorf("reading current limit: %w", err)
				}
				f.KV("current_limit_ma", "current limit", ma, formatMa(ma))
				return nil
			})
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "set <value>",
		Short: "Set the current limit",
		Long: "set writes the current limit, in milliamps on the wire.\n\n" +
			"Accepts \"3\", \"3A\", \"3000mA\" or \"3.0\"; a bare number is amps. The device\n" +
			"default is 5000 mA, which is also the maximum pass-through current.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				ma, err := ParseCurrent(args[0])
				if err != nil {
					return err
				}
				d := CheckCurrent(ma)
				if app.DryRun {
					if err := app.applyDryRun(f, d); err != nil {
						return err
					}
					frame, err := proto.Write(proto.CmdCurrentLimitMa, proto.EncodeU16(uint16(ma)))
					if err != nil {
						return err
					}
					return app.dryRun(f, frame)
				}
				if err := app.apply(ctx, f, d); err != nil {
					return err
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				if err := c.Session.SetCurrentLimitMa(ctx, uint16(ma)); err != nil {
					return fmt.Errorf("setting current limit: %w", err)
				}
				readback, err := c.Session.CurrentLimitMa(ctx)
				if err != nil {
					f.Diag("warning: could not read back the current limit: %v", err)
					f.KV("current_limit_ma", "current limit", uint16(ma), formatMa(uint16(ma))+"  (written, not read back)")
					return nil
				}
				f.KV("current_limit_ma", "current limit", readback, formatMa(readback))
				return nil
			})
		},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// vlimit
// ---------------------------------------------------------------------------

func newVLimitCommand(app *App) *cobra.Command {
	cmd := group(&cobra.Command{
		Use:   "vlimit",
		Short: "Read or set the user voltage limits",
		Long: "The user voltage limits bracket what `voltage set` will accept. They are the\n" +
			"guard rail this tool enforces before writing an output voltage.",
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "get",
		Short: "Read the user voltage limits",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				if app.DryRun {
					return app.dryRun(f, proto.Read(proto.CmdUserVLimit))
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				low, high, err := c.Session.VLimit(ctx)
				if err != nil {
					return fmt.Errorf("reading user voltage limits: %w", err)
				}
				emitVLimit(f, low, high)
				return nil
			})
		},
	})

	var lowArg, highArg string
	set := &cobra.Command{
		Use:   "set --low <value> --high <value>",
		Short: "Set the user voltage limits",
		Long: "set writes the user voltage limits. Narrowing the window is safe and needs no\n" +
			"confirmation; widening it removes the guard rail that `voltage set` depends on and\n" +
			"is confirmed, or needs --yes (SPEC.md §13.3).\n\n" +
			"Either flag may be omitted, in which case the current value is kept. Note that the\n" +
			"wire order is HIGH first then LOW, in both directions (SPEC.md §6.5); this command\n" +
			"takes them in the readable order and the encoder handles the reversal.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				haveLow := cmd.Flags().Changed("low")
				haveHigh := cmd.Flags().Changed("high")
				if !haveLow && !haveHigh {
					return codedf(ExitUsage, "give at least one of --low or --high")
				}
				var newLow, newHigh int
				var err error
				if haveLow {
					if newLow, err = ParseVoltage(lowArg); err != nil {
						return err
					}
				}
				if haveHigh {
					if newHigh, err = ParseVoltage(highArg); err != nil {
						return err
					}
				}

				if app.DryRun {
					if !haveLow || !haveHigh {
						return codedf(ExitUsage,
							"--dry-run needs both --low and --high: the missing one would have to be read from the device")
					}
					if err := app.applyDryRun(f, CheckVLimit(VLimitRequest{NewLowMv: newLow, NewHighMv: newHigh})); err != nil {
						return err
					}
					frame, err := proto.Write(proto.CmdUserVLimit, proto.EncodeVLimit(uint16(newLow), uint16(newHigh)))
					if err != nil {
						return err
					}
					return app.dryRun(f, frame)
				}

				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()

				req := VLimitRequest{NewLowMv: newLow, NewHighMv: newHigh}
				curLow, curHigh, lerr := c.Session.VLimit(ctx)
				if lerr != nil {
					if !haveLow || !haveHigh {
						return fmt.Errorf("reading the current voltage limits (needed to fill in the omitted flag): %w", lerr)
					}
					f.Diag("warning: could not read the current voltage limits: %v", lerr)
				} else {
					req.CurLowMv, req.CurHighMv, req.CurKnown = curLow, curHigh, true
					if !haveLow {
						req.NewLowMv = int(curLow)
					}
					if !haveHigh {
						req.NewHighMv = int(curHigh)
					}
				}
				if err := app.apply(ctx, f, CheckVLimit(req)); err != nil {
					return err
				}
				if err := c.Session.SetVLimit(ctx, uint16(req.NewLowMv), uint16(req.NewHighMv)); err != nil {
					return fmt.Errorf("setting user voltage limits: %w", err)
				}
				low, high, rerr := c.Session.VLimit(ctx)
				if rerr != nil {
					f.Diag("warning: could not read back the voltage limits: %v", rerr)
					emitVLimit(f, uint16(req.NewLowMv), uint16(req.NewHighMv))
					return nil
				}
				emitVLimit(f, low, high)
				return nil
			})
		},
	}
	set.Flags().StringVar(&lowArg, "low", "", "lower limit, e.g. 3.3, 3.3V or 3300mV")
	set.Flags().StringVar(&highArg, "high", "", "upper limit, e.g. 48, 48V or 48000mV")
	cmd.AddCommand(set)
	return cmd
}

func emitVLimit(f Formatter, low, high uint16) {
	f.KV("vlimit_low_mv", "voltage limits", low, vlimitDisplay(low, high))
	f.KV("vlimit_high_mv", "", high, "")
}

// vlimitDisplay renders a window, annotating only the case that actually
// changes behaviour: a pair that cannot bound anything, which makes
// `voltage set` refuse rather than fall back.
//
// A merely narrow window gets no annotation. It is honoured exactly as written
// and there is nothing to warn about.
func vlimitDisplay(low, high uint16) string {
	s := fmt.Sprintf("%d - %d mV (%s - %s V)", low, high,
		trimFloat(float64(low)/1000, 3), trimFloat(float64(high)/1000, 3))
	if !VLimitUsable(low, high) {
		s += "   [unusable window; `voltage set` will refuse until it is repaired]"
	}
	return s
}

// ---------------------------------------------------------------------------
// tolerance
// ---------------------------------------------------------------------------

func newToleranceCommand(app *App) *cobra.Command {
	cmd := group(&cobra.Command{
		Use:   "tolerance",
		Short: "Read or set the output tolerance parameters",
		Long: "The nominal term is in millivolts and defaults to 750. Whether that is a\n" +
			"symmetric band or one-sided is unknown (SPEC.md §14.10).\n\n" +
			"The sag term is a raw 16-bit value with no default, no unit and no consumer in the\n" +
			"vendor app. A literal \"mV per mA\" reading is dimensionally implausible at integer\n" +
			"resolution, so a scale factor almost certainly exists; this tool will not invent one\n" +
			"(SPEC.md §6.5, §14.9).",
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "get",
		Short: "Read the tolerance parameters",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				if app.DryRun {
					return app.dryRun(f,
						proto.Read(proto.CmdVToleranceNominalMv),
						proto.Read(proto.CmdVToleranceSagPerMa))
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				nominal, nerr := c.Session.VToleranceNominalMv(ctx)
				if nerr != nil {
					return fmt.Errorf("reading nominal tolerance: %w", nerr)
				}
				f.KV("vtolerance_nominal_mv", "tolerance (nominal)", nominal, fmt.Sprintf("%d mV", nominal))
				// The vendor app never reads the sag term, so the firmware's
				// willingness to answer is unverified: a failure here is
				// information, not an error.
				sag, serr := c.Session.VToleranceSagPerMa(ctx)
				if serr != nil {
					f.Diag("note: the sag term could not be read (%v); the vendor app never reads it either", serr)
					return nil
				}
				f.KV("vtolerance_sag_per_ma", "tolerance (sag)", sag, fmt.Sprintf("%d  (raw; units unknown)", sag))
				return nil
			})
		},
	})

	var nominalArg, sagArg string
	set := &cobra.Command{
		Use:   "set [--nominal <mV>] [--sag <raw>]",
		Short: "Set the tolerance parameters",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				haveNominal := cmd.Flags().Changed("nominal")
				haveSag := cmd.Flags().Changed("sag")
				if !haveNominal && !haveSag {
					return codedf(ExitUsage, "give at least one of --nominal or --sag")
				}
				var nominal, sag int
				if haveNominal {
					v, err := parseDecimalInt(nominalArg, "--nominal", fmt.Sprintf("0..%d", maxWireValue), 32)
					if err != nil {
						return err
					}
					nominal = int(v)
				}
				if haveSag {
					v, err := parseDecimalInt(sagArg, "--sag", fmt.Sprintf("0..%d", maxWireValue), 32)
					if err != nil {
						return err
					}
					sag = int(v)
				}
				for _, v := range []struct {
					name string
					on   bool
					val  int
				}{{"--nominal", haveNominal, nominal}, {"--sag", haveSag, sag}} {
					if v.on && (v.val < 0 || v.val > maxWireValue) {
						return codedf(ExitUsage, "%s must be in 0..%d, got %d", v.name, maxWireValue, v.val)
					}
				}

				var frames [][]byte
				if haveNominal {
					fr, err := proto.Write(proto.CmdVToleranceNominalMv, proto.EncodeU16(uint16(nominal)))
					if err != nil {
						return err
					}
					frames = append(frames, fr)
				}
				if haveSag {
					fr, err := proto.Write(proto.CmdVToleranceSagPerMa, proto.EncodeU16(uint16(sag)))
					if err != nil {
						return err
					}
					frames = append(frames, fr)
				}
				if app.DryRun {
					return app.dryRun(f, frames...)
				}

				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				if haveNominal {
					if err := c.Session.SetVToleranceNominalMv(ctx, uint16(nominal)); err != nil {
						return fmt.Errorf("setting nominal tolerance: %w", err)
					}
					f.KV("vtolerance_nominal_mv", "tolerance (nominal)", uint16(nominal), fmt.Sprintf("%d mV", nominal))
				}
				if haveSag {
					if err := c.Session.SetVToleranceSagPerMa(ctx, uint16(sag)); err != nil {
						return fmt.Errorf("setting sag tolerance: %w", err)
					}
					f.KV("vtolerance_sag_per_ma", "tolerance (sag)", uint16(sag), fmt.Sprintf("%d  (raw; units unknown)", sag))
				}
				return nil
			})
		},
	}
	// Strings parsed by parseDecimalInt, not IntVar: pflag's integer flags call
	// strconv.ParseInt(s, 0, …), so `--nominal 0750` would read as octal and
	// commit 488 mV to non-volatile memory. Same defect and same fix as
	// `authlock set` -- see parseDecimalInt for the whole argument.
	set.Flags().StringVar(&nominalArg, "nominal", strconv.FormatUint(uint64(proto.DefaultVToleranceNominal), 10),
		"nominal tolerance in millivolts (plain decimal)")
	set.Flags().StringVar(&sagArg, "sag", "0", "sag term as a raw 16-bit value, plain decimal (units unknown)")
	cmd.AddCommand(set)
	return cmd
}

// ---------------------------------------------------------------------------
// measure
// ---------------------------------------------------------------------------

func newMeasureCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "measure",
		Short: "Read the measured output voltage",
		Long: "measure returns the raw ADC count and the calibrated millivolts the device\n" +
			"computes from it. There is no host-side calibration formula anywhere in the vendor\n" +
			"client: the device does the arithmetic and the host only reports it (SPEC.md §6.5).\n\n" +
			"This is a voltage measurement only. The hardware has no current or power sensing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				if app.DryRun {
					return app.dryRun(f, proto.Read(proto.CmdVMeasure))
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				raw, mv, err := c.Session.Measure(ctx)
				if err != nil {
					return fmt.Errorf("reading measurement: %w", err)
				}
				f.KV("vmeasure_calibrated_mv", "measured voltage", mv, formatMv(mv))
				f.KV("vmeasure_raw_adc", "raw adc", raw, fmt.Sprintf("%d counts", raw))
				return nil
			})
		},
	}
}

// ---------------------------------------------------------------------------
// calibrate
// ---------------------------------------------------------------------------

func newCalibrateCommand(app *App) *cobra.Command {
	cmd := group(&cobra.Command{
		Use:   "calibrate",
		Short: "Read or write the ADC calibration",
	})
	var offsetArg, scaleArg string
	adc := &cobra.Command{
		Use:   "adc --offset <int32> --scale <int32>",
		Short: "Write the ADC calibration offset and scale",
		Long: "adc writes the two signed 32-bit calibration values. Both read 0 on a factory\n" +
			"unit that still measures correctly (raw 437 counts -> 5270 mV), so the firmware\n" +
			"does read 0 as \"use the built-in calibration\" rather than as a literal\n" +
			"multiplier -- measured, not inferred (SPEC.md §14.11). The formula it applies\n" +
			"instead is device-side and still unknown.\n\n" +
			"This is confirmed every time, or needs --yes: a wrong calibration makes every\n" +
			"subsequent voltage reading silently wrong, which defeats the range check on\n" +
			"`voltage set` (SPEC.md §13.5). The previous values are printed first, with the\n" +
			"command that restores them.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				haveOffset := cmd.Flags().Changed("offset")
				haveScale := cmd.Flags().Changed("scale")
				if !haveOffset && !haveScale {
					return codedf(ExitUsage, "give at least one of --offset or --scale")
				}
				var offset, scale int32
				if haveOffset {
					v, err := parseDecimalInt(offsetArg, "--offset", "a signed 32-bit value", 32)
					if err != nil {
						return err
					}
					offset = int32(v)
				}
				if haveScale {
					v, err := parseDecimalInt(scaleArg, "--scale", "a signed 32-bit value", 32)
					if err != nil {
						return err
					}
					scale = int32(v)
				}
				if app.DryRun {
					if !haveOffset || !haveScale {
						return codedf(ExitUsage,
							"--dry-run needs both --offset and --scale: the missing one would have to be read from the device")
					}
					if err := app.applyDryRun(f, CheckCalibrate(offset, scale, 0, 0, false)); err != nil {
						return err
					}
					offFrame, err := proto.Write(proto.CmdVMeasureADCOffset, proto.EncodeI32(offset))
					if err != nil {
						return err
					}
					scaleFrame, err := proto.Write(proto.CmdVMeasureADCScale, proto.EncodeI32(scale))
					if err != nil {
						return err
					}
					return app.dryRun(f, offFrame, scaleFrame)
				}

				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()

				prevOffset, oerr := c.Session.ADCOffset(ctx)
				prevScale, serr := c.Session.ADCScale(ctx)
				prevKnown := oerr == nil && serr == nil
				if !prevKnown {
					if !haveOffset || !haveScale {
						return fmt.Errorf("reading the current calibration (needed to fill in the omitted flag): %w",
							firstErr(oerr, serr))
					}
				} else {
					if !haveOffset {
						offset = prevOffset
					}
					if !haveScale {
						scale = prevScale
					}
				}
				if err := app.apply(ctx, f, CheckCalibrate(offset, scale, prevOffset, prevScale, prevKnown)); err != nil {
					return err
				}
				if err := c.Session.SetADCOffset(ctx, offset); err != nil {
					return fmt.Errorf("setting ADC offset: %w", err)
				}
				if err := c.Session.SetADCScale(ctx, scale); err != nil {
					return fmt.Errorf("setting ADC scale (offset was already written as %d): %w", offset, err)
				}
				f.KV("vmeasure_adc_offset", "adc offset", offset, strconv.FormatInt(int64(offset), 10))
				f.KV("vmeasure_adc_scale", "adc scale", scale, strconv.FormatInt(int64(scale), 10))
				return nil
			})
		},
	}
	// Strings parsed by parseDecimalInt rather than Int32Var, for the reason
	// given there: pflag parses an integer flag with base 0, so `--offset 010`
	// would write 8. A wrong calibration makes every later voltage reading
	// silently wrong (SPEC.md §13.5), and --yes suppresses the prompt that
	// would otherwise show the reinterpreted number.
	adc.Flags().StringVar(&offsetArg, "offset", strconv.FormatInt(int64(proto.DefaultADCOffset), 10),
		"signed 32-bit ADC count offset (plain decimal)")
	adc.Flags().StringVar(&scaleArg, "scale", strconv.FormatInt(int64(proto.DefaultADCScale), 10),
		"signed 32-bit ADC count scale (plain decimal)")
	cmd.AddCommand(adc)

	cmd.AddCommand(&cobra.Command{
		Use:   "get",
		Short: "Read the ADC calibration offset and scale",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				if app.DryRun {
					return app.dryRun(f,
						proto.Read(proto.CmdVMeasureADCOffset),
						proto.Read(proto.CmdVMeasureADCScale))
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				offset, err := c.Session.ADCOffset(ctx)
				if err != nil {
					return fmt.Errorf("reading ADC offset: %w", err)
				}
				scale, err := c.Session.ADCScale(ctx)
				if err != nil {
					return fmt.Errorf("reading ADC scale: %w", err)
				}
				f.KV("vmeasure_adc_offset", "adc offset", offset, strconv.FormatInt(int64(offset), 10))
				f.KV("vmeasure_adc_scale", "adc scale", scale, strconv.FormatInt(int64(scale), 10))
				return nil
			})
		},
	})
	return cmd
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// led
// ---------------------------------------------------------------------------

func newLEDCommand(app *App) *cobra.Command {
	cmd := group(&cobra.Command{
		Use:   "led",
		Short: "Read or set the \"LED Always On\" setting, or drive the LED to a colour",
		Long: "get and set carry the \"LED Always On\" setting. \"on\" is the user-facing sense:\n" +
			"the LED stays lit in the Power Good state. \"off\" suppresses it only while solid\n" +
			"green; every other state still lights (SPEC.md §1.1).\n\n" +
			"The wire field is named the other way round -- DISABLE_LED_DURING_OPERATION, where\n" +
			"0 means always-on -- so this command deliberately does not use the wire name\n" +
			"(SPEC.md §6.2).\n\n" +
			"color is a different command (13) and a momentary effect rather than a setting;\n" +
			"see `gflex led color --help`.",
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "get",
		Short: "Read the LED setting",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				if app.DryRun {
					return app.dryRun(f, proto.Read(proto.CmdDisableLEDDuringOp))
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				on, err := c.Session.LEDAlwaysOn(ctx)
				if err != nil {
					return fmt.Errorf("reading the LED setting: %w", err)
				}
				f.KV("led_always_on", "led always on", on, onOff(on))
				return nil
			})
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:       "set on|off",
		Short:     "Set the LED setting",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				var on bool
				switch strings.ToLower(args[0]) {
				case "on", "true", "1", "enable", "enabled":
					on = true
				case "off", "false", "0", "disable", "disabled":
					on = false
				default:
					return codedf(ExitUsage, "expected on or off, got %q", args[0])
				}
				if app.DryRun {
					frame, err := proto.Write(proto.CmdDisableLEDDuringOp, []byte{proto.EncodeLEDAlwaysOn(on)})
					if err != nil {
						return err
					}
					return app.dryRun(f, frame)
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				if err := c.Session.SetLEDAlwaysOn(ctx, on); err != nil {
					return fmt.Errorf("setting the LED setting: %w", err)
				}
				// Annotated "(written)", exactly as `authlock set` is: this
				// command does not spend a round trip reading the setting back,
				// so the value below is what was asked for, not what the device
				// is known to hold. The distinction is not pedantic -- a
				// scratchpad-flagged write is acknowledged and echoed and still
				// never commits (SPEC.md §14.4) -- and without the annotation
				// this line is indistinguishable from `voltage set`'s, which
				// does read back.
				f.KV("led_always_on", "led always on", on, onOff(on)+"  (written)")
				return nil
			})
		},
	})
	cmd.AddCommand(newLEDColorCommand(app))
	return cmd
}

// newLEDColorCommand drives the LED to a colour with command 13.
//
// This is the one command in the tree whose only source is the vendor's
// published library rather than the shipped application or a measurement, and
// the help says so. Everything else here was either read out of the app that
// ships to users or confirmed on hardware (SPEC.md §0); this was transcribed
// from tundra-labs/lib.vflex.app, which documents the write and the eight
// colours and ships a CLI that sends it.
func newLEDColorCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "color " + strings.Join(proto.LEDColorNames(), "|"),
		Short: "Drive the LED to a colour",
		Long: "Sends command 13 (FLASH_LED_SEQUENCE_ADVANCED) with the vendor library's own\n" +
			"five-byte payload, [10, 1, color, 2, 0].\n\n" +
			"Only the colour byte is understood. The other four are sent unchanged because\n" +
			"that is what the vendor's library sends for every colour; the command's name\n" +
			"suggests they describe a sequence, but nothing documents them (SPEC.md §14.17).\n\n" +
			"How long the colour lasts, and whether it survives a power state change, are\n" +
			"both unknown -- there is no read side to ask. Use --dry-run to see the frame\n" +
			"without sending it.",
		Args:      cobra.ExactArgs(1),
		ValidArgs: proto.LEDColorNames(),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				colour, ok := proto.ParseLEDColor(args[0])
				if !ok {
					return codedf(ExitUsage, "unknown colour %q; valid colours are %s",
						args[0], strings.Join(proto.LEDColorNames(), ", "))
				}
				if app.DryRun {
					frame, err := proto.Write(proto.CmdFlashLEDSeqAdvanced, proto.LEDColorPayload(colour))
					if err != nil {
						return err
					}
					return app.dryRun(f, frame)
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				if err := c.Session.SetLEDColor(ctx, colour); err != nil {
					return fmt.Errorf("setting the LED colour: %w", err)
				}
				// "(written)" for the same reason as `led set` above: no read
				// side exists at all here, so this is what was sent, not what
				// the device is known to be showing.
				f.KV("led_color", "led colour", colour.String(), colour.String()+"  (written)")
				return nil
			})
		},
	}
}

// ---------------------------------------------------------------------------
// authlock
// ---------------------------------------------------------------------------

// parseAuthLockLevel parses the <level> argument of `authlock set` as plain
// decimal, and nothing else.
//
// This used to be strconv.ParseInt(arg, 0, 32), and base 0 is Go literal
// syntax: a leading zero means octal, so "010" wrote level 8, and "0x10" and
// "1_0" were accepted too. The auth lock is the last place to be more lenient
// than a voltage: it is the least understood command in the protocol, and a
// non-zero level may not be reversible (SPEC.md §6.3, §14.8), so the byte
// written must be beyond doubt the number the user typed. parseDecimalInt in
// parse.go now carries that grammar, and the argument for it, for every
// device-write integer this CLI accepts.
//
// Range stays out of this function on purpose: CheckAuthLock owns the 0-255
// bound, so enforcement lives in one place (SPEC.md §13, interlock 4), which
// is why the sign and out-of-byte values parse here and are refused there. The
// 32-bit width is conversion safety, not range policy: it makes the int() below
// exact even where int is 32 bits.
func parseAuthLockLevel(arg string) (int, error) {
	level64, err := parseDecimalInt(arg, "auth lock level", "a single byte (0-255)", 32)
	if err != nil {
		return 0, err
	}
	return int(level64), nil
}

func newAuthLockCommand(app *App) *cobra.Command {
	cmd := group(&cobra.Command{
		Use:   "authlock",
		Short: "Read or set the auth lock level",
		Long: "The auth lock is the least understood command in the protocol. The write puts the\n" +
			"level in the first payload byte, and the read response carries two -- [0x16, level],\n" +
			"the command code echoed again and then the level -- so the reader takes the second.\n" +
			"That layout was measured on hardware and matches the vendor client (SPEC.md §14.8).\n" +
			"Only level 0 (\"unlocked\") is named anywhere; what other levels gate, and how to\n" +
			"leave one, is still unknown, and no level above 0 has ever been set (SPEC.md §6.3).\n\n" +
			"`get` prints the whole response payload alongside the level, so a response that does\n" +
			"not match that layout is visible rather than silently decoded.",
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "get",
		Short: "Read the auth lock level",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				if app.DryRun {
					return app.dryRun(f, proto.Read(proto.CmdAuthLock))
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				level, raw, err := c.Session.AuthLock(ctx)
				if err != nil {
					return fmt.Errorf("reading the auth lock: %w", err)
				}
				f.KV("authlock_level", "auth lock level", level, fmt.Sprintf("%d", level))
				f.KV("authlock_raw", "response payload", raw, proto.Hex(raw))
				f.Note("")
				f.Note("The level above is payload byte 1. Hardware settled this: the response is")
				f.Note("[0x16, level], so the vendor client's index was right and there was never an")
				f.Note("off-by-one (SPEC.md §14.8). What levels above 0 gate is still untested.")
				return nil
			})
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "set <level>",
		Short: "Set the auth lock level",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.run(cmd, func(ctx context.Context, f Formatter) error {
				level, err := parseAuthLockLevel(args[0])
				if err != nil {
					return err
				}
				d := CheckAuthLock(level)
				if app.DryRun {
					if err := app.applyDryRun(f, d); err != nil {
						return err
					}
					// The write puts the level in the FIRST payload byte
					// (SPEC.md §6.3); the asymmetry is on the read side.
					frame, err := proto.Write(proto.CmdAuthLock, []byte{uint8(level)})
					if err != nil {
						return err
					}
					return app.dryRun(f, frame)
				}
				if err := app.apply(ctx, f, d); err != nil {
					return err
				}
				c, err := app.connect(ctx, f)
				if err != nil {
					return err
				}
				defer c.Close()
				if err := c.Session.SetAuthLock(ctx, uint8(level)); err != nil {
					return fmt.Errorf("setting the auth lock: %w", err)
				}
				f.KV("authlock_level", "auth lock level", uint8(level), fmt.Sprintf("%d  (written)", level))
				return nil
			})
		},
	})
	return cmd
}
