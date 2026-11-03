// Package ysf builds System Fusion (C4FM) frames: sync, FICH and payload.
//
// The encoders follow MMDVMHost's (G4KLX, GPL-2.0-or-later): CRC-CCITT, Golay
// (24,12), the K=5 rate 1/2 convolutional code, interleaving and whitening.
package ysf

import "math/bits"

// FrameLength is a YSF frame: 5 bytes sync, 25 bytes FICH, 90 bytes payload.
const FrameLength = 120

// Sync is the YSF frame sync.
var Sync = [5]byte{0xD4, 0x71, 0xC9, 0x63, 0x4D}

// whitening is XORed over data before CRC and coding.
var whitening = [20]byte{0x93, 0xD7, 0x51, 0x21, 0x9C, 0x2F, 0x6C, 0xD0, 0xEF, 0x0F,
	0xF8, 0x3D, 0xF1, 0x73, 0x20, 0x94, 0xED, 0x1E, 0x7C, 0xD8}

func getBit(b []byte, i int) bool { return b[i>>3]&(0x80>>(i&7)) != 0 }

func setBit(b []byte, i int, v bool) {
	if v {
		b[i>>3] |= 0x80 >> (i & 7)
	} else {
		b[i>>3] &^= 0x80 >> (i & 7)
	}
}

// crcCCITT is CRC-16 with polynomial 0x1021, initial value 0, inverted at the end.
func crcCCITT(data []byte) uint16 {
	var crc uint16
	for _, x := range data {
		crc ^= uint16(x) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return ^crc
}

// addCRC writes the CRC of buf[:len-2] into its last two bytes, high byte first.
func addCRC(buf []byte) {
	c := crcCCITT(buf[:len(buf)-2])
	buf[len(buf)-2] = byte(c >> 8)
	buf[len(buf)-1] = byte(c)
}

// golay24 encodes 12 data bits as the (24,12) extended Golay code: data, the 11
// (23,12) check bits for generator 0xC75, then overall parity.
func golay24(d uint32) uint32 {
	d &= 0xFFF
	r := d << 11
	for i := 22; i >= 11; i-- {
		if r&(1<<i) != 0 {
			r ^= 0xC75 << (i - 11)
		}
	}
	cw23 := d<<11 | r
	cw := cw23 << 1
	if bits.OnesCount32(cw23)%2 == 1 {
		cw |= 1
	}
	return cw
}

// convolve is the K=5, rate 1/2 code with generators 1+D^3+D^4 and 1+D+D^2+D^4,
// over the first nBits of in. out gets 2*nBits bits.
func convolve(in []byte, nBits int) []byte {
	out := make([]byte, (2*nBits+7)/8)
	var d1, d2, d3, d4 byte
	k := 0
	for i := range nBits {
		var d byte
		if getBit(in, i) {
			d = 1
		}
		g1 := (d + d3 + d4) & 1
		g2 := (d + d1 + d2 + d4) & 1
		d4, d3, d2, d1 = d3, d2, d1, d
		setBit(out, k, g1 == 1)
		setBit(out, k+1, g2 == 1)
		k += 2
	}
	return out
}

// interleave places pairs of coded bits: pair i goes to bit (i%cols)*40 + (i/cols)*2.
// cols is 5 for FICH and V/D Mode 2 DCH, 9 for full-rate data (headers).
func interleave(coded []byte, pairs, cols int) []byte {
	out := make([]byte, (2*pairs+7)/8)
	for i := range pairs {
		n := (i%cols)*40 + (i/cols)*2
		setBit(out, n, getBit(coded, 2*i))
		setBit(out, n+1, getBit(coded, 2*i+1))
	}
	return out
}
