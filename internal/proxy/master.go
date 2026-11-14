package proxy

import (
	"strings"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/usage"
)

// The master drives the real HRI-200 the way WIRES-X does, and is the only
// thing that writes to it: the once-a-second poll and status query, the PTT,
// room traffic and data pages sent as WIRES-X sends them (captured
// 2026-11-14: VA3CDV and DG1SR from room 30401, the node's announcement), the
// settings and version requests WIRES-X makes on startup. What the radio
// receives off the air it hands on as content (MasterEvent), never as the
// radio's own messages. It watches the radio's health and recovers it; none
// of that is seen by WIRES-X, which talks to the face.

const (
	pollEvery   = time.Second
	statusAfter = 600 * time.Millisecond // the D1C follows the poll

	pttVoice = "P100000" // transmit voice
	pttData  = "P110000" // transmit data
	pttOff   = "P010000" // receive: the idle poll
	pttOn    = pttVoice

	// ackWait sends the next message anyway if the radio hasn't acked.
	ackWait = 300 * time.Millisecond
	// prebuffer holds an over's voice until this many frames are queued: the
	// radio transmits from a small buffer and stops for good if it runs dry.
	prebuffer = 3
	// maxLag is the backlog an over that had to wait may still be played
	// with; beyond it, it joins live (prebuffer frames behind).
	maxLag = 15
	// txState is the radio transmitting, after 20000 and 00000 as it keys up.
	// It drops a D1E that arrives while it keys up, so after the first frame
	// the voice waits for txState, or keyWait.
	txState = hri.State("00004")
	keyWait = 300 * time.Millisecond
	// unkeyAfter unkeys this long after the last ack (WIRES-X: 134 ms).
	unkeyAfter = 130 * time.Millisecond
	// idleWait is how long the radio may take to report idle once unkeyed
	// (captured: 495 ms) before it is suspected.
	idleWait = 3 * time.Second
	// keyedWait is how long a keyed radio may take to report transmitting.
	keyedWait = 2 * time.Second
	// mayWait sends a data page anyway if D1B isn't answered.
	mayWait = 300 * time.Millisecond
	// staleWait drops a transmission that has waited this long for the radio.
	staleWait = 30 * time.Second
	// configWait moves on from a request the radio didn't answer.
	configWait = time.Second
	// replyKeyDelay keys a reply to a command heard off the air this long
	// after its page is acked (see router.wiresxData).
	replyKeyDelay = 1100 * time.Millisecond
)

// MasterEventKind says what a MasterEvent carries.
type MasterEventKind int

const (
	RxStart   MasterEventKind = iota + 1 // a reception off the air: Text is CSD1
	RxCSD                                // callsign block Block (0-2): Text
	RxData                               // the D1G's blocks and DT: Data
	RxVoice                              // a voice frame: Voice
	RxEnd                                // the reception ended
	RxCommand                            // a WIRES-X command heard: Command
	RxAnalog                             // an analog receive or hang state: State
	Answer                               // the radio's Reply to Request
)

// MasterEvent is something the radio reported, as content.
type MasterEvent struct {
	Kind    MasterEventKind
	Block   int
	Text    [20]byte
	Data    hri.Data
	Voice   [5][13]byte
	Command hri.Command
	State   hri.State
	Request string
	Reply   string
}

type txKind int

const (
	txVoice txKind = iota
	txDataPage
)

// Transmission is one over or data page for the radio, filled as it arrives.
type Transmission struct {
	Kind   txKind
	Source string // usage.WIRESX or usage.Hotspot
	Name   string // for the log
	// KeyDelay, for a data page: how long after its ack the radio is keyed
	// (20 ms after, as for WIRES-X's announcement, plus this).
	KeyDelay time.Duration

	queue   []string
	ended   bool
	created time.Time
	lastIn  time.Time

	started bool
	waiting string // key of the message the radio is to ack
	sentAt  time.Time
	ackAt   time.Time
	frames  int
	asked   bool // data: D1B sent
	may     bool // data: answered
	keyed   bool
	keyedAt time.Time
	txSeen  bool // the radio has reported transmitting
	ready   bool // ... txState
	sent    bool // data: the radio reported it sent (00002)
	unkeyed bool
	unkeyAt time.Time
}

// Add queues a message for the radio: a D1F or D1E as WIRES-X sends it.
func (t *Transmission) Add(text string, now time.Time) {
	t.queue = append(t.queue, text)
	t.lastIn = now
}

// End marks the transmission complete: nothing more will be added.
func (t *Transmission) End() { t.ended = true }

func voiceQueued(queue []string) int {
	n := 0
	for _, text := range queue {
		if hri.Key(text) == "D1E" {
			n++
		}
	}
	return n
}

// Master drives an attached HRI-200.
type Master struct {
	Write func(text string)
	Log   func(format string, args ...any)
	Cache *Cache
	// Recover, if set, resets the HRI-200 when it stops behaving: level 1
	// resets its USB device, 2 cycles its port's power. See health.go.
	Recover func(level int) error
	// Ladder keeps how far recovery has gone across reattachments.
	Ladder *Ladder
	// Analog, with PTT: see master_analog.go.
	HangZero bool
	Policy   Policy
	Sessions func(now time.Time) int
	// WIRESXBusy, if set, reports whether WIRES-X is transmitting or has
	// lately (packet waits for it).
	WIRESXBusy func(now time.Time) bool

	started  bool
	state    hri.State
	stateAt  time.Time
	pollAt   time.Time
	statusAt time.Time
	match    Matcher
	config   []string // requests to make, in order
	cfgSent  string
	cfgAt    time.Time
	tries    map[string]int
	rx       masterRx
	tx       *Transmission
	next     []*Transmission
	analog   analogState
	packetAt time.Time
	health   health
}

type masterRx struct {
	on        bool      // digital content seen
	analog    bool      // analog receive states seen
	contentAt time.Time // the last content (D1H, D1R, D1G)
}

// contentHold: a state that isn't receiving doesn't end a reception this
// soon after its content. A status reply can be older than the reception's
// first voice frame (07:20:27: D1R at .336, then "05000" at .340, the
// receive states only at .482); a reception's last frame is followed by its
// end states for 0.8 s before an idle one.
const contentHold = 150 * time.Millisecond

func (m *Master) logf(format string, args ...any) {
	if m.Log != nil {
		m.Log(format, args...)
	}
}

func (m *Master) send(text string, now time.Time) {
	m.match.Request(text, now)
	m.health.sent(now)
	m.Write(text)
}

// Start begins as WIRES-X does on startup: what it asks the radio, and the
// radio back to Digital, with the hang time if one is owed.
func (m *Master) Start(now time.Time) {
	m.started = true
	m.health.reset(now)
	m.startup()
	m.pollAt = now.Add(-pollEvery)
}

// startup queues WIRES-X's startup requests. Run again, they bring a wedged
// radio back (health.go): at 06:53 on 2026-11-14 they took one out of 00002,
// where 38 minutes of unkey polls had left it.
func (m *Master) startup() {
	m.tries = map[string]int{}
	m.config = append(m.config, "M10", "R6423", "D1V0000", ModeWrite(digitalMode, m.Cache.HangOwed()))
	m.analog.dirty = true // until that write is echoed
}

// checkSerial logs the attached HRI-200's serial (its R6423 answer), and
// whether it is the one the cache knew, which WIRES-X has been told of.
func (m *Master) checkSerial(r6423 string) {
	got, made, ok := Serial(r6423)
	if !ok {
		m.logf("master: HRI-200 serial unreadable: %q", r6423)
		return
	}
	known, _ := m.Cache.Reply("R6423")
	switch was, _, wasOK := Serial(known); {
	case !wasOK:
		m.logf("master: HRI-200 serial %s (made %s)", got, made)
	case was != got:
		m.logf("master: A DIFFERENT HRI-200: serial %s (made %s); WIRES-X has been told of %s", got, made, was)
	default:
		m.logf("master: HRI-200 serial %s (made %s), the one WIRES-X knows", got, made)
	}
}

// Query asks the radio a request from WIRES-X the face couldn't answer, or
// applies a settings write. The reply comes back as an Answer event.
func (m *Master) Query(text string) {
	if text == SetMode(digitalMode) && m.analog.phase == aIdle {
		return // the master keeps the radio in Digital itself
	}
	m.config = append(m.config, text)
}

// State is the radio's state as it last reported it.
func (m *Master) State() hri.State { return m.state }

// Source names who is transmitting on the radio, for usage.
func (m *Master) Source() string {
	switch {
	case m.analog.phase == aKeyed:
		return usage.Packet
	case m.tx != nil:
		return m.tx.Source
	}
	return usage.WIRESX
}

// Transmitting reports whether the master has a transmission under way or
// waiting.
func (m *Master) Transmitting() bool { return m.tx != nil || len(m.next) > 0 }

// Transmit queues a transmission. One at a time goes to the radio; each ends
// cleanly (all its frames, unkeyed, the radio idle) before the next starts.
func (m *Master) Transmit(t *Transmission, now time.Time) {
	t.created = now
	if t.lastIn.IsZero() {
		t.lastIn = now
	}
	m.next = append(m.next, t)
	m.preemptAnalog(now)
}

// canTransmit reports whether the radio may be keyed now.
func (m *Master) canTransmit() string {
	switch {
	case !m.started:
		return "starting"
	case m.health.sick:
		return "recovering"
	case m.analog.phase != aIdle:
		return "in Analog for packet"
	case m.state.Receiving() || m.state.DigitalReceiving():
		return "receiving"
	case m.state != "" && !m.state.Idle():
		return "in state " + string(m.state)
	}
	return ""
}

// FromRadio takes one message from the HRI-200.
func (m *Master) FromRadio(text string, now time.Time) []MasterEvent {
	m.health.heard(now)
	var ev []MasterEvent
	if req, ok := m.match.Reply(text, now); ok {
		if req == "R6423" {
			m.checkSerial(text) // before it is learned over the known one
		}
		m.Cache.Learn(req, text)
		if req == m.cfgSent {
			m.cfgSent = ""
		}
		if k := hri.Key(req); k != "P" && k != "D1C" {
			ev = append(ev, MasterEvent{Kind: Answer, Request: req, Reply: text})
		}
	}
	k := hri.Key(text)
	if k == "D1M" {
		m.Cache.NoteMode(text)
		m.analogEcho(text, now)
		return ev
	}
	msg, ok := hri.Parse(text)
	if !ok {
		return ev
	}
	if (k == "D1E" || k == "D1F") && len(msg.Payload) == 1 {
		if t := m.tx; t != nil && t.waiting == k {
			t.waiting, t.ackAt = "", now
		}
		return ev
	}
	if k == "D1B" {
		if t := m.tx; t != nil && t.asked {
			t.may = true
		}
		return ev
	}
	if s, ok := hri.ParseState(msg); ok {
		return append(ev, m.setState(s, now)...)
	}
	switch msg.Cmd {
	case 'H':
		c, err := hri.ParseCallsignBlock(msg)
		if err != nil {
			return ev
		}
		ev = append(ev, m.rxContent(now)...)
		return append(ev, MasterEvent{Kind: RxCSD, Block: c.Block - 1, Text: c.Text})
	case 'R':
		v, err := hri.ParseVoice(msg)
		if err != nil {
			return ev
		}
		ev = append(ev, m.rxContent(now)...)
		return append(ev, MasterEvent{Kind: RxVoice, Voice: v.Blocks})
	case 'G':
		if strings.HasPrefix(msg.Payload, "210010") {
			// A WIRES-X command heard off the air: acked as WIRES-X does.
			m.Write(hri.Ack('G'))
			if c, ok := hri.ParseCommand(msg); ok {
				return append(ev, MasterEvent{Kind: RxCommand, Command: c})
			}
			return ev
		}
		if d, ok := hri.ParseData(msg); ok {
			ev = append(ev, m.rxContent(now)...)
			return append(ev, MasterEvent{Kind: RxData, Data: d})
		}
	}
	return ev
}

// rxContent starts a reception at its first receive state, or at its first
// content if the states were missed.
func (m *Master) rxContent(now time.Time) []MasterEvent {
	m.rx.contentAt = now
	if m.rx.on {
		return nil
	}
	m.rx.on = true
	return []MasterEvent{{Kind: RxStart}}
}

// setState notes a state the radio reported.
func (m *Master) setState(s hri.State, now time.Time) []MasterEvent {
	m.state, m.stateAt = s, now
	m.health.state(s, now, m.ours())
	var ev []MasterEvent
	if s.Receiving() && s[4] == '7' {
		m.packetActivity(now)
	}
	if s.DigitalReceiving() && !m.ours() {
		ev = append(ev, m.rxContent(now)...) // digital: the face starts with it
	}
	if !s.DigitalReceiving() && m.rx.on && now.Sub(m.rx.contentAt) >= contentHold {
		m.rx.on = false
		ev = append(ev, MasterEvent{Kind: RxEnd})
	}
	// An analog reception and its hang: shown to WIRES-X as they are, while
	// the master isn't transmitting itself.
	if !m.ours() && (s[4] == '7' || m.rx.analog) {
		m.rx.analog = s[4] == '7'
		ev = append(ev, MasterEvent{Kind: RxAnalog, State: s})
	}
	if t := m.tx; t != nil && t.keyed && !s.Receiving() && !s.DigitalReceiving() {
		if !s.Idle() && !t.txSeen {
			t.txSeen = true
		}
		if s == txState {
			t.ready = true
		}
		if t.Kind == txDataPage && s == "00002" && t.ready {
			t.sent = true
		}
	}
	m.analogState(s, now)
	return ev
}

// ours reports whether the master has the radio transmitting.
func (m *Master) ours() bool {
	return m.tx != nil && m.tx.started || m.analog.phase != aIdle
}

// Tick polls the radio, moves transmissions and settings along, and watches
// the radio's health.
func (m *Master) Tick(now time.Time) {
	if !m.started {
		return
	}
	m.analogTick(now)
	m.pump(now)
	if now.Sub(m.pollAt) >= pollEvery {
		m.pollAt = now
		m.send(m.pollText(), now)
		m.statusAt = now.Add(statusAfter)
	}
	if !m.statusAt.IsZero() && !now.Before(m.statusAt) {
		m.statusAt = time.Time{}
		if !m.keyed() {
			m.send("D1C0000", now)
		}
	}
	m.pumpConfig(now)
	m.healthTick(now)
}

func (m *Master) keyed() bool {
	return m.tx != nil && m.tx.keyed && !m.tx.unkeyed || m.analog.keyed()
}

// pollText is the once-a-second poll: keyed for whatever is transmitting.
func (m *Master) pollText() string {
	switch {
	case m.analog.keyed():
		return pttOn
	case m.tx != nil && m.tx.keyed && !m.tx.unkeyed:
		if m.tx.Kind == txDataPage {
			return pttData
		}
		return pttVoice
	}
	return pttOff
}

// pumpConfig makes the requests queued, one at a time, while not transmitting.
func (m *Master) pumpConfig(now time.Time) {
	if m.cfgSent != "" && now.Sub(m.cfgAt) < configWait {
		return
	}
	if m.cfgSent != "" {
		// Unanswered: the version request goes unanswered until the radio
		// is up, so WIRES-X asks again; so does the master, a few times.
		if hri.Key(m.cfgSent) == "D1V" && m.tries[m.cfgSent] < 5 {
			m.config = append([]string{m.cfgSent}, m.config...)
		}
		m.cfgSent = ""
	}
	if len(m.config) == 0 || m.keyed() || m.analog.phase != aIdle {
		return
	}
	text := m.config[0]
	if hri.Key(text) == "D1M" && m.state != "" && !m.state.Idle() && !m.health.sick {
		return // settings only to an idle radio
	}
	m.config = m.config[1:]
	m.tries[text]++
	m.cfgSent, m.cfgAt = text, now
	if hri.Key(text) == "D1M" {
		m.analog.echoes++
	}
	m.send(text, now)
}

// pump moves the transmission along: started once the radio is free, each
// message once the last is acked, keyed after the first frame, unkeyed after
// the last, done once the radio reports idle.
func (m *Master) pump(now time.Time) {
	for {
		if m.tx == nil {
			if !m.takeNext(now) {
				return
			}
		}
		t := m.tx
		if !t.started {
			if why := m.canTransmit(); why != "" {
				if now.Sub(t.created) > staleWait {
					m.logf("master: dropping %s's %s, the radio %s for %v", t.Name, t.kindName(), why, staleWait)
					m.tx = nil
					continue
				}
				return
			}
			if t.Kind == txVoice && !t.ended && voiceQueued(t.queue) < prebuffer {
				return
			}
			if t.Kind == txVoice && voiceQueued(t.queue) > maxLag {
				if t.ended {
					m.logf("master: %s's over waited %.1f s and is over; not transmitting it", t.Name, now.Sub(t.created).Seconds())
					m.tx = nil
					continue
				}
				t.queue = trimBacklog(t.queue, prebuffer)
				m.logf("master: %s's over waited %.1f s; joining it live", t.Name, now.Sub(t.created).Seconds())
			}
			t.started = true
			m.logf("master: transmitting %s's %s", t.Name, t.kindName())
		}
		if !m.pumpTx(t, now) {
			return
		}
		m.tx = nil // done: on to the next
	}
}

func (t *Transmission) kindName() string {
	if t.Kind == txDataPage {
		return "data"
	}
	return "over"
}

// takeNext starts the next transmission waiting, if any.
func (m *Master) takeNext(now time.Time) bool {
	if len(m.next) == 0 {
		return false
	}
	m.tx, m.next = m.next[0], m.next[1:]
	return true
}

// trimBacklog keeps the first header, and the last keep frames with what
// came among them.
func trimBacklog(queue []string, keep int) []string {
	var head []string
	if len(queue) > 0 && hri.Key(queue[0]) == "D1F" {
		head, queue = queue[:1], queue[1:]
	}
	n := 0
	for i := len(queue) - 1; i >= 0; i-- {
		if hri.Key(queue[i]) == "D1E" {
			if n++; n == keep {
				return append(head, queue[i:]...)
			}
		}
	}
	return append(head, queue...)
}

// pumpTx moves a started transmission along; it reports when it is done.
func (m *Master) pumpTx(t *Transmission, now time.Time) bool {
	if t.Kind == txDataPage && !t.asked {
		t.asked, t.sentAt = true, now
		m.send(hri.D1('B', "0"), now)
		return false
	}
	if t.Kind == txDataPage && !t.may && now.Sub(t.sentAt) < mayWait {
		return false
	}
	for len(t.queue) > 0 && !t.unkeyed {
		if t.waiting != "" && now.Sub(t.sentAt) < ackWait {
			return false
		}
		text := t.queue[0]
		if hri.Key(text) == "D1E" && t.frames == 1 && !t.ready && now.Sub(t.keyedAt) < keyWait {
			return false // the radio drops frames while it keys up
		}
		if hri.Key(text) == "D1E" && t.frames == 0 && !t.ended && voiceQueued(t.queue) < 1 {
			return false
		}
		t.queue = t.queue[1:]
		t.waiting, t.sentAt = hri.Key(text), now
		m.send(text, now)
		if t.waiting == "D1E" {
			if t.frames++; t.frames == 1 {
				m.key(t, pttVoice, now)
			}
		}
	}
	if !t.ended || t.waiting != "" && now.Sub(t.sentAt) < ackWait {
		return false
	}
	if t.Kind == txDataPage && !t.keyed {
		if now.Sub(t.ackAt) >= 20*time.Millisecond+t.KeyDelay {
			m.key(t, pttData, now)
		}
		return false
	}
	if !t.keyed {
		return true // nothing to send, never keyed
	}
	if !t.unkeyed {
		switch {
		case t.Kind == txDataPage && !t.sent && now.Sub(t.keyedAt) < keyedWait+dataAirtime(600):
			return false
		case t.Kind == txVoice && now.Sub(t.ackAt) < unkeyAfter:
			return false
		}
		if t.Kind == txDataPage && !t.sent {
			m.logf("master: the radio never reported the data sent; unkeying")
		}
		t.unkeyed, t.unkeyAt = true, now
		m.send(pttOff, now)
		m.send(pttOff, now)
		return false
	}
	if !t.txSeen && now.Sub(t.keyedAt) > keyedWait {
		m.fault(now, "keyed, but never reported transmitting")
		return true
	}
	if m.state.Idle() && m.stateAt.After(t.unkeyAt) {
		return true
	}
	if now.Sub(t.unkeyAt) > idleWait {
		m.fault(now, "unkeyed, but never reported idle (state "+string(m.state)+")")
		return true
	}
	return false
}

func (m *Master) key(t *Transmission, ptt string, now time.Time) {
	t.keyed, t.keyedAt = true, now
	m.send(ptt, now)
	m.send(ptt, now)
	m.pollAt = now // the next keyed poll a second on, as WIRES-X's
}

// Stop leaves the radio as WIRES-X would: unkeyed, in Digital.
func (m *Master) Stop() {
	if m.keyed() {
		m.Write(pttOff)
		m.Write(pttOff)
	}
	if m.analog.dirty {
		m.logf("master: stopping with the repeater maybe in Analog, switching it back")
		m.Write(pttOff)
		m.Write(ModeWrite(digitalMode, m.Cache.HangOwed()))
	}
}
