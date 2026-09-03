package session

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jzbz/gflex/internal/ctxwait"
	"github.com/jzbz/gflex/internal/proto"
)

// Info reads the device's state in one pass.
//
// The always-read set is exactly what the vendor app issues in normal
// operation: serial number, firmware version, voltage, current limit, the
// vlimit pair and the LED setting. A failure in any of those fails the call,
// because a unit that cannot answer them is not usable.
//
// includeUnused additionally reads chip UUID, hardware id, manufacturing date,
// the authorisation lock, both tolerance terms, the ADC calibration pair and
// the voltage measurement. The vendor application never issues *any* of those
// commands (SPEC.md §6, "Used" column), but the firmware answers them: bring-up
// recorded the three identity strings, the authorisation lock, both ADC terms
// and the measurement, and read the sag tolerance as 0 on a factory unit
// (SPEC.md §14 and its corrections block; only the nominal tolerance has no
// recorded reading). That is one unit and one firmware revision, so each read
// stays best-effort: a failure leaves the corresponding field nil instead of
// failing the whole call, and a nil field means "not read", never "zero".
//
// Best-effort stops where no further read could succeed either. A cancelled
// context or any other failure PermanentErr names ends the call with an error
// rather than with a report full of holes; see the loop below.
func (s *Session) Info(ctx context.Context, includeUnused bool) (*proto.DeviceInfo, error) {
	info := &proto.DeviceInfo{}

	serial, err := s.SerialNumber(ctx)
	if err != nil {
		return nil, fmt.Errorf("read serial number: %w", err)
	}
	info.SerialNum = serial

	fw, err := s.FirmwareVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("read firmware version: %w", err)
	}
	info.FirmwareID = fw

	mv, err := s.VoltageMv(ctx)
	if err != nil {
		return nil, err // VoltageMv already names itself
	}
	info.VoltageMv = &mv

	ma, err := s.CurrentLimitMa(ctx)
	if err != nil {
		return nil, fmt.Errorf("read current limit: %w", err)
	}
	info.CurrentLimitMa = &ma

	low, high, err := s.VLimit(ctx)
	if err != nil {
		return nil, fmt.Errorf("read vlimit: %w", err)
	}
	info.VLimitLowMv, info.VLimitHighMv = &low, &high

	led, err := s.LEDAlwaysOn(ctx)
	if err != nil {
		return nil, fmt.Errorf("read led setting: %w", err)
	}
	info.LEDAlwaysOn = &led

	if !includeUnused {
		return info, nil
	}

	// Everything below is best-effort; see the doc comment. Each step leaves its
	// field nil when the firmware declines to answer.
	optional := []func() error{
		func() error {
			v, err := s.ChipUUID(ctx)
			if err != nil {
				return err
			}
			info.UUID = v
			return nil
		},
		func() error {
			v, err := s.HardwareID(ctx)
			if err != nil {
				return err
			}
			info.HardwareID = v
			return nil
		},
		func() error {
			v, err := s.MfgDate(ctx)
			if err != nil {
				return err
			}
			info.MfgDate = v
			return nil
		},
		func() error {
			level, raw, err := s.AuthLock(ctx)
			if err != nil {
				return err
			}
			info.AuthLockLevel, info.AuthLockRaw = &level, raw
			return nil
		},
		func() error {
			v, err := s.VToleranceNominalMv(ctx)
			if err != nil {
				return err
			}
			info.VToleranceNominalMv = &v
			return nil
		},
		func() error {
			v, err := s.VToleranceSagPerMa(ctx)
			if err != nil {
				return err
			}
			info.VToleranceSagPerMa = &v
			return nil
		},
		func() error {
			v, err := s.ADCOffset(ctx)
			if err != nil {
				return err
			}
			info.VMeasureADCOffset = &v
			return nil
		},
		func() error {
			v, err := s.ADCScale(ctx)
			if err != nil {
				return err
			}
			info.VMeasureADCScale = &v
			return nil
		},
		func() error {
			raw, calibrated, err := s.Measure(ctx)
			if err != nil {
				return err
			}
			info.VMeasureRawADC, info.VMeasureCalibratedMv = &raw, &calibrated
			return nil
		},
	}

	for _, read := range optional {
		err := read()
		if err == nil {
			continue
		}
		// Tolerating a failure is right for a command the firmware may not
		// implement; it is wrong for a cancelled context. ctx.Err() != nil means
		// the operator interrupted the run or its deadline expired -- not that
		// this particular command is unsupported -- and every remaining read
		// would fail identically. Swallowing it would print a report full of
		// missing fields and exit 0, which reads as a device that answered
		// nothing rather than as an aborted run.
		//
		// Checking only after a failure is sufficient, including for a context
		// that ends BETWEEN two reads: every command reaches the wire through
		// framer.SendFrame, which tests ctx.Err() before it writes the first
		// MIDI message of the frame, so once the context is dead the next read
		// cannot succeed and cannot even be transmitted. The failure it returns
		// is what this check then converts into the abort.
		// TestInfoCancellationBetweenReadsAbortsTheCall pins that composition.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("read device info: %w", ctxErr)
		}
		// The same argument covers the rest of what this package classifies as
		// beyond recovery. A device unplugged part-way through these nine reads
		// fails every remaining one instantly and identically, so tolerating the
		// first would print exactly the report an operator cannot tell apart
		// from a unit that simply declines the unused commands -- and exit 0
		// while doing it. PermanentErr is the classifier the sibling loops
		// already use (VoltageMv's ready retry, FullPDOLog's chunk retry), and
		// it deliberately excludes ErrTimeout, which is precisely how firmware
		// that does not implement one of these commands declines it. So the
		// tolerance this block exists for is untouched.
		if PermanentErr(err) {
			return nil, fmt.Errorf("read device info: %w", err)
		}
	}

	return info, nil
}

// How hard the §10.4 replay tries to read the existing vlimit window before it
// concludes it cannot be read.
//
// Every other read in this package is asked once, because a caller can simply
// ask again. This one cannot: what it decides is whether the user's window
// survives the flash or is replaced by the 3300/48000 default, and the protocol
// has no NACK, so a single frame dropped in either direction (SPEC.md §5.2)
// looks exactly like a device that has nothing to report. Three attempts 300 ms
// apart is the vendor's own inner read-retry shape (SPEC.md §7), and it is paid
// only by a unit that is already failing to answer after a flash.
const (
	postUpdateVLimitAttempts   = 3
	postUpdateVLimitRetryDelay = 300 * time.Millisecond
)

// readVLimitRetrying reads the user vlimit window, retrying a read that fails
// for a reason waiting can cure. A permanent failure -- the transport dead, the
// session closed, the context ended -- returns at once: the Session has no
// reconnect path, so the remaining attempts would fail identically.
func (s *Session) readVLimitRetrying(ctx context.Context, log func(string)) (low, high uint16, err error) {
	for attempt := 1; ; attempt++ {
		low, high, err = s.VLimit(ctx)
		if err == nil || attempt >= postUpdateVLimitAttempts || PermanentErr(err) {
			return low, high, err
		}
		// Not the "<step> failed: <err>" shape: an attempt that will be made
		// again is progress being narrated, not a setting left unrestored.
		log(fmt.Sprintf("read vlimit attempt %d of %d did not answer, retrying: %v", attempt, postUpdateVLimitAttempts, err))
		if werr := ctxwait.Sleep(ctx, postUpdateVLimitRetryDelay); werr != nil {
			return 0, 0, err
		}
	}
}

// PostUpdateInit replays the settings a firmware flash erases (SPEC.md §10.4).
//
// It is equivalent to PostUpdateInitForce(ctx, false, log): the vlimit pair is
// rewritten only when the value read back is implausible. A window that could
// not be read at all is left as the flash left it, and reported; see the read
// step there for why the two are not the same thing.
func (s *Session) PostUpdateInit(ctx context.Context, log func(string)) error {
	return s.PostUpdateInitForce(ctx, false, log)
}

// PostUpdateInitForce is PostUpdateInit with control over the vlimit rewrite.
//
// The order below is the vendor's and is reproduced exactly. Writing authlock 0
// first is load-bearing by inference: the lock plausibly gates the other
// writes, though nothing proves it.
//
//	getVLimit()
//	setAuthLock(0)
//	setVLimit(3300, 48000)      // only when the read-back was implausible
//	setVToleranceNominal(750)
//	setVMeasureAdcOffset(0)
//	setVMeasureAdcScale(0)
//	setMaxCurrentMa(5000)
//
// Every step is independently error-tolerant: a failure is reported through log
// and the sequence continues, because a partially restored unit is better than
// one abandoned at the first hiccup. Only a cancelled or expired context stops
// the run and produces a non-nil error, so callers must watch log for per-step
// failures rather than the return value alone.
//
// forceVLimit rewrites the vlimit pair unconditionally. The vendor also does
// this on a major-version jump from 4 to 5; that decision needs the before and
// after firmware versions, which this layer does not have, so the flashing
// caller passes it in.
func (s *Session) PostUpdateInitForce(ctx context.Context, forceVLimit bool, log func(string)) error {
	if log == nil {
		log = func(string) {}
	}

	// 1. Read the existing window first, so we can tell whether the flash left
	//    it in a usable state.
	rewriteVLimit := forceVLimit
	low, high, err := s.readVLimitRetrying(ctx, log)
	switch {
	case err != nil:
		// Unreadable is not the same as erased, and the vendor's own sequence
		// conflates them: SPEC.md §10.4 rewrites the defaults "only if the
		// read-back was invalid", counting a read that went unanswered as
		// invalid. That is the reasoning SPEC.md §17's first row already
		// refuses for `voltage set` -- the protocol has no NACK, so a dropped
		// frame is routine (§5.2), and acting on one here would replace a 5 V
		// ceiling with the 48 V default, which is the direction that ends with
		// 20 V in a 5 V pedal. After the retries above, the honest answer is
		// that the window could not be checked, so it is left exactly as the
		// flash left it.
		//
		// That is safe in the case this gives up on: a window an erase really
		// did wipe cannot bound anything, so the next `voltage set` refuses it
		// outright rather than falling back to the envelope (§13.1, §17 row 1).
		// The wording is the "<step> failed: <err>" shape the flashing caller
		// counts as a step that did not take, so the user is told to check the
		// window by hand instead of being left to assume it was restored.
		log(fmt.Sprintf("read vlimit failed: %v", err))
	case !VLimitPlausible(low, high):
		// Two different states arrive here and only one of them is an erased
		// window. VLimitPlausible is the vendor's erased-window test and
		// nothing else (SPEC.md §17, row 2); its 6000 mV floor rejects every
		// ceiling under 6 V, including [3300, 5000] -- what someone protecting
		// a 5 V pedal sets, and the strictest guard rail in the system. A pair
		// like that did not come out of an erase: it still bounds a request, so
		// it survived the flash and is about to be replaced by the widest
		// window there is.
		//
		// The rewrite still happens -- it is what §10.4 prescribes, and this
		// sequence restores every other setting to its default the same way --
		// but it must not be narrated as if the user's own window had been put
		// back. Widening a guard rail is the one change SPEC.md §13.3 makes
		// `vlimit set` confirm, so the least this can do is say plainly that it
		// happened and print the line that undoes it.
		if high > low {
			log(fmt.Sprintf("vlimit low=%d high=%d survived the flash but does not pass the vendor's erased-window test; WIDENING it to %d/%d -- put your own window back with: gflex vlimit set --low %dmV --high %dmV",
				low, high, proto.DefaultVLimitLowMv, proto.DefaultVLimitHighMv, low, high))
		} else {
			log(fmt.Sprintf("vlimit read back implausible (low=%d high=%d), rewriting defaults", low, high))
		}
		rewriteVLimit = true
	default:
		log(fmt.Sprintf("vlimit low=%d high=%d", low, high))
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("post-update init: %w", err)
	}

	// 2. Unlock before anything else.
	step := func(what string, fn func() error) bool {
		if err := fn(); err != nil {
			log(fmt.Sprintf("%s failed: %v", what, err))
			return false
		}
		log(what + " ok")
		return true
	}

	step("set authlock 0", func() error {
		return s.SetAuthLock(ctx, proto.AuthLockUnlocked)
	})
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("post-update init: %w", err)
	}

	// 3. Restore the guard-rail window if it needs it.
	if rewriteVLimit {
		step(fmt.Sprintf("set vlimit %d/%d", proto.DefaultVLimitLowMv, proto.DefaultVLimitHighMv), func() error {
			return s.SetVLimit(ctx, proto.DefaultVLimitLowMv, proto.DefaultVLimitHighMv)
		})
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("post-update init: %w", err)
		}
	}

	// 4-7. The remaining defaults, in order.
	step(fmt.Sprintf("set vtolerance nominal %d mV", proto.DefaultVToleranceNominal), func() error {
		return s.SetVToleranceNominalMv(ctx, proto.DefaultVToleranceNominal)
	})
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("post-update init: %w", err)
	}

	step(fmt.Sprintf("set adc offset %d", proto.DefaultADCOffset), func() error {
		return s.SetADCOffset(ctx, proto.DefaultADCOffset)
	})
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("post-update init: %w", err)
	}

	step(fmt.Sprintf("set adc scale %d", proto.DefaultADCScale), func() error {
		return s.SetADCScale(ctx, proto.DefaultADCScale)
	})
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("post-update init: %w", err)
	}

	step(fmt.Sprintf("set current limit %d mA", proto.DefaultCurrentLimitMa), func() error {
		return s.SetCurrentLimitMa(ctx, proto.DefaultCurrentLimitMa)
	})

	return ctx.Err()
}

// FirmwareAtLeast reports whether the connected unit's firmware is at least
// major.minor.patch, and returns the raw version string it compared.
//
// The power-supply scan is hard-gated on firmware >= 5.0.0 (SPEC.md §9); this
// is the check that gate should use.
func (s *Session) FirmwareAtLeast(ctx context.Context, major, minor, patch int) (bool, string, error) {
	v, err := s.FirmwareVersion(ctx)
	if err != nil {
		return false, "", fmt.Errorf("read firmware version: %w", err)
	}
	return VersionAtLeast(v, major, minor, patch), v, nil
}

// VersionAtLeast reports whether a device version string is at least
// major.minor.patch, using the vendor's comparison (SPEC.md §10.3).
func VersionAtLeast(version string, major, minor, patch int) bool {
	return CompareVersionComponents(VersionComponents(version), []int{major, minor, patch}) >= 0
}

// VersionComponents extracts the numeric components of a version string.
//
// The vendor's algorithm is: uppercase, trim, then take every run of decimal
// digits in order. That deliberately tolerates the shapes a device might
// report -- "5.0.0", "v5.0", "FW 5.0.0-rc1" all reduce to their numbers -- and
// non-numeric segments simply contribute nothing. Uppercasing is retained from
// the original even though extracting digits makes it a no-op here; it matters
// only in the vendor's separate "X or * means an update is available" branch,
// which belongs to the update checker rather than to this comparison.
//
// A digit run too long to fit in an int is clamped to the maximum rather than
// discarded, so an absurd version still compares as newer instead of older.
func VersionComponents(version string) []int {
	v := strings.ToUpper(strings.TrimSpace(version))
	var out []int
	for i := 0; i < len(v); {
		if v[i] < '0' || v[i] > '9' {
			i++
			continue
		}
		j := i
		for j < len(v) && v[j] >= '0' && v[j] <= '9' {
			j++
		}
		n, err := strconv.Atoi(v[i:j])
		if err != nil {
			n = int(^uint(0) >> 1) // overflowed: treat as arbitrarily new
		}
		out = append(out, n)
		i = j
	}
	return out
}

// CompareVersionComponents compares two component lists element-wise, treating
// missing components as 0 so that "5" and "5.0.0" are equal. It returns -1, 0
// or 1 in the manner of strings.Compare.
func CompareVersionComponents(a, b []int) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	at := func(s []int, i int) int {
		if i < len(s) {
			return s[i]
		}
		return 0
	}
	for i := 0; i < n; i++ {
		switch av, bv := at(a, i), at(b, i); {
		case av < bv:
			return -1
		case av > bv:
			return 1
		}
	}
	return 0
}
