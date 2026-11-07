package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
)

func sockSharer() (*Sharer, *[]string) {
	s, sent := analogSharer()
	s.emu.Cache = &Cache{Replies: map[string]string{pttOff: "B0 0    0000000", pttOn: "B0 0    0000000"}}
	return s, sent
}

func wxHeader() string { return hri.D1('F', txPrefix+strings.Repeat(" ", 60)+"00") }

func TestSockKeepsRoomOffAPacketSession(t *testing.T) {
	s, sent := sockSharer()
	s.FromRadio(stateP("0A017"), ms(0)) // a packet received
	s.FromRadio(stateP("04007"), ms(600))
	s.FromRadio(idleP, ms(1600))
	n := len(*sent)

	pass, out := s.FromWIRESX(wxHeader(), ms(5000))
	if pass {
		t.Fatal("passed WIRES-X's header to the radio 5 s after a packet")
	}
	if len(out) == 0 || out[0].Text != hri.Ack('F') {
		t.Fatalf("WIRES-X's header: answered %q, want an ack", texts(out))
	}
	pass, out = s.FromWIRESX("D1E"+strings.Repeat("0", 20), ms(5100))
	if pass || len(out) == 0 {
		t.Fatalf("WIRES-X's voice: pass %v, answer %q", pass, texts(out))
	}
	if pass, _ := s.FromWIRESX(pttOn, ms(5200)); pass {
		t.Fatal("WIRES-X's keyed poll reached the radio")
	}
	if pass, _ := s.FromWIRESX(pttOff, ms(5300)); pass {
		t.Fatal("WIRES-X's idle poll should go to the radio by the proxy, answered by the emulator")
	}
	for _, text := range (*sent)[n:] {
		if text != pttOff {
			t.Errorf("sent the radio %q during the muted transmission", text)
		}
	}
	// The radio's states are WIRES-X's no more; the emulator's are.
	if out := s.FromRadio(stateP("04000"), ms(5400)); out != nil {
		t.Errorf("passed the radio's state: %q", texts(out))
	}
	// It ends when the frames stop.
	s.Tick(ms(5100 + int(txHang/time.Millisecond) + 500))
	if s.sock.muted {
		t.Fatal("still muted after WIRES-X's frames stopped")
	}
}

func TestSockOnlyNearPacket(t *testing.T) {
	s, _ := sockSharer()
	if pass, _ := s.FromWIRESX(wxHeader(), ms(1000)); !pass {
		t.Fatal("muted WIRES-X with no packet activity")
	}
	s2, _ := sockSharer()
	s2.FromRadio(stateP("0A017"), ms(0))
	s2.FromRadio(idleP, ms(1000))
	if pass, _ := s2.FromWIRESX(wxHeader(), ms(1000+int(packetQuiet/time.Millisecond))); !pass {
		t.Fatal("muted WIRES-X a packetQuiet after the last packet")
	}
	s3, _ := sockSharer()
	s3.FromRadio(stateP("0A038"), ms(0)) // digital
	s3.FromRadio(idleP, ms(1000))
	if pass, _ := s3.FromWIRESX(wxHeader(), ms(3000)); !pass {
		t.Fatal("muted WIRES-X after a digital reception")
	}
}
