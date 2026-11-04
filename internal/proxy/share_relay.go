package proxy

import (
	"encoding/hex"
	"strings"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
)

// Transmitting a client's over on the repeater too. WIRES-X never sends the
// radio what the radio received, so a client's over reaches the room but not
// the repeater. With Sharer.Radio set, the proxy drives the HRI-200's
// transmitter itself as WIRES-X does for room traffic to everyone (captured):
// a voice header, a D1E per voice frame and the header again after the sixth,
// each sent once the radio has acked the one before. After the first D1E it
// keys the transmitter with pttOn twice, repeats it (see pttEvery), and unkeys with
// pttOff twice a second after the last D1E. Without the PTT polls the radio
// acks the frames but never transmits. P commands are polls: the radio
// answers each with a B status reply.
//
// Meanwhile WIRES-X keeps seeing a radio that received the over and went
// idle: the radio's acks, transmit states and answers to the proxy's own
// polls are kept from it. WIRES-X's own unkeyed polls (pttOff, about once a
// second) would unkey the radio, so while keyed they go to it as pttOn.
const (
	// ackWait sends the next message anyway if the radio hasn't acked.
	ackWait = 300 * time.Millisecond
	// relayTail gives up waiting for the radio to report its transmission
	// over this long after the last message.
	relayTail = 3 * time.Second
	// txPrefix starts the transmit header and data D1F payloads, for an over
	// addressed to everyone (as captured; "21216000" when addressed to a room).
	txPrefix = "20216000"

	pttOn  = "P100000" // transmit voice (WIRES-X sends "P110000" for data)
	pttOff = "P010000" // receive: also WIRES-X's idle poll
	// pttEvery repeats pttOn while keyed. WIRES-X repeats it every second.
	// This is twice as often, from a wrong guess at why the radio stopped
	// (see headerAgain); not yet put back to WIRES-X's cadence.
	pttEvery = 500 * time.Millisecond
	// pttHang unkeys this long after the last voice frame, as WIRES-X does.
	pttHang = time.Second

	// prebuffer holds the voice until this many frames are queued. The radio
	// transmits from a small buffer and stops for good if it runs dry (seen
	// 1.1 s into an over fed frame by frame as it arrived). WIRES-X feeds it
	// from a backlog, each frame as the last is acked, so the buffer fills
	// and the radio paces the acks at the air rate.
	prebuffer = 5
	// headerAgain repeats the header after this many voice frames, as WIRES-X
	// does for an over to everyone (captured: one header, six frames,
	// the header again, then voice alone for 16 s). It sends data D1Fs only
	// for overs to a room ("21216000"). A "20216000" data D1F after the 7th
	// frame made the radio stop transmitting as it reached it in its buffer,
	// 1.08 s after keying up, three times out of three.
	headerAgain = 6

	// txState is the radio transmitting, after 20000 and 00000 as it keys up.
	// It silently drops a D1E that arrives while it keys up (seen twice, the
	// second frame, 5 ms before 20000), so after the first frame the voice
	// waits for txState, as WIRES-X's does, or keyWait after keying.
	txState = hri.State("00004")
	keyWait = 300 * time.Millisecond
)

// defaultDT is a DT record for an over whose own hasn't arrived yet: no GPS,
// as an FT-5D sends it.
var defaultDT = [20]byte{0x07, 0x22, 0x61, 0x5F, 0x33, 0x03, 0x1F}

type relayState struct {
	rx       *reception // the over being relayed, nil once it ended
	on       bool       // driving the radio, until it reports the transmission over
	txSeen   bool       // the radio has reported transmitting
	queue    []string
	waiting  string // key of the message awaiting the radio's ack
	sentAt   time.Time
	frames   int
	header   string    // the over's transmit header
	primed   bool      // enough voice queued to start sending it
	keyed    bool      // pttOn sent, pttOff not yet
	keyedAt  time.Time // the first pttOn
	txReady  bool      // the radio has reported transmitting (txState)
	pttAt    time.Time // the last pttOn
	voiceAt  time.Time // the last D1E
	downlink [10]byte  // the node's callsign, from WIRES-X's own headers
	bOwed    int       // answers to the proxy's own polls, to keep from WIRES-X
}

// reset ends the relay, keeping what outlives it.
func (r *relayState) reset() {
	*r = relayState{downlink: r.downlink, bOwed: r.bOwed}
}

// poll sends the radio a P command of the proxy's own, whose answer WIRES-X
// must not see.
func (s *Sharer) poll(text string, now time.Time) {
	r := &s.relay
	r.bOwed++
	switch text {
	case pttOn:
		if !r.keyed {
			r.keyedAt = now
		}
		r.keyed, r.pttAt = true, now
	case pttOff:
		r.keyed, r.sentAt = false, now
	}
	s.Radio(text)
}

// pump sends queued polls, and the next queued message if the radio has acked
// the last.
func (s *Sharer) pump(now time.Time) {
	r := &s.relay
	for len(r.queue) > 0 {
		text := r.queue[0]
		if hri.Key(text) == "P" {
			r.queue = r.queue[1:]
			s.poll(text, now)
			continue
		}
		if r.waiting != "" && now.Sub(r.sentAt) < ackWait {
			return
		}
		if hri.Key(text) == "D1E" && !r.voiceAt.IsZero() && r.keyed && !r.txReady &&
			now.Sub(r.keyedAt) < keyWait {
			return // the radio drops frames while it keys up
		}
		if hri.Key(text) == "D1E" && !r.primed {
			if r.rx != nil && voiceQueued(r.queue) < prebuffer {
				return
			}
			r.primed = true
		}
		r.queue = r.queue[1:]
		r.waiting, r.sentAt = hri.Key(text), now
		if r.waiting == "D1E" {
			r.voiceAt = now
		}
		s.Radio(text)
	}
}

func voiceQueued(queue []string) int {
	n := 0
	for _, text := range queue {
		if hri.Key(text) == "D1E" {
			n++
		}
	}
	return n
}

func (s *Sharer) relayCSD(rx *reception) (csd1, csd2 [20]byte) {
	csd1, csd2 = blank20, blank20
	copy(csd1[:10], "**********")
	copy(csd1[10:], rx.csd1[10:])
	if s.relay.downlink[0] != 0 { // learned
		copy(csd2[:10], s.relay.downlink[:])
	}
	copy(csd2[10:], rx.csd1[10:])
	return csd1, csd2
}

func (rx *reception) record() [20]byte {
	if rx.dt[0] == ' ' {
		return defaultDT
	}
	return rx.dt
}

// relayFrom queues for the radio what is being played to WIRES-X: a header
// when an over starts, and its voice and data.
func (s *Sharer) relayFrom(out []Out, now time.Time) {
	if s.Radio == nil {
		return
	}
	r := &s.relay
	rx := s.emu.rx
	if rx != nil && rx != r.rx {
		if !r.on {
			s.logf("proxy: transmitting %q's over on the repeater", strings.TrimRight(string(rx.csd1[10:]), " "))
		}
		r.rx, r.on, r.frames = rx, true, 0
		csd1, csd2 := s.relayCSD(rx)
		dt := rx.record()
		r.header = hri.D1('F', txPrefix+string(csd1[:])+string(csd2[:])+string(rx.csd3[:])+
			strings.ToUpper(hex.EncodeToString(dt[:7])))
		r.queue = append(r.queue, r.header)
	}
	if r.rx == nil {
		return
	}
	for _, o := range out {
		switch hri.Key(o.Text) {
		case "D1R":
			r.queue = append(r.queue, "D1E"+o.Text[3:]) // the same payload
			if r.frames++; r.frames == 1 && !r.keyed {
				r.queue = append(r.queue, pttOn, pttOn)
			}
			if r.frames == headerAgain {
				r.queue = append(r.queue, r.header)
			}
		}
	}
	s.pump(now)
}

// relaySync notes an over ending: nothing more is queued, and the radio
// finishes what it has.
func (s *Sharer) relaySync() {
	if s.relay.rx != nil && s.emu.rx != s.relay.rx {
		s.relay.rx = nil
	}
}

// relayStop hands the radio to WIRES-X, which has started transmitting.
func (s *Sharer) relayStop() {
	if s.relay.on {
		s.logf("proxy: WIRES-X transmitting, stopping the repeater transmission")
	}
	s.relay.reset() // WIRES-X keys the radio itself
}

// relayPoll keeps WIRES-X's polls from ending the transmission mid-relay. Its
// unkeyed poll would unkey the radio: the radio gets pttOn instead, and
// WIRES-X its answer. WIRES-X never sends a status query (D1C) while it
// transmits itself, so one is answered here, as the radio would while
// transmitting, rewritten for WIRES-X. (Suspected of stopping the radio, but
// it stopped anyway without one.) It reports whether it took the poll, and
// the answer for WIRES-X.
func (s *Sharer) relayPoll(text string, now time.Time) (bool, []Out) {
	if !s.relay.keyed {
		return false, nil
	}
	switch {
	case text == pttOff:
		s.relay.pttAt = now
		s.Radio(pttOn)
		return true, nil
	case hri.Key(text) == "D1C":
		return true, s.FromRadio(hri.D1('C', hri.StatePayload("00004")), now)
	}
	return false, nil
}

// relayFromRadio takes the radio's answers to the relay: its acks and transmit
// states, which WIRES-X must not see. It reports whether it took text, and
// what to tell WIRES-X instead.
func (s *Sharer) relayFromRadio(text string, now time.Time) ([]Out, bool) {
	r := &s.relay
	k := hri.Key(text)
	if k == "B" && r.bOwed > 0 {
		r.bOwed--
		return nil, true
	}
	if !r.on {
		return nil, false
	}
	if (k == "D1E" || k == "D1F") && hri.IsAck(text) {
		if r.waiting == k {
			r.waiting = ""
			s.pump(now)
		}
		return nil, true
	}
	m, ok := hri.Parse(text)
	if !ok {
		return nil, false
	}
	st, ok := hri.ParseState(m)
	if !ok || st.Receiving() {
		return nil, false
	}
	if !st.Idle() && !r.txSeen {
		s.logf("proxy: repeater transmitting")
		r.txSeen = true
	}
	if st == txState && !r.txReady {
		r.txReady = true
		s.pump(now)
	}
	if k == "D1P" && st.Idle() && r.keyed {
		s.logf("proxy: repeater stopped transmitting while keyed, %d messages queued", len(r.queue))
	}
	// Done once the over has ended and the radio has transmitted and gone
	// idle, or never transmitted and has nothing left to send.
	if k == "D1P" && st.Idle() && r.rx == nil && !r.keyed && (r.txSeen || len(r.queue) == 0) {
		if len(r.queue) > 0 {
			s.logf("proxy: repeater stopped transmitting with %d messages unsent", len(r.queue))
		}
		s.logf("proxy: repeater transmission over")
		r.reset()
		s.radio = st
		return []Out{{text, now}}, true
	}
	switch {
	case k == "D1P":
		return nil, true // transmitting: WIRES-X sees a receiving or idle radio
	case k == "D1C" && s.emu.rx == nil:
		return []Out{{hri.D1('C', hri.StatePayload("04000")), now}}, true
	}
	return nil, false // a status reply while receiving: rewritten as usual
}

// relayTick sends on if the radio hasn't acked, keeps the transmitter keyed
// and unkeys it once everything is sent, and gives up on a radio that never
// reports the transmission over.
func (s *Sharer) relayTick(now time.Time) {
	r := &s.relay
	if !r.on {
		return
	}
	s.pump(now)
	sent := r.rx == nil && len(r.queue) == 0
	switch {
	case r.keyed && sent && now.Sub(r.voiceAt) >= pttHang:
		s.poll(pttOff, now)
		s.poll(pttOff, now)
	case r.keyed && now.Sub(r.pttAt) >= pttEvery:
		s.poll(pttOn, now)
	case !r.keyed && sent && now.Sub(r.sentAt) > relayTail:
		s.logf("proxy: repeater never reported transmitting")
		r.reset()
	}
}

// learnDownlink notes the node's callsign from WIRES-X's transmit headers.
func (s *Sharer) learnDownlink(text string) {
	m, ok := hri.Parse(text)
	if !ok {
		return
	}
	if h, ok := hri.ParseTxHeader(m); ok && h.CSD2[0] != ' ' {
		copy(s.relay.downlink[:], h.CSD2[:10])
	}
}
