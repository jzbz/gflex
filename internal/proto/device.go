package proto

import (
	"context"
	"io"
	"time"
)

// USB identification.
const (
	// VendorID is Tundra Labs' USB vendor ID, used by the VFLEX in both
	// application and bootloader mode.
	VendorID uint16 = 0x37BF
	// VendorIDString is the lowercase hex form udev matches on.
	VendorIDString = "37bf"
	// PortNameMatch is the case-insensitive substring the vendor app uses to
	// recognise a VFLEX among MIDI ports. The actual advertised name is
	// unknown, so this match is reproduced rather than replaced.
	PortNameMatch = "vflex"
)

// Timing defaults, matching the vendor application.
const (
	// DefaultTimeout is the response timeout for an ordinary command.
	DefaultTimeout = 5 * time.Second
	// PDOChunkTimeout is the per-chunk timeout when downloading the PDO log.
	PDOChunkTimeout = 8 * time.Second
	// BootloaderACKTimeout is the response timeout in bootloader mode.
	BootloaderACKTimeout = 15 * time.Second
	// VerifyTimeout is the timeout for a firmware CRC verification.
	VerifyTimeout = 120 * time.Second
	// ByteDelay is the inter-message delay the vendor app applies when
	// transmitting MIDI. Whether the device requires it is unknown.
	ByteDelay = 20 * time.Millisecond
)

// DeviceInfo is everything the protocol can tell us about a connected unit.
// Fields use the vendor's own names so that --json output, this code and
// SPEC.md all agree. A nil pointer means "not read".
type DeviceInfo struct {
	SerialNum  string `json:"serial_num,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	HardwareID string `json:"hw_id,omitempty"`
	FirmwareID string `json:"fw_id,omitempty"`
	MfgDate    string `json:"mfg_date,omitempty"`

	VoltageMv      *uint16 `json:"voltage_mv,omitempty"`
	CurrentLimitMa *uint16 `json:"current_limit_ma,omitempty"`
	VLimitLowMv    *uint16 `json:"vlimit_low_mv,omitempty"`
	VLimitHighMv   *uint16 `json:"vlimit_high_mv,omitempty"`

	VToleranceNominalMv *uint16 `json:"vtolerance_nominal_mv,omitempty"`
	// VToleranceSagPerMa is reported as a raw 16-bit value. Its units and
	// scale factor are undetermined; do not convert it.
	VToleranceSagPerMa *uint16 `json:"vtolerance_sag_per_ma,omitempty"`

	VMeasureADCOffset    *int32  `json:"vmeasure_adc_offset,omitempty"`
	VMeasureADCScale     *int32  `json:"vmeasure_adc_scale,omitempty"`
	VMeasureRawADC       *uint16 `json:"vmeasure_raw_adc,omitempty"`
	VMeasureCalibratedMv *uint16 `json:"vmeasure_calibrated_mv,omitempty"`

	// LEDAlwaysOn is the user-facing setting, already un-inverted.
	LEDAlwaysOn *bool `json:"led_always_on,omitempty"`
	// AuthLockLevel is the byte at payload offset 1 of the read response,
	// matching the vendor client. AuthLockRaw preserves the whole payload
	// because the read layout is unverified; see SPEC.md §6.3.
	AuthLockLevel *uint8 `json:"authlock_level,omitempty"`
	AuthLockRaw   []byte `json:"authlock_raw,omitempty"`
}

// Transport is a bidirectional byte-level MIDI stream. Implementations carry
// whole MIDI messages and have no knowledge of the VFLEX protocol.
//
// Implementations must be safe for one reader goroutine concurrent with one
// writer goroutine, but need not support concurrent writers; the session layer
// serialises all command traffic.
type Transport interface {
	// WriteMIDI writes one or more complete MIDI messages.
	WriteMIDI(p []byte) error
	// ReadMIDI reads whatever bytes are available. Implementations may return
	// a partial MIDI message; the framer reassembles across calls.
	//
	// Returning (0, nil) is legal and means "nothing available right now" --
	// a polling transport signals a quiet device that way. Implementations are
	// NOT required to block until data arrives, and callers must therefore not
	// treat (0, nil) as EOF nor busy-loop on it.
	ReadMIDI(p []byte) (int, error)
	// Name identifies the endpoint for diagnostics, e.g. a device node path.
	Name() string
	io.Closer
}

// BulkTransport is a raw bidirectional bulk/interrupt USB channel, used for
// the bootloader's vendor-class interface where no MIDI framing exists.
type BulkTransport interface {
	// Send writes one frame to the OUT endpoint.
	Send(ctx context.Context, p []byte) error
	// Receive reads one packet from the IN endpoint.
	Receive(ctx context.Context, p []byte) (int, error)
	io.Closer
}
