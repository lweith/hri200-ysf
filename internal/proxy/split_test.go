package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/lweith/hri200-ysf/internal/bridge"
	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/ysf"
)

var t0 = time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)

func at(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }

func testCache() *Cache {
	c := &Cache{Replies: map[string]string{}}
	c.Learn("P010000", "B0 0    0000000")
	c.Learn("R6423", "R0303030")
	return c
}

func texts(o []Out) []string {
	s := make([]string, len(o))
	for i, x := range o {
		s[i] = x.Text
	}
	return s
}

func keys(o []Out) []string {
	s := make([]string, len(o))
	for i, x := range o {
		s[i] = hri.Key(x.Text)
	}
	return s
}

// The face plays a reception as the HRI-200 did at 05:07:44: a callsign
// block repeat before each of the first six D1R, a D1G after the 6th and
// every 7th after, busy polls, and the end states.
func TestFaceReceptionAsCaptured(t *testing.T) {
	f := &Face{Cache: testCache()}
	var csd1 [20]byte
	copy(csd1[:], "*****EBKrQKN6FLN/H  ")
	out := f.StartRx(rxClient, csd1, at(0))
	want := []string{"D1P000B0A010R0OR40", "D1P000B2A010R0OR40", "D1P000B0A010R0OR40", "D1P000B0A018R0OR40", "D1P000B0A038R0OR40",
		"D1H001E2100200010*****EBKrQKN6FLN/H  ", "D1H001E2100200020                    ", "B1 0    0000000"}
	if strings.Join(texts(out), "|") != strings.Join(want, "|") {
		t.Fatalf("start:\n%q\nwant\n%q", texts(out), want)
	}
	if got := texts(f.Request("P010000", at(300))); got[0] != "B1 0    0000000" {
		t.Errorf("poll while receiving: %q", got)
	}
	var seq []string
	var voice [5][13]byte
	for i := range 20 {
		seq = append(seq, keys(f.RxVoice(voice, at(400+100*i)))...)
	}
	var gAfter []int
	n := 0
	for _, k := range seq {
		switch k {
		case "D1R":
			n++
		case "D1G":
			gAfter = append(gAfter, n)
		}
	}
	if len(gAfter) != 3 || gAfter[0] != 6 || gAfter[1] != 13 || gAfter[2] != 20 {
		t.Errorf("D1G after frames %v, want 6 13 20", gAfter)
	}
	if strings.Join(seq[:4], " ") != "D1H D1R D1H D1R" {
		t.Errorf("first messages %v", seq[:4])
	}
	end := texts(f.EndRx(at(3000)))
	if strings.Join(end, "|") != "D1P000B04008R0OR40|B0 0    0000000|D1P000B23008R0OR40|D1P000B03008R0OR40|D1P000B04000R0OR40" {
		t.Errorf("end %q", end)
	}
}

// Unsolicited messages never overtake one another, whatever their source.
func TestFaceOutputsInOrder(t *testing.T) {
	f := &Face{Cache: testCache()}
	var csd [20]byte
	out := f.StartRx(rxClient, csd, at(0))
	out = append(out, f.RxVoice([5][13]byte{}, at(50))...)
	for i := 1; i < len(out); i++ {
		if out[i].At.Before(out[i-1].At) {
			t.Fatalf("%q at %v before %q at %v", out[i].Text, out[i].At, out[i-1].Text, out[i-1].At)
		}
	}
}

// WIRES-X's voice: acked on the radio's clock, transmit states on the keyed
// poll, end states on the unkeyed one, as captured.
func TestFaceTransmissionFollowsPTT(t *testing.T) {
	f := &Face{Cache: testCache()}
	hdr := hri.D1('F', roomPrefix+strings.Repeat(" ", 60)+"1722615F33032F")
	if got := texts(f.Request(hdr, at(0))); len(got) != 1 || got[0] != "D1F00010" {
		t.Fatalf("header: %q", got)
	}
	voice := hri.D1('E', hri.VoicePayload(5, [5][13]byte{}))
	var acks []time.Time
	for i := range 3 {
		for _, o := range f.Request(voice, at(60+i)) {
			acks = append(acks, o.At)
		}
	}
	if acks[1].Sub(acks[0]) != ackInterval || acks[2].Sub(acks[1]) != ackInterval {
		t.Errorf("acks at %v", acks)
	}
	if f.State(at(100)) != "04000" {
		t.Errorf("transmitting before keyed")
	}
	on := texts(f.Request(pttVoice, at(100)))
	if strings.Join(on, "|") != "B0 0    0000000|D1P000B20000R0OR40|D1P000B00000R0OR40|D1P000B00004R0OR40" {
		t.Errorf("keyed: %q", on)
	}
	if f.State(at(300)) != "00004" {
		t.Errorf("state %s while keyed", f.State(at(300)))
	}
	off := texts(f.Request(pttOff, at(2000)))
	if strings.Join(off, "|") != "B0 0    0000000|D1P000B00002R0OR40|D1P000B24002R0OR40|D1P000B04000R0OR40" {
		t.Errorf("unkeyed: %q", off)
	}
	if f.Transmitting() {
		t.Error("still transmitting")
	}
}

// A data page: D1B answered, the page acked, keyed with P110000, 00002 once
// sent, then the rest of the end on the unkeyed poll.
func TestFaceDataPage(t *testing.T) {
	f := &Face{Cache: testCache()}
	if got := texts(f.Request("D1B00010", at(0))); got[0] != "D1B00011" {
		t.Fatalf("D1B: %q", got)
	}
	page := hri.D1('F', dataPrefix+strings.Repeat(" ", 60)+"0D5D415F2603AA")
	f.Request(page, at(10))
	f.Request(pttData, at(100))
	if got := texts(f.Tick(at(100).Add(dataAirtime(len(page)) + time.Millisecond))); len(got) != 1 || got[0] != "D1P000B00002R0OR40" {
		t.Errorf("data sent: %q", got)
	}
	off := texts(f.Request(pttOff, at(2000)))
	if strings.Join(off[1:], "|") != "D1P000B24002R0OR40|D1P000B04000R0OR40" {
		t.Errorf("unkeyed: %q", off)
	}
}

// The face answers from the cache, and forwards what it can't answer.
func TestFaceForwards(t *testing.T) {
	var fwd []string
	f := &Face{Cache: testCache(), Forward: func(text string, _ time.Time) { fwd = append(fwd, text) }}
	if got := texts(f.Request("R6423", at(0))); got[0] != "R0303030" || len(fwd) != 0 {
		t.Errorf("cached: %q, forwarded %q", got, fwd)
	}
	if got := f.Request("D1V0000", at(0)); got != nil || len(fwd) != 1 {
		t.Errorf("uncached: %q, forwarded %q", got, fwd)
	}
	if got := texts(f.Answer("D1V0000", "D1V0003abc", at(10))); len(got) != 1 {
		t.Errorf("answer: %q", got)
	}
	if got := f.Answer("D1V0000", "D1V0003abc", at(20)); got != nil {
		t.Errorf("answered twice")
	}
	f.Request(SetMode(digitalMode), at(30))
	if len(fwd) != 2 {
		t.Errorf("a settings write isn't forwarded: %q", fwd)
	}
}

// radio is a fake HRI-200 for the master: it records what it is sent.
type radio struct{ sent []string }

func (r *radio) write(text string) { r.sent = append(r.sent, text) }

func (r *radio) take() []string { s := r.sent; r.sent = nil; return s }

func newMaster(t *testing.T) (*Master, *radio) {
	r := &radio{}
	m := &Master{Write: r.write, Log: t.Logf, Cache: testCache()}
	m.Start(at(0))
	m.config = nil // past startup
	m.analog.dirty = false
	m.state = "04000"
	return m, r
}

// The master sends room traffic as WIRES-X does: header, voice on acks,
// keyed twice after the first frame, unkeyed twice after the last ack, done
// once the radio reports idle.
func TestMasterTransmitsAsWIRESX(t *testing.T) {
	m, r := newMaster(t)
	m.pollAt = at(0) // the poll clock out of the way
	tx := &Transmission{Kind: txVoice, Source: "wiresx", Name: "test"}
	hdr := hri.D1('F', roomPrefix+strings.Repeat(" ", 60)+"1722615F33032F")
	tx.Add(hdr, at(0))
	voice := hri.D1('E', hri.VoicePayload(5, [5][13]byte{}))
	for range 4 {
		tx.Add(voice, at(0))
	}
	m.Transmit(tx, at(0))
	m.pump(at(10))
	if got := r.take(); len(got) != 1 || got[0] != hdr {
		t.Fatalf("first: %q", got)
	}
	m.FromRadio("D1F00010", at(40))
	m.pump(at(41))
	if got := r.take(); strings.Join(keys2(got), " ") != "D1E P P" || got[1] != pttVoice {
		t.Fatalf("after the header ack: %q", got)
	}
	m.FromRadio("D1E00010", at(90))
	m.pump(at(91))
	if got := r.take(); len(got) != 0 {
		t.Fatalf("sent before keyed up: %q", got)
	}
	m.FromRadio("D1P000B20000R0OR40", at(100))
	m.FromRadio("D1P000B00000R0OR40", at(130))
	m.FromRadio("D1P000B00004R0OR40", at(140))
	m.pump(at(141))
	for i := range 3 {
		if got := r.take(); len(got) != 1 || hri.Key(got[0]) != "D1E" {
			t.Fatalf("frame %d: %q", i+2, got)
		}
		m.FromRadio("D1E00010", at(200+100*i))
		m.pump(at(201 + 100*i))
	}
	tx.End()
	m.pump(at(500))
	if got := r.take(); len(got) != 0 {
		t.Fatalf("unkeyed too soon: %q", got)
	}
	m.pump(at(450 + 140))
	if got := r.take(); strings.Join(got, " ") != pttOff+" "+pttOff {
		t.Fatalf("unkey: %q", got)
	}
	m.FromRadio("D1P000B00002R0OR40", at(800))
	m.FromRadio("D1P000B24002R0OR40", at(1100))
	m.FromRadio("D1P000B04000R0OR40", at(1130))
	m.pump(at(1131))
	if m.tx != nil {
		t.Error("not done once idle")
	}
}

func keys2(s []string) []string {
	k := make([]string, len(s))
	for i, x := range s {
		k[i] = hri.Key(x)
	}
	return k
}

// A data page goes as WIRES-X sends one: D1B, the page, keyed for data once
// acked, unkeyed once the radio reports it sent.
func TestMasterDataPage(t *testing.T) {
	m, r := newMaster(t)
	page := hri.D1('F', dataPrefix+strings.Repeat(" ", 60)+"0D5D415F2603AA")
	tx := &Transmission{Kind: txDataPage, Source: "wiresx", Name: "test"}
	tx.Add(page, at(0))
	tx.End()
	m.Transmit(tx, at(0))
	m.pump(at(1))
	if got := r.take(); len(got) != 1 || got[0] != "D1B00010" {
		t.Fatalf("first: %q", got)
	}
	m.FromRadio("D1B00011", at(7))
	m.pump(at(8))
	if got := r.take(); len(got) != 1 || got[0] != page {
		t.Fatalf("then: %q", got)
	}
	m.FromRadio("D1F00010", at(100))
	m.pump(at(150))
	m.pump(at(201))
	if got := r.take(); strings.Join(got, " ") != pttData+" "+pttData {
		t.Fatalf("keying: %q", got)
	}
	m.FromRadio("D1P000B20000R0OR40", at(220))
	m.FromRadio("D1P000B00004R0OR40", at(250))
	m.FromRadio("D1P000B00002R0OR40", at(1000))
	m.pump(at(1001))
	if got := r.take(); strings.Join(got, " ") != pttOff+" "+pttOff {
		t.Fatalf("unkey: %q", got)
	}
	m.FromRadio("D1P000B04000R0OR40", at(1300))
	m.pump(at(1301))
	if m.tx != nil {
		t.Error("not done")
	}
}

// The case that wedged the radio twice on 2026-11-14: WIRES-X's data page
// arriving while a client's over is on the radio. The over ends cleanly
// first; the page follows once the radio is idle; nothing is cut.
func TestMasterQueuesInsteadOfCutting(t *testing.T) {
	m, r := newMaster(t)
	over := &Transmission{Kind: txVoice, Source: "hotspot", Name: "client"}
	over.Add(hri.D1('F', roomPrefix+strings.Repeat(" ", 60)+"1722615F33032F"), at(0))
	for range 3 {
		over.Add(hri.D1('E', hri.VoicePayload(5, [5][13]byte{})), at(0))
	}
	m.Transmit(over, at(0))
	m.pump(at(1))
	page := &Transmission{Kind: txDataPage, Source: "wiresx", Name: "WIRES-X"}
	page.Add(hri.D1('F', dataPrefix+strings.Repeat(" ", 60)+"0D5D415F2603AA"), at(5))
	page.End()
	m.Transmit(page, at(5))
	for _, s := range r.take() {
		if s == "D1B00010" || s == pttData {
			t.Fatalf("the page went out mid-over: %q", s)
		}
	}
	if m.tx != over {
		t.Fatal("the over was dropped")
	}
}

// A sick radio is hidden: the face goes on answering WIRES-X idle while the
// master stops answering nothing.
func TestSickRadioHiddenFromWIRESX(t *testing.T) {
	r := &router{bridge: &bridge.Bridge{}, face: &Face{Cache: testCache()}, policy: DefaultPolicy()}
	m, _ := newMaster(t)
	r.master = m
	m.WIRESXBusy = r.wiresxBusy
	for i := range 10 {
		r.Tick(at(1000 * i)) // polls the radio; it never answers
	}
	if !m.health.sick {
		t.Fatal("silent radio not noticed")
	}
	if got := texts(r.FromWIRESX("P010000", at(10000))); got[0] != "B0 0    0000000" {
		t.Errorf("WIRES-X's poll: %q", got)
	}
	if got := texts(r.FromWIRESX("D1C0000", at(10001))); got[0] != "D1C000B04000R0OR40" {
		t.Errorf("WIRES-X's status: %q", got)
	}
}

// What the radio says never reaches WIRES-X as it is: a reception off the
// air is played by the face, a stuck transmit state isn't shown at all.
func TestRadioNeverPassedThrough(t *testing.T) {
	r := &router{bridge: &bridge.Bridge{}, face: &Face{Cache: testCache()}, policy: DefaultPolicy()}
	m, _ := newMaster(t)
	r.master = m
	if got := r.FromRadio("D1P000B00002R0OR40", at(0)); got != nil {
		t.Errorf("a transmit state reached WIRES-X: %q", texts(got))
	}
	out := r.FromRadio("D1P000B0A010R0OR40", at(10))
	if len(out) != len(faceRxStart) || out[0].Text != "D1P000B0A010R0OR40" {
		t.Errorf("reception not started by the face: %q", texts(out))
	}
	if got := r.FromRadio("D1H001E2100200010*****EBKrQKN6FLN/H  ", at(260)); got != nil {
		t.Errorf("D1H 10 alone passed on: %q", texts(got))
	}
	got := texts(r.FromRadio("D1H001E2100200020                    ", at(270)))
	if len(got) != 3 || got[0] != "D1H001E2100200010*****EBKrQKN6FLN/H  " || got[2] != "B1 0    0000000" {
		t.Errorf("callsigns: %q", got)
	}
}

// The relay builds a client's over as WIRES-X builds room traffic.
func TestRelayHeaderIsRoomTraffic(t *testing.T) {
	r := &router{bridge: &bridge.Bridge{}, face: &Face{Cache: testCache()}, policy: DefaultPolicy(), transmit: true}
	m, _ := newMaster(t)
	r.master = m
	copy(r.node[:], "W8HHF     ")
	copy(r.room[:], "30401")
	var csd1, csd2 [20]byte
	copy(csd1[:], "*****EBKrQKN6FLN    ")
	copy(csd2[:], "                    ")
	fich := ysf.FICH{CS: 2, FT: 7, DT: ysf.DTVDMode2, FI: ysf.FIHeader}
	r.FromClient(YSF{Frame: ysf.HeaderFrame(fich, csd1, csd2)}, at(0))
	if r.client.tx == nil || len(r.client.tx.queue) != 1 {
		t.Fatal("no relay")
	}
	h := r.client.tx.queue[0]
	want := "D1F0052" + roomPrefix + "*****EBKrQKN6FLN    " + "W8HHF     KN6FLN    " + "30401               " + "0722615F33031F"
	if h != want {
		t.Errorf("header\n %q\nwant\n %q", h, want)
	}
}
