package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/ysf"
)

var t0 = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

func ms(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }

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

func TestMatcherPairsInOrder(t *testing.T) {
	var m Matcher
	c := &Cache{Replies: map[string]string{}}
	m.Request("P010000", ms(0))
	m.Request("D1C0000", ms(1))
	m.Request(hri.Ack('G'), ms(2))                          // WIRES-X's ack: no reply expected
	m.Request(hri.D1('E', strings.Repeat("0", 137)), ms(3)) // voice: only acked
	for _, r := range []string{"D1P000B0A010R0OR40", "B0 0    0000000", "D1C000B04000R0OR40"} {
		if req, ok := m.Reply(r, ms(4)); ok {
			c.Learn(req, r)
		}
	}
	if got, _ := c.Reply("P010000"); got != "B0 0    0000000" {
		t.Errorf("poll reply = %q", got)
	}
	if got, _ := c.Reply("D1C0000"); got != "D1C000B04000R0OR40" {
		t.Errorf("status reply = %q", got)
	}
	if c.Len() != 2 {
		t.Errorf("cached %d replies, want 2: %v", c.Len(), c.Replies)
	}
}

func TestCacheKeepsOnlyIdleStatus(t *testing.T) {
	c := &Cache{Replies: map[string]string{}}
	c.Learn("D1C0000", "D1C000B0A038R0OR40") // receiving
	c.Learn("P010000", "B1 0    0000000")    // busy
	if c.Len() != 0 {
		t.Fatalf("cached busy replies: %v", c.Replies)
	}
}

func TestEmulatorAnswersFromCache(t *testing.T) {
	e := &Emulator{Cache: testCache()}
	if got := texts(e.Request("R6423", ms(0))); len(got) != 1 || got[0] != "R0303030" {
		t.Errorf("R6423 -> %q", got)
	}
	if got := texts(e.Request("D1C0000", ms(0))); len(got) != 1 || got[0] != "D1C000B04000R0OR40" {
		t.Errorf("D1C0000 -> %q", got)
	}
	if got := e.Request("X9999", ms(0)); got != nil {
		t.Errorf("unknown request answered: %v", got)
	}
	if got := e.Request(hri.Ack('G'), ms(0)); got != nil {
		t.Errorf("ack answered: %v", got)
	}
}

func TestEmulatorTransmission(t *testing.T) {
	e := &Emulator{Cache: testCache()}
	hdr := hri.D1('F', "21216000"+"**********N0CALL    "+"N0GW      N0UP      "+"12345               "+strings.Repeat(" ", 14))
	out := texts(e.Request(hdr, ms(0)))
	want := []string{"D1F00010", "D1P000B20000R0OR40", "D1P000B00000R0OR40", "D1P000B00004R0OR40"}
	if strings.Join(out, " ") != strings.Join(want, " ") {
		t.Fatalf("header -> %q, want %q", out, want)
	}
	// WIRES-X sends voice faster than real time; the acks must come 100 ms apart.
	voice := hri.D1('E', hri.VoicePayload(5, [5][13]byte{}))
	var acks []time.Time
	for i := range 3 {
		for _, o := range e.Request(voice, ms(i)) {
			if o.Text == "D1E00010" {
				acks = append(acks, o.At)
			}
		}
	}
	if len(acks) != 3 || acks[1].Sub(acks[0]) != ackInterval || acks[2].Sub(acks[1]) != ackInterval {
		t.Fatalf("acks at %v", acks)
	}
	if got := texts(e.Request("D1C0000", ms(10))); got[0] != "D1C000B00004R0OR40" {
		t.Errorf("status while transmitting = %q", got)
	}
	if got := e.Tick(ms(200)); got != nil {
		t.Errorf("ended before the acks were out: %v", got)
	}
	end := texts(e.Tick(ms(600)))
	if strings.Join(end, " ") != "D1P000B00002R0OR40 D1P000B24002R0OR40 D1P000B04000R0OR40" {
		t.Errorf("end -> %q", end)
	}
}

func TestEmulatorInjection(t *testing.T) {
	e := &Emulator{Cache: testCache()}
	var csd1, csd2 [20]byte
	copy(csd1[:], "*****ABCDEN0CALL    ")
	copy(csd2[:], "N0CALL    N0CALL    ")
	fich := ysf.FICH{CS: 2, FT: 7, DT: ysf.DTVDMode2}
	var src [10]byte
	copy(src[:], "N0CALL    ")

	fich.FI = ysf.FIHeader
	out := texts(e.Inject(ysf.HeaderFrame(fich, csd1, csd2), src, false, ms(0)))
	want := []string{"D1P000B0A010R0OR40", "D1P000B2A010R0OR40", "D1P000B0A010R0OR40", "D1P000B0A018R0OR40", "D1P000B0A038R0OR40",
		"D1H001E2100200010*****ABCDEN0CALL    ", "D1H001E2100200020                    "}
	if strings.Join(out, "|") != strings.Join(want, "|") {
		t.Fatalf("header ->\n%q\nwant\n%q", out, want)
	}
	if got := texts(e.Request("P010000", ms(1))); got[0] != "B1 0    0000000" {
		t.Errorf("poll while receiving = %q", got)
	}

	fich.FI = ysf.FICommunications
	var voice [5][ysf.VoiceBlockLength]byte
	voice[0][0] = 0xAB
	var all []string
	for i := range 9 {
		fich.FN = byte(i % 8)
		var dch [10]byte
		copy(dch[:], "ABCDEFGHIJ")
		all = append(all, texts(e.Inject(ysf.VDMode2Frame(fich, dch, voice), src, false, ms(100*(i+1))))...)
	}
	// First frame: callsign block 1 repeat 1, then the voice with counter 5.
	if !strings.HasPrefix(all[0], "D1H001E2100200011") || !strings.HasPrefix(all[1], "D1R00892000500AB") {
		t.Errorf("first frame -> %q", all[:2])
	}
	var gs, rs int
	for _, s := range all {
		switch hri.Key(s) {
		case "D1G":
			gs++
			m, _ := hri.Parse(s)
			if d, ok := hri.ParseData(m); !ok || string(d.DT[:10]) != "ABCDEFGHIJ" {
				t.Errorf("D1G = %q", s)
			}
		case "D1R":
			rs++
			m, _ := hri.Parse(s)
			v, err := hri.ParseVoice(m)
			if err != nil || v.Blocks != voice {
				t.Errorf("D1R doesn't round-trip: %v", err)
			}
		}
	}
	if rs != 9 || gs != 1 {
		t.Errorf("%d D1R, %d D1G; want 9, 1", rs, gs)
	}

	fich.FI = ysf.FITerminator
	end := texts(e.Inject(ysf.HeaderFrame(fich, csd1, csd2), src, true, ms(1000)))
	if len(end) != len(hri.RxEnd) || end[len(end)-1] != "D1P000B04000R0OR40" {
		t.Errorf("terminator -> %q", end)
	}
}

// A hotspot's short DT (a radio without a position, as captured 2026-11-14)
// goes to WIRES-X as the HRI-200 sends it: cut after its end and sum, in a D1G
// of 0x52 characters, not padded out to 20 bytes.
func TestEmulatorShortDT(t *testing.T) {
	e := &Emulator{Cache: testCache()}
	var csd1, csd2 [20]byte
	copy(csd1[:], "*****EBKrQKN6FLN/H  ")
	copy(csd2[:], "                    ")
	fich := ysf.FICH{CS: 2, FT: 7, DT: ysf.DTVDMode2, FI: ysf.FIHeader}
	var src [10]byte
	copy(src[:], "KN6FLN/H  ")
	e.Inject(ysf.HeaderFrame(fich, csd1, csd2), src, false, ms(0))

	fich.FI = ysf.FICommunications
	var voice [5][ysf.VoiceBlockLength]byte
	halves := map[byte][10]byte{
		6: {0x17, 0x22, 0x61, 0x5F, 0x33, 0x03, 0x2F, 0, 0, 0},
		7: {' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '},
	}
	var gs []string
	for i := range 9 {
		fich.FN = byte(i % 8)
		dch := halves[fich.FN]
		for _, s := range texts(e.Inject(ysf.VDMode2Frame(fich, dch, voice), src, false, ms(100*(i+1)))) {
			if hri.Key(s) == "D1G" {
				gs = append(gs, s)
			}
		}
	}
	if len(gs) != 1 {
		t.Fatalf("%d D1G", len(gs))
	}
	if !strings.HasPrefix(gs[0], "D1G0052") || !strings.HasSuffix(gs[0], "1722615F33032F") {
		t.Errorf("D1G = %q, want a 0x52 payload ending in the DT through its sum", gs[0])
	}
}

func TestInjectionYieldsToWIRESX(t *testing.T) {
	e := &Emulator{Cache: testCache()}
	e.Request(hri.D1('E', hri.VoicePayload(5, [5][13]byte{})), ms(0))
	var csd [20]byte
	frame := ysf.HeaderFrame(ysf.FICH{FI: ysf.FIHeader, CS: 2, FT: 7, DT: ysf.DTVDMode2}, csd, csd)
	if got := e.Inject(frame, [10]byte{}, false, ms(10)); got != nil {
		t.Errorf("injected while WIRES-X transmits: %v", got)
	}
}
