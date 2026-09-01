// Package proto defines the VFLEX wire protocol: the command table, the frame
// format, and the decoded device-state model.
//
// The protocol is a length-prefixed request/response protocol carried either
// over USB-MIDI (application mode, see package framer) or over raw bulk USB
// (bootloader mode). Every frame is
//
//	byte[0] = total frame length = 2 + len(payload)
//	byte[1] = command code | flags
//	byte[2:] = payload
//
// All multi-byte scalars in this protocol are BIG-ENDIAN. The single exception
// in the whole system is the PDO log blob (package pdo), which is little-endian.
package proto

import "fmt"

// Cmd is a VFLEX command code. Codes occupy the low 6 bits of the command byte;
// the upper two bits are Flag values.
type Cmd uint8

// The complete command table, recovered from the vendor application.
//
// "Used" in the comments means the shipped vendor app actually issues the
// command. Commands marked UNKNOWN are present in the enum but have no helper,
// no caller and no response parser anywhere in the vendor client; their payload
// format and effect could not be determined. See SPEC.md §14.
const (
	CmdBootloaderWriteChunk Cmd = 0  // bootloader only
	CmdBootloaderCommitPage Cmd = 1  // bootloader only
	CmdBootloaderVerify     Cmd = 2  // bootloader only; response carries a 1-byte CRC
	CmdBootloadEnd          Cmd = 3  // bootloader only; jump to application
	CmdReserved0            Cmd = 4  // UNKNOWN
	CmdReserved1            Cmd = 5  // UNKNOWN
	CmdReserved2            Cmd = 6  // UNKNOWN
	CmdReserved3            Cmd = 7  // UNKNOWN
	CmdSerialNumber         Cmd = 8  // used: 8 ASCII bytes
	CmdChipUUID             Cmd = 9  // 16 ASCII bytes (measured); never issued by the vendor app
	CmdHardwareID           Cmd = 10 // 8 ASCII bytes; never issued by the vendor app
	CmdFirmwareVersion      Cmd = 11 // used: 12 ASCII bytes
	CmdMfgDate              Cmd = 12 // 8 ASCII bytes; never issued by the vendor app
	CmdFlashLEDSeqAdvanced  Cmd = 13 // used by the vendor library: LED colour write, see LEDColor
	CmdFlashLED             Cmd = 14 // UNKNOWN
	CmdDisableLEDDuringOp   Cmd = 15 // used: 1 byte, inverted sense (see LEDAlwaysOn)
	CmdEncryptMsg           Cmd = 16 // challenge/response of unknown construction
	CmdPDOLog               Cmd = 17 // used: erase (write) / read chunk
	CmdVoltageMv            Cmd = 18 // used: uint16 BE millivolts
	CmdCurrentLimitMa       Cmd = 19 // used: uint16 BE milliamps
	CmdJumpAppToBootloader  Cmd = 20 // used: no ACK, device disconnects
	CmdIOSHostModeFlag      Cmd = 21 // response discarded by the vendor app
	CmdAuthLock             Cmd = 22 // used (write only); read layout is asymmetric
	CmdUserVLimit           Cmd = 23 // used: 4 bytes, HIGH then LOW
	CmdVToleranceNominalMv  Cmd = 24 // used (write only): uint16 BE millivolts
	CmdVToleranceSagPerMa   Cmd = 25 // uint16 BE, units UNKNOWN
	CmdVMeasureADCOffset    Cmd = 26 // int32 BE (signed)
	CmdVMeasureADCScale     Cmd = 27 // int32 BE (signed)
	CmdVMeasure             Cmd = 28 // uint16 BE raw ADC + uint16 BE millivolts
)

// Frame flags OR'd into the command byte.
const (
	FlagWrite      uint8 = 0x80 // this frame carries a value to be stored
	FlagScratchpad uint8 = 0x40 // never set by the vendor app; validate-and-discard (SPEC.md §14.4)
	CmdCodeMask    uint8 = 0x3F // mask isolating the command code
)

// Protocol sizing limits.
//
// Two different ceilings apply, and conflating them is a mistake worth naming.
// The frame format itself is bounded only by its single-byte length field, so a
// frame may be up to 255 bytes total. The 64-byte limit is a property of the
// MIDI *receive* state machine, which caps its accumulator and refuses to
// dispatch anything longer. It therefore constrains responses, and requests
// only insofar as the device shares the same receive code. The bootloader's
// bulk path does not go through that state machine at all, and its firmware
// WRITE_CHUNK frames routinely exceed 64 bytes.
const (
	// PreambleLen is the two-byte header present on every frame.
	PreambleLen = 2
	// MaxFrameLen is the largest frame the MIDI receive state machine will
	// accept. Frames outside [PreambleLen, MaxFrameLen] are dropped, silently,
	// by the vendor client.
	MaxFrameLen = 64
	// MaxPayloadLen is the payload ceiling on the MIDI path.
	MaxPayloadLen = MaxFrameLen - PreambleLen
	// MaxEncodableFrameLen is the hard limit imposed by the length byte.
	MaxEncodableFrameLen = 255
	// MaxEncodablePayloadLen is the payload ceiling Build enforces.
	MaxEncodablePayloadLen = MaxEncodableFrameLen - PreambleLen
)

// Device defaults, as written by the vendor app's post-firmware-update routine.
const (
	DefaultVoltageMv         uint16 = 5000
	DefaultCurrentLimitMa    uint16 = 5000
	DefaultVLimitLowMv       uint16 = 3300
	DefaultVLimitHighMv      uint16 = 48000
	DefaultVToleranceNominal uint16 = 750
	DefaultADCOffset         int32  = 0
	DefaultADCScale          int32  = 0
	AuthLockUnlocked         uint8  = 0
)

// Documented hardware envelope, from the vendor manual.
const (
	HardwareMinVoltageMv uint16 = 3300  // 3.3 V
	HardwareMaxVoltageMv uint16 = 48000 // 48 V
	HardwareMaxCurrentMa uint16 = 5000  // 5 A
	// EPRThresholdMv is the boundary above which Extended Power Range and an
	// eMarker-equipped 5 A cable are required.
	EPRThresholdMv uint16 = 20000
)

// stringLen holds the fixed payload length of each string-valued command.
// The vendor client only consults this table on an unreachable write path, so
// treat these as expectations rather than guarantees: decode using the frame's
// own declared length.
// Measured against a real unit (firmware APP.05.00.00, 2026-08-21): serial
// "81a0bcc3" (8), chip uuid "1732abcd7fc0bcc1" (16), hardware id "VFLEX..." (8),
// firmware "APP.05.00.00" (12), mfg date "004apr26" (8).
//
// CMD_CHIP_UUID is 16, not the 8 the vendor client's own table claims -- its
// write guard would have refused a correct 16-character UUID, which is one more
// sign that path was never exercised. Nothing here decodes by these lengths
// (DecodeString takes bytes[2:frame[0]]), which is why the wrong value was
// harmless; they are expectations for a caller that wants to sanity-check a
// reply, and StringLen is the only reader.
var stringLen = map[Cmd]int{
	CmdSerialNumber:    8,
	CmdChipUUID:        16,
	CmdHardwareID:      8,
	CmdFirmwareVersion: 12,
	CmdMfgDate:         8,
}

// StringLen reports the expected payload length for a string-valued command,
// and whether the command is string-valued at all.
func StringLen(c Cmd) (int, bool) {
	n, ok := stringLen[c]
	return n, ok
}

var cmdNames = map[Cmd]string{
	CmdBootloaderWriteChunk: "CMD_BOOTLOADER_WRITE_CHUNK",
	CmdBootloaderCommitPage: "CMD_BOOTLOADER_COMMIT_PAGE",
	CmdBootloaderVerify:     "CMD_BOOTLOADER_VERIFY",
	CmdBootloadEnd:          "CMD_BOOTLOAD_END",
	CmdReserved0:            "CMD_RESERVED0",
	CmdReserved1:            "CMD_RESERVED1",
	CmdReserved2:            "CMD_RESERVED2",
	CmdReserved3:            "CMD_RESERVED3",
	CmdSerialNumber:         "CMD_SERIAL_NUMBER",
	CmdChipUUID:             "CMD_CHIP_UUID",
	CmdHardwareID:           "CMD_HARDWARE_ID",
	CmdFirmwareVersion:      "CMD_FIRMWARE_VERSION",
	CmdMfgDate:              "CMD_MFG_DATE",
	CmdFlashLEDSeqAdvanced:  "CMD_FLASH_LED_SEQUENCE_ADVANCED",
	CmdFlashLED:             "CMD_FLASH_LED",
	CmdDisableLEDDuringOp:   "CMD_DISABLE_LED_DURING_OPERATION",
	CmdEncryptMsg:           "CMD_ENCRYPT_MSG",
	CmdPDOLog:               "CMD_PDO_LOG",
	CmdVoltageMv:            "CMD_VOLTAGE_MV",
	CmdCurrentLimitMa:       "CMD_CURRENT_LIMIT_MA",
	CmdJumpAppToBootloader:  "CMD_JUMP_APP_TO_BOOTLOADER",
	CmdIOSHostModeFlag:      "CMD_IOS_HOST_MODE_FLAG",
	CmdAuthLock:             "CMD_AUTHLOCK",
	CmdUserVLimit:           "CMD_USER_VLIMIT",
	CmdVToleranceNominalMv:  "CMD_VTOLERANCE_NOMINAL_MV",
	CmdVToleranceSagPerMa:   "CMD_VTOLERANCE_SAG_PER_MA",
	CmdVMeasureADCOffset:    "CMD_VMEASURE_ADC_COUNT_OFFSET",
	CmdVMeasureADCScale:     "CMD_VMEASURE_ADC_COUNT_SCALE",
	CmdVMeasure:             "CMD_VMEASURE",
}

// String returns the vendor's own identifier for the command, or a synthetic
// CMD_<n> for codes outside the known table.
func (c Cmd) String() string {
	if n, ok := cmdNames[c]; ok {
		return n
	}
	return fmt.Sprintf("CMD_%d", uint8(c))
}

// Known reports whether c is a documented command code.
func (c Cmd) Known() bool {
	_, ok := cmdNames[c]
	return ok
}

// Undocumented reports whether c is in the command table but its payload format
// and effect were never determined (SPEC.md §14.5-§14.7, all still open).
//
// CmdFlashLEDSeqAdvanced stays in this set even though §6.2 now documents one
// payload for it. One shape out of 256^n is not characterisation: `led color`
// sends the shape the vendor sends, and every other shape a raw frame can carry
// is as uncharacterised as it ever was (SPEC.md §14.17).
//
// Callers must not emit one silently: SPEC.md §13.10 requires the raw escape
// hatch to name the code and confirm, which is what cli.CheckRawFrame does. Do
// not turn that into an override flag -- SPEC.md §11 rejects a global --force
// precisely because it would shadow `firmware flash --force`, where the same
// word already means "flash an image that carries no CRC".
func (c Cmd) Undocumented() bool {
	switch c {
	case CmdReserved0, CmdReserved1, CmdReserved2, CmdReserved3,
		CmdFlashLEDSeqAdvanced, CmdFlashLED, CmdEncryptMsg, CmdIOSHostModeFlag:
		return true
	}
	return false
}
