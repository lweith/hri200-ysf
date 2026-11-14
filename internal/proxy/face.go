package proxy

import (
	"encoding/hex"
	"strings"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/ysf"
)

// The face is the HRI-200 WIRES-X talks to. It is a model of a healthy radio,
// and nothing else reaches WIRES-X: not one message from the real HRI-200 is
// passed through. What the real radio receives off the air comes to the face
// as content (callsign blocks, voice, DT, commands; see Master and router),
// and the face plays it to WIRES-X as a reception of its own, message for
// message as an HRI-200 does (captured 2026-11-14 05:07:44, an FT-5D on the
// repeater). WIRES-X's transmissions are acked and keyed by the face's own
// clock, whether the real radio transmits them, is busy, is wedged or is
// unplugged. Requests the model doesn't cover (settings, version) are
// answered from the cache of the real radio's replies, and forwarded to it.

// Out is a message for WIRES-X, to be written no earlier than At. Messages
// are written in order of At (see writeOuts), so a poll is answered at once
// while a state sequence unfolds.
type Out struct {
	Text string
	At   time.Time
}

// step is a state, or another message, some time into a sequence.
type step struct {
	after time.Duration
	state hri.State // or text, when state is ""
	text  string
}

func st(after time.Duration, s hri.State) step { return step{after: after, state: s} }

const ms_ = time.Millisecond

// Sequences as the HRI-200 reports them, timed from captures (2026-11-14).
var (
	// faceRxStart starts a reception off the air; the first callsign blocks
	// follow at 250 ms, then an unsolicited busy status.
	faceRxStart = []step{st(0, "0A010"), st(130*ms_, "2A010"), st(163*ms_, "0A010"), st(169*ms_, "0A018"), st(240*ms_, "0A038")}
	// faceRxEnd ends it, 70 ms after the last voice frame.
	faceRxEnd = []step{st(0, "04008"), {after: 32 * ms_, text: "B0"}, st(610*ms_, "23008"), st(643*ms_, "03008"), st(710*ms_, "04000")}
	// faceCmdEnd follows a command (a D1G with no voice), 50 ms after it.
	faceCmdEnd = []step{st(0, "04008"), st(628*ms_, "24008"), st(661*ms_, "04008"), st(728*ms_, "04000")}
	// faceTxStart follows the first keyed poll (P100000 voice, P110000 data).
	faceTxStart = []step{st(45*ms_, "20000"), st(80*ms_, "00000"), st(88*ms_, "00004")}
	// faceTxEnd follows the unkeyed poll after voice.
	faceTxEnd = []step{st(180*ms_, "00002"), st(460*ms_, "24002"), st(495*ms_, "04000")}
	// faceDataEnd follows the unkeyed poll after data; 00002 came first, when
	// the data had gone out.
	faceDataEnd = []step{st(250*ms_, "24002"), st(285*ms_, "04000")}
)

const (
	// ackInterval is how fast the HRI-200 acks D1E voice: one per frame time,
	// on its own clock. WIRES-X waits for each ack, so this paces it.
	ackInterval = 100 * time.Millisecond
	// ackLatency is the soonest an ack follows its D1E or D1F.
	ackLatency = 40 * time.Millisecond
	// rxHang ends a reception this long after its last frame.
	rxHang = 400 * time.Millisecond
	// pttLapse ends a keyed transmission when WIRES-X hasn't polled keyed for
	// this long; it polls once a second.
	pttLapse = 3 * time.Second
	// unkeyedLapse forgets a transmission WIRES-X never keyed.
	unkeyedLapse = 3 * time.Second
	// stateStep spaces messages that would otherwise share a time.
	stateStep = 10 * time.Millisecond
	// mirrorMax stops mirroring the real radio's analog states after this long.
	mirrorMax = 5 * time.Minute
)

// dataAirtime is how long the radio takes to send a data page of n
// characters once keyed (captured: 0xFA 775 ms, 0x146 872 ms).
func dataAirtime(n int) time.Duration {
	return 455*time.Millisecond + time.Duration(n)*1280*time.Microsecond
}

type rxSource int

const (
	rxAir    rxSource = iota + 1 // the real radio's reception
	rxClient                     // a YSF client's over
)

func (s rxSource) String() string {
	if s == rxAir {
		return "air"
	}
	return "client"
}

// Face answers WIRES-X as an HRI-200.
type Face struct {
	Cache *Cache
	Log   func(format string, args ...any)
	// Forward, if set, passes on to the real radio a request the model
	// doesn't cover; Answer delivers its reply if the cache had none.
	Forward func(text string, now time.Time)

	settings string // the last settings echo WIRES-X was given

	sched    []timedState // states reported or to be, in time order
	last     hri.State    // the latest state whose time has come
	floor    time.Time    // unsolicited messages go out no earlier
	tx       *faceTx
	rx       *reception
	cmdTill  time.Time // a command is being played until then
	mirror   bool      // reporting the real radio's analog states
	mirrorAt time.Time
	owed     map[string]int // forwarded requests WIRES-X still waits on
	unknown  map[string]bool
}

type timedState struct {
	at time.Time
	s  hri.State
}

type faceTx struct {
	data    bool // a data page, not voice
	keyed   bool
	pttAt   time.Time // the last keyed poll
	seenAt  time.Time // the last D1E or D1F
	nextAck time.Time
	dataLen int       // characters of data pages
	doneAt  time.Time // data: when the radio reports it sent (00002)
	done    bool
}

type reception struct {
	src              rxSource
	csd1, csd2, csd3 [20]byte
	dt               [20]byte
	frames           int
	last             time.Time
	named            bool // the first callsign blocks (D1H 10, 20) sent
}

func (f *Face) logf(format string, args ...any) {
	if f.Log != nil {
		f.Log(format, args...)
	}
}

// State is the state the face reports now.
func (f *Face) State(now time.Time) hri.State {
	i := 0
	for i < len(f.sched) && !f.sched[i].at.After(now) {
		f.last = f.sched[i].s
		i++
	}
	f.sched = f.sched[i:]
	if f.last == "" {
		return "04000"
	}
	return f.last
}

// Transmitting reports whether WIRES-X is transmitting to the face.
func (f *Face) Transmitting() bool { return f.tx != nil }

// Receiving reports what the face is playing to WIRES-X, 0 if nothing.
func (f *Face) Receiving() rxSource {
	if f.rx == nil {
		return 0
	}
	return f.rx.src
}

// Busy reports whether the face has anything under way: a reception, a
// transmission, a command being played.
func (f *Face) Busy(now time.Time) bool {
	return f.rx != nil || f.tx != nil || now.Before(f.cmdTill)
}

// play schedules a sequence from start, after anything already scheduled, so
// that the radio's unsolicited messages never overtake one another.
func (f *Face) play(seq []step, start time.Time) []Out {
	if start.Before(f.floor) {
		start = f.floor
	}
	out := make([]Out, 0, len(seq))
	for _, s := range seq {
		at := start.Add(s.after)
		if s.state == "" {
			out = append(out, Out{f.bReply(s.text == "B1"), at})
			continue
		}
		out = append(out, Out{hri.D1('P', hri.StatePayload(s.state)), at})
		f.sched = append(f.sched, timedState{at, s.state})
		f.floor = at
	}
	if len(out) > 0 && out[len(out)-1].At.After(f.floor) {
		f.floor = out[len(out)-1].At
	}
	return out
}

// emit schedules one unsolicited message (D1H, D1R, D1G) after the last.
func (f *Face) emit(text string, now time.Time) Out {
	at := now
	if !at.After(f.floor) {
		at = f.floor.Add(time.Millisecond)
	}
	f.floor = at
	return Out{text, at}
}

// bReply is the answer to a poll: the radio's idle answer, busy while receiving.
func (f *Face) bReply(busy bool) string {
	r, ok := f.Cache.Reply(pttOff)
	if !ok || len(r) < 2 || r[0] != 'B' {
		r = "B0 0    0000000"
	}
	if busy {
		return "B1" + r[2:]
	}
	return "B0" + r[2:]
}

// Request answers one message from WIRES-X.
func (f *Face) Request(text string, now time.Time) []Out {
	switch k := hri.Key(text); k {
	case "P":
		return f.poll(text, now)
	case "D1C":
		return []Out{{hri.D1('C', hri.StatePayload(f.State(now))), now.Add(9 * ms_)}}
	case "D1B":
		if m, ok := hri.Parse(text); ok && m.Payload == "0" {
			return []Out{{hri.D1('B', "1"), now.Add(6 * ms_)}} // the radio may send
		}
	case "D1E", "D1F":
		if hri.IsAck(text) {
			return nil
		}
		return f.transmission(k, text, now)
	case "D1G":
		if hri.IsAck(text) {
			return nil // WIRES-X's ack of a command the face played
		}
	}
	r, cached := f.Cache.Reply(text)
	if !cached && isWrite(text) {
		// A write the radio never echoed: echoed as it would, over the
		// settings it last echoed (identity.go); the radio still gets it.
		if cur := f.settingsNow(); cur != "" {
			r, cached = settingsEcho(cur, text), true
		}
	}
	if cached {
		f.noteSettings(r)
	}
	if f.Forward != nil && (!cached || isWrite(text)) {
		if !cached {
			if f.owed == nil {
				f.owed = map[string]int{}
			}
			f.owed[text]++
		}
		f.Forward(text, now)
	}
	if cached {
		return []Out{{r, now.Add(ms_)}}
	}
	if f.Forward == nil {
		f.missing(text)
	}
	return nil
}

// isWrite reports whether a request changes the radio's settings, so that the
// real radio gets it even when the face answers it from the cache.
func isWrite(text string) bool { return hri.Key(text) == "D1M" }

// Answer delivers the real radio's reply to a forwarded request, if WIRES-X
// is still waiting on it.
func (f *Face) Answer(request, reply string, now time.Time) []Out {
	if f.owed[request] == 0 {
		return nil
	}
	f.owed[request]--
	f.noteSettings(reply)
	return []Out{{reply, now}}
}

// settingsNow is the radio's settings as the face last told WIRES-X of them,
// or as the radio last echoed them.
func (f *Face) settingsNow() string {
	if f.settings == "" {
		return f.Cache.LastSettings()
	}
	return f.settings
}

// noteSettings keeps the settings echoes WIRES-X is given, so that a write
// the cache has no echo for is echoed on top of them.
func (f *Face) noteSettings(reply string) {
	if hri.Key(reply) == "D1M" {
		f.settings = reply
	}
}

func (f *Face) missing(text string) {
	if f.unknown == nil {
		f.unknown = map[string]bool{}
	}
	if !f.unknown[text] {
		f.unknown[text] = true
		f.logf("face: no reply known to %.40q and no radio to ask; not answering", text)
	}
}

// poll answers a P command and follows WIRES-X's PTT: keyed ("P1...") starts
// the radio transmitting, unkeyed ends it.
func (f *Face) poll(text string, now time.Time) []Out {
	keyed := len(text) >= 2 && text[1] == '1'
	out := []Out{{f.bReply(f.rx != nil && f.rx.named), now.Add(ms_)}}
	switch {
	case keyed:
		if f.tx == nil {
			f.beginTx(text == pttData, now, &out)
		}
		t := f.tx
		t.pttAt = now
		if !t.keyed {
			t.keyed = true
			out = append(out, f.play(faceTxStart, now)...)
			if t.data {
				t.doneAt = now.Add(dataAirtime(t.dataLen))
			}
		}
	case f.tx != nil && f.tx.keyed:
		out = append(out, f.endTx(now)...)
	}
	return out
}

// transmission takes a D1E or D1F from WIRES-X: acked on the radio's clock.
func (f *Face) transmission(k, text string, now time.Time) []Out {
	var out []Out
	data := k == "D1F" && isDataPage(text)
	if f.tx == nil {
		f.beginTx(data, now, &out)
	}
	t := f.tx
	t.seenAt = now
	if k == "D1F" {
		if data {
			t.dataLen += len(text)
			if t.keyed && !t.done {
				t.doneAt = t.doneAt.Add(dataAirtime(len(text)) - dataAirtime(0))
			}
		}
		return append(out, Out{hri.Ack('F'), now.Add(35 * ms_)})
	}
	at := now.Add(ackLatency)
	if t.nextAck.After(at) {
		at = t.nextAck
	}
	t.nextAck = at.Add(ackInterval)
	return append(out, Out{hri.Ack('E'), at})
}

// isDataPage reports whether a D1F from WIRES-X is a data page (a reply to a
// command, the node's announcement: "20211000"), not voice.
func isDataPage(text string) bool {
	m, ok := hri.Parse(text)
	return ok && m.Cmd == 'F' && len(m.Payload) > 8 && m.Payload[:8] == dataPrefix
}

// dataPrefix starts WIRES-X's data pages.
const dataPrefix = "20211000"

func (f *Face) beginTx(data bool, now time.Time, out *[]Out) {
	if f.rx != nil {
		// A radio can't do both. WIRES-X sends only when told the radio is
		// idle, so this is room traffic crossing a local over: the local over
		// gives way (the router holds a client's for later).
		f.logf("face: WIRES-X transmitting, ending the %s reception it was shown", f.rx.src)
		*out = append(*out, f.play([]step{st(0, "04008"), st(stateStep, "04000")}, now)...)
		f.rx = nil
	}
	f.mirror = false
	f.tx = &faceTx{data: data, seenAt: now, nextAck: now}
}

func (f *Face) endTx(now time.Time) []Out {
	t := f.tx
	f.tx = nil
	if !t.keyed {
		return nil
	}
	if t.data {
		seq := faceDataEnd
		if !t.done {
			seq = append([]step{st(0, "00002")}, seq...)
		}
		return f.play(seq, now)
	}
	return f.play(faceTxEnd, now)
}

// Tick ends what has stopped: a reception whose frames stopped, a
// transmission WIRES-X stopped keying, and reports a data page sent.
func (f *Face) Tick(now time.Time) []Out {
	var out []Out
	if t := f.tx; t != nil {
		switch {
		case t.keyed && t.data && !t.done && !now.Before(t.doneAt):
			t.done = true
			out = append(out, f.play([]step{st(0, "00002")}, now)...)
		case t.keyed && now.Sub(t.pttAt) > pttLapse:
			f.logf("face: WIRES-X stopped keying without unkeying; ending its transmission")
			out = append(out, f.endTx(now)...)
		case !t.keyed && now.Sub(t.seenAt) > unkeyedLapse:
			f.tx = nil // never keyed: the radio never transmitted it
		}
	}
	if f.rx != nil && now.Sub(f.rx.last) > f.rx.hang() {
		out = append(out, f.EndRx(now)...)
	}
	if f.mirror && now.Sub(f.mirrorAt) > mirrorMax {
		f.logf("face: the radio's analog states lasted %v; showing it idle", mirrorMax)
		f.mirror = false
		out = append(out, f.play([]step{st(0, "04000")}, now)...)
	}
	return out
}

// Mirror shows WIRES-X a state of the real radio's own, for what the face
// can't make itself: an analog reception and the repeater's hang after it.
// It is shown only while the face has nothing of its own under way, and only
// receive and hang states ("?A??7", "0??07") and the idle that ends them.
func (f *Face) Mirror(s hri.State, now time.Time) []Out {
	if f.Busy(now) || len(s) != 5 {
		return nil
	}
	switch {
	case s[4] == '7' && (s.Receiving() || s[0] == '0' && s[1] != '0'):
		if !f.mirror {
			f.mirror, f.mirrorAt = true, now
		}
	case f.mirror && s.Idle():
		f.mirror = false
	default:
		return nil
	}
	if s == f.State(now) {
		return nil
	}
	return f.play([]step{st(0, s)}, now)
}

// StartRx starts playing a reception to WIRES-X, with its first callsign
// blocks: a client's over, whose header carries them.
func (f *Face) StartRx(src rxSource, csd1 [20]byte, now time.Time) []Out {
	out := f.BeginRx(src, now)
	f.rx.csd1 = csd1
	return append(out, f.Callsigns(now.Add(250*ms_))...)
}

// BeginRx starts a reception as the radio's receive states start, before its
// callsign blocks are known: one off the air, shown as the radio shows it.
func (f *Face) BeginRx(src rxSource, now time.Time) []Out {
	f.mirror = false
	f.rx = &reception{src: src, csd1: blank20, csd2: blank20, csd3: blank20, last: now}
	copy(f.rx.dt[:], blank20[:])
	return f.play(faceRxStart, now)
}

// Callsigns sends the reception's first callsign blocks (D1H 10 and 20) and
// the busy status that follows them, once.
func (f *Face) Callsigns(now time.Time) []Out {
	rx := f.rx
	if rx == nil || rx.named {
		return nil
	}
	rx.named = true
	f.logf("face: receiving %q (%s)", string(rx.csd1[10:]), rx.src)
	return []Out{
		f.emit(hri.D1('H', hri.CallsignPayload(hri.RadioPrefix, 1, 0, rx.csd1)), now),
		f.emit(hri.D1('H', hri.CallsignPayload(hri.RadioPrefix, 2, 0, rx.csd2)), now),
		f.emit(f.bReply(true), now),
	}
}

// RxData takes the data the real radio sent (its D1G: callsign blocks and
// DT) and, for a reception off the air, sends it on as the radio did, when
// it did: the radio's D1G carries the DT it has just completed.
func (f *Face) RxData(d hri.Data, now time.Time) []Out {
	rx := f.rx
	if rx == nil {
		return nil
	}
	rx.csd1, rx.csd2, rx.csd3, rx.dt = d.CSD1, d.CSD2, d.CSD3, d.DT
	if rx.src != rxAir {
		return nil
	}
	return []Out{f.emit(hri.D1('G', hri.RadioDataPayload(hri.RadioPrefix, rx.csd1, rx.csd2, rx.csd3, rx.dt)), now)}
}

// SetCSD sets a callsign block (0-2) of the reception, as it becomes known.
func (f *Face) SetCSD(i int, text [20]byte) {
	if f.rx == nil {
		return
	}
	switch i {
	case 0:
		f.rx.csd1 = text
	case 1:
		f.rx.csd2 = text
	case 2:
		f.rx.csd3 = text
	}
}

// SetDT sets the reception's DT data, as the latest frames 6 and 7 carry it.
func (f *Face) SetDT(dt [20]byte) {
	if f.rx != nil {
		f.rx.dt = dt
	}
}

// RxVoice plays one voice frame of the reception: as the HRI-200 does, a
// callsign block repeat before each of the first six, and the data (D1G)
// after the sixth and every seventh after it.
func (f *Face) RxVoice(blocks [5][ysf.VoiceBlockLength]byte, now time.Time) []Out {
	rx := f.rx
	if rx == nil {
		return nil
	}
	rx.last = now
	out := f.Callsigns(now)
	if k := rx.frames; k < 6 {
		block, repeat := k/2+1, k%2+1
		out = append(out, f.emit(hri.D1('H', hri.CallsignPayload(hri.RadioPrefix, block, repeat, rx.block(block))), now))
	}
	rx.frames++
	out = append(out, f.emit(hri.D1('R', hri.VoicePayload(uint16(5*rx.frames), blocks)), now))
	// A client's over has no radio to send its D1G: made here, on the
	// radio's cadence. One off the air gets the radio's own (RxData).
	if rx.src == rxClient && rx.frames >= 6 && (rx.frames-6)%7 == 0 {
		out = append(out, f.emit(hri.D1('G', hri.RadioDataPayload(hri.RadioPrefix, rx.csd1, rx.csd2, rx.csd3, rx.dt)), now))
	}
	return out
}

// EndRx ends the reception.
func (f *Face) EndRx(now time.Time) []Out {
	if f.rx == nil {
		return nil
	}
	f.logf("face: reception ended, %d frames (%s)", f.rx.frames, f.rx.src)
	f.rx = nil
	return f.play(faceRxEnd, now.Add(70*ms_))
}

// PlayCommand plays a WIRES-X command as the radio does one it hears: receive
// states, the command as a D1G with no voice, the end states (captured from an
// FT-5D on the repeater, 2026-11-14 04:58:53). WIRES-X acks it, asks whether
// the radio may send (D1B), and sends its reply as a data page.
//
// prefix is the D1G's first eight characters: the radio's own for a command
// heard off the air ("2100107F" for a node info request, captured); for a
// client's, as hri.CommandPayload makes it. A reception the face has already
// begun for the command (its receive states, no callsigns yet) carries on
// into it, as the radio's did.
func (f *Face) PlayCommand(prefix string, csd1, csd2, csd3 [20]byte, data []byte, now time.Time) []Out {
	var out []Out
	at := now
	if f.rx != nil && f.rx.src == rxAir && f.rx.frames == 0 && !f.rx.named {
		f.rx = nil
	} else {
		f.mirror = false
		out = f.play(faceRxStart, now)
		at = now.Add(485 * ms_)
	}
	payload := prefix + string(csd1[:]) + string(csd2[:]) + string(csd3[:]) + strings.ToUpper(hex.EncodeToString(data))
	out = append(out, f.emit(hri.D1('G', payload), at))
	out = append(out, f.play(faceCmdEnd, out[len(out)-1].At.Add(50*ms_))...)
	f.cmdTill = f.floor
	return out
}

// Inject plays one YSF frame from a client to WIRES-X as a reception. source
// is the YSF packet's source field, used when the header was missed. Only
// V/D Mode 2 (DN) voice is played; it is what the HRI-200 relays.
func (f *Face) Inject(frame []byte, source [10]byte, end bool, now time.Time) []Out {
	if f.tx != nil || (f.rx != nil && f.rx.src != rxClient) {
		return nil
	}
	fi, err := ysf.DecodeFICH(frame)
	if err != nil {
		return nil
	}
	switch fi.FI {
	case ysf.FIHeader:
		if f.rx != nil {
			return nil
		}
		csd1, _, err1, _ := ysf.DecodeHeader(frame)
		if err1 != nil {
			csd1 = sourceCSD1(source)
		}
		return f.StartRx(rxClient, csd1, now)
	case ysf.FITerminator:
		return f.EndRx(now)
	case ysf.FICommunications:
	default:
		return nil
	}
	if fi.DT != ysf.DTVDMode2 {
		if f.rx == nil {
			f.logf("face: dropping YSF data type %d; only V/D Mode 2 (DN) is played", fi.DT)
		}
		return nil
	}
	var out []Out
	if f.rx == nil { // the header was lost: start from the packet's source
		out = f.StartRx(rxClient, sourceCSD1(source), now)
	}
	if dch, err := ysf.DecodeDCH(frame); err == nil {
		takeDCH(f.rx, fi.FN, dch)
	}
	out = append(out, f.RxVoice(ysf.VoiceBlocks(frame), now)...)
	if end {
		out = append(out, f.EndRx(now)...)
	}
	return out
}

// takeDCH keeps what a frame's data channel carries: CSD3 in frames 4 and 5,
// the DT data in 6 and 7.
func takeDCH(rx *reception, fn byte, dch [10]byte) {
	switch fn {
	case 4:
		copy(rx.csd3[:10], dch[:])
	case 5:
		copy(rx.csd3[10:], dch[:])
	case 6:
		copy(rx.dt[:10], dch[:])
	case 7:
		copy(rx.dt[10:], dch[:])
	}
}

func sourceCSD1(source [10]byte) [20]byte {
	csd1 := blank20
	copy(csd1[:10], "**********")
	copy(csd1[10:], source[:])
	return csd1
}

// airHang ends a reception off the air if the radio says nothing of it for
// this long; normally its end states end it.
const airHang = 3 * time.Second

// hang is how long the reception may go without frames before it is ended.
func (r *reception) hang() time.Duration {
	if r.src == rxAir {
		return airHang
	}
	return rxHang
}

func (r *reception) block(n int) [20]byte {
	switch n {
	case 1:
		return r.csd1
	case 2:
		return r.csd2
	}
	return r.csd3
}

var blank20 = [20]byte{' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}
