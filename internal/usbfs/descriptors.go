package usbfs

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// USB descriptor types we care about. Everything else in the blob -- string
// descriptors, interface association, the class-specific 0x24/0x25 blocks that
// USB-MIDI and USB-Audio interleave between the standard ones, SuperSpeed
// endpoint companions -- is skipped by bLength rather than rejected, because a
// descriptor we do not understand is not an error.
const (
	descTypeDevice      = 0x01
	descTypeConfig      = 0x02
	descTypeInterface   = 0x04
	descTypeEndpoint    = 0x05
	descTypeCSInterface = 0x24 // e.g. the MIDIStreaming class-specific header
	descTypeCSEndpoint  = 0x25 // e.g. the MIDIStreaming jack association
)

// Endpoint bmAttributes and bEndpointAddress bit fields (USB 2.0 §9.6.6).
const (
	endpointDirMask  = 0x80
	endpointNumMask  = 0x0F
	endpointXferMask = 0x03

	xferControl   = 0
	xferIsoch     = 1
	xferBulk      = 2
	xferInterrupt = 3
)

// ErrBadDescriptor reports a descriptor blob that could not be parsed at all.
var ErrBadDescriptor = errors.New("usbfs: malformed USB descriptor blob")

// Endpoint is one endpoint of one interface alt setting.
type Endpoint struct {
	// Address is the full bEndpointAddress, i.e. the endpoint number with
	// 0x80 set for an IN endpoint. This is what usbfs wants and what
	// Device.Transfer takes. SPEC.md §10.2 flags the opposite convention --
	// the vendor's WebUSB code stores the bare 4-bit endpoint number -- as a
	// translation trap; use Number if you need that form.
	Address uint8
	// Attributes is bmAttributes. The low two bits are the transfer type.
	Attributes uint8
	// MaxPacketSize is wMaxPacketSize.
	MaxPacketSize uint16
	// Interval is bInterval, the polling interval for interrupt endpoints.
	Interval uint8
}

// Number returns the bare 4-bit endpoint number, without the direction bit.
func (e Endpoint) Number() uint8 { return e.Address & endpointNumMask }

// IsIn reports whether this is a device-to-host endpoint.
func (e Endpoint) IsIn() bool { return e.Address&endpointDirMask != 0 }

// IsBulk reports whether bmAttributes says bulk.
//
// Callers must actually check this. SPEC.md §4.2 records that snd-usb-audio
// accepts interrupt endpoints for USB-MIDI just as readily as bulk, so
// hardcoding bulk would be a real bug rather than a theoretical one. The unit
// dumped at bring-up declares bulk endpoints on both its MIDIStreaming and its
// vendor-class interface (SPEC.md §14.3) -- which settles what that one VFLEX
// does in application mode, not what a bootloader-mode descriptor set or a
// later hardware revision will.
func (e Endpoint) IsBulk() bool { return e.Attributes&endpointXferMask == xferBulk }

// IsInterrupt reports whether bmAttributes says interrupt.
func (e Endpoint) IsInterrupt() bool { return e.Attributes&endpointXferMask == xferInterrupt }

// String renders the endpoint for diagnostics, e.g. "ep 0x81 IN interrupt 64".
func (e Endpoint) String() string {
	dir := "OUT"
	if e.IsIn() {
		dir = "IN"
	}
	var kind string
	switch e.Attributes & endpointXferMask {
	case xferControl:
		kind = "control"
	case xferIsoch:
		kind = "isochronous"
	case xferBulk:
		kind = "bulk"
	default:
		kind = "interrupt"
	}
	return fmt.Sprintf("ep 0x%02x %s %s %d", e.Address, dir, kind, e.MaxPacketSize)
}

// Interface is one alternate setting of one USB interface. Every alt setting
// appears separately, so an interface with three alt settings yields three
// Interface values sharing a Number.
type Interface struct {
	// Number is bInterfaceNumber; Alt is bAlternateSetting.
	Number, Alt int
	// Class, SubClass and Protocol are the bInterface* triple. The VFLEX's
	// MIDI interface is class 0x01 or 0xFF with subclass 0x03; its bootloader
	// interface is class 0xFF (SPEC.md §4.2, §10.1).
	Class, SubClass, Protocol uint8
	// Endpoints lists the interface's endpoints in descriptor order.
	Endpoints []Endpoint
	// ConfigurationValue is the bConfigurationValue of the configuration
	// descriptor this alt setting was declared under, or 0 for an interface that
	// appeared before any configuration descriptor (a malformed blob).
	//
	// It matters because bInterfaceNumber is only unique *within* a
	// configuration: on a device that declares two, "interface 1" names two
	// unrelated interfaces and claiming by number alone can claim the wrong one.
	// Selection must therefore be confined to the active configuration -- see
	// Config and Device.Descriptors.
	ConfigurationValue uint8
}

// In returns the first device-to-host endpoint of the interface.
func (i Interface) In() (Endpoint, bool) {
	for _, e := range i.Endpoints {
		if e.IsIn() {
			return e, true
		}
	}
	return Endpoint{}, false
}

// Out returns the first host-to-device endpoint of the interface.
func (i Interface) Out() (Endpoint, bool) {
	for _, e := range i.Endpoints {
		if !e.IsIn() {
			return e, true
		}
	}
	return Endpoint{}, false
}

// String renders the interface for diagnostics.
func (i Interface) String() string {
	return fmt.Sprintf("interface %d alt %d class 0x%02x/0x%02x/0x%02x, %d endpoint(s)",
		i.Number, i.Alt, i.Class, i.SubClass, i.Protocol, len(i.Endpoints))
}

// Configuration is one USB configuration descriptor: the value that selects it
// and the interface alt settings declared under it.
type Configuration struct {
	// Value is bConfigurationValue -- the number Device.SetConfiguration takes.
	// It is not an index: the values need not be contiguous, need not start at
	// 1, and are only guaranteed to be non-zero, since 0 is reserved to mean
	// "unconfigured" (USB 2.0 §9.4.7).
	Value uint8
	// Interfaces is every interface alt setting declared under this
	// configuration, in descriptor order.
	Interfaces []Interface
}

// Config is the parsed descriptor blob for one device -- the whole blob, not a
// single USB configuration descriptor, which is Configuration.
//
// The blob usbfs hands back contains *every* configuration one after another
// (drivers/usb/core/devio.c walks dev->rawdescriptors for all
// bNumConfigurations), and interface numbers are only unique within one of
// them. Configurations keeps them apart; Interfaces is the working set that
// selection runs over, and Device.Descriptors narrows it to the active
// configuration when it can. The unit dumped at bring-up presented its three
// interfaces under configuration 1 (SPEC.md §14.3), but that dump records no
// bNumConfigurations and no bootloader-mode descriptor set has ever been seen,
// so nothing here assumes a single configuration.
type Config struct {
	// Interfaces is the set of interface alt settings to select from, in
	// descriptor order. For a single-configuration device -- and for any device
	// whose active configuration could not be determined -- it is every
	// interface in the blob. Device.Descriptors narrows it to the interfaces of
	// Active on a device that declares more than one configuration.
	Interfaces []Interface
	// Configurations is every configuration descriptor in the blob, in
	// descriptor order, each with its own interfaces. It is never narrowed, so
	// diagnostics can still show what the device declares in full.
	Configurations []Configuration
	// Active is the bConfigurationValue the device currently has selected, or 0
	// when the device is unconfigured or nothing could determine it.
	// ParseDescriptors always leaves it 0: the blob says which configurations
	// exist, never which one is in force, so only Device.Descriptors can fill
	// this in.
	Active uint8
	// VendorID and ProductID come from the device descriptor at the head of
	// the blob, so they are the device's own answer rather than sysfs's.
	VendorID, ProductID uint16
}

// FindInterface returns the first interface alt setting satisfying match.
//
// It searches Interfaces, so on a device whose active configuration is known it
// searches only that configuration.
func (c *Config) FindInterface(match func(Interface) bool) (Interface, bool) {
	for _, i := range c.Interfaces {
		if match(i) {
			return i, true
		}
	}
	return Interface{}, false
}

// InterfacesIn returns the interface alt settings declared under the
// configuration whose bConfigurationValue is value, in descriptor order.
func (c *Config) InterfacesIn(value uint8) []Interface {
	var out []Interface
	for _, cf := range c.Configurations {
		if cf.Value == value {
			out = append(out, cf.Interfaces...)
		}
	}
	return out
}

// FirstConfigurationValue reports the bConfigurationValue to select on a device
// that is not configured at all.
//
// SPEC.md §10.1 phase 2 says "select configuration 1 if unset". 1 is what
// practically every device numbers its first configuration, but that is a
// convention rather than a rule, so the first value the device actually
// declares is preferred and 1 is only the fallback for a blob that declares
// none. A declared value of 0 is skipped: 0 is the "unconfigured" sentinel and
// selecting it would deconfigure the device rather than configure it.
func (c *Config) FirstConfigurationValue() uint8 {
	for _, cf := range c.Configurations {
		if cf.Value != 0 {
			return cf.Value
		}
	}
	return 1
}

// restrictToActive narrows Interfaces to the interfaces of the active
// configuration, so that a selection by interface number cannot pick one that
// belongs to a configuration the device is not currently in.
//
// Every guard here is a reason to leave Interfaces alone rather than risk
// emptying it: an unknown or unconfigured device (Active 0) has no active
// configuration to narrow to; a single-configuration device -- the only shape
// anyone has ever seen a VFLEX in -- cannot benefit, so it keeps byte-identical
// behaviour; and an Active value matching no declared configuration is a
// disagreement between sysfs and the descriptors that should surface as a
// normal "no usable interface" error, not as an empty descriptor set.
func (c *Config) restrictToActive() {
	if c.Active == 0 || len(c.Configurations) < 2 {
		return
	}
	if sub := c.InterfacesIn(c.Active); len(sub) > 0 {
		c.Interfaces = sub
	}
}

// ParseDescriptors decodes a raw usbfs descriptor blob: an 18-byte device
// descriptor followed by the configuration descriptor trees.
//
// The walk is a plain TLV chain keyed on bLength/bDescriptorType. Parsing is
// deliberately lenient about content and strict about structure: unknown
// descriptor types are skipped, but a zero or sub-minimum bLength is fatal
// because it would otherwise loop forever, and a descriptor claiming to run
// past the end of the blob truncates the walk rather than being read out of
// bounds.
//
// The nesting the blob flattens is reconstructed as it goes: each endpoint
// belongs to the interface descriptor that most recently preceded it, and each
// interface to the configuration descriptor that most recently preceded it, so
// Config.Configurations comes back split even though the bytes are not.
// Config.Active is left 0 -- the blob says which configurations exist, never
// which one is in force.
func ParseDescriptors(b []byte) (*Config, error) {
	if len(b) < 2 {
		return nil, fmt.Errorf("%w: %d bytes is too short for any descriptor", ErrBadDescriptor, len(b))
	}
	if b[1] != descTypeDevice {
		return nil, fmt.Errorf("%w: blob starts with descriptor type 0x%02x, want 0x01 (device)", ErrBadDescriptor, b[1])
	}

	cfg := &Config{}
	var (
		cur    *Interface
		curCfg *Configuration
	)

	// flush moves the interface being built into the result. Endpoints belong
	// to whichever interface descriptor most recently preceded them, and the
	// interface belongs to whichever configuration descriptor most recently
	// preceded *it* -- which is how the flattened blob encodes the nesting.
	flush := func() {
		if cur == nil {
			return
		}
		if curCfg != nil {
			cur.ConfigurationValue = curCfg.Value
			curCfg.Interfaces = append(curCfg.Interfaces, *cur)
		}
		// Appended to the union unconditionally, including for an interface
		// that preceded any configuration descriptor: such a blob is malformed,
		// but dropping the interface would turn a device we can still drive into
		// one we cannot.
		cfg.Interfaces = append(cfg.Interfaces, *cur)
		cur = nil
	}

	// flushConfig closes the configuration being built. A configuration
	// descriptor ends the previous configuration and its last interface.
	flushConfig := func() {
		flush()
		if curCfg != nil {
			cfg.Configurations = append(cfg.Configurations, *curCfg)
			curCfg = nil
		}
	}

	for i := 0; i+2 <= len(b); {
		length := int(b[i])
		typ := b[i+1]
		if length < 2 {
			return nil, fmt.Errorf("%w: descriptor at offset %d declares bLength %d", ErrBadDescriptor, i, length)
		}
		if i+length > len(b) {
			// Truncated tail: keep everything parsed so far.
			break
		}
		d := b[i : i+length]

		switch typ {
		case descTypeDevice:
			if length >= 12 {
				cfg.VendorID = binary.LittleEndian.Uint16(d[8:10])
				cfg.ProductID = binary.LittleEndian.Uint16(d[10:12])
			}
		case descTypeInterface:
			if length >= 9 {
				flush()
				cur = &Interface{
					Number:   int(d[2]),
					Alt:      int(d[3]),
					Class:    d[5],
					SubClass: d[6],
					Protocol: d[7],
				}
			}
		case descTypeEndpoint:
			// Standard endpoint descriptors are 7 bytes; audio-class ones are
			// 9 (bRefresh, bSynchAddress follow). The first seven fields are
			// identical, so one decode covers both.
			if length >= 7 && cur != nil {
				cur.Endpoints = append(cur.Endpoints, Endpoint{
					Address:       d[2],
					Attributes:    d[3],
					MaxPacketSize: binary.LittleEndian.Uint16(d[4:6]),
					Interval:      d[6],
				})
			}
		case descTypeConfig:
			// bConfigurationValue is byte 5 of the configuration descriptor
			// (USB 2.0 §9.6.3). Everything after this descriptor and before the
			// next one belongs to this configuration.
			if length >= 9 {
				flushConfig()
				curCfg = &Configuration{Value: d[5]}
			}
		case descTypeCSInterface, descTypeCSEndpoint:
			// Nothing needed from these; skipped by length.
		default:
			// Unknown or future descriptor type; also skipped by length.
		}
		i += length
	}
	flushConfig()

	return cfg, nil
}
