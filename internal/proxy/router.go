package proxy

import (
	"strings"
	"time"

	"github.com/lweith/hri200-ysf/internal/bridge"
	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/usage"
	"github.com/lweith/hri200-ysf/internal/wiresx"
	"github.com/lweith/hri200-ysf/internal/ysf"
)

// The router carries content between the two state machines, the YSF
// clients and the bridge; it is the only part that knows both exist.
//
//	off the air (Master events)  -> the face, as a reception; the bridge
//	WIRES-X's over (face input)  -> the master, as WIRES-X sent it; the bridge
//	WIRES-X's data page          -> the clients if it answers a client's
//	                                command, otherwise the master
//	a client's over              -> the face, as a reception; the master,
//	                                built as WIRES-X builds room traffic
//	a client's WIRES-X command   -> the face, as a command heard off the air
//
// Each side keeps its own state: the face answers WIRES-X whatever the radio
// does, and the master drives the radio whatever WIRES-X does.

const (
	// wiresxHold counts WIRES-X as busy this long after its last frame.
	wiresxHold = 3 * time.Second
	// wiresxQuiet holds packet transmissions until WIRES-X has sent nothing
	// for this long (the policy's default).
	wiresxQuiet = 60 * time.Second
	// packetQuiet is how long the radio stays in Analog after packet activity.
	packetQuiet = 20 * time.Second
	// commandWait drops a client's command that can't be played this long
	// after it arrived.
	commandWait = 5 * time.Second
	// replyWait is how long WIRES-X's reply to a client's command is waited
	// for. It answered a room list request 12 s after it, a node info request
	// after 1 s.
	replyWait = 20 * time.Second
	// frameTime is one YSF frame's air time.
	frameTime = 100 * time.Millisecond
	// roomPrefix starts the D1F WIRES-X sends the radio for room traffic.
	roomPrefix = "21216000"
	// headerAgain repeats the header after this many frames, as WIRES-X does.
	headerAgain = 6
	// dtGap spaces a header for a new DT from the one before.
	dtGap = 8
)

// defaultDT is a DT record for an over whose own hasn't arrived yet: no GPS,
// as an FT-5D sends it.
var defaultDT = [20]byte{0x07, 0x22, 0x61, 0x5F, 0x33, 0x03, 0x1F}

type router struct {
	face     *Face
	master   *Master // nil while the HRI-200 is away
	bridge   *bridge.Bridge
	log      func(format string, args ...any)
	transmit bool // clients' overs on the repeater too
	usage    *usage.Tracker
	policy   Policy

	wxTx     *Transmission // WIRES-X's over, for the radio
	wxMuted  bool          // WIRES-X's over kept off a packet session
	wiresxAt time.Time     // WIRES-X's last frame for the radio
	node     [10]byte      // the node's callsign, from WIRES-X's headers
	room     [5]byte       // the room, from WIRES-X's headers
	airHid   bool          // a reception off the air not shown (WIRES-X transmitting)
	// rfCommandAt is when a WIRES-X command was last heard off the air: a
	// reply within replyWait of it is keyed late, as WIRES-X keys one.
	rfCommandAt time.Time

	client clientOver
	wx     wxState
}

type clientOver struct {
	header   []byte // its header frame, for a late start
	waiting  bool   // waiting for the face
	last     time.Time
	tx       *Transmission // for the radio
	rx       reception     // what it carries, for the radio's headers
	sentDT   [20]byte
	headerAt int // frames sent when the last header was queued
	frames   int
	source   string
}

type wxState struct {
	asm      wiresx.Assembler
	command  *wiresx.Command // a client's, waiting for the face
	since    time.Time
	replyDue time.Time // until when WIRES-X's reply goes to the clients
	queue    []timedPacket
	seq      byte
}

type timedPacket struct {
	at time.Time
	p  bridge.Packet
}

func (r *router) logf(format string, args ...any) {
	if r.log != nil {
		r.log(format, args...)
	}
}

func (r *router) attach(m *Master, now time.Time) {
	r.master = m
	m.WIRESXBusy = r.wiresxBusy
	m.Start(now)
	if r.usage != nil {
		r.usage.Attached()
	}
}

func (r *router) detach(now time.Time) {
	r.master = nil
	r.wxTx, r.client.tx, r.airHid = nil, nil, false
	if r.usage != nil {
		r.usage.Detached(now)
	}
}

// wiresxBusy reports whether WIRES-X is transmitting, or did within the
// policy's quiet time: packet waits for it.
func (r *router) wiresxBusy(now time.Time) bool {
	if r.face.Transmitting() {
		return true
	}
	return !r.wiresxAt.IsZero() && now.Sub(r.wiresxAt) < r.policy.withDefaults().WIRESXQuiet
}

// packetHolds reports whether WIRES-X's traffic stays off the radio: packet
// has the channel and the policy gives it priority.
func (r *router) packetHolds(now time.Time) bool {
	return r.policy.withDefaults().Priority == PriorityPacket && r.master.PacketSession(now)
}

// FromWIRESX takes a message from WIRES-X.
func (r *router) FromWIRESX(text string, now time.Time) []Out {
	if m, ok := hri.Parse(text); ok {
		r.bridge.Handle(bridge.Network, m, now)
	}
	clientRx := r.face.Receiving() == rxClient
	out := r.face.Request(text, now)
	if clientRx && r.face.Receiving() == 0 {
		r.logf("router: WIRES-X transmitting; the client's over waits")
		r.client.waiting = true
	}
	k := hri.Key(text)
	switch {
	case (k == "D1E" || k == "D1F") && !hri.IsAck(text):
		if k == "D1F" && isDataPage(text) {
			r.wiresxData(text, now)
		} else {
			r.wiresxVoice(text, now)
		}
	case k == "P" && !r.face.Transmitting():
		r.endWIRESX()
	}
	return out
}

func (r *router) wiresxVoice(text string, now time.Time) {
	if m, ok := hri.Parse(text); ok && m.Cmd == 'F' && len(m.Payload) >= 68 {
		if m.Payload[28] != ' ' {
			copy(r.node[:], m.Payload[28:38])
		}
		if m.Payload[48] != ' ' {
			copy(r.room[:], m.Payload[48:53])
		}
	}
	if r.master == nil || r.wxMuted {
		return
	}
	if r.wxTx == nil {
		if r.packetHolds(now) {
			r.logf("router: packet has the channel: WIRES-X's over stays off the repeater")
			r.wxMuted = true
			return
		}
		r.wxTx = &Transmission{Kind: txVoice, Source: usage.WIRESX, Name: "WIRES-X"}
		r.master.Transmit(r.wxTx, now)
	}
	r.wiresxAt = now
	r.wxTx.Add(text, now)
}

func (r *router) endWIRESX() {
	if r.wxTx != nil {
		r.wxTx.End()
	}
	r.wxTx, r.wxMuted = nil, false
}

// wiresxData routes a data page: to the clients when it answers a client's
// command, otherwise to the radio as WIRES-X would send it.
func (r *router) wiresxData(text string, now time.Time) {
	m, _ := hri.Parse(text)
	c, ok := hri.ParseCommand(m)
	if ok && !r.wx.replyDue.IsZero() && now.Before(r.wx.replyDue) && wiresx.IsReply(c.Data) {
		r.wx.replyDue = time.Time{}
		frames := wiresx.Frames(wiresx.Command{CSD1: c.CSD1, CSD2: c.CSD2, CSD3: c.CSD3, Data: c.Data})
		r.logf("router: WIRES-X reply, %d bytes, to the clients in %d frames", len(c.Data), len(frames))
		var src, dst [10]byte
		copy(src[:], c.CSD1[10:])
		copy(dst[:], "ALL       ")
		at := now.Add(2 * frameTime)
		for i, f := range frames {
			r.wx.queue = append(r.wx.queue, timedPacket{at, bridge.Packet{
				Source: src, Dest: dst, Seq: r.wx.seq & 0x7F, End: i == len(frames)-1, Frame: f,
			}})
			r.wx.seq++
			at = at.Add(frameTime)
		}
		return
	}
	if r.master == nil {
		return
	}
	if r.packetHolds(now) {
		r.logf("router: packet has the channel: WIRES-X's data stays off the repeater")
		return
	}
	r.wiresxAt = now
	t := &Transmission{Kind: txDataPage, Source: usage.WIRESX, Name: "WIRES-X"}
	if ok && wiresx.IsReply(c.Data) && !r.rfCommandAt.IsZero() && now.Sub(r.rfCommandAt) < replyWait {
		// A reply to a command heard off the air: WIRES-X keyed 1.1 s and
		// 2.3 s after the radio acked it (2026-11-14 04:58:54, 04:59:19),
		// against 20 ms for its own announcement: the asking radio is back
		// on receive by then.
		t.KeyDelay = replyKeyDelay
		r.rfCommandAt = time.Time{}
	}
	t.Add(text, now)
	t.End()
	r.master.Transmit(t, now)
}

// FromRadio takes a message from the HRI-200.
func (r *router) FromRadio(text string, now time.Time) []Out {
	m := r.master
	if m == nil {
		return nil
	}
	if pm, ok := hri.Parse(text); ok {
		r.bridge.Handle(bridge.Radio, pm, now)
		if s, ok := hri.ParseState(pm); ok && r.usage != nil {
			r.usage.StateFrom(s, now, m.Source())
		}
	}
	var out []Out
	for _, ev := range m.FromRadio(text, now) {
		out = append(out, r.radioEvent(ev, now)...)
	}
	return out
}

func (r *router) radioEvent(ev MasterEvent, now time.Time) []Out {
	f := r.face
	switch ev.Kind {
	case RxStart:
		if f.Transmitting() || now.Before(f.cmdTill) {
			r.logf("router: the repeater is receiving while WIRES-X has the face; not shown")
			r.airHid = true
			return nil
		}
		var out []Out
		if f.Receiving() == rxClient {
			r.logf("router: the repeater is receiving; the client's over waits")
			out = f.EndRx(now)
			r.client.waiting = true
		}
		r.airHid = false
		return append(out, f.BeginRx(rxAir, now)...)
	case RxCSD:
		if f.Receiving() == rxAir {
			f.SetCSD(ev.Block, ev.Text)
			if ev.Block == 1 { // the radio's D1H 20 follows its 10 at once
				return f.Callsigns(now)
			}
		}
	case RxData:
		if f.Receiving() == rxAir {
			return f.RxData(ev.Data, now)
		}
	case RxVoice:
		if f.Receiving() == rxAir {
			return f.RxVoice(ev.Voice, now)
		}
	case RxEnd:
		r.airHid = false
		if f.Receiving() == rxAir {
			return f.EndRx(now)
		}
	case RxCommand:
		begun := f.Receiving() == rxAir && f.rx.frames == 0 && !f.rx.named
		if f.Busy(now) && !begun {
			r.logf("router: a WIRES-X command off the air while the face is busy; dropped")
			return nil
		}
		r.logf("router: WIRES-X %s off the air", wiresx.Name(ev.Command.Data))
		r.rfCommandAt = now
		c := ev.Command
		return f.PlayCommand(c.Prefix, c.CSD1, c.CSD2, c.CSD3, c.Data, now)
	case RxAnalog:
		return f.Mirror(ev.State, now)
	case Answer:
		return f.Answer(ev.Request, ev.Reply, now)
	}
	return nil
}

// FromClient takes a frame from a YSF client.
func (r *router) FromClient(y YSF, now time.Time) []Out {
	c := &r.client
	c.last = now
	fi, err := ysf.DecodeFICH(y.Frame)
	if err != nil {
		return nil
	}
	if fi.DT == ysf.DTDataFRMode {
		return r.commandFrame(y, now)
	}
	r.relayClient(y, fi, now)
	if fi.FI == ysf.FIHeader && r.face.Receiving() == 0 {
		c.header = y.Frame
	}
	defer func() {
		if y.End {
			c.header, c.waiting = nil, false
		}
	}()
	busy := r.face.Transmitting() || now.Before(r.face.cmdTill) ||
		r.face.Receiving() == rxAir || r.face.Receiving() == 0 && r.airHid
	if busy {
		if !c.waiting {
			r.logf("router: the face is busy, holding %q's over", string(y.Source[:]))
			c.waiting = true
		}
		return nil
	}
	var out []Out
	if c.waiting && r.face.Receiving() == 0 {
		c.waiting = false
		if c.header != nil && fi.FI != ysf.FIHeader {
			r.logf("router: the face is free, starting %q's over late", string(y.Source[:]))
			out = r.face.Inject(c.header, y.Source, false, now)
		}
	}
	return append(out, r.face.Inject(y.Frame, y.Source, y.End, now)...)
}

// relayClient builds a client's over for the radio, as WIRES-X sends room
// traffic (captured 2026-11-14, VA3CDV and DG1SR via room 30401): a D1F with
// the over's callsign blocks and its DT, prefix 21216000, of 0x52 characters
// for a short DT and 0x6C for a long one; the D1F again after the sixth
// frame and whenever the DT changes; a D1E per frame.
func (r *router) relayClient(y YSF, fi ysf.FICH, now time.Time) {
	c := &r.client
	if !r.transmit || r.master == nil {
		return
	}
	switch fi.FI {
	case ysf.FIHeader:
		if c.tx == nil {
			csd1, _, err1, _ := ysf.DecodeHeader(y.Frame)
			if err1 != nil {
				csd1 = sourceCSD1(y.Source)
			}
			r.startRelay(csd1, now)
		}
		return
	case ysf.FITerminator:
		r.endRelay()
		return
	case ysf.FICommunications:
	default:
		return
	}
	if fi.DT != ysf.DTVDMode2 {
		return
	}
	if c.tx == nil {
		r.startRelay(sourceCSD1(y.Source), now)
	}
	if dch, err := ysf.DecodeDCH(y.Frame); err == nil {
		takeDCH(&c.rx, fi.FN, dch)
	}
	c.frames++
	c.tx.Add(hri.D1('E', hri.VoicePayload(uint16(5*c.frames), ysf.VoiceBlocks(y.Frame))), now)
	// The header again after the sixth frame, then with each new DT, but no
	// sooner than dtGap frames after the last (WIRES-X: frames 0, 6, 14, 30).
	newDT := c.rx.dt != c.sentDT && validDT(c.rx.dt) && c.frames-c.headerAt >= dtGap
	if c.frames == headerAgain || newDT {
		c.tx.Add(r.roomHeader(), now)
	}
	if y.End {
		r.endRelay()
	}
}

func (r *router) startRelay(csd1 [20]byte, now time.Time) {
	c := &r.client
	c.rx = reception{csd1: csd1, csd2: blank20, csd3: blank20}
	c.rx.dt, c.sentDT, c.frames = defaultDT, [20]byte{}, 0
	c.source = strings.TrimRight(string(csd1[10:]), " ")
	c.tx = &Transmission{Kind: txVoice, Source: usage.Hotspot, Name: c.source}
	c.tx.Add(r.roomHeader(), now)
	r.master.Transmit(c.tx, now)
}

func (r *router) endRelay() {
	if r.client.tx != nil {
		r.client.tx.End()
		r.client.tx = nil
	}
}

// validDT reports whether dt holds a DT record through its end and sum.
func validDT(dt [20]byte) bool {
	for i := len(dt) - 2; i >= 1; i-- {
		if dt[i] != 0x03 {
			continue
		}
		var sum byte
		for _, c := range dt[:i+1] {
			sum += c
		}
		if dt[i+1] == sum {
			return true
		}
	}
	return false
}

// roomHeader is the D1F for the client's over, as WIRES-X builds one: CSD1
// as the radio sent it, CSD2 the node and the station, CSD3 the room, then
// the radio's ID as in its own CSD3, and the DT as it last arrived.
func (r *router) roomHeader() string {
	c := &r.client
	csd2, csd3 := blank20, blank20
	if r.node[0] != 0 {
		copy(csd2[:10], r.node[:])
	}
	copy(csd2[10:], c.rx.csd1[10:])
	if r.room[0] != 0 {
		copy(csd3[:5], r.room[:])
	}
	copy(csd3[10:], c.rx.csd3[10:])
	c.sentDT, c.headerAt = c.rx.dt, c.frames
	return hri.D1('F', hri.RadioDataPayload(roomPrefix, c.rx.csd1, csd2, csd3, c.rx.dt))
}

// commandFrame takes a client's Data FR frame: a WIRES-X command.
func (r *router) commandFrame(y YSF, now time.Time) []Out {
	c, ok := r.wx.asm.Frame(y.Frame)
	if !ok {
		return nil
	}
	r.logf("router: WIRES-X %s from %q", wiresx.Name(c.Data), string(y.Source[:]))
	r.wx.command, r.wx.since = &c, now
	return r.playCommand(now)
}

// playCommand plays a client's waiting command to WIRES-X once the face is free.
func (r *router) playCommand(now time.Time) []Out {
	c := r.wx.command
	if c == nil {
		return nil
	}
	if now.Sub(r.wx.since) > commandWait {
		r.logf("router: the face stayed busy, dropping the WIRES-X %s", wiresx.Name(c.Data))
		r.wx.command = nil
		return nil
	}
	if r.face.Busy(now) || !r.wx.replyDue.IsZero() {
		return nil
	}
	r.wx.command = nil
	r.wx.replyDue = now.Add(replyWait)
	prefix := hri.CommandPayload(c.CSD1, c.CSD2, c.CSD3, c.Data)[:8]
	return r.face.PlayCommand(prefix, c.CSD1, c.CSD2, c.CSD3, c.Data, now)
}

// PTT takes the packet modem's PTT.
func (r *router) PTT(on bool, now time.Time) {
	if r.master == nil {
		if on {
			r.logf("router: PTT, but the HRI-200 is away")
		}
		return
	}
	r.master.PTT(on, now)
}

// TXInhibited reports whether the packet modem should hold its frames.
func (r *router) TXInhibited(now time.Time) bool {
	return r.master == nil || r.master.TXInhibited(now)
}

// Tick moves both sides along. It returns messages for WIRES-X and frames
// for the clients.
func (r *router) Tick(now time.Time) ([]Out, []bridge.Packet) {
	out := r.face.Tick(now)
	if !r.face.Transmitting() && (r.wxTx != nil || r.wxMuted) {
		r.endWIRESX()
	}
	c := &r.client
	if now.Sub(c.last) > rxHang {
		if r.face.Receiving() == 0 {
			c.header, c.waiting = nil, false
		}
		r.endRelay()
	}
	if r.wx.command != nil {
		out = append(out, r.playCommand(now)...)
	}
	if !r.wx.replyDue.IsZero() && now.After(r.wx.replyDue) {
		r.logf("router: no WIRES-X reply to the client's command")
		r.wx.replyDue = time.Time{}
	}
	var due []bridge.Packet
	for len(r.wx.queue) > 0 && !now.Before(r.wx.queue[0].at) {
		due = append(due, r.wx.queue[0].p)
		r.wx.queue = r.wx.queue[1:]
	}
	if r.master != nil {
		r.master.Tick(now)
	}
	return out, due
}
