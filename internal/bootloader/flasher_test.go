package bootloader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/usbfs"
)

// fakeBulk stands in for the device's vendor-class endpoint pair. Frames
// written OUT are recorded; a responder decides what, if anything, becomes
// readable on IN.
type fakeBulk struct {
	sent    [][]byte
	inQueue [][]byte
	// respond is called with each OUT frame and returns the frames to make
	// readable in reply. nil means the device stays silent, which is what
	// stream mode expects.
	respond func(out []byte) [][]byte
	// inErr, when set, fails every IN read with that error, standing in for a
	// transport-level failure such as the device leaving the bus.
	inErr error
	// inReads counts IN transfer attempts, so a test can assert that a fatal
	// error aborted the wait instead of being polled through.
	inReads int
}

var errNothingToRead = errors.New("fake: no data")

func (f *fakeBulk) Transfer(ctx context.Context, endpoint uint8, data []byte, _ time.Duration) (int, error) {
	if endpoint&0x80 == 0 { // OUT
		frame := append([]byte(nil), data...)
		f.sent = append(f.sent, frame)
		if f.respond != nil {
			f.inQueue = append(f.inQueue, f.respond(frame)...)
		}
		return len(data), nil
	}
	f.inReads++
	if f.inErr != nil {
		return 0, f.inErr
	}
	if len(f.inQueue) == 0 {
		return 0, errNothingToRead
	}
	frame := f.inQueue[0]
	f.inQueue = f.inQueue[1:]
	return copy(data, frame), nil
}

// sent reports whether frame was written to the OUT endpoint at any point.
func (f *fakeBulk) hasSent(frame []byte) bool {
	for _, out := range f.sent {
		if bytes.Equal(out, frame) {
			return true
		}
	}
	return false
}

// logLines collects the Log callback so a test can assert on what the caller
// was told — for an unverified flash that warning is the only place the risk
// surfaces at all.
type logLines []string

func (l logLines) contains(sub string) bool {
	for _, s := range l {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func newLog() (*logLines, func(string)) {
	var l logLines
	return &l, func(s string) { l = append(l, s) }
}

// newTestFlasher wires a Flasher straight to a fake, bypassing NewFlasher's
// descriptor-driven endpoint selection, and shrinks every delay so the tests
// are fast.
func newTestFlasher(dev bulkDevice) *Flasher {
	return &Flasher{
		dev:     dev,
		in:      usbfs.Endpoint{Address: 0x81, Attributes: 0x02, MaxPacketSize: 64},
		out:     usbfs.Endpoint{Address: 0x01, Attributes: 0x02, MaxPacketSize: 64},
		readLen: minReadBufferLen,
		t: timing{
			chunk:       0,
			commit:      0,
			postFlash:   0,
			verifyRound: time.Millisecond,
			ack:         200 * time.Millisecond,
			verify:      200 * time.Millisecond,
			serial:      200 * time.Millisecond,
		},
	}
}

// newTimedFlasher keeps the real SPEC.md §10.1 pacing but records each delay
// instead of waiting for it, so a test can assert *which* delays a sequence
// applies rather than only how long it took. The three response budgets stay
// short because they are deadlines, not sleeps.
func newTimedFlasher(dev bulkDevice) (*Flasher, *[]time.Duration) {
	f := newTestFlasher(dev)
	f.t = defaultTiming()
	f.t.ack = 200 * time.Millisecond
	f.t.verify = 200 * time.Millisecond
	f.t.serial = 200 * time.Millisecond
	var seen []time.Duration
	f.sleep = func(ctx context.Context, d time.Duration) error {
		seen = append(seen, d)
		return ctx.Err()
	}
	return f, &seen
}

func countDelay(seen []time.Duration, d time.Duration) int {
	n := 0
	for _, got := range seen {
		if got == d {
			n++
		}
	}
	return n
}

// ackEverything answers each OUT frame with a bare acknowledgement echoing its
// command code, which is what a healthy bootloader does.
func ackEverything(out []byte) [][]byte {
	cmd := out[1] & proto.CmdCodeMask
	switch proto.Cmd(cmd) {
	case proto.CmdSerialNumber:
		return [][]byte{append([]byte{0x0A, 0x08}, []byte("VF001234")...)}
	case proto.CmdBootloaderVerify:
		// Only the read form produces the CRC, matching "write starts the
		// computation, read collects it".
		if out[1]&proto.FlagWrite != 0 {
			return nil
		}
		return [][]byte{{0x03, 0x02, 0x5A}}
	case proto.CmdBootloadEnd:
		return nil
	default:
		return [][]byte{{0x02, cmd}}
	}
}

func TestFlashStreamModeSequence(t *testing.T) {
	t.Parallel()
	fw := &Firmware{Pages: [][]byte{page(0x00, 16), page(0x40, 16)}, CRC: 0x5A, CRCKnown: true}
	dev := &fakeBulk{}
	f := newTestFlasher(dev)

	var phases []string
	if err := f.Flash(t.Context(), fw, false, func(p Progress) {
		phases = append(phases, p.Phase)
	}); err != nil {
		t.Fatalf("Flash: %v", err)
	}

	// Two pages: 8 chunks plus a commit each.
	wantFrames := 2 * (ChunksPerPage + 1)
	if len(dev.sent) != wantFrames {
		t.Fatalf("sent %d frames, want %d", len(dev.sent), wantFrames)
	}
	for pageIdx := 0; pageIdx < 2; pageIdx++ {
		for chunkIdx := 0; chunkIdx < ChunksPerPage; chunkIdx++ {
			got := dev.sent[pageIdx*(ChunksPerPage+1)+chunkIdx]
			want, err := WriteChunkFrame(uint16(pageIdx), uint8(chunkIdx),
				fw.Pages[pageIdx][chunkIdx*2:(chunkIdx+1)*2])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("page %d chunk %d = %s, want %s", pageIdx, chunkIdx,
					proto.Hex(got), proto.Hex(want))
			}
		}
		commit := dev.sent[pageIdx*(ChunksPerPage+1)+ChunksPerPage]
		if !bytes.Equal(commit, CommitPageFrame()) {
			t.Errorf("page %d commit = %s, want %s", pageIdx, proto.Hex(commit),
				proto.Hex(CommitPageFrame()))
		}
	}
	// Stream mode waits for nothing, so no reads should have happened.
	if len(dev.inQueue) != 0 {
		t.Errorf("unexpected queued replies: %d", len(dev.inQueue))
	}
	if last := phases[len(phases)-1]; last != PhaseSettle {
		t.Errorf("last phase = %q, want %q", last, PhaseSettle)
	}
}

func TestFlashACKMode(t *testing.T) {
	t.Parallel()
	fw := &Firmware{Pages: [][]byte{page(0, 16)}}
	dev := &fakeBulk{respond: ackEverything}
	f := newTestFlasher(dev)
	if err := f.Flash(t.Context(), fw, true, nil); err != nil {
		t.Fatalf("Flash: %v", err)
	}
	if len(dev.sent) != ChunksPerPage+1 {
		t.Errorf("sent %d frames, want %d", len(dev.sent), ChunksPerPage+1)
	}
}

// In ACK mode a silent device must fail rather than corrupt the image quietly.
func TestFlashACKModeTimesOut(t *testing.T) {
	t.Parallel()
	fw := &Firmware{Pages: [][]byte{page(0, 16)}}
	f := newTestFlasher(&fakeBulk{})
	err := f.Flash(t.Context(), fw, true, nil)
	if !errors.Is(err, ErrACKTimeout) {
		t.Fatalf("error = %v, want an ACK timeout", err)
	}
}

func TestFlashRejectsInvalidImage(t *testing.T) {
	t.Parallel()
	f := newTestFlasher(&fakeBulk{})
	bad := &Firmware{Pages: [][]byte{make([]byte, 12)}}
	if err := f.Flash(t.Context(), bad, false, nil); err == nil {
		t.Fatal("expected an error for a page not divisible by 8")
	}
	if len(f.dev.(*fakeBulk).sent) != 0 {
		t.Error("an invalid image must not put anything on the wire")
	}
}

// A frame for a different command is not an error; the wait simply continues
// (SPEC.md §5.2).
func TestAwaitACKSkipsMismatchedCommand(t *testing.T) {
	t.Parallel()
	dev := &fakeBulk{inQueue: [][]byte{
		{0x02, 0x81},       // a stale commit ack
		{0x03, 0x02, 0x5A}, // the verify ack we are waiting for
	}}
	f := newTestFlasher(dev)
	resp, err := f.awaitACK(t.Context(), proto.CmdBootloaderVerify, time.Second)
	if err != nil {
		t.Fatalf("awaitACK: %v", err)
	}
	if !resp.HasCRC || resp.CRC != 0x5A {
		t.Errorf("CRC = 0x%02x has=%v, want 0x5A true", resp.CRC, resp.HasCRC)
	}
}

// A device that has left the bus can never answer, so the wait must abort on
// the first ErrNoDevice instead of polling out the whole budget in 5 ms hops
// (up to ~60 s across the split verify attempts). The sentinel has to survive
// the wrapping so ExitCode can classify the failure.
func TestAwaitACKFailsFastWhenDeviceGone(t *testing.T) {
	t.Parallel()
	dev := &fakeBulk{inErr: fmt.Errorf("fake transport: %w", usbfs.ErrNoDevice)}
	f := newTestFlasher(dev)
	_, err := f.awaitACK(t.Context(), proto.CmdBootloaderWriteChunk, time.Minute)
	if !errors.Is(err, usbfs.ErrNoDevice) {
		t.Fatalf("error = %v, want it to unwrap to usbfs.ErrNoDevice", err)
	}
	// The old code recorded the error as lastErr and kept polling, so the
	// budget-expiry message wrapped both sentinels: the errors.Is above would
	// pass either way. What distinguishes fail-fast is a single read and no
	// timeout dressing.
	if errors.Is(err, ErrACKTimeout) {
		t.Errorf("error = %v; a vanished device is not an ACK timeout", err)
	}
	if dev.inReads != 1 {
		t.Errorf("device polled %d times after ErrNoDevice, want exactly 1", dev.inReads)
	}
}

// An all-zero bulk packet — classic noise — leniently parses as command code
// 0, which is CMD_BOOTLOADER_WRITE_CHUNK; it must not satisfy the wait for a
// WRITE_CHUNK acknowledgement, or the paced ACK-mode re-flash after a CRC
// mismatch silently degrades to the unpaced streaming it exists to recover
// from (SPEC.md §10.5).
func TestAwaitACKIgnoresAllZeroNoise(t *testing.T) {
	t.Parallel()
	dev := &fakeBulk{inQueue: [][]byte{make([]byte, 64)}}
	f := newTestFlasher(dev)
	if _, err := f.awaitACK(t.Context(), proto.CmdBootloaderWriteChunk, 50*time.Millisecond); !errors.Is(err, ErrACKTimeout) {
		t.Fatalf("error = %v, want an ACK timeout: noise must match nothing", err)
	}

	// A well-formed code-0 acknowledgement after the noise still matches.
	dev = &fakeBulk{inQueue: [][]byte{make([]byte, 64), {0x02, 0x80}}}
	f = newTestFlasher(dev)
	resp, err := f.awaitACK(t.Context(), proto.CmdBootloaderWriteChunk, time.Second)
	if err != nil {
		t.Fatalf("awaitACK: %v", err)
	}
	if resp.Cmd != proto.CmdBootloaderWriteChunk || !resp.DeclaredValid {
		t.Errorf("resp = %+v, want a strictly valid WRITE_CHUNK ack", resp)
	}
}

// A verify response that declares only the preamble, padded out by the bus,
// must not verify anything: the padding byte used to be handed back as the
// CRC, and with zero padding that fabricated 0x00 could match a legitimate
// expected CRC of 0 and walk an unperformed verify through to
// CMD_BOOTLOAD_END. Timing out instead is the fail-safe direction.
func TestVerifyRejectsUnderDeclaredResponse(t *testing.T) {
	t.Parallel()
	dev := &fakeBulk{respond: func(out []byte) [][]byte {
		if proto.Cmd(out[1]&proto.CmdCodeMask) != proto.CmdBootloaderVerify {
			return nil
		}
		// Declared length 2: no CRC inside the frame; zeros after it are
		// endpoint padding.
		return [][]byte{{0x02, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}}
	}}
	f := newTestFlasher(dev)
	_, err := f.Verify(t.Context())
	if err == nil {
		t.Fatal("Verify returned a CRC fabricated from bus padding")
	}
	if !strings.Contains(err.Error(), "no CRC") {
		t.Errorf("error = %v, want it to say the response carried no CRC", err)
	}
}

func TestAwaitACKHonoursContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f := newTestFlasher(&fakeBulk{})
	if _, err := f.awaitACK(ctx, proto.CmdBootloaderVerify, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestVerifySendsWriteThenReadForm(t *testing.T) {
	t.Parallel()
	dev := &fakeBulk{respond: ackEverything}
	f := newTestFlasher(dev)
	crc, err := f.Verify(t.Context())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if crc != 0x5A {
		t.Errorf("crc = 0x%02x, want 0x5A", crc)
	}
	if len(dev.sent) != 2 {
		t.Fatalf("sent %d frames, want 2", len(dev.sent))
	}
	if !bytes.Equal(dev.sent[0], VerifyWriteFrame()) {
		t.Errorf("first frame = %s, want %s", proto.Hex(dev.sent[0]), proto.Hex(VerifyWriteFrame()))
	}
	if !bytes.Equal(dev.sent[1], VerifyReadFrame()) {
		t.Errorf("second frame = %s, want %s", proto.Hex(dev.sent[1]), proto.Hex(VerifyReadFrame()))
	}
}

func TestVerifyRetriesThenFails(t *testing.T) {
	t.Parallel()
	dev := &fakeBulk{}
	f := newTestFlasher(dev)
	if _, err := f.Verify(t.Context()); err == nil {
		t.Fatal("expected a verification failure from a silent device")
	}
	// Two rounds of the write/read pair.
	if len(dev.sent) != 2*verifyAttempts {
		t.Errorf("sent %d frames, want %d", len(dev.sent), 2*verifyAttempts)
	}
}

func TestSerial(t *testing.T) {
	t.Parallel()
	dev := &fakeBulk{respond: ackEverything}
	f := newTestFlasher(dev)
	s, err := f.Serial(t.Context())
	if err != nil {
		t.Fatalf("Serial: %v", err)
	}
	if s != "VF001234" {
		t.Errorf("serial = %q, want VF001234", s)
	}
	if !bytes.Equal(dev.sent[0], []byte{0x02, 0x08}) {
		t.Errorf("frame = %s, want 02 08", proto.Hex(dev.sent[0]))
	}
}

func TestSerialRejectsUnusableString(t *testing.T) {
	t.Parallel()
	dev := &fakeBulk{respond: func(out []byte) [][]byte {
		return [][]byte{{0x04, 0x08, 'A', 'B'}} // too short to trust
	}}
	f := newTestFlasher(dev)
	if _, err := f.Serial(t.Context()); err == nil {
		t.Fatal("expected an error for a two-character serial")
	}
}

func TestJumpToApp(t *testing.T) {
	t.Parallel()
	dev := &fakeBulk{}
	f := newTestFlasher(dev)
	if err := f.JumpToApp(t.Context()); err != nil {
		t.Fatalf("JumpToApp: %v", err)
	}
	if len(dev.sent) != 1 || !bytes.Equal(dev.sent[0], []byte{0x02, 0x03}) {
		t.Errorf("sent %v, want a single 02 03", dev.sent)
	}
}

func TestNewFlasherWithoutDevice(t *testing.T) {
	t.Parallel()
	f := NewFlasher(nil, usbfs.Interface{})
	if err := f.JumpToApp(t.Context()); err == nil {
		t.Fatal("expected an error with no device")
	}
	if _, err := ReadSerial(t.Context(), nil, usbfs.Interface{}); err == nil {
		t.Fatal("ReadSerial: expected an error with no device")
	}
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

func TestUpdateSuccess(t *testing.T) {
	t.Parallel()
	fw := &Firmware{Pages: [][]byte{page(0, 16)}, Version: "5.1.0", CRC: 0x5A, CRCKnown: true}
	dev := &fakeBulk{respond: ackEverything}
	f := newTestFlasher(dev)

	res, err := update(t.Context(), f, fw, UpdateOptions{ExpectSerial: "VF001234"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !res.CRCChecked || res.CRC != 0x5A {
		t.Errorf("CRC = 0x%02x checked=%v, want 0x5A true", res.CRC, res.CRCChecked)
	}
	if !res.Verifiable || res.ExpectedCRC != 0x5A || res.Unverified {
		t.Errorf("verifiable=%v expected=0x%02x unverified=%v, want true 0x5A false",
			res.Verifiable, res.ExpectedCRC, res.Unverified)
	}
	if res.Reflashed {
		t.Error("Reflashed = true, want false")
	}
	if !res.JumpedToApp {
		t.Error("JumpedToApp = false, want true")
	}
	if !bytes.Equal(dev.sent[len(dev.sent)-1], BootloadEndFrame()) {
		t.Errorf("last frame = %s, want CMD_BOOTLOAD_END",
			proto.Hex(dev.sent[len(dev.sent)-1]))
	}
}

func TestUpdateSerialMismatchAbortsBeforeWriting(t *testing.T) {
	t.Parallel()
	fw := &Firmware{Pages: [][]byte{page(0, 16)}, CRC: 0x5A, CRCKnown: true}
	dev := &fakeBulk{respond: ackEverything}
	f := newTestFlasher(dev)

	_, err := update(t.Context(), f, fw, UpdateOptions{ExpectSerial: "OTHER123"})
	if !errors.Is(err, ErrSerialMismatch) {
		t.Fatalf("error = %v, want a serial mismatch", err)
	}
	// Only the serial read should have gone out.
	if len(dev.sent) != 1 {
		t.Errorf("sent %d frames, want only the serial read", len(dev.sent))
	}
}

// SPEC.md §10.5 / §13 interlock 6: a unit whose CRC never matches must be left
// in the bootloader, so CMD_BOOTLOAD_END must not be sent.
func TestUpdateCRCMismatchNeverSendsBootloadEnd(t *testing.T) {
	t.Parallel()
	fw := &Firmware{Pages: [][]byte{page(0, 16)}, CRC: 0x11, CRCKnown: true}
	dev := &fakeBulk{respond: ackEverything} // always answers 0x5A
	f := newTestFlasher(dev)

	res, err := update(t.Context(), f, fw, UpdateOptions{})
	if !errors.Is(err, ErrCRCMismatch) {
		t.Fatalf("error = %v, want a CRC mismatch", err)
	}
	if !res.Reflashed {
		t.Error("Reflashed = false; the image should have been retried in ACK mode")
	}
	if res.JumpedToApp {
		t.Error("JumpedToApp = true after a CRC mismatch")
	}
	for _, frame := range dev.sent {
		if bytes.Equal(frame, BootloadEndFrame()) {
			t.Fatal("CMD_BOOTLOAD_END was sent after a CRC mismatch")
		}
	}
	// The message has to tell the user the unit is recoverable.
	for _, want := range []string{"bootloader mode", "re-flashed"} {
		if !bytes.Contains([]byte(err.Error()), []byte(want)) {
			t.Errorf("error %q should mention %q", err.Error(), want)
		}
	}
}

// Force is for an image nothing *can* check. It must never turn a device that
// answered with the wrong CRC into a device that gets told to run the image
// (SPEC.md §10.5, §13 interlock 6).
func TestUpdateForceDoesNotBypassCRCMismatch(t *testing.T) {
	t.Parallel()
	fw := &Firmware{Pages: [][]byte{page(0, 16)}, CRC: 0x11, CRCKnown: true}
	dev := &fakeBulk{respond: ackEverything} // always answers 0x5A
	f := newTestFlasher(dev)

	res, err := update(t.Context(), f, fw, UpdateOptions{Force: true})
	if !errors.Is(err, ErrCRCMismatch) {
		t.Fatalf("error = %v, want a CRC mismatch", err)
	}
	if res.JumpedToApp || dev.hasSent(BootloadEndFrame()) {
		t.Fatal("CMD_BOOTLOAD_END was sent after a CRC mismatch because Force was set")
	}
}

// A raw .bin has no CRC to compare against. Refusing outright would break the
// documented --force behaviour, but the flash cannot be silent about it: the
// risk is stated through the Log callback and nowhere else.
func TestUpdateForceFlashesUnverifiableImageWithAWarning(t *testing.T) {
	t.Parallel()
	fw := &Firmware{Pages: [][]byte{page(0, 16)}}
	dev := &fakeBulk{respond: ackEverything}
	f := newTestFlasher(dev)

	logged, logf := newLog()
	res, err := update(t.Context(), f, fw, UpdateOptions{Force: true, Log: logf})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.CRCChecked || !res.Unverified || res.Verifiable {
		t.Errorf("checked=%v unverified=%v verifiable=%v, want false true false",
			res.CRCChecked, res.Unverified, res.Verifiable)
	}
	if dev.hasSent(VerifyWriteFrame()) || dev.hasSent(VerifyReadFrame()) {
		t.Error("verification was attempted for an image with no CRC")
	}
	if !res.JumpedToApp {
		t.Error("JumpedToApp = false; --force is documented to start the image")
	}
	// The warning has to say both what was not done and what it costs.
	for _, want := range []string{"declares no CRC", "WARNING", "will NOT be detected"} {
		if !logged.contains(want) {
			t.Errorf("log %q should mention %q", *logged, want)
		}
	}
}

// Without Force there is no route to starting an unverifiable image, and the
// refusal has to come before the first WRITE_CHUNK: a device rejected only
// after it has been half-written is the worst outcome available.
func TestUpdateRefusesUnverifiableImageBeforeWriting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fw   *Firmware
		opts UpdateOptions
	}{
		{
			name: "no CRC anywhere",
			fw:   &Firmware{Pages: [][]byte{page(0, 16)}},
		},
		{
			// SkipVerify on an image that *could* be checked ends in the same
			// place: an image nothing verified, about to be started.
			name: "verification deliberately skipped",
			fw:   &Firmware{Pages: [][]byte{page(0, 16)}, CRC: 0x5A, CRCKnown: true},
			opts: UpdateOptions{SkipVerify: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dev := &fakeBulk{respond: ackEverything}
			f := newTestFlasher(dev)
			res, err := update(t.Context(), f, tc.fw, tc.opts)
			if !errors.Is(err, ErrUnverifiable) {
				t.Fatalf("error = %v, want ErrUnverifiable", err)
			}
			if len(dev.sent) != 0 {
				t.Errorf("sent %d frames; nothing may be written before the refusal", len(dev.sent))
			}
			if res.JumpedToApp {
				t.Error("JumpedToApp = true after a refusal")
			}
		})
	}
}

// The dangerous direction is skipping a check that was available. Nothing but
// an explicit SkipVerify may suppress verification of an image that carries a
// CRC — Force in particular must not, because Force is about images that
// cannot be checked at all.
func TestUpdateAlwaysVerifiesWhenACRCIsAvailable(t *testing.T) {
	t.Parallel()
	override := uint8(0x5A)
	tests := []struct {
		name string
		fw   *Firmware
		opts UpdateOptions
	}{
		{"plain", &Firmware{Pages: [][]byte{page(0, 16)}, CRC: 0x5A, CRCKnown: true}, UpdateOptions{}},
		{"force set", &Firmware{Pages: [][]byte{page(0, 16)}, CRC: 0x5A, CRCKnown: true}, UpdateOptions{Force: true}},
		{"ack mode", &Firmware{Pages: [][]byte{page(0, 16)}, CRC: 0x5A, CRCKnown: true}, UpdateOptions{ACKMode: true}},
		{"skip jump", &Firmware{Pages: [][]byte{page(0, 16)}, CRC: 0x5A, CRCKnown: true}, UpdateOptions{SkipJump: true}},
		// An override supplies a CRC for an image that declared none, and that
		// image is then verified like any other.
		{"crc supplied for a raw image", &Firmware{Pages: [][]byte{page(0, 16)}}, UpdateOptions{CRC: &override}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dev := &fakeBulk{respond: ackEverything}
			f := newTestFlasher(dev)
			res, err := update(t.Context(), f, tc.fw, tc.opts)
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if !res.CRCChecked {
				t.Error("CRCChecked = false; verification must not be skipped")
			}
			if res.Unverified {
				t.Error("Unverified = true although a CRC was available")
			}
			if !dev.hasSent(VerifyWriteFrame()) || !dev.hasSent(VerifyReadFrame()) {
				t.Error("neither verify frame was sent")
			}
		})
	}
}

// An override also overrides the image's own CRC, so a stale value in the
// payload cannot mask a bad flash.
func TestUpdateCRCOverrideBeatsTheImage(t *testing.T) {
	t.Parallel()
	wrong := uint8(0x11)
	fw := &Firmware{Pages: [][]byte{page(0, 16)}, CRC: 0x5A, CRCKnown: true}
	dev := &fakeBulk{respond: ackEverything} // answers 0x5A
	f := newTestFlasher(dev)

	res, err := update(t.Context(), f, fw, UpdateOptions{CRC: &wrong})
	if !errors.Is(err, ErrCRCMismatch) {
		t.Fatalf("error = %v, want a CRC mismatch against the override", err)
	}
	if res.ExpectedCRC != wrong {
		t.Errorf("ExpectedCRC = 0x%02x, want the override 0x%02x", res.ExpectedCRC, wrong)
	}
}

// SkipJump means nothing unverified is ever run, so an unverifiable image needs
// no Force: the unit is left in the bootloader, where it stays re-flashable.
func TestUpdateSkipJumpAllowsUnverifiableImage(t *testing.T) {
	t.Parallel()
	fw := &Firmware{Pages: [][]byte{page(0, 16)}}
	dev := &fakeBulk{respond: ackEverything}
	f := newTestFlasher(dev)

	logged, logf := newLog()
	res, err := update(t.Context(), f, fw, UpdateOptions{SkipJump: true, Log: logf})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.JumpedToApp || dev.hasSent(BootloadEndFrame()) {
		t.Error("CMD_BOOTLOAD_END was sent despite SkipJump")
	}
	if !res.Unverified {
		t.Error("Unverified = false for an image with no CRC")
	}
	if !logged.contains("declares no CRC") {
		t.Errorf("log %q should still warn that nothing was verified", *logged)
	}
}

// SPEC.md §10.1: POST_FLASH_DELAY_MS is 2 s, once per flash. It used to be
// applied twice — Flash ends with it and the caller waited again — so every
// flash settled for 4 s.
func TestUpdateAppliesPostFlashDelayOncePerFlash(t *testing.T) {
	t.Parallel()

	t.Run("clean flash", func(t *testing.T) {
		t.Parallel()
		fw := &Firmware{Pages: [][]byte{page(0, 16)}, CRC: 0x5A, CRCKnown: true}
		f, seen := newTimedFlasher(&fakeBulk{respond: ackEverything})
		if _, err := update(t.Context(), f, fw, UpdateOptions{}); err != nil {
			t.Fatalf("update: %v", err)
		}
		if n := countDelay(*seen, PostFlashDelay); n != 1 {
			t.Errorf("waited PostFlashDelay %d times, want exactly 1 (delays: %v)", n, *seen)
		}
	})

	t.Run("re-flashed after a mismatch", func(t *testing.T) {
		t.Parallel()
		fw := &Firmware{Pages: [][]byte{page(0, 16)}, CRC: 0x11, CRCKnown: true}
		f, seen := newTimedFlasher(&fakeBulk{respond: ackEverything}) // answers 0x5A
		if _, err := update(t.Context(), f, fw, UpdateOptions{}); !errors.Is(err, ErrCRCMismatch) {
			t.Fatalf("error = %v, want a CRC mismatch", err)
		}
		// Two flashes, two settles: one each, still never two for one flash.
		if n := countDelay(*seen, PostFlashDelay); n != 2 {
			t.Errorf("waited PostFlashDelay %d times for two flashes, want 2 (delays: %v)", n, *seen)
		}
	})
}

// Geometry is the one thing verification cannot warn about afterwards, so it is
// checked before the first frame rather than page by page as the flash runs.
func TestUpdateValidatesGeometryBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fw   *Firmware
	}{
		{"page not divisible by eight", &Firmware{Pages: [][]byte{make([]byte, 12)}, CRC: 1, CRCKnown: true}},
		{"unequal pages", &Firmware{
			Pages:    [][]byte{make([]byte, 16), make([]byte, 8)},
			CRC:      1,
			CRCKnown: true,
		}},
		{"chunks too big for one frame", &Firmware{
			Pages:    [][]byte{make([]byte, (MaxChunkSize+1)*ChunksPerPage)},
			CRC:      1,
			CRCKnown: true,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dev := &fakeBulk{respond: ackEverything}
			f := newTestFlasher(dev)
			if _, err := update(t.Context(), f, tc.fw, UpdateOptions{}); err == nil {
				t.Fatal("expected the image to be rejected")
			}
			if len(dev.sent) != 0 {
				t.Errorf("sent %d frames; a bad geometry must be caught before the first write", len(dev.sent))
			}
		})
	}
}

func TestUpdateSkipJumpLeavesDeviceInBootloader(t *testing.T) {
	t.Parallel()
	fw := &Firmware{Pages: [][]byte{page(0, 16)}, CRC: 0x5A, CRCKnown: true}
	dev := &fakeBulk{respond: ackEverything}
	f := newTestFlasher(dev)

	res, err := update(t.Context(), f, fw, UpdateOptions{SkipJump: true})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.JumpedToApp {
		t.Error("JumpedToApp = true despite SkipJump")
	}
	for _, frame := range dev.sent {
		if bytes.Equal(frame, BootloadEndFrame()) {
			t.Fatal("CMD_BOOTLOAD_END was sent despite SkipJump")
		}
	}
}

func TestUpdateRejectsNilFirmware(t *testing.T) {
	t.Parallel()
	f := newTestFlasher(&fakeBulk{})
	if _, err := update(t.Context(), f, nil, UpdateOptions{}); err == nil {
		t.Fatal("expected an error for a nil image")
	}
}

func TestPickBootloaderInterface(t *testing.T) {
	t.Parallel()
	if _, ok := PickBootloaderInterface(nil); ok {
		t.Error("a nil config must not match")
	}
	cfg := &usbfs.Config{Interfaces: []usbfs.Interface{
		// Not vendor class: the MIDI interface.
		{Number: 0, Class: 0x01, SubClass: 0x03, Endpoints: []usbfs.Endpoint{
			{Address: 0x02, Attributes: 0x02}, {Address: 0x82, Attributes: 0x02},
		}},
		// Vendor class but OUT only.
		{Number: 1, Class: 0xFF, Endpoints: []usbfs.Endpoint{{Address: 0x03, Attributes: 0x02}}},
		// The one we want.
		{Number: 2, Class: 0xFF, Endpoints: []usbfs.Endpoint{
			{Address: 0x04, Attributes: 0x02}, {Address: 0x84, Attributes: 0x02},
		}},
	}}
	iface, ok := PickBootloaderInterface(cfg)
	if !ok {
		t.Fatal("expected a match")
	}
	if iface.Number != 2 {
		t.Errorf("picked interface %d, want 2", iface.Number)
	}
}

// usbfs flattens every configuration of a device into one descriptor blob, and
// bInterfaceNumber is only unique within a configuration. A vendor-class
// interface belonging to a configuration the device is not in must not be
// picked: claiming it by number would claim whatever wears that number in the
// live configuration, which here is an audio-class interface with a different
// endpoint pair — i.e. firmware frames written to the wrong endpoints.
func TestPickBootloaderInterfaceIgnoresInactiveConfigurations(t *testing.T) {
	t.Parallel()
	bootIface := usbfs.Interface{
		Number: 1, Class: 0xFF, ConfigurationValue: 1,
		Endpoints: []usbfs.Endpoint{
			{Address: 0x01, Attributes: 0x02}, {Address: 0x81, Attributes: 0x02},
		},
	}
	otherIface := usbfs.Interface{
		Number: 1, Class: 0x01, SubClass: 0x03, ConfigurationValue: 2,
		Endpoints: []usbfs.Endpoint{
			{Address: 0x02, Attributes: 0x02}, {Address: 0x82, Attributes: 0x02},
		},
	}
	cfg := &usbfs.Config{
		Interfaces: []usbfs.Interface{bootIface, otherIface},
		Configurations: []usbfs.Configuration{
			{Value: 1, Interfaces: []usbfs.Interface{bootIface}},
			{Value: 2, Interfaces: []usbfs.Interface{otherIface}},
		},
	}

	// Configuration 2 is live: nothing vendor-class is reachable.
	cfg.Active = 2
	if iface, ok := PickBootloaderInterface(cfg); ok {
		t.Errorf("picked %+v from an inactive configuration; want no match", iface)
	}

	// Configuration 1 is live: the same descriptors now do match.
	cfg.Active = 1
	iface, ok := PickBootloaderInterface(cfg)
	if !ok {
		t.Fatal("expected a match in the active configuration")
	}
	if iface.Number != 1 || iface.ConfigurationValue != 1 {
		t.Errorf("picked interface %d of configuration %d, want 1 of 1",
			iface.Number, iface.ConfigurationValue)
	}

	// Active 0 means "not determined", not "configuration 0": the filter is off
	// and the union is searched, which is what every single-configuration device
	// and every pre-existing caller relies on.
	cfg.Active = 0
	if _, ok := PickBootloaderInterface(cfg); !ok {
		t.Error("an undetermined active configuration must not filter everything out")
	}
}

// An interface with ConfigurationValue 0 appeared before any configuration
// descriptor: a malformed blob that usbfs.ParseDescriptors deliberately keeps
// so the device stays drivable. The active-configuration filter must not throw
// it away — it belongs to no inactive configuration, it is unattributed — or
// the salvage is undone and a device we could still flash reports "no
// vendor-class interface".
func TestPickBootloaderInterfaceKeepsOrphanInterfaces(t *testing.T) {
	t.Parallel()
	orphan := usbfs.Interface{
		Number: 3, Class: 0xFF, ConfigurationValue: 0,
		Endpoints: []usbfs.Endpoint{
			{Address: 0x01, Attributes: 0x02}, {Address: 0x81, Attributes: 0x02},
		},
	}
	audio := usbfs.Interface{
		Number: 0, Class: 0x01, SubClass: 0x03, ConfigurationValue: 1,
		Endpoints: []usbfs.Endpoint{
			{Address: 0x02, Attributes: 0x02}, {Address: 0x82, Attributes: 0x02},
		},
	}
	cfg := &usbfs.Config{Active: 1, Interfaces: []usbfs.Interface{orphan, audio}}
	iface, ok := PickBootloaderInterface(cfg)
	if !ok {
		t.Fatal("the orphan vendor-class interface was filtered out with the inactive configurations")
	}
	if iface.Number != 3 || iface.ConfigurationValue != 0 {
		t.Errorf("picked interface %d of configuration %d, want the orphan 3 of 0",
			iface.Number, iface.ConfigurationValue)
	}

	// When an interface provably in the active configuration also qualifies,
	// it wins over the orphan — even when the orphan comes first in
	// descriptor order.
	exact := usbfs.Interface{
		Number: 2, Class: 0xFF, ConfigurationValue: 1,
		Endpoints: []usbfs.Endpoint{
			{Address: 0x03, Attributes: 0x02}, {Address: 0x83, Attributes: 0x02},
		},
	}
	cfg = &usbfs.Config{Active: 1, Interfaces: []usbfs.Interface{orphan, exact}}
	iface, ok = PickBootloaderInterface(cfg)
	if !ok {
		t.Fatal("expected a match")
	}
	if iface.Number != 2 || iface.ConfigurationValue != 1 {
		t.Errorf("picked interface %d of configuration %d, want the exact match 2 of 1",
			iface.Number, iface.ConfigurationValue)
	}

	// An interface genuinely in an inactive configuration is still rejected:
	// the orphan exemption is only for ConfigurationValue 0.
	inactive := usbfs.Interface{
		Number: 1, Class: 0xFF, ConfigurationValue: 2,
		Endpoints: []usbfs.Endpoint{
			{Address: 0x04, Attributes: 0x02}, {Address: 0x84, Attributes: 0x02},
		},
	}
	cfg = &usbfs.Config{Active: 1, Interfaces: []usbfs.Interface{inactive, audio}}
	if iface, ok := PickBootloaderInterface(cfg); ok {
		t.Errorf("picked %+v from an inactive configuration; want no match", iface)
	}
}

// The error for "no bootloader interface here" has to distinguish a unit still
// in application mode from a unit whose bootloader interface is in a
// configuration it is not currently in -- the second is invisible otherwise,
// since the interface is plainly there in the descriptors.
func TestOtherConfigurationNote(t *testing.T) {
	t.Parallel()
	vendor := usbfs.Interface{Number: 1, Class: 0xFF, ConfigurationValue: 1}
	audio := usbfs.Interface{Number: 1, Class: 0x01, SubClass: 0x03, ConfigurationValue: 2}

	multi := &usbfs.Config{Configurations: []usbfs.Configuration{
		{Value: 1, Interfaces: []usbfs.Interface{vendor}},
		{Value: 2, Interfaces: []usbfs.Interface{audio}},
	}}

	multi.Active = 2
	got := otherConfigurationNote(multi)
	if !strings.Contains(got, "configuration 2") || !strings.Contains(got, "configuration 1 does declare") {
		t.Errorf("note = %q; want it to name the active configuration and the one that has the interface", got)
	}

	// Active configuration has no vendor-class interface and neither does any
	// other: still worth saying how many there are, but no false lead.
	noneAnywhere := &usbfs.Config{Active: 2, Configurations: []usbfs.Configuration{
		{Value: 1, Interfaces: []usbfs.Interface{audio}},
		{Value: 2, Interfaces: []usbfs.Interface{audio}},
	}}
	got = otherConfigurationNote(noneAnywhere)
	if !strings.Contains(got, "configuration 2") || strings.Contains(got, "does declare") {
		t.Errorf("note = %q; want no claim that another configuration has one", got)
	}

	// The ordinary case must not grow any of this.
	single := &usbfs.Config{Active: 1, Configurations: []usbfs.Configuration{
		{Value: 1, Interfaces: []usbfs.Interface{audio}},
	}}
	if got := otherConfigurationNote(single); got != "" {
		t.Errorf("single-configuration note = %q, want empty", got)
	}
	multi.Active = 0
	if got := otherConfigurationNote(multi); got != "" {
		t.Errorf("undetermined-configuration note = %q, want empty", got)
	}
}

// A device that leaves the bus mid-verify fails the whole Verify at once.
// Before this was pinned, awaitACK's ErrNoDevice was swallowed into lastErr
// and the loop spent another round pause plus a doomed send on a unit that
// was definitively gone.
func TestVerifyAbortsWhenTheDeviceLeavesTheBus(t *testing.T) {
	dev := &fakeBulk{inErr: usbfs.ErrNoDevice}
	f := newTestFlasher(dev)
	_, err := f.Verify(context.Background())
	if !errors.Is(err, usbfs.ErrNoDevice) {
		t.Fatalf("error = %v, want usbfs.ErrNoDevice", err)
	}
	// One verify round only: the write form and the read form, no second
	// attempt against a vanished device.
	if n := len(dev.sent); n != 2 {
		t.Errorf("sent %d frames, want 2 (one round); a gone device earns no retry round", n)
	}
}

// TestInApplicationModeUsesTheMIDIInterface pins the application-vs-bootloader
// discriminator against descriptors measured from a real VFLEX.
//
// The device (APP.05.00.00, PID 0x800F, observed 2026-08-21) exposes its
// vendor-class 0xFF interface WHILE RUNNING THE APPLICATION, which is why
// picking by class alone is not enough -- see InApplicationMode.
func TestInApplicationModeUsesTheMIDIInterface(t *testing.T) {
	// Verbatim from the real unit: audio control, MIDIStreaming, vendor class.
	appMode := &usbfs.Config{Interfaces: []usbfs.Interface{
		{Number: 0, Class: 0x01, SubClass: 0x01},
		{Number: 1, Class: 0x01, SubClass: 0x03, Endpoints: []usbfs.Endpoint{
			{Address: 0x02, Attributes: 0x02}, {Address: 0x83, Attributes: 0x02}}},
		{Number: 2, Class: 0xFF, SubClass: 0x00, Endpoints: []usbfs.Endpoint{
			{Address: 0x01, Attributes: 0x02}, {Address: 0x81, Attributes: 0x02}}},
	}}
	if !InApplicationMode(appMode) {
		t.Error("a unit presenting a MIDIStreaming interface must read as application mode")
	}
	// PickBootloaderInterface still finds the vendor interface -- which is
	// exactly the hazard the guard exists for, so pin that too.
	if _, ok := PickBootloaderInterface(appMode); !ok {
		t.Error("PickBootloaderInterface should still select the vendor interface; " +
			"the guard, not the picker, is what refuses application mode")
	}

	// Bootloader mode: SPEC.md §10.1 says the MIDI interface is gone.
	blMode := &usbfs.Config{Interfaces: []usbfs.Interface{
		{Number: 0, Class: 0xFF, SubClass: 0x00, Endpoints: []usbfs.Endpoint{
			{Address: 0x01, Attributes: 0x02}, {Address: 0x81, Attributes: 0x02}}},
	}}
	if InApplicationMode(blMode) {
		t.Error("a unit with no MIDI interface must read as bootloader mode")
	}
	if _, ok := PickBootloaderInterface(blMode); !ok {
		t.Error("the bootloader interface must still be selectable")
	}
	if InApplicationMode(nil) {
		t.Error("a nil config must not read as application mode")
	}
}
