package proto

import (
	"bytes"
	"errors"
	"testing"
)

// TestBuildGoldenFrames pins every request frame documented in SPEC.md §6.1.
// These are the bytes that actually go on the wire, so a change here is a
// protocol change, not a refactor.
func TestBuildGoldenFrames(t *testing.T) {
	tests := []struct {
		name    string
		cmd     Cmd
		payload []byte
		write   bool
		want    []byte
	}{
		{"read serial", CmdSerialNumber, nil, false, []byte{0x02, 0x08}},
		{"read chip uuid", CmdChipUUID, nil, false, []byte{0x02, 0x09}},
		{"read hardware id", CmdHardwareID, nil, false, []byte{0x02, 0x0A}},
		{"read firmware version", CmdFirmwareVersion, nil, false, []byte{0x02, 0x0B}},
		{"read mfg date", CmdMfgDate, nil, false, []byte{0x02, 0x0C}},
		{"read led", CmdDisableLEDDuringOp, nil, false, []byte{0x02, 0x0F}},
		{"write led always-on", CmdDisableLEDDuringOp, []byte{0x00}, true, []byte{0x03, 0x8F, 0x00}},
		{"write led off-when-green", CmdDisableLEDDuringOp, []byte{0x01}, true, []byte{0x03, 0x8F, 0x01}},
		{"clear pdo log", CmdPDOLog, nil, true, []byte{0x02, 0x91}},
		{"read pdo chunk 3", CmdPDOLog, []byte{0x03}, false, []byte{0x03, 0x11, 0x03}},
		{"read voltage", CmdVoltageMv, nil, false, []byte{0x02, 0x12}},
		{"write voltage 12V", CmdVoltageMv, EncodeU16(12000), true, []byte{0x04, 0x92, 0x2E, 0xE0}},
		{"read current limit", CmdCurrentLimitMa, nil, false, []byte{0x02, 0x13}},
		{"write current 5000mA", CmdCurrentLimitMa, EncodeU16(5000), true, []byte{0x04, 0x93, 0x13, 0x88}},
		{"jump to bootloader", CmdJumpAppToBootloader, nil, false, []byte{0x02, 0x14}},
		{"jump to app", CmdBootloadEnd, nil, false, []byte{0x02, 0x03}},
		{"ios host mode", CmdIOSHostModeFlag, nil, false, []byte{0x02, 0x15}},
		{"read authlock", CmdAuthLock, nil, false, []byte{0x02, 0x16}},
		{"write authlock unlocked", CmdAuthLock, []byte{0x00}, true, []byte{0x03, 0x96, 0x00}},
		{"read vlimit", CmdUserVLimit, nil, false, []byte{0x02, 0x17}},
		{
			"write vlimit 3300/48000", CmdUserVLimit,
			EncodeVLimit(DefaultVLimitLowMv, DefaultVLimitHighMv), true,
			[]byte{0x06, 0x97, 0xBB, 0x80, 0x0C, 0xE4},
		},
		{"read vtol nominal", CmdVToleranceNominalMv, nil, false, []byte{0x02, 0x18}},
		{"write vtol nominal 750", CmdVToleranceNominalMv, EncodeU16(750), true, []byte{0x04, 0x98, 0x02, 0xEE}},
		{"read vtol sag", CmdVToleranceSagPerMa, nil, false, []byte{0x02, 0x19}},
		{"read adc offset", CmdVMeasureADCOffset, nil, false, []byte{0x02, 0x1A}},
		{"write adc offset 0", CmdVMeasureADCOffset, EncodeI32(0), true, []byte{0x06, 0x9A, 0, 0, 0, 0}},
		{"read adc scale", CmdVMeasureADCScale, nil, false, []byte{0x02, 0x1B}},
		{"write adc scale 0", CmdVMeasureADCScale, EncodeI32(0), true, []byte{0x06, 0x9B, 0, 0, 0, 0}},
		{"read vmeasure", CmdVMeasure, nil, false, []byte{0x02, 0x1C}},
		{"bootloader commit page", CmdBootloaderCommitPage, nil, true, []byte{0x02, 0x81}},
		{"bootloader verify write", CmdBootloaderVerify, nil, true, []byte{0x02, 0x82}},
		{"bootloader verify read", CmdBootloaderVerify, nil, false, []byte{0x02, 0x02}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Build(tc.cmd, tc.payload, tc.write, false)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("Build = %s, want %s", Hex(got), Hex(tc.want))
			}
			// The length byte must always describe the whole frame.
			if int(got[0]) != len(got) {
				t.Errorf("length byte %d != frame length %d", got[0], len(got))
			}
		})
	}
}

func TestBuildFlags(t *testing.T) {
	// The scratchpad bit is never set by the vendor app, but Build must still
	// encode it correctly for the raw escape hatch.
	f, err := Build(CmdVoltageMv, []byte{0x01}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if f[1] != 0x12|FlagWrite|FlagScratchpad {
		t.Errorf("command byte = %#02x, want %#02x", f[1], 0x12|FlagWrite|FlagScratchpad)
	}
	parsed, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Cmd != CmdVoltageMv || !parsed.Write || !parsed.Scratchpad {
		t.Errorf("Parse = %+v, want cmd=18 write=true scratchpad=true", parsed)
	}
}

// TestBuildLimits covers the two distinct ceilings. Build enforces only the
// length-byte limit; the tighter MIDI limit is FitsMIDI's job, because the
// bootloader's bulk path legitimately exceeds it.
func TestBuildLimits(t *testing.T) {
	// A WRITE_CHUNK-sized frame must build: 64-byte chunk + 3-byte header.
	big, err := Build(CmdBootloaderWriteChunk, make([]byte, 67), true, false)
	if err != nil {
		t.Fatalf("Build(67-byte payload) = %v, want success", err)
	}
	if len(big) != 69 || big[0] != 69 {
		t.Errorf("frame len %d, length byte %d, want 69/69", len(big), big[0])
	}
	if FitsMIDI(big) {
		t.Error("FitsMIDI(69-byte frame) = true, want false")
	}
	if !FitsMIDI([]byte{0x02, 0x08}) {
		t.Error("FitsMIDI(2-byte frame) = false, want true")
	}

	if _, err := Build(CmdEncryptMsg, make([]byte, MaxEncodablePayloadLen+1), true, false); !errors.Is(err, ErrPayloadTooLong) {
		t.Errorf("Build(oversize) error = %v, want ErrPayloadTooLong", err)
	}
	if _, err := Build(CmdEncryptMsg, make([]byte, MaxEncodablePayloadLen), true, false); err != nil {
		t.Errorf("Build(max payload) = %v, want success", err)
	}
}

// TestParseLenient reproduces the vendor client's tolerance of a bogus length
// byte: it falls back to the bytes actually received rather than rejecting.
func TestParseLenient(t *testing.T) {
	tests := []struct {
		name        string
		raw         []byte
		wantPayload []byte
	}{
		{"exact", []byte{0x04, 0x12, 0x2E, 0xE0}, []byte{0x2E, 0xE0}},
		{"declared shorter, truncates", []byte{0x03, 0x12, 0x2E, 0xE0}, []byte{0x2E}},
		{"declared longer, uses buffer", []byte{0x09, 0x12, 0x2E, 0xE0}, []byte{0x2E, 0xE0}},
		{"declared zero, uses buffer", []byte{0x00, 0x12, 0x2E, 0xE0}, []byte{0x2E, 0xE0}},
		{"declared one, uses buffer", []byte{0x01, 0x12, 0x2E}, []byte{0x2E}},
		{"minimum frame", []byte{0x02, 0x12}, []byte{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !bytes.Equal(f.Payload, tc.wantPayload) {
				t.Errorf("payload = %s, want %s", Hex(f.Payload), Hex(tc.wantPayload))
			}
		})
	}
	if _, err := Parse([]byte{0x02}); !errors.Is(err, ErrShortFrame) {
		t.Errorf("Parse(1 byte) error = %v, want ErrShortFrame", err)
	}
}

// TestParseMasksFlags: the command code must come back with the flag bits
// stripped, so response matching compares codes and not codes-plus-flags.
func TestParseMasksFlags(t *testing.T) {
	for _, b := range []byte{0x12, 0x92, 0x52, 0xD2} {
		f, err := Parse([]byte{0x02, b})
		if err != nil {
			t.Fatal(err)
		}
		if f.Cmd != CmdVoltageMv {
			t.Errorf("command byte %#02x parsed as %v, want CMD_VOLTAGE_MV", b, f.Cmd)
		}
	}
}

func TestValidResponseLen(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want bool
	}{{0, false}, {1, false}, {2, true}, {63, true}, {64, true}, {65, false}} {
		if got := ValidResponseLen(tc.n); got != tc.want {
			t.Errorf("ValidResponseLen(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}

func TestU16RoundTrip(t *testing.T) {
	for _, v := range []uint16{0, 1, 3300, 5000, 12000, 20000, 48000, 65535} {
		got, err := DecodeU16(EncodeU16(v))
		if err != nil {
			t.Fatalf("DecodeU16(%d): %v", v, err)
		}
		if got != v {
			t.Errorf("round trip %d -> %d", v, got)
		}
	}
	if _, err := DecodeU16([]byte{0x01}); err == nil {
		t.Error("DecodeU16(1 byte) = nil error, want failure")
	}
}

// TestI32SignedRoundTrip guards the ADC calibration fields. They are genuinely
// signed -- the vendor client decodes them with JavaScript shifts, which yield
// int32 -- so a uint32 implementation would silently misreport negative values.
func TestI32SignedRoundTrip(t *testing.T) {
	for _, v := range []int32{0, 1, -1, 32767, -32768, 2147483647, -2147483648} {
		enc := EncodeI32(v)
		if len(enc) != 4 {
			t.Fatalf("EncodeI32(%d) produced %d bytes", v, len(enc))
		}
		got, err := DecodeI32(enc)
		if err != nil {
			t.Fatal(err)
		}
		if got != v {
			t.Errorf("round trip %d -> %d (%s)", v, got, Hex(enc))
		}
	}
	// Big-endian, top bit set, must read back negative.
	got, err := DecodeI32([]byte{0x80, 0x00, 0x00, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	if got != -2147483648 {
		t.Errorf("DecodeI32(80 00 00 00) = %d, want -2147483648", got)
	}
}

// TestVLimitWireOrder is the one most likely to be got backwards: the wire
// carries HIGH before LOW, while the API argument order is (low, high).
func TestVLimitWireOrder(t *testing.T) {
	enc := EncodeVLimit(3300, 48000)
	want := []byte{0xBB, 0x80, 0x0C, 0xE4} // 48000 then 3300
	if !bytes.Equal(enc, want) {
		t.Fatalf("EncodeVLimit(3300, 48000) = %s, want %s", Hex(enc), Hex(want))
	}
	low, high, err := DecodeVLimit(enc)
	if err != nil {
		t.Fatal(err)
	}
	if low != 3300 || high != 48000 {
		t.Errorf("DecodeVLimit = low %d high %d, want 3300/48000", low, high)
	}
	if _, _, err := DecodeVLimit([]byte{0x01, 0x02, 0x03}); err == nil {
		t.Error("DecodeVLimit(3 bytes) = nil error, want failure")
	}
}

func TestDecodeVMeasure(t *testing.T) {
	raw, mv, err := DecodeVMeasure([]byte{0x04, 0xD2, 0x2E, 0xE0})
	if err != nil {
		t.Fatal(err)
	}
	if raw != 1234 || mv != 12000 {
		t.Errorf("DecodeVMeasure = %d/%d, want 1234/12000", raw, mv)
	}
}

// TestLEDInversion: the command is DISABLE_LED_DURING_OPERATION, so the wire
// byte is the logical inverse of the user-facing "LED Always On" toggle.
func TestLEDInversion(t *testing.T) {
	if got := EncodeLEDAlwaysOn(true); got != 0 {
		t.Errorf("EncodeLEDAlwaysOn(true) = %d, want 0", got)
	}
	if got := EncodeLEDAlwaysOn(false); got != 1 {
		t.Errorf("EncodeLEDAlwaysOn(false) = %d, want 1", got)
	}
	if !DecodeLEDAlwaysOn(0) {
		t.Error("DecodeLEDAlwaysOn(0) = false, want true")
	}
	for _, b := range []byte{1, 2, 0xFF} {
		if DecodeLEDAlwaysOn(b) {
			t.Errorf("DecodeLEDAlwaysOn(%d) = true, want false", b)
		}
	}
}

func TestDecodeString(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"plain", []byte("VF001234"), "VF001234"},
		{"nul padded", []byte("VF0012\x00\x00"), "VF0012"},
		{"space padded", []byte("VF0012  "), "VF0012"},
		{"high bytes stripped", []byte{'V', 'F', 0xFF, 0x80, '1'}, "VF1"},
		{"control bytes stripped", []byte{'V', 0x01, 'F', 0x1F}, "VF"},
		{"empty", []byte{}, ""},
		{"all padding", []byte{0, 0, 0}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecodeString(tc.in); got != tc.want {
				t.Errorf("DecodeString(%s) = %q, want %q", Hex(tc.in), got, tc.want)
			}
		})
	}
}

func TestSerialUsable(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want bool
	}{{"", false}, {"VF", false}, {"VF0", false}, {"VF00", true}, {"VF001234", true}} {
		if got := SerialUsable(tc.s); got != tc.want {
			t.Errorf("SerialUsable(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestCmdNames(t *testing.T) {
	if got := CmdVoltageMv.String(); got != "CMD_VOLTAGE_MV" {
		t.Errorf("CmdVoltageMv.String() = %q", got)
	}
	if got := Cmd(63).String(); got != "CMD_63" {
		t.Errorf("Cmd(63).String() = %q, want CMD_63", got)
	}
	if Cmd(63).Known() {
		t.Error("Cmd(63).Known() = true, want false")
	}
	// Every code 0..28 must be named; the table is the protocol.
	for c := Cmd(0); c <= 28; c++ {
		if !c.Known() {
			t.Errorf("command %d has no name", c)
		}
	}
	// The commands whose payload format was never determined must be flagged,
	// so the CLI can refuse to emit them without an explicit override.
	for _, c := range []Cmd{CmdReserved0, CmdReserved1, CmdReserved2, CmdReserved3,
		CmdFlashLEDSeqAdvanced, CmdFlashLED, CmdEncryptMsg, CmdIOSHostModeFlag} {
		if !c.Undocumented() {
			t.Errorf("%v should be marked undocumented", c)
		}
	}
	for _, c := range []Cmd{CmdSerialNumber, CmdVoltageMv, CmdUserVLimit, CmdPDOLog} {
		if c.Undocumented() {
			t.Errorf("%v should not be marked undocumented", c)
		}
	}
}

func TestStringLen(t *testing.T) {
	for _, tc := range []struct {
		c    Cmd
		want int
	}{
		{CmdSerialNumber, 8}, {CmdChipUUID, 8}, {CmdHardwareID, 8},
		{CmdFirmwareVersion, 12}, {CmdMfgDate, 8},
	} {
		n, ok := StringLen(tc.c)
		if !ok || n != tc.want {
			t.Errorf("StringLen(%v) = %d,%v want %d,true", tc.c, n, ok, tc.want)
		}
	}
	if _, ok := StringLen(CmdVoltageMv); ok {
		t.Error("StringLen(CmdVoltageMv) reported string-valued")
	}
}

func TestHex(t *testing.T) {
	if got := Hex([]byte{0x04, 0x92, 0x2E, 0xE0}); got != "04 92 2e e0" {
		t.Errorf("Hex = %q", got)
	}
	if got := Hex(nil); got != "" {
		t.Errorf("Hex(nil) = %q, want empty", got)
	}
}

// TestBuildCopiesPayload: Build must not alias its input, or a caller reusing a
// scratch buffer would mutate a frame already queued for transmission.
func TestBuildCopiesPayload(t *testing.T) {
	payload := []byte{0x2E, 0xE0}
	f, err := Build(CmdVoltageMv, payload, true, false)
	if err != nil {
		t.Fatal(err)
	}
	payload[0] = 0xFF
	if f[2] != 0x2E {
		t.Error("Build aliased its payload argument")
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte{0x02, 0x08})
	f.Add([]byte{0x04, 0x92, 0x2E, 0xE0})
	f.Add([]byte{0x00, 0x12, 0x2E, 0xE0})
	f.Add([]byte{0xFF, 0x12})
	f.Fuzz(func(t *testing.T, raw []byte) {
		fr, err := Parse(raw)
		if err != nil {
			return
		}
		if len(raw) < PreambleLen {
			t.Fatalf("Parse accepted a %d-byte input", len(raw))
		}
		// The payload must always be a slice of the input, never longer.
		if len(fr.Payload) > len(raw)-PreambleLen {
			t.Fatalf("payload %d bytes from a %d-byte frame", len(fr.Payload), len(raw))
		}
		if uint8(fr.Cmd) > CmdCodeMask {
			t.Fatalf("command code %d exceeds the 6-bit mask", fr.Cmd)
		}
	})
}
