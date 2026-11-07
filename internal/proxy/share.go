package proxy

import (
	"time"

	"github.com/lweith/hri200-ysf/internal/bridge"
	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/ysf"
)

// wiresxHold keeps the channel busy this long after WIRES-X last sent the
// radio voice or a header, unless the radio has since reported transmitting
// and gone idle again. The HRI-200 can take over a second to report a
// transmission after acking its header.
const wiresxHold = 3 * time.Second

// Sharer plays YSF from clients to WIRES-X while the HRI-200 is attached, as
// the emulator does when it is absent, but only while the repeater is neither
// receiving nor transmitting. The repeater wins: a reception that starts on the
// air ends the injected one. A client's over that finds the channel busy, or
// loses it, waits and starts late, from its header, once the channel is free.
type Sharer struct {
	Log func(format string, args ...any)
	// Radio, if set, writes a message to the HRI-200, and clients' overs are
	// transmitted on the repeater too: see share_relay.go.
	Radio func(text string)
	// Write, if set, writes a message to the HRI-200 for an analog
	// transmission keyed by PTT: see share_analog.go.
	Write func(text string)
	// Sessions, if set, counts the AX.25 sessions open on the channel: room
	// traffic stays off the repeater while there are any (share_sock.go).
	Sessions func(now time.Time) int

	emu      Emulator
	radio    hri.State // as the HRI-200 last reported it
	wiresxAt time.Time // when WIRES-X last sent the radio voice or a header
	txSeen   bool      // the radio has reported transmitting since wiresxAt
	header   []byte    // the current client over's header frame, if seen
	waiting  bool      // the current client over is waiting for the channel
	lastYSF  time.Time

	wx     wxState     // WIRES-X commands from clients: see share_wiresx.go
	relay  relayState  // clients' overs on the repeater: see share_relay.go
	analog analogState // packet modem's transmissions: see share_analog.go
	sock   sockState   // room traffic kept off a packet session: see share_sock.go
}

func (s *Sharer) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

// Relaying reports whether a client's over is being transmitted on the
// repeater, until the radio reports the transmission over.
func (s *Sharer) Relaying() bool { return s.relay.on }

// Injecting reports whether a client's over is being played to WIRES-X.
func (s *Sharer) Injecting() bool { return s.emu.rx != nil }

func (s *Sharer) busy(now time.Time) bool {
	if s.analogTX() {
		return true // left in Analog between packets, clients still reach WIRES-X
	}
	if s.radio != "" && !s.radio.Idle() {
		return true
	}
	return s.wiresxBusy(now)
}

// wiresxBusy reports whether WIRES-X has, or may be about to take, the radio.
func (s *Sharer) wiresxBusy(now time.Time) bool {
	if s.wx.replying() {
		return true
	}
	return now.Sub(s.wiresxAt) < wiresxHold && !s.txSeen
}

// FromWIRESX notes a message from WIRES-X on its way to the HRI-200. It
// reports whether to pass it on, and what to answer WIRES-X instead if not.
func (s *Sharer) FromWIRESX(text string, now time.Time) (bool, []Out) {
	if taken, out := s.analogFromWIRESX(text, now); taken {
		return false, out
	}
	if out, taken := s.wiresxReply(text, now); taken {
		return false, out
	}
	if taken, out := s.relayPoll(text, now); taken {
		return false, out
	}
	if taken, out := s.sockFromWIRESX(text, now); taken {
		return false, out
	}
	switch hri.Key(text) {
	case "D1E", "D1F":
		if hri.IsAck(text) {
			return true, nil
		}
		s.learnDownlink(text)
		s.relayStop()
		s.wiresxAt = now
		s.txSeen = false
		if s.emu.rx != nil {
			// As the emulator does: WIRES-X has taken the channel. The
			// HRI-200 reports the transmission's states itself.
			s.logf("proxy: WIRES-X transmitting, holding the injected over")
			s.emu.rx = nil
			s.waiting = true
		}
	}
	return true, nil
}

// FromRadio passes on a message from the HRI-200, rewritten or held back while
// a client's over is being played so WIRES-X sees one consistent radio.
func (s *Sharer) FromRadio(text string, now time.Time) []Out {
	if out, taken := s.analogFromRadio(text, now); taken {
		return out
	}
	if out, taken := s.relayFromRadio(text, now); taken {
		return out
	}
	if m, ok := hri.Parse(text); ok {
		if st, ok := hri.ParseState(m); ok {
			s.radio = st
			if !st.Idle() && !st.Receiving() {
				s.txSeen = true
			}
			if st.Receiving() && st[4] == '7' { // analog: see usage.go
				s.packetActivity(now)
			}
		}
	}
	if s.sockFromRadio(text) {
		return nil
	}
	if s.wx.replying() {
		return s.fromRadioReplying(text, now)
	}
	if s.emu.rx == nil {
		return []Out{{text, now}}
	}
	switch k := hri.Key(text); {
	case k == "B" && len(text) >= 2:
		return []Out{{"B1" + text[2:], now}} // busy: receiving
	case k == "D1C":
		return []Out{{hri.D1('C', hri.StatePayload(s.emu.current())), now}}
	case k == "D1P" && s.radio.Idle():
		return nil // the HRI-200 idles while the injected over plays
	case k == "D1P", k == "D1H", k == "D1R", k == "D1G":
		// The repeater has the channel: end the injected over first.
		s.logf("proxy: repeater busy, holding the injected over")
		s.waiting = true
		out := s.emu.endRx(now)
		s.relaySync()
		return append(out, Out{text, now})
	}
	return []Out{{text, now}}
}

// FromClient plays a client's frame to WIRES-X if the channel is free.
func (s *Sharer) FromClient(y YSF, now time.Time) []Out {
	s.lastYSF = now
	f, err := ysf.DecodeFICH(y.Frame)
	if err == nil && f.DT == ysf.DTDataFRMode {
		return s.commandFrame(y, now)
	}
	if s.emu.rx == nil && err == nil && f.FI == ysf.FIHeader {
		s.header = y.Frame
	}
	defer func() {
		if y.End {
			s.header, s.waiting = nil, false
		}
	}()
	if s.emu.rx == nil && s.busy(now) {
		if !s.waiting {
			s.logf("proxy: channel busy, holding %q's over", string(y.Source[:]))
			s.waiting = true
		}
		return nil
	}
	var out []Out
	if s.waiting {
		s.waiting = false
		if s.header != nil {
			s.logf("proxy: channel free, starting %q's over late", string(y.Source[:]))
			out = s.emu.Inject(s.header, y.Source, false, now)
			s.relayFrom(out, now)
		}
	}
	more := s.emu.Inject(y.Frame, y.Source, y.End, now)
	s.relayFrom(more, now)
	s.relaySync()
	return append(out, more...)
}

// Tick ends an injected over whose frames have stopped, forgets a waiting
// over that stopped without an end, and moves WIRES-X commands and replies
// along. It returns messages for WIRES-X and frames for the clients.
func (s *Sharer) Tick(now time.Time) ([]Out, []bridge.Packet) {
	if s.emu.rx == nil && now.Sub(s.lastYSF) > rxHang {
		s.header, s.waiting = nil, false
	}
	out := s.emu.Tick(now)
	s.sockTick()
	s.relaySync()
	s.relayTick(now)
	out = append(out, s.analogTick(now)...)
	wxOut, packets := s.wiresxTick(now)
	return append(out, wxOut...), packets
}

// End ends an injected over, for when the HRI-200 goes away.
func (s *Sharer) End(now time.Time) []Out {
	s.relayStop()
	if s.emu.rx == nil {
		return nil
	}
	return s.emu.endRx(now)
}
