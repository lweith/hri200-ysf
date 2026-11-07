package proxy

import (
	"strings"
	"testing"
	"time"
)

func policySharer(p Policy) (*Sharer, *[]string) {
	s, sent := analogSharer()
	s.Policy = p
	s.Sessions = func(time.Time) int { return 1 } // a packet session is open
	s.sock.packetAt = ms(0)
	return s, sent
}

// Under WIRES-X priority, an over during a packet session still reaches the
// radio: it is not muted or answered by the emulator.
func TestWIRESXPriorityPassesOverDuringPacket(t *testing.T) {
	s, _ := policySharer(Policy{Priority: PriorityWIRESX})
	if pass, _ := s.FromWIRESX(wxHeader(), ms(1000)); !pass {
		t.Fatal("WIRES-X priority: an over during a packet session was muted")
	}
}

// The default (packet priority) keeps the old behaviour: the same over is muted.
func TestPacketPriorityStillMutesOver(t *testing.T) {
	s, _ := policySharer(DefaultPolicy())
	if pass, _ := s.FromWIRESX(wxHeader(), ms(1000)); pass {
		t.Fatal("packet priority: an over during a packet session got through")
	}
}

// A packet hold in Analog gives way to WIRES-X's over: the radio goes back to
// Digital before the over is sent.
func TestWIRESXPreemptsAnalogHold(t *testing.T) {
	s, sent := policySharer(Policy{Priority: PriorityWIRESX})
	s.analog.phase = aStay
	s.FromWIRESX(wxHeader(), ms(1000))
	if len(*sent) == 0 || (*sent)[0] != wxSetDigital {
		t.Fatalf("expected a switch back to Digital first, sent %q", *sent)
	}
	if s.analog.phase != aRestoring {
		t.Fatalf("phase %v, want restoring", s.analog.phase)
	}
}

// Priority wiresx with no packet activity: an over passes, and nothing is
// switched.
func TestWIRESXPriorityIdleChannel(t *testing.T) {
	s, sent := analogSharer()
	s.Policy = Policy{Priority: PriorityWIRESX}
	if pass, _ := s.FromWIRESX(wxHeader(), ms(1000)); !pass {
		t.Fatal("idle channel: over was not passed")
	}
	for _, m := range *sent {
		if strings.HasPrefix(m, "D1M") {
			t.Fatalf("idle channel: unexpected mode switch %q", m)
		}
	}
}

func TestPolicyValidate(t *testing.T) {
	if err := (Policy{Priority: "fred", PacketQuiet: time.Second, WIRESXQuiet: time.Second}).Validate(); err == nil {
		t.Error("bad priority accepted")
	}
	if err := (Policy{Priority: PriorityWIRESX, PacketQuiet: 0, WIRESXQuiet: time.Second}).Validate(); err == nil {
		t.Error("zero quiet time accepted")
	}
	if err := DefaultPolicy().Validate(); err != nil {
		t.Errorf("default policy rejected: %v", err)
	}
}
