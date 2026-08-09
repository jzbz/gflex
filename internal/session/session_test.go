package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
)

// TestACKMismatchKeepsWaiting: a frame whose command code does not match is not
// an error. The reference client logs "ack_cmd_mismatch", drops the frame and
// leaves the wait pending so a later matching frame still satisfies it
// (SPEC.md §5.2).
func TestACKMismatchKeepsWaiting(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: 2 * time.Second})
	d.SetHandler(proto.CmdSerialNumber, func(proto.Frame) []byte {
		// Two decoys carrying other command codes, then the real answer.
		d.Emit(mustBuild(proto.CmdVoltageMv, proto.EncodeU16(5000), false))
		d.Emit(mustBuild(proto.CmdCurrentLimitMa, proto.EncodeU16(5000), false))
		return mustBuild(proto.CmdSerialNumber, []byte("VF999999"), false)
	})

	got, err := s.SerialNumber(context.Background())
	if err != nil {
		t.Fatalf("SerialNumber: %v", err)
	}
	if got != "VF999999" {
		t.Errorf("SerialNumber = %q, want \"VF999999\"", got)
	}
}

// TestACKMismatchThenTimeout: when the matching frame never arrives, the
// mismatches alone must not fail the call early -- the deadline does.
func TestACKMismatchThenTimeout(t *testing.T) {
	const timeout = 200 * time.Millisecond
	s, d := newTestSession(t, Options{Timeout: timeout})
	d.SetHandler(proto.CmdSerialNumber, func(proto.Frame) []byte {
		return mustBuild(proto.CmdVoltageMv, proto.EncodeU16(5000), false)
	})

	start := time.Now()
	_, err := s.SerialNumber(context.Background())
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want it to wrap ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < timeout {
		t.Errorf("failed after %v, want it to wait out the full %v", elapsed, timeout)
	}
	// The message must name the command and the transmitted frame so an
	// operator can correlate it with a --verbose trace.
	if !strings.Contains(err.Error(), "CMD_SERIAL_NUMBER") {
		t.Errorf("error = %v, want the command name in it", err)
	}
	if !strings.Contains(err.Error(), "02 08") {
		t.Errorf("error = %v, want the tx frame hex in it", err)
	}
	if !strings.Contains(err.Error(), "Response timeout exceeded") {
		t.Errorf("error = %v, want the vendor's own timeout wording", err)
	}
}

// TestNoResponseTimesOut is the plain case: the protocol has no NACK, so an
// unanswered command can only fail by deadline. ReadyTimeout is cut down
// because VoltageMv keeps retrying a silent device for its whole budget.
func TestNoResponseTimesOut(t *testing.T) {
	s, _ := newTestSession(t, Options{Timeout: 100 * time.Millisecond, ReadyTimeout: 50 * time.Millisecond})
	_, err := s.VoltageMv(context.Background())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
}

// TestShortFrameDoesNotSatisfy: a sub-preamble frame is ignored without
// clearing the pending state (SPEC.md §5.2), so the command still times out
// rather than returning garbage.
func TestShortFrameDoesNotSatisfy(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: 200 * time.Millisecond})
	d.SetHandler(proto.CmdSerialNumber, func(proto.Frame) []byte {
		return []byte{0x08} // one byte: shorter than the two-byte preamble
	})

	if _, err := s.SerialNumber(context.Background()); !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
}

// TestStaleFrameDropped: a response that arrives after its command has already
// timed out must not satisfy the *next* command of the same code. The reference
// client calls this "unexpected_frame_while_waiting".
func TestStaleFrameDropped(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: 100 * time.Millisecond})

	var once sync.Once
	d.SetHandler(proto.CmdSerialNumber, func(proto.Frame) []byte {
		once.Do(func() {
			// Answer long after the first command has given up.
			go func() {
				time.Sleep(200 * time.Millisecond)
				d.Emit(mustBuild(proto.CmdSerialNumber, []byte("STALE001"), false))
			}()
		})
		return nil
	})

	if _, err := s.SerialNumber(context.Background()); !errors.Is(err, ErrTimeout) {
		t.Fatalf("first read: error = %v, want ErrTimeout", err)
	}
	time.Sleep(250 * time.Millisecond) // let the stale answer land

	got, err := s.SerialNumber(context.Background())
	if err == nil {
		t.Fatalf("second read returned %q; the stale frame must not satisfy it", got)
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("second read: error = %v, want ErrTimeout", err)
	}
}

// TestUnplugMidCommandReportsTheCause: when the link dies while a command is
// outstanding, the framer's reader publishes the real error and then closes its
// channels. await must carry that error out with ErrTransportClosed -- reporting
// only "transport closed" leaves the user to guess between an unplugged device,
// a lost USB link and a bug, when the transport already said which.
func TestUnplugMidCommandReportsTheCause(t *testing.T) {
	unplugged := errors.New("read /dev/snd/midiC1D0: no such device")

	d := newHotplugDev()
	s := New(d, Options{ByteDelay: time.Nanosecond, Timeout: 5 * time.Second})
	t.Cleanup(func() { _ = s.Close() })

	// Unplug once the command is on the wire: before that, the stale-frame
	// drain at the head of exchange would legitimately swallow the error.
	go func() {
		<-d.wrote
		d.Unplug(unplugged)
	}()

	start := time.Now()
	_, err := s.SerialNumber(context.Background())
	if err == nil {
		t.Fatal("want an error when the transport dies mid-command")
	}
	if !errors.Is(err, ErrTransportClosed) {
		t.Errorf("error = %v, want it to wrap ErrTransportClosed", err)
	}
	if !errors.Is(err, unplugged) {
		t.Errorf("error = %v, want the underlying transport error carried through", err)
	}
	if !strings.Contains(err.Error(), "no such device") {
		t.Errorf("error = %v, want the cause visible in the message", err)
	}
	// It must not sit out the 5 s command timeout either.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v; a dead transport fails the command immediately", elapsed)
	}
}

// TestTraceHook checks that every frame in both directions is reported.
func TestTraceHook(t *testing.T) {
	var mu sync.Mutex
	type ev struct {
		dir   string
		frame string
	}
	var events []ev

	s, d := newTestSession(t, Options{
		Timeout: time.Second,
		Trace: func(dir string, frame []byte) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev{dir, proto.Hex(frame)})
		},
	})
	d.SetHandler(proto.CmdSerialNumber, func(proto.Frame) []byte {
		d.Emit(mustBuild(proto.CmdVoltageMv, proto.EncodeU16(5000), false)) // mismatched
		return mustBuild(proto.CmdSerialNumber, []byte("VF000001"), false)
	})

	if _, err := s.SerialNumber(context.Background()); err != nil {
		t.Fatalf("SerialNumber: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []ev{
		{"tx", "02 08"},
		{"rx", "04 12 13 88"}, // dropped for mismatch, but still traced
		{"rx", "0a 08 56 46 30 30 30 30 30 31"},
	}
	if len(events) != len(want) {
		t.Fatalf("trace = %+v, want %+v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("trace[%d] = %+v, want %+v", i, events[i], want[i])
		}
	}
}

// TestSingleFlight: the protocol has one pending slot and no tagging, so
// concurrent callers must be serialised. If they were not, one command's
// response would satisfy another's wait.
func TestSingleFlight(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: 2 * time.Second})

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	respond := func(cmd proto.Cmd, payload []byte) func(proto.Frame) []byte {
		return func(proto.Frame) []byte {
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			return mustBuild(cmd, payload, false)
		}
	}
	d.SetHandler(proto.CmdSerialNumber, respond(proto.CmdSerialNumber, []byte("VF000001")))
	d.SetHandler(proto.CmdCurrentLimitMa, respond(proto.CmdCurrentLimitMa, proto.EncodeU16(5000)))
	d.SetHandler(proto.CmdUserVLimit, respond(proto.CmdUserVLimit, proto.EncodeVLimit(3300, 48000)))

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 10; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _, err := s.SerialNumber(ctx); errs <- err }()
		go func() { defer wg.Done(); _, err := s.CurrentLimitMa(ctx); errs <- err }()
		go func() { defer wg.Done(); _, _, err := s.VLimit(ctx); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent command failed: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if maxInFlight != 1 {
		t.Errorf("saw %d commands in flight at once, want 1", maxInFlight)
	}
}

// TestContextCancellation: a cancelled context must abort the wait without
// burning the full timeout.
func TestContextCancellation(t *testing.T) {
	s, _ := newTestSession(t, Options{Timeout: 10 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := s.Do(ctx, proto.CmdSerialNumber, nil, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v; the cancellation should not wait out the timeout", elapsed)
	}
}

// TestSendNoACKDoesNotWait: the bootloader jump has no response by design.
func TestSendNoACKDoesNotWait(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: 10 * time.Second})

	start := time.Now()
	if err := s.SendNoACK(context.Background(), []byte{0x02, 0x14}); err != nil {
		t.Fatalf("SendNoACK: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("SendNoACK took %v; it must not wait for a response", elapsed)
	}
	if got := d.SentHex(); len(got) != 1 || got[0] != "02 14" {
		t.Errorf("tx = %q, want [\"02 14\"]", got)
	}
}

// TestClosedSessionRefuses: after Close every command reports the vendor's
// "No connection established" rather than waiting out a timeout.
func TestClosedSessionRefuses(t *testing.T) {
	s, _ := newTestSession(t, Options{Timeout: 5 * time.Second})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	_, err := s.SerialNumber(context.Background())
	if !errors.Is(err, ErrNoConnection) {
		t.Fatalf("error = %v, want ErrNoConnection", err)
	}
}

// TestDoRawScratchpad: the scratchpad flag is never set by any typed accessor,
// but the raw path can reach it. 0x40 | 0x12 = 0x52.
func TestDoRawScratchpad(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: 500 * time.Millisecond})
	d.SetHandler(proto.CmdVoltageMv, func(proto.Frame) []byte {
		return mustBuild(proto.CmdVoltageMv, proto.EncodeU16(5000), false)
	})

	if _, err := s.DoRaw(context.Background(), proto.CmdVoltageMv, nil, false, true); err != nil {
		t.Fatalf("DoRaw: %v", err)
	}
	if got := d.SentHex(); len(got) != 1 || got[0] != "02 52" {
		t.Errorf("tx = %q, want [\"02 52\"]", got)
	}
}

// TestDefaultsApplied checks the zero-value Options substitutions.
func TestDefaultsApplied(t *testing.T) {
	d := newFakeDev()
	s := New(d, Options{})
	t.Cleanup(func() { _ = s.Close() })
	if s.timeout != proto.DefaultTimeout {
		t.Errorf("timeout = %v, want %v", s.timeout, proto.DefaultTimeout)
	}
}

// TestPDOChunkTimeoutSelection: PDO chunks get 8 s rather than the ordinary
// 5 s, but an operator's longer --timeout still wins.
func TestPDOChunkTimeoutSelection(t *testing.T) {
	s, _ := newTestSession(t, Options{Timeout: time.Second})
	if got := s.pdoChunkTimeout(); got != proto.PDOChunkTimeout {
		t.Errorf("pdoChunkTimeout = %v, want %v", got, proto.PDOChunkTimeout)
	}

	s2, _ := newTestSession(t, Options{Timeout: 30 * time.Second})
	if got := s2.pdoChunkTimeout(); got != 30*time.Second {
		t.Errorf("pdoChunkTimeout = %v, want the configured 30s", got)
	}
}
