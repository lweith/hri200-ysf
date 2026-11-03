package proxy

import (
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/ysf"
)

// Out is a message for WIRES-X, to be written no earlier than At.
type Out struct {
	Text string
	At   time.Time
}

const (
	// ackInterval is how fast the HRI-200 acks D1E voice: one per frame time.
	// WIRES-X waits for the ack, so this paces its transmission.
	ackInterval = 100 * time.Millisecond
	// txHang ends a transmission this long after its last voice frame.
	txHang = 300 * time.Millisecond
	// rxHang ends an injected reception this long after its last frame.
	rxHang = 400 * time.Millisecond
	// stateStep spaces the D1P states of a sequence.
	stateStep = 10 * time.Millisecond
)

// Emulator answers WIRES-X in place of an absent HRI-200.
type Emulator struct {
	Cache *Cache
	Log   func(format string, args ...any)

	state   hri.State
	tx      bool
	lastE   time.Time
	nextAck time.Time
	rx      *reception
	unknown map[string]bool
}

type reception struct {
	csd1, csd2, csd3 [20]byte
	dt               [20]byte
	frames           int
	last             time.Time
}

func (e *Emulator) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log(format, args...)
	}
}

// current is the state D1C reports: the last one sent, idle at first.
func (e *Emulator) current() hri.State {
	if e.state == "" {
		return "04000"
	}
	return e.state
}

// states reports a sequence of D1P states, ending in the last.
func (e *Emulator) states(seq []hri.State, now time.Time) []Out {
	out := make([]Out, len(seq))
	for i, s := range seq {
		out[i] = Out{hri.D1('P', hri.StatePayload(s)), now.Add(time.Duration(i) * stateStep)}
	}
	e.state = seq[len(seq)-1]
	return out
}

// Request answers one message from WIRES-X.
func (e *Emulator) Request(text string, now time.Time) []Out {
	switch hri.Key(text) {
	case "P":
		r, ok := e.Cache.Reply(text)
		if !ok {
			e.missing(text)
			return nil
		}
		if e.rx != nil && len(r) >= 2 {
			r = "B1" + r[2:] // busy: receiving
		}
		return []Out{{r, now}}
	case "D1C":
		return []Out{{hri.D1('C', hri.StatePayload(e.current())), now}}
	case "D1E":
		m, ok := hri.Parse(text)
		if !ok || len(m.Payload) == 1 {
			return nil
		}
		var out []Out
		if !e.tx {
			out = e.startTx(now)
		}
		e.lastE = now
		at := now
		if e.nextAck.After(at) {
			at = e.nextAck
		}
		e.nextAck = at.Add(ackInterval)
		return append(out, Out{hri.Ack('E'), at})
	case "D1F":
		out := []Out{{hri.Ack('F'), now}}
		if m, ok := hri.Parse(text); ok {
			if _, hdr := hri.ParseTxHeader(m); hdr && !e.tx {
				out = append(out, e.startTx(now.Add(stateStep))...)
			}
		}
		return out
	}
	if text == hri.Ack('G') {
		return nil
	}
	if r, ok := e.Cache.Reply(text); ok {
		return []Out{{r, now}}
	}
	e.missing(text)
	return nil
}

func (e *Emulator) startTx(now time.Time) []Out {
	e.tx = true
	e.nextAck = now
	if e.rx != nil {
		e.logf("emulator: WIRES-X transmitting, dropping the injected reception")
		e.rx = nil
	}
	return e.states(hri.TxStart, now)
}

func (e *Emulator) missing(text string) {
	if e.unknown == nil {
		e.unknown = map[string]bool{}
	}
	if !e.unknown[text] {
		e.unknown[text] = true
		e.logf("emulator: no cached reply to %.40q; not answering", text)
	}
}

// Tick ends transmissions and receptions whose frames have stopped.
func (e *Emulator) Tick(now time.Time) []Out {
	var out []Out
	if e.tx && now.Sub(e.lastE) > txHang && !now.Before(e.nextAck) {
		e.tx = false
		out = append(out, e.states(hri.TxEnd, now)...)
	}
	if e.rx != nil && now.Sub(e.rx.last) > rxHang {
		out = append(out, e.endRx(now)...)
	}
	return out
}

// Transmitting reports whether WIRES-X is sending to the emulated radio.
func (e *Emulator) Transmitting() bool { return e.tx }

var blank20 = [20]byte{' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}

// Inject plays one YSF frame to WIRES-X as if the radio had received it off
// the air. source is the YSF packet's source field, used when the header was
// missed. Only V/D Mode 2 (DN) voice is passed; it is what the HRI-200 relays.
func (e *Emulator) Inject(frame []byte, source [10]byte, end bool, now time.Time) []Out {
	if e.tx {
		return nil // WIRES-X has the channel
	}
	f, err := ysf.DecodeFICH(frame)
	if err != nil {
		return nil
	}
	switch f.FI {
	case ysf.FIHeader:
		if e.rx != nil {
			return nil
		}
		csd1, _, err1, _ := ysf.DecodeHeader(frame)
		if err1 != nil {
			csd1 = blank20
			copy(csd1[:10], "**********")
			copy(csd1[10:], source[:])
		}
		return e.startRx(csd1, now)
	case ysf.FITerminator:
		if e.rx == nil {
			return nil
		}
		return e.endRx(now)
	case ysf.FICommunications:
	default:
		return nil
	}
	if f.DT != ysf.DTVDMode2 {
		if e.rx == nil {
			e.logf("emulator: dropping YSF data type %d; only V/D Mode 2 (DN) is relayed", f.DT)
		}
		return nil
	}
	var out []Out
	if e.rx == nil { // the header was lost: start from the packet's source
		csd1 := blank20
		copy(csd1[:10], "**********")
		copy(csd1[10:], source[:])
		out = e.startRx(csd1, now)
	}
	rx := e.rx
	rx.last = now
	if dch, err := ysf.DecodeDCH(frame); err == nil {
		switch f.FN {
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
	// The HRI-200 repeats each callsign block twice more, one before each of
	// the first six voice messages.
	if k := rx.frames; k < 6 {
		block, repeat := k/2+1, k%2+1
		out = append(out, Out{hri.D1('H', hri.CallsignPayload(hri.RadioPrefix, block, repeat, rx.block(block))), now})
	}
	rx.frames++
	out = append(out, Out{hri.D1('R', hri.VoicePayload(uint16(5*rx.frames), ysf.VoiceBlocks(frame))), now})
	if rx.frames%8 == 7 { // as captured: after the 7th, 15th, ... voice message
		out = append(out, Out{hri.D1('G', hri.DataPayload(hri.RadioPrefix, rx.csd1, rx.csd2, rx.csd3, rx.dt)), now})
	}
	if end {
		out = append(out, e.endRx(now)...)
	}
	return out
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

func (e *Emulator) startRx(csd1 [20]byte, now time.Time) []Out {
	// CSD2 (downlink, uplink) stays blank, as for a station heard directly.
	e.rx = &reception{csd1: csd1, csd2: blank20, csd3: blank20, last: now}
	copy(e.rx.dt[:], blank20[:])
	e.logf("emulator: receiving %q", string(csd1[10:]))
	out := e.states(hri.RxStart, now)
	at := now.Add(time.Duration(len(hri.RxStart)) * stateStep)
	out = append(out,
		Out{hri.D1('H', hri.CallsignPayload(hri.RadioPrefix, 1, 0, e.rx.csd1)), at},
		Out{hri.D1('H', hri.CallsignPayload(hri.RadioPrefix, 2, 0, e.rx.csd2)), at})
	return out
}

func (e *Emulator) endRx(now time.Time) []Out {
	e.logf("emulator: reception ended, %d frames", e.rx.frames)
	e.rx = nil
	return e.states(hri.RxEnd, now)
}
