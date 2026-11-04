package proxy

import (
	"strings"
	"testing"

	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/ysf"
)

func stateP(s hri.State) string { return hri.D1('P', hri.StatePayload(s)) }

var (
	idleP      = stateP("04000")
	receivingP = stateP("0A010")
)

// clientOver is a client's header and n voice frames, as YSF.
func clientOver(n int) []YSF {
	var csd1, csd2 [20]byte
	copy(csd1[:], "*****12345N0CALL    ")
	var src [10]byte
	copy(src[:], "N0CALL    ")
	fich := ysf.FICH{FI: ysf.FIHeader, CS: 2, FT: 6, DT: ysf.DTVDMode2}
	out := []YSF{{Source: src, Frame: ysf.HeaderFrame(fich, csd1, csd2)}}
	fich.FI = ysf.FICommunications
	for i := range n {
		fich.FN = byte(i % 7)
		out = append(out, YSF{Source: src, Frame: ysf.VDMode2Frame(fich, [10]byte{}, [5][ysf.VoiceBlockLength]byte{})})
	}
	return out
}

func play(s *Sharer, frames []YSF, start int) []string {
	var out []string
	for i, y := range frames {
		out = append(out, texts(s.FromClient(y, ms(start+100*i)))...)
	}
	return out
}

// startsWithHeader reports whether out starts an injected reception with the
// client's header callsigns.
func startsWithHeader(out []string) bool {
	return len(out) > len(hri.RxStart) && out[0] == stateP(hri.RxStart[0]) &&
		strings.HasSuffix(out[len(hri.RxStart)], "*****12345N0CALL    ")
}

func TestSharerInjectsWhileIdle(t *testing.T) {
	s := &Sharer{}
	s.FromRadio(idleP, ms(0))
	if out := play(s, clientOver(3), 10); !startsWithHeader(out) {
		t.Fatalf("idle repeater: injected %q", out)
	}
	// WIRES-X must see a busy, receiving radio, not the HRI-200's idle replies.
	if got := texts(s.FromRadio("B0 0    0000000", ms(400))); got[0] != "B1 0    0000000" {
		t.Errorf("poll reply = %q", got)
	}
	if got := texts(s.FromRadio(hri.D1('C', hri.StatePayload("04000")), ms(400))); got[0] != "D1C000B0A038R0OR40" {
		t.Errorf("status reply = %q", got)
	}
	if got := s.FromRadio(stateP("05000"), ms(400)); got != nil {
		t.Errorf("passed the HRI-200's idle state: %q", texts(got))
	}
	if got := texts(s.FromRadio("R0303030", ms(400))); got[0] != "R0303030" {
		t.Errorf("other reply = %q", got)
	}
	end := texts(s.Tick(ms(1000)))
	if len(end) == 0 || end[len(end)-1] != idleP || s.Injecting() {
		t.Errorf("stopped frames ended with %q", end)
	}
}

func TestSharerStartsLateAfterRepeater(t *testing.T) {
	s := &Sharer{}
	s.FromRadio(receivingP, ms(0))
	over := clientOver(6)
	if out := play(s, over[:3], 10); out != nil {
		t.Fatalf("injected while the repeater receives: %q", out)
	}
	s.FromRadio(idleP, ms(250))
	// The rest of the over starts late, with the header's callsigns.
	if out := play(s, over[3:], 310); !startsWithHeader(out) {
		t.Errorf("late start: %q", out)
	}
}

func TestSharerHoldsForWIRESXTransmission(t *testing.T) {
	s := &Sharer{}
	s.FromRadio(idleP, ms(0))
	hdr := hri.D1('F', "21216000"+"**********N0GW      "+strings.Repeat(" ", 66))
	s.FromWIRESX(hdr, ms(0))
	// The HRI-200 acks the header but reports transmitting only 1.3 s later.
	if out := play(s, clientOver(2), 1200); out != nil {
		t.Fatalf("injected before the radio reported WIRES-X's transmission: %q", out)
	}
	for i, st := range append(append([]hri.State{}, hri.TxStart...), hri.TxEnd...) {
		s.FromRadio(stateP(st), ms(1300+10*i))
	}
	if out := play(s, clientOver(1)[1:], 1500); !startsWithHeader(out) {
		t.Errorf("after WIRES-X's transmission: %q", out)
	}
}

func TestSharerHoldExpires(t *testing.T) {
	s := &Sharer{}
	s.FromWIRESX(hri.D1('E', hri.VoicePayload(5, [5][13]byte{})), ms(0))
	if out := play(s, clientOver(1), 100); out != nil {
		t.Fatalf("injected while WIRES-X transmits: %q", out)
	}
	if out := play(s, clientOver(1), 3500); !startsWithHeader(out) {
		t.Errorf("hold never expired: %q", out)
	}
}

func TestSharerYieldsToWIRESX(t *testing.T) {
	s := &Sharer{}
	play(s, clientOver(2), 0)
	s.FromWIRESX(hri.D1('E', hri.VoicePayload(5, [5][13]byte{})), ms(300))
	if s.Injecting() {
		t.Error("still injecting after WIRES-X took the channel")
	}
}

func TestSharerRepeaterWins(t *testing.T) {
	s := &Sharer{}
	play(s, clientOver(2), 0)
	out := texts(s.FromRadio(receivingP, ms(250)))
	var want []string
	for _, st := range hri.RxEnd {
		want = append(want, stateP(st))
	}
	want = append(want, receivingP)
	if strings.Join(out, " ") != strings.Join(want, " ") {
		t.Errorf("repeater started ->\n%q\nwant\n%q", out, want)
	}
	if s.Injecting() {
		t.Error("still injecting")
	}
}

func TestSharerForgetsAbandonedOver(t *testing.T) {
	s := &Sharer{}
	s.FromRadio(receivingP, ms(0))
	play(s, clientOver(2), 10)
	s.Tick(ms(1000)) // the client stopped without an end
	s.FromRadio(idleP, ms(1000))
	// A voice frame of a later over whose header was lost: no stale header.
	out := play(s, clientOver(1)[1:], 1100)
	if len(out) == 0 || startsWithHeader(out) {
		t.Errorf("later over started with the abandoned header: %q", out)
	}
}
