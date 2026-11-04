package proxy

import (
	"bytes"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/wiresx"
)

func field20(s string) (b [20]byte) {
	copy(b[:], s+strings.Repeat(" ", 20))
	return b
}

func withSum(b []byte) []byte {
	b = append(b, 0x03)
	var s byte
	for _, v := range b {
		s += v
	}
	return append(b, s)
}

// dxRequest is a node info request as the radio sends it.
var dxRequest = wiresx.Command{
	CSD1: field20("*****12345N0CALL"), CSD2: field20(""), CSD3: field20("               12345"),
	Data: withSum([]byte{0x7F, 0x5D, 0x71, 0x5F, '3', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}),
}

// dxReply is WIRES-X's D1F reply to it.
func dxReply() string {
	data := withSum(append([]byte{0x87, 0x5D, 0x51, 0x5F, 0x26}, bytes.Repeat([]byte(" "), 122)...))
	return hri.D1('F', "20211000"+"**********N0GW-RPT  "+"N0GW                "+"12345          12345"+
		strings.ToUpper(hex.EncodeToString(data)))
}

func sendCommand(s *Sharer, c wiresx.Command, start int) []string {
	var src [10]byte
	copy(src[:], "N0CALL    ")
	var out []string
	for i, f := range wiresx.Frames(c) {
		out = append(out, texts(s.FromClient(YSF{Source: src, Frame: f}, ms(start+100*i)))...)
	}
	return out
}

func TestSharerPlaysCommand(t *testing.T) {
	s := &Sharer{}
	s.FromRadio(idleP, ms(0))
	out := sendCommand(s, dxRequest, 10)
	var want []string
	for _, st := range hri.RxStart {
		want = append(want, stateP(st))
	}
	want = append(want, hri.D1('G', hri.CommandPayload(dxRequest.CSD1, dxRequest.CSD2, dxRequest.CSD3, dxRequest.Data)))
	for _, st := range commandEnd {
		want = append(want, stateP(st))
	}
	if !slices.Equal(out, want) {
		t.Fatalf("command ->\n%q\nwant\n%q", out, want)
	}
	if !strings.HasPrefix(out[len(hri.RxStart)], "D1G00662100107F") {
		t.Errorf("D1G %q: not as captured", out[len(hri.RxStart)])
	}
}

func TestSharerAnswersReply(t *testing.T) {
	s := &Sharer{}
	s.FromRadio(idleP, ms(0))
	sendCommand(s, dxRequest, 0)
	at := 1000

	if pass, _ := s.FromWIRESX(hri.Ack('G'), ms(at)); pass {
		t.Error("passed WIRES-X's ack of our D1G to the HRI-200")
	}
	if pass, ans := s.FromWIRESX(hri.D1('B', "0"), ms(at+900)); pass || len(ans) != 1 || ans[0].Text != "D1B00011" {
		t.Errorf("D1B: pass %v, answer %q", pass, texts(ans))
	}
	pass, ans := s.FromWIRESX(dxReply(), ms(at+950))
	if pass || len(ans) == 0 || ans[0].Text != hri.Ack('F') {
		t.Fatalf("reply: pass %v, answer %q", pass, texts(ans))
	}
	if got := texts(s.FromRadio(hri.D1('C', hri.StatePayload("04000")), ms(at+1000))); got[0] != "D1C000B00004R0OR40" {
		t.Errorf("status while replying = %q", got)
	}
	if got := s.FromRadio(idleP, ms(at+1000)); got != nil {
		t.Errorf("passed an idle state while replying: %q", texts(got))
	}

	var packets int
	var ended bool
	var end []string
	for now := at + 1000; now < at+3000; now += 50 {
		out, p := s.Tick(ms(now))
		for _, pk := range p {
			packets++
			ended = pk.End
		}
		end = append(end, texts(out)...)
	}
	if want := 7; packets != want || !ended {
		t.Errorf("%d reply frames, ended %v; want %d, true", packets, ended, want)
	}
	if len(end) == 0 || end[len(end)-1] != idleP {
		t.Errorf("transmission not ended: %q", end)
	}
	// Later acks and replies are WIRES-X's own business.
	if pass, _ := s.FromWIRESX(hri.Ack('G'), ms(at+5000)); !pass {
		t.Error("held back a later ack")
	}
	if pass, _ := s.FromWIRESX(dxReply(), ms(at+6000)); !pass {
		t.Error("took a reply nobody asked for")
	}
}

func TestSharerWIRESXOwnRepliesPass(t *testing.T) {
	s := &Sharer{}
	for _, m := range []string{hri.D1('B', "0"), dxReply(), hri.Ack('G')} {
		if pass, _ := s.FromWIRESX(m, ms(0)); !pass {
			t.Errorf("held back %.20q with no command outstanding", m)
		}
	}
}

func TestSharerCommandWaitsForChannel(t *testing.T) {
	s := &Sharer{}
	s.FromRadio(receivingP, ms(0))
	if out := sendCommand(s, dxRequest, 0); out != nil {
		t.Fatalf("played while the repeater receives: %q", out)
	}
	s.FromRadio(idleP, ms(1000))
	if out, _ := s.Tick(ms(1050)); len(out) == 0 {
		t.Error("not played once the channel was free")
	}

	s = &Sharer{}
	s.FromRadio(receivingP, ms(0))
	sendCommand(s, dxRequest, 0)
	s.FromRadio(idleP, ms(6000))
	if out, _ := s.Tick(ms(6050)); out != nil {
		t.Errorf("played a stale command: %q", texts(out))
	}
}
