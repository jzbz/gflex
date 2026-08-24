# gflex

[![CI](https://github.com/jzbz/gflex/actions/workflows/ci.yml/badge.svg)](https://github.com/jzbz/gflex/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Program a [Werewolf VFLEX](https://werewolf.us/) from Linux, without the proprietary phone app.

The VFLEX is a programmable USB-C Power Delivery adapter: you tell it what voltage you want, it
stores that in non-volatile memory, and from then on it negotiates that voltage from any PD source
you plug it into and presents it on its X-Connector. It's aimed at guitar pedals and other
small DC loads that want an odd voltage from a modern USB-C charger.

The vendor ships an Android app, an iOS app, and a web app that only works in Chrome and Edge.
There is no Linux client. This is one — a single static binary, no cgo, no browser, no account.

```
gflex info
gflex voltage set 9V
gflex scan
gflex firmware flash firmware.json
```

> **Unofficial and independent.** Not affiliated with, endorsed by, or supported by Werewolf or
> Tundra Labs. No vendor code is used here — see [Provenance](#provenance) below, and
> [SPEC.md](SPEC.md) for the complete protocol documentation plus an honest list of what is still
> unknown.

**Contents:** [How it works](#how-it-works) · [Install](#install) · [Usage](#usage)
· [Commands](#command-reference) · [Safety](#safety) · [Troubleshooting](#troubleshooting)
· [Provenance](#provenance) · [Status](#status-and-limitations) · [Development](#development)

---

## How it works

### The device speaks MIDI

This is the surprising part, and it's worth understanding before you read any of the code.

The VFLEX enumerates as a **class-compliant USB-MIDI device**. That's a clever choice: every desktop
and mobile OS already has a driver for it, so the vendor needed no kernel driver, no signed Windows
INF, and no special permissions — and in a browser it's reachable through the Web MIDI API, which is
why the web app requires Chrome or Edge.

But MIDI data bytes are only 7 bits, and the protocol needs to send arbitrary 8-bit values. So each
protocol byte is **split into two nibbles and carried in a single MIDI Note-On message** — the high
nibble as the note number, the low nibble as the velocity. A frame is bracketed by two more MIDI
messages acting as delimiters:

```
0x80 0x00 0x00            Note Off              start of frame
0x90 (b>>4)&0xF  b&0xF    Note On               one message per protocol byte
      …
0xA0 0x00 0x00            Poly Aftertouch       end of frame
```

Everything on the wire is therefore well-formed, in-range MIDI. That has a very practical
consequence for us: the Linux kernel's `snd-usb-audio` driver passes it through untouched, so this
tool can just write bytes to `/dev/snd/midiC*D*` with no driver detaching, no root, and no cgo.

> ⚠️ One hazard falls out of this encoding. Any protocol byte whose low nibble is zero becomes
> `0x90 <hi> 0x00` — a Note-On with velocity 0, which MIDI convention treats as a Note-Off. Any
> MIDI layer that "helpfully" canonicalises that would corrupt roughly **one byte in sixteen**, and
> the corruption would look exactly like a start-of-frame marker. We verified the kernel and
> alsa-lib don't do this, which is a large part of why this tool talks to rawmidi directly instead
> of going through a MIDI library or the ALSA sequencer.

### Inside the frames

Underneath the MIDI wrapping is a small request/response protocol:

```
byte[0]  total frame length (2 + payload length)
byte[1]  command code | flags        0x80 = write, 0x3F = code mask
byte[2:] payload                     big-endian throughout
```

There are 29 commands. Reading the output voltage is `02 12`; setting it to 12 V is
`04 92 2E E0`. The device answers with a frame carrying the same command code. There is no NACK —
a command either gets a matching response or it times out — and only one command may be in flight at
a time, so all device access is serialised through a single mutex.

A few encodings are worth knowing because they're easy to get backwards, and each is commented at
the point of use:

- **The LED setting is inverted.** The command is `DISABLE_LED_DURING_OPERATION`, so the wire byte
  `0` means the user-facing "LED Always On" is *enabled*.
- **Voltage limits go HIGH before LOW** on the wire, in both directions — the opposite of the
  vendor's own API argument order.
- **ADC calibration values are signed** 32-bit, not unsigned.
- **The PDO log is little-endian**, the only little-endian data in the entire system.
- **A voltage read of 0 mV means "not ready"**, not zero volts. Retry it.

### Two transports

```
                      ┌─────────────────────────────────────────┐
   application mode   │  /dev/snd/midiC*D*   (ALSA rawmidi)     │  default
                      │  or direct USB-MIDI endpoints (usbfs)   │  --transport usb
                      └─────────────────────────────────────────┘
                                        │
                         CMD_JUMP_APP_TO_BOOTLOADER
                                        ▼
                      ┌─────────────────────────────────────────┐
   bootloader mode    │  vendor-class (0xFF) bulk USB via usbfs │
                      └─────────────────────────────────────────┘
```

Normally the tool uses **ALSA rawmidi**, which needs no special permissions on a systemd desktop and
doesn't disturb anything else using your sound hardware.

If something else has already claimed the MIDI port — Chrome running the vendor's own web app,
PipeWire, JACK, a DAW — the open fails with `EBUSY`, and `--transport usb` bypasses ALSA entirely by
talking to the device's USB endpoints and building the 4-byte USB-MIDI event packets by hand. That
path detaches `snd-usb-audio` for the duration and asks the kernel to rebind it on exit — but on the
one host and kernel this was measured on, the MIDI node did not come back until the device was
physically replugged, so treat `--transport usb` as a deliberate trade rather than a free fallback.
See [Troubleshooting](#troubleshooting).

Firmware update is a third case: in bootloader mode the device stops being a MIDI device altogether
and exposes a vendor-class interface, so there's no MIDI framing at all — the same frames go out as
raw bulk transfers.

All three are pure Go. rawmidi is an ordinary file under `/dev/snd`; the two USB paths go through
Linux `usbfs` ioctls rather than libusb. That is what keeps the whole thing in one static, cgo-free
binary — firmware update included, with no build tag to remember.

### Code layout

```
cmd/gflex/                     entry point
internal/proto/                command table, frame codec, device model — the shared contract
internal/framer/               nibble MIDI encoder + status-byte-driven receive parser
internal/session/              typed accessors, single-flight mutex, ACK matching, timeouts
internal/transport/rawmidi/    ALSA rawmidi (default), sysfs device discovery by vendor ID
internal/transport/usbmidi/    direct USB-MIDI fallback
internal/transport/fake/       in-memory device for tests
internal/usbfs/                pure-Go Linux usbfs binding
internal/pdo/                  PDO log decoding and power-supply compatibility evaluation
internal/bootloader/           firmware flashing, minimal WebSocket client
internal/cli/                  cobra command tree, output formatting, safety interlocks
```

The layering is deliberate: the protocol lives in exactly one place, and each transport is a thin
byte-level implementation that knows nothing about VFLEX commands. That's also what makes the whole
protocol testable without hardware — `internal/transport/fake` is a scripted in-memory device, and
`testdata/golden/frames.json` pins the frame ↔ MIDI encoding so it can't silently drift.

---

## Install

However you get it, gflex is one static, cgo-free binary with no runtime dependencies: no shared
libraries, no Go toolchain, no browser, nothing to install beside it. Whichever route you take,
finish with [Permissions](#permissions) below — the USB path and every firmware operation need a
udev rule.

### Download a binary

Every release attaches Linux binaries for **amd64** and **arm64**, built and checked on the
architecture they target, plus a `SHA256SUMS` file listing both and a `SHA256SUMS.asc` signing that
list: [github.com/jzbz/gflex/releases](https://github.com/jzbz/gflex/releases).

`uname -m` says which one you want — `x86_64` is amd64, `aarch64` is arm64. Download it and both
checksum files alongside it, then:

```bash
gpg --verify SHA256SUMS.asc SHA256SUMS && sha256sum --ignore-missing -c SHA256SUMS
install -m 0755 gflex-*-linux-amd64 ~/.local/bin/gflex
gflex version
```

(`--ignore-missing` because the file lists both architectures and you have one of them.) What
`gpg --verify` has to say for that to mean anything is [below](#verifying-a-download).

### Verifying a download

Fetch the signing key once, from GitHub:

```bash
curl -sL https://github.com/jzbz.gpg | gpg --import
```

or from a keyserver, which is the better of the two — it does not come from the same host as the
release:

```bash
gpg --locate-keys jz@jz.bz
```

Either way it is the fingerprint that is trusted, not where the key came from. `gpg --verify` has to
report a *Good signature* from `252B 901C 8885 3CF9 F939  2559 2497 38C8 641C 3359`; any other key,
or none at all, and the checksums it vouches for mean nothing.

A freshly imported key also draws *"WARNING: This key is not certified with a trusted signature"*.
That is expected and is not a failed check: it says the key has no web-of-trust path from anything
you already trust, which a key you just fetched never does. The signature is still good. Compare the
fingerprint gpg prints against the one above and move on, or sign the key locally
(`gpg --lsign-key jz@jz.bz`) to silence it on later releases.

The order is the whole point. `SHA256SUMS` ships from the same release as the binaries it describes,
so on its own it catches a truncated download and nothing else — anyone who could replace a binary
could replace the list beside it just as easily. The signature is what turns it into a check, and it
is made by hand after the release workflow has published: the signing key never goes near CI, so a
compromised workflow can publish a binary but cannot sign for one.

### Install with Go

```bash
CGO_ENABLED=0 go install github.com/jzbz/gflex/cmd/gflex@latest
```

Requires Go 1.26+. `CGO_ENABLED=0` is what makes the result static: gflex imports `net` to reach the
vendor's firmware service, and a cgo-enabled build links that against the system resolver.

Every package here lives under `internal/`, so the module exposes no importable Go API — the version
number promises nothing but the CLI.

### Build from source

```bash
git clone https://github.com/jzbz/gflex
cd gflex
CGO_ENABLED=0 go build -o ~/.local/bin/gflex ./cmd/gflex
```

### Permissions

On a systemd desktop, the ALSA path usually works with no setup: `/usr/lib/udev/rules.d/70-uaccess.rules`
already grants your seat user an ACL on `/dev/snd/*`.

The USB path (`--transport usb`, and all firmware operations) needs a udev rule:

```bash
sudo gflex install-udev
```

That writes `/etc/udev/rules.d/70-gflex.rules` and reloads udev. The write is atomic — a temporary
file in the same directory, then a rename — so an interrupted install can never leave a half-written
rule behind. If the file is already there and you have edited it, `install-udev` shows you and asks
before overwriting rather than discarding your changes silently; `--yes` answers that in a script,
and with no terminal to ask on it refuses instead.

`gflex install-udev --print` writes the rule to stdout and needs no root, so you can read it first,
or install it yourself with `gflex install-udev --print | sudo tee /etc/udev/rules.d/70-gflex.rules`.
If you'd rather do it by hand:

```
SUBSYSTEM=="usb", ATTR{idVendor}=="37bf", MODE="0660", TAG+="uaccess"
```

> The udev rule published in the vendor's own manual does **not** work: it matches
> `SUBSYSTEM=="hidraw"`, and the VFLEX is not an HID device. It also names a product ID that appears
> nowhere in the vendor's own software. See [SPEC.md §4.4](SPEC.md).

---

## Usage

### Look at the device

```bash
gflex devices          # what's connected, and how it was found
gflex info             # identity and current settings
gflex info --all       # also read the fields the vendor app never touches
gflex info --json      # stable machine-readable output
```

### Set the output voltage

```bash
gflex voltage get
gflex voltage set 9V           # also accepts 9, 9.0, 9000mV
```

The setting is stored in non-volatile memory. Unplug the VFLEX from your computer, plug it into a
PD charger, and it will negotiate 9 V and light green.

Above 5 V you'll be asked to confirm (`--yes` to skip, required when stdin isn't a terminal).
Above 20 V you'll also be warned that you need an EPR-capable source and an eMarker-equipped 5 A
cable — without one, the device fast-blinks red.

### Other settings

```bash
gflex current set 3A                        # negotiated current limit, max 5 A
gflex vlimit set --low 3.3V --high 24V      # clamp the range voltage set will accept
gflex led set off                           # LED off while in the green "power good" state
gflex measure                               # device's own measurement of its output
```

### Scan a power supply

The VFLEX can capture what a charger actually offers. It's a physical dance, because the device has
to be plugged into the charger — not your computer — while it happens, so `gflex scan` walks you
through it:

```bash
gflex scan --voltage 9 --current 2
```

1. Erases the capture log and records the serial number
2. You unplug the VFLEX and plug it into the charger under test
3. The LED goes green or red when the negotiation completes
4. You plug it back into your computer; the serial is re-checked to make sure it's the same unit
5. The log is downloaded and decoded

You get the charger's full advertised capability — fixed voltages, PPS and AVS ranges, SPR and EPR
sections. Pass `--voltage` and `--current` together (both, deliberately: a voltage that's reachable
at 100 mA but not at 3 A is a different answer) and you also get a verdict on whether your target is
achievable, in which PD mode, and at what current.

For scripting, `--no-prompt` waits for the unplug/replug instead of asking, with `--wait` and
`--settle` controlling the timings.

Requires firmware 5.0.0 or newer.

```bash
gflex pdo dump --json     # re-decode the stored log without rescanning
```

Two small improvements over the vendor app here: it decodes Battery and Variable PDOs (the vendor
app silently ignores both), and it evaluates compatibility against the actual scan rather than
against a cloud record.

### Firmware update

```bash
gflex firmware version
gflex firmware flash firmware.json --yes
gflex firmware flash --fetch --yes             # pull the image from the vendor's server
gflex firmware flash firmware.json --recover   # unit already stuck in the bootloader
```

| Flag | Meaning |
|---|---|
| `--fetch` | Pull the image for this unit's serial from the vendor service |
| `--ws-url` | Override that service's endpoint. It must be TLS (`wss://`); a `ws://` URL is refused, because the image and the CRC it is checked against arrive in the same document. If the endpoint really is cleartext, say so with `ws+insecure://` |
| `--fetch-timeout` | Budget for the whole download (default 15 s). `--timeout` bounds MIDI commands, not this |
| `--recover` | Skip the jump; talk straight to a unit already in the bootloader |
| `--crc <byte>` | Supply the expected CRC for an image that carries none (a raw `.bin`) |
| `--force` | Flash an unverifiable image anyway — see below |
| `--ack-mode` | Stream with acknowledgements from the start: slower, more robust |
| `--page-size <n>` | Page size used to split a raw `.bin` (default 512). A JSON image and `--fetch` carry their own page split, so it cannot be combined with either |

The sequence is: jump to the bootloader over MIDI → wait for re-enumeration → reconnect over the
vendor-class USB interface → confirm the serial matches → stream the image page by page → verify the
device's CRC → jump back to the application → restore the settings the flash erased.

**If the CRC doesn't match, the tool stops and does not jump back.** That leaves the device sitting
in the bootloader with a slow-blinking white LED, which is recoverable — run the flash again with
`--recover`. It is not bricked.

An image that carries a CRC is *always* verified; `--force` cannot suppress that, and it cannot
override a mismatch. `--force` exists for the one case where verification is impossible — a raw
`.bin`, which has no CRC to check against — and it warns loudly about what is being skipped. If you
know the expected value, `--crc` is the better answer. Page geometry is validated before the first
byte is written, so a mis-split image is refused rather than half-flashed: the CRC would not catch
that anyway, since the device computes it over whatever it was told to write. If your part's flash
page is not the 512 bytes assumed for a raw `.bin`, that refusal is what `--page-size` is for.

### Debugging

```bash
gflex monitor                    # live decoded frames, as they arrive
gflex voltage set 12V --dry-run  # print the frame and MIDI bytes, send nothing
gflex raw 02 12                  # send an arbitrary frame, print the response
gflex -v info                    # hex trace of every frame, both directions
```

`monitor` shows the **receive** direction only: the ALSA rawmidi node doesn't loop back what
another process writes, so to watch a full exchange run `monitor` in one terminal and the command
you care about with `-v` in another, which traces the session's own frames as well as the replies.

`gflex monitor` and `gflex raw` exist mainly to help resolve the open questions in
[SPEC.md §14](SPEC.md) against real hardware.

---

## Command reference

| Command | What it does |
|---|---|
| `gflex devices` | List candidate MIDI ports and USB devices, and how each was found |
| `gflex info [--all]` | Identity and settings. `--all` adds the fields the vendor app never reads |
| `gflex voltage get \| set <v>` | Read or set the stored output voltage |
| `gflex current get \| set <a>` | Read or set the negotiated current limit (never a measurement) |
| `gflex vlimit get \| set` | Read or set the window `voltage set` is bounded by |
| `gflex tolerance get \| set` | Read or set the out-of-tolerance thresholds |
| `gflex measure` | The device's own measurement of its output |
| `gflex calibrate get \| adc` | Read or write the ADC calibration |
| `gflex led get \| set on\|off` | The "LED Always On" setting |
| `gflex authlock get \| set` | Read or set the auth lock (effect undocumented — see SPEC §14) |
| `gflex scan` | Guided capture of a power source's PD capabilities |
| `gflex pdo dump \| clear` | Re-decode or erase the stored capability log |
| `gflex version` | Print gflex's own version, commit and platform |
| `gflex firmware version \| fetch \| bootloader \| flash` | Firmware version, download this unit's image without flashing, enter the bootloader, flash an image |
| `gflex raw <hex...>` | Send a frame verbatim — the escape hatch |
| `gflex monitor` | Print inbound decoded frames live |
| `gflex install-udev [--print]` | Install (or show) the udev rule |

Global flags: `--port`, `--transport rawmidi|usb`, `--json`, `--timeout`, `--byte-delay`,
`-v/--verbose`, `--dry-run`, `-y/--yes`. The first six are also readable from the environment —
`GFLEX_PORT`, `GFLEX_TRANSPORT`, `GFLEX_JSON`, `GFLEX_TIMEOUT`, `GFLEX_BYTE_DELAY`,
`GFLEX_VERBOSE` — with flag beating env beating default. `--dry-run` and `--yes` have no
environment counterpart on purpose: both decide whether the device gets written to, and a
`GFLEX_YES` left in a shell profile would pre-answer every safety confirmation for months.

`--transport usb` has a cost worth knowing before you reach for it. It detaches `snd-usb-audio` from
the device's MIDI interface for the duration, and on at least one host and kernel the ALSA MIDI node
did not come back afterwards — `/dev/snd/midiC*D*` stayed gone until the device was physically
replugged, even though the kernel accepted the request to rebind. Use it when rawmidi genuinely
cannot be had; if the node is merely busy, closing whatever holds it (often a Chrome tab running the
vendor's web app — see [Troubleshooting](#troubleshooting)) is the cheaper answer.

`--byte-delay` paces successive messages, and defaults to **1 ms** rather than the 20 ms the
vendor's app uses — the measurement behind that is in
[Status and limitations](#status-and-limitations). `--byte-delay 0` is refused: zero pacing dropped
frames on both units measured, so it is not an option the flag will accept.

There is no global `--force`. `--force` means one specific thing, on one command:
`firmware flash --force` flashes an image that carries no CRC. A persistent flag of the same name
would shadow that on the most dangerous command in the tool.

### Exit codes

Stable, so scripts can branch on them.

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Generic failure |
| 2 | Usage error — bad flag, unknown subcommand, bad value |
| 3 | No device found, or it disappeared mid-operation |
| 4 | Device busy — another process holds the port |
| 5 | Timed out waiting for the device |
| 6 | Permission denied — usually a missing udev rule |
| 7 | Refused by a safety interlock |

---

## Safety

This device drives a power rail into someone's equipment, and the vendor's own app performs **no
range validation whatsoever** — it will happily write any 16-bit value. This tool won't:

- `voltage set` reads the device's limit window first and refuses anything outside it, and outside
  the documented 3.3 V – 48 V envelope. If that window can't be read, or the unit reports one that
  can't bound anything, the write is **refused** rather than falling back to the hardware envelope —
  the window is the limit you chose for your own load, and the protocol has no NACK, so a single
  dropped frame must not silently downgrade a 5 V ceiling to 48 V. `--ignore-device-limits`
  overrides that deliberately; `--yes` does not
- Anything above 5 V needs confirmation; anything above 20 V warns about EPR and cable requirements
- Widening the limit window needs `--yes`; narrowing it doesn't
- `calibrate adc` needs `--yes`, prints the previous values, and tells you how to restore them —
  bad calibration makes every later voltage reading silently wrong
- `authlock set` confirms every write, and a non-zero level has to be confirmed against a warning
  that its effect is undocumented and may not be reversible — nobody knows what those levels gate
- On a non-TTY, every one of these fails closed unless `--yes` is passed, so a script can't quietly
  over-volt a load
- `gflex raw` is guarded too: a read of a documented command goes straight through, but a write
  frame, an undocumented command code, the scratchpad flag, or `02 14` (a plain *read* that drops
  the device into the bootloader) each says what it is and asks first

`--dry-run` prints exactly what would go on the wire and sends nothing. It's refused only where the
frame can't be known without first reading the device (`firmware flash --recover`/`--fetch`, and
`vlimit set`/`calibrate adc` given only half of a pair).

A bare number in `voltage set` / `current set` is always volts or amps, never guessed from
magnitude — `voltage set 12000` is refused rather than quietly read as 12000 mV, because
magnitude-guessing would turn a typo into a 1000× over-volt.

A freshly plugged VFLEX answers `0 mV` for a moment, meaning "not ready" rather than zero volts, so
voltage reads retry with escalating backoff against a ~10 s budget. A device that's ready pays
nothing for this — the first read succeeds and there's no delay at all.

A mistyped subcommand is a hard usage error (exit 2), never a silent success — `gflex voltage sett
12` must not exit 0 having written nothing, or a script guarded with `|| exit 1` sails past the typo
and attaches a load to whatever voltage was already there.

---

## Troubleshooting

**"Nothing found"** (exit 3). `gflex devices` shows what was searched and what turned up. In order of
likelihood: the udev rule isn't installed (`sudo gflex install-udev`, then replug); the device is in
bootloader mode from an interrupted flash (slow-blinking white LED — use `gflex firmware flash
--recover`); or it genuinely isn't plugged in. Note the tool matches on USB vendor `0x37bf`, so a
device that enumerates but isn't a VFLEX won't be picked up unless it's your only MIDI port.

**"Device busy"** (exit 4). Something else holds the ALSA rawmidi node, which is exclusive per
direction. **Check the browser first.** The vendor ships a functionally identical web app at
`https://vflex.app` that drives the device over Web MIDI in Chrome, so the most likely holder is a
tab you left open — and it is the likeliest of all if you were comparing this tool against the
vendor's. Chrome reaches the device through the ALSA sequencer, and the sequencer's kernel module is
what opens the node on its behalf, so don't go looking for the browser among the processes holding
`/dev/snd/midiC*D*`. Read `/proc/asound/seq/clients` instead: the `Werewolf VFLEX` port lists what
it is connected to and connected from, one entry per direction, and that is the real holder.
PipeWire, JACK and a DAW are the other candidates.

Closing the other client is the fix. `--transport usb` also works — it bypasses ALSA entirely — but
it is not free: on at least one host and kernel a single run of it left the device with no
`/dev/snd/midiC*D*` node at all until it was physically unplugged and plugged back in, because
reattaching `snd-usb-audio` to the MIDI interface alone does not recreate the node
([SPEC.md §4.2](SPEC.md#42-fallback-and-bootloader-direct-usb-via-usbfs)). Prefer closing the tab.

**"Permission denied"** (exit 6). `sudo gflex install-udev`, then unplug and replug. On a
non-systemd or headless system the `uaccess` tag does nothing, so uncomment the `GROUP=` fallback in
the rule and add yourself to that group.

**Timeouts** (exit 5). The protocol has no NACK, so any lost frame becomes a timeout. A single one
is unremarkable; repeated ones are worth investigating with `gflex monitor` in one terminal and the
failing command with `-v` in another. If a freshly plugged unit times out, that's expected briefly —
voltage reads already retry for ~10 s. Pacing is worth ruling out on hardware unlike what has been
measured: the 1 ms default was verified on two units of one firmware revision on one host, so
`--byte-delay 20ms` puts back the vendor's conservative pacing if a different unit or a different
USB controller proves flakier.

**The voltage didn't change.** Check `gflex voltage get`, then the LED. Solid green means it
negotiated and is in tolerance; slow-blinking red means the *source* can't supply what you asked for
— run `gflex scan` against that charger to see what it actually offers.

**A scan says "not achievable" for a voltage you believe is supported.** Run `gflex pdo dump` and
read the decoded table. Fixed PDOs must match exactly; PPS and AVS ranges are inclusive. If the log
records an EPR cable failure, the scan may not have seen the source's real capability — refit an
eMarker-equipped 5 A cable and rescan.

---

## Provenance

This is an independent implementation, built by reverse-engineering the vendor's own shipped
application. It contains **no vendor code**.

The process was **clean-room structured**, in two gated phases: the vendor's app was analysed and its
observable protocol written up as [SPEC.md](SPEC.md), with a citation for every claim; the Go code
was then written against that specification rather than by transliterating the original.

Two honest qualifications, because the term gets used loosely:

- It is **not a formal clean-room**. That requires personnel separation — a second team, provably
  never exposed to the original, implementing from the specification alone. Here the same author
  directed both phases.
- What the process *does* establish is that no vendor code was copied. The vendor ships minified
  JavaScript and Hermes bytecode; this is Go, written from a written description of behaviour. What
  was extracted are interface facts — that command 18 carries a big-endian millivolt value, that the
  LED byte is inverted — not creative expression.

Nothing here derives from vendor source, licensed documentation, or confidential material. The
manual cited is public; the APK was obtained as a shipped artifact. Reverse-engineering for
interoperability is protected in many jurisdictions (EU Directive 2009/24/EC Art. 6, US DMCA
§1201(f)), but that's general context rather than legal advice.

Where the implementation deliberately departs from what the vendor's app does — and it does, in
twenty places, mostly because the vendor's behaviour is unsafe — those are catalogued in
[SPEC.md §17](SPEC.md#17-where-the-implementation-deliberately-differs-from-this-spec).

---

## Status and limitations

**Verified on hardware, 2026-08-21.** Two real units — serials `81a0bcc3` and `58b4f621`, both
firmware `APP.05.00.00`, PID `0x800F` — have been driven by this tool. On the first, the protocol
worked on the first attempt: the nibble-encoded MIDI framing, the big-endian scalars, the identity
strings, the inverted LED byte, the HIGH-before-LOW voltage limits, and the full PD capability scan
including its serial-latch invariant. No decode had to be corrected afterwards. The second was
brought up to settle the pacing question below, and matched the first on everything re-measured.

Ten of the sixteen questions in [SPEC.md §14](SPEC.md#14-open-questions--mostly-resolved-on-hardware-2026-08-21)
are now answered from measurement — plus one the original list did not contain — and three of the
answers corrected the documentation. Highlights:

- The **USB product ID is `0x800F`**, and the vendor's own udev rule had it right all along.
- The device **clears the write/scratchpad flag bits** in its echo, so masking on receive is
  required rather than merely defensive. Confirmed on both units.
- The **scratchpad flag makes a write validate-and-discard** — acknowledged, never committed —
  confirmed on both units and on two different commands. What the *response* carries turned out to
  be per-command rather than a property of the flag: on one unit, minutes apart, a scratchpad write
  of `CMD_VOLTAGE_MV` answered with the value sent while one of `CMD_CURRENT_LIMIT_MA` answered with
  the value the device kept, and neither was stored. The remaining 27 commands are uncharacterised.
- **The inter-message delay now defaults to 1 ms, down from the vendor's 20 ms.** The second unit
  reproduced the first unit's pacing measurements, which is exactly what
  [SPEC.md §14](SPEC.md#14-open-questions--mostly-resolved-on-hardware-2026-08-21) question 15 named
  as the precondition for changing the default. 1 ms was clean on both: 120 trials each, 240 in
  total, 0 failures, and every trial is six command round trips. End to end, `info` goes from 0.38 s
  to 0.04 s on unit 1 and from 0.391 s to 0.045 s on unit 2. Going below 1 ms buys nothing
  measurable — 100 µs timed 0.043 s against 1 ms's 0.045 s, a 4% difference that is inside the
  noise, because at 1 ms the wall time is already the device's own turnaround rather than our
  pacing. What lowering it does buy is risk: at 1 ns the two units lost 2.5% and 3.3% of commands,
  which is why `--byte-delay 0` is still refused outright.
- The **device sends nothing unsolicited** — a 12 s idle capture on the second unit recorded 0
  frames, matching the first and corroborating §14 question 14 as well.
- The **vendor-class interface is present while the application is running**, which the spec had not
  anticipated. `firmware flash --recover` now refuses a unit that still presents a MIDI interface.

The vendor firmware service now works end to end. `gflex firmware fetch` downloads the image the
service holds for a unit and reports it without touching the device — and doing that for the first
time found that `flash --fetch` had never worked: the real payload is an array of `{pg_id, chunks}`
objects with the chunks keyed by index, and the parser read a non-array first element as a flat byte
list, so it failed with *"cannot unmarshal object into Go value of type json.Number"*. The measured
shape is now in [SPEC.md §10.3](SPEC.md#103-firmware-image-delivery), along with the real page
geometry: 320-byte pages of 8×40, not the 512 assumed for a raw `.bin`. Note also that the service
returned `APP.05.00.00` for a unit already running it — it serves the current image for a serial, so
a fetch is not by itself an update.

**Firmware flashing is verified end to end.** Unit `58b4f621` was flashed on 2026-08-21 with the
image the vendor service holds for it: the jump to the bootloader, the re-enumeration, the
vendor-class interface opening inside the 8 s retry window, the serial matching across the
excursion, 165 pages of 320 bytes streamed in acknowledged mode, the device's own CRC coming back
`0x30` as the image declared, the jump back to the application, and every setting replayed. A field
by field comparison before and after found no drift at all.

That also answers [SPEC.md §14](SPEC.md#14-open-questions--mostly-resolved-on-hardware-2026-08-21)
question 16, the last one a single unit could settle, and it independently validates the payload
parsing above: the CRC is computed by the device over what it was actually given, so an image
assembled wrongly from the vendor's chunk map would not have matched.

Note what was flashed was the same version already installed — the service serves a unit's current
image, so this exercised the path rather than delivering an update.

Still unverified: the auth-lock levels beyond 0,
the tolerance-sag units, and the six commands that are dead code in the vendor's app. Those, and the
CRC algorithm, are the remaining §14 entries.

**One known limitation, found the same day: `--transport usb` can cost you the ALSA MIDI port.**
After one `gflex --transport usb info`, `/dev/snd/midiC2D*` disappeared and did not return until the
device was physically replugged; sysfs showed the MIDIStreaming interface with no driver bound while
the audio-control interface next to it stayed bound to `snd-usb-audio`. The reattach is not silently
failing — the `USBDEVFS_CONNECT` ioctl returns success, because it means the kernel accepted a
re-probe request, not that a driver bound. `snd-usb-audio` appears to bind the MIDI interface only
while probing the whole audio function, so re-probing that interface on its own does not recreate
the node. Measured on one host and one kernel, so the mechanism is inferred rather than established;
the effect on the user is not. Details in
[SPEC.md §4.2](SPEC.md#42-fallback-and-bootloader-direct-usb-via-usbfs).

**What two units do and do not establish.** Both run firmware `APP.05.00.00` and carry manufacturing
date `004apr26`, so they are plausibly from a single production batch, and both were measured on the
same host. That is materially stronger than n=1 and it is what §14 asked for — but it is not
evidence about a different firmware revision or a different USB controller, and nothing above should
be read as such. Writes have now been re-tested at 1 ms on both units, and neither has lost one:
0/30 failed and 0/30 read back wrong on unit 1, then the same on unit 2, which took 0.077 s per
write plus read-back. Unit 2's run is the stronger of the two, because it wrote the current limit
**alternating between 4900 and 5000 mA**, checked each read-back against the value just written, and
afterwards restored 5000 mA and verified the restore separately. A write that never reaches the
device reads back as the value it already held, so a test that writes the same number every time
reports success in precisely the case where the write was lost; alternating removes that blind spot.
What makes 1 ms a reasonable default rather than a gamble is the shape of the failure: too little
pacing surfaces as a response timeout, which is visible and retryable, not as a silent wrong write —
and the paths that can damage a load, `voltage`, `current` and `vlimit`, all verify by read-back.

Reports from real hardware are still the most useful contribution — especially from a unit on a
different firmware revision, a different batch, or a different USB controller, since that is where
the pacing default is untested.

---

## Development

```bash
go test ./...                # the whole suite; no hardware needed
go test -race ./...
CGO_ENABLED=0 go build ./cmd/gflex
```

Everything is testable without a device. The protocol is exercised against
`internal/transport/fake`, an in-memory VFLEX that decodes the MIDI framing with a deliberately
*independent* implementation — so a bug in the encoder can't hide behind a matching bug in the
decoder. `testdata/golden/frames.json` pins the frame ↔ MIDI ↔ USB-MIDI encodings so they can't
drift silently.

Coverage is highest where correctness is protocol-critical — `framer`, `pdo`, the fake device and
`proto` lead, with `rawmidi` and `session` close behind — and lowest where the code is mostly ioctl
plumbing or cobra wiring that only real hardware exercises: `usbmidi`, `bootloader`, `usbfs` and
`cli` trail, `cli` by the widest margin. Run `go test -cover ./...` for the current figures; earlier
revisions of this file quoted per-package percentages, which went stale the first time the suite
grew.

The `internal/session` suite dominates the wall time — around 15 s — because no clock is injected:
the retry, backoff and timeout tests sleep for real, on the same code paths that run in production.

The material this was derived from — the vendor's application — is not redistributed here. If you
want to check a claim in [SPEC.md](SPEC.md) against the original,
[§16](SPEC.md#16-reproducing-the-analysis) describes how to reconstruct the same corpus from
publicly obtainable inputs.

## License

[MIT](LICENSE).
