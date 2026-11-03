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
	set  [3]bool
	data [3][20]byte
}

type session struct {
	dir   Direction
	csd   csd
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
	case dir == Network && m.Cmd == 'F':
		h, ok := hri.ParseTxHeader(m)
		if !ok {
			return
		}
		b.setCSD(dir, 0, h.CSD1)
		b.setCSD(dir, 1, h.CSD2)
		b.setCSD(dir, 2, h.CSD3)
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

func (b *Bridge) voice(dir Direction, v hri.Voice, now time.Time) {
	if b.active != nil && b.active.dir != dir {
		b.end()
	}
	if b.active == nil {
		b.active = &session{dir: dir, csd: b.pending[dir], start: now}
		s := b.active
		b.logf("%s: start, from %q to %q", dir, trim(s.source()), trim(s.dest()))
		b.send(ysf.HeaderFrame(s.fich(ysf.FIHeader, 0), s.block(0), s.block(1)), false)
	}
	s := b.active
	s.last = now
	s.count++
	b.send(ysf.VDMode2Frame(s.fich(ysf.FICommunications, s.fn), s.dch(s.fn), v.Blocks), false)
	s.fn = (s.fn + 1) % 8
}

func (b *Bridge) end() {
	s := b.active
	b.send(ysf.HeaderFrame(s.fich(ysf.FITerminator, s.fn), s.block(0), s.block(1)), true)
	b.logf("%s: end, %d frames, %.1f s", s.dir, s.count, s.last.Sub(s.start).Seconds()+0.1)
	b.active = nil
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
		return s.csd.data[i]
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

// dch is the data channel of frame number fn: the callsign data blocks in
// halves for 0-5. Frames 6 and 7 carry data (such as position) that this
// bridge doesn't decode, so they are left blank.
func (s *session) dch(fn byte) [10]byte {
	var d [10]byte
	if fn > 5 {
		return spaces
	}
	b := s.block(int(fn / 2))
	copy(d[:], b[10*(fn%2):10*(fn%2)+10])
	return d
}

func (s *session) fich(fi, fn byte) ysf.FICH {
	return ysf.FICH{FI: fi, CS: 2, FN: fn, FT: 7, DT: ysf.DTVDMode2}
}

func trim(c [10]byte) string { return string(bytes.TrimRight(c[:], " ")) }
