package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/transport/fake"
)

// scriptCore installs answers for the commands the vendor app actually issues.
func scriptCore(d *fake.Device) {
	d.SetResponse(proto.CmdSerialNumber, []byte("VF000042"))
	d.SetResponse(proto.CmdFirmwareVersion, []byte("5.0.0\x00\x00\x00\x00\x00\x00\x00"))
	d.SetResponse(proto.CmdVoltageMv, proto.EncodeU16(9000))
	d.SetResponse(proto.CmdCurrentLimitMa, proto.EncodeU16(5000))
	d.SetResponse(proto.CmdUserVLimit, proto.EncodeVLimit(3300, 48000))
	d.SetResponse(proto.CmdDisableLEDDuringOp, []byte{0x00})
}

func TestInfoCoreOnly(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: time.Second})
	scriptCore(d)

	info, err := s.Info(context.Background(), false)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.SerialNum != "VF000042" || info.FirmwareID != "5.0.0" {
		t.Errorf("identity = %q / %q", info.SerialNum, info.FirmwareID)
	}
	if info.VoltageMv == nil || *info.VoltageMv != 9000 {
		t.Errorf("voltage = %v, want 9000", info.VoltageMv)
	}
	if info.CurrentLimitMa == nil || *info.CurrentLimitMa != 5000 {
		t.Errorf("current limit = %v, want 5000", info.CurrentLimitMa)
	}
	if info.VLimitLowMv == nil || *info.VLimitLowMv != 3300 ||
		info.VLimitHighMv == nil || *info.VLimitHighMv != 48000 {
		t.Errorf("vlimit = %v / %v, want 3300 / 48000", info.VLimitLowMv, info.VLimitHighMv)
	}
	if info.LEDAlwaysOn == nil || !*info.LEDAlwaysOn {
		t.Errorf("led always on = %v, want true", info.LEDAlwaysOn)
	}
	// Nothing outside the core set may have been read.
	if info.UUID != "" || info.HardwareID != "" || info.MfgDate != "" ||
		info.AuthLockLevel != nil || info.VToleranceNominalMv != nil ||
		info.VToleranceSagPerMa != nil || info.VMeasureADCOffset != nil ||
		info.VMeasureADCScale != nil || info.VMeasureRawADC != nil {
		t.Errorf("unused-command fields populated without includeUnused: %+v", info)
	}
	if n := len(d.Sent()); n != 6 {
		t.Errorf("sent %d frames, want 6", n)
	}
}

// TestInfoIncludeUnusedToleratesFailure: none of the extra commands is ever
// issued by the vendor app, so a silent unit must leave those fields nil rather
// than fail the whole call.
func TestInfoIncludeUnusedToleratesFailure(t *testing.T) {
	// A short timeout keeps the nine unanswered reads quick.
	s, d := newTestSession(t, Options{Timeout: 20 * time.Millisecond})
	scriptCore(d)
	// Answer just one of the extras, to prove the others' failure is isolated.
	d.SetResponse(proto.CmdVToleranceNominalMv, proto.EncodeU16(750))

	info, err := s.Info(context.Background(), true)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.VToleranceNominalMv == nil || *info.VToleranceNominalMv != 750 {
		t.Errorf("vtolerance nominal = %v, want 750", info.VToleranceNominalMv)
	}
	if info.UUID != "" || info.HardwareID != "" || info.MfgDate != "" {
		t.Errorf("unanswered identity fields should stay empty: %+v", info)
	}
	if info.AuthLockLevel != nil || info.VMeasureADCOffset != nil ||
		info.VMeasureADCScale != nil || info.VMeasureRawADC != nil ||
		info.VToleranceSagPerMa != nil {
		t.Errorf("unanswered fields should stay nil: %+v", info)
	}
	if info.SerialNum == "" {
		t.Error("core fields must still be populated")
	}
}

func TestInfoIncludeUnusedFull(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: time.Second})
	scriptCore(d)
	d.SetResponse(proto.CmdChipUUID, []byte("CHIP0001"))
	d.SetResponse(proto.CmdHardwareID, []byte("HW000001"))
	d.SetResponse(proto.CmdMfgDate, []byte("20250101"))
	d.SetResponse(proto.CmdAuthLock, []byte{0x01, 0x00})
	d.SetResponse(proto.CmdVToleranceNominalMv, proto.EncodeU16(750))
	d.SetResponse(proto.CmdVToleranceSagPerMa, proto.EncodeU16(4))
	d.SetResponse(proto.CmdVMeasureADCOffset, proto.EncodeI32(-3))
	d.SetResponse(proto.CmdVMeasureADCScale, proto.EncodeI32(1024))
	d.SetResponse(proto.CmdVMeasure, []byte{0x04, 0xD2, 0x23, 0x28})

	info, err := s.Info(context.Background(), true)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.UUID != "CHIP0001" || info.HardwareID != "HW000001" || info.MfgDate != "20250101" {
		t.Errorf("extra identity = %+v", info)
	}
	if info.AuthLockLevel == nil || *info.AuthLockLevel != 0 || len(info.AuthLockRaw) != 2 {
		t.Errorf("authlock = %v raw=% x, want level 0 and both bytes", info.AuthLockLevel, info.AuthLockRaw)
	}
	if info.VMeasureADCOffset == nil || *info.VMeasureADCOffset != -3 {
		t.Errorf("adc offset = %v, want -3", info.VMeasureADCOffset)
	}
	if info.VMeasureRawADC == nil || *info.VMeasureRawADC != 1234 ||
		info.VMeasureCalibratedMv == nil || *info.VMeasureCalibratedMv != 9000 {
		t.Errorf("vmeasure = %v / %v", info.VMeasureRawADC, info.VMeasureCalibratedMv)
	}
}

// TestInfoCancellationAbortsTheCall: the best-effort block tolerates a command
// the firmware declines, but a cancelled context is not that. Ctrl-C during
// `gflex info --all` used to return a partial report and exit 0, which reads as
// a device that answered nothing rather than as an aborted run.
func TestInfoCancellationAbortsTheCall(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: 20 * time.Millisecond})
	scriptCore(d)
	// The extras are answered, so nothing but the cancellation can stop the run.
	d.SetResponse(proto.CmdChipUUID, []byte("CHIP0001"))
	d.SetResponse(proto.CmdHardwareID, []byte("HW000001"))
	d.SetResponse(proto.CmdMfgDate, []byte("20250101"))
	d.SetResponse(proto.CmdAuthLock, []byte{0x01, 0x00})
	d.SetResponse(proto.CmdVToleranceNominalMv, proto.EncodeU16(750))
	d.SetResponse(proto.CmdVToleranceSagPerMa, proto.EncodeU16(4))
	d.SetResponse(proto.CmdVMeasureADCOffset, proto.EncodeI32(0))
	d.SetResponse(proto.CmdVMeasureADCScale, proto.EncodeI32(0))
	d.SetResponse(proto.CmdVMeasure, []byte{0x04, 0xD2, 0x23, 0x28})

	// Cancel as soon as the first best-effort read goes out, so the remaining
	// reads are the ones that must not be silently skipped.
	ctx, cancel := context.WithCancel(context.Background())
	d.SetHandler(proto.CmdChipUUID, func(proto.Frame) []byte {
		cancel()
		return nil
	})

	info, err := s.Info(ctx, true)
	if err == nil {
		t.Fatalf("Info returned a partial report and no error: %+v", info)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	if info != nil {
		t.Errorf("info = %+v, want nil alongside the error", info)
	}
}

// TestInfoCancellationBetweenReadsAbortsTheCall covers the gap *between* two
// best-effort reads, which TestInfoCancellationAbortsTheCall above does not:
// there the cancelled read itself fails, which is the easy case for a check
// made only after a failure. Here the first optional read is cancelled after
// its answer has already arrived, so it succeeds and leaves that check nothing
// to fire on.
//
// Info aborts anyway, and this pins the composition that makes it do so rather
// than Info alone: every command reaches the wire through framer.SendFrame,
// which tests the context before writing the first MIDI message of a frame, so
// the next read cannot succeed once the context is dead -- it cannot even be
// transmitted, which is what the frame count checks -- and the failure it
// returns is what Info's post-read check turns into the abort. If SendFrame
// ever stops refusing on a dead context, this test fails and Info needs a
// check of its own at the head of the loop.
func TestInfoCancellationBetweenReadsAbortsTheCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once

	// Cancelling from the trace hook lands the cancellation after the chip UUID
	// response has been received but before the read returns: the read succeeds
	// and the context is dead by the time the loop comes round again.
	s, d := newTestSession(t, Options{
		Timeout: 20 * time.Millisecond,
		Trace: func(dir string, frame []byte) {
			if dir != "rx" {
				return
			}
			if f, err := proto.Parse(frame); err == nil && f.Cmd == proto.CmdChipUUID {
				once.Do(cancel)
			}
		},
	})
	scriptCore(d)
	// Every optional command answers, so only the cancellation can stop the run.
	d.SetResponse(proto.CmdChipUUID, []byte("CHIP0001"))
	d.SetResponse(proto.CmdHardwareID, []byte("HW000001"))
	d.SetResponse(proto.CmdMfgDate, []byte("20250101"))
	d.SetResponse(proto.CmdAuthLock, []byte{0x01, 0x00})
	d.SetResponse(proto.CmdVToleranceNominalMv, proto.EncodeU16(750))
	d.SetResponse(proto.CmdVToleranceSagPerMa, proto.EncodeU16(4))
	d.SetResponse(proto.CmdVMeasureADCOffset, proto.EncodeI32(0))
	d.SetResponse(proto.CmdVMeasureADCScale, proto.EncodeI32(0))
	d.SetResponse(proto.CmdVMeasure, []byte{0x04, 0xD2, 0x23, 0x28})

	info, err := s.Info(ctx, true)
	if err == nil {
		t.Fatalf("Info reported success for a cancelled run: %+v", info)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	if info != nil {
		t.Errorf("info = %+v, want nil alongside the error", info)
	}
	// Six core reads plus the one optional read that was in flight when the
	// context ended. Anything more is a command issued after cancellation.
	if n := len(d.Sent()); n != 7 {
		t.Errorf("sent %d frames, want 7 (6 core + chip uuid); the run continued past the cancellation", n)
	}
	// ... and the last of them is the chip UUID read, so the run really did stop
	// at the cancellation rather than somewhere earlier.
	if sent := d.SentHex(); len(sent) == 0 || sent[len(sent)-1] != "02 09" {
		t.Errorf("tx = %q, want the chip uuid read (02 09) last", sent)
	}
}

func TestInfoFailsOnCoreCommand(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: 20 * time.Millisecond})
	scriptCore(d)
	d.SetHandler(proto.CmdSerialNumber, func(proto.Frame) []byte { return nil })

	if _, err := s.Info(context.Background(), false); err == nil {
		t.Fatal("want an error when the serial number cannot be read")
	}
}

// TestPostUpdateInitOrder pins the replay sequence of SPEC.md §10.4.
func TestPostUpdateInitOrder(t *testing.T) {
	t.Run("plausible vlimit is left alone", func(t *testing.T) {
		s, d := newTestSession(t, Options{Timeout: time.Second})
		d.SetResponse(proto.CmdUserVLimit, proto.EncodeVLimit(3300, 48000))
		d.SetResponse(proto.CmdAuthLock, []byte{0x00})
		d.SetResponse(proto.CmdVToleranceNominalMv, proto.EncodeU16(750))
		d.SetResponse(proto.CmdVMeasureADCOffset, proto.EncodeI32(0))
		d.SetResponse(proto.CmdVMeasureADCScale, proto.EncodeI32(0))
		d.SetResponse(proto.CmdCurrentLimitMa, proto.EncodeU16(5000))

		var logged []string
		if err := s.PostUpdateInit(context.Background(), func(m string) { logged = append(logged, m) }); err != nil {
			t.Fatalf("PostUpdateInit: %v", err)
		}
		want := []string{
			"02 17",             // read vlimit first
			"03 96 00",          // unlock before anything else
			"04 98 02 ee",       // vtolerance nominal 750
			"06 9a 00 00 00 00", // adc offset 0
			"06 9b 00 00 00 00", // adc scale 0
			"04 93 13 88",       // current limit 5000
		}
		got := d.SentHex()
		if len(got) != len(want) {
			t.Fatalf("tx = %q, want %q", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("tx[%d] = %q, want %q", i, got[i], want[i])
			}
		}
		if len(logged) == 0 {
			t.Error("want progress reported through the log callback")
		}
	})

	t.Run("implausible vlimit is rewritten", func(t *testing.T) {
		s, d := newTestSession(t, Options{Timeout: time.Second})
		// A flash left the window at 0/0.
		d.SetResponse(proto.CmdUserVLimit, proto.EncodeVLimit(0, 0))
		d.SetResponse(proto.CmdAuthLock, []byte{0x00})
		d.SetResponse(proto.CmdVToleranceNominalMv, proto.EncodeU16(750))
		d.SetResponse(proto.CmdVMeasureADCOffset, proto.EncodeI32(0))
		d.SetResponse(proto.CmdVMeasureADCScale, proto.EncodeI32(0))
		d.SetResponse(proto.CmdCurrentLimitMa, proto.EncodeU16(5000))

		if err := s.PostUpdateInit(context.Background(), nil); err != nil {
			t.Fatalf("PostUpdateInit: %v", err)
		}
		want := []string{
			"02 17",             // read vlimit: comes back 0/0
			"03 96 00",          // unlock
			"06 97 bb 80 0c e4", // rewrite the defaults, low=3300 high=48000
			"04 98 02 ee",
			"06 9a 00 00 00 00",
			"06 9b 00 00 00 00",
			"04 93 13 88",
		}
		got := d.SentHex()
		if len(got) != len(want) {
			t.Fatalf("tx = %q, want %q", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("tx[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("write failures are tolerated and reported", func(t *testing.T) {
		s, d := newTestSession(t, Options{Timeout: 20 * time.Millisecond})
		d.SetResponse(proto.CmdUserVLimit, proto.EncodeVLimit(3300, 48000))
		// Nothing else answers: every write times out.

		var logged []string
		if err := s.PostUpdateInit(context.Background(), func(m string) { logged = append(logged, m) }); err != nil {
			t.Fatalf("PostUpdateInit returned %v; step failures must not fail the run", err)
		}
		// One line per step, and the failures must be visible in them.
		joined := strings.Join(logged, "\n")
		for _, want := range []string{"set authlock 0 failed", "set vtolerance nominal 750 mV failed", "set current limit 5000 mA failed"} {
			if !strings.Contains(joined, want) {
				t.Errorf("log missing %q; got:\n%s", want, joined)
			}
		}
		// All the later steps still ran despite the first one failing.
		if n := len(d.Sent()); n != 6 {
			t.Errorf("sent %d frames, want 6 (the sequence must run to completion)", n)
		}
	})

	t.Run("force rewrites the vlimit", func(t *testing.T) {
		s, d := newTestSession(t, Options{Timeout: 20 * time.Millisecond})
		d.SetResponse(proto.CmdUserVLimit, proto.EncodeVLimit(3300, 48000))

		if err := s.PostUpdateInitForce(context.Background(), true, nil); err != nil {
			t.Fatalf("PostUpdateInitForce: %v", err)
		}
		if got := d.SentHex(); len(got) < 3 || got[2] != "06 97 bb 80 0c e4" {
			t.Errorf("tx = %q, want a forced vlimit write at index 2", got)
		}
	})
}

func TestFirmwareAtLeast(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: time.Second})
	d.SetResponse(proto.CmdFirmwareVersion, []byte("5.0.0\x00\x00\x00\x00\x00\x00\x00"))

	ok, version, err := s.FirmwareAtLeast(context.Background(), 5, 0, 0)
	if err != nil {
		t.Fatalf("FirmwareAtLeast: %v", err)
	}
	if !ok {
		t.Error("5.0.0 should satisfy >= 5.0.0")
	}
	if version != "5.0.0" {
		t.Errorf("version = %q, want \"5.0.0\"", version)
	}

	s2, d2 := newTestSession(t, Options{Timeout: time.Second})
	d2.SetResponse(proto.CmdFirmwareVersion, []byte("4.9.9\x00\x00\x00\x00\x00\x00\x00"))
	ok, _, err = s2.FirmwareAtLeast(context.Background(), 5, 0, 0)
	if err != nil {
		t.Fatalf("FirmwareAtLeast: %v", err)
	}
	if ok {
		t.Error("4.9.9 should not satisfy >= 5.0.0")
	}
}

func TestVersionComparison(t *testing.T) {
	cases := []struct {
		version             string
		major, minor, patch int
		want                bool
	}{
		{"5.0.0", 5, 0, 0, true},
		{"5", 5, 0, 0, true},            // missing components count as 0
		{"5.0", 5, 0, 0, true},          //
		{"4.9.9", 5, 0, 0, false},       //
		{"5.0.1", 5, 0, 0, true},        //
		{"6.0.0", 5, 0, 0, true},        //
		{"v5.0.0", 5, 0, 0, true},       // non-digits are skipped
		{" 5.0.0 ", 5, 0, 0, true},      // trimmed
		{"FW 5.0.0-rc1", 5, 0, 0, true}, // trailing runs only add components
		{"4.10.0", 5, 0, 0, false},      // numeric, not lexical, comparison
		{"10.0.0", 9, 0, 0, true},       //
		{"", 5, 0, 0, false},            // unreadable version is not >= 5.0.0
		{"", 0, 0, 0, true},             // ... but it is >= 0.0.0
		{"5.0.0", 5, 0, 1, false},
	}
	for _, tc := range cases {
		if got := VersionAtLeast(tc.version, tc.major, tc.minor, tc.patch); got != tc.want {
			t.Errorf("VersionAtLeast(%q, %d, %d, %d) = %t, want %t",
				tc.version, tc.major, tc.minor, tc.patch, got, tc.want)
		}
	}
}

func TestVersionComponents(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"5.0.0", []int{5, 0, 0}},
		{"v5.1", []int{5, 1}},
		{"", nil},
		{"X", nil},
		{"1.2.3.4", []int{1, 2, 3, 4}},
	}
	for _, tc := range cases {
		got := VersionComponents(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("VersionComponents(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("VersionComponents(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}
