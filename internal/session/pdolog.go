package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jzbz/gflex/internal/proto"
)

// PDO log download parameters (SPEC.md §9.1).
const (
	// PDOChunkCount is the number of chunks the host requests, 0..11. Twelve
	// eight-byte chunks yield 96 bytes, of which the first 90 are the log.
	PDOChunkCount = 12
	// PDOChunkDataBytes is the payload carried by one chunk response, after the
	// echoed chunk id.
	PDOChunkDataBytes = 8
	// PDOLogBytes is the size of the log blob. It matches pdo.LogBytes; it is
	// restated here so this package does not depend on the decoder.
	PDOLogBytes = 90

	// pdoChunkAttempts is THREE ATTEMPTS in total -- one try plus two retries --
	// not three retries after a first try. SPEC.md §9.1 says so in those words,
	// and §9.2's neighbouring "6 attempts, 300 ms apart" for the serial reads
	// shows the document counts attempts, not retries, when it says attempts.
	//
	// Worth stating because the count is the whole retry budget for a download
	// that cannot simply be repeated: a failed chunk fails the download, and
	// recovering means the user physically unplugs the unit, revisits the PD
	// source and rescans (§9.2). If evidence ever shows the firmware needs more
	// patience than the vendor gives it, raising this is the one-line change --
	// the delay applies before each retry, so N attempts pause (N-1) * 250 ms.
	pdoChunkAttempts   = 3
	pdoChunkRetryDelay = 250 * time.Millisecond
)

// PDO log errors.
//
// These reproduce the vendor client's wording verbatim (SPEC.md §9.6),
// capitalisation and all, because users troubleshooting a failed scan will
// search for the exact string the app printed. That is a deliberate departure
// from Go's lowercase error-string convention.
var (
	// ErrPDOEmptyChunk is formatted as
	// "PDO log read returned empty chunk (requested=N, got=M)".
	ErrPDOEmptyChunk = errors.New("PDO log read returned empty chunk")

	// ErrPDOShortChunk means a chunk arrived with fewer than PDOChunkDataBytes
	// of payload. It has no vendor counterpart -- the vendor client appends
	// whatever arrives -- because silently misaligning a positionally decoded
	// little-endian blob is worse than failing.
	ErrPDOShortChunk = errors.New("PDO log read returned a short chunk")

	// ErrPDOChunkMismatch is formatted as
	// "PDO log chunk mismatch (requested=N, got=M)".
	ErrPDOChunkMismatch = errors.New("PDO log chunk mismatch")

	// ErrPDOLogLength is formatted as
	// "Invalid PDO log length: expected ≥90 bytes, got N".
	ErrPDOLogLength = errors.New("Invalid PDO log length")

	// ErrPDOLogEmpty reports an all-zero blob: the device was never attached to
	// a PD source between the erase and the read-back.
	ErrPDOLogEmpty = errors.New("No PDO data captured (log is empty). Unplug vFlex from phone, plug into a USB-C PD charger (e.g. MacBook charger) for ~10s, then reconnect and retry.")

	// ErrSerialMismatch is the scan workflow's hard invariant: the unit whose
	// log was erased must be the unit read back (SPEC.md §9.2). The scan
	// wizard, not this package, drives that comparison.
	ErrSerialMismatch = errors.New("A different VFLEX serial number was detected. This scan has been aborted.")

	// ErrFirmwareTooOld gates the scan on firmware >= 5.0.0 (SPEC.md §9).
	ErrFirmwareTooOld = errors.New("Power Supply Scan requires VFLEX firmware 5.0.0 or newer. Update firmware before scanning.")
)

// ClearPDOLog erases the capture buffer: a write with an empty payload, 02 91
// (SPEC.md §9.1).
//
// This is step 2 of the scan workflow. After it the device must be detached
// from the host and attached to the PD source under test for a few seconds, so
// that the firmware can capture that source's advertised capabilities.
func (s *Session) ClearPDOLog(ctx context.Context) error {
	if _, err := s.Do(ctx, proto.CmdPDOLog, nil, true); err != nil {
		return fmt.Errorf("clear PDO log: %w", err)
	}
	return nil
}

// PDOLogChunk reads one 8-byte chunk of the capture buffer: 03 11 kk.
//
// The response payload is [chunkID, b0..b7]: byte 0 echoes the requested index
// and the rest is data. A caller must check that chunkID equals the index it
// asked for -- the echo is the only integrity check the protocol offers.
func (s *Session) PDOLogChunk(ctx context.Context, idx uint8) (chunkID uint8, data []byte, err error) {
	f, err := s.exchange(ctx, proto.CmdPDOLog, []byte{idx}, false, false, s.pdoChunkTimeout())
	if err != nil {
		return 0, nil, err
	}
	if len(f.Payload) == 0 {
		return 0, nil, fmt.Errorf("PDO log chunk %d: response carried no payload", idx)
	}
	chunkID = f.Payload[0]
	data = f.Payload[1:]
	// Guard against a longer-than-expected frame: only the first eight bytes
	// after the id belong to this chunk.
	if len(data) > PDOChunkDataBytes {
		data = data[:PDOChunkDataBytes]
	}
	return chunkID, data, nil
}

// pdoChunkTimeout returns the per-chunk deadline. The vendor uses 8000 ms here
// rather than the ordinary 5000 ms; a session configured with an even longer
// timeout keeps it, since that is an explicit operator choice.
func (s *Session) pdoChunkTimeout() time.Duration {
	if s.timeout > proto.PDOChunkTimeout {
		return s.timeout
	}
	return proto.PDOChunkTimeout
}

// FullPDOLog downloads the whole 90-byte capture blob (SPEC.md §9.1).
//
// Chunks 0..11 are requested in order, each with up to three attempts 250 ms
// apart. A chunk is accepted only when the echoed id equals the requested index
// *and* the data is non-empty; anything else is retried and, on exhaustion,
// fails the download. The concatenation is 96 bytes, of which the first 90 are
// the log -- the trailing 6 are padding the layout never defines.
//
// An all-zero blob is rejected rather than decoded: it means the erase
// succeeded but nothing was ever captured, which is what happens when the unit
// was reconnected to the host without visiting a PD source in between.
//
// progress, when non-nil, is called after each accepted chunk with the chunk
// index and the number of bytes accumulated so far.
func (s *Session) FullPDOLog(ctx context.Context, progress func(chunk int, bytes int)) ([]byte, error) {
	buf := make([]byte, 0, PDOChunkCount*PDOChunkDataBytes)

	for i := 0; i < PDOChunkCount; i++ {
		var accepted []byte
		var last error

		for attempt := 0; attempt < pdoChunkAttempts; attempt++ {
			if attempt > 0 {
				if err := sleepCtx(ctx, pdoChunkRetryDelay); err != nil {
					return nil, fmt.Errorf("PDO log chunk %d: %w", i, err)
				}
			}
			id, data, err := s.PDOLogChunk(ctx, uint8(i))
			if err != nil {
				last = err
				continue
			}
			if int(id) != i {
				last = fmt.Errorf("%w (requested=%d, got=%d)", ErrPDOChunkMismatch, i, id)
				continue
			}
			if len(data) == 0 {
				last = fmt.Errorf("%w (requested=%d, got=%d)", ErrPDOEmptyChunk, i, id)
				continue
			}
			// Every chunk carries exactly PDOChunkDataBytes. A short one means a
			// truncated frame, and appending it would shift every subsequent
			// byte of the blob -- which is little-endian and positionally
			// decoded, so the result would not look corrupt, it would look like
			// a different power supply. Retry it like any other bad chunk.
			if len(data) < PDOChunkDataBytes {
				last = fmt.Errorf("%w (requested=%d, got %d of %d bytes)",
					ErrPDOShortChunk, i, len(data), PDOChunkDataBytes)
				continue
			}
			accepted, last = data, nil
			break
		}
		if last != nil {
			return nil, last
		}

		buf = append(buf, accepted...)
		if progress != nil {
			progress(i, len(buf))
		}
	}

	if len(buf) < PDOLogBytes {
		return nil, fmt.Errorf("%w: expected ≥%d bytes, got %d", ErrPDOLogLength, PDOLogBytes, len(buf))
	}
	buf = buf[:PDOLogBytes]

	if allZero(buf) {
		return nil, ErrPDOLogEmpty
	}
	return buf, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
