package proxy

import (
	"strings"
	"testing"

	"github.com/lweith/hri200-ysf/internal/bridge"
	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/ysf"
)

// A weak signal on the repeater (2026-11-14 07:20:27, the serial trace): the
// radio's receive states never read "A" (the level, not a receive flag), it
// got no header, so sent no D1H, and its first voice frame came before its
// receive states, after a status reply of "05000". Times in ms; "R" is a
// voice frame.
var weakOver = []struct {
	ms   int
	text string
}{
	{336, "R"}, {340, "D1C05000"}, {435, "R"}, {482, "D1P06010"}, {488, "D1P26010"}, {521, "D1P06010"},
	{527, "D1P06018"}, {566, "R"}, {636, "R"}, {736, "R"}, {783, "D1P04008"}, {837, "R"}, {841, "D1P06018"},
	{940, "R"}, {1040, "R"}, {1139, "R"}, {1239, "R"}, {1243, "D1P05008"}, {1257, "D1P06018"},
	{1274, "D1P05038"}, {1337, "R"}, {1341, "D1C05038"}, {1440, "R"}, {1539, "R"}, {1636, "R"},
	{1736, "R"}, {1836, "R"}, {1939, "R"}, {2039, "R"}, {2136, "R"}, {2239, "R"}, {2336, "R"},
	{2355, "D1C06038"}, {2440, "R"}, {2539, "R"}, {2636, "R"}, {2736, "R"}, {2839, "R"}, {2940, "R"},
	{3040, "R"}, {3141, "R"}, {3239, "R"}, {3336, "R"}, {3355, "D1C05038"}, {3436, "R"}, {3537, "R"},
	{3636, "R"}, {3736, "R"}, {3836, "R"}, {3936, "R"}, {4036, "R"}, {4136, "R"}, {4237, "R"},
	{4336, "R"}, {4355, "D1C04038"}, {4436, "R"}, {4536, "R"}, {4636, "R"}, {4736, "R"}, {4837, "R"},
	{4936, "R"}, {5036, "R"}, {5136, "R"}, {5237, "R"}, {5336, "R"}, {5354, "D1C04038"}, {5436, "R"},
	{5537, "R"}, {5637, "R"}, {5737, "R"}, {5837, "R"}, {5937, "R"}, {6037, "R"}, {6136, "R"},
	{6237, "R"}, {6337, "R"}, {6375, "D1C04038"}, {6437, "R"}, {6605, "D1P03008"}, {7171, "D1P24008"},
	{7205, "D1P04008"}, {7271, "D1P03000"}, {7395, "D1C04000"}, {8395, "D1C03000"},
}

// It is one reception to WIRES-X, not fifteen, with every voice frame and no
// callsigns the radio never sent; also when the signal fades for a second
// and the radio sends no voice, only its receive states.
func TestWeakSignalIsOneReception(t *testing.T) {
	t.Run("as heard", func(t *testing.T) { weakSignal(t, 0, 0) })
	t.Run("fading", func(t *testing.T) { weakSignal(t, 2000, 3000) })
}

// weakSignal plays weakOver with no voice frames from fadeFrom to fadeTo ms.
func weakSignal(t *testing.T, fadeFrom, fadeTo int) {
	r := &router{bridge: &bridge.Bridge{}, face: &Face{Cache: testCache()}, policy: DefaultPolicy()}
	m, _ := newMaster(t)
	r.master = m
	starts, ends, voice, headers := 0, 0, 0, 0
	var blocks [5][ysf.VoiceBlockLength]byte
	frames := uint16(0)
	was := rxSource(0)
	for _, e := range weakOver {
		text := e.text
		switch {
		case text == "R" && e.ms >= fadeFrom && e.ms < fadeTo:
			continue
		case text == "R":
			frames++
			text = hri.D1('R', hri.VoicePayload(5*frames, blocks))
		default:
			text = text[:3] + "000B" + text[3:] + "R0OR40"
		}
		for _, o := range r.FromRadio(text, at(e.ms)) {
			switch hri.Key(o.Text) {
			case "D1R":
				voice++
			case "D1H":
				headers++
			}
			if strings.HasPrefix(o.Text, "B1") {
				t.Errorf("busy at %d ms; the radio said B0 throughout", e.ms)
			}
		}
		now := r.face.Receiving()
		if was == 0 && now != 0 {
			starts++
		}
		if was != 0 && now == 0 {
			ends++
			if e.ms < 6437 {
				t.Errorf("reception ended at %d ms (%s), before its last frame", e.ms, e.text)
			}
		}
		was = now
	}
	if starts != 1 || ends != 1 {
		t.Errorf("%d receptions started, %d ended; want 1 and 1", starts, ends)
	}
	if voice != int(frames) {
		t.Errorf("%d voice frames to WIRES-X of %d", voice, frames)
	}
	if headers != 0 {
		t.Errorf("%d D1H sent; the radio sent none", headers)
	}
}

func TestDigitalReceiving(t *testing.T) {
	for _, s := range []string{"0A010", "2A010", "0A018", "0A038", "06010", "26010", "06018", "04008",
		"05038", "03008", "24008", "23008", "09038", "08010", "06008"} {
		if !hri.State(s).DigitalReceiving() {
			t.Errorf("%s: not receiving", s)
		}
	}
	for _, s := range []string{"04000", "03000", "05000", "02000", "01000", "06000", "00000", "00002",
		"00004", "20000", "24002", "23002", "22002", "04002", "24000", "0A017", "04007", "03007", "0400B", "0300B"} {
		if hri.State(s).DigitalReceiving() {
			t.Errorf("%s: receiving", s)
		}
	}
}
