// Package usbredir speaks the device ("usb-host") side of the usbredir
// protocol (usbredirproto.h, version 0.7) to a QEMU usb-redir device, for an
// emulated USB device. Conn does the framing; the caller answers requests.
//
// Only what a full-speed device with control, isochronous and interrupt
// endpoints needs is implemented. Capabilities offered: connect_device_version,
// ep_info_max_packet_size, 64-bit ids and 32-bit bulk lengths (QEMU needs the
// last three for a device on XHCI). Bulk streams are not offered, so the
// ep_info header is 160 bytes.
package usbredir

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// Packet types.
const (
	Hello                    = 0
	DeviceConnect            = 1
	DeviceDisconnect         = 2
	Reset                    = 3
	InterfaceInfo            = 4
	EpInfo                   = 5
	SetConfiguration         = 6
	GetConfiguration         = 7
	ConfigurationStatus      = 8
	SetAltSetting            = 9
	GetAltSetting            = 10
	AltSettingStatus         = 11
	StartIsoStream           = 12
	StopIsoStream            = 13
	IsoStreamStatus          = 14
	StartInterruptReceiving  = 15
	StopInterruptReceiving   = 16
	InterruptReceivingStatus = 17
	AllocBulkStreams         = 18
	FreeBulkStreams          = 19
	BulkStreamsStatus        = 20
	CancelDataPacket         = 21
	FilterReject             = 22
	FilterFilter             = 23
	DeviceDisconnectAck      = 24
	StartBulkReceiving       = 25
	StopBulkReceiving        = 26
	BulkReceivingStatus      = 27
	ControlPacket            = 100
	BulkPacket               = 101
	IsoPacket                = 102
	InterruptPacket          = 103
	BufferedBulkPacket       = 104
)

// Status codes.
const (
	StatusSuccess   = 0
	StatusCancelled = 1
	StatusInval     = 2
	StatusIOError   = 3
	StatusStall     = 4
	StatusTimeout   = 5
)

// Endpoint (transfer) types, as in the USB spec; TypeInvalid for no endpoint.
const (
	TypeControl   = 0
	TypeIso       = 1
	TypeBulk      = 2
	TypeInterrupt = 3
	TypeInvalid   = 255
)

// Speeds.
const (
	SpeedLow  = 0
	SpeedFull = 1
	SpeedHigh = 2
)

// Capability bits.
const (
	capBulkStreams          = 0
	capConnectDeviceVersion = 1
	capFilter               = 2
	capDisconnectAck        = 3
	capEpInfoMaxPacketSize  = 4
	cap64bitIDs             = 5
	cap32bitBulkLength      = 6
	capBulkReceiving        = 7
)

// OurCaps are the capabilities this side offers.
const OurCaps = 1<<capConnectDeviceVersion | 1<<capEpInfoMaxPacketSize | 1<<cap64bitIDs | 1<<cap32bitBulkLength

// Packet is one usbredir packet: Header is the type-specific header, Data
// what follows it.
type Packet struct {
	Type   uint32
	ID     uint64
	Header []byte
	Data   []byte
}

// typeHeaderLen is the size of the type-specific header of a packet the peer
// (QEMU) sends, given the capabilities both sides have. -1: hello (the rest
// is the version and capabilities), -2: not expected from the peer.
func typeHeaderLen(t uint32, both uint32) int {
	switch t {
	case Hello:
		return -1
	case Reset, GetConfiguration, CancelDataPacket, DeviceDisconnectAck:
		return 0
	case SetConfiguration, GetAltSetting, StopIsoStream, StartInterruptReceiving, StopInterruptReceiving:
		return 1
	case SetAltSetting:
		return 2
	case StartIsoStream:
		return 3
	case ControlPacket:
		return 10
	case IsoPacket, InterruptPacket:
		return 4
	case BulkPacket:
		if both&(1<<cap32bitBulkLength) != 0 {
			return 10
		}
		return 8
	case AllocBulkStreams:
		return 8
	case FreeBulkStreams:
		return 4
	case StartBulkReceiving:
		return 10
	case StopBulkReceiving:
		return 5
	case FilterFilter, FilterReject:
		return 0 // filter_filter is a string in the data
	}
	return -2
}

// Conn is a usbredir connection, device side.
type Conn struct {
	r        *bufio.Reader
	w        io.Writer
	mu       sync.Mutex // serializes writes
	ids64    bool       // both have 64-bit ids: 16-byte headers after the hellos
	PeerCaps uint32
	PeerVer  string
}

// NewConn returns a Conn on rw. Call Handshake before anything else.
func NewConn(rw io.ReadWriter) *Conn {
	return &Conn{r: bufio.NewReaderSize(rw, 64<<10), w: rw}
}

// Handshake sends our hello and reads the peer's. Both hellos have 32-bit
// ids; what follows has 64-bit ids if both sides offer them.
func (c *Conn) Handshake(version string) error {
	h := make([]byte, 64+4)
	copy(h[:63], version)
	binary.LittleEndian.PutUint32(h[64:], OurCaps)
	if err := c.Send(Hello, 0, h, nil); err != nil {
		return err
	}
	p, err := c.readRaw()
	if err != nil {
		return err
	}
	if p.Type != Hello || len(p.Data) < 64 {
		return fmt.Errorf("usbredir: expected hello, got type %d", p.Type)
	}
	v := p.Data[:64]
	for i, b := range v {
		if b == 0 {
			v = v[:i]
			break
		}
	}
	c.PeerVer = string(v)
	if len(p.Data) >= 68 {
		c.PeerCaps = binary.LittleEndian.Uint32(p.Data[64:])
	}
	c.mu.Lock()
	c.ids64 = c.both(cap64bitIDs)
	c.mu.Unlock()
	return nil
}

// Both reports whether both sides have a capability.
func (c *Conn) both(bit uint) bool { return OurCaps&c.PeerCaps&(1<<bit) != 0 }

// readRaw reads one packet without splitting its type header off.
func (c *Conn) readRaw() (Packet, error) {
	n := 12
	if c.ids64 {
		n = 16
	}
	var h [16]byte
	if _, err := io.ReadFull(c.r, h[:n]); err != nil {
		return Packet{}, err
	}
	p := Packet{Type: binary.LittleEndian.Uint32(h[0:])}
	if c.ids64 {
		p.ID = binary.LittleEndian.Uint64(h[8:])
	} else {
		p.ID = uint64(binary.LittleEndian.Uint32(h[8:]))
	}
	l := binary.LittleEndian.Uint32(h[4:])
	if l > 1<<20 {
		return Packet{}, fmt.Errorf("usbredir: packet type %d too long (%d bytes)", p.Type, l)
	}
	p.Data = make([]byte, l)
	if _, err := io.ReadFull(c.r, p.Data); err != nil {
		return Packet{}, err
	}
	return p, nil
}

// Read reads the peer's next packet.
func (c *Conn) Read() (Packet, error) {
	p, err := c.readRaw()
	if err != nil {
		return p, err
	}
	n := typeHeaderLen(p.Type, OurCaps&c.PeerCaps)
	if n == -2 {
		return p, fmt.Errorf("usbredir: unexpected packet type %d", p.Type)
	}
	if n < 0 {
		n = 0
	}
	if len(p.Data) < n {
		return p, fmt.Errorf("usbredir: packet type %d: %d bytes, want at least %d", p.Type, len(p.Data), n)
	}
	p.Header, p.Data = p.Data[:n], p.Data[n:]
	return p, nil
}

// Send sends a packet.
func (c *Conn) Send(t uint32, id uint64, header, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 12
	if c.ids64 {
		n = 16
	}
	b := make([]byte, n+len(header)+len(data))
	binary.LittleEndian.PutUint32(b[0:], t)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(header)+len(data)))
	if c.ids64 {
		binary.LittleEndian.PutUint64(b[8:], id)
	} else {
		binary.LittleEndian.PutUint32(b[8:], uint32(id))
	}
	copy(b[n:], header)
	copy(b[n+len(header):], data)
	_, err := c.w.Write(b)
	return err
}

// SendDeviceConnect announces the device.
func (c *Conn) SendDeviceConnect(speed, class, subclass, protocol uint8, vendor, product, bcdDevice uint16) error {
	h := []byte{speed, class, subclass, protocol, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(h[4:], vendor)
	binary.LittleEndian.PutUint16(h[6:], product)
	if c.both(capConnectDeviceVersion) {
		h = binary.LittleEndian.AppendUint16(h, bcdDevice)
	}
	return c.Send(DeviceConnect, 0, h, nil)
}

// Interface describes an interface for interface_info.
type Interface struct{ Number, Class, Subclass, Protocol uint8 }

// SendInterfaceInfo sends the interfaces of the current configuration.
func (c *Conn) SendInterfaceInfo(ifs []Interface) error {
	h := make([]byte, 4+32*4)
	binary.LittleEndian.PutUint32(h, uint32(len(ifs)))
	for i, f := range ifs {
		h[4+i] = f.Number
		h[4+32+i] = f.Class
		h[4+64+i] = f.Subclass
		h[4+96+i] = f.Protocol
	}
	return c.Send(InterfaceInfo, 0, h, nil)
}

// Endpoint describes an endpoint for ep_info.
type Endpoint struct {
	Address       uint8 // with the direction bit
	Type          uint8
	Interval      uint8
	Interface     uint8
	MaxPacketSize uint16
}

// EpIndex is usbredir's endpoint index: OUT 0-15, IN 16-31.
func EpIndex(addr uint8) int { return int(addr&0x0f) | int(addr&0x80)>>3 }

// SendEpInfo sends the endpoints of the current configuration and alternate
// settings. Endpoints not listed are reported invalid.
func (c *Conn) SendEpInfo(eps []Endpoint) error {
	n := 32 * 3
	if c.both(capEpInfoMaxPacketSize) {
		n += 32 * 2
	}
	if c.both(capBulkStreams) {
		n += 32 * 4
	}
	h := make([]byte, n)
	for i := 0; i < 32; i++ {
		h[i] = TypeInvalid
	}
	for _, e := range eps {
		i := EpIndex(e.Address)
		h[i] = e.Type
		h[32+i] = e.Interval
		h[64+i] = e.Interface
		if c.both(capEpInfoMaxPacketSize) {
			binary.LittleEndian.PutUint16(h[96+2*i:], e.MaxPacketSize)
		}
	}
	return c.Send(EpInfo, 0, h, nil)
}
