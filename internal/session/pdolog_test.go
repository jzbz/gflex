package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
	"github.com/jzbz/gflex/internal/transport/fake"
)

// pdoResponder answers chunk reads. data(idx) supplies the bytes after the
// echoed chunk id; id(idx) supplies the echoed id itself.
func pdoResponder(d *fake.Device, id func(int) uint8, data func(int) []byte) {
	d.SetHandler(proto.CmdPDOLog, func(f proto.Frame) []byte {
		if f.Write || len(f.Payload) != 1 {
			return []byte{} // bare acknowledgement
		}
		idx := int(f.Payload[0])
		return append([]byte{id(idx)}, data(idx)...)
	})
}

// fullChunk returns eight non-zero bytes derived from the index, so the
// assembled blob is order-sensitive.
func fullChunk(idx int) []byte {
	b := make([]byte, PDOChunkDataBytes)
	for i := range b {
		b[i] = byte(idx*PDOChunkDataBytes + i + 1)
	}
	return b
}

func TestFullPDOLogSuccess(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: time.Second})
	pdoResponder(d, func(i int) uint8 { return uint8(i) }, fullChunk)

	var progressChunks []int
	var progressBytes []int
	blob, err := s.FullPDOLog(context.Background(), func(chunk, bytes int) {
		progressChunks = append(progressChunks, chunk)
		progressBytes = append(progressBytes, bytes)
	})
	if err != nil {
		t.Fatalf("FullPDOLog: %v", err)
	}
	// 12 chunks x 8 bytes = 96, truncated to the 90-byte log.
	if len(blob) != PDOLogBytes {
		t.Fatalf("blob is %d bytes, want %d", len(blob), PDOLogBytes)
	}
	for i := 0; i < PDOLogBytes; i++ {
		if want := byte(i + 1); blob[i] != want {
			t.Fatalf("blob[%d] = %d, want %d (chunks concatenated out of order?)", i, blob[i], want)
		}
	}
	if len(progressChunks) != PDOChunkCount {
		t.Fatalf("progress called %d times, want %d", len(progressChunks), PDOChunkCount)
	}
	for i := range progressChunks {
		if progressChunks[i] != i {
			t.Errorf("progress chunk[%d] = %d, want %d", i, progressChunks[i], i)
		}
		if want := (i + 1) * PDOChunkDataBytes; progressBytes[i] != want {
			t.Errorf("progress bytes[%d] = %d, want %d", i, progressBytes[i], want)
		}
	}

	// Every chunk answered first time, so exactly twelve reads went out.
	sent := d.SentHex()
	if len(sent) != PDOChunkCount {
		t.Fatalf("sent %d frames, want %d", len(sent), PDOChunkCount)
	}
	if sent[0] != "03 11 00" || sent[11] != "03 11 0b" {
		t.Errorf("chunk frames = %q, want 03 11 00 .. 03 11 0b", sent)
	}
}

func TestFullPDOLogChunkMismatch(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: time.Second})
	// Chunk 3 always answers with someone else's id.
	pdoResponder(d, func(i int) uint8 {
		if i == 3 {
			return 7
		}
		return uint8(i)
	}, fullChunk)

	_, err := s.FullPDOLog(context.Background(), nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, ErrPDOChunkMismatch) {
		t.Fatalf("error = %v, want ErrPDOChunkMismatch", err)
	}
	// The vendor's wording, verbatim (SPEC.md §9.6).
	if got, want := err.Error(), "PDO log chunk mismatch (requested=3, got=7)"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	// The failing chunk gets the whole budget: 3 good chunks + pdoChunkAttempts.
	if n := len(d.Sent()); n != 3+pdoChunkAttempts {
		t.Errorf("sent %d frames, want %d (3 good chunks plus %d attempts)", n, 3+pdoChunkAttempts, pdoChunkAttempts)
	}
}

func TestFullPDOLogEmptyChunk(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: time.Second})
	// Correct id, but no data after it.
	pdoResponder(d, func(i int) uint8 { return uint8(i) }, func(int) []byte { return nil })

	_, err := s.FullPDOLog(context.Background(), nil)
	if !errors.Is(err, ErrPDOEmptyChunk) {
		t.Fatalf("error = %v, want ErrPDOEmptyChunk", err)
	}
	if got, want := err.Error(), "PDO log read returned empty chunk (requested=0, got=0)"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if n := len(d.Sent()); n != pdoChunkAttempts {
		t.Errorf("sent %d frames, want %d", n, pdoChunkAttempts)
	}
}

func TestFullPDOLogRetrySucceeds(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: time.Second})
	var attempts int
	d.SetHandler(proto.CmdPDOLog, func(f proto.Frame) []byte {
		idx := int(f.Payload[0])
		id := uint8(idx)
		if idx == 0 {
			attempts++
			if attempts == 1 {
				id = 9 // first attempt comes back mislabelled
			}
		}
		return append([]byte{id}, fullChunk(idx)...)
	})

	blob, err := s.FullPDOLog(context.Background(), nil)
	if err != nil {
		t.Fatalf("FullPDOLog: %v", err)
	}
	if len(blob) != PDOLogBytes {
		t.Errorf("blob is %d bytes, want %d", len(blob), PDOLogBytes)
	}
	if attempts != 2 {
		t.Errorf("chunk 0 was requested %d times, want 2", attempts)
	}
}

// TestPDOChunkRetryBudget pins the numbers of SPEC.md §9.1 literally, because
// "3 retries" and "3 attempts" differ by one request and the difference is only
// visible on the wire. §9.1 specifies three attempts per chunk -- one try plus
// two retries, 250 ms apart -- so a chunk that never answers correctly is
// requested exactly three times, with two pauses between them.
func TestPDOChunkRetryBudget(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: time.Second})
	// Every response echoes an id no chunk index can equal, so no attempt is
	// ever accepted and chunk 0 consumes the entire budget.
	pdoResponder(d, func(int) uint8 { return 0xFE }, fullChunk)

	start := time.Now()
	if _, err := s.FullPDOLog(context.Background(), nil); !errors.Is(err, ErrPDOChunkMismatch) {
		t.Fatalf("error = %v, want ErrPDOChunkMismatch", err)
	}
	elapsed := time.Since(start)

	if n := len(d.Sent()); n != 3 {
		t.Errorf("chunk 0 was requested %d times, want 3 (SPEC.md §9.1: three attempts, not three retries)", n)
	}
	// Two retries, each preceded by the 250 ms pause.
	if want := 2 * pdoChunkRetryDelay; elapsed < want {
		t.Errorf("the three attempts took %v, want at least %v of retry delay between them", elapsed, want)
	}
}

// TestFullPDOLogAbortsOnDeadTransport pins the chunk retry classification:
// the three-attempt budget is for a chunk the device answered wrongly or not
// at all (mismatch, empty, short, ErrTimeout -- SPEC.md §9.1, §5.2), never
// for a transport that died. A Session has no reconnect path, so retrying a
// dead link adds two 250 ms delays -- and, on a link that dies less tidily,
// whole 8 s chunk timeouts -- before the user hears the download failed. The
// request count is the load-bearing assertion: the fake records frames the
// host transmits even after the unplug, so a loop that retried the dead
// chunk would show 5 requests instead of 3.
func TestFullPDOLogAbortsOnDeadTransport(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: time.Second})
	d.SetHandler(proto.CmdPDOLog, func(f proto.Frame) []byte {
		idx := int(f.Payload[0])
		if idx < 2 {
			return append([]byte{byte(idx)}, fullChunk(idx)...)
		}
		// The unit vanishes while chunk 2 is being served.
		d.Unplug(errors.New("read /dev/snd/midiC1D0: no such device"))
		return nil
	})

	start := time.Now()
	_, err := s.FullPDOLog(context.Background(), nil)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("error = %v, want ErrTransportClosed", err)
	}
	// Two good chunks plus a single request against the dead transport.
	if n := len(d.Sent()); n != 3 {
		t.Errorf("sent %d chunk requests, want 3 (a dead transport is not retried)", n)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v; the abort must not sit out retry delays against a dead transport", elapsed)
	}
}

// TestFullPDOLogShort covers a truncated chunk.
//
// A short chunk is rejected where it arrives rather than appended and caught
// later by the total-length check. Appending it would shift every subsequent
// byte of a positionally decoded little-endian blob, so the download would not
// fail -- it would succeed and describe a different power supply.
func TestFullPDOLogShort(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: time.Second})
	// Four data bytes per chunk instead of eight.
	pdoResponder(d, func(i int) uint8 { return uint8(i) }, func(i int) []byte {
		return []byte{byte(i + 1), 2, 3, 4}
	})

	_, err := s.FullPDOLog(context.Background(), nil)
	if !errors.Is(err, ErrPDOShortChunk) {
		t.Fatalf("error = %v, want ErrPDOShortChunk", err)
	}
	if got, want := err.Error(), "PDO log read returned a short chunk (requested=0, got 4 of 8 bytes)"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

func TestFullPDOLogAllZero(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: time.Second})
	// Well-formed chunks that are entirely zero: the erase worked but the unit
	// never saw a PD source.
	pdoResponder(d, func(i int) uint8 { return uint8(i) }, func(int) []byte {
		return make([]byte, PDOChunkDataBytes)
	})

	_, err := s.FullPDOLog(context.Background(), nil)
	if !errors.Is(err, ErrPDOLogEmpty) {
		t.Fatalf("error = %v, want ErrPDOLogEmpty", err)
	}
	if got := err.Error(); got != ErrPDOLogEmpty.Error() {
		t.Errorf("error = %q, want the vendor's guidance verbatim", got)
	}
}

// TestPDOLogChunkCapsData: a longer-than-expected response contributes only its
// first eight data bytes.
func TestPDOLogChunkCapsData(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: time.Second})
	d.SetHandler(proto.CmdPDOLog, func(f proto.Frame) []byte {
		payload := append([]byte{f.Payload[0]}, make([]byte, 20)...)
		for i := range payload[1:] {
			payload[1+i] = byte(i + 1)
		}
		return payload
	})

	id, data, err := s.PDOLogChunk(context.Background(), 2)
	if err != nil {
		t.Fatalf("PDOLogChunk: %v", err)
	}
	if id != 2 {
		t.Errorf("chunkID = %d, want 2", id)
	}
	if len(data) != PDOChunkDataBytes {
		t.Fatalf("data is %d bytes, want %d", len(data), PDOChunkDataBytes)
	}
	for i, b := range data {
		if want := byte(i + 1); b != want {
			t.Errorf("data[%d] = %d, want %d", i, b, want)
		}
	}
}

// TestClearPDOLogFrame pins the erase frame: a write with an empty payload.
func TestClearPDOLogFrame(t *testing.T) {
	s, d := newTestSession(t, Options{Timeout: time.Second})
	d.SetResponse(proto.CmdPDOLog, nil)

	if err := s.ClearPDOLog(context.Background()); err != nil {
		t.Fatalf("ClearPDOLog: %v", err)
	}
	if got := d.SentHex(); len(got) != 1 || got[0] != "02 91" {
		t.Errorf("tx = %q, want [\"02 91\"]", got)
	}
}
