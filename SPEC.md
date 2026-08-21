# VFLEX Linux Utility — Engineering Specification

**Target:** a Go CLI for Linux that fully programs a Werewolf VFLEX, replacing the proprietary
Android/iOS app and the Chrome-only web app.

**Status:** derived entirely by reverse-engineering `com.tundralabs.vflex_2.0.0.xapk`, the
functionally identical web build served from `https://vflex.app`, and the vendor manual at
<https://werewolf.us/a/pages/vflex-user-manual>. No hardware was available. Every claim below is
either **VERIFIED** against decompiled source (with a citation) or explicitly marked **UNKNOWN** /
**INFERRED**. Nothing here is a guess presented as fact — see §14 for the full list of things we
could not determine and which therefore need a bring-up session with a real device.

**Contents:** [0 Provenance](#0-provenance-and-method) · [1 The device](#1-what-the-vflex-actually-is)
· [2 Scope](#2-scope) · [3 Transport](#3-transport--usb-midi-with-nibble-framing)
· [4 Linux strategy](#4-linux-transport-strategy) · [5 Protocol](#5-application-protocol)
· [6 Commands](#6-command-reference) · [7 Session](#7-session-semantics)
· [8 Data model](#8-device-data-model) · [9 PD scan](#9-power-supply-capability-scan)
· [10 Firmware](#10-firmware-update) · [11 CLI](#11-cli-surface)
· [12 Architecture](#12-architecture) · [13 Safety](#13-safety-interlocks)
· [14 Hardware answers](#14-open-questions--mostly-resolved-on-hardware-2026-08-21)
· [15 Vectors](#15-golden-test-vectors) · [16 Reproducing](#16-reproducing-the-analysis)
· [17 Deliberate deviations](#17-where-the-implementation-deliberately-differs-from-this-spec)

---

## 0. Provenance and method

How this document was produced matters, both for trusting it and for the legal posture of anything
built from it. The process was **clean-room structured**, in two gated phases:

1. **Analysis.** The vendor's shipped application was decompiled and read, and the observable facts
   of its wire protocol — command codes, byte layouts, units, endianness, timings, state machines —
   were recorded here, each with a citation to the module it came from.
2. **Implementation.** The Go code was written against *this document*. Implementers worked from the
   specification and the protocol contract in `internal/proto`, not by transliterating vendor code.

Two honest qualifications, so nobody over-relies on the label:

- This is **not a formal clean-room** in the strict sense. That requires personnel separation — a
  second team, provably never exposed to the original, implementing from the specification alone.
  Here the same party directed both phases, and the implementers were not isolated by anything
  stronger than instruction.
- What that process *does* establish is that **no vendor code was copied**. The vendor ships
  minified JavaScript and Hermes bytecode; this is Go, written from a written description of
  behaviour. What was extracted are interface facts — that command 18 carries a big-endian
  millivolt value, that the LED byte is inverted — not creative expression.

Nothing in this repository is derived from vendor source, documentation under licence, or any
confidential material. The manual cited is public. The APK was obtained as a shipped artifact.

**This is an unofficial, independent implementation.** It is not affiliated with, endorsed by, or
supported by Werewolf or Tundra Labs, and it carries no warranty (see [LICENSE](LICENSE)).
Reverse-engineering for interoperability is protected in many jurisdictions — notably EU Directive
2009/24/EC Art. 6 and the US DMCA §1201(f) — but that is a statement of general context, not legal
advice, and the position varies by jurisdiction and by what you do with it.

---

## 1. What the VFLEX actually is

It is a programmable USB-C **Power Delivery voltage adapter** — a small inline dongle that
negotiates a user-programmed voltage from a USB-C PD source and presents it on a proprietary
"X-Connector" for guitar pedals and similar loads.

| Fact | Value | Source |
|---|---|---|
| Voltage range | 3.3 V – 48 V | manual FAQ, matches `DEFAULT_VLIMIT_LOW_MV=3300` / `HIGH=48000` |
| Max pass-through current | 5 A | manual FAQ, matches `DEFAULT_CURRENT_LIMIT_MA=5000` |
| Factory default output | 5 V | manual, matches `DEFAULT_VOLTAGE_MV=5000` |
| Settings storage | non-volatile, survives power loss | manual "Theory of Operation" |
| Measurement capability | **voltage only**; no current or power sensing, nothing logged | manual FAQ |
| Host link | **wired USB only** — explicitly *not* WiFi/BLE/Bluetooth | manual "Data Connection" |
| Host protocol | **USB-MIDI**, class-compliant | `VFlexMidiTransport`, `navigator.requestMIDIAccess()` |
| USB vendor ID | **0x37BF** (`TUNDRA_VENDOR_ID = 14271`) | web module 2812 |
| USB product ID | **UNKNOWN** — see §14.1 | — |
| Firmware update link | vendor-class (0xFF) bulk USB, WebUSB in the app | web module 2811 |

The Chrome/Edge-only requirement for the web app is explained by **Web MIDI** (application mode) and
**WebUSB** (firmware update) — neither is supported by Firefox or Safari. A Linux Go CLI has no such
restriction and can implement both.

### 1.1 LED status reference

The CLI cannot read LED state — no such command exists — but users will describe faults by colour,
so error messages should map to this table.

| LED | Meaning |
|---|---|
| Solid white | Booting (should be brief) |
| Solid blue | USB data connection present (host attached) |
| Blink blue | Configuration data written successfully (ACK) |
| Solid green | Power Good — negotiated OK, output within tolerance |
| Solid red | Negotiation succeeded but output voltage **out of tolerance** |
| Slow blink red | PD negotiation **failed** — source cannot supply the configured voltage |
| Fast blink red | **eMarker cable error** — typically >20 V configured without an EPR-rated cable |
| Slow blink white | **Bootloader / firmware update mode** |

The vendor's own LED page is an unfinished draft: three of these descriptions are truncated
mid-sentence in the source CMS and the missing text was never written (proven from the page's
CRDT edit log — a 241-character deletion tombstone on the "Slow Blink White" block). Do not
expect more detail to appear.

With **"LED Always On" disabled**, the LED is suppressed *only* in the Solid Green state; every
other state still lights.

---

## 2. Scope

**In scope (feature parity with the app's device-facing half):**

- Discover and connect to a VFLEX over USB-MIDI
- Read identity: serial number, firmware version, and (untested) chip UUID, hardware ID, mfg date
- Read/write output voltage, current limit, user voltage limits, tolerance parameters
- Read/write the "LED Always On" setting
- Read the measured output voltage and ADC calibration values
- Power-supply capability scan (erase PDO log → attach to charger → read back → decode USB-PD PDOs)
- Firmware update over the vendor-class bootloader interface
- A raw-frame escape hatch and a live traffic monitor

**Out of scope:** the cloud half of the app — user accounts, the Devices/Power-Sources inventory,
photo uploads, sharing, telemetry. These are a NestJS REST backend
(`https://vflex-nestjs-prod-ylaqjkd4na-uc.a.run.app/api`) plus Firebase Auth, and **none of it is
required to program the hardware.** Selecting a saved "Device" in the app results in exactly two
wire writes: `CMD_VOLTAGE_MV = round(device.voltage × 1000)` and
`CMD_CURRENT_LIMIT_MA = round(device.currentRating × 1000)` (or 5000 mA when unset). Everything
else — adapter cable, polarity, photos, model number — is metadata the firmware never sees.

The one exception worth replicating locally is **power-source compatibility checking** (§9.5): pure
client-side arithmetic over scanned PDO data, valuable offline, no server needed.

---

## 3. Transport — USB-MIDI with nibble framing

This is the single most important section. Get it wrong and nothing works; get it subtly wrong and
one byte in sixteen corrupts.

### 3.1 The encoding

Each protocol frame is bracketed by two MIDI channel messages, and **every protocol byte is carried
as one MIDI Note-On whose note number is the byte's high nibble and whose velocity is its low
nibble.**

```
0x80 0x00 0x00              Note Off, ch 0          START OF FRAME
0x90 (b>>4)&0x0F  b&0x0F    Note On,  ch 0          one message per protocol byte
   … repeated for every byte of the frame …
0xA0 0x00 0x00              Poly Key Pressure, ch 0 END OF FRAME
```

*Evidence: `VFlexMidiTransport.port.send`, web module 2801 — `o.send([128,0,0])`, then
`const n=t[i], c=n>>4&15, h=15&n; o.send([144,c,h])`, then `o.send([160,0,0])`.*

This is the protocol's 7-bit-safety mechanism. Both emitted data bytes are always `0x00–0x0F`, so
8-bit values (the `0x80` write flag, voltage MSBs) survive a channel that only permits 7-bit data.
**There is no SysEx anywhere in this product** — `sysex:true` is never requested from Web MIDI, and
`grep -c 'SysEx\|sysex'` over the 6 MB bundle is 0. Do not build a SysEx path.

Timing: the reference client sleeps `midiPacketDelayMs = 20 ms` after the start marker and after
each data byte, with **no** trailing delay after the end marker. A frame of N bytes therefore costs
N+2 MIDI messages and (N+1) × 20 ms ≈ 100 ms for a 4-byte frame. Whether 20 ms is a firmware
requirement or defensive padding is **UNKNOWN** — make it a `--byte-delay` flag defaulting to 20 ms
and measure on real hardware before lowering it.

### 3.2 ⚠ The Note-On velocity-0 hazard

Any protocol byte whose **low nibble is zero** encodes as `0x90 <hi> 0x00` — a Note-On with velocity
0, which by MIDI convention is equivalent to Note-Off. Any middleware that normalises that to `0x80`
will silently corrupt roughly **one byte in sixteen**, and the corruption looks like a frame-start
marker, so it will resync the receiver mid-frame.

We verified that this does **not** happen in the Linux kernel or in alsa-lib: `seq_midi_event.c`
maps 0x80/0x90/0xA0 through `note_event`/`note_decode` with no velocity test
(`status_event[]` :54-62, `note_decode` :339-343). ALSA rawmidi is a transparent byte FIFO and does
not touch it at all.

It **is** a real risk for anything routing through a MIDI abstraction layer that "helpfully"
canonicalises messages. This is one of the reasons we recommend against the Go MIDI libraries (§4.3)
and in favour of writing bytes to rawmidi directly.

### 3.3 Receive path

```
for each inbound MIDI message:
    status = b & 0xF0
    0x80 -> reset the accumulator                          (start of frame)
    0x90 -> if len(acc) < 64: acc = append(acc, (d1&0x0F)<<4 | (d2&0x0F))
    0xA0 -> if len(acc) >= 2:                              (end of frame)
                n = acc[0]
                if n >= 2 && n <= len(acc) && n <= 64 { dispatch(acc[:n]) }
            reset the accumulator
```

*Evidence: the `midi.on("message")` handler in web module 2435. Channel nibble is masked off and
ignored in both directions.*

**⚠ Critical porting hazard.** The reference implementation walks the buffer in a fixed 3-byte
stride (`for(t=0; t+2<i.length; t+=3)`). That is only correct because Web MIDI delivers exactly one
complete message per event, guaranteeing index 0 is a status byte. **ALSA rawmidi delivers an
unframed byte stream.** A single 1-byte realtime message (0xF8 clock, 0xFE active sensing), a 2-byte
message (0xC0/0xD0), or running-status compression will permanently desynchronise a fixed stride,
after which every subsequent frame is garbage.

The Go decoder **must be a status-byte-driven MIDI parser**:

```go
for _, b := range chunk {
    switch {
    case b >= 0xF8:            // system realtime — may appear anywhere, ignore
        continue
    case b >= 0x80:            // status byte — resync
        status, data, nData = b, data[:0], expectedDataBytes(b)
    default:
        data = append(data, b)
        if len(data) == nData { handleMessage(status, data); data = data[:0] }
    }
}
```

Note also that a Read() can split a message across calls, so the accumulator must persist across
reads. This is the single most likely source of a subtly broken port.

Two more receive-side behaviours worth reproducing deliberately or fixing deliberately:

- The 64-byte accumulator cap **does not reset** on overflow, so an over-long frame still dispatches
  a truncated 64-byte prefix.
- A frame whose length byte is 0, 1, >64, or larger than what actually arrived is **silently
  dropped** at the transport with no diagnostic; the pending command just times out at 5 s. Log it —
  the decoder does, through a drop hook that `gflex monitor` surfaces (§17). Both malformed shapes
  are pinned as `rx` vectors in `testdata/golden/frames.json`.

### 3.4 Device discovery

The app matches MIDI ports by name only:

```
port.name.toLowerCase().contains("vflex")     // plain substring, not a regex, not anchored
```

applied independently to inputs and outputs, with a fallback: if no name matches **and exactly one
input (resp. output) exists on the entire system**, use it. Both an input and an output are
required; `tryConnect` refreshes MIDI access once and then throws
`"No VFLEX MIDI ports available"`.

**The actual port name the device advertises is UNKNOWN** (§14.2) — it appears nowhere in the app,
the APK, or the manual. Reproduce the substring match rather than hardcoding a name.

Better, for a Linux tool: anchor on the **USB vendor ID**, which is authoritative. Walk
`/sys/bus/usb/devices/*`, match `idVendor == "37bf"`, then glob
`<devpath>/*/sound/card*/midiC*D*` for the rawmidi node. Fall back to the name substring, then to
the sole-port rule.

One consequence of the name-only matching worth knowing: the app's hot-plug *disconnect* handler
also filters on the name. On a unit whose port name lacks "vflex", the firmware-update jump (§10)
would be reported as failed even though it succeeded, because that step waits for a disconnect
event. A VID-anchored Go implementation avoids this bug.

---

## 4. Linux transport strategy

### 4.1 Primary: pure-Go ALSA rawmidi (recommended)

Open `/dev/snd/midiC<card>D<device>` with `os.OpenFile(path, os.O_RDWR, 0)` and use plain
`Read`/`Write`. No ioctls are needed; alsa-lib's `hw` rawmidi plugin does nothing more.

Why this is the right default here:

- Every byte on the wire is well-formed MIDI (§3.1), so the concern that normally forces raw USB —
  ALSA silently dropping non-conformant bytes — **does not apply**. (We confirmed ALSA *would* drop
  a raw 8-bit stream: a data byte arriving in `STATE_UNKNOWN` matches no case in
  `snd_usbmidi_transmit_byte`. The device simply never sends one.)
- `CGO_ENABLED=0` → a single static binary for amd64 and arm64.
- No kernel driver detach, no root, and no disrupting other MIDI clients.
- On systemd distros the node is already accessible to the seat user (§4.4).

Caveats to design for:

- rawmidi is opened **exclusively per direction**. Handle `EBUSY` with a message naming the likely
  culprit (PipeWire, JACK, a DAW) and pointing at `--transport usb`.
- Use `O_NONBLOCK` + poll/epoll via `golang.org/x/sys/unix`, or a dedicated blocking reader
  goroutine — either is fine for a CLI.

### 4.2 Fallback and bootloader: direct USB via usbfs

Required for the bootloader (vendor class 0xFF, no MIDI at all), and useful when rawmidi returns
`EBUSY` or when you need deterministic pacing.

**Implemented without cgo, and without a build tag.** This section originally called for gousb
behind a `usb` tag, accepting cgo and libusb-1.0 for the two paths that need raw USB. That trade
was declined. The ioctls involved are a small, stable uapi surface — `USBDEVFS_CONTROL`,
`USBDEVFS_BULK`, `USBDEVFS_SETINTERFACE`, `USBDEVFS_DISCONNECT_CLAIM`, `USBDEVFS_CONNECT` — so
`internal/usbfs` binds them directly through `golang.org/x/sys/unix`. Both `--transport usb` and
the bootloader are consequently in the **default** build: one static, cgo-free binary does
everything including firmware update, and no `//go:build usb` tag exists anywhere in the tree.

At the USB level every MIDI message is wrapped in a **4-byte USB-MIDI Event Packet**:
`byte0 = (cableNumber << 4) | codeIndexNumber`, then up to 3 MIDI bytes. For VFLEX traffic:

```
SOF   0x80 0x00 0x00  ->  08 80 00 00     (CN=0, CIN=0x8 Note Off)
DATA  0x90 hi   lo    ->  09 90 hi   lo   (CN=0, CIN=0x9 Note On)
EOF   0xA0 0x00 0x00  ->  0A A0 00 00     (CN=0, CIN=0xA Poly Key Pressure)
```

Inbound: 4-byte stride, skip packets whose `byte0 == 0`, take
`cin_length[byte0 & 0x0F]` bytes starting at offset 1, where
`cin_length = {0,0,2,3,3,1,2,3,3,3,3,3,2,2,3,1}`.

Interface selection: find the alt setting with `(Class == 0x01 || Class == 0xFF) && SubClass == 0x03`.

> **⚠ Do not hardcode bulk endpoints.** `snd-usb-audio` fully supports interrupt endpoints for
> USB-MIDI (`midi.c:2006`: `if (!usb_endpoint_xfer_bulk(ep) && !usb_endpoint_xfer_int(ep)) continue;`),
> and VFLEX's descriptors are unknown. Read `bmAttributes` and use interrupt transfers when the
> endpoint is interrupt. Also check whether the device declares a MIDI 2.0/UMP alt setting, in which
> case the kernel may expose UMP rather than legacy rawmidi.

Detaching the kernel driver: `USBDEVFS_DISCONNECT_CLAIM` detaches and claims atomically, closing
the race where udev re-binds in between; the older `USBDEVFS_DISCONNECT` + `CLAIMINTERFACE` pair is
the fallback on kernels that lack it. **While detached the ALSA card and its `/dev/snd/midiC*D*`
node disappear**, so any DAW or PipeWire client loses the port. Always reattach on exit —
`USBDEVFS_CONNECT` is what rebinds the driver, and closing the fd does not do it for you.

### 4.3 Go MIDI libraries — recommendation: use none

`gitlab.com/gomidi/midi/v2` (v2.3.24, actively maintained) has a suitable API shape
(`Send([]byte)`, `Listen`), but **there is no ALSA-rawmidi driver in the project**. Every usable
driver is a separate module last released 2022-04-19, and:

- `rtmididrv` — cgo + RtMidi (C++) + libasound, and RtMidi's Linux backend uses the **ALSA
  sequencer**, adding two parse/re-encode stages between your bytes and the wire for zero benefit.
- `portmididrv` — cgo + PortMidi.
- `midicatdrv` — no cgo, but shells out to an external `midicat` binary.

Also rejected: `github.com/yobert/alsa` (PCM only, no MIDI), `github.com/karalabe/usb` (stale, HID).

gomidi's value is MIDI *semantics*; this protocol needs none — just three fixed 3-byte messages. If
you want the ecosystem anyway, implement `drivers.Driver/In/Out` over rawmidi yourself (~200 LOC, no
cgo) rather than adopting `rtmididrv`.

**Verified dependency set** (queried from proxy.golang.org, 2026-08-06):

| Module | Version | Date | cgo |
|---|---|---|---|
| `github.com/spf13/cobra` | v1.10.2 | 2025-12-03 | no |
| `golang.org/x/sys/unix` | — | — | no |
| `github.com/google/gousb` | v1.1.3 | 2024-02-24 | **yes** |
| `github.com/gotmc/libusb/v2` | v2.6.0 | 2026-04-14 | **yes** |

The two cgo rows are recorded for completeness; neither is used. The shipped dependency set is
exactly **cobra + x/sys**, and raw USB is `internal/usbfs` (§4.2). There is one build, it is
cgo-free, and it contains every transport.

### 4.4 udev rules

**On systemd distros no rule is needed for the rawmidi path.**
`/usr/lib/udev/rules.d/70-uaccess.rules` already contains
`SUBSYSTEM=="sound", TAG+="uaccess"`, which grants the active seat user an ACL on every `/dev/snd`
node. The same file has no generic USB rule, so a custom rule **is** required for usbfs and the
bootloader.

Ship `packaging/udev/70-gflex.rules`:

```
# Werewolf VFLEX -- Tundra Labs, USB vendor 0x37BF (14271 decimal).
# Product ID is deliberately not matched: the vendor app filters on VID only.

# Raw USB (usbfs) for the direct-USB transport and the class-0xFF bootloader.
# ATTR{} (not ATTRS{}) is correct: the usb_device node owns idVendor itself.
SUBSYSTEM=="usb", ATTR{idVendor}=="37bf", MODE="0660", TAG+="uaccess"

# Non-seat fallback -- ssh, headless, service accounts, non-systemd distros.
#SUBSYSTEM=="usb", ATTR{idVendor}=="37bf", MODE="0660", GROUP="plugdev"

# ALSA rawmidi node. Redundant on systemd; needed only headless / non-systemd.
# ATTRS{} (not ATTR{}) is required: the sound node has no idVendor of its own,
# so udev must walk up to the USB parent.
#SUBSYSTEM=="sound", ATTRS{idVendor}=="37bf", MODE="0660", GROUP="audio"
```

Gotchas: `idVendor` matches as lowercase hex with no `0x`; `ATTR{}` on a `sound` node silently never
matches (the classic failure mode); `plugdev` exists on Debian/Ubuntu but not Fedora/Arch, so make
the group a packaging variable and prefer `uaccess`.

> **The vendor's published udev rule is wrong** and must not be copied. It reads
> `SUBSYSTEM=="hidraw", KERNEL=="hidraw*", ATTRS{idVendor}=="37bf", ATTRS{idProduct}=="800f",
> MODE="0666", GROUP="plugdev", TAG+="snap_chromium_daemon", TAG+="uaccess"`.
> The app never uses HID — it uses Web MIDI (ALSA rawmidi on Linux) and WebUSB (usbfs). `MODE="0666"`
> is world-writable and redundant alongside `uaccess`; `snap_chromium_daemon` is Ubuntu-snap-specific.
> The page is also visibly AI-generated (the rule is tagged as a "Python" code block). The PID
> `0x800f` in it is the only PID reference anywhere and is **uncorroborated** — see §14.1.

---

## 5. Application protocol

### 5.1 Frame format

```
byte[0] = total frame length = 2 + len(payload)
byte[1] = command | flags
byte[2…] = payload
```

Flags: `CMD_FLAG_WRITE = 0x80`, `CMD_FLAG_SCRATCHPAD = 0x40`, `CMD_CODE_MASK = 0x3F`.

- A **read** is always the 2-byte frame `[0x02, code]`.
- A **write** is `[2+n, code|0x80, payload…]`.
- **All multi-byte scalars are BIG-ENDIAN.** The only little-endian data in the entire system is the
  PDO log blob (§9.3).
- The length field is a single byte, so a frame cannot exceed 255 bytes; responses are further
  constrained to 2–64 bytes by the receiver (§3.3), i.e. payload ≤ 62.

**`CMD_FLAG_SCRATCHPAD` (0x40) is never set by the shipped app.** The only code path that could
(`VFlexProtocol.stringWrapper`) has zero callers in the 6 MB bundle, and the constant is imported
nowhere. Its volatile-vs-NVM meaning is therefore **UNKNOWN**. Expose it only through a raw escape
hatch, never as a `--volatile` flag implying known semantics.

### 5.2 Response handling

```
declaredLen  = resp[0]
effectiveLen = (declaredLen >= 2 && declaredLen <= len(resp)) ? declaredLen : len(resp)
frame        = resp[:effectiveLen]
code         = frame[1] & 0x3F        // flag bits are masked off and never inspected
```

Semantics the reference client applies, worth matching:

- If no command is outstanding → log `unexpected_frame_while_waiting`, drop.
- If `len(resp) < 2` → ignore **without** clearing pending state (the command then times out).
- If `code != pendingCode` → log `ack_cmd_mismatch`, drop the frame, but **leave the wait pending**
  so a later matching frame can still satisfy it. Do not treat this as a hard error.
- Otherwise dispatch, then clear the pending slot and set ACK.

`awaitResponse` polls every 25 ms with a **5000 ms** default timeout and rejects with
`"Response timeout exceeded"`. There is **no NACK or device-reported error** anywhere in the
protocol: a command either gets a matching response or times out.

**Write-echo suppression.** The device echoes writes. For commands 15, 22, 23, 24, 25, 26, 27 the
client deliberately ignores the echo; for 16, 17, 18, 19, 28 and the identity strings it parses the
echo as if it were a read. Reproduce this or, better, always read back explicitly — which is what
the app does for voltage anyway.

### 5.3 Concurrency

The protocol is **strictly half-duplex, one command in flight.** There is no tagging and no
pipelining: `VFlexProtocol` has exactly one pending slot (`ack`, `ackCmd`, `expectingResponse` are
scalars) and the app serialises every operation through a promise-chain mutex (`runExclusive`). A Go
implementation **must** serialise all device access through a single mutex.

---

## 6. Command reference

29 commands, codes 0–28. "Used" means the shipped app actually issues it.

| # | Name | Dir | Payload / response | Used |
|---|---|---|---|---|
| 0 | `CMD_BOOTLOADER_WRITE_CHUNK` | W | `[pgHi, pgLo, chunkId, data…]` | bootloader |
| 1 | `CMD_BOOTLOADER_COMMIT_PAGE` | W | empty | bootloader |
| 2 | `CMD_BOOTLOADER_VERIFY` | R/W | resp `[len, code, crc8]` | bootloader |
| 3 | `CMD_BOOTLOAD_END` | — | empty; jump to app | bootloader |
| 4–7 | `CMD_RESERVED0…3` | — | **UNKNOWN** | no |
| 8 | `CMD_SERIAL_NUMBER` | R | 8 ASCII bytes | **yes** |
| 9 | `CMD_CHIP_UUID` | R | 8 ASCII bytes | no |
| 10 | `CMD_HARDWARE_ID` | R | 8 ASCII bytes | no |
| 11 | `CMD_FIRMWARE_VERSION` | R | 12 ASCII bytes | **yes** |
| 12 | `CMD_MFG_DATE` | R | 8 ASCII bytes | no |
| 13 | `CMD_FLASH_LED_SEQUENCE_ADVANCED` | — | **UNKNOWN** | no |
| 14 | `CMD_FLASH_LED` | — | **UNKNOWN** | no |
| 15 | `CMD_DISABLE_LED_DURING_OPERATION` | R/W | 1 byte, **inverted** (§6.2) | **yes** |
| 16 | `CMD_ENCRYPT_MSG` | W | n bytes out, n bytes back | no |
| 17 | `CMD_PDO_LOG` | R/W | §9 | **yes** |
| 18 | `CMD_VOLTAGE_MV` | R/W | u16 BE, millivolts | **yes** |
| 19 | `CMD_CURRENT_LIMIT_MA` | R/W | u16 BE, milliamps | **yes** |
| 20 | `CMD_JUMP_APP_TO_BOOTLOADER` | — | empty, no ACK | **yes** |
| 21 | `CMD_IOS_HOST_MODE_FLAG` | R | response discarded | no |
| 22 | `CMD_AUTHLOCK` | R/W | §6.3 — **asymmetric** | write only |
| 23 | `CMD_USER_VLIMIT` | R/W | 4 bytes, **HIGH then LOW** | **yes** |
| 24 | `CMD_VTOLERANCE_NOMINAL_MV` | R/W | u16 BE, millivolts | write only |
| 25 | `CMD_VTOLERANCE_SAG_PER_MA` | R/W | u16 BE, **units UNKNOWN** | no |
| 26 | `CMD_VMEASURE_ADC_COUNT_OFFSET` | R/W | **i32** BE (signed) | write only |
| 27 | `CMD_VMEASURE_ADC_COUNT_SCALE` | R/W | **i32** BE (signed) | write only |
| 28 | `CMD_VMEASURE` | R | u16 BE raw ADC + u16 BE mV | no |

### 6.1 Ready-made frames

```
read  serial              02 08
read  chip uuid           02 09          (never issued by the app; response format assumed)
read  hardware id         02 0A          (ditto)
read  firmware version    02 0B
read  mfg date            02 0C          (ditto)
read  led setting         02 0F      write always-on:  03 8F 00
                                     write off-when-green: 03 8F 01
clear pdo log             02 91
read  pdo chunk k         03 11 kk
read  voltage             02 12      write 12.000 V:   04 92 2E E0
read  current limit       02 13      write 5000 mA:    04 93 13 88
jump to bootloader        02 14      (no ACK; device disconnects immediately)
read  authlock            02 16      write unlocked:   03 96 00
read  vlimit              02 17      write low=3300 high=48000: 06 97 BB 80 0C E4
read  vtol nominal        02 18      write 750 mV:     04 98 02 EE
read  vtol sag            02 19      write v:          04 99 hh ll
read  adc offset          02 1A      write 0:          06 9A 00 00 00 00
read  adc scale           02 1B      write 0:          06 9B 00 00 00 00
read  vmeasure            02 1C
```

### 6.2 LED — the sense is inverted

```
encode: wireByte = alwaysOn ? 0 : 1
decode: alwaysOn = (wireByte == 0)
```

`0x00` = "LED Always On" **enabled** (LED lit in Power Good). `0x01` = suppressed while green.
**Do not name the CLI flag after the wire field** (`disable_led_during_operation`) or users will get
it backwards. Use `gflex led set on|off` where `on` means the user-facing "always on".

### 6.3 AUTHLOCK — asymmetric, and unexercised

Write puts the level in the **first** payload byte (`[0x03, 0x96, level]`). The read parser takes
`device_data.authlock_level = frame[3]` — the **second** payload byte. This is confirmed verbatim in
the source and is the only genuinely asymmetric command in the protocol.

Two possibilities, undecidable from the client: the read response really carries two payload bytes
(e.g. `[maxLevel, currentLevel]`), or it is an off-by-one bug. `getAuthLock` has **zero callers**, so
that path was never exercised. **A Go tool should read and log both `frame[2]` and `frame[3]`** until
this is checked on hardware.

Only `AUTH_LOCK_UNLOCKED = 0` is defined anywhere. What other levels exist, what they gate, and how
to unlock are **UNKNOWN**. Note that the post-firmware-update sequence writes `setAuthLock(0)` *first*,
before every other parameter write — suggesting the lock gates the other writes, but that is
inference, not proof.

New lead: the Android Hermes string table contains `SET_AUTH_LOCK_LEVEL`, `refreshAuthLockLevel` and
`authLockLevel`, none of which exist in the web bundle — a newer native build surfaces the lock level
in its UI. Worth revisiting with a newer APK.

### 6.4 Identity strings

Fixed payload lengths: serial 8, chip UUID 8, hardware ID 8, firmware version 12, mfg date 8.

Read `bytes[2:frame[0]]` and sanitise by dropping NUL, U+FFFD, and everything outside `0x20–0x7E`,
then trim — the firmware NUL-pads. A serial is considered usable only when ≥ 4 chars after
sanitising.

Do **not** replicate the client's decode, which UTF-8-decodes the entire frame *including* the
length and command bytes and then slices off two *characters*; that misaligns on any byte ≥ 0x80.
Also do not assume the response lengths: the length table is only consulted by an unreachable write
path, so take whatever `frame[0]` declares.

Commands 9, 10 and 12 are never issued by the app. The frames are constructible and the parser
exists, so the CLI can offer them — but flag in `--help` that the firmware's willingness to answer is
unverified.

### 6.5 Voltage, current, limits

**`CMD_VOLTAGE_MV` (18)** — u16 BE millivolts. **The app applies no range validation whatsoever**:
`Math.round(1000 * volts)` with no clamp, no min, no max. Values above 65535 silently wrap. Range
checking is entirely the Go tool's job (§13).

Read semantics: the app treats a returned **0 mV as "not ready"** and retries (3 attempts, 300 ms
apart), returning null if never > 0. Match this or you will report 0 V on a freshly connected
device. After a write, always issue an explicit read-back rather than trusting the echo.

**`CMD_CURRENT_LIMIT_MA` (19)** — u16 BE milliamps. On every successful connect the app performs a
read-modify-write to 5000 mA if it differs. This is a **negotiation request, not a measurement** —
the hardware has no current sensing, so never report it as a measured value.

**`CMD_USER_VLIMIT` (23)** — the wire order is **HIGH first, then LOW**, in *both* directions:

```
write: [0x06, 0x97, high>>8, high&0xFF, low>>8, low&0xFF]
read:  frame[2:4] = high,  frame[4:6] = low
```

The confusion is that the *API* signature is `setVLimit(lowMv, highMv)` — reversed from the wire.
Keep that reversal out of the encoder. Defaults 3300 / 48000 mV. The app rewrites the pair after a
firmware update if the read-back is invalid, defined as: low not finite, high not finite,
low < 3000, high < 6000, or high ≤ low.

**Tolerance (24, 25).** Nominal is u16 BE millivolts, default 750. Sag is u16 BE with **unknown
units and no default** — the app never reads or writes it. A literal "mV per mA" reading is
dimensionally implausible at integer resolution (1 unit = 1 Ω), so a scale factor almost certainly
exists. Expose it as a raw u16; do not invent a unit.

The manual's claim that tolerance is "automatically calculated or specified by the device selection"
has **no counterpart in this app build** — there is no tolerance field on a saved device and no code
computing one. Firmware-side behaviour is the only plausible explanation.

**Calibration (26, 27, 28).** Offset and scale are **signed int32** big-endian — JavaScript's
`<<`/`|` produce int32, so a top-bit-set response reads back negative and the reference
implementation is signed in both directions. Use `int32`, not `uint32`. Both default to 0, which
implies the firmware treats 0 as "use built-in calibration" rather than as a literal multiplier
(otherwise every reading would be zero) — inference, not proof.

**There is no host-side calibration formula anywhere in the client.** The device computes the
calibrated millivolts itself and returns them in `CMD_VMEASURE`. The fixed-point interpretation of
`scale` is **UNKNOWN**.

---

## 7. Session semantics

This is the vendor's choreography, and it encodes real timing requirements discovered by the
vendor. It is reproduced **in substance, not literally** — see §17 for the two rows that say how
and why. In short: the *persistence* is kept, because without it a freshly plugged unit answers
0 mV and fails; the fixed 500 ms + 800 ms settle and the ~25 s backoff chain are replaced by an
adaptive backoff against a ~10 s budget that a ready device pays nothing for, and
`ensureCurrentLimitMa(5000)` is not issued at all, because a CLI that rewrites your current limit
during `voltage get` is a bug rather than parity.

**On connect** (module 2815):

```
sleep 500 ms
getSerialNumber()                         -> trim
sleep 800 ms
readVoltageWithRetry(maxAttempts=3, delay=300ms)   // 0 mV counts as failure
   on total failure: backoff retry chain starting at 1500 ms, +1000 ms per attempt,
                     max 5 attempts, each doing 4 reads 400 ms apart
ensureCurrentLimitMa(5000)                // read-modify-write, failures ignored
```

Whenever connected and not suspended, the app additionally reads the LED setting and the vlimit
pair; on resume from suspend it re-reads serial and voltage.

**Timeouts:** 5000 ms default; 8000 ms per PDO-log chunk; 15000 ms for bootloader ACKs; 120000 ms
for CRC verify.

**State to model:** `{connected, connecting, resetting, suspended, serialNumber, voltageMv,
vlimitLowMv, vlimitHighMv, ledAlwaysOn, lastError}`.

**Invalidate the cache on disconnect.** The reference client never clears its `device_data` object —
a latent staleness bug worth not reproducing.

**Surface write errors.** The reference `port.send` swallows every exception, emits an `error` event,
and resolves; a failed write therefore surfaces ~5 s later as a generic timeout. Return a real error.

---

## 8. Device data model

Fields the protocol populates, with the command that sets each. Reuse these names for `--json`
output so the spec and the tool agree.

| Field | Type | Unit | Cmd |
|---|---|---|---|
| `serial_num` | string | — | 8 |
| `uuid` | string | — | 9 |
| `hw_id` | string | — | 10 |
| `fw_id` | string | — | 11 |
| `mfg_date` | string | — | 12 |
| `led_disable_during_operation` | u8 | inverted flag | 15 |
| `secretsecrets` | []byte | — | 16 |
| `pdo_payload`, `pdo_last_chunk_id`, `pdo_last_chunk_payload` | — | — | 17 |
| `voltage_mv` | u16 | mV | 18 |
| `current_limit_ma` | u16 | mA | 19 |
| `authlock_level` | u8 | — | 22 |
| `vlimit_high_mv`, `vlimit_low_mv` | u16 | mV | 23 |
| `vtolerance_nominal_mv` | u16 | mV | 24 |
| `vtolerance_sag_per_ma` | u16 | **unknown** | 25 |
| `vmeasure_adc_offset`, `vmeasure_adc_scale` | **i32** | counts | 26, 27 |
| `vmeasure_raw_adc` | u16 | ADC counts | 28 |
| `vmeasure_calibrated_mv` | u16 | mV | 28 |
| `crc` | u8 | — | 2 |

---

## 9. Power-supply capability scan

The VFLEX captures a 90-byte "PDO log" while attached to a PD source, which the host reads back
afterwards. **Requires firmware ≥ 5.0.0** — the app hard-gates on this and so must the CLI.

### 9.1 Wire protocol

```
erase:      [0x02, 0x91]                    // write flag, empty payload
read chunk: [0x03, 0x11, chunkIndex]
response:   [0x0B, 0x11, chunkId, b0…b7]    // byte[2] echoes the index, bytes 3-10 are data
```

Download: request chunks 0–11 sequentially, 8000 ms per chunk, **three attempts per chunk** — not
three retries on top of a first try — 250 ms apart. Accept a chunk only when
`receivedChunkId == requestedIndex` **and** the payload is non-empty; a chunk shorter than the full
8 bytes is also rejected and retried rather than appended (§17). Concatenate (96 bytes), require
≥ 90, keep the first 90, and reject an all-zero blob.

### 9.2 Workflow and the serial invariant

```
1. connect              gate on firmware >= 5.0.0
2. enter scan mode      latch expectedSerial := currentSerial; send erase [0x02,0x91]
3. disconnect           wait for the device to go away
4. attach to the PD source under test; wait ~5 s (green or red LED = scan done)
5. reconnect            re-read serial
                        != expectedSerial            -> abort, "serialMismatch"
                        serial unreadable            -> abort
6. download             12 chunks
7. complete
```

The serial equality check is a hard invariant: the unit whose log was erased must be the unit read
back. Serial reads during this phase use 6 attempts, 300 ms apart.

### 9.3 Blob layout — **little-endian**, unlike the rest of the protocol

```
[0..1]   u16 LE  targetVoltageMv
[2..3]   u16 LE  measuredVoltageMv
[4]      u8      nPdosReceived
[5]      u8      idSelectedPdo
[6..7]   u16 LE  flags
[8..9]   u16 LE  flags2      (bit 3 / 0x0008 = eprCableFail)
[10..89] 20 × u32 LE  standard USB-PD Source_Capabilities PDOs
```

Only `min(nPdosReceived, 20)` entries are parsed; bytes beyond that are never read. Only
`eprCableFail` is consumed downstream — target/measured voltage, selected PDO id and `flags` are
parsed and discarded. The CLI should surface all of them; they are free diagnostics.

### 9.4 PDO decode

`pdoType = (pdo >> 30) & 3`. Zero words are skipped.

**Fixed (type 0):**
```
voltageV    = 0.05 * ((pdo >> 10) & 0x3FF)     // 50 mV units
maxCurrentA = 0.01 * ( pdo        & 0x3FF)     // 10 mA units
keep only if voltageV >= 5 && maxCurrentA > 0
section: voltageV <= 20 -> SPR, else EPR       // EPR fixed uses the same 50 mV scale
```

**Augmented (type 3),** `subtype = (pdo >> 28) & 3`:
```
0 SPR_PPS:  maxV = 0.1*((pdo>>17)&0xFF)   minV = 0.1*((pdo>>8)&0xFF)   maxI = 0.05*(pdo&0x7F)
1 EPR_AVS:  maxV = 0.1*((pdo>>17)&0x1FF)  minV = 0.1*((pdo>>8)&0xFF)   pdpW = pdo&0xFF
2 SPR_AVS:  maxI20V = 0.01*((pdo>>10)&0x3FF)   maxI15V = 0.01*(pdo&0x3FF)
3        :  ignored
validity:  pps/epr_avs need minV>0, maxV>0, maxV>=minV, and maxI>0 / pdpW>0
           spr_avs needs maxI15V>0 || maxI20V>0
section:   epr_avs -> EPR variable;  pps and spr_avs -> SPR variable
```

An `EPR_AVS` APDO that **fails** validation sets `eprCableFail = true`, a second computed source for
that flag besides `flags2` bit 3.

**Battery (type 1) and Variable (type 2) PDOs are silently ignored by the app.** The CLI should at
minimum report their presence rather than pretend they do not exist.

> ⚠ The SPR-AVS field naming (`maxI20V` from bits 19:10, `maxI15V` from bits 9:0) could not be
> verified against the USB-PD 3.2 field order. If the firmware follows the published layout the two
> may be swapped. Low impact — the only consumer takes `max()` of the two.

### 9.5 Compatibility check (worth reimplementing offline)

Given a requested voltage V (volts), requested current I (amps), and a scanned port:

```
KNOWN = {5,9,12,15,20,28,36,48}
if V > 20:                                       # EPR
    eprFixedA > 0        -> ok; if eprAvs fits and has more current and fixed < I, upgrade to AVS
    else eprAvs fits     -> ok at eprAvs.maxCurrent
    else eprSupported    -> "EPR fixed not supported" / "AVS range only" / "AVS not supported"
    else                 -> "Power source with Extended Power Range is Required"
else:                                            # SPR
    sprFixedA > 0        -> ok; if PPS fits and has more current and fixed < I, upgrade to PPS
    else PPS fits        -> ok; if SPR-AVS fits with more current, upgrade
    else SPR-AVS fits    -> ok
    else                 -> "fixed not supported" / "PPS range only" / "AVS range only"
```

> **Known defect to fix, not copy:** this algorithm reads only `.amps` for fixed voltages and never
> checks the sibling `.enabled` flag. A CLI that models supported voltages as "the enabled set" will
> diverge from the app on records where the two disagree.

### 9.6 Error strings

Reproduce or map these; users will search for them:

```
"No connection established"
"PDO log read returned empty chunk (requested=N, got=M)"
"PDO log chunk mismatch (requested=N, got=M)"
"Invalid PDO log length: expected ≥90 bytes, got N"
"No PDO data captured (log is empty). Unplug vFlex from phone, plug into a USB-C PD charger
 (e.g. MacBook charger) for ~10s, then reconnect and retry."
"Response timeout exceeded"
"A different VFLEX serial number was detected. This scan has been aborted."
"Power Supply Scan requires VFLEX firmware 5.0.0 or newer. Update firmware before scanning."
```

---

## 10. Firmware update

Three phases across two transports. **This is the riskiest feature; ship it last, behind `--yes`.**

### 10.1 State machine

```
1. MIDI mode:  send [0x02, 0x14] (CMD_JUMP_APP_TO_BOOTLOADER), no ACK
               require a disconnect event within 3000 ms as proof the jump took
               wait BOOTLOADER_MODE_SWITCH_DELAY_MS = 4000
2. Bootloader: device re-enumerates. Open by vendor 0x37BF, select configuration 1 if unset,
               pick the first interface alt whose class is 0xFF and that has both IN and OUT
               endpoints, claim it, then a best-effort class control-OUT
               (bRequest 0x22 SET_CONTROL_LINE_STATE, wValue 1, wIndex=ifnum), then a
               64-byte read loop.
               Confirm identity: [0x02, 0x08] over raw bulk returns the serial — the bootloader
               speaks the ordinary 2-byte app protocol for this command.
3. Flash:      stream every chunk and page-commit with NO ACK (2 ms between chunks, 25 ms after
               each commit), wait POST_FLASH_DELAY_MS = 2000, then verify the CRC.
               Mismatch -> full re-flash in ACK mode. Second mismatch -> stop, report error,
               DO NOT jump to the app.
4. Exit:       [0x02, 0x03] (CMD_BOOTLOAD_END) over bulk, wait 4000 ms, re-init MIDI with a
               15000 ms timeout, then run the post-update sequence (§10.4).
```

### 10.2 Bootloader frames — raw bulk bytes, no nibble encoding

Same `[len, cmd|0x80]` preamble, written straight to the bulk OUT endpoint.

```
WRITE_CHUNK   [len, 0x80, pgHi, pgLo, chunkId, data…]    // page id is u16 BE
COMMIT_PAGE   [0x02, 0x81]
VERIFY        [0x02, 0x82] (write form) then [0x02, 0x02] (read form)
BOOTLOAD_END  [0x02, 0x03]
```

Each page is split into exactly 8 equal chunks; a page length not divisible by 8 is an error. Chunk
size is **not** a constant — it is derived from the server payload's page size.

> **Do not design around `BOOTLOADER_PREAMBLE_LEN = 4` or `MAX_VERIFY_PACKET_DATA = 60`.** Both are
> declared in module 2812 and referenced nowhere; the real bootloader preamble is 2 bytes. Several
> other constants there are likewise dead: `PROTOCOL_PREAMBLE_LEN`, `ACK_POLL_INTERVAL_MS`,
> `VERBOSE_CHUNK_STATUS`, `USB_CLASS_VENDOR`, `FIRMWARE_WS_URL_LOCAL`.

**CRC is a single byte** (`frame[2]` of the verify response), compared against the `crc` field of the
firmware payload. The algorithm is **UNKNOWN** — the device computes it and the host only compares.

> **⚠ Endpoint-addressing trap:** the WebUSB code stores `endpointNumber` (a 4-bit number). usbfs —
> like libusb — addresses endpoints by **full `bEndpointAddress`**: `number` for OUT,
> `number | 0x80` for IN. Translate, don't copy.

### 10.3 Firmware image delivery

Fetched *after* the device is already in bootloader mode, over a WebSocket:

- Production: `wss://vflex-nestjs-prod-ylaqjkd4na-uc.a.run.app/bootloader`
  (derived from the REST API URL; the `wss://api.vflex.app/bootloader` constant is a fallback)
- Client sends the **plain serial-number string** on open; the server replies with one JSON blob.
- Schema: `{ app_bin | app_bin_data | firmware: <array of pages>, app_version: string, crc: <u8> }`
- 15000 ms timeout. **No HTTP fallback and no local `.bin` path exist in the client.**
- No server-side auth was observed — only client-side UI gating. Unverified.

Design implication, and what was built: the **local firmware file** is the primary input
(`gflex firmware flash <file>`) and the WebSocket is opt-in behind `--fetch`, which keeps the tool
useful offline and off an undocumented vendor endpoint. The file format is detected from the first
non-whitespace byte — `{` or `[` is the vendor's JSON payload, anything else is a flat binary split
into equal pages. A flat binary carries no CRC, so it needs `--crc` or `--force` (§13.6). The fetch
has its own budget, `--fetch-timeout` (15 s per this section); `--timeout` bounds MIDI commands and
must not be reused for a download.

Version comparison (for "is an update available"): uppercase and trim both strings, extract all
decimal runs, compare element-wise with missing components as 0; an `X` or `*` in the new version
always means "update available".

### 10.4 Mandatory post-update sequence

A flash erases settings. Replay **in this order**, each independently error-tolerant:

```
getVLimit()
setAuthLock(0)
setVLimit(3300, 48000)          // only if the read-back was invalid, or on a major 4 -> 5 jump
setVToleranceNominal(750)
setVMeasureAdcOffset(0)
setVMeasureAdcScale(0)
setMaxCurrentMa(5000)
```

### 10.5 Failure guidance

On CRC mismatch the unit is **still in bootloader mode (slow blink white) and is re-flashable, not
bricked.** Say so explicitly. The vendor ships a web "rescue" page for exactly this; the CLI offers
`gflex firmware flash --recover`, which skips phase 1 and goes straight to the bootloader
interface. With no application-mode serial to compare against, `--recover` reads the serial from
the bootloader itself and enforces the §10.1 identity check against that, so a unit that was never
identified is still never flashed blind.

---

## 11. CLI surface

```
gflex devices                            list candidate ports / USB devices
gflex info [--all]                       identity + settings in one shot; --all adds the
                                         commands the vendor app never issues
gflex voltage   get | set <value>        "12", "12V" and "12000mV" all work; a bare number
                                         is volts, never guessed from magnitude
                  set … --ignore-device-limits    proceed when the unit's own window is
                                                  unreadable or unusable (§13.1, §17)
gflex current   get | set <value>
gflex vlimit    get | set [--low <v>] [--high <v>]   either flag alone keeps the other
gflex tolerance get | set [--nominal <mV>] [--sag <raw-u16>]
gflex measure                            raw ADC + calibrated mV
gflex calibrate get | adc --offset <int32> --scale <int32>
gflex led       get | set on|off         "on" = user-facing "LED Always On"
gflex authlock  get | set <level>
gflex scan [--voltage <v> --current <a>] [--no-prompt] [--wait <d>] [--settle <d>]
                                         guided PDO capture wizard
gflex pdo       dump [--raw] | clear
gflex firmware  version
                bootloader               jump and stop
                flash [file] [--recover] [--fetch] [--ws-url <u>] [--fetch-timeout <d>]
                                         [--crc <byte>] [--force] [--ack-mode]
gflex raw <hex…> [--no-ack] [--any-length]   send a frame verbatim, print the response
gflex monitor [--for <d>]                decode and print live inbound frames
gflex install-udev [--print]             write the rule, reload, report
gflex version
```

Global flags: `--port <name|path>`, `--transport rawmidi|usb` (default `rawmidi`), `--json`,
`--timeout` (default 5s), `--byte-delay` (default 20ms), `-v/--verbose` (hex TX/RX), `--dry-run`,
`-y/--yes`.

**There is no global `--force`.** An earlier draft of this section listed one alongside `--yes`,
and it was dropped deliberately: `--force` already means something specific and narrow on
`firmware flash` — *this image carries no CRC, flash it unverified* — and a persistent flag of the
same name would shadow that on the single most dangerous command in the tool, so `--force` would
start reading as "and also skip verification". The interlocks of §13 are gated on `--yes` or an
interactive answer instead; the one place that needs a second, differently-named key uses
`--ignore-device-limits`, which nobody passes out of habit.

Output: human-readable by default; `--json` emits a stable flat object using the §8 field names in
**native wire units** (millivolts, milliamps). Never convert units in the data path. Exit codes are
distinct per failure class: 0 success, 1 generic failure, 2 usage, 3 no device, 4 EBUSY, 5 timeout,
6 permission denied, 7 refused-by-interlock. There is deliberately no "ACK mismatch" code — a
mismatched response leaves the wait pending (§5.2), so it surfaces as a timeout and nothing else.

**Configuration: environment variables only; the config file was dropped.** Precedence is
flag > env (`GFLEX_*`) > default, and `$XDG_CONFIG_HOME/gflex/config.toml` is not read. An earlier
draft required it. Parsing TOML means either a dependency outside the fixed set (cobra, x/sys,
stdlib) or a hand-rolled parser in a tool whose failure mode is a wrong voltage — and the file
would carry no setting the environment does not already carry. `GFLEX_PORT`, `GFLEX_TRANSPORT`,
`GFLEX_TIMEOUT`, `GFLEX_BYTE_DELAY`, `GFLEX_JSON` and `GFLEX_VERBOSE` cover the whole persistent
surface: an operator who wants a fixed port or a shorter byte delay exports one line.

`--dry-run` and `--yes` are the two global flags with **no** environment counterpart, and that is
also deliberate. Both change whether the device is written to, and a `GFLEX_YES` left exported in a
shell profile would silently pre-answer every §13 confirmation for months afterwards.

---

## 12. Architecture

Three layers, so the protocol lives in exactly one place and each transport is a thin byte-level
implementation that knows nothing about VFLEX commands.

```
Layer 1  proto.Transport     byte-level MIDI, no protocol knowledge
         ├─ rawmidi.Stream   os.File over /dev/snd/midiC*D*             (default)
         └─ usbmidi.Stream   usbfs bulk/interrupt + 4-byte packet codec (--transport usb)

Layer 2  framer.Framer       SOF / nibble / EOF codec + the RX state machine of §3.3
                             ByteDelay is configurable here; a drop hook reports
                             malformed frames instead of swallowing them (§17)

Layer 3  session.Session     typed accessors over the command table, single-flight
                             mutex, ACK matching, per-command timeouts

Aside    bootloader          bypasses layers 1-2 entirely: raw frames straight onto a
                             usbfs bulk endpoint, no MIDI framing at all (§10.2)
```

**Everything is one cgo-free build.** There is no `//go:build usb` tag and no libusb: raw USB is
`internal/usbfs`, a direct binding to the Linux usbfs ioctls (§4.2), so `--transport usb` and
firmware update ship in the same static binary as the default path.

```
cmd/gflex/main.go
internal/proto/              command table, frame encode/decode, device constants,
                             the Transport interface -- the shared contract
internal/framer/             layer 2: nibble MIDI encoder, status-byte-driven decoder
internal/session/            layer 3: session, typed accessors, info, PDO log download
internal/pdo/                PDO blob decode + power-source compatibility evaluation;
                             pure computation, no device and no I/O
internal/transport/rawmidi/  ALSA rawmidi + sysfs discovery by USB vendor ID
internal/transport/usbmidi/  direct USB-MIDI over usbfs
internal/transport/fake/     in-memory stream + scripted device
internal/usbfs/              pure-Go Linux usbfs binding (ioctls, descriptors, enumeration)
internal/bootloader/         firmware image model, flasher, update sequence, WebSocket fetch
internal/cli/                cobra tree, human + json formatters, the §13 interlocks
testdata/golden/frames.json
packaging/udev/70-gflex.rules
```

Two splits are worth naming because an earlier draft of this section had them wrong. `internal/proto`
holds the *contract* only — no session, no accessors, no I/O — so that every other package can
depend on it without depending on a transport; the typed accessors live in `internal/session`. And
`internal/pdo` is deliberately separate from the download in `internal/session`: decoding a blob and
judging a power source are pure functions over 90 bytes, testable exhaustively and fuzzable without
a device.

### Testing without hardware

- **Golden vectors** (§15) — `testdata/golden/frames.json` pins every vector as
  (frame hex ↔ MIDI stream hex ↔ USB-MIDI packet hex), in both directions: the TX entries are what
  an encoder must produce, the RX entries are what a decoder must accept, and two malformed RX
  entries pin what it must *drop*.
- **Fake device** — `fake.Device` speaks the real wire protocol over an in-memory pipe, decoding the
  MIDI stream with a **deliberately independent** implementation of the §3.3 receive state machine,
  so a bug in the encoder cannot hide behind a matching bug in the decoder. Round-trip every typed
  accessor.
- **RX state machine** must have explicit tests for: mid-frame SOF resetting the buffer; >64 bytes
  truncating without aborting; `declaredLen` of 0, 1, >64, or > buffered, all dropped; a `Read()`
  splitting a message across calls; unrelated MIDI (0xB0 CC, 0xF8 clock) ignored; trailing partial
  message ignored.
- **Protocol layer** — a mismatched command code leaves the wait pending and the call ends in a
  timeout, never in a distinct "ACK mismatch" error (§5.2); the mismatched frame is still traced,
  because it is exactly the evidence §14.13 and §14.14 need; unexpected frames while idle are
  dropped; write echoes do not clobber cached state for guarded commands.
- **Fuzz** — `framer.FuzzDecoder` and `fake.FuzzFrameDecoder` must never panic and never emit a
  frame violating the length invariants, `fake.FuzzFrameRoundTrip` checks encode→decode for all
  `b` in 0..255, and `proto.FuzzParse` and `pdo.FuzzParse` cover the frame and blob parsers.

Two testing requirements in earlier drafts of this section are **not implemented**, and are recorded
here rather than quietly dropped:

- **No clock is injected.** Timeout, backoff and retry tests sleep for real, which costs the
  `internal/session` suite about 15 s of wall time. It is honest — the same code paths run as in
  production — but it is why that one package dominates `go test ./...`.
- **There is no hardware-test harness** (`//go:build hardware` + `GFLEX_TEST_PORT`). Nothing has
  ever been run against a device, so there was nothing to gate; §14 is the bring-up plan instead.
  Anyone with a unit adding one should keep it out of the default build, as originally specified.

Packaging: `CGO_ENABLED=0` static binaries for amd64/arm64. The udev rule is embedded in the binary
and `gflex install-udev` writes it to `/etc/udev/rules.d/70-gflex.rules`, which is the correct
location for a manual install; a distro package should ship the same file under
`/usr/lib/udev/rules.d/` instead, so that a local edit still wins. No release tooling
(goreleaser/nfpm, deb/rpm/apk) exists yet — `go build` is the only supported route today. The
PipeWire/JACK `EBUSY` → `--transport usb` escape route is documented in the README.

---

## 13. Safety interlocks

This device drives a power rail into someone's guitar pedal. Wrong values destroy hardware, and the
vendor app has **no range validation at all**. The CLI must supply it.

The gate is **`--yes`, or an interactive answer**. There is no global `--force` (§11), so where an
item below once said "require `--force`" it now says what the code does: confirm, every time. The
one interlock that needs a second key uses a self-describing name rather than a second general one.

1. **`voltage set`** — always read `CMD_USER_VLIMIT` first and refuse outside `[low, high]`. Also
   bound against the documented envelope 3300–48000 mV, and reject anything > 65535 that would
   silently wrap in the 16-bit field. If the window cannot be read, or the unit reports a pair that
   cannot bound anything (`high <= low`), **refuse** — do not fall back to the hardware envelope
   (§17). `--ignore-device-limits` is the deliberate override, and `--yes` is deliberately not: the
   routine scripting flag must not be able to discard the owner's own guard rail.
2. **`voltage set` above 5000 mV** — require interactive confirmation, or `--yes` on a non-TTY.
3. **`vlimit set` that widens the window** — confirm; it removes the guard rail interlock 1 depends
   on. Narrowing is safe and does not prompt. When the current pair cannot be read, widening cannot
   be told from narrowing, so the dangerous case is assumed and it confirms.
4. **`authlock set`** — confirm every write, and for any non-zero level print that the level has no
   documented effect, that only level 0 is named anywhere, and that there may be no way back
   (§6.3, §14.8). It is not refused outright: `authlock` is one of the commands §14.8 needs
   exercised on real hardware, and a hard refusal would push that experiment into
   `gflex raw 03 96 <level>`, where the warning is generic and the payload is unchecked.
5. **`calibrate adc`** — confirm. A wrong offset/scale makes every subsequent voltage reading
   silently wrong, defeating interlock 1. Print the previous values together with the exact command
   that restores them, or the factory defaults when they could not be read.
6. **`firmware flash`** — confirm; verify the CRC before jumping to the app; **never** send
   `CMD_BOOTLOAD_END` on a mismatch; on failure state clearly that the unit is still in bootloader
   mode and is re-flashable. `--force` here has one narrow meaning — *this image carries no CRC, so
   flash it unverified* — and it can neither suppress verification of an image that does carry one
   nor override a mismatch.
7. **Refuse all of the above on a non-TTY stdin without `--yes`**, so a script cannot silently
   over-volt a load.
8. **`--dry-run` must print the exact frame and MIDI bytes** for every command, including dangerous
   ones. It is refused only where the frame cannot be known without first reading the device.
9. **Warn above 20 V** that an eMarker/EPR-rated 5 A cable is required, and that a fast-blinking red
   LED means exactly that.
10. **`gflex raw`** — the escape hatch is guarded too, which nothing above anticipated. A read of a
    documented command is harmless and passes silently; a write frame, an undocumented command code
    (§14.5), a code outside the table, or the scratchpad flag (§5.1) each names itself and confirms.
    Three cases warn on their own: `CMD_JUMP_APP_TO_BOOTLOADER`, which is a plain *read* frame that
    drops the device off the bus where no other command can reach it; a bootloader command sent in
    application mode; and a raw write of `CMD_VOLTAGE_MV`, which is the one path to the rail that
    interlock 1 does not police.

---

## 14. Open questions — mostly resolved on hardware, 2026-08-21

**Bring-up happened.** A real unit — serial `81a0bcc3`, firmware `APP.05.00.00`, PID `0x800F` — was
attached and driven by this tool for the first time. Ten of the sixteen questions below are now
answered from measurement rather than inference, and they are marked **RESOLVED** with what was
observed. Three of the answers corrected this document; one corrected the code.

The single most important result is not in the list: **the protocol works.** Every frame this
document describes round-tripped on the first attempt — the nibble-encoded MIDI framing, the
big-endian scalars, the HIGH-before-LOW vlimit order, the inverted LED byte, the identity string
layouts, the 90-byte PDO blob and its little-endian header, and the full capability scan including
the serial-latch invariant. No decode had to be corrected after contact with hardware.

What remains open needs either a second unit, a firmware image, or a deliberate bootloader
excursion. The originals are kept below for provenance; nothing has been deleted.

### Answered on hardware

| # | Question | Measured answer |
|---|---|---|
| 1 | USB product ID | **`0x800F`** in application mode. The vendor's own udev rule had this right; it was dismissed here because it appeared nowhere in the app. Its `SUBSYSTEM=="hidraw"` is still wrong. |
| 2 | Advertised MIDI port name | **`Werewolf VFLEX`**; ALSA card id `VFLEX`, node `/dev/snd/midiC2D0`. The `"vflex"` substring match works. Type is Legacy — no UMP. |
| 3 | Descriptor layout | Three interfaces: `1.0` audio control (01/01), `1.1` **MIDIStreaming (01/03)** with **bulk** EP `0x02` OUT / `0x83` IN, 64-byte packets, and `1.2` **vendor class (0xFF)** with bulk EP `0x01`/`0x81`. Both driven by `snd-usb-audio` except 1.2, which is unbound. No interrupt endpoints, no UMP alt setting. |
| 4 | `CMD_FLAG_SCRATCHPAD` (0x40) | **Validate-and-discard.** A scratchpad write of 6000 mV was acknowledged and echoed back (`tx 04 d2 17 70` → `rx 04 12 17 70`), yet `voltage get` still returned 5000. A scratchpad *read* returns the same value as a normal read. The flag makes a write not take effect. |
| 8 | AUTHLOCK read layout | **The vendor client was right.** `tx 02 16` → `rx 04 16 16 00`: a two-byte payload of `[0x16, level]` — the command code echoed a second time, then the level. Reading `payload[1]` was never an off-by-one. Levels beyond 0 remain untested. |
| 11 | ADC calibration | Offset and scale both read **0** on a factory unit, and `CMD_VMEASURE` still returns a sensible calibrated value (raw 437 counts → 5270 mV). Confirms the inference that firmware treats 0 as "use built-in calibration". The formula itself is still device-side and unknown. |
| 13 | Does the device echo the flag bits? | **No — it clears them.** `tx 04 92 13 88` (write flag set) → `rx 04 12 13 88`. Masking the received command byte with `CmdCodeMask` is required, not merely defensive. |
| 14 | Unsolicited frames? | **None.** Twelve seconds idle on a connected unit produced nothing. The device speaks only when spoken to. |
| 15 | Is the 20 ms inter-message delay required? | **No, but zero is not safe.** Failure rates over `info` (7 commands each): 20 ms 0/40, 1 ms 0/120, 100 µs 0/120, **1 ns 3/120 (2.5%)**, each failure a response timeout. Writes at 1 ms: 0/30 failed, 0/30 wrong read-back. So the vendor's 20 ms is roughly 20× more conservative than needed and 1 ms is ~10× faster end to end (`info` 0.38 s → 0.04 s) — but the device does drop frames when pushed at full rate. Measured on one unit, one host; the default stays 20 ms until that is corroborated. |
| — | `SelectedPDOID` semantics (§8) | **1-based USB-PD object position.** A unit targeting 5 V against a charger whose PDO 0 is the 5 V fixed supply reported `selected pdo: 1`. |

### Corrections this produced

- **§6.4's string-length table was wrong.** `CMD_CHIP_UUID` returns **16** bytes, not 8 (`rx 12 09 …`,
  `1732abcd7fc0bcc1`). Harmless in practice — the decoder slices `bytes[2:frame[0]]` — but the
  vendor client's own write guard would have refused a correct 16-character UUID, one more sign that
  path was never exercised. Measured lengths: serial 8, chip uuid 16, hardware id 8
  (`VFLEX...`), firmware version 12 (`APP.05.00.00`), mfg date 8 (`004apr26`).
- **§10.1's bootloader assumption was wrong, and this one mattered.** The vendor-class 0xFF interface
  is present *while the application is running* — it is interface 1.2 above, unbound, alongside MIDI.
  This document assumed it appeared only after the jump. `PickBootloaderInterface` selects by class,
  so it would have chosen that interface on a perfectly healthy unit and `firmware flash --recover`
  would have streamed WRITE_CHUNK frames at a device that never entered the bootloader. The code now
  refuses when a MIDIStreaming interface is present (`bootloader.InApplicationMode`), which is the
  discriminator §10.1 does get right: a unit in the bootloader presents no MIDI interface.
- **The LED table (§1.1) is confirmed at both ends.** Solid green on a PD charger that could supply
  the configured voltage; and configuring 9 V while attached to a host port that cannot supply it
  left the stored value at 9000 mV with `CMD_VMEASURE` still reading 5270 mV — the negotiation-failed
  state.
- **§4.4's permission split is confirmed empirically.** `/dev/snd/midiC2D0` carries a `uaccess` ACL
  (`crw-rw----+`) with no rule installed, so the default rawmidi path needs no udev rule at all;
  `/dev/bus/usb/005/017` is `crw-rw-r--` with no ACL, so `--transport usb` and every firmware
  operation do need one.

### Still open

Questions **5** (commands 4–7, 13, 14), **6** (`CMD_ENCRYPT_MSG`), **7** (`CMD_IOS_HOST_MODE_FLAG`),
**9** (`VTOLERANCE_SAG_PER_MA` units — it reads 0 on a factory unit, so nothing can be inferred about
its scale), **10** (whether the 750 mV tolerance is symmetric — needs a variable load), **12** (the
CRC algorithm — needs a firmware image) and **16** (bootloader re-enumeration — needs a deliberate
excursion) are unchanged. Question 5's probes remain the ones to run last and with nothing attached.

---

The original text follows, unedited.

These are genuinely undetermined, not merely unresearched. Each was chased to exhaustion across the
web bundle, the APK (including a Hermes string-table parse), and the manual.

**Bring-up checklist.** With a device in hand, most of these fall out of one session. Work down the
table; the first four take minutes and unblock the rest.

| # | Question | How to answer it | Blocks |
|---|---|---|---|
| 1 | USB product ID (both modes) | `lsusb -d 37bf:` in app mode, then again in the bootloader | Precise udev rules; PID-based matching |
| 2 | Advertised MIDI port name | `amidi -l`, `aconnect -l` | Replacing the `"vflex"` substring match |
| 3 | Descriptor layout: bulk vs interrupt, UMP alt setting | `lsusb -v -d 37bf:` | Whether `--transport usb` works at all |
| 13 | Does the device echo the write/scratchpad flag bits? | `gflex monitor` while running `gflex -v voltage get` | Whether response matching can tighten |
| 14 | Are there unsolicited frames? | `gflex monitor` on an idle connected device | Whether the RX path needs an async channel |
| 15 | Is the 20 ms inter-message delay required? | Bisect `--byte-delay` down from 20ms to 1ns, watch for timeouts | A large speed-up on every command |
| 8 | AUTHLOCK read layout and lock levels | `gflex raw 02 16`, compare payload[0] vs payload[1] | Implementing a real authlock command |
| 9 | `VTOLERANCE_SAG_PER_MA` units | `gflex raw 02 19` on a factory unit, then vary under load | Exposing tolerance in real units |
| 10 | Is the 750 mV tolerance symmetric? | Set a known voltage, vary load, watch for the red LED | Same |
| 11 | ADC calibration formula | `gflex measure` against a meter, at several voltages | `calibrate adc` being usable rather than raw |
| 12 | Firmware CRC algorithm | Compare `res.CRC` against candidate CRC-8 variants over a known image | Verifying an image offline |
| 4 | `CMD_FLAG_SCRATCHPAD` semantics | `gflex raw` a write with 0x40 set, power-cycle, re-read | Whether volatile writes are possible |
| 5 | Commands 4–7, 13, 14 | Last resort. Probe read-form only, one at a time, nothing attached | Nothing — they are unused |
| 16 | Bootloader re-enumeration details | `udevadm monitor` across a `firmware bootloader` jump | Tightening the flash sequence |

⚠️ Questions 4 and 5 involve sending frames whose effect is unknown. Do that on a unit with
**nothing attached to the X-Connector**, and preferably not your only one.

1. **USB product ID.** Only the vendor ID 0x37BF is used anywhere; the WebUSB filter matches on VID
   alone and no PID literal exists in any artifact. `0x800f` appears **only** in the manual's udev
   rule, which itself hedges ("It typically looks something like this") and targets the wrong
   subsystem. Confirm with `lsusb` on real hardware. Also confirm whether MIDI mode and bootloader
   mode use different PIDs.
2. **The advertised MIDI port name.** Only the substring test is known. Get it from
   `amidi -l` / `aconnect -l`.
3. **USB descriptor layout** — interface numbers, whether endpoints are bulk or interrupt, and
   whether a MIDI 2.0/UMP alt setting is declared. Determines whether `--transport usb` works at all.
   The implementation accepts bulk *or* interrupt endpoints on either class 0x01 or 0xFF with
   subclass 0x03, so it should survive most of the possibilities, but that is untested.
4. **`CMD_FLAG_SCRATCHPAD` (0x40)** — volatile vs committed semantics. Never set by the shipped app.
5. **Commands 4–7 (`RESERVED0..3`), 13 (`FLASH_LED_SEQUENCE_ADVANCED`), 14 (`FLASH_LED`)** — payload
   format, direction and effect all unknown. Enum entries only; no helper, no caller, no parser case,
   nothing in the Android bundle or the manual.
6. **Command 16 (`ENCRYPT_MSG`)** — structurally a challenge/response (host writes n bytes, device
   returns m bytes stored as `secretsecrets`), but there is no client-side crypto, no key, no caller,
   and no server round-trip. Its relationship to AUTHLOCK is **pure speculation**; no function
   references both.
7. **Command 21 (`IOS_HOST_MODE_FLAG`)** — the helper is named `setIosHostMode` yet emits a *read*
   frame with no payload, and the response is explicitly discarded. Contradiction unresolved in the
   source.
8. **Command 22 (`AUTHLOCK`)** — whether the read response really carries two payload bytes (level at
   index 3) or the client has an off-by-one; the set of lock levels; what they gate; how to unlock.
9. **Command 25 (`VTOLERANCE_SAG_PER_MA`)** — units and scale factor. No default, no caller,
   no consumer.
10. **Command 24** — whether 750 mV is a symmetric ± band or one-sided, and how it combines with the
    sag term.
11. **ADC calibration formula** and the fixed-point interpretation of `scale`; why 0 is a valid
    default for a multiplier.
12. **Firmware CRC algorithm** (8-bit, but which polynomial/method).
13. **Whether the device echoes the WRITE/SCRATCHPAD flag bits** in the response command byte — the
    client masks with 0x3F before any comparison, so it never observes them.
14. **Whether the device ever sends unsolicited frames.** The client has an
    `unexpected_frame_while_waiting` path, which implies the possibility, but nothing documents it.
15. **Whether 20 ms per MIDI message is required** or merely defensive.
16. **Bootloader re-enumeration details** — does it appear as a different PID, and does the CDC-style
    `SET_CONTROL_LINE_STATE` control transfer actually matter?

Additionally, the vendor manual is an unfinished draft: three LED descriptions are truncated
mid-sentence (proven unrecoverable from the CMS edit history), 9 of 15 FAQ answers were never
written, and the "Scan Power Supply Capabilities" page is an empty stub. The FAQ's claim that VFLEX
outputs "430 different voltages" could not be derived from any code path — there is no step-size
constant anywhere — so **do not encode 430 as a protocol constant.**

---

## 15. Golden test vectors

Frame → MIDI stream. These seeded the framer's table-driven tests and are now pinned, along with
the rest, in `testdata/golden/frames.json` — which carries the same vectors plus their USB-MIDI
packet form, several response (`rx`) frames, and two malformed frames that must be dropped. The
file is the authority the tests read; every vector below appears in it verbatim.

**Read serial number** — frame `02 08`

```
80 00 00 | 90 00 02 | 90 00 08 | A0 00 00
```

**Set voltage to 12.000 V** — 12000 mV = 0x2EE0, frame `04 92 2E E0`

```
80 00 00 | 90 00 04 | 90 09 02 | 90 02 0E | 90 0E 00 | A0 00 00
                                             ^^^^^^^^ velocity 0 — see §3.2
```

**Set current limit to 5000 mA** — 0x1388, frame `04 93 13 88`

```
80 00 00 | 90 00 04 | 90 09 03 | 90 01 03 | 90 08 08 | A0 00 00
```

**Set vlimit low=3300 high=48000** — frame `06 97 BB 80 0C E4`

```
80 00 00 | 90 00 06 | 90 09 07 | 90 0B 0B | 90 08 00 | 90 00 0C | 90 0E 04 | A0 00 00
```

**LED always-on** — frame `03 8F 00`

```
80 00 00 | 90 00 03 | 90 08 0F | 90 00 00 | A0 00 00
```

**Jump to bootloader** — frame `02 14`

```
80 00 00 | 90 00 02 | 90 01 04 | A0 00 00
```

At the USB level each 3-byte message becomes a 4-byte packet prefixed `08` (SOF), `09` (data) or
`0A` (EOF) — e.g. the set-voltage frame is
`08 80 00 00 | 09 90 00 04 | 09 90 09 02 | 09 90 02 0E | 09 90 0E 00 | 0A A0 00 00`.

---

## 16. Reproducing the analysis

The material this specification was derived from is not distributed with it: it is the vendor's
copyrighted application. Anyone wanting to check a claim here can reconstruct the same corpus from
publicly obtainable inputs.

**Inputs.** The Android package `com.tundralabs.vflex` (version 2.0.0 was used here), the web build
served from `https://vflex.app`, and the vendor manual at
<https://werewolf.us/a/pages/vflex-user-manual>.

**The web build is the primary source** and by far the easiest to read: it is the same application
compiled by Metro for the browser, so it is minified JavaScript rather than the Hermes bytecode
shipped in the APK. Fetch the bundle referenced by the page's `<script src>`, then split it into
addressable modules — it is a Metro bundle, so every module is a `__d(function(g,r,i,a,m,e,d){…},
<id>, <deps>)` call, and the numeric ids in the dependency map are what the citations throughout
this document refer to.

To read one legibly, break it at statement boundaries:

```bash
python3 -c "import re,sys;s=open(sys.argv[1]).read();print(re.sub(r'(?<=[,;{])','\n',s))" 2436.js
```

**The modules cited most often:**

| Module | Contents |
|---|---|
| 2437 | `VFLEX_COMMANDS` — the command table |
| 2436 | `VFlexProtocol` — framing and response parsing |
| 2435 | `VFlexApi` — session, mutex, RX decoder, PDO download |
| 2801 | `VFlexMidiTransport` — the nibble encoder and port discovery |
| 2812 | all constants, including `TUNDRA_VENDOR_ID` |
| 2811 | `WebUsbTransport` — the bootloader USB path |
| 3029, 3008 | PDO decode and scan constants |
| 3165–3172 | the firmware update pipeline |
| 2813–2822, 2433 | the hardware state machine |

**The APK** is worth unpacking mainly to confirm the two builds agree. Its JS is Hermes bytecode
(`assets/index.android.bundle`); the string table parses out with a small reader for the HBC header,
which is enough to cross-check identifiers, and `strings` over `classes*.dex` confirms the native
MIDI plumbing. The Android build adds nothing to the protocol — it is the same JS — and it lacks
firmware update entirely.

**The manual** pages embed their content as a Notion `recordMap` inside `<script id="__NEXT_DATA__">`,
so a naive text scrape loses the FAQ answers and the LED descriptions; parse the JSON instead.

---

## 17. Where the implementation deliberately differs from this spec

This document describes **what the vendor's application does**. In several places the shipped Go
implementation deliberately does something else, because the vendor's behaviour is a defect, a
limitation, or a fit for a long-lived phone app rather than a one-shot CLI.

They are listed here so that a future reader comparing code against spec does not "correct" the code
back into a bug. Each was found during code review and carries a regression test.

| § | Vendor / spec behaviour | Implementation | Why |
|---|---|---|---|
| 6.5, 13.1 | Voltage limits unreadable → fall back to the hardware envelope | **Refuse**, unless `--ignore-device-limits` | The protocol has no NACK, so a dropped frame is routine. A fallback lets one lost frame downgrade a 5 V ceiling to 48 V. |
| 6.5 | The vendor's `VLimitPlausible` (`low≥3000, high≥6000`) | Used **only** for the post-flash "was this erased?" test; trust uses `high > low` | Its 6000 mV floor would discard every window with a ceiling under 6 V — the most protective setting a user can choose. |
| 9.4 | SPR AVS: consumers take `max()` of the two band limits | Band-aware `PDO.CurrentAt(v)` | The two limits are per-band (9–15 V, 15–20 V). `max()` reports the strong band's current while operating in the weak one — over-reporting, the direction that destroys hardware. |
| 9.4 | Only EPR AVS current is clamped to a cable's 5 A rating | **Every** reported current is clamped, centrally | A malformed fixed PDO decodes to 10.23 A. The raw value survives as `DeclaredMaxCurrentA`; only the verdict is conservative. |
| 9.4 | Battery and Variable PDOs are silently discarded | Decoded, classified and displayed | They are real capability the user paid for; hiding them helps nobody. |
| 9.5 | Candidates partitioned by requested voltage (`if V > 20`) | Any object whose decoded range covers the request is considered | An EPR AVS range routinely starts at 15 V. Partitioning hid it, so an 18 V request against a 140 W charger was answered "not achievable" — from a log that plainly covered it. |
| 9.5 | Compatibility judged against a cloud record; `.enabled` ignored | Judged against the actual scan | The scan is ground truth and cannot disagree with the hardware in front of the user. Also sidesteps the vendor's `.amps`/`.enabled` inconsistency. |
| 9.1 | A short PDO chunk is appended as-is | Rejected and retried | The blob is positionally decoded little-endian: appending a short chunk does not look corrupt, it looks like *a different power supply*. |
| 3.3 | Malformed frames dropped silently; the command just times out | Reported through a drop hook, surfaced by `gflex monitor` | The spec itself says "Log it." It is also the evidence that would settle §14.13 and §14.14. |
| 7 | 500 ms + 800 ms settle, then a ~25 s fixed backoff chain | No fixed settle; adaptive backoff against a ~10 s budget | The settle suits a long-lived app session; 1.3 s on every CLI invocation does not. The *persistence* is kept — without it a freshly plugged unit fails. A ready device pays nothing. |
| 7 | `ensureCurrentLimitMa(5000)` on every connect | Not done | A CLI that silently rewrites your current limit during `voltage get` would be a bug, not parity. |
| 5.2 | Send failures swallowed; surface as a 5 s timeout | Returned directly | "Timed out" for a failed write sends the user hunting the wrong problem. |
| 8 | `device_data` never cleared on disconnect | Cache invalidated | Latent staleness bug; not worth reproducing. |
| 6.4 | Identity strings UTF-8-decoded over the whole frame, then sliced by *character* | `bytes[2:len]`, sanitised | The vendor's decode misaligns on any byte ≥ 0x80. |
| 6.5 | No range validation of any kind | Full interlocks (§13) | The vendor writes whatever 16-bit value it is given. Ours is the only guard. |

### Resolved since this document was written

- **§9.4's SPR AVS field-order caveat is settled.** The 15 V/20 V assignment *does* match the
  published USB-PD 3.2 layout — bits 19:10 bound the 15–20 V band, bits 9:0 the 9–15 V band. The
  "may be swapped" warning no longer applies; the band-aware decode depends on this ordering.
- **§10.2's dead constants confirmed dead.** `BOOTLOADER_PREAMBLE_LEN = 4` and
  `MAX_VERIFY_PACKET_DATA = 60` are referenced nowhere in the vendor client. The real bootloader
  preamble is 2 bytes, as the frame table shows.
