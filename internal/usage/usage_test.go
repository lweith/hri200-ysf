package usage

import (
	"strings"
	"testing"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func ms(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }

// play reports states 100 ms apart from at, and returns the time after.
func play(tr *Tracker, at int, relaying bool, states ...hri.State) int {
	for _, s := range states {
		tr.State(s, ms(at), relaying)
		at += 100
	}
	return at
}

func wantLine(t *testing.T, metrics, line string) {
	t.Helper()
	for _, l := range strings.Split(metrics, "\n") {
		if l == line {
			return
		}
	}
	t.Errorf("no %q in:\n%s", line, metrics)
}

func TestAirAnalogAndDigital(t *testing.T) {
	var tr Tracker
	// Analog: mode unknown at first, then 0A017, 1 s in all; ends 04007.
	at := play(&tr, 0, false, "04000", "0A010", "0A017", "0A017", "0A017", "0A017",
		"0A017", "0A017", "0A017", "0A017", "0A017", "04007")
	// Digital, as captured (RxStart, RxEnd): 0.5 s.
	at = play(&tr, at, false, hri.RxStart...)
	play(&tr, at, false, hri.RxEnd...)
	m := tr.metrics(ms(10000))
	wantLine(t, m, `hri200_keyed_seconds_total{source="air",mode="analog"} 1.000`)
	wantLine(t, m, `hri200_keyed_seconds_total{source="air",mode="digital"} 0.500`)
	wantLine(t, m, `hri200_overs_total{source="air",mode="analog"} 1`)
	wantLine(t, m, `hri200_overs_total{source="air",mode="digital"} 1`)
	wantLine(t, m, `hri200_overs_total{source="air",mode="unknown"} 0`)
	wantLine(t, m, `hri200_overs_total{source="wiresx",mode="digital"} 0`)
}

func TestOverUnderWay(t *testing.T) {
	var tr Tracker
	play(&tr, 0, false, "0A010", "0A038")
	m := tr.metrics(ms(2500))
	wantLine(t, m, `hri200_keyed_seconds_total{source="air",mode="digital"} 2.500`)
	wantLine(t, m, `hri200_overs_total{source="air",mode="digital"} 0`)
	wantLine(t, m, `hri200_keyed{source="air",mode="digital"} 1`)
	wantLine(t, m, `hri200_keyed{source="air",mode="analog"} 0`)
}

func TestTransmissionsBySource(t *testing.T) {
	var tr Tracker
	// WIRES-X room traffic: keyed 20000 through 00002 (0.4 s), unkeyed 24002.
	at := play(&tr, 0, false, hri.TxStart...)
	at = play(&tr, at, false, hri.TxEnd...)
	// A relayed hotspot over: 1 s keyed.
	at = play(&tr, at, true, "20000", "00000", "00004", "00004", "00004", "00004",
		"00004", "00004", "00004", "00002", "23002", "03000")
	m := tr.metrics(ms(at))
	wantLine(t, m, `hri200_keyed_seconds_total{source="wiresx",mode="digital"} 0.400`)
	wantLine(t, m, `hri200_keyed_seconds_total{source="hotspot",mode="digital"} 1.000`)
	wantLine(t, m, `hri200_overs_total{source="wiresx",mode="digital"} 1`)
	wantLine(t, m, `hri200_overs_total{source="hotspot",mode="digital"} 1`)
	wantLine(t, m, `hri200_overs_total{source="air",mode="unknown"} 0`)
	wantLine(t, m, `hri200_keyed{source="hotspot",mode="digital"} 0`)
}

func TestDetachedEndsWhatWasUnderWay(t *testing.T) {
	var tr Tracker
	tr.Attached()
	wantLine(t, tr.metrics(ms(0)), `hri200_attached 1`)
	play(&tr, 0, false, "0A010")
	tr.Detached(ms(700))
	m := tr.metrics(ms(5000))
	wantLine(t, m, `hri200_keyed_seconds_total{source="air",mode="unknown"} 0.700`)
	wantLine(t, m, `hri200_overs_total{source="air",mode="unknown"} 1`)
	wantLine(t, m, `hri200_attached 0`)
}
