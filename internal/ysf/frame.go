package ysf

// Frame information (FI).
const (
	FIHeader         = 0
	FICommunications = 1
	FITerminator     = 2
)

// Data types (DT).
const (
	DTVDMode1    = 0
	DTDataFRMode = 1
	DTVDMode2    = 2
	DTVoiceFR    = 3
)

// FICH is the frame information channel.
type FICH struct {
	FI   byte // frame information: header, communications, terminator
	CS   byte // callsign information size; 2 for 20-character callsign data
	CM   byte // call mode: 0 group
	BN   byte // block number
	BT   byte // block total
	FN   byte // frame number, 0-7
	FT   byte // frame total
	Dev  bool // low deviation
	MR   byte // message route
	VoIP bool
	DT   byte // data type
	SQL  bool
	DGID byte
}

// raw returns the four FICH data bytes, MMDVMHost's layout.
func (f FICH) raw() [4]byte {
	var b [4]byte
	b[0] = (f.FI&3)<<6 | (f.CS&3)<<4 | (f.CM&3)<<2 | f.BN&3
	b[1] = (f.BT&3)<<6 | (f.FN&7)<<3 | f.FT&7
	b[2] = (f.MR&7)<<3 | f.DT&3
	if f.Dev {
		b[2] |= 0x40
	}
	if f.VoIP {
		b[2] |= 0x04
	}
	b[3] = f.DGID & 0x7F
	if f.SQL {
		b[3] |= 0x80
	}
	return b
}

// encodeFICH returns the 25 coded FICH bytes.
func encodeFICH(f FICH) []byte {
	raw := f.raw()
	var fich [6]byte
	copy(fich[:], raw[:])
	addCRC(fich[:])

	words := [4]uint32{
		uint32(fich[0])<<4 | uint32(fich[1])>>4,
		uint32(fich[1]&0x0F)<<8 | uint32(fich[2]),
		uint32(fich[3])<<4 | uint32(fich[4])>>4,
		uint32(fich[4]&0x0F)<<8 | uint32(fich[5]),
	}
	conv := make([]byte, 13) // 96 bits of Golay code and 4 zero tail bits
	for i, w := range words {
		c := golay24(w)
		conv[3*i] = byte(c >> 16)
		conv[3*i+1] = byte(c >> 8)
		conv[3*i+2] = byte(c)
	}
	return interleave(convolve(conv, 100), 100, 5)
}

// encodeData20 codes 20 bytes of full-rate data (CSD1 or CSD2 in a header) into
// 45 bytes: whitened, CRC, convolutional code, 9x20 interleave.
func encodeData20(dt [20]byte) []byte {
	out := make([]byte, 23) // 20 data, 2 CRC, and the tail bits
	for i := range 20 {
		out[i] = dt[i] ^ whitening[i]
	}
	addCRC(out[:22])
	return interleave(convolve(out, 180), 180, 9)
}

// encodeDCH10 codes the 10 bytes of a V/D Mode 2 frame's data channel into 25
// bytes: whitened, CRC, convolutional code, 5x20 interleave.
func encodeDCH10(dt [10]byte) []byte {
	out := make([]byte, 13) // 10 data, 2 CRC, and the tail bits
	for i := range 10 {
		out[i] = dt[i] ^ whitening[i]
	}
	addCRC(out[:12])
	return interleave(convolve(out, 100), 100, 5)
}

func newFrame(f FICH) []byte {
	frame := make([]byte, FrameLength)
	copy(frame, Sync[:])
	copy(frame[5:30], encodeFICH(f))
	return frame
}

// HeaderFrame builds a header or terminator frame carrying CSD1 (destination,
// source) and CSD2 (downlink, uplink).
func HeaderFrame(f FICH, csd1, csd2 [20]byte) []byte {
	frame := newFrame(f)
	payload := frame[30:]
	a, b := encodeData20(csd1), encodeData20(csd2)
	for i := range 5 {
		copy(payload[18*i:18*i+9], a[9*i:9*i+9])
		copy(payload[18*i+9:18*i+18], b[9*i:9*i+9])
	}
	return frame
}

// VoiceBlockLength is one V/D Mode 2 voice channel block, 104 bits, as on air:
// interleaved and whitened.
const VoiceBlockLength = 13

// VDMode2Frame builds a V/D Mode 2 communications frame from 10 bytes of data
// channel and five voice blocks, which are copied unchanged.
func VDMode2Frame(f FICH, dch [10]byte, voice [5][VoiceBlockLength]byte) []byte {
	frame := newFrame(f)
	payload := frame[30:]
	coded := encodeDCH10(dch)
	for i := range 5 {
		copy(payload[18*i:18*i+5], coded[5*i:5*i+5])
		copy(payload[18*i+5:18*i+18], voice[i][:])
	}
	return frame
}
