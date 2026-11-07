package main

import (
	"encoding/binary"
	"encoding/hex"
	"strings"
	"unicode/utf16"

	"github.com/lweith/hri200-ysf/internal/usbredir"
)

// The HRI-200's audio codec (26aa:0003), as read from the real one
// (/sys/bus/usb/devices/3-4.2/descriptors on tmra-pncsrv, 2026-11-06):
// full speed; interface 0 audio control, 1 capture (alt 1: EP 0x81 iso async,
// 100 bytes, 16-bit mono), 2 playback (alt 1: EP 0x02 iso adaptive, 200 bytes,
// 16-bit stereo), 3 HID consumer control (EP 0x83 interrupt, 1 byte, 64 ms).
var (
	// sysfsDescriptors is the real codec's descriptors file, verbatim: the
	// device descriptor (18 bytes), then the configuration descriptor set.
	sysfsDescriptors = mustHex(`
		12 01 10 01 00 00 00 08 aa 26 03 00 00 01 01 02
		00 01 09 02 1c 01 04 01 00 80 31 09 04 00 00 00
		01 01 00 00 0a 24 01 00 01 65 00 02 01 02 0c 24
		02 03 01 01 04 02 03 00 00 00 09 24 03 04 01 03
		03 08 00 0c 24 02 01 01 02 02 01 00 00 00 00 09
		24 03 02 01 01 01 05 00 08 24 06 05 07 01 03 00
		08 24 06 06 01 01 03 00 0d 24 06 08 09 02 01 01
		02 00 02 00 00 0d 24 04 09 02 06 03 02 03 00 00
		00 00 07 24 05 07 01 01 00 09 04 01 00 00 01 02
		00 00 09 04 01 01 01 01 02 00 00 07 24 01 02 00
		01 00 1d 24 02 01 01 02 10 07 40 1f 00 11 2b 00
		80 3e 00 22 56 00 00 7d 00 44 ac 00 80 bb 00 09
		05 81 05 64 00 01 00 00 07 25 01 01 00 00 00 09
		04 02 00 00 01 02 00 00 09 04 02 01 01 01 02 00
		00 07 24 01 03 00 01 00 1d 24 02 01 02 02 10 07
		40 1f 00 11 2b 00 80 3e 00 22 56 00 00 7d 00 44
		ac 00 80 bb 00 09 05 02 09 c8 00 01 00 00 07 25
		01 01 00 00 00 09 04 03 00 01 03 00 00 00 09 21
		10 01 00 01 22 1f 00 07 05 83 03 01 00 40`)
	deviceDescriptor = sysfsDescriptors[:18]
	configDescriptor = sysfsDescriptors[18:]
	// hidDescriptor is interface 3's HID descriptor, inside configDescriptor.
	hidDescriptor = mustHex("09 21 10 01 00 01 22 1f 00")
	// hidReportDescriptor: consumer control, volume up/down and mute.
	hidReportDescriptor = mustHex(`
		05 0c 09 01 a1 01 15 00 25 01 09 e9 09 ea 75 01
		95 02 81 2a 09 e2 95 01 81 2e 95 05 81 01 c0`)
	strings_ = map[uint8]string{
		1: pad("YAESU MUSEN Co., Ltd.", 50),
		2: pad("HRI-200 A(CH1) USB Audio codec", 50),
	}
)

const (
	vendorID  = 0x26aa
	productID = 0x0003
	bcdDevice = 0x0100

	capture  = 0x81 // EP, interface 1
	playback = 0x02 // EP, interface 2
	hidIn    = 0x83 // EP, interface 3
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		panic(err)
	}
	return b
}

func pad(s string, n int) string { return s + strings.Repeat(" ", n-len(s)) }

func stringDescriptor(i uint8) []byte {
	if i == 0 {
		return []byte{4, 3, 0x09, 0x04} // English (US)
	}
	s, ok := strings_[i]
	if !ok {
		return nil
	}
	u := utf16.Encode([]rune(s))
	b := []byte{byte(2 + 2*len(u)), 3}
	for _, c := range u {
		b = binary.LittleEndian.AppendUint16(b, c)
	}
	return b
}

var interfaces = []usbredir.Interface{
	{Number: 0, Class: 1, Subclass: 1}, // audio control
	{Number: 1, Class: 1, Subclass: 2}, // audio streaming: capture
	{Number: 2, Class: 1, Subclass: 2}, // audio streaming: playback
	{Number: 3, Class: 3},              // HID
}

// endpoints returns the endpoints for the alternate settings of interfaces 1
// and 2. Endpoint 0 is control both ways; the HID endpoint is always there.
func endpoints(alt [4]uint8) []usbredir.Endpoint {
	eps := []usbredir.Endpoint{
		{Address: 0x00, Type: usbredir.TypeControl, MaxPacketSize: 8},
		{Address: 0x80, Type: usbredir.TypeControl, MaxPacketSize: 8},
		{Address: hidIn, Type: usbredir.TypeInterrupt, Interval: 64, Interface: 3, MaxPacketSize: 1},
	}
	if alt[1] == 1 {
		eps = append(eps, usbredir.Endpoint{Address: capture, Type: usbredir.TypeIso, Interval: 1, Interface: 1, MaxPacketSize: 100})
	}
	if alt[2] == 1 {
		eps = append(eps, usbredir.Endpoint{Address: playback, Type: usbredir.TypeIso, Interval: 1, Interface: 2, MaxPacketSize: 200})
	}
	return eps
}

// Audio class requests.
const (
	setCur = 0x01
	getCur = 0x81
	getMin = 0x82
	getMax = 0x83
	getRes = 0x84
)

// Feature unit controls (selector in wValue's high byte).
const (
	muteControl      = 1
	volumeControl    = 2
	bassBoostControl = 9
)

// volumeRange is a feature unit's volume range, 1/256 dB, read from the real
// codec (/proc/asound/card0/usbmixer). 5: capture, 6: mic monitor, 8: speaker.
var volumeRange = map[uint8][3]int16{ // min, max, res
	5: {-7936, 6144, 256},
	6: {-7936, 6144, 256},
	8: {-12032, 0, 256},
}

// controls holds what the guest last set: SET_CUR values by (recipient,
// wValue, wIndex), returned by GET_CUR.
type controls map[[3]uint16][]byte

// audioRequest answers an audio class request, or returns ok false (stall).
func (c controls) audioRequest(reqType, req uint8, value, index uint16, data []byte, length uint16) (reply []byte, ok bool) {
	recip := uint16(reqType & 0x1f)
	key := [3]uint16{recip, value, index}
	sel := uint8(value >> 8)
	unit := uint8(index >> 8)
	switch req {
	case setCur:
		c[key] = append([]byte(nil), data...)
		return nil, true
	case getCur:
		if v, ok := c[key]; ok {
			return trim(v, length), true
		}
		if recip == 2 { // endpoint: sampling frequency
			return trim([]byte{0x80, 0xbb, 0x00}, length), true // 48000
		}
		if unit == 7 { // selector: its only input
			return trim([]byte{1}, length), true
		}
		return make([]byte, length), true // muted false, 0 dB, bass boost off
	case getMin, getMax, getRes:
		if recip == 1 && sel == volumeControl {
			if r, ok := volumeRange[unit]; ok {
				v := r[req-getMin]
				return trim(binary.LittleEndian.AppendUint16(nil, uint16(v)), length), true
			}
		}
		if recip == 1 && unit == 7 {
			return trim([]byte{1}, length), true
		}
		if recip == 1 && (sel == muteControl || sel == bassBoostControl) {
			if req == getMax || req == getRes {
				return trim([]byte{1}, length), true
			}
			return trim([]byte{0}, length), true
		}
	}
	return nil, false
}

func trim(b []byte, n uint16) []byte {
	if len(b) > int(n) {
		return b[:n]
	}
	return b
}
