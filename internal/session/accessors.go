package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jzbz/gflex/internal/ctxwait"
	"github.com/jzbz/gflex/internal/proto"
)

// Voltage read-ready retry policy (SPEC.md §6.5, §7).
//
// The vendor client's connect choreography is a fixed ladder: sleep 500 ms,
// read the serial, sleep 800 ms, then readVoltageWithRetry(3 attempts, 300 ms
// apart), and on total failure an OUTER backoff chain starting at 1500 ms,
// +1000 ms per attempt, up to 5 attempts, each doing 4 reads 400 ms apart --
// roughly 25 s of persistence against a unit that keeps answering 0 mV, i.e.
// "not ready" (SPEC.md §6.5).
//
// The substance of that is reproduced here; the literal ladder is not. Two
// deliberate departures:
//
//   - No fixed settle. The 500/800 ms sleeps make sense for an app that holds
//     one long-lived session; this is a CLI that runs once per command, so
//     paying 1.3 s on every invocation to help the first read of a fresh unit
//     is the wrong trade. The first read goes out immediately instead, and a
//     device that is ready costs nothing extra.
//   - A total budget rather than a fixed attempt count. Only a not-ready
//     answer (or a failed read) starts an escalating backoff, and it keeps
//     going until the budget is spent -- which is what the vendor's two nested
//     loops amount to, without hard-coding how many probes fit inside it.
const (
	// DefaultReadyTimeout is how long VoltageMv persists against a device that
	// is not yet answering usefully. It is a budget, not a delay: a unit that
	// answers on the first read waits zero. Ten seconds is comfortably longer
	// than the ~1.3 s the vendor spends settling before its first read and
	// shorter than its ~25 s worst case, which is far past the point where a
	// CLI should be telling the user something is wrong.
	DefaultReadyTimeout = 10 * time.Second

	// readyRetryInitialDelay is the pause after the first not-ready answer. It
	// is short because the common case is a unit that is nearly ready: the
	// vendor's own inner loop retries at 300 ms and usually succeeds there.
	readyRetryInitialDelay = 100 * time.Millisecond
	// readyRetryMaxDelay caps the doubling, so a long budget still gets probed
	// about once a second instead of spending its tail asleep.
	readyRetryMaxDelay = time.Second
)

// errNotReady is the sentinel for a 0 mV answer. See readVoltageOnce.
var errNotReady = errors.New("device reported 0 mV (output not ready)")

// ---------------------------------------------------------------------------
// Identity strings (commands 8-12). SPEC.md §6.4.
// ---------------------------------------------------------------------------

// SerialNumber reads the unit's serial number (command 8).
//
// The serial is the only stable identifier the protocol offers, and the PDO
// scan uses it as a hard invariant that the unit read back is the unit that was
// erased (SPEC.md §9.2). Use proto.SerialUsable to check that a returned serial
// is long enough to trust.
func (s *Session) SerialNumber(ctx context.Context) (string, error) {
	return s.readString(ctx, proto.CmdSerialNumber)
}

// ChipUUID reads the MCU unique ID (command 9).
//
// The vendor application never issues this command, but the firmware answers
// it: bring-up measured a 16-byte UUID, not the 8 bytes §6.4's table assumed
// (SPEC.md §14, "Corrections this produced"). That is why readString below
// decodes by the frame's declared length rather than by proto.StringLen -- the
// vendor's own write guard, which trusts the table, would have rejected a
// correct UUID.
func (s *Session) ChipUUID(ctx context.Context) (string, error) {
	return s.readString(ctx, proto.CmdChipUUID)
}

// HardwareID reads the hardware revision string (command 10).
//
// Never issued by the vendor application; see ChipUUID.
func (s *Session) HardwareID(ctx context.Context) (string, error) {
	return s.readString(ctx, proto.CmdHardwareID)
}

// FirmwareVersion reads the firmware version string (command 11).
func (s *Session) FirmwareVersion(ctx context.Context) (string, error) {
	return s.readString(ctx, proto.CmdFirmwareVersion)
}

// MfgDate reads the manufacturing date string (command 12).
//
// Never issued by the vendor application; see ChipUUID.
func (s *Session) MfgDate(ctx context.Context) (string, error) {
	return s.readString(ctx, proto.CmdMfgDate)
}

// readString issues a read and sanitises the payload. The declared frame length
// governs how many bytes are decoded; the fixed lengths in proto.StringLen are
// expectations, not guarantees (SPEC.md §6.4).
func (s *Session) readString(ctx context.Context, cmd proto.Cmd) (string, error) {
	f, err := s.Do(ctx, cmd, nil, false)
	if err != nil {
		return "", err
	}
	return proto.DecodeString(f.Payload), nil
}

// ---------------------------------------------------------------------------
// Voltage and current (commands 18, 19). SPEC.md §6.5.
// ---------------------------------------------------------------------------

// VoltageMv reads the programmed output voltage in millivolts (command 18).
//
// A freshly plugged unit is not immediately readable: it answers 0 mV, or does
// not answer at all, until the PD negotiation has settled. This is the first
// thing a new user meets, so the read is persistent rather than a fixed handful
// of attempts -- the first read is issued immediately, and only a not-ready
// answer or a failed read starts an escalating backoff (100 ms, doubling, capped
// at 1 s) that runs until Options.ReadyTimeout is spent. A device that is ready
// pays nothing for any of that.
//
// See the retry-policy block above for how this relates to the vendor client's
// connect choreography (SPEC.md §7), and readVoltageOnce for why 0 mV is a
// failure rather than a reading.
func (s *Session) VoltageMv(ctx context.Context) (uint16, error) {
	budget := s.readyTimeout
	deadline := time.Now().Add(budget)

	attempts := 0
	delay := readyRetryInitialDelay
	var last error

	for {
		attempts++
		mv, err := s.readVoltageOnce(ctx)
		if err == nil {
			return mv, nil
		}
		last = err

		// A cancelled context is not "not ready": every further attempt would
		// fail the same way, and the caller wants to hear that it was cancelled
		// rather than that the device never settled. (The classification below
		// would catch the cancellation too, but only when it surfaced through
		// the read; this check also catches a context that ended while the
		// read itself came back errNotReady.)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, fmt.Errorf("read voltage: %w", ctxErr)
		}

		// Retry only what waiting can cure. That is exactly two conditions:
		//
		//   - errNotReady: the unit answered 0 mV because the PD negotiation
		//     has not settled, and time is precisely the remedy (SPEC.md §6.5).
		//   - ErrTimeout: the protocol has no NACK, so a frame lost in either
		//     direction surfaces only as a timeout and re-asking is the
		//     designed recovery (SPEC.md §5.2) -- a silent still-settling unit
		//     looks like this too.
		//
		// Everything else is structural and returns immediately: the permanent
		// conditions of PermanentErr (ErrTransportClosed,
		// ErrNoConnection, framer.ErrClosed, a dead context -- a Session has
		// no reconnect path, so those fail identically on every attempt), and
		// likewise a decode failure, which means the device is answering
		// malformed frames, not settling. Retrying any of those burns the
		// whole ready budget on attempts that fail instantly -- and on
		// SetVoltageMv's read-back that delay lands at the worst moment: after
		// a post-write unplug the caller must hear NOW that the write was
		// acknowledged but unverifiable (ErrReadBack), because the rail is
		// already live at the new value (SPEC.md §6.5, §13).
		if !errors.Is(err, errNotReady) && !errors.Is(err, ErrTimeout) {
			return 0, fmt.Errorf("read voltage: %w", err)
		}

		// The budget is a wall-clock deadline, not a sum of pauses: on a silent
		// device every attempt already costs a full command timeout, and adding
		// those on top of the requested budget is not what a caller who lowered
		// ReadyTimeout asked for. Clipping the last backoff to what remains puts
		// the final probe on the deadline rather than past it.
		wait := time.Until(deadline)
		if wait <= 0 {
			break
		}
		if wait > delay {
			wait = delay
		}
		if err := ctxwait.Sleep(ctx, wait); err != nil {
			return 0, fmt.Errorf("read voltage: %w", err)
		}
		delay *= 2
		if delay > readyRetryMaxDelay {
			delay = readyRetryMaxDelay
		}
	}
	return 0, fmt.Errorf("read voltage: no usable reading after %d attempts over %v: %w",
		attempts, budget, last)
}

// readVoltageOnce issues one voltage read (02 12) and treats a 0 mV response as
// a failure.
//
// 0 mV means "not ready", not "zero volts" (SPEC.md §6.5): the vendor client
// discards it and retries rather than reporting it, and matching that is not
// optional. Reporting 0 V would tell a user their rail is dead when it is
// merely still coming up -- and, worse, would sail through any downstream check
// that only looks for an over-voltage.
func (s *Session) readVoltageOnce(ctx context.Context) (uint16, error) {
	f, err := s.Do(ctx, proto.CmdVoltageMv, nil, false)
	if err != nil {
		return 0, err
	}
	mv, err := proto.DecodeU16(f.Payload)
	if err != nil {
		return 0, err
	}
	if mv == 0 {
		return 0, errNotReady
	}
	return mv, nil
}

// SetVoltageMv programmes the output voltage and returns the value read back
// from the device.
//
// The device echoes writes, but the echo is never trusted here: the vendor app
// itself issues an explicit read-back for voltage, and that read-back carries
// the not-ready retry of VoltageMv. The returned value is what the device
// reports, which is what should be shown to the user.
//
// No range checking happens here. The vendor app applies none at all, and the
// safety interlocks of SPEC.md §13 (clamp to the user vlimit window and to the
// 3300-48000 mV hardware envelope, confirm above 5 V, warn above 20 V) belong
// to the CLI layer, which owns the terminal and the confirmation prompts.
// (The gate is a confirmation or --yes, plus the self-describing
// --ignore-device-limits where a second key is genuinely needed; there is no
// global --force flag -- SPEC.md §11, §13's preamble.)
func (s *Session) SetVoltageMv(ctx context.Context, mv uint16) (uint16, error) {
	if _, err := s.Do(ctx, proto.CmdVoltageMv, proto.EncodeU16(mv), true); err != nil {
		return 0, fmt.Errorf("write voltage %d mV: %w", mv, err)
	}
	got, err := s.VoltageMv(ctx)
	if err != nil {
		// The write was acknowledged, so the rail is already at mv. Only the
		// confirming read failed -- which on a unit that was just attached means
		// it kept answering 0 mV, "not ready", for the whole ReadyTimeout budget
		// (SPEC.md §6.5).
		//
		// Collapsing this into a plain error would tell the user the voltage
		// was not set while the rail is live at the new value: the most
		// dangerous possible thing to be wrong about here. ErrReadBack lets the
		// caller say what actually happened.
		return 0, fmt.Errorf("%w: wrote %d mV and the device acknowledged it, but reading it back failed: %w",
			ErrReadBack, mv, err)
	}
	return got, nil
}

// CurrentLimitMa reads the negotiated current limit in milliamps (command 19).
//
// This is a request made of the PD source, not a measurement: the hardware has
// no current sensing whatsoever, so never present it as an observed current.
func (s *Session) CurrentLimitMa(ctx context.Context) (uint16, error) {
	f, err := s.Do(ctx, proto.CmdCurrentLimitMa, nil, false)
	if err != nil {
		return 0, err
	}
	ma, err := proto.DecodeU16(f.Payload)
	if err != nil {
		return 0, fmt.Errorf("decode current limit: %w", err)
	}
	return ma, nil
}

// SetCurrentLimitMa writes the current limit in milliamps (command 19).
func (s *Session) SetCurrentLimitMa(ctx context.Context, ma uint16) error {
	if _, err := s.Do(ctx, proto.CmdCurrentLimitMa, proto.EncodeU16(ma), true); err != nil {
		return fmt.Errorf("write current limit %d mA: %w", ma, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// User voltage limits (command 23). SPEC.md §6.5.
// ---------------------------------------------------------------------------

// VLimit reads the user voltage-limit window (command 23).
//
// The wire carries HIGH before LOW in both directions; this signature is
// (low, high), the reverse. The swap lives entirely in proto.DecodeVLimit --
// never re-derive it here.
func (s *Session) VLimit(ctx context.Context) (lowMv, highMv uint16, err error) {
	f, err := s.Do(ctx, proto.CmdUserVLimit, nil, false)
	if err != nil {
		return 0, 0, err
	}
	lowMv, highMv, err = proto.DecodeVLimit(f.Payload)
	if err != nil {
		return 0, 0, fmt.Errorf("decode vlimit: %w", err)
	}
	return lowMv, highMv, nil
}

// SetVLimit writes the user voltage-limit window (command 23).
//
// Arguments are (low, high); proto.EncodeVLimit puts HIGH on the wire first,
// producing e.g. 06 97 BB 80 0C E4 for low=3300 high=48000.
//
// Widening this window removes the guard rail that voltage writes are checked
// against, which is why SPEC.md §13.3 has the CLI confirm it -- interactively,
// or with --yes when there is no terminal. Narrowing needs no confirmation.
// (There is no global --force flag: SPEC.md §11, §13's preamble.)
func (s *Session) SetVLimit(ctx context.Context, lowMv, highMv uint16) error {
	if _, err := s.Do(ctx, proto.CmdUserVLimit, proto.EncodeVLimit(lowMv, highMv), true); err != nil {
		return fmt.Errorf("write vlimit low=%d high=%d: %w", lowMv, highMv, err)
	}
	return nil
}

// VLimitPlausible reports whether a vlimit pair has the shape the vendor
// firmware expects to find on an un-erased unit. This is the canonical copy;
// other packages import it rather than restating the thresholds.
//
// It answers exactly one question: did a firmware flash wipe the stored window?
// The thresholds are the vendor's own, taken from the post-update routine that
// rewrites the pair to the 3300/48000 defaults when low < 3000, high < 6000, or
// high <= low (SPEC.md §6.5, §10.4). PostUpdateInitForce is the intended -- and
// only -- kind of caller.
//
// It is NEVER a test of whether to trust a window the user configured, and must
// never gate the `voltage set` interlock of SPEC.md §13.1. That interlock needs
// only that the window can bound something, i.e. high > low. Running a
// configured window through this predicate instead discards every window with a
// ceiling under 6 V -- [3300, 5000], exactly what someone protecting a 5 V pedal
// would set, and the strictest guard rail in the system -- and then falls back
// to the 48 V envelope, which is how a request for 20 V onto a 5 V pedal gets
// approved. That defect has already been shipped once; it is what this doc
// comment and the single shared copy exist to prevent.
func VLimitPlausible(lowMv, highMv uint16) bool {
	return lowMv >= 3000 && highMv >= 6000 && highMv > lowMv
}

// ---------------------------------------------------------------------------
// Tolerance (commands 24, 25). SPEC.md §6.5.
// ---------------------------------------------------------------------------

// VToleranceNominalMv reads the nominal voltage tolerance in millivolts
// (command 24). The default is 750 mV. Whether that is a symmetric band or
// one-sided is undetermined (SPEC.md §14.10).
func (s *Session) VToleranceNominalMv(ctx context.Context) (uint16, error) {
	return s.readU16(ctx, proto.CmdVToleranceNominalMv, "vtolerance nominal")
}

// SetVToleranceNominalMv writes the nominal voltage tolerance in millivolts.
func (s *Session) SetVToleranceNominalMv(ctx context.Context, mv uint16) error {
	return s.writeU16(ctx, proto.CmdVToleranceNominalMv, mv, "vtolerance nominal")
}

// VToleranceSagPerMa reads the sag tolerance term (command 25) as a raw 16-bit
// value.
//
// The units are unknown and there is no default: the vendor app neither reads
// nor writes this command. A literal "mV per mA" reading is dimensionally
// implausible at integer resolution (one unit would be a whole ohm), so an
// undocumented scale factor almost certainly exists. Do not convert this value
// or label it with a unit.
func (s *Session) VToleranceSagPerMa(ctx context.Context) (uint16, error) {
	return s.readU16(ctx, proto.CmdVToleranceSagPerMa, "vtolerance sag")
}

// SetVToleranceSagPerMa writes the raw 16-bit sag tolerance term (command 25).
// See VToleranceSagPerMa: the units are unknown.
func (s *Session) SetVToleranceSagPerMa(ctx context.Context, v uint16) error {
	return s.writeU16(ctx, proto.CmdVToleranceSagPerMa, v, "vtolerance sag")
}

func (s *Session) readU16(ctx context.Context, cmd proto.Cmd, what string) (uint16, error) {
	f, err := s.Do(ctx, cmd, nil, false)
	if err != nil {
		return 0, err
	}
	v, err := proto.DecodeU16(f.Payload)
	if err != nil {
		return 0, fmt.Errorf("decode %s: %w", what, err)
	}
	return v, nil
}

func (s *Session) writeU16(ctx context.Context, cmd proto.Cmd, v uint16, what string) error {
	if _, err := s.Do(ctx, cmd, proto.EncodeU16(v), true); err != nil {
		return fmt.Errorf("write %s %d: %w", what, v, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// ADC calibration and measurement (commands 26, 27, 28). SPEC.md §6.5.
// ---------------------------------------------------------------------------

// ADCOffset reads the voltage-measurement ADC count offset (command 26).
//
// The value is a signed int32: the vendor client decodes it with JavaScript
// shift operators, which produce a signed 32-bit result, so a response with the
// top bit set really does mean a negative offset.
func (s *Session) ADCOffset(ctx context.Context) (int32, error) {
	return s.readI32(ctx, proto.CmdVMeasureADCOffset, "adc offset")
}

// SetADCOffset writes the signed ADC count offset (command 26).
//
// A wrong offset makes every subsequent voltage reading silently wrong, which
// defeats the vlimit interlock that voltage writes depend on; SPEC.md §13.5 has
// the CLI confirm the write and print the previous values first, so the
// operator can put them back. (Confirmation, not a --force flag: §11, §13.)
func (s *Session) SetADCOffset(ctx context.Context, v int32) error {
	return s.writeI32(ctx, proto.CmdVMeasureADCOffset, v, "adc offset")
}

// ADCScale reads the voltage-measurement ADC count scale (command 27), also a
// signed int32. Its fixed-point interpretation is undetermined; 0 is the
// factory default and appears to mean "use the built-in calibration" rather
// than a literal zero multiplier (SPEC.md §14.11).
func (s *Session) ADCScale(ctx context.Context) (int32, error) {
	return s.readI32(ctx, proto.CmdVMeasureADCScale, "adc scale")
}

// SetADCScale writes the signed ADC count scale (command 27). See SetADCOffset
// for the safety note.
func (s *Session) SetADCScale(ctx context.Context, v int32) error {
	return s.writeI32(ctx, proto.CmdVMeasureADCScale, v, "adc scale")
}

func (s *Session) readI32(ctx context.Context, cmd proto.Cmd, what string) (int32, error) {
	f, err := s.Do(ctx, cmd, nil, false)
	if err != nil {
		return 0, err
	}
	v, err := proto.DecodeI32(f.Payload)
	if err != nil {
		return 0, fmt.Errorf("decode %s: %w", what, err)
	}
	return v, nil
}

func (s *Session) writeI32(ctx context.Context, cmd proto.Cmd, v int32, what string) error {
	if _, err := s.Do(ctx, cmd, proto.EncodeI32(v), true); err != nil {
		return fmt.Errorf("write %s %d: %w", what, v, err)
	}
	return nil
}

// Measure reads the output voltage measurement (command 28): raw ADC counts and
// the millivolt value the device computed from them.
//
// The calibration arithmetic lives in the firmware. There is no host-side
// formula anywhere in the vendor client, so calibratedMv must be taken as
// given, not recomputed.
func (s *Session) Measure(ctx context.Context) (rawADC, calibratedMv uint16, err error) {
	f, err := s.Do(ctx, proto.CmdVMeasure, nil, false)
	if err != nil {
		return 0, 0, err
	}
	rawADC, calibratedMv, err = proto.DecodeVMeasure(f.Payload)
	if err != nil {
		return 0, 0, fmt.Errorf("decode vmeasure: %w", err)
	}
	return rawADC, calibratedMv, nil
}

// ---------------------------------------------------------------------------
// LED (command 15). SPEC.md §6.2.
// ---------------------------------------------------------------------------

// LEDAlwaysOn reads the user-facing "LED Always On" setting (command 15).
//
// The wire sense is inverted -- the command is named
// DISABLE_LED_DURING_OPERATION, so 0 on the wire means always-on is enabled and
// 1 means the LED is suppressed. proto.DecodeLEDAlwaysOn owns that inversion;
// this returns the setting the way the user thinks about it. Note the
// suppression only applies to the solid-green Power Good state: every fault
// colour still lights (SPEC.md §1.1).
func (s *Session) LEDAlwaysOn(ctx context.Context) (bool, error) {
	f, err := s.Do(ctx, proto.CmdDisableLEDDuringOp, nil, false)
	if err != nil {
		return false, err
	}
	if len(f.Payload) < 1 {
		return false, fmt.Errorf("decode led setting: empty payload")
	}
	return proto.DecodeLEDAlwaysOn(f.Payload[0]), nil
}

// SetLEDAlwaysOn writes the user-facing "LED Always On" setting (command 15).
// on=true emits 03 8F 00; on=false emits 03 8F 01.
func (s *Session) SetLEDAlwaysOn(ctx context.Context, on bool) error {
	payload := []byte{proto.EncodeLEDAlwaysOn(on)}
	if _, err := s.Do(ctx, proto.CmdDisableLEDDuringOp, payload, true); err != nil {
		return fmt.Errorf("write led always-on=%t: %w", on, err)
	}
	return nil
}

// SetLEDColor drives the LED to a colour (command 13).
//
// Unlike SetLEDAlwaysOn, which changes a stored setting, this is an action. The
// colour latches in RAM until the next write and does not survive a power
// cycle: after a replug the unit shows its idle indication again, so a replug
// is the way back and this costs no flash wear (SPEC.md §6.2, measured §14.17).
//
// There is no read side. The device acknowledges with a bare two-byte frame
// carrying no payload, so nothing here reads the colour back and nothing can:
// the only way to confirm a colour is to look at the unit.
func (s *Session) SetLEDColor(ctx context.Context, c proto.LEDColor) error {
	if _, err := s.Do(ctx, proto.CmdFlashLEDSeqAdvanced, proto.LEDColorPayload(c), true); err != nil {
		return fmt.Errorf("write led colour %s: %w", c, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// AUTHLOCK (command 22). SPEC.md §6.3 -- read and write disagree.
// ---------------------------------------------------------------------------

// AuthLock reads the authorisation lock (command 22) and returns both an
// interpreted level and the untouched payload.
//
// This is the only asymmetric command in the protocol. A write puts the level
// in the *first* payload byte ([0x03, 0x96, level]), while the vendor's read
// parser takes device_data.authlock_level from frame[3] -- the *second*. Since
// getAuthLock has zero callers in the vendor client, that read had never run
// anywhere, and an off-by-one was as good an explanation as any. Hardware
// settled it (SPEC.md §14.8): `tx 02 16` answers `rx 04 16 16 00`, a two-byte
// payload of [command code echoed a second time, level]. payload[1] is the
// level and the vendor parser was right all along.
//
// raw is the whole payload, still returned because byte 0 is an echo nobody
// documented and a unit answering some other shape should be visible rather
// than silently reinterpreted. For the same reason a payload too short to hold
// the level is an error and not a guess: under the measured layout a lone byte
// is the echoed command code, so reading a level out of it would report 22.
//
// Only AUTH_LOCK_UNLOCKED = 0 is defined anywhere, and it is the only level
// ever seen on hardware. What other levels exist, what they gate, and how to
// unlock are all still unknown (SPEC.md §14.8).
func (s *Session) AuthLock(ctx context.Context) (level uint8, raw []byte, err error) {
	f, err := s.Do(ctx, proto.CmdAuthLock, nil, false)
	if err != nil {
		return 0, nil, err
	}
	raw = f.Payload // already a private copy, made by await
	if len(raw) < 2 {
		return 0, raw, fmt.Errorf("decode authlock: payload is %d byte(s), the measured layout is [command code, level]", len(raw))
	}
	return raw[1], raw, nil
}

// SetAuthLock writes an authorisation lock level (command 22), placing it in
// the first payload byte: [0x03, 0x96, level].
//
// Note the asymmetry with AuthLock. Only level 0 (proto.AuthLockUnlocked) has a
// documented meaning; the effect of any other value is unknown, which is why
// SPEC.md §13.4 has the CLI confirm a non-zero level and say outright that its
// effect is undocumented and possibly irreversible. The
// post-firmware-update sequence writes 0 before every other parameter, which
// suggests the lock gates the other writes -- inference, not proof.
func (s *Session) SetAuthLock(ctx context.Context, level uint8) error {
	if _, err := s.Do(ctx, proto.CmdAuthLock, []byte{level}, true); err != nil {
		return fmt.Errorf("write authlock level %d: %w", level, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Bootloader entry (command 20).
// ---------------------------------------------------------------------------

// JumpToBootloader sends 02 14 and returns without waiting.
//
// This command has no response by design: the device leaves application mode
// immediately, the USB device disappears and re-enumerates as a vendor-class
// (0xFF) bootloader interface with no MIDI at all. Success is proven by the
// disconnect, not by an ACK, so a caller should watch for the device going away
// within about 3 s and then wait the 4 s mode-switch delay (SPEC.md §10.1).
func (s *Session) JumpToBootloader(ctx context.Context) error {
	frame := proto.Read(proto.CmdJumpAppToBootloader) // 02 14
	if err := s.SendNoACK(ctx, frame); err != nil {
		return fmt.Errorf("jump to bootloader: %w", err)
	}
	return nil
}
