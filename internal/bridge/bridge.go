// Package bridge turns HRI-200 messages into YSF transmissions: a header frame,
// one V/D Mode 2 frame per voice message, and a terminator.
package bridge

import (
	"bytes"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/ysf"
)

// Direction says which side of the HRI-200 a message came from.
type Direction int

const (
	// Radio is traffic the radio received over the air (D1H, D1R).
	Radio Direction = iota
	// Network is traffic WIRES-X sent for the radio to transmit (D1F, D1E).
	Network
)

func (d Direction) String() string {
	if d == Radio {
		return "radio"
	}
	return "network"
}

// Packet is one YSF frame ready for the network.
type Packet struct {
	Source, Dest [10]byte
	Seq          byte // 7 bits
	End          bool
	Frame        []byte
}

// DefaultTimeout ends a transmission when no voice frame has arrived for this
// long. Frames come every 100 ms.
const DefaultTimeout = 400 * time.Millisecond

type csd struct {
	set   [3]bool
	data  [3][20]byte
	dtSet bool
	dt    [20]byte // frames 6 and 7, from D1G
}

type session struct {
	dir   Direction
	csd   csd
	ft    byte // frame total: 6, or 7 when the DT record needs frames 6 and 7
	fn    byte
	seq   byte
	last  time.Time
	start time.Time
	count int
}

// Bridge holds the state of the current transmission.
type Bridge struct {
	Out     func(Packet)
	Log     func(format string, args ...any)
	Timeout time.Duration

	pending [2]csd
	active  *session
}

func (b *Bridge) logf(format string, args ...any) {
	if b.Log != nil {
		b.Log(format, args...)
	}
}

// Handle processes one message from one direction.
func (b *Bridge) Handle(dir Direction, m hri.Message, now time.Time) {
	switch {
	case dir == Radio && m.Cmd == 'H':
		c, err := hri.ParseCallsignBlock(m)
		if err != nil {
			b.logf("radio: %v", err)
			return
		}
		b.setCSD(dir, c.Block-1, c.Text)
	case dir == Network && m.Cmd == 'F' && len(m.Payload) == 0x52:
		h, _ := hri.ParseTxHeader(m)
		b.setCSD(dir, 0, h.CSD1)
		b.setCSD(dir, 1, h.CSD2)
		b.setCSD(dir, 2, h.CSD3)
	case m.Cmd == 'G' || (dir == Network && m.Cmd == 'F'):
		// The callsign blocks and the DT data of frames 6 and 7: a D1G from
		// either side about once per eight frames, or WIRES-X's long D1F
		// header, which it repeats every few seconds.
		d, ok := hri.ParseData(m)
		if !ok {
			return
		}
		b.setCSD(dir, 0, d.CSD1)
		b.setCSD(dir, 1, d.CSD2)
		b.setCSD(dir, 2, d.CSD3)
		b.setDT(dir, d.DT)
	case (dir == Radio && m.Cmd == 'R') || (dir == Network && m.Cmd == 'E'):
		if len(m.Payload) == 1 {
			return // an acknowledgement
		}
		v, err := hri.ParseVoice(m)
		if err != nil {
			b.logf("%s: %v", dir, err)
			return
		}
		b.voice(dir, v, now)
	}
}

// Tick ends a transmission whose voice frames have stopped.
func (b *Bridge) Tick(now time.Time) {
	timeout := b.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if b.active != nil && now.Sub(b.active.last) > timeout {
		b.end()
	}
}

func (b *Bridge) setCSD(dir Direction, i int, text [20]byte) {
	b.pending[dir].data[i] = text
	b.pending[dir].set[i] = true
	if b.active != nil && b.active.dir == dir {
		b.active.csd.data[i] = text
		b.active.csd.set[i] = true
	}
}

func (b *Bridge) setDT(dir Direction, dt [20]byte) {
	b.pending[dir].dt = dt
	b.pending[dir].dtSet = true
	if b.active != nil && b.active.dir == dir {
		b.active.csd.dt = dt
		b.active.csd.dtSet = true
	}
}

func (b *Bridge) voice(dir Direction, v hri.Voice, now time.Time) {
	if b.active != nil && b.active.dir != dir {
		b.end()
	}
	if b.active == nil {
		b.active = &session{dir: dir, csd: b.pending[dir], start: now, ft: 6}
		s := b.active
		if n, ok := s.record(); ok && n > 10 {
			s.ft = 7
		}
		b.logf("%s: start, from %q to %q", dir, trim(s.source()), trim(s.dest()))
		b.send(ysf.HeaderFrame(s.fich(ysf.FIHeader, 0), s.block(0), s.block(1)), false)
	}
	s := b.active
	s.last = now
	s.count++
	b.send(ysf.VDMode2Frame(s.fich(ysf.FICommunications, s.fn), s.dch(s.fn), v.Blocks), false)
	s.fn = (s.fn + 1) % (s.ft + 1)
}

func (b *Bridge) end() {
	s := b.active
	b.send(ysf.HeaderFrame(s.fich(ysf.FITerminator, s.fn), s.block(0), s.block(1)), true)
	b.logf("%s: end, %d frames, %.1f s", s.dir, s.count, s.last.Sub(s.start).Seconds()+0.1)
	b.active = nil
	if s.dir == Network {
		// WIRES-X sometimes pauses mid-transmission long enough to end it
		// here, then carries on without a new header. Keep the callsigns for
		// that; a new transmission always starts with a header that replaces
		// them. The DT record is the transmitting radio's, so it goes.
		b.pending[s.dir].dt, b.pending[s.dir].dtSet = [20]byte{}, false
		return
	}
	b.pending[s.dir] = csd{}
}

func (b *Bridge) send(frame []byte, end bool) {
	s := b.active
	if b.Out != nil {
		b.Out(Packet{Source: s.source(), Dest: s.dest(), Seq: s.seq, End: end, Frame: frame})
	}
	s.seq = (s.seq + 1) & 0x7F
}

var spaces = [10]byte{' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}

func (s *session) block(i int) [20]byte {
	if s.csd.set[i] {
		b := s.csd.data[i]
		// A radio's destination is "**********" (everyone) or "*****" and
		// a radio ID. WIRES-X sometimes sends the room and an ID instead
		// ("12345ABCDE"), and Yaesu radios show no callsigns for those, so
		// they go out addressed to everyone.
		if i == 0 && string(b[:5]) != "*****" {
			copy(b[:10], "**********")
		}
		return b
	}
	var b [20]byte
	copy(b[:], spaces[:])
	copy(b[10:], spaces[:])
	if i == 0 {
		copy(b[:], "**********")
	}
	return b
}

// source is the transmitting station, CSD1's second half.
func (s *session) source() [10]byte {
	var c [10]byte
	b := s.block(0)
	copy(c[:], b[10:])
	if c == spaces {
		copy(c[:], "UNKNOWN   ")
	}
	return c
}

func (s *session) dest() [10]byte {
	var c [10]byte
	copy(c[:], "ALL       ")
	return c
}

// noGPS is the DT record a Yaesu radio sends without a position: length 7,
// type 22 61, 5F, radio model (33, FT-5D), end marker 03, and the 8-bit sum
// of the bytes before it. A radio shows no callsigns for a transmission whose
// DT doesn't hold a valid record, so this stands in when WIRES-X sends none.
var noGPS = [20]byte{0x07, 0x22, 0x61, 0x5F, 0x33, 0x03, 0x1F}

// dtRecord returns the length of the record at the start of dt if it is
// valid: a length byte, an 03 end marker and the 8-bit sum of the bytes
// before the sum.
func dtRecord(dt [20]byte) (int, bool) {
	n := int(dt[0])
	if n < 3 || n > len(dt) || dt[n-2] != 0x03 {
		return 0, false
	}
	var sum byte
	for _, b := range dt[:n-1] {
		sum += b
	}
	return n, sum == dt[n-1]
}

// record returns the length of the DT record WIRES-X sent, if it is valid.
func (s *session) record() (int, bool) {
	if !s.csd.dtSet {
		return 0, false
	}
	return dtRecord(s.csd.dt)
}

// dt is the DT data of frames 6 (and 7): the record from WIRES-X if it is
// valid and fits the frame total, otherwise noGPS.
func (s *session) dt() [20]byte {
	if n, ok := s.record(); ok && n <= 10*int(s.ft-5) {
		return s.csd.dt
	}
	return noGPS
}

// dch is the data channel of frame number fn: the callsign data blocks in
// halves for 0-5, and the DT record in 6 and 7.
func (s *session) dch(fn byte) [10]byte {
	var d [10]byte
	if fn > 5 {
		dt := s.dt()
		copy(d[:], dt[10*(fn-6):10*(fn-6)+10])
		return d
	}
	b := s.block(int(fn / 2))
	copy(d[:], b[10*(fn%2):10*(fn%2)+10])
	return d
}

func (s *session) fich(fi, fn byte) ysf.FICH {
	return ysf.FICH{FI: fi, CS: 2, FN: fn, FT: s.ft, DT: ysf.DTVDMode2}
}

func trim(c [10]byte) string { return string(bytes.TrimRight(c[:], " ")) }
