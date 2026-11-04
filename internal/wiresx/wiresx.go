// Package wiresx carries WIRES-X commands between YSF and the HRI-200.
//
// A radio's WIRES-X button sends a command (node info, room list, connect,
// disconnect) as a short Data FR mode transmission; the node answers the same
// way. On the HRI-200's serial line the radio's command is a D1G with no voice
// around it, and the node's answer a D1F carrying the reply's bytes. Both carry
// the same bytes YSFGateway assembles and builds (its WiresX.cpp): a sequence
// byte, a four-byte code such as 5D 71 5F (node info), the arguments, an end
// marker 03 and an 8-bit sum of everything before it.
package wiresx

import (
	"github.com/lweith/hri200-ysf/internal/ysf"
)

// Command is a WIRES-X command or reply and the callsign blocks it came with.
type Command struct {
	CSD1, CSD2, CSD3 [20]byte
	Data             []byte // through the end marker and sum
}

// Codes in the bytes after the sequence byte.
var (
	DXRequest      = [3]byte{0x5D, 0x71, 0x5F}
	AllRequest     = [3]byte{0x5D, 0x66, 0x5F}
	ConnectRequest = [3]byte{0x5D, 0x23, 0x5F}
	DisconnRequest = [3]byte{0x5D, 0x2A, 0x5F}
	CatRequest     = [3]byte{0x5D, 0x67, 0x5F}
)

// IsReply reports whether data is a node's reply: 5D xx 5F 26 after the
// sequence byte.
func IsReply(data []byte) bool {
	return len(data) >= 5 && data[1] == 0x5D && data[3] == 0x5F && data[4] == 0x26
}

// Name names a command or reply by its code, for logs.
func Name(data []byte) string {
	if len(data) < 4 {
		return "unknown"
	}
	switch [3]byte(data[1:4]) {
	case DXRequest:
		return "node info request"
	case AllRequest:
		return "room list request"
	case ConnectRequest:
		return "connect request"
	case DisconnRequest:
		return "disconnect request"
	case CatRequest:
		return "category request"
	}
	if IsReply(data) {
		return "reply"
	}
	return "unknown"
}

// sum is the 8-bit sum that follows the end marker.
func sum(b []byte) byte {
	var s byte
	for _, v := range b {
		s += v
	}
	return s
}

// Assembler collects a command from a radio's Data FR frames, as YSFGateway
// does: the header and frame 0 carry CSD1 and CSD2, frame 1 CSD3 and the first
// 20 bytes, each later frame 40 more, and the last frame is the frame total.
type Assembler struct {
	cmd              [300]byte
	csd1, csd2, csd3 [20]byte
}

// Frame takes one frame, and returns the command when its last frame arrives
// with a valid end marker and sum.
func (a *Assembler) Frame(frame []byte) (Command, bool) {
	f, err := ysf.DecodeFICH(frame)
	if err != nil || f.DT != ysf.DTDataFRMode {
		return Command{}, false
	}
	d1, d2, err1, err2 := ysf.DecodeHeader(frame) // Data FR frames are coded as headers
	switch f.FI {
	case ysf.FIHeader:
		a.cmd = [300]byte{}
		a.csd1, a.csd2, a.csd3 = blank, blank, blank
		if err1 == nil {
			a.csd1 = d1
		}
		if err2 == nil {
			a.csd2 = d2
		}
		return Command{}, false
	case ysf.FICommunications:
	default:
		return Command{}, false
	}
	switch fn := int(f.FN); fn {
	case 0:
		return Command{}, false
	case 1:
		if err2 != nil {
			return Command{}, false
		}
		if err1 == nil {
			a.csd3 = d1
		}
		copy(a.cmd[0:], d2[:])
	default:
		if err1 != nil || err2 != nil {
			return Command{}, false
		}
		copy(a.cmd[(fn-2)*40+20:], d1[:])
		copy(a.cmd[(fn-2)*40+40:], d2[:])
	}
	if f.FN != f.FT || f.FN == 0 {
		return Command{}, false
	}
	// The end marker is the last 03 whose following byte is the sum.
	for i := (int(f.FN)-1)*40 + 20; i > 0; i-- {
		if a.cmd[i] != 0x03 {
			continue
		}
		if sum(a.cmd[:i+1]) != a.cmd[i+1] {
			return Command{}, false
		}
		return Command{
			CSD1: a.csd1, CSD2: a.csd2, CSD3: a.csd3,
			Data: append([]byte(nil), a.cmd[:i+2]...),
		}, true
	}
	return Command{}, false
}

var blank = [20]byte{' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}

// frameTotal is YSFGateway's calculateFT: the last frame number for what is
// left to send.
func frameTotal(left int) byte {
	switch {
	case left > 220:
		return 7
	case left > 180:
		return 6
	case left > 140:
		return 5
	case left > 100:
		return 4
	case left > 60:
		return 3
	case left > 20:
		return 2
	}
	return 1
}

// Frames builds the Data FR transmission that carries c to a radio, as
// YSFGateway's createReply does: a header, the frames, a terminator. Replies
// over 260 bytes go in blocks of eight frames, each block after the first
// starting its data with a 00.
func Frames(c Command) [][]byte {
	length := len(c.Data)
	bt := 0
	if length > 260 {
		bt = 1 + (length-260)/259
		length += bt
	}
	if length > 20 {
		blocks := (length - 20) / 40
		if length%40 > 0 {
			blocks++
		}
		length = blocks*40 + 20
	} else {
		length = 20
	}
	// Room for the block markers and the last frame's padding.
	data := make([]byte, length+40)
	copy(data, c.Data)
	field := func(off int) (b [20]byte) {
		copy(b[:], data[off:])
		return b
	}

	fich := ysf.FICH{FI: ysf.FIHeader, CS: 2, DT: ysf.DTDataFRMode, BT: byte(bt), FT: frameTotal(length)}
	frames := [][]byte{ysf.HeaderFrame(fich, c.CSD1, c.CSD2)}
	fich.FI = ysf.FICommunications
	fn, bn := 0, 0
	for offset := 0; offset < length; {
		var d1, d2 [20]byte
		switch fn {
		case 0:
			fich.FT = frameTotal(length - offset)
			d1, d2 = c.CSD1, c.CSD2
		case 1:
			d1 = c.CSD3
			if bn == 0 {
				d2 = field(offset)
				offset += 20
			} else {
				copy(d2[1:], data[offset:offset+19])
				offset += 19
			}
		default:
			d1 = field(offset)
			offset += 20
			d2 = field(offset)
			offset += 20
		}
		fich.FN, fich.BN = byte(fn), byte(bn)
		frames = append(frames, ysf.HeaderFrame(fich, d1, d2))
		if fn++; fn >= 8 {
			fn, bn = 0, bn+1
		}
	}
	fich.FI = ysf.FITerminator
	fich.FN, fich.BN = byte(fn), byte(bn)
	return append(frames, ysf.HeaderFrame(fich, c.CSD1, c.CSD2))
}
