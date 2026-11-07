package proxy

import (
	"strings"
	"testing"

	"github.com/lweith/hri200-ysf/internal/hri"
)

// The masked mode writes WIRES-X sent when switched to Analog and back
// (reply cache, 2026-11-07).
const (
	wxSetAnalog  = "D1M006BFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF0F          FFFFFFFFFFFFFFFFFFFFFFFFFFFF"
	wxSetDigital = "D1M006BFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF1F          FFFFFFFFFFFFFFFFFFFFFFFFFFFF"
	echoAnalog   = "D1M006BE000146.23500+000.000000210302310020146.83500+000.0000002103023100200W8HHF/R   0180002000012010000105020500"
	echoDigital  = "D1M006BE000146.23500+000.000000210302310020146.83500+000.0000002103023100210W8HHF/R   0180002000012010000105020500"
)

func TestSetModeIsWIRESXs(t *testing.T) {
	if got := SetMode(analogMode); got != wxSetAnalog {
		t.Errorf("SetMode(analog) = %q", got)
	}
	if got := SetMode(digitalMode); got != wxSetDigital {
		t.Errorf("SetMode(digital) = %q", got)
	}
}

func analogSharer() (*Sharer, *[]string) {
	var sent []string
	s := &Sharer{Write: func(text string) { sent = append(sent, text) }}
	s.FromRadio(idleP, ms(0))
	return s, &sent
}

// A transmission as the probe saw it (2026-11-07 00:00): echo 76 ms after the
// switch, 20000 and 00000 after it, 00002 once keyed.
func TestAnalogTransmission(t *testing.T) {
	s, sent := analogSharer()
	s.PTT(true, ms(1000))
	if len(*sent) != 1 || (*sent)[0] != wxSetAnalog {
		t.Fatalf("on PTT: sent %q, want the switch to Analog", *sent)
	}
	for _, r := range []struct {
		at   int
		text string
	}{{1076, echoAnalog}, {1080, stateP("20000")}} {
		if out := s.FromRadio(r.text, ms(r.at)); out != nil {
			t.Errorf("passed %q to WIRES-X", texts(out))
		}
	}
	if len(*sent) != 1 {
		t.Fatalf("keyed before the radio settled: %q", (*sent)[1:])
	}
	s.FromRadio(stateP("00000"), ms(1116))
	if len(*sent) != 3 || (*sent)[1] != pttOn || (*sent)[2] != pttOn {
		t.Fatalf("once settled: %q, want PTT on twice", (*sent)[1:])
	}
	// The answers to the proxy's polls and the transmit state stay hidden.
	for _, text := range []string{"B0 0    0000000", "B0 0    0000000", stateP("00002")} {
		if out := s.FromRadio(text, ms(1140)); out != nil {
			t.Errorf("passed %q to WIRES-X", texts(out))
		}
	}
	// WIRES-X's idle poll keeps the radio keyed, and it sees an idle radio.
	if pass, _ := s.FromWIRESX(pttOff, ms(1500)); pass {
		t.Error("passed WIRES-X's unkeyed poll to the keyed radio")
	}
	if (*sent)[len(*sent)-1] != pttOn {
		t.Errorf("for WIRES-X's poll: sent %q, want PTT on", (*sent)[len(*sent)-1])
	}
	if out := s.FromRadio("B0 0    0000000", ms(1510)); len(out) != 1 {
		t.Errorf("kept the answer to WIRES-X's own poll: %q", texts(out))
	}
	if pass, out := s.FromWIRESX("D1C0000", ms(1600)); pass || len(out) != 1 ||
		!strings.Contains(out[0].Text, "04000") {
		t.Errorf("WIRES-X's status query: pass %v, answer %q", pass, texts(out))
	}
	// Repeated while keyed.
	n := len(*sent)
	s.Tick(ms(2600))
	if len(*sent) != n+1 || (*sent)[n] != pttOn {
		t.Errorf("a second on: sent %q, want PTT on", (*sent)[n:])
	}
	// Released: unkeyed at once, back to Digital after the hang.
	n = len(*sent)
	s.PTT(false, ms(3000))
	if len(*sent) != n+2 || (*sent)[n] != pttOff {
		t.Fatalf("on release: sent %q, want PTT off twice", (*sent)[n:])
	}
	s.Tick(ms(3000 + 250))
	if len(*sent) != n+2 {
		t.Fatalf("switched back during the hang: %q", (*sent)[n+2:])
	}
	s.Tick(ms(3000 + 300))
	if (*sent)[len(*sent)-1] != wxSetDigital {
		t.Fatalf("after the hang: sent %q, want the switch to Digital", (*sent)[n+2:])
	}
	if !s.AnalogDirty() {
		t.Error("not dirty before the radio confirmed Digital")
	}
	s.FromRadio(echoDigital, ms(3374))
	s.FromRadio(stateP("20000"), ms(3379))
	s.FromRadio(stateP("00000"), ms(3414))
	if s.AnalogDirty() {
		t.Error("still dirty once the radio confirmed Digital")
	}
	s.Tick(ms(3374 + 200))
	if s.Analog() {
		t.Fatal("still holding the radio after settling")
	}
	if out := s.FromRadio(idleP, ms(4000)); len(out) != 1 {
		t.Errorf("after: kept the radio's state from WIRES-X")
	}
}

func TestAnalogHoldsWIRESXTransmission(t *testing.T) {
	s, sent := analogSharer()
	s.PTT(true, ms(0))
	s.FromRadio(echoAnalog, ms(76))
	s.FromRadio(stateP("00000"), ms(116))
	header := hri.D1('F', txPrefix+strings.Repeat(" ", 60)+"00")
	if pass, _ := s.FromWIRESX(header, ms(500)); pass {
		t.Fatal("passed WIRES-X's transmission to the radio in Analog")
	}
	if pass, _ := s.FromWIRESX(wxSetDigital, ms(600)); pass {
		t.Fatal("passed WIRES-X's settings write mid-transmission")
	}
	s.PTT(false, ms(1000))
	s.Tick(ms(1300))
	s.FromRadio(echoDigital, ms(1370))
	n := len(*sent)
	s.Tick(ms(1600))
	if len(*sent) != n+2 || (*sent)[n] != header || (*sent)[n+1] != wxSetDigital {
		t.Errorf("once back in Digital: sent %q, want WIRES-X's held messages", (*sent)[n:])
	}
	// The write's echo is WIRES-X's reply now.
	if out := s.FromRadio(echoDigital, ms(1680)); len(out) != 1 {
		t.Errorf("kept the reply to WIRES-X's own write")
	}
}

func TestAnalogRefusedWhenBusy(t *testing.T) {
	s, sent := analogSharer()
	s.FromRadio(stateP("0A038"), ms(0)) // receiving
	s.PTT(true, ms(10))
	if len(*sent) != 0 {
		t.Fatalf("receiving: sent %q", *sent)
	}
	s.FromRadio(idleP, ms(500))
	s.Tick(ms(600))
	if len(*sent) != 0 {
		t.Fatalf("a refused PTT went ahead later: %q", *sent)
	}
	s.PTT(false, ms(700))
	s.PTT(true, ms(800))
	if len(*sent) != 1 || (*sent)[0] != wxSetAnalog {
		t.Errorf("next PTT on an idle channel: sent %q", *sent)
	}
}

func TestAnalogBackToBack(t *testing.T) {
	s, sent := analogSharer()
	s.PTT(true, ms(0))
	s.FromRadio(echoAnalog, ms(76))
	s.FromRadio(stateP("00000"), ms(116))
	s.PTT(false, ms(1000))
	s.PTT(true, ms(1100))
	for _, text := range *sent {
		if text == wxSetDigital {
			t.Fatalf("switched back between back-to-back transmissions: %q", *sent)
		}
	}
	if (*sent)[len(*sent)-1] != pttOn {
		t.Errorf("rekeyed with %q, want PTT on", (*sent)[len(*sent)-1])
	}
}

func TestAnalogStuckPTT(t *testing.T) {
	s, sent := analogSharer()
	s.PTT(true, ms(0))
	s.FromRadio(echoAnalog, ms(76))
	s.FromRadio(stateP("00000"), ms(116))
	for t := 200; t < 16000; t += 50 {
		s.Tick(ms(t))
	}
	found := false
	for _, text := range *sent {
		if text == wxSetDigital {
			found = true
		}
	}
	if !found {
		t.Errorf("a PTT held 16 s: never switched back")
	}
}

func TestAnalogRestoreRetries(t *testing.T) {
	s, sent := analogSharer()
	s.ForceDigital(ms(0))
	for t := 50; t < 4000; t += 50 {
		s.Tick(ms(t))
	}
	n := 0
	for _, text := range *sent {
		if text == wxSetDigital {
			n++
		}
	}
	if n != restoreTries {
		t.Errorf("unanswered: switched back %d times, want %d", n, restoreTries)
	}
	if s.Analog() {
		t.Error("still holding the radio after giving up")
	}
	if !s.AnalogDirty() {
		t.Error("not dirty after giving up")
	}
}
