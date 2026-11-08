// Package usage times how the repeater is used, from the states its HRI-200
// reports, and serves the totals as Prometheus metrics.
//
// Every over has a source: received off the air from the repeater, or
// transmitted by the radio, for WIRES-X (room traffic), for the proxy (a
// hotspot client's over), or for a packet modem (direwolf, keyed in Analog).
// And a mode: analog or digital.
//
// A reception lasts from the first receive state ("?A???") to the first state
// that isn't one. Its mode is the receive state's last character: '7' analog,
// '8' digital, others ("0A010") not known yet. A transmission lasts from the
// first transmit state ("?0???": 20000, 00000, 00004, 00002) to the first that
// isn't one, as the radio unkeys (24002, 23002). WIRES-X and the proxy send
// the radio C4FM; the packet modem's transmissions are analog.
//
// An analog reception is also told apart by what it carried: packet if the
// packet modem decoded an AX.25 frame during it (Frame), else voice.
package usage

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
)

// Sources and modes: the metrics' labels.
const (
	Air     = "air"     // received from the repeater
	WIRESX  = "wiresx"  // transmitted: room traffic WIRES-X sent the radio
	Hotspot = "hotspot" // transmitted: a client's over the proxy relayed
	Packet  = "packet"  // transmitted: the packet modem's, keyed in Analog

	Analog  = "analog"
	Digital = "digital"
	Unknown = "unknown" // ended before the radio reported its mode
)

// Contents of analog receptions.
const (
	Voice     = "voice"  // no frame decoded: voice, a kerchunk, noise
	PacketRx  = "packet" // the packet modem decoded a frame during it
	frameLate = 3 * time.Second
)

var contents = []string{PacketRx, Voice}

type kind struct{ source, mode string }

// kinds are the series served, always, so each starts at zero.
var kinds = []kind{
	{Air, Analog}, {Air, Digital}, {Air, Unknown},
	{WIRESX, Digital}, {Hotspot, Digital}, {Packet, Analog},
}

type over struct {
	on     bool
	kind   kind
	start  time.Time
	end    time.Time
	packet bool // a frame was decoded during it
}

// Tracker times overs. Its methods may be called from any goroutine.
type Tracker struct {
	mu       sync.Mutex
	attached bool
	rx, tx   over
	seconds  map[kind]float64 // ended overs
	count    map[kind]int
	// Analog receptions by content. An ended one waits frameLate for a
	// frame decoded as it ended (the modem's audio lags the radio's states).
	pending  []over
	cSeconds map[string]float64
	cCount   map[string]int
}

func (t *Tracker) end(o *over, at time.Time) {
	if !o.on {
		return
	}
	o.on = false
	o.end = at
	if t.seconds == nil {
		t.seconds, t.count = map[kind]float64{}, map[kind]int{}
	}
	t.seconds[o.kind] += at.Sub(o.start).Seconds()
	t.count[o.kind]++
	if o.kind == (kind{Air, Analog}) {
		t.pending = append(t.pending, *o)
	}
}

// settle files analog receptions ended more than frameLate ago by content.
func (t *Tracker) settle(now time.Time) {
	keep := t.pending[:0]
	for _, o := range t.pending {
		if now.Sub(o.end) < frameLate {
			keep = append(keep, o)
			continue
		}
		if t.cSeconds == nil {
			t.cSeconds, t.cCount = map[string]float64{}, map[string]int{}
		}
		c := Voice
		if o.packet {
			c = PacketRx
		}
		t.cSeconds[c] += o.end.Sub(o.start).Seconds()
		t.cCount[c]++
	}
	t.pending = keep
}

func modeOf(s hri.State) string {
	switch s[4] {
	case '7':
		return Analog
	case '8':
		return Digital
	}
	return ""
}

// State notes a state the radio reported at at. relaying says whether the
// proxy is transmitting a client's over, which names a transmission's source.
func (t *Tracker) State(s hri.State, at time.Time, relaying bool) {
	src := WIRESX
	if relaying {
		src = Hotspot
	}
	t.StateFrom(s, at, src)
}

// StateFrom is State with a transmission's source given: WIRESX, Hotspot or
// Packet. It names one only as it starts.
func (t *Tracker) StateFrom(s hri.State, at time.Time, source string) {
	if len(s) != 5 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.attached = true
	t.settle(at)
	if s[1] == 'A' {
		if !t.rx.on {
			t.rx = over{on: true, kind: kind{Air, Unknown}, start: at}
		}
		if m := modeOf(s); m != "" {
			t.rx.kind.mode = m
		}
	} else {
		t.end(&t.rx, at)
	}
	if s[1] == '0' {
		if !t.tx.on {
			mode := Digital
			if source == Packet {
				mode = Analog
			}
			t.tx = over{on: true, kind: kind{source, mode}, start: at}
		}
	} else {
		t.end(&t.tx, at)
	}
}

// Frame notes that the packet modem decoded a frame off the air at at: the
// analog reception under way, or one that ended within frameLate, carried
// packet.
func (t *Tracker) Frame(at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.rx.on {
		t.rx.packet = true
	}
	for i := range t.pending {
		o := &t.pending[i]
		if !at.Before(o.start) && at.Sub(o.end) < frameLate {
			o.packet = true
		}
	}
}

// Attached notes the HRI-200 has been attached.
func (t *Tracker) Attached() {
	t.mu.Lock()
	t.attached = true
	t.mu.Unlock()
}

// Detached ends whatever was under way when the HRI-200 went away.
func (t *Tracker) Detached(at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.attached = false
	t.end(&t.rx, at)
	t.end(&t.tx, at)
}

// ServeHTTP serves the metrics in the Prometheus text format. The seconds
// include the over under way, so they climb while it lasts; the counts go up
// when it ends.
func (t *Tracker) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprint(w, t.metrics(time.Now()))
}

func (t *Tracker) metrics(now time.Time) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.settle(now)
	keyed := func(k kind) bool {
		return (t.rx.on && t.rx.kind == k) || (t.tx.on && t.tx.kind == k)
	}
	var b []byte
	put := func(format string, args ...any) { b = fmt.Appendf(b, format, args...) }
	labels := func(k kind) string { return fmt.Sprintf("source=%q,mode=%q", k.source, k.mode) }

	put("# HELP hri200_keyed_seconds_total Time the repeater's HRI-200 radio has been receiving (source air) or transmitting, by source and mode.\n")
	put("# TYPE hri200_keyed_seconds_total counter\n")
	for _, k := range kinds {
		s := t.seconds[k]
		for _, o := range []over{t.rx, t.tx} {
			if o.on && o.kind == k {
				s += now.Sub(o.start).Seconds()
			}
		}
		put("hri200_keyed_seconds_total{%s} %.3f\n", labels(k), s)
	}
	put("# HELP hri200_overs_total Overs that have ended, by source and mode.\n")
	put("# TYPE hri200_overs_total counter\n")
	for _, k := range kinds {
		put("hri200_overs_total{%s} %d\n", labels(k), t.count[k])
	}
	put("# HELP hri200_keyed Whether an over is under way, by source and mode (air/unknown until the radio reports the mode).\n")
	put("# TYPE hri200_keyed gauge\n")
	for _, k := range kinds {
		v := 0
		if keyed(k) {
			v = 1
		}
		put("hri200_keyed{%s} %d\n", labels(k), v)
	}
	put("# HELP hri200_air_analog_seconds_total Analog receptions' time by content: packet (the packet modem decoded a frame) or voice. Filed %v after each ends.\n", frameLate)
	put("# TYPE hri200_air_analog_seconds_total counter\n")
	for _, c := range contents {
		put("hri200_air_analog_seconds_total{content=%q} %.3f\n", c, t.cSeconds[c])
	}
	put("# HELP hri200_air_analog_overs_total Analog receptions by content: packet or voice.\n")
	put("# TYPE hri200_air_analog_overs_total counter\n")
	for _, c := range contents {
		put("hri200_air_analog_overs_total{content=%q} %d\n", c, t.cCount[c])
	}
	put("# HELP hri200_attached Whether the HRI-200 is attached.\n")
	put("# TYPE hri200_attached gauge\n")
	v := 0
	if t.attached {
		v = 1
	}
	put("hri200_attached %d\n", v)
	return string(b)
}
