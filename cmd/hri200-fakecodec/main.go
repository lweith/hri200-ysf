// hri200-fakecodec is a stand-in for the HRI-200's USB audio codec
// (26aa:0003), for the WIRES-X guest, while the host keeps the real one. It
// is a usbredir server: QEMU's usb-redir device connects to it and the guest
// sees a codec with the real one's IDs, descriptors and strings. Capture
// sends silence; playback is thrown away. The HID interface never reports a
// key.
//
// WIRES-X wants the codec present to work at all, but digital (C4FM) voice
// goes over the communication device, not through it. With the real codec on
// the host, capture there stays open whatever the guest does.
//
//	hri200-fakecodec -listen 127.0.0.1:7373
//
// libvirt (the guest connects; it retries if this isn't running yet):
//
//	<redirdev bus='usb' type='tcp'>
//	  <source mode='connect' host='127.0.0.1' service='7373'>
//	    <reconnect enabled='yes' timeout='5'/>
//	  </source>
//	</redirdev>
package main

import (
	"encoding/binary"
	"flag"
	"log"
	"net"
	"sync"
	"time"

	"github.com/lweith/hri200-ysf/internal/usbredir"
)

var (
	listen  = flag.String("listen", "127.0.0.1:7373", "address to accept QEMU's usb-redir connection on")
	verbose = flag.Bool("v", false, "log every request")
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	flag.Parse()
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fake HRI-200 codec on %s", l.Addr())
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("usb-redir connected from %s", c.RemoteAddr())
		err = serve(c)
		c.Close()
		log.Printf("usb-redir disconnected: %v", err)
	}
}

// device is the fake codec's state for one connection.
type device struct {
	c      *usbredir.Conn
	config uint8
	alt    [4]uint8
	ctl    controls

	mu      sync.Mutex
	streams map[uint8]chan struct{} // running IN iso streams; close to stop
}

func serve(nc net.Conn) error {
	c := usbredir.NewConn(nc)
	if err := c.Handshake("hri200-fakecodec"); err != nil {
		return err
	}
	log.Printf("peer %q, capabilities %#x", c.PeerVer, c.PeerCaps)
	d := &device{c: c, config: 1, ctl: controls{}, streams: map[uint8]chan struct{}{}}
	defer d.stopAll()
	if err := c.SendInterfaceInfo(interfaces); err != nil {
		return err
	}
	if err := c.SendEpInfo(endpoints(d.alt)); err != nil {
		return err
	}
	if err := c.SendDeviceConnect(usbredir.SpeedFull, 0, 0, 0, vendorID, productID, bcdDevice); err != nil {
		return err
	}
	for {
		p, err := c.Read()
		if err != nil {
			return err
		}
		if err := d.handle(p); err != nil {
			return err
		}
	}
}

func (d *device) handle(p usbredir.Packet) error {
	c := d.c
	switch p.Type {
	case usbredir.Reset:
		if *verbose {
			log.Printf("reset")
		}
		d.stopAll()
		d.alt = [4]uint8{}
		return c.SendEpInfo(endpoints(d.alt))

	case usbredir.SetConfiguration:
		d.config = p.Header[0]
		d.stopAll()
		d.alt = [4]uint8{}
		if *verbose {
			log.Printf("set configuration %d", d.config)
		}
		if err := c.SendInterfaceInfo(interfaces); err != nil {
			return err
		}
		if err := c.SendEpInfo(endpoints(d.alt)); err != nil {
			return err
		}
		return c.Send(usbredir.ConfigurationStatus, p.ID, []byte{usbredir.StatusSuccess, d.config}, nil)

	case usbredir.GetConfiguration:
		return c.Send(usbredir.ConfigurationStatus, p.ID, []byte{usbredir.StatusSuccess, d.config}, nil)

	case usbredir.SetAltSetting:
		ifn, alt := p.Header[0], p.Header[1]
		status := uint8(usbredir.StatusSuccess)
		switch {
		case ifn > 3, (ifn == 1 || ifn == 2) && alt > 1, (ifn == 0 || ifn == 3) && alt != 0:
			status = usbredir.StatusInval
		default:
			if d.alt[ifn] != alt {
				if ifn == 1 {
					d.stop(capture)
				}
				d.alt[ifn] = alt
				log.Printf("guest: interface %d (%s) alt %d", ifn, map[uint8]string{0: "control", 1: "capture", 2: "playback", 3: "HID"}[ifn], alt)
				if err := c.SendEpInfo(endpoints(d.alt)); err != nil {
					return err
				}
			}
		}
		return c.Send(usbredir.AltSettingStatus, p.ID, []byte{status, ifn, d.alt[ifn&3]}, nil)

	case usbredir.GetAltSetting:
		ifn := p.Header[0]
		if ifn > 3 {
			return c.Send(usbredir.AltSettingStatus, p.ID, []byte{usbredir.StatusInval, ifn, 0}, nil)
		}
		return c.Send(usbredir.AltSettingStatus, p.ID, []byte{usbredir.StatusSuccess, ifn, d.alt[ifn]}, nil)

	case usbredir.StartIsoStream:
		ep := p.Header[0]
		status := uint8(usbredir.StatusSuccess)
		switch {
		case ep == capture && d.alt[1] == 1:
			d.start(ep)
		case ep == playback && d.alt[2] == 1:
			// Iso OUT packets arrive on their own; nothing to start.
		default:
			status = usbredir.StatusInval
		}
		if *verbose {
			log.Printf("start iso %#02x: status %d", ep, status)
		}
		return c.Send(usbredir.IsoStreamStatus, p.ID, []byte{status, ep}, nil)

	case usbredir.StopIsoStream:
		ep := p.Header[0]
		d.stop(ep)
		return c.Send(usbredir.IsoStreamStatus, p.ID, []byte{usbredir.StatusSuccess, ep}, nil)

	case usbredir.StartInterruptReceiving, usbredir.StopInterruptReceiving:
		ep := p.Header[0]
		status := uint8(usbredir.StatusSuccess)
		if ep != hidIn {
			status = usbredir.StatusInval
		}
		// No key is ever pressed: nothing is ever sent.
		return c.Send(usbredir.InterruptReceivingStatus, p.ID, []byte{status, ep}, nil)

	case usbredir.IsoPacket:
		return nil // playback: discarded

	case usbredir.ControlPacket:
		return d.control(p)

	case usbredir.CancelDataPacket, usbredir.FilterFilter, usbredir.FilterReject, usbredir.DeviceDisconnectAck:
		return nil

	case usbredir.InterruptPacket, usbredir.BulkPacket:
		// Nothing to write to; fail it.
		h := append([]byte(nil), p.Header...)
		h[1] = usbredir.StatusInval
		return c.Send(p.Type, p.ID, h, nil)
	}
	log.Printf("ignoring packet type %d", p.Type)
	return nil
}

// control answers a control transfer on endpoint 0.
func (d *device) control(p usbredir.Packet) error {
	h := p.Header
	ep, req, reqType := h[0], h[1], h[2]
	value := binary.LittleEndian.Uint16(h[4:])
	index := binary.LittleEndian.Uint16(h[6:])
	length := binary.LittleEndian.Uint16(h[8:])
	reply, ok := d.answer(reqType, req, value, index, p.Data, length)
	if *verbose || !ok {
		log.Printf("control ep %#02x type %#02x req %#02x value %#04x index %#04x length %d: ok %v, %d bytes", ep, reqType, req, value, index, length, ok, len(reply))
	}
	out := append([]byte(nil), h...)
	out[3] = usbredir.StatusSuccess
	if !ok {
		out[3] = usbredir.StatusStall
		reply = nil
	}
	if reqType&0x80 != 0 {
		binary.LittleEndian.PutUint16(out[8:], uint16(len(reply)))
	} else {
		reply = nil // OUT: the reply carries no data; length is what was written
	}
	return d.c.Send(usbredir.ControlPacket, p.ID, out, reply)
}

func (d *device) answer(reqType, req uint8, value, index uint16, data []byte, length uint16) ([]byte, bool) {
	kind := reqType & 0x60
	recip := reqType & 0x1f
	switch kind {
	case 0x00: // standard
		switch req {
		case 0x00: // GET_STATUS
			if recip == 0 {
				return trim([]byte{0, 0}, length), true // bus powered, no remote wakeup
			}
			return trim([]byte{0, 0}, length), true
		case 0x01, 0x03: // CLEAR_FEATURE, SET_FEATURE
			return nil, true
		case 0x06: // GET_DESCRIPTOR
			typ, i := uint8(value>>8), uint8(value)
			switch {
			case recip == 0 && typ == 1:
				return trim(deviceDescriptor, length), true
			case recip == 0 && typ == 2 && i == 0:
				return trim(configDescriptor, length), true
			case recip == 0 && typ == 3:
				if s := stringDescriptor(i); s != nil {
					return trim(s, length), true
				}
			case recip == 1 && index == 3 && typ == 0x21:
				return trim(hidDescriptor, length), true
			case recip == 1 && index == 3 && typ == 0x22:
				return trim(hidReportDescriptor, length), true
			}
			return nil, false // includes the device qualifier: full speed only
		case 0x08: // GET_CONFIGURATION (QEMU normally handles it)
			return trim([]byte{d.config}, length), true
		case 0x0a: // GET_INTERFACE
			if index < 4 {
				return trim([]byte{d.alt[index]}, length), true
			}
		}
		return nil, false
	case 0x20: // class
		if recip == 1 && uint8(index) == 3 { // HID
			switch req {
			case 0x0a, 0x0b, 0x09: // SET_IDLE, SET_PROTOCOL, SET_REPORT
				return nil, true
			case 0x01: // GET_REPORT: nothing pressed
				return trim([]byte{0}, length), true
			case 0x02: // GET_IDLE
				return trim([]byte{0}, length), true
			case 0x03: // GET_PROTOCOL
				return trim([]byte{1}, length), true
			}
			return nil, false
		}
		if (recip == 1 && uint8(index) == 0) || recip == 2 {
			return d.ctl.audioRequest(reqType, req, value, index, data, length)
		}
	}
	return nil, false
}

// sampleRate is the capture rate the guest last set, 48 kHz until it does.
func (d *device) sampleRate() int {
	v, ok := d.ctl[[3]uint16{2, 0x0100, capture}]
	if !ok || len(v) < 3 {
		return 48000
	}
	r := int(v[0]) | int(v[1])<<8 | int(v[2])<<16
	if r < 8000 || r > 48000 {
		return 48000
	}
	return r
}

// start sends silence on an IN iso endpoint at the capture rate, one packet
// per 1 ms frame, until stop.
func (d *device) start(ep uint8) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.streams[ep]; ok {
		return
	}
	done := make(chan struct{})
	d.streams[ep] = done
	rate := d.sampleRate()
	log.Printf("guest: capture started, %d Hz (silence)", rate)
	go func() {
		const batch = 4 * time.Millisecond
		t := time.NewTicker(batch)
		defer t.Stop()
		begin := time.Now()
		var frames, samples int64
		silence := make([]byte, 100)
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				due := int64(now.Sub(begin) / time.Millisecond)
				if due-frames > 50 { // fell behind (stalled): don't burst
					frames = due - 4
					samples = frames * int64(rate) / 1000
				}
				for ; frames < due; frames++ {
					n := (frames+1)*int64(rate)/1000 - samples
					samples += n
					h := []byte{ep, usbredir.StatusSuccess, 0, 0}
					binary.LittleEndian.PutUint16(h[2:], uint16(2*n))
					if err := d.c.Send(usbredir.IsoPacket, 0, h, silence[:2*n]); err != nil {
						return
					}
				}
			}
		}
	}()
}

func (d *device) stop(ep uint8) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if done, ok := d.streams[ep]; ok {
		close(done)
		delete(d.streams, ep)
		if ep == capture {
			log.Printf("guest: capture stopped")
		}
	}
}

func (d *device) stopAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for ep, done := range d.streams {
		close(done)
		delete(d.streams, ep)
	}
}
