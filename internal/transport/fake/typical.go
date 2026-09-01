package fake

import (
	"encoding/binary"

	"github.com/jzbz/gflex/internal/proto"
)

// Values NewTypical reports, exported so tests can assert against them without
// repeating string literals.
const (
	TypicalSerial = "VF001234"
	// Sixteen characters, not eight: see the identity-string comment in
	// NewTypical for why the chip UUID is the one that differs.
	TypicalChipUUID   = "CU5A1B2C3D4E5F60"
	TypicalHardwareID = "HW000200"
	TypicalFirmware   = "5.0.1"
	TypicalMfgDate    = "20250131"

	// TypicalRawADC and TypicalMeasuredMv are the two halves of the
	// CMD_VMEASURE response: raw counts, then the millivolts the device
	// calibrated for itself (there is no host-side formula, SPEC.md §6.5).
	TypicalRawADC     uint16 = 2048
	TypicalMeasuredMv uint16 = 5000
)

// pdoLogChunkBytes is the payload size of one CMD_PDO_LOG chunk response, and
// pdoLogChunks how many the host requests. Twelve 8-byte chunks cover the
// 90-byte log with six bytes to spare (SPEC.md §9.1).
const (
	pdoLogChunkBytes = 8
	pdoLogChunks     = 12
)

// NewTypical returns a Device that behaves like a healthy, factory-default
// VFLEX, so a dependent package's test can get a working device in one line.
//
// It reports:
//
//	serial number          "VF001234"        (CMD_SERIAL_NUMBER)
//	chip UUID              "CU5A1B2C3D4E5F60" (CMD_CHIP_UUID, 16 bytes)
//	hardware ID            "HW000200"        (CMD_HARDWARE_ID)
//	firmware version       "5.0.1"           (CMD_FIRMWARE_VERSION, NUL-padded to 12)
//	manufacturing date     "20250131"        (CMD_MFG_DATE)
//	output voltage         5000 mV           (CMD_VOLTAGE_MV)
//	current limit          5000 mA           (CMD_CURRENT_LIMIT_MA)
//	user voltage limits    low 3300, high 48000 mV (CMD_USER_VLIMIT)
//	tolerance nominal      750 mV            (CMD_VTOLERANCE_NOMINAL_MV)
//	tolerance sag          0                 (CMD_VTOLERANCE_SAG_PER_MA)
//	ADC offset and scale   0                 (CMD_VMEASURE_ADC_COUNT_OFFSET/SCALE)
//	measurement            2048 counts, 5000 mV (CMD_VMEASURE)
//	LED always on          enabled           (CMD_DISABLE_LED_DURING_OPERATION, wire byte 0x00)
//	authlock               level 0, unlocked (CMD_AUTHLOCK)
//	PDO log                TypicalPDOLog, served in 12 chunks (CMD_PDO_LOG)
//
// Every one of those except the identity strings and CMD_VMEASURE is a
// register: a write updates it and a subsequent read returns the written value,
// so write-then-read-back sequences behave as they do on hardware.
//
// Two deliberate departures from a bare echo device:
//
//   - CMD_JUMP_APP_TO_BOOTLOADER is answered with silence, because the real
//     device does not acknowledge it and disconnects instead (SPEC.md §10.1).
//   - CMD_AUTHLOCK is asymmetric. A write carries the level in the first
//     payload byte, while a read answers with [0x16, level] -- the command code
//     echoed a second time, then the level (SPEC.md §14, question 8, measured
//     2026-08-21: tx 02 16 -> rx 04 16 16 00). So the stored register is not
//     the payload of the write that set it.
//
// Erasing the PDO log zeroes it, as it does on hardware, so a scan test that
// erases first must put a capture back with
// StoreRegister(proto.CmdPDOLog, TypicalPDOLog()) before downloading, or it
// will read the all-zero blob a host is required to reject (SPEC.md §9.1).
//
// Commands with no registration fall through to the echo default, so an
// unmodelled command still answers rather than timing out.
func NewTypical() *Device {
	d := New()

	// Identity strings are read-only, NUL-padded to their fixed lengths. Those
	// lengths are proto's stringLen table, which carries the measured ones:
	// CMD_CHIP_UUID returns 16 bytes ("1732abcd7fc0bcc1" on the unit that was
	// brought up), not the 8 §6.4's table claims -- see SPEC.md §14,
	// "Corrections this produced". TypicalChipUUID fills all sixteen so the
	// fake serves a UUID the shape hardware sends rather than a half-padded
	// one; the value stays mnemonic so a failing test names itself.
	d.SetResponse(proto.CmdSerialNumber, padString(TypicalSerial, 8))
	d.SetResponse(proto.CmdChipUUID, padString(TypicalChipUUID, 16))
	d.SetResponse(proto.CmdHardwareID, padString(TypicalHardwareID, 8))
	d.SetResponse(proto.CmdFirmwareVersion, padString(TypicalFirmware, 12))
	d.SetResponse(proto.CmdMfgDate, padString(TypicalMfgDate, 8))

	// Settings. The LED byte is inverted: 0x00 means "LED Always On" enabled
	// (SPEC.md §6.2). The vlimit pair is HIGH first on the wire, in both
	// directions (SPEC.md §6.5), which is exactly what proto.EncodeVLimit does.
	d.SetRegister(proto.CmdDisableLEDDuringOp, []byte{proto.EncodeLEDAlwaysOn(true)})
	d.SetRegister(proto.CmdVoltageMv, proto.EncodeU16(proto.DefaultVoltageMv))
	d.SetRegister(proto.CmdCurrentLimitMa, proto.EncodeU16(proto.DefaultCurrentLimitMa))
	d.SetRegister(proto.CmdUserVLimit, proto.EncodeVLimit(proto.DefaultVLimitLowMv, proto.DefaultVLimitHighMv))
	d.SetRegister(proto.CmdVToleranceNominalMv, proto.EncodeU16(proto.DefaultVToleranceNominal))
	d.SetRegister(proto.CmdVToleranceSagPerMa, proto.EncodeU16(0))
	d.SetRegister(proto.CmdVMeasureADCOffset, proto.EncodeI32(proto.DefaultADCOffset))
	d.SetRegister(proto.CmdVMeasureADCScale, proto.EncodeI32(proto.DefaultADCScale))

	// Measurement is read-only: raw ADC counts then the device's calibrated
	// millivolts, both big-endian.
	measure := make([]byte, 0, 4)
	measure = append(measure, proto.EncodeU16(TypicalRawADC)...)
	measure = append(measure, proto.EncodeU16(TypicalMeasuredMv)...)
	d.SetResponse(proto.CmdVMeasure, measure)

	// AUTHLOCK: a read answers [0x16, level] -- the command code echoed a
	// second time, then the level. Measured on hardware (SPEC.md §14, question
	// 8: tx 02 16 -> rx 04 16 16 00), which also settled that the vendor
	// client's payload[1] was never an off-by-one. The stored register holds
	// the read payload verbatim, so byte 0 is the echo and byte 1 the level.
	d.StoreRegister(proto.CmdAuthLock, []byte{byte(proto.CmdAuthLock), proto.AuthLockUnlocked})
	d.SetHandler(proto.CmdAuthLock, func(f proto.Frame) []byte {
		if f.Write {
			level := proto.AuthLockUnlocked
			if len(f.Payload) > 0 {
				level = f.Payload[0]
			}
			d.StoreRegister(proto.CmdAuthLock, []byte{byte(proto.CmdAuthLock), level})
			return append([]byte{}, f.Payload...) // echo the write, never nil
		}
		v, _ := d.Register(proto.CmdAuthLock)
		return v
	})

	// PDO log: a write with no payload erases it, a read of chunk k returns
	// [k, 8 data bytes] (SPEC.md §9.1).
	d.StoreRegister(proto.CmdPDOLog, TypicalPDOLog())
	d.SetHandler(proto.CmdPDOLog, func(f proto.Frame) []byte {
		log, _ := d.Register(proto.CmdPDOLog)
		if f.Write {
			d.StoreRegister(proto.CmdPDOLog, make([]byte, len(log)))
			return []byte{} // bare acknowledgement
		}
		if len(f.Payload) < 1 {
			return nil // malformed chunk request: no answer
		}
		off := int(f.Payload[0]) * pdoLogChunkBytes
		if off < 0 || off+pdoLogChunkBytes > len(log) {
			return nil // chunk out of range: no answer, the host times out
		}
		out := make([]byte, 0, 1+pdoLogChunkBytes)
		out = append(out, f.Payload[0]) // the response echoes the chunk index
		return append(out, log[off:off+pdoLogChunkBytes]...)
	})

	// The jump command is never acknowledged; the device disconnects instead.
	d.SetFault(proto.CmdJumpAppToBootloader, Fault{Drop: true})

	return d
}

// TypicalPDOLog returns the capture NewTypical serves: a plausible 90-byte PDO
// log padded to the 96 bytes twelve chunks carry.
//
// Unlike everything else in this protocol the blob is little-endian
// (SPEC.md §9.3). It describes a source that negotiated 5 V, measured 4998 mV,
// offered three fixed PDOs (5 V 3 A, 9 V 3 A, 20 V 5 A) and selected the first.
func TypicalPDOLog() []byte {
	b := make([]byte, pdoLogChunks*pdoLogChunkBytes) // 96
	binary.LittleEndian.PutUint16(b[0:2], 5000)      // targetVoltageMv
	binary.LittleEndian.PutUint16(b[2:4], 4998)      // measuredVoltageMv
	b[4] = 3                                         // nPdosReceived
	b[5] = 1                                         // idSelectedPdo
	// The negotiation flags of a source that was asked for 5 V and delivered it
	// (SPEC.md §9.3). Written as literals rather than through internal/pdo's
	// constants, for the reason the MIDI codec here is independent too: a fake
	// that shares its vocabulary with the decoder cannot catch the decoder
	// misreading the vocabulary.
	//
	//	flags  0x0005 = pd request accepted | voltage within tolerance
	//	flags2 0x0003 = spr init pdos received | spr ps ready
	//
	// Bit 3 of flags2 -- eprCableFail -- is deliberately clear: this source is
	// an SPR one and never attempted EPR entry.
	binary.LittleEndian.PutUint16(b[6:8], 0x0005)  // flags
	binary.LittleEndian.PutUint16(b[8:10], 0x0003) // flags2

	// Fixed PDOs (type 0): voltage in 50 mV units at bits 19:10, maximum
	// current in 10 mA units at bits 9:0 (SPEC.md §9.4).
	for i, p := range []struct{ mv, ma uint32 }{
		{5000, 3000},
		{9000, 3000},
		{20000, 5000},
	} {
		word := ((p.mv / 50) << 10) | (p.ma / 10)
		binary.LittleEndian.PutUint32(b[10+4*i:14+4*i], word)
	}
	return b
}

// padString renders s as a fixed-length NUL-padded identity payload, the way
// the firmware stores them. Over-long input is truncated rather than
// overflowing the field.
func padString(s string, n int) []byte {
	b := make([]byte, n)
	copy(b, s)
	return b
}
