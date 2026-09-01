package cli

import (
	"strings"
	"testing"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/session"
)

// TestNarrowWindowIsHonoured is a regression test for the worst defect found in
// review: `voltage set` filtered the device's window through VLimitPlausible,
// whose >= 6000 mV floor comes from the vendor's post-flash "was this erased?"
// check. Any ceiling below 6 V was therefore discarded and the write fell back
// to the 3300-48000 mV envelope -- so a unit its owner had limited to 5 V would
// accept 48 V, and --yes was enough to get there.
//
// A [3300, 5000] window is not suspicious. It is the single most protective
// configuration in the product, and it is exactly what someone with a 5 V pedal
// would set.
func TestNarrowWindowIsHonoured(t *testing.T) {
	for _, mv := range []int{5001, 9000, 12000, 20000, 48000} {
		req := VoltageRequest{Mv: mv, LimitLowMv: 3300, LimitHighMv: 5000, Limits: LimitsValid}
		if d := CheckVoltage(req); d.OK() {
			t.Errorf("%d mV permitted on a unit limited to 5000 mV (confirm=%v)", mv, d.Confirm)
		}
	}
	// The window must still permit what it actually covers.
	if d := CheckVoltage(VoltageRequest{Mv: 5000, LimitLowMv: 3300, LimitHighMv: 5000, Limits: LimitsValid}); !d.OK() {
		t.Errorf("5000 mV refused inside its own [3300, 5000] window: %s", d.Refused)
	}
}

// TestVLimitUsableVsPlausible pins the distinction the bug conflated.
//
// The plausibility half is deliberately taken from package session: there is
// only one copy of that predicate now, and this test is what says the two
// questions still have different answers for the window that matters. Two
// copies is how the bug above comes back.
func TestVLimitUsableVsPlausible(t *testing.T) {
	// Usable as a bound, but below the vendor's post-flash rewrite threshold.
	if !VLimitUsable(3300, 5000) {
		t.Error("VLimitUsable(3300, 5000) = false; a 5 V ceiling is a legitimate window")
	}
	if session.VLimitPlausible(3300, 5000) {
		t.Error("session.VLimitPlausible(3300, 5000) = true; it should still report the vendor's shape test")
	}
	// Cannot bound anything.
	for _, p := range [][2]uint16{{5000, 5000}, {9000, 5000}, {0, 0}} {
		if VLimitUsable(p[0], p[1]) {
			t.Errorf("VLimitUsable(%d, %d) = true", p[0], p[1])
		}
	}
}

func TestCheckVoltage(t *testing.T) {
	limits := VoltageRequest{LimitLowMv: 3300, LimitHighMv: 48000, Limits: LimitsValid}
	with := func(mv int, base VoltageRequest) VoltageRequest {
		base.Mv = mv
		return base
	}

	tests := []struct {
		name        string
		req         VoltageRequest
		wantRefused bool
		wantConfirm bool
		wantWarn    string // substring expected in some warning, "" for none required
	}{
		{name: "5V default needs no confirmation", req: with(5000, limits)},
		{name: "3.3V floor allowed", req: with(3300, limits)},
		{name: "9V confirms", req: with(9000, limits), wantConfirm: true},
		{name: "12V confirms", req: with(12000, limits), wantConfirm: true},
		{name: "28V warns about EPR", req: with(28000, limits), wantConfirm: true, wantWarn: "eMarker"},
		{name: "48V ceiling allowed", req: with(48000, limits), wantConfirm: true},
		{name: "below hardware minimum", req: with(3000, limits), wantRefused: true},
		{name: "above hardware maximum", req: with(48001, limits), wantRefused: true},
		{name: "16-bit wrap", req: with(66000, limits), wantRefused: true},
		{name: "bare millivolt typo wraps", req: with(12000000, limits), wantRefused: true},
		{name: "negative", req: with(-1, limits), wantRefused: true},
		{
			name:        "outside a narrowed user window",
			req:         VoltageRequest{Mv: 20000, LimitLowMv: 4500, LimitHighMv: 9500, Limits: LimitsValid},
			wantRefused: true,
		},
		{
			name:        "inside a narrowed user window",
			req:         VoltageRequest{Mv: 9000, LimitLowMv: 4500, LimitHighMv: 9500, Limits: LimitsValid},
			wantConfirm: true,
		},
		{
			// SPEC.md §13.1 gives interlock 1 no fallback. A failed read must
			// refuse, not silently downgrade to the hardware envelope: the
			// protocol has no NACK, so a dropped frame is an ordinary event
			// and would otherwise turn a 5 V ceiling into 48 V.
			name:        "unreadable limits refuse",
			req:         VoltageRequest{Mv: 12000, Limits: LimitsUnread},
			wantRefused: true,
		},
		{
			name:        "unreadable limits proceed under the explicit override",
			req:         VoltageRequest{Mv: 12000, Limits: LimitsUnread, IgnoreDeviceLimits: true},
			wantConfirm: true,
			wantWarn:    "could not be read",
		},
		{
			name:        "malformed window refuses",
			req:         VoltageRequest{Mv: 12000, LimitLowMv: 9000, LimitHighMv: 5000, Limits: LimitsMalformed},
			wantRefused: true,
		},
		{
			name:        "malformed window proceeds under the explicit override",
			req:         VoltageRequest{Mv: 12000, LimitLowMv: 9000, LimitHighMv: 5000, Limits: LimitsMalformed, IgnoreDeviceLimits: true},
			wantConfirm: true,
			wantWarn:    "unusable device window",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := CheckVoltage(tt.req)
			if got := !d.OK(); got != tt.wantRefused {
				t.Fatalf("refused = %v (%q), want %v", got, d.Refused, tt.wantRefused)
			}
			if d.OK() && d.Confirm != tt.wantConfirm {
				t.Errorf("Confirm = %v, want %v", d.Confirm, tt.wantConfirm)
			}
			if d.Confirm && d.Prompt == "" {
				t.Error("Confirm is set but Prompt is empty")
			}
			if tt.wantWarn != "" && !containsAny(d.Warnings, tt.wantWarn) {
				t.Errorf("warnings %q do not mention %q", d.Warnings, tt.wantWarn)
			}
		})
	}
}

// The refusal for a wrapped value must say so: it is the failure mode that is
// otherwise invisible until something is already broken.
func TestCheckVoltageWrapRefusalExplainsItself(t *testing.T) {
	d := CheckVoltage(VoltageRequest{Mv: 70000})
	if d.OK() {
		t.Fatal("70000 mV should be refused")
	}
	if !strings.Contains(d.Refused, "wrap") {
		t.Errorf("refusal %q does not mention wrapping", d.Refused)
	}
}

func TestCheckCurrent(t *testing.T) {
	tests := []struct {
		ma          int
		wantRefused bool
	}{
		{ma: 3000},
		{ma: 5000},
		{ma: 0},
		{ma: 5001, wantRefused: true},
		{ma: 65536, wantRefused: true},
		{ma: 3000000, wantRefused: true},
		{ma: -1, wantRefused: true},
	}
	for _, tt := range tests {
		d := CheckCurrent(tt.ma)
		if got := !d.OK(); got != tt.wantRefused {
			t.Errorf("CheckCurrent(%d): refused = %v (%q), want %v", tt.ma, got, d.Refused, tt.wantRefused)
		}
		// A current-limit write is a negotiation request, not a rail change,
		// so it never prompts.
		if d.Confirm {
			t.Errorf("CheckCurrent(%d) should not require confirmation", tt.ma)
		}
	}
}

func TestCheckVLimit(t *testing.T) {
	cur := func(low, high uint16) VLimitRequest {
		return VLimitRequest{CurLowMv: low, CurHighMv: high, CurKnown: true}
	}
	set := func(r VLimitRequest, low, high int) VLimitRequest {
		r.NewLowMv, r.NewHighMv = low, high
		return r
	}

	tests := []struct {
		name        string
		req         VLimitRequest
		wantRefused bool
		wantConfirm bool
	}{
		{name: "narrowing both ends", req: set(cur(3300, 48000), 4500, 12000)},
		{name: "narrowing the top only", req: set(cur(3300, 48000), 3300, 20000)},
		{name: "identical", req: set(cur(3300, 48000), 3300, 48000)},
		{name: "widening the top", req: set(cur(3300, 12000), 3300, 20000), wantConfirm: true},
		{name: "widening the bottom", req: set(cur(4500, 12000), 3300, 12000), wantConfirm: true},
		{name: "unknown current pair is assumed to widen", req: set(VLimitRequest{}, 3300, 48000), wantConfirm: true},
		{name: "inverted", req: set(cur(3300, 48000), 12000, 9000), wantRefused: true},
		{name: "equal", req: set(cur(3300, 48000), 9000, 9000), wantRefused: true},
		{name: "below the hardware floor", req: set(cur(3300, 48000), 1000, 12000), wantRefused: true},
		{name: "above the hardware ceiling", req: set(cur(3300, 48000), 3300, 60000), wantRefused: true},
		{name: "wraps the wire field", req: set(cur(3300, 48000), 3300, 70000), wantRefused: true},
		{name: "negative", req: set(cur(3300, 48000), -1, 12000), wantRefused: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := CheckVLimit(tt.req)
			if got := !d.OK(); got != tt.wantRefused {
				t.Fatalf("refused = %v (%q), want %v", got, d.Refused, tt.wantRefused)
			}
			if d.OK() && d.Confirm != tt.wantConfirm {
				t.Errorf("Confirm = %v, want %v", d.Confirm, tt.wantConfirm)
			}
		})
	}
}

// The table that used to live here exercised this package's own copy of
// VLimitPlausible. The copy is gone -- session.TestVLimitPlausible covers the
// predicate itself, and TestVLimitUsableVsPlausible above covers the only thing
// this package needs from it.

func TestCheckAuthLock(t *testing.T) {
	unlocked := CheckAuthLock(int(proto.AuthLockUnlocked))
	if !unlocked.OK() {
		t.Fatalf("level 0 should be allowed, got %q", unlocked.Refused)
	}
	if !unlocked.Confirm {
		t.Error("even level 0 must be confirmed")
	}
	if len(unlocked.Warnings) != 0 {
		t.Errorf("level 0 is documented; unexpected warnings %q", unlocked.Warnings)
	}

	locked := CheckAuthLock(1)
	if !locked.OK() {
		t.Fatalf("level 1 should be allowed behind a confirmation, got %q", locked.Refused)
	}
	if !locked.Confirm {
		t.Error("a non-zero level must be confirmed")
	}
	if !containsAny(locked.Warnings, "NO DOCUMENTED EFFECT") {
		t.Errorf("a non-zero level must say its effect is undocumented; got %q", locked.Warnings)
	}

	for _, bad := range []int{-1, 256, 1000} {
		if d := CheckAuthLock(bad); d.OK() {
			t.Errorf("CheckAuthLock(%d) should be refused", bad)
		}
	}
}

func TestCheckCalibrate(t *testing.T) {
	d := CheckCalibrate(10, 20, 1, 2, true)
	if !d.OK() {
		t.Fatalf("unexpected refusal %q", d.Refused)
	}
	if !d.Confirm {
		t.Error("calibration must always be confirmed")
	}
	if !containsAny(d.Warnings, "--offset 1 --scale 2") {
		t.Errorf("the restore command must carry the previous values; got %q", d.Warnings)
	}

	unknown := CheckCalibrate(10, 20, 0, 0, false)
	if !containsAny(unknown.Warnings, "could not be read") {
		t.Errorf("an unreadable previous calibration must be called out; got %q", unknown.Warnings)
	}
}

func TestCheckFlash(t *testing.T) {
	if d := CheckFlash(0, "1.2.3", true, false); d.OK() {
		t.Error("an empty image must be refused")
	}
	// No CRC and no --force: refuse rather than flash something that cannot be
	// verified before the jump.
	if d := CheckFlash(10, "1.2.3", false, false); d.OK() {
		t.Error("an image with no CRC must be refused without --force")
	}
	forced := CheckFlash(10, "1.2.3", false, true)
	if !forced.OK() {
		t.Fatalf("--force should allow an unverifiable image, got %q", forced.Refused)
	}
	if !containsAny(forced.Warnings, "SKIPPED") {
		t.Errorf("--force must warn that verification is skipped; got %q", forced.Warnings)
	}
	ok := CheckFlash(10, "5.0.1", true, false)
	if !ok.OK() || !ok.Confirm {
		t.Errorf("a normal flash should be allowed and confirmed, got refused=%q confirm=%v", ok.Refused, ok.Confirm)
	}
}

func TestCheckRawFrame(t *testing.T) {
	// A plain documented read is the one case that goes through unchallenged.
	read := proto.Frame{Cmd: proto.CmdSerialNumber}
	if d := CheckRawFrame(read); d.Confirm || !d.OK() {
		t.Errorf("a documented read should not prompt; got confirm=%v refused=%q", d.Confirm, d.Refused)
	}

	write := proto.Frame{Cmd: proto.CmdVoltageMv, Write: true}
	if d := CheckRawFrame(write); !d.Confirm {
		t.Error("a raw write must be confirmed")
	}

	undoc := proto.Frame{Cmd: proto.CmdReserved0}
	d := CheckRawFrame(undoc)
	if !d.Confirm {
		t.Error("an undocumented command must be confirmed")
	}
	if !containsAny(d.Warnings, "no documented payload format") {
		t.Errorf("warnings %q should explain why", d.Warnings)
	}

	// Command 13's payload was measured (SPEC.md §6.2), but only for lists of one
	// and two records, so a raw frame to it still confirms -- and the warning
	// says which part is charted and names the command that sends the charted
	// part, because somebody reaching for `raw` to set a colour has `led color`.
	led := proto.Frame{Cmd: proto.CmdFlashLEDSeqAdvanced, Write: true}
	dl := CheckRawFrame(led)
	if !dl.Confirm {
		t.Error("a raw frame to command 13 must still be confirmed; a payload measured to depth two is not characterisation")
	}
	for _, want := range []string{"counted list", "led color"} {
		if !containsAny(dl.Warnings, want) {
			t.Errorf("warnings %q should mention %q", dl.Warnings, want)
		}
	}
	if containsAny(dl.Warnings, "no documented payload format") {
		t.Errorf("command 13 still claims nothing is documented: %q", dl.Warnings)
	}

	scratch := proto.Frame{Cmd: proto.CmdVoltageMv, Scratchpad: true}
	if d := CheckRawFrame(scratch); !d.Confirm || !containsAny(d.Warnings, "scratchpad") {
		t.Errorf("the scratchpad flag must be called out; got confirm=%v warnings=%q", d.Confirm, d.Warnings)
	}
	// And it must say what the flag actually does. Both units measured it as
	// validate-and-discard -- acknowledged, never committed (SPEC.md §14.4) --
	// which is the one thing a user needs to be told here, because the frame
	// will look like it worked. Deliberately NOT pinned: what the response
	// carries. That is per-command rather than a property of the flag (18
	// answers with the value sent, 19 with the value kept), so asserting one
	// of them here would pin a detail the firmware does not hold to.
	if d := CheckRawFrame(scratch); !containsAny(d.Warnings, "never committed") {
		t.Errorf("the scratchpad warning does not say the write will not take effect: %q", d.Warnings)
	}
}

// TestCheckRawFrameDisruptiveCommands covers the three self-warning cases of
// SPEC.md §13.10 -- the ones the four generic reasons above the switch cannot
// see.
//
// CMD_JUMP_APP_TO_BOOTLOADER is the case that motivated the switch: a plain
// READ frame carrying a documented, known command code, so Write, Undocumented,
// !Known and Scratchpad all say nothing about it, and without its arm
// CheckRawFrame returns a zero Decision -- `gflex raw 02 14` would go out
// unchallenged and leave the unit somewhere only `firmware flash --recover` can
// reach.
//
// Every case asserts the warning TEXT, not merely that a confirmation happened.
// For the voltage write that distinction is the whole test: the generic Write
// reason already sets Confirm, so an assertion on Confirm alone would still
// pass with the arm deleted and would be measuring nothing.
func TestCheckRawFrameDisruptiveCommands(t *testing.T) {
	cases := []struct {
		name     string
		frame    proto.Frame
		wantWarn string
	}{
		{
			"the jump is a documented read and still confirms",
			proto.Frame{Cmd: proto.CmdJumpAppToBootloader},
			"disconnects the device into bootloader mode",
		},
		{
			"bootload end in application mode",
			proto.Frame{Cmd: proto.CmdBootloadEnd},
			"is a bootloader command",
		},
		{
			"write chunk in application mode",
			proto.Frame{Cmd: proto.CmdBootloaderWriteChunk},
			"is a bootloader command",
		},
		{
			"commit page in application mode",
			proto.Frame{Cmd: proto.CmdBootloaderCommitPage},
			"is a bootloader command",
		},
		{
			"verify in application mode",
			proto.Frame{Cmd: proto.CmdBootloaderVerify},
			"is a bootloader command",
		},
		{
			"a raw voltage write names the checks it bypasses",
			proto.Frame{Cmd: proto.CmdVoltageMv, Write: true},
			"bypassing every range check",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := CheckRawFrame(tc.frame)
			if !d.OK() {
				t.Fatalf("refused outright (%q); these are confirmations, not refusals", d.Refused)
			}
			if !d.Confirm {
				t.Errorf("%s went through with no confirmation", tc.frame.Cmd)
			}
			if !containsAny(d.Warnings, tc.wantWarn) {
				t.Errorf("warnings %q do not name the reason (%q)", d.Warnings, tc.wantWarn)
			}
		})
	}
}

func containsAny(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
