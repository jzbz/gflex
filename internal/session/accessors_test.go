package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
)

// TestExactTxFrames pins every typed accessor to the byte sequence it must put
// on the wire, cross-checked against the ready-made frames in SPEC.md §6.1 and
// the golden vectors in §15.
func TestExactTxFrames(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(d *fakeDev)
		call   func(ctx context.Context, s *Session) error
		wantTx []string
	}{
		{
			name:  "read serial",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdSerialNumber, []byte("VF001234")) },
			call: func(ctx context.Context, s *Session) error {
				_, err := s.SerialNumber(ctx)
				return err
			},
			wantTx: []string{"02 08"},
		},
		{
			name:  "read chip uuid",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdChipUUID, []byte("UUID0001")) },
			call: func(ctx context.Context, s *Session) error {
				_, err := s.ChipUUID(ctx)
				return err
			},
			wantTx: []string{"02 09"},
		},
		{
			name:  "read hardware id",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdHardwareID, []byte("HW000001")) },
			call: func(ctx context.Context, s *Session) error {
				_, err := s.HardwareID(ctx)
				return err
			},
			wantTx: []string{"02 0a"},
		},
		{
			name:  "read firmware version",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdFirmwareVersion, []byte("5.0.1\x00\x00\x00\x00\x00\x00\x00")) },
			call: func(ctx context.Context, s *Session) error {
				_, err := s.FirmwareVersion(ctx)
				return err
			},
			wantTx: []string{"02 0b"},
		},
		{
			name:  "read mfg date",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdMfgDate, []byte("20250101")) },
			call: func(ctx context.Context, s *Session) error {
				_, err := s.MfgDate(ctx)
				return err
			},
			wantTx: []string{"02 0c"},
		},
		{
			name:  "read led setting",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdDisableLEDDuringOp, []byte{0x00}) },
			call: func(ctx context.Context, s *Session) error {
				_, err := s.LEDAlwaysOn(ctx)
				return err
			},
			wantTx: []string{"02 0f"},
		},
		{
			// SPEC.md §15: frame 03 8F 00. Always-on is wire value 0.
			name:   "write led always on",
			setup:  func(d *fakeDev) { d.SetPayload(proto.CmdDisableLEDDuringOp, []byte{0x00}) },
			call:   func(ctx context.Context, s *Session) error { return s.SetLEDAlwaysOn(ctx, true) },
			wantTx: []string{"03 8f 00"},
		},
		{
			name:   "write led off when green",
			setup:  func(d *fakeDev) { d.SetPayload(proto.CmdDisableLEDDuringOp, []byte{0x01}) },
			call:   func(ctx context.Context, s *Session) error { return s.SetLEDAlwaysOn(ctx, false) },
			wantTx: []string{"03 8f 01"},
		},
		{
			name:   "clear pdo log",
			setup:  func(d *fakeDev) { d.SetPayload(proto.CmdPDOLog, nil) },
			call:   func(ctx context.Context, s *Session) error { return s.ClearPDOLog(ctx) },
			wantTx: []string{"02 91"},
		},
		{
			name:  "read pdo chunk",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdPDOLog, []byte{5, 1, 2, 3, 4, 5, 6, 7, 8}) },
			call: func(ctx context.Context, s *Session) error {
				_, _, err := s.PDOLogChunk(ctx, 5)
				return err
			},
			wantTx: []string{"03 11 05"},
		},
		{
			name:  "read voltage",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdVoltageMv, proto.EncodeU16(5000)) },
			call: func(ctx context.Context, s *Session) error {
				_, err := s.VoltageMv(ctx)
				return err
			},
			wantTx: []string{"02 12"},
		},
		{
			// SPEC.md §15: 12000 mV = 0x2EE0 -> 04 92 2E E0, then the explicit
			// read-back the vendor app also performs.
			name:  "write voltage 12V then read back",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdVoltageMv, proto.EncodeU16(12000)) },
			call: func(ctx context.Context, s *Session) error {
				got, err := s.SetVoltageMv(ctx, 12000)
				if err == nil && got != 12000 {
					t.Errorf("read-back = %d, want 12000", got)
				}
				return err
			},
			wantTx: []string{"04 92 2e e0", "02 12"},
		},
		{
			name:  "read current limit",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdCurrentLimitMa, proto.EncodeU16(5000)) },
			call: func(ctx context.Context, s *Session) error {
				_, err := s.CurrentLimitMa(ctx)
				return err
			},
			wantTx: []string{"02 13"},
		},
		{
			// SPEC.md §15: 5000 mA = 0x1388 -> 04 93 13 88.
			name:   "write current limit 5000 mA",
			setup:  func(d *fakeDev) { d.SetPayload(proto.CmdCurrentLimitMa, proto.EncodeU16(5000)) },
			call:   func(ctx context.Context, s *Session) error { return s.SetCurrentLimitMa(ctx, 5000) },
			wantTx: []string{"04 93 13 88"},
		},
		{
			// No response is scripted: the jump must not wait for one.
			name:   "jump to bootloader",
			setup:  func(d *fakeDev) {},
			call:   func(ctx context.Context, s *Session) error { return s.JumpToBootloader(ctx) },
			wantTx: []string{"02 14"},
		},
		{
			name:  "read authlock",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdAuthLock, []byte{0x01, 0x00}) },
			call: func(ctx context.Context, s *Session) error {
				_, _, err := s.AuthLock(ctx)
				return err
			},
			wantTx: []string{"02 16"},
		},
		{
			name:   "write authlock unlocked",
			setup:  func(d *fakeDev) { d.SetPayload(proto.CmdAuthLock, []byte{0x00}) },
			call:   func(ctx context.Context, s *Session) error { return s.SetAuthLock(ctx, proto.AuthLockUnlocked) },
			wantTx: []string{"03 96 00"},
		},
		{
			name:  "read vlimit",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdUserVLimit, proto.EncodeVLimit(3300, 48000)) },
			call: func(ctx context.Context, s *Session) error {
				_, _, err := s.VLimit(ctx)
				return err
			},
			wantTx: []string{"02 17"},
		},
		{
			// SPEC.md §15: low=3300 high=48000 -> 06 97 BB 80 0C E4. HIGH goes
			// on the wire first even though the API takes (low, high).
			name:   "write vlimit",
			setup:  func(d *fakeDev) { d.SetPayload(proto.CmdUserVLimit, proto.EncodeVLimit(3300, 48000)) },
			call:   func(ctx context.Context, s *Session) error { return s.SetVLimit(ctx, 3300, 48000) },
			wantTx: []string{"06 97 bb 80 0c e4"},
		},
		{
			name:  "read vtolerance nominal",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdVToleranceNominalMv, proto.EncodeU16(750)) },
			call: func(ctx context.Context, s *Session) error {
				_, err := s.VToleranceNominalMv(ctx)
				return err
			},
			wantTx: []string{"02 18"},
		},
		{
			// SPEC.md §6.1: write 750 mV -> 04 98 02 EE.
			name:   "write vtolerance nominal",
			setup:  func(d *fakeDev) { d.SetPayload(proto.CmdVToleranceNominalMv, proto.EncodeU16(750)) },
			call:   func(ctx context.Context, s *Session) error { return s.SetVToleranceNominalMv(ctx, 750) },
			wantTx: []string{"04 98 02 ee"},
		},
		{
			name:  "read vtolerance sag",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdVToleranceSagPerMa, proto.EncodeU16(0x1234)) },
			call: func(ctx context.Context, s *Session) error {
				_, err := s.VToleranceSagPerMa(ctx)
				return err
			},
			wantTx: []string{"02 19"},
		},
		{
			name:   "write vtolerance sag",
			setup:  func(d *fakeDev) { d.SetPayload(proto.CmdVToleranceSagPerMa, proto.EncodeU16(0x1234)) },
			call:   func(ctx context.Context, s *Session) error { return s.SetVToleranceSagPerMa(ctx, 0x1234) },
			wantTx: []string{"04 99 12 34"},
		},
		{
			name:  "read adc offset",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdVMeasureADCOffset, proto.EncodeI32(0)) },
			call: func(ctx context.Context, s *Session) error {
				_, err := s.ADCOffset(ctx)
				return err
			},
			wantTx: []string{"02 1a"},
		},
		{
			// SPEC.md §6.1: write 0 -> 06 9A 00 00 00 00.
			name:   "write adc offset zero",
			setup:  func(d *fakeDev) { d.SetPayload(proto.CmdVMeasureADCOffset, proto.EncodeI32(0)) },
			call:   func(ctx context.Context, s *Session) error { return s.SetADCOffset(ctx, 0) },
			wantTx: []string{"06 9a 00 00 00 00"},
		},
		{
			// Signed: -1 is two's complement FF FF FF FF, not a rejected value.
			name:   "write adc offset negative",
			setup:  func(d *fakeDev) { d.SetPayload(proto.CmdVMeasureADCOffset, proto.EncodeI32(-1)) },
			call:   func(ctx context.Context, s *Session) error { return s.SetADCOffset(ctx, -1) },
			wantTx: []string{"06 9a ff ff ff ff"},
		},
		{
			name:  "read adc scale",
			setup: func(d *fakeDev) { d.SetPayload(proto.CmdVMeasureADCScale, proto.EncodeI32(0)) },
			call: func(ctx context.Context, s *Session) error {
				_, err := s.ADCScale(ctx)
				return err
			},
			wantTx: []string{"02 1b"},
		},
		{
			name:   "write adc scale",
			setup:  func(d *fakeDev) { d.SetPayload(proto.CmdVMeasureADCScale, proto.EncodeI32(0)) },
			call:   func(ctx context.Context, s *Session) error { return s.SetADCScale(ctx, 0) },
			wantTx: []string{"06 9b 00 00 00 00"},
		},
		{
			name: "read vmeasure",
			setup: func(d *fakeDev) {
				d.SetPayload(proto.CmdVMeasure, []byte{0x04, 0xD2, 0x13, 0x88})
			},
			call: func(ctx context.Context, s *Session) error {
				_, _, err := s.Measure(ctx)
				return err
			},
			wantTx: []string{"02 1c"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, d := newTestSession(t, Options{Timeout: time.Second})
			tc.setup(d)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := tc.call(ctx, s); err != nil {
				t.Fatalf("call: %v", err)
			}

			got := d.SentHex()
			if len(got) != len(tc.wantTx) {
				t.Fatalf("tx frames = %q, want %q", got, tc.wantTx)
			}
			for i := range got {
				if got[i] != tc.wantTx[i] {
					t.Errorf("tx[%d] = %q, want %q", i, got[i], tc.wantTx[i])
				}
			}
		})
	}
}

// TestDecodeRoundTrip checks the value decoding of each accessor, especially
// the encodings that are easy to get backwards.
func TestDecodeRoundTrip(t *testing.T) {
	s, d := newTestSession(t, Options{})
	ctx := context.Background()

	d.SetPayload(proto.CmdSerialNumber, []byte("VF12345\x00"))
	d.SetPayload(proto.CmdFirmwareVersion, []byte(" 5.1.2 \x00\x00\x00\x00\x00"))
	d.SetPayload(proto.CmdVoltageMv, proto.EncodeU16(12000))
	d.SetPayload(proto.CmdCurrentLimitMa, proto.EncodeU16(5000))
	d.SetPayload(proto.CmdUserVLimit, proto.EncodeVLimit(3300, 48000))
	d.SetPayload(proto.CmdDisableLEDDuringOp, []byte{0x01}) // 1 = suppressed
	d.SetPayload(proto.CmdVMeasureADCOffset, proto.EncodeI32(-12345))
	d.SetPayload(proto.CmdVMeasureADCScale, proto.EncodeI32(2147483647))
	d.SetPayload(proto.CmdVMeasure, []byte{0x04, 0xD2, 0x2E, 0xE0})

	if got, err := s.SerialNumber(ctx); err != nil || got != "VF12345" {
		t.Errorf("SerialNumber = %q, %v; want \"VF12345\", nil", got, err)
	}
	if got, err := s.FirmwareVersion(ctx); err != nil || got != "5.1.2" {
		t.Errorf("FirmwareVersion = %q, %v; want \"5.1.2\", nil", got, err)
	}
	if got, err := s.VoltageMv(ctx); err != nil || got != 12000 {
		t.Errorf("VoltageMv = %d, %v; want 12000, nil", got, err)
	}
	if got, err := s.CurrentLimitMa(ctx); err != nil || got != 5000 {
		t.Errorf("CurrentLimitMa = %d, %v; want 5000, nil", got, err)
	}
	// The wire carries HIGH first; a decoder that reads in order would report
	// low=48000 high=3300 here.
	low, high, err := s.VLimit(ctx)
	if err != nil || low != 3300 || high != 48000 {
		t.Errorf("VLimit = %d, %d, %v; want 3300, 48000, nil", low, high, err)
	}
	// Wire 1 means "suppressed while green", i.e. always-on is false.
	if got, err := s.LEDAlwaysOn(ctx); err != nil || got {
		t.Errorf("LEDAlwaysOn = %t, %v; want false, nil", got, err)
	}
	if got, err := s.ADCOffset(ctx); err != nil || got != -12345 {
		t.Errorf("ADCOffset = %d, %v; want -12345, nil", got, err)
	}
	if got, err := s.ADCScale(ctx); err != nil || got != 2147483647 {
		t.Errorf("ADCScale = %d, %v; want 2147483647, nil", got, err)
	}
	raw, mv, err := s.Measure(ctx)
	if err != nil || raw != 1234 || mv != 12000 {
		t.Errorf("Measure = %d, %d, %v; want 1234, 12000, nil", raw, mv, err)
	}
}

// TestAuthLockReadsBothBytes covers the one asymmetric command: the vendor
// parser takes the level from payload offset 1, and that path has never run
// against hardware, so the raw payload must come back too (SPEC.md §6.3).
func TestAuthLockReadsBothBytes(t *testing.T) {
	t.Run("two byte payload uses offset 1", func(t *testing.T) {
		s, d := newTestSession(t, Options{})
		d.SetPayload(proto.CmdAuthLock, []byte{0x07, 0x02})

		level, raw, err := s.AuthLock(context.Background())
		if err != nil {
			t.Fatalf("AuthLock: %v", err)
		}
		if level != 0x02 {
			t.Errorf("level = %d, want 2 (payload[1], per the vendor parser)", level)
		}
		if len(raw) != 2 || raw[0] != 0x07 || raw[1] != 0x02 {
			t.Errorf("raw = % x, want 07 02", raw)
		}
	})

	t.Run("one byte payload falls back to offset 0", func(t *testing.T) {
		s, d := newTestSession(t, Options{})
		d.SetPayload(proto.CmdAuthLock, []byte{0x03})

		level, raw, err := s.AuthLock(context.Background())
		if err != nil {
			t.Fatalf("AuthLock: %v", err)
		}
		if level != 0x03 {
			t.Errorf("level = %d, want 3", level)
		}
		if len(raw) != 1 {
			t.Errorf("raw = % x, want one byte", raw)
		}
	})

	t.Run("empty payload is an error", func(t *testing.T) {
		s, d := newTestSession(t, Options{})
		d.SetPayload(proto.CmdAuthLock, nil)
		if _, _, err := s.AuthLock(context.Background()); err == nil {
			t.Fatal("want an error for an empty authlock payload")
		}
	})
}

// TestVoltageZeroMeansNotReady covers SPEC.md §6.5: a freshly attached unit
// answers 0 mV until it settles, and 0 must never be reported as 0 V.
func TestVoltageZeroMeansNotReady(t *testing.T) {
	t.Run("retries until non-zero", func(t *testing.T) {
		s, d := newTestSession(t, Options{})
		var calls int
		d.SetHandler(proto.CmdVoltageMv, func(proto.Frame) []byte {
			calls++
			if calls < 3 {
				return mustBuild(proto.CmdVoltageMv, proto.EncodeU16(0), false)
			}
			return mustBuild(proto.CmdVoltageMv, proto.EncodeU16(9000), false)
		})

		start := time.Now()
		got, err := s.VoltageMv(context.Background())
		if err != nil {
			t.Fatalf("VoltageMv: %v", err)
		}
		if got != 9000 {
			t.Errorf("VoltageMv = %d, want 9000", got)
		}
		if calls != 3 {
			t.Errorf("device saw %d reads, want 3", calls)
		}
		// The backoff after the first two not-ready answers is 100 ms then
		// 200 ms; the read itself is essentially free against the fake.
		if elapsed := time.Since(start); elapsed < 3*readyRetryInitialDelay {
			t.Errorf("elapsed %v, want at least the %v of backoff between three attempts",
				elapsed, 3*readyRetryInitialDelay)
		}
	})

	t.Run("always zero fails", func(t *testing.T) {
		s, d := newTestSession(t, Options{ReadyTimeout: 250 * time.Millisecond})
		d.SetPayload(proto.CmdVoltageMv, proto.EncodeU16(0))

		_, err := s.VoltageMv(context.Background())
		if err == nil {
			t.Fatal("want an error when the device never reports a non-zero voltage")
		}
		if !strings.Contains(err.Error(), "0 mV") {
			t.Errorf("error = %v, want it to mention the 0 mV not-ready reading", err)
		}
		if n := len(d.Sent()); n < 2 {
			t.Errorf("device saw %d reads, want the budget spent on several", n)
		}
	})

	t.Run("read back after write also retries", func(t *testing.T) {
		s, d := newTestSession(t, Options{})
		var reads int
		d.SetHandler(proto.CmdVoltageMv, func(f proto.Frame) []byte {
			if f.Write {
				return mustBuild(proto.CmdVoltageMv, f.Payload, false)
			}
			reads++
			if reads == 1 {
				return mustBuild(proto.CmdVoltageMv, proto.EncodeU16(0), false)
			}
			return mustBuild(proto.CmdVoltageMv, proto.EncodeU16(5000), false)
		})

		got, err := s.SetVoltageMv(context.Background(), 5000)
		if err != nil {
			t.Fatalf("SetVoltageMv: %v", err)
		}
		if got != 5000 {
			t.Errorf("SetVoltageMv = %d, want the read-back value 5000", got)
		}
	})
}

// TestSetVoltageReturnsReadBackNotEcho proves the write echo is not trusted:
// the device echoes the requested value but reports a different one on read.
func TestSetVoltageReturnsReadBackNotEcho(t *testing.T) {
	s, d := newTestSession(t, Options{})
	d.SetHandler(proto.CmdVoltageMv, func(f proto.Frame) []byte {
		if f.Write {
			return mustBuild(proto.CmdVoltageMv, f.Payload, false) // echo: 20000
		}
		return mustBuild(proto.CmdVoltageMv, proto.EncodeU16(19980), false)
	})

	got, err := s.SetVoltageMv(context.Background(), 20000)
	if err != nil {
		t.Fatalf("SetVoltageMv: %v", err)
	}
	if got != 19980 {
		t.Errorf("SetVoltageMv = %d, want the read-back 19980 rather than the echoed 20000", got)
	}
}

// TestVoltageReadyPersistence covers the connect-time persistence of SPEC.md §7.
//
// The vendor app keeps at a unit that answers 0 mV for roughly 25 s across two
// nested retry loops. Only the inner 3-attempt loop existed here, so a freshly
// plugged unit -- the very first thing a new user has -- failed `voltage get`,
// `info` and `voltage set` where the vendor app succeeds. The budget replaces
// the fixed attempt count, so what matters is that the retrying outlives three
// attempts and stops when the budget says so.
func TestVoltageReadyPersistence(t *testing.T) {
	t.Run("ready device pays nothing", func(t *testing.T) {
		// The fast path is the whole reason there is no upfront settle delay:
		// a device that answers immediately must cost ZERO extra time.
		s, d := newTestSession(t, Options{})
		d.SetPayload(proto.CmdVoltageMv, proto.EncodeU16(9000))

		start := time.Now()
		got, err := s.VoltageMv(context.Background())
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("VoltageMv: %v", err)
		}
		if got != 9000 {
			t.Errorf("VoltageMv = %d, want 9000", got)
		}
		if n := len(d.Sent()); n != 1 {
			t.Errorf("device saw %d reads, want exactly 1", n)
		}
		if elapsed >= readyRetryInitialDelay {
			t.Errorf("a ready device took %v; it must not pay any settle or backoff delay", elapsed)
		}
	})

	t.Run("outlasts the old three-attempt loop", func(t *testing.T) {
		s, d := newTestSession(t, Options{})
		var calls int
		d.SetHandler(proto.CmdVoltageMv, func(proto.Frame) []byte {
			calls++
			// Three not-ready answers: exactly where the old fixed loop gave up.
			if calls <= 3 {
				return mustBuild(proto.CmdVoltageMv, proto.EncodeU16(0), false)
			}
			return mustBuild(proto.CmdVoltageMv, proto.EncodeU16(12000), false)
		})

		got, err := s.VoltageMv(context.Background())
		if err != nil {
			t.Fatalf("VoltageMv: %v", err)
		}
		if got != 12000 {
			t.Errorf("VoltageMv = %d, want 12000", got)
		}
		if calls != 4 {
			t.Errorf("device saw %d reads, want 4 (the fourth is past the old 3-attempt cap)", calls)
		}
	})

	t.Run("voltage set survives a settling device", func(t *testing.T) {
		// The user-visible headline: `voltage set` on a just-plugged unit. The
		// write is acknowledged, then the read-back has to outlast the settle.
		s, d := newTestSession(t, Options{})
		var reads int
		d.SetHandler(proto.CmdVoltageMv, func(f proto.Frame) []byte {
			if f.Write {
				return mustBuild(proto.CmdVoltageMv, f.Payload, false)
			}
			reads++
			if reads <= 3 {
				return mustBuild(proto.CmdVoltageMv, proto.EncodeU16(0), false)
			}
			return mustBuild(proto.CmdVoltageMv, proto.EncodeU16(9000), false)
		})

		got, err := s.SetVoltageMv(context.Background(), 9000)
		if err != nil {
			t.Fatalf("SetVoltageMv: %v (the read-back must outlast the settle rather than report ErrReadBack)", err)
		}
		if got != 9000 {
			t.Errorf("SetVoltageMv = %d, want the read-back 9000", got)
		}
	})

	t.Run("budget bounds the persistence", func(t *testing.T) {
		const budget = 150 * time.Millisecond
		s, d := newTestSession(t, Options{ReadyTimeout: budget})
		d.SetPayload(proto.CmdVoltageMv, proto.EncodeU16(0))

		start := time.Now()
		_, err := s.VoltageMv(context.Background())
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("want an error once the ready budget is spent")
		}
		if elapsed < budget {
			t.Errorf("gave up after %v, want it to persist for the whole %v budget", elapsed, budget)
		}
		// Bounded: the last backoff is clipped to what is left of the budget,
		// so this must not run on to the 10 s default.
		if elapsed > budget+2*time.Second {
			t.Errorf("took %v, want the %v budget to bound it", elapsed, budget)
		}
		if n := len(d.Sent()); n < 2 {
			t.Errorf("device saw %d reads in %v, want several", n, budget)
		}
	})

	t.Run("cancellation is reported as cancellation", func(t *testing.T) {
		s, d := newTestSession(t, Options{})
		d.SetPayload(proto.CmdVoltageMv, proto.EncodeU16(0))

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		_, err := s.VoltageMv(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled rather than a spent-budget message", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("took %v; cancellation must not wait out the ready budget", elapsed)
		}
	})
}

// TestReadyTimeoutDefault pins the Options substitution: zero means the
// documented default, not "no retrying at all".
func TestReadyTimeoutDefault(t *testing.T) {
	s, _ := newTestSession(t, Options{})
	if s.readyTimeout != DefaultReadyTimeout {
		t.Errorf("readyTimeout = %v, want %v", s.readyTimeout, DefaultReadyTimeout)
	}
	s2, _ := newTestSession(t, Options{ReadyTimeout: 25 * time.Millisecond})
	if s2.readyTimeout != 25*time.Millisecond {
		t.Errorf("readyTimeout = %v, want the configured 25ms", s2.readyTimeout)
	}
}

// TestVLimitPlausible covers the vendor's own validity rule (SPEC.md §6.5).
//
// This is the canonical copy of the predicate -- internal/cli imports it rather
// than restating the thresholds, because a second copy is how the safety bug it
// was once misused for comes back. What it answers is "did a flash erase this
// window?", nothing else; see the doc comment and the [3300, 5000] case below.
func TestVLimitPlausible(t *testing.T) {
	cases := []struct {
		low, high uint16
		want      bool
	}{
		{3300, 48000, true},
		{3000, 6000, true},
		{2999, 48000, false}, // low below 3000
		{3300, 5999, false},  // high below 6000
		{48000, 48000, false},
		{48000, 3300, false}, // high must exceed low
		{0, 0, false},
		// A deliberate 5 V ceiling, e.g. someone protecting a 5 V pedal. It is
		// false here -- the shape test only means "the vendor would rewrite
		// this to its defaults after a flash". It is a perfectly usable bound,
		// and anything that reads this false as "do not trust the window" and
		// falls back to the 48 V envelope has reintroduced the bug this
		// predicate was once the vehicle for (SPEC.md §13.1).
		{3300, 5000, false},
	}
	for _, tc := range cases {
		if got := VLimitPlausible(tc.low, tc.high); got != tc.want {
			t.Errorf("VLimitPlausible(%d, %d) = %t, want %t", tc.low, tc.high, got, tc.want)
		}
	}
}

// TestWriteEchoWithFlagBitsMatches: the device may or may not echo the write
// flag in the response command byte (SPEC.md §14.13). Either way the code is
// masked with 0x3F before comparison, so both must match.
func TestWriteEchoWithFlagBitsMatches(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: 500 * time.Millisecond})
	d.SetHandler(proto.CmdCurrentLimitMa, func(f proto.Frame) []byte {
		// Respond with both flag bits set.
		resp, err := proto.Build(proto.CmdCurrentLimitMa, f.Payload, true, true)
		if err != nil {
			t.Error(err)
			return nil
		}
		return resp
	})

	if err := s.SetCurrentLimitMa(context.Background(), 5000); err != nil {
		t.Fatalf("SetCurrentLimitMa: %v", err)
	}
}
