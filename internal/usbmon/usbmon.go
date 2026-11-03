// Package usbmon reads USB traffic from Linux usbmon, live from /dev/usbmonN or
// from a pcap file written by tcpdump on a usbmon interface.
package usbmon

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Transfer types.
const (
	Isochronous = 0
	Interrupt   = 1
	Control     = 2
	Bulk        = 3
)

// Event is one URB submission ('S'), completion ('C') or error ('E').
type Event struct {
	Time     time.Time
	Type     byte
	XferType byte
	Endpoint byte // with 0x80 set for IN
	Device   byte
	Bus      uint16
	Status   int32
	Data     []byte
}

// headerLength is the binary API's original event header, which read() returns.
const headerLength = 48

func parseHeader(h []byte) Event {
	return Event{
		Type:     h[8],
		XferType: h[9],
		Endpoint: h[10],
		Device:   h[11],
		Bus:      binary.LittleEndian.Uint16(h[12:]),
		Time: time.Unix(int64(binary.LittleEndian.Uint64(h[16:])),
			int64(int32(binary.LittleEndian.Uint32(h[24:])))*1000),
		Status: int32(binary.LittleEndian.Uint32(h[28:])),
	}
}

// Payload returns the data a bulk transfer carried: OUT data is in the
// submission, IN data in the completion. Anything else returns nil.
func (e Event) Payload() []byte {
	if e.XferType != Bulk || len(e.Data) == 0 {
		return nil
	}
	if e.Endpoint&0x80 != 0 && e.Type == 'C' && e.Status == 0 {
		return e.Data
	}
	if e.Endpoint&0x80 == 0 && e.Type == 'S' {
		return e.Data
	}
	return nil
}

// FindBus returns the USB bus number of the device with this vendor and
// product ID, from sysfs.
func FindBus(vendor, product string) (int, error) {
	dirs, _ := filepath.Glob("/sys/bus/usb/devices/*")
	for _, d := range dirs {
		v, _ := os.ReadFile(filepath.Join(d, "idVendor"))
		p, _ := os.ReadFile(filepath.Join(d, "idProduct"))
		if strings.TrimSpace(string(v)) != vendor || strings.TrimSpace(string(p)) != product {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d, "busnum"))
		if err != nil {
			return 0, err
		}
		var bus int
		_, err = fmt.Sscan(string(b), &bus)
		return bus, err
	}
	return 0, fmt.Errorf("no USB device %s:%s", vendor, product)
}

const (
	ioctlRingSize = 0x9204     // MON_IOCT_RING_SIZE
	ioctlStats    = 0x80089203 // MON_IOCG_STATS
)

// Live reads events from a usbmon device.
type Live struct {
	f   *os.File
	buf []byte
}

// OpenLive opens /dev/usbmonN and asks for a 1 MiB ring, so the audio device on
// the same bus can't crowd out the serial traffic between reads.
func OpenLive(path string) (*Live, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	_ = unix.IoctlSetInt(int(f.Fd()), ioctlRingSize, 1<<20)
	return &Live{f: f, buf: make([]byte, 1<<20)}, nil
}

// Next blocks for the next event.
func (l *Live) Next() (Event, error) {
	n, err := l.f.Read(l.buf)
	if err != nil {
		return Event{}, err
	}
	if n < headerLength {
		return Event{}, fmt.Errorf("short usbmon read: %d bytes", n)
	}
	e := parseHeader(l.buf[:headerLength])
	e.Data = append([]byte(nil), l.buf[headerLength:n]...)
	return e, nil
}

// Dropped returns the number of events the kernel dropped because the ring was full.
func (l *Live) Dropped() (uint32, error) {
	var stats [2]uint32
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, l.f.Fd(), ioctlStats, uintptr(unsafe.Pointer(&stats)))
	if errno != 0 {
		return 0, errno
	}
	return stats[1], nil
}

// Close closes the device.
func (l *Live) Close() error { return l.f.Close() }

// Pcap reads events from a tcpdump capture of a usbmon interface
// (link type USB_LINUX_MMAPPED, 220).
type Pcap struct {
	r *bufio.Reader
	f *os.File
}

// OpenPcap opens a capture file.
func OpenPcap(path string) (*Pcap, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(f)
	var hdr [24]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		f.Close()
		return nil, err
	}
	if binary.LittleEndian.Uint32(hdr[0:]) != 0xA1B2C3D4 {
		f.Close()
		return nil, errors.New("not a little-endian microsecond pcap file")
	}
	if lt := binary.LittleEndian.Uint32(hdr[20:]); lt != 220 {
		f.Close()
		return nil, fmt.Errorf("link type %d, want 220 (USB_LINUX_MMAPPED)", lt)
	}
	return &Pcap{r: r, f: f}, nil
}

// Next returns the next event, or io.EOF.
func (p *Pcap) Next() (Event, error) {
	var rec [16]byte
	if _, err := io.ReadFull(p.r, rec[:]); err != nil {
		return Event{}, err
	}
	n := binary.LittleEndian.Uint32(rec[8:])
	data := make([]byte, n)
	if _, err := io.ReadFull(p.r, data); err != nil {
		return Event{}, err
	}
	if n < 64 {
		return Event{}, fmt.Errorf("short usbmon record: %d bytes", n)
	}
	e := parseHeader(data[:headerLength])
	ndesc := binary.LittleEndian.Uint32(data[60:])
	start := 64
	if e.XferType == Isochronous {
		start += int(ndesc) * 16
	}
	if start <= len(data) {
		e.Data = data[start:]
	}
	return e, nil
}

// Close closes the file.
func (p *Pcap) Close() error { return p.f.Close() }
