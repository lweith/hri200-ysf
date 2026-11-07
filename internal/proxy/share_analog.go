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
func SetMode(mode byte) string { return ModeWrite(mode, "") }

// sqltAt is the hang time's offset in a D1M payload (4 digits, ms): what the
// repeater holds its transmitter up for after an analog reception, during
// which the proxy won't key it.
const sqltAt = 85

// ModeWrite sets the radio's WIRES Operation and, unless sqlt is "", its hang
// time. The mode must always be given: an F there is not "leave alone" to the
// DR-1X but Analog (2026-11-07 02:54:35, a hang-time write with it F left the
// repeater in Analog).
func ModeWrite(mode byte, sqlt string) string {
	if sqlt == "" {
		sqlt = "FFFF"
	}
	return hri.D1('M', strings.Repeat("F", 67)+string(mode)+"F"+strings.Repeat(" ", 10)+
		"FFFFFF"+sqlt+strings.Repeat("F", 18))
}

// hangFor is the hang time to write with a switch to mode, "" for none: with
// HangZero, none while in Analog for packet, the repeater's own (saved in the
// cache first, so that a restart still owes it back) on the way back.
func (s *Sharer) hangFor(mode byte) string {
	c := s.emu.Cache
	if mode != analogMode {
		return c.HangOwed()
	}
	if !s.HangZero {
		return ""
	}
	if c.HangOwed() == "" {
		cur := c.LastSQLT()
		if cur == "" || cur == "0000" {
			return "" // unknown, or none already: nothing to put back
		}
		c.OweHang(cur)
		s.logf("proxy: hang time %s ms, none while in Analog", cur)
	}
	return "0000"
}

type analogPhase int

const (
	aIdle      analogPhase = iota
	aSwitching             // SetMode(analog) sent
	aKeyed                 // pttOn sent
	aHang                  // unkeyed, PTT may come back
	aStay                  // unkeyed, left in Analog for the packet session
	aRestoring             // SetMode(digital) sent
	aSettling              // echoed; the radio's states follow
)

type analogState struct {
	phase        analogPhase
	ptt          bool      // as the modem last set it
	pttOnAt      time.Time // when it last went on
	refused      bool      // this PTT was refused: wait for its release
	at           time.Time // the phase started
	echoed       bool      // the radio has echoed the switch to Analog
	keyedAt      time.Time
	pttAt        time.Time // the last pttOn
	tries        int       // SetMode(digital) sent this many times
	echoes       int       // D1M echoes owed (to keep from WIRES-X)
	dirty        bool      // the radio may be in Analog
	forceDigital bool      // switch it back to Digital once idle (ForceDigital)
	held         []string  // from WIRES-X, for the radio once back in Digital
}

// Analog reports whether the proxy has the radio for an analog transmission.
func (s *Sharer) Analog() bool { return s.analog.phase != aIdle }

// analogTX reports whether the proxy is switching, keying or unkeying the
// radio: not when it is only left in Analog between a session's transmissions.
func (s *Sharer) analogTX() bool { return s.Analog() && s.analog.phase != aStay }

// packetSession reports whether the channel is in use for packet: a session
// open, or analog packet activity within packetQuiet. The radio stays in
// Analog, and room traffic off it, for as long.
func (s *Sharer) packetSession(now time.Time) bool {
	if s.Sessions != nil && s.Sessions(now) > 0 {
		return true
	}
	return !s.sock.packetAt.IsZero() && now.Sub(s.sock.packetAt) < s.policy().PacketQuiet
}

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
	s.toRadio(ModeWrite(mode, s.hangFor(mode)))
}

// PTT takes the modem's PTT.
func (s *Sharer) PTT(on bool, now time.Time) []Out {
	a := &s.analog
	if on == a.ptt {
		return nil
	}
	a.ptt = on
	if on {
		a.pttOnAt = now
	}
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
	case aStay: // in Analog for the session: key an idle radio only
		if why := s.analogBusy(now); why != "" {
			s.logf("proxy: PTT refused, channel busy (%s)", why)
			a.refused = true
			return nil
		}
		s.packetActivity(now)
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
	s.packetActivity(now)
	s.setMode(analogMode, now)
	return nil
}

// TXInhibited reports whether PTT would be refused now: the packet modem
// should hold its frames (TXINH) rather than send them into it.
func (s *Sharer) TXInhibited(now time.Time) bool {
	a := &s.analog
	if s.Write == nil {
		return true
	}
	switch a.phase {
	case aSwitching, aKeyed, aHang:
		return false // the modem has the radio, or may key back to back
	case aRestoring, aSettling:
		return true
	}
	if a.forceDigital {
		return true
	}
	return s.analogBusy(now) != ""
}

func (s *Sharer) analogBusy(now time.Time) string {
	switch {
	case s.relay.on:
		return "transmitting a client's over"
	case s.radio.Receiving():
		return "receiving"
	case s.wiresxBusy(now) || s.wiresxRecent(now):
		return "WIRES-X"
	case s.radio != "" && !s.radio.Idle():
		// Including the repeater's hang after repeating an analog reception
		// ("04007", "06007", "03007" for the SQLT time, then idle). Switching
		// WIRES Operation and keying in it (2026-11-07 00:38:59 on) was
		// followed by the DR-1X reporting every transmission, analog and
		// digital, with nothing on the air until it was power-cycled. Only an
		// idle radio is switched: the modem waits out the hang (DWAIT).
		return "repeater in state " + string(s.radio)
	}
	return ""
}

func (s *Sharer) analogKey(now time.Time) {
	a := &s.analog
	a.phase, a.keyedAt = aKeyed, now
	s.logf("proxy: keying the repeater in Analog (%d ms after PTT)", now.Sub(a.pttOnAt).Milliseconds())
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
	if a.phase == aStay {
		// Left in Analog between a session's transmissions, for minutes
		// perhaps: WIRES-X runs as usual, its transmissions to the sock and
		// its command replies to the clients (FromWIRESX goes on), but it
		// must not see or change the mode.
		switch k := hri.Key(text); k {
		case "D1C":
			// To the radio: its answer is the channel check's, rewritten for
			// WIRES-X in analogFromRadio. The radio only reports going idle
			// (04000) when asked.
			return false, nil
		case "D1M":
			if r, ok := s.emu.Cache.Reply(text); ok {
				return true, []Out{{r, now}} // as if in Digital
			}
			fallthrough
		case "D1S", "D1T":
			if len(a.held) < maxHeld {
				a.held = append(a.held, text)
			}
			return true, nil
		}
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
			if owed := s.emu.Cache.HangOwed(); owed != "" {
				if ok && len(m.Payload) >= sqltAt+4 && m.Payload[sqltAt:sqltAt+4] == owed {
					s.logf("proxy: hang time back to %s ms", owed)
					s.emu.Cache.OweHang("")
				} else {
					s.logf("proxy: the repeater didn't take the hang time back (%s ms); written again next time", owed)
				}
			}
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
	if a.phase == aStay {
		// The radio's own states, for the channel check; WIRES-X sees the
		// emulator's (idle, or a client's over being played to it).
		s.radio = st
		if st.Receiving() && st[4] == '7' {
			s.packetActivity(now)
		}
		if k == "D1P" {
			return nil, true
		}
		return []Out{{hri.D1('C', hri.StatePayload(s.emu.current())), now}}, true
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
	s.forceTick(now)
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
			if s.packetSession(now) {
				return s.analogStay(now)
			}
			a.phase, a.tries = aRestoring, 1
			s.setMode(digitalMode, now)
		}
	case aStay:
		if !s.packetSession(now) && s.radio.Idle() {
			s.logf("proxy: packet quiet for %v: switching the repeater back to Digital", s.policy().PacketQuiet)
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
	return s.release(held, now)
}

// analogStay leaves the radio in Analog for the packet session. What WIRES-X
// sent during the transmission goes on now: its room traffic to the sock.
func (s *Sharer) analogStay(now time.Time) []Out {
	a := &s.analog
	a.phase, a.at = aStay, now
	held := a.held
	a.held = nil
	return s.release(held, now)
}

// release hands WIRES-X's held messages on as if they had just arrived.
func (s *Sharer) release(held []string, now time.Time) []Out {
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
	s.logf("proxy: the repeater may have been left in Analog; switching it back once it is idle")
	a.forceDigital = true
}

// forceTick switches a radio that may be in Analog back once it is idle: never
// mid-transmission or in its hang.
func (s *Sharer) forceTick(now time.Time) {
	a := &s.analog
	if !a.forceDigital || a.phase != aIdle || !s.radio.Idle() {
		return
	}
	a.forceDigital = false
	a.phase, a.tries, a.dirty = aRestoring, 1, true
	s.setMode(digitalMode, now)
}
