package cli

import (
	"regexp"
	"strconv"
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

// TestRepairCommandsSurviveBeingPasted guards the trap in every `vlimit set`
// command this file prints: those flags are parsed by ParseVoltage, where a
// bare number is VOLTS and is never guessed from magnitude (SPEC.md §11), so
// "--low 3300 --high 48000" asks for 3300 V and is refused on the 16-bit wrap
// check. Both refusals below are reached with no working guard rail and exist
// to say how to restore one, so an instruction that cannot be pasted leaves the
// user stuck with the tool refusing every `voltage set`.
func TestRepairCommandsSurviveBeingPasted(t *testing.T) {
	args := regexp.MustCompile(`gflex vlimit set --low (\S+) --high (\S+)`)

	// The unusable-window refusal prints a complete command, so it goes back
	// through the parser and the interlock exactly as pasted.
	malformed := CheckVoltage(VoltageRequest{
		Mv: 9000, LimitLowMv: 48000, LimitHighMv: 3300, Limits: LimitsMalformed,
	})
	m := args.FindStringSubmatch(malformed.Refused)
	if m == nil {
		t.Fatalf("the unusable-window refusal prints no repair command:\n%s", malformed.Refused)
	}
	low, high := mustParseVoltage(t, m[1]), mustParseVoltage(t, m[2])
	if low != int(proto.HardwareMinVoltageMv) || high != int(proto.HardwareMaxVoltageMv) {
		t.Errorf("the printed repair command asks for [%d, %d] mV, not the [%d, %d] mV envelope it names",
			low, high, proto.HardwareMinVoltageMv, proto.HardwareMaxVoltageMv)
	}
	if d := CheckVLimit(VLimitRequest{NewLowMv: low, NewHighMv: high}); !d.OK() {
		t.Errorf("the repair command the refusal prints is itself refused when pasted:\n  %s --high %s\n  %s",
			m[1], m[2], d.Refused)
	}

	// The outside-the-window refusal prints placeholders, so the numbers are
	// substituted the way a user would and what has to survive is the unit
	// around them.
	outside := CheckVoltage(VoltageRequest{
		Mv: 20000, LimitLowMv: 3300, LimitHighMv: 12000, Limits: LimitsValid,
	})
	p := args.FindStringSubmatch(outside.Refused)
	if p == nil {
		t.Fatalf("the outside-the-window refusal prints no repair command:\n%s", outside.Refused)
	}
	for _, tc := range []struct {
		arg string
		mv  int
	}{{p[1], 3300}, {p[2], 48000}} {
		filled := strings.Replace(tc.arg, "<n>", strconv.Itoa(tc.mv), 1)
		got, err := ParseVoltage(filled)
		if err != nil {
			t.Errorf("the placeholder %q does not take a number: %q -> %v", tc.arg, filled, err)
			continue
		}
		if got != tc.mv {
			t.Errorf("%q filled in with %d mV parses as %d mV; the placeholder does not carry its unit",
				tc.arg, tc.mv, got)
		}
	}
}

func mustParseVoltage(t *testing.T, arg string) int {
	t.Helper()
	mv, err := ParseVoltage(arg)
	if err != nil {
		t.Fatalf("the printed argument %q does not parse: %v", arg, err)
	}
	return mv
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
		wantWarn    string // substring expected in some warning, "" for none required
	}{
		{name: "narrowing both ends", req: set(cur(3300, 48000), 4500, 12000)},
		{name: "narrowing the top only", req: set(cur(3300, 48000), 3300, 20000)},
		// A ceiling above 20 V carries the §13.9 advisory here as well as on
		// `voltage set`: the window is what makes such a request possible at
		// all, and the cable and source it needs are the same either way.
		{name: "identical", req: set(cur(3300, 48000), 3300, 48000), wantWarn: "eMarker"},
		{name: "widening to the hardware ceiling", req: set(cur(3300, 12000), 3300, 48000),
			wantConfirm: true, wantWarn: "eMarker"},
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
			if tt.wantWarn != "" && !containsAny(d.Warnings, tt.wantWarn) {
				t.Errorf("warnings %q do not mention %q", d.Warnings, tt.wantWarn)
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
	d := CheckCalibrate(10, 20, 1, 2, true, true, true)
	if !d.OK() {
		t.Fatalf("unexpected refusal %q", d.Refused)
	}
	if !d.Confirm {
		t.Error("calibration must always be confirmed")
	}
	if !containsAny(d.Warnings, "--offset 1 --scale 2") {
		t.Errorf("the restore command must carry the previous values; got %q", d.Warnings)
	}

	unknown := CheckCalibrate(10, 20, 0, 0, false, true, true)
	if !containsAny(unknown.Warnings, "could not be read") {
		t.Errorf("an unreadable previous calibration must be called out; got %q", unknown.Warnings)
	}

	// `calibrate adc --offset 10` keeps the scale it read, so the prompt must
	// not offer to write it. The prompt is the whole of what --yes suppresses,
	// and a wrong calibration is invisible afterwards (SPEC.md §13.5), so it
	// has to describe the writes that will actually happen.
	kept := CheckCalibrate(10, 20, 1, 20, true, true, false)
	if !strings.Contains(kept.Prompt, "scale=20 (unchanged)") {
		t.Errorf("the prompt offers to write a term the command keeps: %q", kept.Prompt)
	}
	if strings.Contains(kept.Prompt, "offset=10 (unchanged)") {
		t.Errorf("the prompt calls the term it is writing unchanged: %q", kept.Prompt)
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
	// The prompt is the only moment interlock 6 gives anyone to say no, so it
	// has to describe what really happens afterwards. The SPEC.md §10.4 replay
	// writes factory defaults and never read this unit's own values, so a
	// narrowed window can come back as [3300, 48000] -- calling that a
	// restoration understated it in the direction §13.3 otherwise demands an
	// explicit widening confirmation for.
	if strings.Contains(ok.Prompt, "restored") {
		t.Errorf("the flash prompt calls the factory-default replay a restoration: %q", ok.Prompt)
	}
	if !strings.Contains(ok.Prompt, "vlimit set") {
		t.Errorf("the flash prompt does not say how to put a narrowed window back: %q", ok.Prompt)
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

// TestCheckRawFrameGenericReasons covers the two reasons above the switch that
// nothing else reaches on its own, and both are the sole gate on frames a user
// can type today.
//
// Every write case elsewhere in this file is a command with an arm of its own --
// CMD_VOLTAGE_MV has one, command 13 is caught by Undocumented -- so the generic
// WRITE reason could be deleted outright and the suite stayed green while
// `gflex raw 03 96 07` wrote a possibly irreversible auth lock level (SPEC.md
// §6.3, §14.8) and `gflex raw 06 97 bb 80 0c e4` widened the guard rail, both on
// a non-TTY with no --yes. No frame anywhere carried a code outside the table
// either, so the same was true of the unknown-code reason.
//
// As in TestCheckRawFrameDisruptiveCommands, every case asserts the warning
// TEXT: an assertion on Confirm alone passes as soon as any other reason fires
// and measures nothing.
func TestCheckRawFrameGenericReasons(t *testing.T) {
	cases := []struct {
		name     string
		frame    proto.Frame
		wantWarn string
	}{
		{
			"a raw auth lock write",
			proto.Frame{Cmd: proto.CmdAuthLock, Write: true, Payload: []byte{7}},
			"WRITE frame",
		},
		{
			"a raw window write",
			proto.Frame{Cmd: proto.CmdUserVLimit, Write: true, Payload: []byte{0xbb, 0x80, 0x0c, 0xe4}},
			"WRITE frame",
		},
		{
			// A plain read, so the unknown-code reason is the only thing that
			// can speak for it.
			"the first code past the table",
			proto.Frame{Cmd: proto.Cmd(29)},
			"outside the known table",
		},
		{
			"the last code the 6-bit field can carry",
			proto.Frame{Cmd: proto.Cmd(0x3f)},
			"outside the known table",
		},
		{
			// The 0x80-less typo of `04 92 2e e0`: neither a read nor a write by
			// SPEC.md §5.1, and the one command where being wrong about that
			// puts a voltage on the rail.
			"a voltage payload with the write flag clear",
			proto.Frame{Cmd: proto.CmdVoltageMv, Payload: []byte{0x2e, 0xe0}},
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

// The exception the payload-without-write rule is written around. `03 11 kk` is
// the documented PDO chunk read (SPEC.md §6.1): a flag-clear frame that carries
// a payload and is nonetheless a read, which SPEC.md §13.10 requires to pass
// silently. Without this case the exclusion in CheckRawFrame looks like a
// tidyable special case.
func TestCheckRawFramePDOChunkReadPassesSilently(t *testing.T) {
	d := CheckRawFrame(proto.Frame{Cmd: proto.CmdPDOLog, Payload: []byte{0}})
	if !d.OK() || d.Confirm || len(d.Warnings) != 0 {
		t.Errorf("`raw 03 11 00` is a documented read; got confirm=%v refused=%q warnings=%q",
			d.Confirm, d.Refused, d.Warnings)
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
