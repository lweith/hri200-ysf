package proxy

import (
	"fmt"
	"time"
)

// Priority decides which traffic keeps the channel when WIRES-X and packet
// both want it.
type Priority string

const (
	// PriorityPacket: WIRES-X's overs wait out a packet session, which is the
	// behaviour before the policy existed. Room traffic stays off the repeater
	// while packet is in use (share_sock.go).
	PriorityPacket Priority = "packet"
	// PriorityWIRESX: WIRES-X's overs always reach the radio, and go back to
	// Digital first if packet has it in Analog. Packet waits until WIRES-X has
	// been quiet for WIRESXQuiet.
	PriorityWIRESX Priority = "wiresx"
)

// Policy is the channel's sharing rules, set from the command line.
type Policy struct {
	Priority    Priority
	PacketQuiet time.Duration // the radio stays in Analog this long after packet activity
	WIRESXQuiet time.Duration // packet waits this long after WIRES-X's last transmission
}

// DefaultPolicy is the behaviour the proxy had before the policy existed.
func DefaultPolicy() Policy {
	return Policy{Priority: PriorityPacket, PacketQuiet: packetQuiet, WIRESXQuiet: wiresxQuiet}
}

// Validate reports a policy that can't be run.
func (p Policy) Validate() error {
	switch p.Priority {
	case PriorityPacket, PriorityWIRESX:
	default:
		return fmt.Errorf("priority %q: want %q or %q", p.Priority, PriorityPacket, PriorityWIRESX)
	}
	if p.PacketQuiet <= 0 || p.WIRESXQuiet <= 0 {
		return fmt.Errorf("quiet times must be positive")
	}
	return nil
}

// policy returns the Sharer's policy, with unset fields at their defaults.
func (s *Sharer) policy() Policy {
	p := s.Policy
	d := DefaultPolicy()
	if p.Priority == "" {
		p.Priority = d.Priority
	}
	if p.PacketQuiet <= 0 {
		p.PacketQuiet = d.PacketQuiet
	}
	if p.WIRESXQuiet <= 0 {
		p.WIRESXQuiet = d.WIRESXQuiet
	}
	return p
}

// preemptAnalog returns the radio to Digital so WIRES-X's over can go out,
// when packet has it held in Analog between transmissions. A packet
// transmission already keyed is left to finish.
func (s *Sharer) preemptAnalog(now time.Time) {
	a := &s.analog
	if a.phase != aStay && a.phase != aHang {
		return
	}
	s.logf("proxy: WIRES-X transmitting: switching the repeater back to Digital for it")
	a.phase, a.tries = aRestoring, 1
	s.setMode(digitalMode, now)
}
