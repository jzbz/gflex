package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/session"
)

// serialReader is a scripted CMD_SERIAL_NUMBER, counting how many times it was
// asked so a test can prove the retry did or did not happen.
type serialReader struct {
	replies []struct {
		serial string
		err    error
	}
	calls int
}

func (r *serialReader) read(context.Context) (string, error) {
	i := r.calls
	r.calls++
	if i >= len(r.replies) {
		i = len(r.replies) - 1 // the last reply repeats forever
	}
	return r.replies[i].serial, r.replies[i].err
}

func scripted(pairs ...[2]any) *serialReader {
	r := &serialReader{}
	for _, p := range pairs {
		serial, _ := p[0].(string)
		err, _ := p[1].(error)
		r.replies = append(r.replies, struct {
			serial string
			err    error
		}{serial, err})
	}
	return r
}

// TestScanSerialPolicyMatchesSpec pins the numbers themselves. SPEC.md §9.2
// specifies six attempts 300 ms apart for the serial reads that bracket a scan,
// and the whole point of the fix is that the CLI now honours them.
func TestScanSerialPolicyMatchesSpec(t *testing.T) {
	if scanSerialAttempts != 6 {
		t.Errorf("scanSerialAttempts = %d, SPEC.md §9.2 says 6", scanSerialAttempts)
	}
	if scanSerialRetryDelay != 300*time.Millisecond {
		t.Errorf("scanSerialRetryDelay = %s, SPEC.md §9.2 says 300ms", scanSerialRetryDelay)
	}
}

// TestReadSerialRetryingSurvivesASlowReconnect is the regression test.
//
// The reconnect read used to be a single attempt, so one dropped frame aborted
// the scan at the point where the user had already unplugged the device, walked
// it to a charger, waited and plugged it back in. A just-enumerated unit
// answering slowly is the normal case there, and the protocol has no NACK, so a
// lone timeout says nothing at all (SPEC.md §5.2).
func TestReadSerialRetryingSurvivesASlowReconnect(t *testing.T) {
	r := scripted(
		[2]any{"", session.ErrTimeout},
		[2]any{"", session.ErrTimeout},
		[2]any{"VF001234", nil},
	)
	got, err := readSerialRetrying(context.Background(), r.read, scanSerialAttempts, time.Microsecond)
	if err != nil {
		t.Fatalf("two timeouts then a good read should succeed, got %v", err)
	}
	if got != "VF001234" {
		t.Errorf("serial = %q, want %q", got, "VF001234")
	}
	if r.calls != 3 {
		t.Errorf("read %d times, want 3 (it must stop as soon as a serial is readable)", r.calls)
	}
}

// A response that sanitises to fewer than four characters identifies nothing
// (SPEC.md §6.4), so it counts as "could not read it" and is retried too --
// a settling unit answering with padding is the same event as a timeout.
func TestReadSerialRetryingRetriesAnUnusableSerial(t *testing.T) {
	r := scripted(
		[2]any{"", nil},
		[2]any{"VF0", nil},
		[2]any{"VF001234", nil},
	)
	got, err := readSerialRetrying(context.Background(), r.read, scanSerialAttempts, time.Microsecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "VF001234" || r.calls != 3 {
		t.Errorf("serial = %q after %d reads, want %q after 3", got, r.calls, "VF001234")
	}
}

// TestReadSerialRetryingNeverRetriesPastAReadableSerial is the invariant half,
// and it is the more important of the two.
//
// The serial equality check is the hard invariant of SPEC.md §9.2: the unit
// whose log was erased must be the unit read back. A retry that kept going
// after a perfectly readable serial would convert "this is a different VFLEX"
// into "ask again until one of them agrees" -- and the scan would then decode
// one unit's capture and label it with another's, with nothing downstream able
// to tell. So the first readable answer is final, even when a later attempt
// would have returned the serial the caller is hoping for.
func TestReadSerialRetryingNeverRetriesPastAReadableSerial(t *testing.T) {
	r := scripted(
		[2]any{"OTHER999", nil},
		[2]any{"VF001234", nil}, // must never be reached
	)
	got, err := readSerialRetrying(context.Background(), r.read, scanSerialAttempts, time.Microsecond)
	if err != nil {
		t.Fatalf("a readable serial is not an error: %v", err)
	}
	if got != "OTHER999" {
		t.Fatalf("serial = %q, want %q: the mismatching unit must be reported, not retried away", got, "OTHER999")
	}
	if r.calls != 1 {
		t.Errorf("read %d times, want 1: retrying past a readable serial would soften the §9.2 invariant", r.calls)
	}
}

// Exhausting the attempts is a failure, not a silent empty serial: an empty
// string compared against the latched one would be reported as a mismatch,
// which blames the wrong thing.
func TestReadSerialRetryingGivesUp(t *testing.T) {
	r := scripted([2]any{"", session.ErrTimeout})
	got, err := readSerialRetrying(context.Background(), r.read, 4, time.Microsecond)
	if err == nil {
		t.Fatalf("expected failure after 4 unreadable attempts, got %q", got)
	}
	if r.calls != 4 {
		t.Errorf("read %d times, want 4", r.calls)
	}
	if !errors.Is(err, session.ErrTimeout) {
		t.Errorf("the last cause must survive for ExitCode to classify it; got %v", err)
	}
	if got != "" {
		t.Errorf("serial = %q on failure, want empty", got)
	}
}

// A cancelled context stops the retry rather than sitting out the remaining
// attempts: Ctrl-C during a scan has to unwind so the deferred Close releases
// the device.
func TestReadSerialRetryingHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := scripted([2]any{"", session.ErrTimeout})
	if _, err := readSerialRetrying(ctx, r.read, scanSerialAttempts, time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if r.calls != 1 {
		t.Errorf("read %d times after cancellation, want 1", r.calls)
	}
}

// TestScanDryRunListsBothSerialReads holds `scan --dry-run` to interlock 8: the
// workflow of SPEC.md §9.2 reads the serial twice, once to latch it and once to
// check it, and a listing that shows one misdescribes the step the whole
// workflow turns on.
func TestScanDryRunListsBothSerialReads(t *testing.T) {
	frames, err := scanFrames()
	if err != nil {
		t.Fatalf("scanFrames: %v", err)
	}
	got := cmdNames(frames)
	want := []string{
		proto.CmdFirmwareVersion.String(),
		proto.CmdSerialNumber.String(),
		proto.CmdPDOLog.String(), // erase
		proto.CmdSerialNumber.String(),
	}
	if len(got) != len(want)+pdoChunks {
		t.Fatalf("scan lists %d frames, want %d preamble + %d chunk reads: %v",
			len(got), len(want), pdoChunks, got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("frame %d = %s, want %s\n  full listing: %v", i, got[i], w, got)
		}
	}
	// The erase really is a write; a read of command 17 is a chunk request.
	erase, err := proto.Parse(frames[2])
	if err != nil {
		t.Fatalf("parsing the erase frame: %v", err)
	}
	if !erase.Write || len(erase.Payload) != 0 {
		t.Errorf("erase frame = %s, want a write with an empty payload (02 91)", proto.Hex(frames[2]))
	}
}
