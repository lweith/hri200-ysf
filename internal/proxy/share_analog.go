package proxy

import (
	"strings"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
)

// Analog transmission for a local packet modem (direwolf), keyed by PTT. The
// DR-1X in AMS transmits only digital voice while its WIRES Operation is
// Digital. Switched to Analog, it keys up in FM with the codec's playback
// audio when polled with pttOn (radio state "00002"), which WIRES-X never
// asks for. So on PTT the proxy switches the radio to Analog, keys it, and
// on PTT release unkeys it and switches it back to Digital.
//
// The switch is WIRES-X's own: a D1M whose fields are all "F" (leave alone)
// but WIRES Operation (payload offset 67, "0" Analog, "1" Digital). The radio
// echoes it with its full settings about 75 ms later, then reports "20000" and
// "00000" (captured 2026-11-06: settled 116 ms after sending; keyed 23 ms
// after pttOn).
//
// While the proxy has the radio, WIRES-X sees an idle radio: the echoes, the
// transmit states and the answers to the proxy's own polls are kept from it,
// its status queries are answered idle, and its unkeyed polls go to the radio
// as pttOn. What it sends the radio meanwhile (a transmission, a settings
// write) waits until the radio is back in Digital. PTT that finds the channel
// busy is refused: the packet's audio goes nowhere and the sender retries.
const (
	analogMode  = '0'
	digitalMode = '1'

	// switchWait keys anyway if the radio hasn't reported the switch.
	switchWait = 500 * time.Millisecond
	// unkeyHang waits this long after PTT release before switching back, so
	// that back-to-back transmissions stay in Analog.
	unkeyHang = 300 * time.Millisecond
	// restoreWait resends the switch back to Digital if the radio hasn't
	// echoed it, up to restoreTries times.
	restoreWait  = time.Second
	restoreTries = 3
	// settleTail keeps the radio's states from WIRES-X this long after the
	// switch back is echoed: they follow the echo (seen 5 and 40 ms after).
	settleTail = 200 * time.Millisecond
	// maxAnalogTX unkeys a PTT held this long (a stuck modem).
	maxAnalogTX = 15 * time.Second
	// maxHeld bounds what WIRES-X sends while the proxy has the radio.
	maxHeld = 256
)

// SetMode is the D1M that sets only the radio's WIRES Operation.
func SetMode(mode byte) string {
	return hri.D1('M', strings.Repeat("F", 67)+string(mode)+"F"+strings.Repeat(" ", 10)+strings.Repeat("F", 28))
}

type analogPhase int

const (
	aIdle      analogPhase = iota
	aSwitching             // SetMode(analog) sent
	aKeyed                 // pttOn sent
	aHang                  // unkeyed, PTT may come back
	aRestoring             // SetMode(digital) sent
	aSettling              // echoed; the radio's states follow
)

type analogState struct {
	phase   analogPhase
	ptt     bool      // as the modem last set it
	refused bool      // this PTT was refused: wait for its release
	at      time.Time // the phase started
	echoed  bool      // the radio has echoed the switch to Analog
	keyedAt time.Time
	pttAt   time.Time // the last pttOn
	tries   int       // SetMode(digital) sent this many times
	echoes  int       // D1M echoes owed (to keep from WIRES-X)
	dirty   bool      // the radio may be in Analog
	held    []string  // from WIRES-X, for the radio once back in Digital
}

// Analog reports whether the proxy has the radio for an analog transmission.
func (s *Sharer) Analog() bool { return s.analog.phase != aIdle }

// AnalogDirty reports whether the radio may have been left in Analog.
func (s *Sharer) AnalogDirty() bool { return s.analog.dirty }

func (s *Sharer) toRadio(text string) {
	if s.Write != nil {
		s.Write(text)
	}
}

// analogPoll sends the radio a poll whose answer WIRES-X must not see.
func (s *Sharer) analogPoll(text string, now time.Time) {
	s.relay.bOwed++
	if text == pttOn {
		s.analog.pttAt = now
	}
	s.toRadio(text)
}

func (s *Sharer) setMode(mode byte, now time.Time) {
	a := &s.analog
	a.echoes++
	a.at = now
	if mode == analogMode {
		a.dirty = true
	}
	s.toRadio(SetMode(mode))
}

// PTT takes the modem's PTT.
func (s *Sharer) PTT(on bool, now time.Time) []Out {
	a := &s.analog
	if on == a.ptt {
		return nil
	}
	a.ptt = on
	if !on {
		a.refused = false
		if a.phase == aSwitching || a.phase == aKeyed {
			s.analogUnkey(now)
		}
		return nil
	}
	if s.Write == nil {
		s.logf("proxy: PTT, but no radio to transmit on")
		a.refused = true
		return nil
	}
	switch a.phase {
	case aHang: // back to back: still in Analog
		s.analogKey(now)
		return nil
	case aIdle:
	default:
		s.logf("proxy: PTT while switching back to Digital, refused")
		a.refused = true
		return nil
	}
	if why := s.analogBusy(now); why != "" {
		s.logf("proxy: PTT refused, channel busy (%s)", why)
		a.refused = true
		return nil
	}
	s.logf("proxy: PTT: switching the repeater to Analog")
	a.phase, a.echoed = aSwitching, false
	s.setMode(analogMode, now)
	return nil
}

func (s *Sharer) analogBusy(now time.Time) string {
	switch {
	case s.relay.on:
		return "transmitting a client's over"
	case s.emu.rx != nil:
		return "playing a client's over to WIRES-X"
	case s.radio.Receiving():
		return "receiving"
	case s.busy(now):
		return "WIRES-X or the repeater"
	}
	return ""
}

func (s *Sharer) analogKey(now time.Time) {
	a := &s.analog
	a.phase, a.keyedAt = aKeyed, now
	s.logf("proxy: keying the repeater in Analog (%d ms after PTT)", now.Sub(a.at).Milliseconds())
	s.analogPoll(pttOn, now)
	s.analogPoll(pttOn, now)
}

func (s *Sharer) analogUnkey(now time.Time) {
	a := &s.analog
	if a.phase == aKeyed {
		s.analogPoll(pttOff, now)
		s.analogPoll(pttOff, now)
	}
	a.phase, a.at = aHang, now
}

// analogFromWIRESX takes WIRES-X's messages while the proxy has the radio. It
// reports whether it took text, and what to answer WIRES-X.
func (s *Sharer) analogFromWIRESX(text string, now time.Time) (bool, []Out) {
	a := &s.analog
	if a.phase == aIdle {
		return false, nil
	}
	switch k := hri.Key(text); {
	case text == pttOff && a.phase == aKeyed:
		// WIRES-X's idle poll would unkey the radio; it gets the answer.
		a.pttAt = now
		s.toRadio(pttOn)
		return true, nil
	case k == "P":
		return false, nil // its answer goes to WIRES-X
	case k == "D1C":
		return true, []Out{{hri.D1('C', hri.StatePayload("04000")), now}}
	case (k == "D1E" || k == "D1F") && hri.IsAck(text):
		return false, nil // acks a reception
	case k == "D1E", k == "D1F", k == "D1M", k == "D1S", k == "D1T":
		if len(a.held) == 0 {
			s.logf("proxy: holding WIRES-X's %s until the repeater is back in Digital", k)
		}
		if len(a.held) < maxHeld {
			a.held = append(a.held, text)
		}
		return true, nil
	}
	return false, nil
}

// analogFromRadio takes the radio's answers to the proxy's switches and keying.
// It reports whether it took text, and what to tell WIRES-X instead.
func (s *Sharer) analogFromRadio(text string, now time.Time) ([]Out, bool) {
	a := &s.analog
	k := hri.Key(text)
	if k == "D1M" && a.echoes > 0 {
		a.echoes--
		m, ok := hri.Parse(text)
		mode := byte('?')
		if ok && len(m.Payload) > 67 {
			mode = m.Payload[67]
		}
		switch {
		case a.phase == aSwitching:
			a.echoed = true
			if mode != analogMode {
				s.logf("proxy: the repeater echoed WIRES Operation %q, not Analog", mode)
			}
		case a.phase == aRestoring && mode == digitalMode:
			s.logf("proxy: repeater back in Digital")
			a.phase, a.at, a.dirty = aSettling, now, false
		}
		return nil, true
	}
	if a.phase == aIdle {
		return nil, false
	}
	m, ok := hri.Parse(text)
	if !ok {
		return nil, false
	}
	st, ok := hri.ParseState(m)
	if !ok {
		return nil, false
	}
	if st.Receiving() && a.phase != aKeyed {
		// Kept from WIRES-X: it sees the reception once Digital is back.
		return nil, true
	}
	if a.phase == aSwitching && a.echoed && st == "00000" {
		s.analogKey(now)
	}
	if k == "D1P" {
		return nil, true
	}
	return []Out{{hri.D1('C', hri.StatePayload("04000")), now}}, true
}

// analogTick moves the switch along on time, and hands WIRES-X back the radio.
func (s *Sharer) analogTick(now time.Time) []Out {
	a := &s.analog
	switch a.phase {
	case aSwitching:
		if a.echoed && now.Sub(a.at) >= 150*time.Millisecond || now.Sub(a.at) >= switchWait {
			if !a.echoed {
				s.logf("proxy: the repeater never echoed the switch to Analog, keying anyway")
			}
			s.analogKey(now)
		}
	case aKeyed:
		switch {
		case now.Sub(a.keyedAt) >= maxAnalogTX:
			s.logf("proxy: PTT held %v, unkeying", maxAnalogTX)
			s.analogUnkey(now)
			a.refused = true // until the modem lets go
		case now.Sub(a.pttAt) >= pttEvery:
			s.analogPoll(pttOn, now)
		}
	case aHang:
		if now.Sub(a.at) >= unkeyHang {
			a.phase, a.tries = aRestoring, 1
			s.setMode(digitalMode, now)
		}
	case aRestoring:
		if now.Sub(a.at) >= restoreWait {
			if a.tries >= restoreTries {
				s.logf("proxy: the repeater never confirmed Digital after %d tries; it may be left in Analog", a.tries)
				return s.analogDone(now)
			}
			a.tries++
			s.logf("proxy: no echo of the switch back to Digital, resending")
			s.setMode(digitalMode, now)
		}
	case aSettling:
		if now.Sub(a.at) >= settleTail {
			return s.analogDone(now)
		}
	}
	return nil
}

// analogDone gives WIRES-X the radio back, with what it sent meanwhile.
func (s *Sharer) analogDone(now time.Time) []Out {
	a := &s.analog
	held := a.held
	*a = analogState{ptt: a.ptt, refused: a.ptt, dirty: a.dirty}
	s.radio = "04000" // its next status reply says otherwise if not
	var out []Out
	for _, text := range held {
		pass, answer := s.FromWIRESX(text, now)
		out = append(out, answer...)
		if pass {
			s.toRadio(text)
		}
	}
	return out
}

// ForceDigital switches the radio back to Digital, for a radio that may have
// been left in Analog (the HRI-200 went away mid-transmission).
func (s *Sharer) ForceDigital(now time.Time) {
	a := &s.analog
	if a.phase != aIdle || s.Write == nil {
		return
	}
	s.logf("proxy: the repeater may have been left in Analog, switching it back")
	a.phase, a.tries, a.dirty = aRestoring, 1, true
	s.analogPoll(pttOff, now)
	s.setMode(digitalMode, now)
}
