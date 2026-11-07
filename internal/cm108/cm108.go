// Package cm108 presents a fake C-Media CM108 USB audio adapter's HID
// interface through uhid, so that direwolf's "PTT CM108" drives the HRI-200's
// transmitter. direwolf (1.7, cm108.c cm108_write) sets a GPIO by writing a
// five-byte output report to the hidraw node: 0, 0, data, direction mask, 0,
// with GPIO n as bit n-1. Each report it writes arrives here as a UHID_OUTPUT
// event.
//
// The device is on the virtual bus (BUS_VIRTUAL), so a udev rule can tell it
// from a real CM108: its hid device is named 0006:0D8C:000C.*.
package cm108

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// linux/uhid.h
const (
	uhidDestroy        = 1
	uhidStart          = 2
	uhidStop           = 3
	uhidOpen           = 4
	uhidClose          = 5
	uhidOutput         = 6
	uhidGetReport      = 9
	uhidGetReportReply = 10
	uhidCreate2        = 11
	uhidSetReport      = 13
	uhidSetReportReply = 14
	uhidDataMax        = 4096
	hidMaxDescriptor   = 4096
	busVirtual         = 0x06
	create2Size        = 128 + 64 + 64 + 2 + 2 + 4 + 4 + 4 + 4 + hidMaxDescriptor
	eventSize          = 4 + create2Size // the largest member of the union
	eio                = 5
	cmediaVendor       = 0x0d8c
	cm108Product       = 0x000c
	defaultGPIO        = 3 // direwolf's default, and every known CM108 interface's
)

// descriptor: a vendor-defined application collection with a 4-byte input and
// a 4-byte output report and no report IDs, the shape of the CM108's HID
// interface. Only the output report is used.
var descriptor = []byte{
	0x06, 0x00, 0xff, // Usage Page (Vendor Defined 0xFF00)
	0x09, 0x01, // Usage (1)
	0xa1, 0x01, // Collection (Application)
	0x15, 0x00, //   Logical Minimum (0)
	0x26, 0xff, 0x00, //   Logical Maximum (255)
	0x75, 0x08, //   Report Size (8)
	0x95, 0x04, //   Report Count (4)
	0x09, 0x01, //   Usage (1)
	0x81, 0x02, //   Input (Data, Variable, Absolute)
	0x95, 0x04, //   Report Count (4)
	0x09, 0x02, //   Usage (2)
	0x91, 0x02, //   Output (Data, Variable, Absolute)
	0xc0, // End Collection
}

// Device is the fake adapter.
type Device struct {
	f    *os.File
	GPIO int // the PTT GPIO, 1-8 (default 3)
}

// Open creates the device through /dev/uhid. The hidraw node appears once the
// kernel has started it.
func Open(name string) (*Device, error) {
	f, err := os.OpenFile("/dev/uhid", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	ev := make([]byte, eventSize)
	binary.LittleEndian.PutUint32(ev, uhidCreate2)
	c := ev[4:]
	copy(c[0:128], name)
	copy(c[128:192], "hri200-ysf")
	copy(c[192:256], "hri200-ptt")
	binary.LittleEndian.PutUint16(c[256:], uint16(len(descriptor)))
	binary.LittleEndian.PutUint16(c[258:], busVirtual)
	binary.LittleEndian.PutUint32(c[260:], cmediaVendor)
	binary.LittleEndian.PutUint32(c[264:], cm108Product)
	binary.LittleEndian.PutUint32(c[268:], 0x0100)
	copy(c[276:], descriptor)
	if _, err := f.Write(ev); err != nil {
		f.Close()
		return nil, fmt.Errorf("uhid create: %w", err)
	}
	return &Device{f: f, GPIO: defaultGPIO}, nil
}

// Close destroys the device.
func (d *Device) Close() error {
	ev := make([]byte, 4)
	binary.LittleEndian.PutUint32(ev, uhidDestroy)
	_, _ = d.f.Write(ev)
	return d.f.Close()
}

// Run reports the PTT GPIO's state on ptt each time a report sets it as an
// output, until the device fails or is closed. log, if set, gets every report
// and the device's lifecycle events.
func (d *Device) Run(ptt chan<- bool, log func(format string, args ...any)) error {
	logf := func(format string, args ...any) {
		if log != nil {
			log(format, args...)
		}
	}
	bit := byte(1) << (d.GPIO - 1)
	buf := make([]byte, eventSize)
	for {
		n, err := d.f.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return nil
			}
			return err
		}
		if n < 4 {
			continue
		}
		u := buf[4:n]
		switch binary.LittleEndian.Uint32(buf) {
		case uhidStart:
			logf("ptt: CM108 device started")
		case uhidStop:
			logf("ptt: CM108 device stopped")
		case uhidOpen, uhidClose:
		case uhidOutput:
			if len(u) < uhidDataMax+3 {
				continue
			}
			size := int(binary.LittleEndian.Uint16(u[uhidDataMax:]))
			r := u[:min(size, uhidDataMax)]
			// direwolf writes report number 0 then the CM108's four bytes:
			// 0, data, direction, 0.
			if len(r) == 5 && r[0] == 0 {
				r = r[1:]
			}
			if len(r) < 3 {
				logf("ptt: short report % x", r)
				continue
			}
			data, mask := r[1], r[2]
			if mask&bit == 0 {
				logf("ptt: report % x leaves GPIO %d alone", r, d.GPIO)
				continue
			}
			ptt <- data&bit != 0
		case uhidGetReport:
			// Nothing reads reports; answer at once rather than leave the
			// kernel waiting out its timeout.
			id := binary.LittleEndian.Uint32(u)
			d.reply(uhidGetReportReply, id)
		case uhidSetReport:
			id := binary.LittleEndian.Uint32(u)
			d.reply(uhidSetReportReply, id)
		}
	}
}

func (d *Device) reply(typ uint32, id uint32) {
	ev := make([]byte, 4+4+2+2)
	binary.LittleEndian.PutUint32(ev, typ)
	binary.LittleEndian.PutUint32(ev[4:], id)
	binary.LittleEndian.PutUint16(ev[8:], eio)
	_, _ = d.f.Write(ev)
}
