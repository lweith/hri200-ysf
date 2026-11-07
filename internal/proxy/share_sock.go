package proxy

import (
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
)

// The sock: room traffic stays off the repeater while packet is in use. A
// WIRES-X transmission that starts within packetQuiet of the last analog
// packet activity (an analog reception, or an analog transmission for the
// modem) is answered by the emulator, as if the radio transmitted it: acks at
// the air rate, transmit states, cached poll answers. Nothing of it reaches
// the radio, so a packet session isn't stepped on by the room, and WIRES-X and
// its hotspot clients carry on as usual. Its unkeyed polls still go to the
// radio (their answers kept from WIRES-X), so the HRI-200 is polled as ever.
//
// With Sharer.Sessions set (AX.25 sessions followed from direwolf, see
// ax25mon), it also holds for as long as any connected-mode session is open,
// however long its stations take between frames.
const packetQuiet = 60 * time.Second

type sockState struct {
	packetAt time.Time // the last analog packet activity
	muted    bool      // a WIRES-X transmission is going to the emulator
}

// packetActivity notes analog packet activity on the channel.
func (s *Sharer) packetActivity(now time.Time) { s.sock.packetAt = now }

func (s *Sharer) sockFromWIRESX(text string, now time.Time) (bool, []Out) {
	k := hri.Key(text)
	tx := (k == "D1F" || k == "D1E") && !hri.IsAck(text)
	if !s.sock.muted {
		if !tx {
			return false, nil
		}
		recent := !s.sock.packetAt.IsZero() && now.Sub(s.sock.packetAt) < packetQuiet
		sessions := 0
		if s.Sessions != nil {
			sessions = s.Sessions(now)
		}
		switch {
		case sessions > 0:
			s.logf("proxy: %d packet session(s) open: keeping WIRES-X's transmission off the repeater", sessions)
		case recent:
			s.logf("proxy: packet %d s ago: keeping WIRES-X's transmission off the repeater",
				int(now.Sub(s.sock.packetAt).Seconds()))
		default:
			return false, nil
		}
		s.sock.muted = true
		s.learnDownlink(text)
		return true, s.emu.Request(text, now)
	}
	switch {
	case tx, k == "D1C":
		return true, s.emu.Request(text, now)
	case text == pttOff:
		if s.Write == nil && s.Radio == nil {
			return false, nil // nothing else polls the radio: let this one
		}
		s.relay.bOwed++ // the radio's answer
		s.toRadioAny(text)
		return true, s.emu.Request(text, now)
	case k == "P":
		return true, s.emu.Request(text, now) // never key the radio
	}
	return false, nil
}

// sockFromRadio keeps the radio's states from WIRES-X while the emulator
// reports its transmission.
func (s *Sharer) sockFromRadio(text string) bool {
	return s.sock.muted && hri.Key(text) == "D1P"
}

func (s *Sharer) sockTick() {
	if s.sock.muted && !s.emu.Transmitting() {
		s.logf("proxy: WIRES-X's transmission over (kept off the repeater)")
		s.sock.muted = false
	}
}

// toRadioAny writes to the radio by whichever writer is set.
func (s *Sharer) toRadioAny(text string) {
	switch {
	case s.Write != nil:
		s.Write(text)
	case s.Radio != nil:
		s.Radio(text)
	}
}
