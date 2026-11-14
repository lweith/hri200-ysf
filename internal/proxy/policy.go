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
	// while packet is in use (router.go packetHolds).
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

// withDefaults returns the policy with unset fields at their defaults.
func (p Policy) withDefaults() Policy {
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
