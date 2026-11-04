package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
)

// relaySharer returns a Sharer that transmits clients' overs, and the
// messages it sends the radio.
func relaySharer() (*Sharer, *[]string) {
	var sent []string
	s := &Sharer{Radio: func(text string) { sent = append(sent, text) }}
	s.FromRadio(idleP, ms(0))
	return s, &sent
}

func TestRelayHeaderThenVoiceOnAcks(t *testing.T) {
	s, sent := relaySharer()
	over := clientOver(10)
	var d1r []string
	collect := func(out []string) {
		for _, o := range out {
			if hri.Key(o) == "D1R" {
				d1r = append(d1r, o)
			}
		}
	}
	collect(play(s, over[:2], 10)) // header and first voice frame
	if len(*sent) != 1 || !strings.HasPrefix((*sent)[0], "D1F0052"+txPrefix+"**********N0CALL") {
		t.Fatalf("sent %q, want one transmit header", *sent)
	}
	if got := s.FromRadio(hri.Ack('F'), ms(120)); got != nil {
		t.Errorf("passed the radio's ack to WIRES-X: %q", texts(got))
	}
	// Voice waits for a few frames to give the radio a buffer.
	collect(play(s, over[2:prebuffer], 210)) // over[0] is the header
	if len(*sent) != 1 {
		t.Fatalf("with %d frames queued: sent %q", prebuffer-1, (*sent)[1:])
	}
	collect(play(s, over[prebuffer:prebuffer+1], 210+100*prebuffer))
	if len(*sent) != 4 || (*sent)[1] != "D1E"+d1r[0][3:] {
		t.Fatalf("with %d frames queued: %q, want the first voice frame", prebuffer, (*sent)[1:])
	}
	// Then the transmitter is keyed, without waiting for the ack.
	if (*sent)[2] != pttOn || (*sent)[3] != pttOn {
		t.Fatalf("after the first voice frame: %q, want PTT on twice", (*sent)[2:])
	}
	// Voice goes one per ack once the radio transmits, the same payload
	// WIRES-X got as D1R.
	for _, st := range hri.TxStart {
		s.FromRadio(stateP(st), ms(780))
	}
	s.FromRadio(hri.Ack('E'), ms(800))
	if len(*sent) != 5 || (*sent)[4] != "D1E"+d1r[1][3:] {
		t.Errorf("after an ack: %q, want %q", (*sent)[4:], "D1E"+d1r[1][3:])
	}
}

func TestRelayWaitsForKeyUp(t *testing.T) {
	s, sent := relaySharer()
	play(s, clientOver(prebuffer+2), 10)
	s.FromRadio(hri.Ack('F'), ms(20))
	if voiceQueued(*sent) != 1 {
		t.Fatalf("sent %q, want the first voice frame", *sent)
	}
	n := len(*sent)
	s.FromRadio(hri.Ack('E'), ms(30))
	if voiceQueued((*sent)[n:]) != 0 {
		t.Fatalf("sent %q while the radio keys up", (*sent)[n:])
	}
	for _, st := range hri.TxStart {
		s.FromRadio(stateP(st), ms(40))
	}
	if voiceQueued((*sent)[n:]) != 1 {
		t.Errorf("sent %q once transmitting, want the next voice frame", (*sent)[n:])
	}
}

func TestRelayRepeatsHeaderNoData(t *testing.T) {
	s, sent := relaySharer()
	ackAll(t, s, sent, 30, 10)
	for len(s.relay.queue) > 0 {
		s.relay.waiting = ""
		s.pump(ms(4000))
	}
	var headers []int
	voice := 0
	for _, x := range *sent {
		switch {
		case strings.HasPrefix(x, "D1F0052"):
			headers = append(headers, voice)
		case strings.HasPrefix(x, "D1F"):
			t.Errorf("sent a data D1F: %q", x)
		case hri.Key(x) == "D1E":
			voice++
		}
	}
	if len(headers) != 2 || headers[0] != 0 || headers[1] != headerAgain {
		t.Errorf("headers after %v voice frames, want [0 %d]", headers, headerAgain)
	}
}

// ackAll plays an over to the end with the radio acking everything at once,
// and returns what was sent the radio.
func ackAll(t *testing.T, s *Sharer, sent *[]string, frames, at int) {
	t.Helper()
	for i, f := range clientOver(frames) {
		now := at + 100*i
		s.FromClient(f, ms(now))
		for range 3 {
			if n := len(*sent); n > 0 {
				if k := hri.Key((*sent)[n-1]); k == "D1E" || k == "D1F" {
					s.FromRadio(hri.Ack(k[2]), ms(now+1))
				}
			}
		}
		s.Tick(ms(now + 50))
	}
}

func count(sent []string, text string) int {
	n := 0
	for _, x := range sent {
		if x == text {
			n++
		}
	}
	return n
}

func TestRelayKeysAndUnkeys(t *testing.T) {
	s, sent := relaySharer()
	ackAll(t, s, sent, 30, 10) // 3 s
	if !s.relay.keyed {
		t.Fatal("not keyed during the over")
	}
	// Polls answered for the proxy are kept from WIRES-X.
	if owed := s.relay.bOwed; owed < 2 {
		t.Fatalf("owed %d answers, want the two PTT polls", owed)
	}
	for range s.relay.bOwed {
		if got := s.FromRadio("B0 0    0000000", ms(3100)); got != nil {
			t.Errorf("passed the answer to the proxy's poll: %q", texts(got))
		}
	}
	if got := s.FromRadio("B0 0    0000000", ms(3100)); len(got) != 1 {
		t.Errorf("kept an answer to WIRES-X's poll: %q", texts(got))
	}
	// Repeated every second while the over lasts.
	if on := count(*sent, pttOn); on < 4 {
		t.Errorf("PTT on sent %d times over 3 s", on)
	}
	n := len(*sent)
	s.Tick(ms(3200))
	if count((*sent)[n:], pttOff) != 0 {
		t.Fatal("unkeyed right after the last frame")
	}
	s.Tick(ms(4200))
	if count((*sent)[n:], pttOff) != 2 || s.relay.keyed {
		t.Fatalf("a second after the last frame: %q, want PTT off twice", (*sent)[n:])
	}
	for _, st := range append(append([]hri.State{}, hri.TxStart...), hri.TxEnd[:2]...) {
		s.FromRadio(stateP(st), ms(4300))
	}
	if got := s.FromRadio(stateP("03000"), ms(4400)); len(got) != 1 || s.relay.on {
		t.Errorf("radio idle: passed %q, relaying %v", texts(got), s.relay.on)
	}
}

func TestRelayKeepsWIRESXFromUnkeying(t *testing.T) {
	s, sent := relaySharer()
	ackAll(t, s, sent, 5, 10)
	n := len(*sent)
	if pass, out := s.FromWIRESX(pttOff, ms(520)); pass || out != nil {
		t.Errorf("WIRES-X's poll: pass %v, answer %q", pass, texts(out))
	}
	if len(*sent) != n+1 || (*sent)[n] != pttOn {
		t.Errorf("radio got %q for WIRES-X's poll, want PTT on", (*sent)[n:])
	}
	// A status query would stop the transmission: answered here instead.
	n = len(*sent)
	pass, out := s.FromWIRESX("D1C0000", ms(530))
	if pass || len(*sent) != n {
		t.Errorf("status query went to the radio: pass %v, sent %q", pass, (*sent)[n:])
	}
	if got := texts(out); len(got) != 1 || hri.Key(got[0]) != "D1C" {
		t.Errorf("answered WIRES-X's status query with %q", got)
	}
	owed := s.relay.bOwed
	if got := s.FromRadio("B0 0    0000000", ms(521)); owed == 0 && len(got) != 1 {
		t.Errorf("kept the answer from WIRES-X: %q", texts(got))
	}
	// Unkeyed, its polls go through.
	for at := 550; at < 4000; at += 50 {
		s.Tick(ms(at))
	}
	if pass, _ := s.FromWIRESX(pttOff, ms(2010)); !pass {
		t.Error("held WIRES-X's poll with the radio unkeyed")
	}
}

func TestRelaySendsOnWithoutAck(t *testing.T) {
	s, sent := relaySharer()
	play(s, clientOver(prebuffer+5), 10) // more than the radio gets unacked
	n, last := len(*sent), s.relay.sentAt
	s.Tick(last.Add(ackWait / 2))
	if len(*sent) != n {
		t.Fatalf("sent %q before the ack wait", (*sent)[n:])
	}
	s.Tick(last.Add(ackWait + time.Millisecond))
	if got := voiceQueued((*sent)[n:]); got != 1 {
		t.Errorf("sent %q after the ack wait, want one voice frame", (*sent)[n:])
	}
}

func TestRelayHidesTransmitterFromWIRESX(t *testing.T) {
	s, _ := relaySharer()
	play(s, clientOver(3), 10)
	for _, st := range hri.TxStart {
		if got := s.FromRadio(stateP(st), ms(400)); got != nil {
			t.Errorf("passed transmit state %s: %q", st, texts(got))
		}
	}
	if got := texts(s.FromRadio(hri.D1('C', hri.StatePayload("00004")), ms(450))); got[0] != "D1C000B0A038R0OR40" {
		t.Errorf("status while the over plays = %q", got)
	}
	s.Tick(ms(1500)) // the over's frames stopped: WIRES-X sees it end
	if !s.relay.on {
		t.Fatal("stopped relaying before the radio finished")
	}
	if got := texts(s.FromRadio(hri.D1('C', hri.StatePayload("00004")), ms(1600))); got[0] != "D1C000B04000R0OR40" {
		t.Errorf("status while the radio finishes = %q", got)
	}
	for at := 1650; at < 4000; at += 50 { // unacked: drains, then unkeys
		s.Tick(ms(at))
	}
	if s.relay.keyed || len(s.relay.queue) != 0 {
		t.Fatalf("keyed %v with %d queued", s.relay.keyed, len(s.relay.queue))
	}
	if got := s.FromRadio(stateP("00002"), ms(4000)); got != nil {
		t.Errorf("passed transmit end state: %q", texts(got))
	}
	if got := texts(s.FromRadio(stateP("03000"), ms(4100))); len(got) != 1 || s.relay.on {
		t.Errorf("radio idle: passed %q, relaying %v", got, s.relay.on)
	}
	// The channel is free for the next over.
	if out := play(s, clientOver(1), 4200); !startsWithHeader(out) {
		t.Errorf("next over: %q", out)
	}
}

func TestRelayYieldsToWIRESX(t *testing.T) {
	s, sent := relaySharer()
	play(s, clientOver(3), 10)
	s.FromWIRESX(hri.D1('F', "21216000"+strings.Repeat(" ", 60)+"07226120200353"), ms(300))
	n := len(*sent)
	s.Tick(ms(1000))
	if len(*sent) != n {
		t.Errorf("kept sending after WIRES-X took the radio: %q", (*sent)[n:])
	}
	if got := s.FromRadio(hri.Ack('F'), ms(1000)); len(got) != 1 {
		t.Errorf("held back WIRES-X's ack: %q", texts(got))
	}
}

func TestNoRelayWithoutRadio(t *testing.T) {
	s := &Sharer{}
	play(s, clientOver(3), 10)
	if s.relay.on {
		t.Error("relaying with no radio to write to")
	}
}

func TestRelayLearnsNodeCallsign(t *testing.T) {
	s, sent := relaySharer()
	s.FromWIRESX(hri.D1('F', "21216000"+"12345ABCDEN0TALK    "+"N0GW      N0TALK    "+strings.Repeat(" ", 20)+"07226120200353"), ms(0))
	s.FromRadio(idleP, ms(3500))
	s.txSeen = true
	play(s, clientOver(1), 4000)
	if len(*sent) == 0 || !strings.Contains((*sent)[0], "**********N0CALL    N0GW      N0CALL") {
		t.Errorf("header %q: want downlink N0GW, uplink N0CALL", *sent)
	}
}
