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
// asks for. So on PTT the master switches the radio to Analog, keys it, and
// on PTT release unkeys it and switches it back to Digital.
//
// The switch is WIRES-X's own: a D1M whose fields are all "F" (leave alone)
// but WIRES Operation (payload offset 67, "0" Analog, "1" Digital). The radio
// echoes it with its full settings about 75 ms later, then reports "20000" and
// "00000" (captured 2026-11-06: settled 116 ms after sending; keyed 23 ms
// after pttOn).
//
// WIRES-X knows none of this: it talks to the face, which shows it an idle
// radio throughout. PTT that finds the channel busy is refused: the packet's
// audio goes nowhere and the sender retries.
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
	// settleTail waits this long after the switch back is echoed before
	// anything else is transmitted: the radio's states follow the echo.
	settleTail = 200 * time.Millisecond
	// maxAnalogTX unkeys a PTT held this long (a stuck modem).
	maxAnalogTX = 15 * time.Second
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
	phase   analogPhase
	ptt     bool      // as the modem last set it
	pttOnAt time.Time // when it last went on
	refused bool      // this PTT was refused: wait for its release
	at      time.Time // the phase started
	echoed  bool      // the radio has echoed the switch to Analog
	keyedAt time.Time
	tries   int  // SetMode(digital) sent this many times
	echoes  int  // D1M echoes owed
	dirty   bool // the radio may be in Analog
}

func (a *analogState) keyed() bool { return a.phase == aKeyed }

// policy returns the master's policy, with unset fields at their defaults.
func (m *Master) policy() Policy { return m.Policy.withDefaults() }

// hangFor is the hang time to write with a switch to mode, "" for none: with
// HangZero, none while in Analog for packet, the repeater's own (saved in the
// cache first, so that a restart still owes it back) on the way back.
func (m *Master) hangFor(mode byte) string {
	c := m.Cache
	if mode != analogMode {
		return c.HangOwed()
	}
	if !m.HangZero {
		return ""
	}
	if c.HangOwed() == "" {
		cur := c.LastSQLT()
		if cur == "" || cur == "0000" {
			return "" // unknown, or none already: nothing to put back
		}
		c.OweHang(cur)
		m.logf("master: hang time %s ms, none while in Analog", cur)
	}
	return "0000"
}

func (m *Master) setMode(mode byte, now time.Time) {
	a := &m.analog
	a.echoes++
	a.at = now
	if mode == analogMode {
		a.dirty = true
	}
	m.send(ModeWrite(mode, m.hangFor(mode)), now)
}

// PacketKeyed reports whether the radio is keyed for the packet modem.
func (m *Master) PacketKeyed() bool { return m.analog.phase == aKeyed }

// AnalogDirty reports whether the radio may have been left in Analog.
func (m *Master) AnalogDirty() bool { return m.analog.dirty }

// packetActivity notes analog packet activity on the channel.
func (m *Master) packetActivity(now time.Time) { m.packetAt = now }

// PacketSession reports whether the channel is in use for packet: a session
// open, or analog packet activity within the policy's PacketQuiet.
func (m *Master) PacketSession(now time.Time) bool {
	if m.Sessions != nil && m.Sessions(now) > 0 {
		return true
	}
	return !m.packetAt.IsZero() && now.Sub(m.packetAt) < m.policy().PacketQuiet
}

// PTT takes the modem's PTT.
func (m *Master) PTT(on bool, now time.Time) {
	a := &m.analog
	if on == a.ptt {
		return
	}
	a.ptt = on
	if !on {
		a.refused = false
		if a.phase == aSwitching || a.phase == aKeyed {
			m.analogUnkey(now)
		}
		return
	}
	a.pttOnAt = now
	switch a.phase {
	case aHang: // back to back: still in Analog
		m.analogKey(now)
		return
	case aStay: // in Analog for the session: key an idle radio only
		if why := m.analogBusy(now); why != "" {
			m.logf("master: PTT refused, channel busy (%s)", why)
			a.refused = true
			return
		}
		m.packetActivity(now)
		m.analogKey(now)
		return
	case aIdle:
	default:
		m.logf("master: PTT while switching back to Digital, refused")
		a.refused = true
		return
	}
	if why := m.analogBusy(now); why != "" {
		m.logf("master: PTT refused, channel busy (%s)", why)
		a.refused = true
		return
	}
	m.logf("master: PTT: switching the repeater to Analog")
	a.phase, a.echoed = aSwitching, false
	m.packetActivity(now)
	m.setMode(analogMode, now)
}

// TXInhibited reports whether PTT would be refused now: the packet modem
// should hold its frames (TXINH) rather than send them into it.
func (m *Master) TXInhibited(now time.Time) bool {
	switch m.analog.phase {
	case aSwitching, aKeyed, aHang:
		return false // the modem has the radio, or may key back to back
	case aRestoring, aSettling:
		return true
	}
	return m.analogBusy(now) != ""
}

func (m *Master) analogBusy(now time.Time) string {
	switch {
	case !m.started || len(m.config) > 0 || m.cfgSent != "":
		return "starting"
	case m.health.sick:
		return "the radio is being recovered"
	case m.Transmitting():
		return "transmitting an over"
	case m.state.Receiving() || m.state.DigitalReceiving():
		return "receiving"
	case m.WIRESXBusy != nil && m.WIRESXBusy(now):
		return "WIRES-X"
	case m.state != "" && !m.state.Idle():
		// Including the repeater's hang after repeating an analog reception
		// ("04007", "06007", "03007" for the SQLT time, then idle). Switching
		// WIRES Operation and keying in it (2026-11-07 00:38:59 on) was
		// followed by the DR-1X reporting every transmission, analog and
		// digital, with nothing on the air until it was power-cycled. Only an
		// idle radio is switched: the modem waits out the hang (DWAIT).
		return "repeater in state " + string(m.state)
	}
	return ""
}

func (m *Master) analogKey(now time.Time) {
	a := &m.analog
	a.phase, a.keyedAt = aKeyed, now
	m.logf("master: keying the repeater in Analog (%d ms after PTT)", now.Sub(a.pttOnAt).Milliseconds())
	m.send(pttOn, now)
	m.send(pttOn, now)
	m.pollAt = now
}

func (m *Master) analogUnkey(now time.Time) {
	a := &m.analog
	if a.phase == aKeyed {
		m.send(pttOff, now)
		m.send(pttOff, now)
	}
	a.phase, a.at = aHang, now
}

// analogEcho takes the radio's echo of a settings write.
func (m *Master) analogEcho(text string, now time.Time) {
	a := &m.analog
	if a.echoes > 0 {
		a.echoes--
	}
	msg, ok := hri.Parse(text)
	if !ok || len(msg.Payload) <= 67 {
		return
	}
	mode := msg.Payload[67]
	switch {
	case a.phase == aSwitching:
		a.echoed = true
		if mode != analogMode {
			m.logf("master: the repeater echoed WIRES Operation %q, not Analog", mode)
		}
		return
	case mode != digitalMode || (a.phase != aRestoring && a.phase != aIdle):
		return
	}
	if a.phase == aRestoring {
		m.logf("master: repeater back in Digital")
		a.phase, a.at = aSettling, now
	}
	a.dirty = false
	if owed := m.Cache.HangOwed(); owed != "" {
		if len(msg.Payload) >= sqltAt+4 && msg.Payload[sqltAt:sqltAt+4] == owed {
			m.logf("master: hang time back to %s ms", owed)
			m.Cache.OweHang("")
		} else {
			m.logf("master: the repeater didn't take the hang time back (%s ms); written again next time", owed)
		}
	}
}

// analogState follows the radio's states through the switch.
func (m *Master) analogState(s hri.State, now time.Time) {
	if m.analog.phase == aSwitching && m.analog.echoed && s == "00000" {
		m.analogKey(now)
	}
}

// analogTick moves the switch along on time.
func (m *Master) analogTick(now time.Time) {
	a := &m.analog
	switch a.phase {
	case aSwitching:
		if a.echoed && now.Sub(a.at) >= 150*time.Millisecond || now.Sub(a.at) >= switchWait {
			if !a.echoed {
				m.logf("master: the repeater never echoed the switch to Analog, keying anyway")
			}
			m.analogKey(now)
		}
	case aKeyed:
		if now.Sub(a.keyedAt) >= maxAnalogTX {
			m.logf("master: PTT held %v, unkeying", maxAnalogTX)
			m.analogUnkey(now)
			a.refused = true // until the modem lets go
		}
	case aHang:
		if now.Sub(a.at) >= unkeyHang {
			if m.PacketSession(now) && len(m.next) == 0 {
				a.phase, a.at = aStay, now
				return
			}
			m.restore(now)
		}
	case aStay:
		if !m.PacketSession(now) && m.state.Idle() {
			m.logf("master: packet quiet for %v: switching the repeater back to Digital", m.policy().PacketQuiet)
			m.restore(now)
		}
	case aRestoring:
		if now.Sub(a.at) >= restoreWait {
			if a.tries >= restoreTries {
				m.logf("master: the repeater never confirmed Digital after %d tries; it may be left in Analog", a.tries)
				m.analogDone()
				return
			}
			a.tries++
			m.logf("master: no echo of the switch back to Digital, resending")
			m.setMode(digitalMode, now)
		}
	case aSettling:
		if now.Sub(a.at) >= settleTail {
			m.analogDone()
		}
	}
}

func (m *Master) restore(now time.Time) {
	m.analog.phase, m.analog.tries = aRestoring, 1
	m.setMode(digitalMode, now)
}

func (m *Master) analogDone() {
	a := &m.analog
	*a = analogState{ptt: a.ptt, refused: a.ptt, dirty: a.dirty, echoes: a.echoes}
}

// preemptAnalog returns the radio to Digital for a transmission waiting,
// when packet has it held in Analog between transmissions. A packet
// transmission already keyed is left to finish.
func (m *Master) preemptAnalog(now time.Time) {
	a := &m.analog
	if a.phase != aStay && a.phase != aHang {
		return
	}
	m.logf("master: a transmission is waiting: switching the repeater back to Digital for it")
	m.restore(now)
}
