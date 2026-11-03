package ysf

import (
	"errors"
	"math/bits"
)

// ErrCRC means a decoded field failed its CRC.
var ErrCRC = errors.New("ysf: CRC mismatch")

// deinterleave undoes interleave.
func deinterleave(raw []byte, pairs, cols int) []byte {
	out := make([]byte, (2*pairs+7)/8)
	for i := range pairs {
		n := (i%cols)*40 + (i/cols)*2
		setBit(out, 2*i, getBit(raw, n))
		setBit(out, 2*i+1, getBit(raw, n+1))
	}
	return out
}

// viterbi decodes the K=5 rate 1/2 code of convolve with hard decisions: coded
// holds 2*nBits bits, and the result nBits. The encoder starts in state zero
// and every coded field ends in four zero tail bits, so the trace back starts
// from state zero too.
func viterbi(coded []byte, nBits int) []byte {
	const states = 16 // d1 d2 d3 d4, d1 the most recent input, in bits 3..0
	const inf = 1 << 30
	var metric [states]int
	for s := 1; s < states; s++ {
		metric[s] = inf
	}
	decisions := make([][states]byte, nBits)
	for i := range nBits {
		var r1, r2 int
		if getBit(coded, 2*i) {
			r1 = 1
		}
		if getBit(coded, 2*i+1) {
			r2 = 1
		}
		var next [states]int
		for ns := range states {
			d := ns >> 3
			best, choice := inf, byte(0)
			for x := range 2 {
				s := (ns<<1)&0xF | x // predecessor: the bit shifted out is x
				if metric[s] >= inf {
					continue
				}
				d1, d2, d3, d4 := s>>3&1, s>>2&1, s>>1&1, s&1
				g1 := d ^ d3 ^ d4
				g2 := d ^ d1 ^ d2 ^ d4
				m := metric[s]
				if g1 != r1 {
					m++
				}
				if g2 != r2 {
					m++
				}
				if m < best {
					best, choice = m, byte(x)
				}
			}
			next[ns] = best
			decisions[i][ns] = choice
		}
		metric = next
	}
	out := make([]byte, (nBits+7)/8)
	s := 0
	for i := nBits - 1; i >= 0; i-- {
		setBit(out, i, s>>3 == 1)
		s = (s<<1)&0xF | int(decisions[i][s])
	}
	return out
}

var golayTable [4096]uint32

func init() {
	for d := range golayTable {
		golayTable[d] = golay24(uint32(d))
	}
}

// golayDecode returns the 12 data bits of the nearest (24,12) codeword, and
// false if that is more than three bits away (more than the code corrects).
func golayDecode(w uint32) (uint32, bool) {
	w &= 0xFFFFFF
	if golayTable[w>>12] == w {
		return w >> 12, true
	}
	best, bestDist := uint32(0), 25
	for d, c := range golayTable {
		if dist := bits.OnesCount32(c ^ w); dist < bestDist {
			best, bestDist = uint32(d), dist
		}
	}
	return best, bestDist <= 3
}

// DecodeFICH decodes a frame's FICH.
func DecodeFICH(frame []byte) (FICH, error) {
	if len(frame) < FrameLength {
		return FICH{}, errors.New("ysf: short frame")
	}
	conv := viterbi(deinterleave(frame[5:30], 100, 5), 100)
	var fich [6]byte
	for i := range 4 {
		w := uint32(conv[3*i])<<16 | uint32(conv[3*i+1])<<8 | uint32(conv[3*i+2])
		d, ok := golayDecode(w)
		if !ok {
			return FICH{}, errors.New("ysf: FICH Golay uncorrectable")
		}
		switch i {
		case 0:
			fich[0] = byte(d >> 4)
			fich[1] = byte(d&0xF) << 4
		case 1:
			fich[1] |= byte(d >> 8)
			fich[2] = byte(d)
		case 2:
			fich[3] = byte(d >> 4)
			fich[4] = byte(d&0xF) << 4
		case 3:
			fich[4] |= byte(d >> 8)
			fich[5] = byte(d)
		}
	}
	if crcCCITT(fich[:4]) != uint16(fich[4])<<8|uint16(fich[5]) {
		return FICH{}, ErrCRC
	}
	b := fich
	return FICH{
		FI:   b[0] >> 6,
		CS:   b[0] >> 4 & 3,
		CM:   b[0] >> 2 & 3,
		BN:   b[0] & 3,
		BT:   b[1] >> 6,
		FN:   b[1] >> 3 & 7,
		FT:   b[1] & 7,
		Dev:  b[2]&0x40 != 0,
		MR:   b[2] >> 3 & 7,
		VoIP: b[2]&0x04 != 0,
		DT:   b[2] & 3,
		SQL:  b[3]&0x80 != 0,
		DGID: b[3] & 0x7F,
	}, nil
}

// decodeData decodes one coded, CRC-protected, whitened field of n data bytes
// (20 for header CSD, 10 for a V/D Mode 2 DCH).
func decodeData(raw []byte, n, cols int) ([]byte, error) {
	nBits := (n+2)*8 + 4
	out := viterbi(deinterleave(raw, nBits, cols), nBits)
	if crcCCITT(out[:n]) != uint16(out[n])<<8|uint16(out[n+1]) {
		return nil, ErrCRC
	}
	data := make([]byte, n)
	for i := range n {
		data[i] = out[i] ^ whitening[i]
	}
	return data, nil
}

// DecodeHeader decodes CSD1 and CSD2 from a header or terminator frame. Each
// carries its own CRC, so either can fail alone.
func DecodeHeader(frame []byte) (csd1, csd2 [20]byte, err1, err2 error) {
	payload := frame[30:]
	a, b := make([]byte, 45), make([]byte, 45)
	for i := range 5 {
		copy(a[9*i:9*i+9], payload[18*i:18*i+9])
		copy(b[9*i:9*i+9], payload[18*i+9:18*i+18])
	}
	d1, err1 := decodeData(a, 20, 9)
	if err1 == nil {
		copy(csd1[:], d1)
	}
	d2, err2 := decodeData(b, 20, 9)
	if err2 == nil {
		copy(csd2[:], d2)
	}
	return csd1, csd2, err1, err2
}

// DecodeDCH decodes the data channel of a V/D Mode 2 frame.
func DecodeDCH(frame []byte) ([10]byte, error) {
	var dch [10]byte
	payload := frame[30:]
	raw := make([]byte, 25)
	for i := range 5 {
		copy(raw[5*i:5*i+5], payload[18*i:18*i+5])
	}
	d, err := decodeData(raw, 10, 5)
	if err == nil {
		copy(dch[:], d)
	}
	return dch, err
}

// VoiceBlocks returns a V/D Mode 2 frame's five voice blocks, as on air.
func VoiceBlocks(frame []byte) [5][VoiceBlockLength]byte {
	var v [5][VoiceBlockLength]byte
	payload := frame[30:]
	for i := range 5 {
		copy(v[i][:], payload[18*i+5:18*i+18])
	}
	return v
}
