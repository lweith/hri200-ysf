package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/lweith/hri200-ysf/internal/usbredir"
)

// qemuCaps is what QEMU 8.2's usb-redir offers: filter, disconnect ack,
// ep_info max packet size, 64-bit ids, 32-bit bulk length, bulk receiving,
// connect device version, bulk streams.
const qemuCaps = 0xff

// peer plays QEMU's side.
type peer struct {
	t     *testing.T
	nc    net.Conn
	ids64 bool // after the hellos
}

func (q *peer) hlen() int {
	if q.ids64 {
		return 16
	}
	return 12
}

func (q *peer) send(typ, id uint32, payload []byte) {
	n := q.hlen()
	b := make([]byte, n+len(payload))
	binary.LittleEndian.PutUint32(b[0:], typ)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(b[8:], id) // low half of a 64-bit id
	copy(b[n:], payload)
	if _, err := q.nc.Write(b); err != nil {
		q.t.Fatal(err)
	}
}

func (q *peer) read() (typ, id uint32, payload []byte) {
	q.nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	h := make([]byte, q.hlen())
	if _, err := readFull(q.nc, h); err != nil {
		q.t.Fatal(err)
	}
	payload = make([]byte, binary.LittleEndian.Uint32(h[4:]))
	if _, err := readFull(q.nc, payload); err != nil {
		q.t.Fatal(err)
	}
	return binary.LittleEndian.Uint32(h[0:]), binary.LittleEndian.Uint32(h[8:]), payload
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// expect reads packets until one of type typ (skipping iso packets).
func (q *peer) expect(typ uint32) (uint32, []byte) {
	for {
		t, id, p := q.read()
		if t == typ {
			return id, p
		}
		if t != usbredir.IsoPacket {
			q.t.Fatalf("got packet type %d, want %d", t, typ)
		}
	}
}

func (q *peer) control(id uint32, reqType, req uint8, value, index, length uint16, data []byte) (status uint8, reply []byte) {
	h := make([]byte, 10)
	h[0], h[1], h[2] = reqType&0x80, req, reqType
	binary.LittleEndian.PutUint16(h[4:], value)
	binary.LittleEndian.PutUint16(h[6:], index)
	binary.LittleEndian.PutUint16(h[8:], length)
	q.send(usbredir.ControlPacket, id, append(h, data...))
	gotID, p := q.expect(usbredir.ControlPacket)
	if gotID != id {
		q.t.Fatalf("control reply id %d, want %d", gotID, id)
	}
	return p[3], p[10:]
}

func start(t *testing.T) *peer {
	a, b := net.Pipe()
	go serve(a)
	t.Cleanup(func() { b.Close() })
	q := &peer{t: t, nc: b}
	hello := make([]byte, 68)
	copy(hello, "qemu usb-redir guest 8.2.2")
	binary.LittleEndian.PutUint32(hello[64:], qemuCaps)
	go q.send(usbredir.Hello, 0, hello)
	typ, _, p := q.read()
	if typ != usbredir.Hello || len(p) != 68 {
		t.Fatalf("hello: type %d, %d bytes", typ, len(p))
	}
	q.ids64 = true // both offer 64-bit ids
	if _, p := q.expect(usbredir.InterfaceInfo); len(p) != 132 || p[0] != 4 {
		t.Fatalf("interface_info: %d bytes, %d interfaces", len(p), p[0])
	}
	_, ep := q.expect(usbredir.EpInfo)
	if len(ep) != 160 { // no bulk streams: we don't offer them
		t.Fatalf("ep_info: %d bytes, want 160", len(ep))
	}
	if ep[usbredir.EpIndex(hidIn)] != usbredir.TypeInterrupt || ep[usbredir.EpIndex(capture)] != usbredir.TypeInvalid {
		t.Fatalf("ep_info types: hid %d, capture %d", ep[usbredir.EpIndex(hidIn)], ep[usbredir.EpIndex(capture)])
	}
	_, dc := q.expect(usbredir.DeviceConnect)
	want := []byte{usbredir.SpeedFull, 0, 0, 0, 0xaa, 0x26, 0x03, 0x00, 0x00, 0x01}
	if !bytes.Equal(dc, want) {
		t.Fatalf("device_connect % x, want % x", dc, want)
	}
	return q
}

func TestEnumerate(t *testing.T) {
	q := start(t)
	st, d := q.control(1, 0x80, 0x06, 0x0100, 0, 64, nil)
	if st != 0 || !bytes.Equal(d, deviceDescriptor) {
		t.Fatalf("device descriptor: status %d, % x", st, d)
	}
	if st, d = q.control(2, 0x80, 0x06, 0x0200, 0, 9, nil); st != 0 || len(d) != 9 || binary.LittleEndian.Uint16(d[2:]) != 284 {
		t.Fatalf("config header: status %d, % x", st, d)
	}
	if st, d = q.control(3, 0x80, 0x06, 0x0200, 0, 284, nil); st != 0 || len(d) != 284 {
		t.Fatalf("config: status %d, %d bytes", st, len(d))
	}
	if st, d = q.control(4, 0x80, 0x06, 0x0302, 0x0409, 255, nil); st != 0 || d[0] != 102 || d[2] != 'H' {
		t.Fatalf("product string: status %d, % x", st, d)
	}
	if st, _ = q.control(5, 0x80, 0x06, 0x0600, 0, 10, nil); st != usbredir.StatusStall {
		t.Fatalf("device qualifier: status %d, want stall", st)
	}
	if st, d = q.control(6, 0x81, 0x06, 0x2200, 3, 31, nil); st != 0 || len(d) != 31 {
		t.Fatalf("HID report descriptor: status %d, %d bytes", st, len(d))
	}
	// Speaker volume range, unit 8 channel 1.
	if st, d = q.control(7, 0xa1, getMin, 0x0201, 0x0800, 2, nil); st != 0 || int16(binary.LittleEndian.Uint16(d)) != -12032 {
		t.Fatalf("GET_MIN speaker: status %d, % x", st, d)
	}
	// SET_CUR then GET_CUR capture volume.
	if st, _ = q.control(8, 0x21, setCur, 0x0200, 0x0500, 2, []byte{0x00, 0x05}); st != 0 {
		t.Fatalf("SET_CUR: status %d", st)
	}
	if st, d = q.control(9, 0xa1, getCur, 0x0200, 0x0500, 2, nil); st != 0 || !bytes.Equal(d, []byte{0x00, 0x05}) {
		t.Fatalf("GET_CUR: status %d, % x", st, d)
	}
}

func TestCapture(t *testing.T) {
	q := start(t)
	q.send(usbredir.SetConfiguration, 10, []byte{1})
	q.expect(usbredir.InterfaceInfo)
	q.expect(usbredir.EpInfo)
	if id, p := q.expect(usbredir.ConfigurationStatus); id != 10 || p[0] != 0 || p[1] != 1 {
		t.Fatalf("configuration_status id %d % x", id, p)
	}
	q.send(usbredir.SetAltSetting, 11, []byte{1, 1})
	_, ep := q.expect(usbredir.EpInfo)
	if ep[usbredir.EpIndex(capture)] != usbredir.TypeIso || binary.LittleEndian.Uint16(ep[96+2*usbredir.EpIndex(capture):]) != 100 {
		t.Fatalf("capture endpoint after alt 1: type %d", ep[usbredir.EpIndex(capture)])
	}
	if id, p := q.expect(usbredir.AltSettingStatus); id != 11 || !bytes.Equal(p, []byte{0, 1, 1}) {
		t.Fatalf("alt_setting_status id %d % x", id, p)
	}
	// 44.1 kHz: packets of 44 and 45 samples.
	q.control(12, 0x22, setCur, 0x0100, capture, 3, []byte{0x44, 0xac, 0x00})
	q.send(usbredir.StartIsoStream, 13, []byte{capture, 32, 3})
	if _, p := q.expect(usbredir.IsoStreamStatus); !bytes.Equal(p, []byte{0, capture}) {
		t.Fatalf("iso_stream_status % x", p)
	}
	var bytes_, n int
	for n < 100 {
		typ, _, p := q.read()
		if typ != usbredir.IsoPacket {
			t.Fatalf("got type %d during capture", typ)
		}
		l := int(binary.LittleEndian.Uint16(p[2:]))
		if p[0] != capture || p[1] != 0 || l != len(p)-4 || (l != 88 && l != 90) {
			t.Fatalf("iso packet % x", p[:4])
		}
		bytes_ += l
		n++
	}
	if got := bytes_ / 2; got < 4405 || got > 4415 {
		t.Fatalf("%d samples in 100 packets, want 4410", got)
	}
	q.send(usbredir.StopIsoStream, 14, []byte{capture})
	q.expect(usbredir.IsoStreamStatus)
}
