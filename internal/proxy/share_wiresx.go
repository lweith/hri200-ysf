package proxy

import (
	"time"

	"github.com/lweith/hri200-ysf/internal/bridge"
	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/wiresx"
)

// A client's WIRES-X button. Its command is played to WIRES-X as the radio
// plays a command it hears off the air (captured from an FT-5D on the
// repeater): receive states, the command as a D1G with no voice, idle states.
// WIRES-X acks the D1G, asks the radio whether it may send (D1B "0", answered
// "1"), and sends its reply as a D1F, which the radio acks and transmits. While
// a reply is owed to a client, that exchange is answered here and the reply
// goes to the clients as Data FR frames, so the repeater never transmits it.
const (
	// commandWait drops a command that can't be played this long after it
	// arrived; the radio gives up on its own.
	commandWait = 5 * time.Second
	// replyWait is how long WIRES-X's reply is waited for. It answered a room
	// list request 12 s after it, a node info request after 1 s.
	replyWait = 20 * time.Second
	// frameTime is one YSF frame's air time.
	frameTime = 100 * time.Millisecond
)

// commandEnd is what the radio reports after a command: no voice followed.
var commandEnd = []hri.State{"04008", "24008", "04008", "04000"}

type wxState struct {
	asm      wiresx.Assembler
	command  *wiresx.Command // waiting for the channel
	since    time.Time       // when command arrived
	ackOwed  time.Time       // until when WIRES-X's ack of our D1G is ours
	replyDue time.Time       // until when WIRES-X's reply goes to the clients
	txUntil  time.Time       // the reply's air time, reported to WIRES-X as transmitting
	queue    []timedPacket   // reply frames for the clients
	seq      byte
}

type timedPacket struct {
	at time.Time
	p  bridge.Packet
}

// replying reports whether a reply is going out to the clients, during which
// the radio is presented as transmitting it.
func (w *wxState) replying() bool { return !w.txUntil.IsZero() }

// commandFrame takes a client's Data FR frame.
func (s *Sharer) commandFrame(y YSF, now time.Time) []Out {
	c, ok := s.wx.asm.Frame(y.Frame)
	if !ok {
		return nil
	}
	s.logf("proxy: WIRES-X %s from %q", wiresx.Name(c.Data), string(y.Source[:]))
	s.wx.command, s.wx.since = &c, now
	return s.playCommand(now)
}

// playCommand plays a waiting command to WIRES-X if the channel is free.
func (s *Sharer) playCommand(now time.Time) []Out {
	c := s.wx.command
	if c == nil {
		return nil
	}
	if now.Sub(s.wx.since) > commandWait {
		s.logf("proxy: channel busy, dropping the WIRES-X %s", wiresx.Name(c.Data))
		s.wx.command = nil
		return nil
	}
	if s.emu.rx != nil || s.busy(now) || !s.wx.replyDue.IsZero() {
		return nil
	}
	s.wx.command = nil
	var out []Out
	at := now
	add := func(text string) {
		out = append(out, Out{text, at})
		at = at.Add(stateStep)
	}
	for _, st := range hri.RxStart {
		add(hri.D1('P', hri.StatePayload(st)))
	}
	add(hri.D1('G', hri.CommandPayload(c.CSD1, c.CSD2, c.CSD3, c.Data)))
	for _, st := range commandEnd {
		add(hri.D1('P', hri.StatePayload(st)))
	}
	s.wx.ackOwed = now.Add(3 * time.Second)
	s.wx.replyDue = now.Add(replyWait)
	return out
}

// wiresxReply answers the messages of WIRES-X's reply to a client's command,
// reporting whether it took text.
func (s *Sharer) wiresxReply(text string, now time.Time) ([]Out, bool) {
	k := hri.Key(text)
	if k == "D1G" && hri.IsAck(text) && now.Before(s.wx.ackOwed) {
		s.wx.ackOwed = time.Time{}
		return nil, true // acks our D1G; the HRI-200 never sent it
	}
	if s.wx.replyDue.IsZero() {
		return nil, false
	}
	if now.After(s.wx.replyDue) {
		s.logf("proxy: no WIRES-X reply")
		s.wx.replyDue = time.Time{}
		return nil, false
	}
	switch k {
	case "D1B":
		return []Out{{hri.D1('B', "1"), now}}, true // the radio may send
	case "D1F":
		m, ok := hri.Parse(text)
		if !ok {
			return nil, false
		}
		c, ok := hri.ParseCommand(m)
		if !ok || !wiresx.IsReply(c.Data) {
			return nil, false
		}
		s.wx.replyDue = time.Time{}
		frames := wiresx.Frames(wiresx.Command{CSD1: c.CSD1, CSD2: c.CSD2, CSD3: c.CSD3, Data: c.Data})
		s.logf("proxy: WIRES-X reply, %d bytes, to the clients in %d frames", len(c.Data), len(frames))
		var src, dst [10]byte
		copy(src[:], c.CSD1[10:])
		copy(dst[:], "ALL       ")
		at := now.Add(2 * frameTime)
		for i, f := range frames {
			s.wx.queue = append(s.wx.queue, timedPacket{at, bridge.Packet{
				Source: src, Dest: dst, Seq: s.wx.seq & 0x7F, End: i == len(frames)-1, Frame: f,
			}})
			s.wx.seq++
			at = at.Add(frameTime)
		}
		s.wx.txUntil = at
		out := []Out{{hri.Ack('F'), now}}
		for i, st := range hri.TxStart {
			out = append(out, Out{hri.D1('P', hri.StatePayload(st)), now.Add(time.Duration(i+1) * stateStep)})
		}
		return out, true
	}
	return nil, false
}

// fromRadioReplying passes on a message from the HRI-200 while the radio is
// presented as transmitting a reply: its idle states are hidden, and if the
// repeater starts receiving, the presented transmission ends first.
func (s *Sharer) fromRadioReplying(text string, now time.Time) []Out {
	switch k := hri.Key(text); {
	case k == "D1C":
		return []Out{{hri.D1('C', hri.StatePayload(hri.TxStart[len(hri.TxStart)-1])), now}}
	case k == "D1P" && s.radio.Idle():
		return nil
	case k == "D1P", k == "D1H", k == "D1R", k == "D1G":
		out := s.endReply(now) // the clients still get the rest of the reply
		return append(out, Out{text, now})
	}
	return []Out{{text, now}}
}

func (s *Sharer) endReply(now time.Time) []Out {
	s.wx.txUntil = time.Time{}
	var out []Out
	for i, st := range hri.TxEnd {
		out = append(out, Out{hri.D1('P', hri.StatePayload(st)), now.Add(time.Duration(i) * stateStep)})
	}
	return out
}

// wiresxTick plays a waiting command, ends a reply's presented transmission,
// and releases the reply frames that are due.
func (s *Sharer) wiresxTick(now time.Time) ([]Out, []bridge.Packet) {
	out := s.playCommand(now)
	if s.wx.replying() && !now.Before(s.wx.txUntil) {
		out = append(out, s.endReply(now)...)
	}
	var due []bridge.Packet
	for len(s.wx.queue) > 0 && !now.Before(s.wx.queue[0].at) {
		due = append(due, s.wx.queue[0].p)
		s.wx.queue = s.wx.queue[1:]
	}
	return out, due
}
