package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/transport/fake"
)

// TestACKMismatchKeepsWaiting: a frame whose command code does not match is not
// an error. The reference client logs "ack_cmd_mismatch", drops the frame and
// leaves the wait pending so a later matching frame still satisfies it
// (SPEC.md §5.2).
func TestACKMismatchKeepsWaiting(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: 2 * time.Second})
	d.SetHandler(proto.CmdSerialNumber, func(proto.Frame) []byte {
		// Two decoys carrying other command codes, then the real answer. The
		// decoys are frames the Device would never build for this request, so
		// they are pushed raw; Push queues them ahead of the handler's reply.
		_ = d.Push(mustBuild(proto.CmdVoltageMv, proto.EncodeU16(5000), false))
		_ = d.Push(mustBuild(proto.CmdCurrentLimitMa, proto.EncodeU16(5000), false))
		return []byte("VF999999")
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
	// Every answer to the serial read arrives relabelled as a voltage frame.
	d.SetResponse(proto.CmdSerialNumber, proto.EncodeU16(5000))
	d.SetFault(proto.CmdSerialNumber, fake.Fault{Mismatch: true, MismatchCmd: proto.CmdVoltageMv})

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

// TestUnansweredWriteIsMarkedUnacknowledged: once SendFrame has returned, the
// whole frame including the end-of-frame marker is on the wire and the device
// acts on it -- there is no NACK to decline it with (SPEC.md §5.2). A write that
// then goes unanswered has an unknown outcome, not a failed one, and
// ErrUnacknowledged is what says so to the layers that phrase it for the user.
func TestUnansweredWriteIsMarkedUnacknowledged(t *testing.T) {
	write := func(t *testing.T, ctx context.Context, s *Session) error {
		t.Helper()
		_, err := s.Do(ctx, proto.CmdVoltageMv, proto.EncodeU16(12000), true)
		if err == nil {
			t.Fatal("want an error from an unanswered write")
		}
		return err
	}

	t.Run("no answer within the deadline", func(t *testing.T) {
		s, _ := newTestSession(t, Options{Timeout: 100 * time.Millisecond})
		err := write(t, context.Background(), s)
		if !errors.Is(err, ErrUnacknowledged) {
			t.Errorf("error = %v, want it to wrap ErrUnacknowledged", err)
		}
		if !errors.Is(err, ErrTimeout) {
			t.Errorf("error = %v, want the timeout still visible underneath", err)
		}
	})

	t.Run("context cancelled while waiting for the echo", func(t *testing.T) {
		// The window a user's Ctrl-C actually lands in: the frame is gone, the
		// echo is late. Collapsing that to "interrupted" tells the user nothing
		// happened while the rail may already have moved.
		s, d := newTestSession(t, Options{Timeout: 10 * time.Second})
		d.SetResponse(proto.CmdVoltageMv, proto.EncodeU16(12000))
		d.SetFault(proto.CmdVoltageMv, fake.Fault{Delay: 5 * time.Second})

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		err := write(t, ctx, s)
		if !errors.Is(err, ErrUnacknowledged) {
			t.Errorf("error = %v, want it to wrap ErrUnacknowledged", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want the cancellation still visible underneath", err)
		}
	})

	t.Run("a read is not marked", func(t *testing.T) {
		// Nothing was applied, so there is nothing to warn about; the sentinel
		// has to stay rare enough to mean something.
		s, _ := newTestSession(t, Options{Timeout: 100 * time.Millisecond})
		_, err := s.Do(context.Background(), proto.CmdVoltageMv, nil, false)
		if errors.Is(err, ErrUnacknowledged) {
			t.Errorf("error = %v, want a read timeout left unmarked", err)
		}
	})

	t.Run("a failed send is not marked", func(t *testing.T) {
		// SendFrame stops at the message it could not write, so the frame is
		// truncated and the device drops it for want of an end marker. Warning
		// that the rail may have moved would be a false alarm here.
		s, _, tr := newFailingWriteSession(t, Options{Timeout: 100 * time.Millisecond})
		tr.unplug()

		err := write(t, context.Background(), s)
		if errors.Is(err, ErrUnacknowledged) {
			t.Errorf("error = %v; a frame that never left must not be reported as sent", err)
		}
		if !strings.Contains(err.Error(), "send failed") {
			t.Errorf("error = %v, want it to name the send as the failure", err)
		}
	})

	t.Run("a dead transport is not marked", func(t *testing.T) {
		// ErrTransportClosed already names the link as the cause and is acted
		// on as permanent; the sentinel is for the two failures that otherwise
		// read as "the device refused it" and "the run was aborted".
		s, d := newTestSession(t, Options{Timeout: 5 * time.Second})
		d.SetHandler(proto.CmdVoltageMv, func(proto.Frame) []byte {
			d.Unplug(errors.New("read /dev/snd/midiC1D0: no such device"))
			return nil
		})

		err := write(t, context.Background(), s)
		if !errors.Is(err, ErrTransportClosed) {
			t.Fatalf("error = %v, want ErrTransportClosed", err)
		}
		if errors.Is(err, ErrUnacknowledged) {
			t.Errorf("error = %v, want the transport-closed verdict left alone", err)
		}
	})
}

// TestSendFailureOnADeadLinkIsPermanent: an unplug that lands between commands
// fails the next SEND, not a wait, and PermanentErr has to see it.
//
// Nothing else in the chain does. The framer's reader dies without closing the
// done channel SendFrame gates on, so the command is transmitted into a port
// the kernel has already disconnected and comes back as the raw ENODEV. Left
// unclassified, that is a retryable error to every loop in this package.
func TestSendFailureOnADeadLinkIsPermanent(t *testing.T) {
	s, _, tr := newFailingWriteSession(t, Options{Timeout: 2 * time.Second})
	tr.unplug()

	_, err := s.Do(context.Background(), proto.CmdSerialNumber, nil, false)
	if err == nil {
		t.Fatal("want an error when the write fails")
	}
	if !errors.Is(err, syscall.ENODEV) {
		t.Fatalf("error = %v, want the kernel's cause carried through", err)
	}
	if !PermanentErr(err) {
		t.Errorf("PermanentErr(%v) = false; a device that is gone is not worth retrying", err)
	}
	if !strings.Contains(err.Error(), "send failed") {
		t.Errorf("error = %v, want it to name the send as the failure", err)
	}
}

// TestTransferTimeoutOnSendStaysRetryable is the other half of the
// classification: a write that fails without saying the device is gone -- a
// usbfs transfer that timed out, say -- is a failed write to a unit that is
// still there, and the retry loops must keep their budgets for it.
func TestTransferTimeoutOnSendStaysRetryable(t *testing.T) {
	err := fmt.Errorf("CMD_SERIAL_NUMBER: send failed (tx 02 08): %w",
		fmt.Errorf("usbfs: bulk transfer: %w", syscall.ETIMEDOUT))
	if PermanentErr(err) {
		t.Errorf("PermanentErr(%v) = true; only a device that is gone is permanent", err)
	}
}

// TestShortFrameDoesNotSatisfy: a sub-preamble frame is ignored without
// clearing the pending state (SPEC.md §5.2), so the command still times out
// rather than returning garbage.
func TestShortFrameDoesNotSatisfy(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: 200 * time.Millisecond})
	d.SetHandler(proto.CmdSerialNumber, func(proto.Frame) []byte {
		// One byte: shorter than the two-byte preamble. The Device cannot
		// build such a frame, so it is injected raw.
		_ = d.Push([]byte{0x08})
		return nil
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
				_ = d.Push(mustBuild(proto.CmdSerialNumber, []byte("STALE001"), false))
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

	s, d := newTestSession(t, Options{Timeout: 5 * time.Second})

	// Unplug once the command is on the wire: before that, the stale-frame
	// drain at the head of exchange would legitimately swallow the error.
	// The handler fires only after the Device has decoded the complete
	// request frame, which pins exactly that point without sleeping.
	d.SetHandler(proto.CmdSerialNumber, func(proto.Frame) []byte {
		d.Unplug(unplugged)
		return nil
	})

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
		_ = d.Push(mustBuild(proto.CmdVoltageMv, proto.EncodeU16(5000), false)) // mismatched
		return []byte("VF000001")
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
//
// What proves that is every caller getting ITS OWN answer back, not a count of
// how many handlers ran at once. A counter cannot see this at all: the fake
// dispatches from inside writeMIDI, which the framer calls while holding its
// write lock for the whole frame, so the reply is produced before the sender's
// SendFrame returns and every other caller is parked on that lock -- the count
// stays at one whatever this package does with its own semaphore.
//
// So the answers are delayed instead. That puts each reply in flight while its
// caller waits and the next caller is free to send, which is exactly the
// overlap the semaphore exists to prevent: without it a reply lands with a
// different command's wait outstanding, gets dropped for a code mismatch
// (SPEC.md §5.2), and its owner times out.
func TestSingleFlight(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: 2 * time.Second})

	const replyDelay = 20 * time.Millisecond
	answers := map[proto.Cmd][]byte{
		proto.CmdSerialNumber:   []byte("VF000001"),
		proto.CmdCurrentLimitMa: proto.EncodeU16(5000),
		proto.CmdUserVLimit:     proto.EncodeVLimit(3300, 48000),
	}
	for cmd, payload := range answers {
		d.SetResponse(cmd, payload)
		d.SetFault(cmd, fake.Fault{Delay: replyDelay})
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 10; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			got, err := s.SerialNumber(ctx)
			if err == nil && got != "VF000001" {
				err = fmt.Errorf("serial = %q, want \"VF000001\"", got)
			}
			errs <- err
		}()
		go func() {
			defer wg.Done()
			got, err := s.CurrentLimitMa(ctx)
			if err == nil && got != 5000 {
				err = fmt.Errorf("current limit = %d, want 5000", got)
			}
			errs <- err
		}()
		go func() {
			defer wg.Done()
			low, high, err := s.VLimit(ctx)
			if err == nil && (low != 3300 || high != 48000) {
				err = fmt.Errorf("vlimit = %d/%d, want 3300/48000", low, high)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent command: %v", err)
		}
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
	d.SetResponse(proto.CmdVoltageMv, proto.EncodeU16(5000))

	if _, err := s.DoRaw(context.Background(), proto.CmdVoltageMv, nil, false, true); err != nil {
		t.Fatalf("DoRaw: %v", err)
	}
	if got := d.SentHex(); len(got) != 1 || got[0] != "02 52" {
		t.Errorf("tx = %q, want [\"02 52\"]", got)
	}
}

// TestDefaultsApplied checks the zero-value Options substitutions.
func TestDefaultsApplied(t *testing.T) {
	d := fake.New()
	s := New(d.Transport(), Options{})
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
