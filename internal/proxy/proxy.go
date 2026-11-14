package proxy

import (
	"context"
	"errors"
	"io"
	"os"
	"sort"
	"time"

	"github.com/lweith/hri200-ysf/internal/bridge"
	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/usage"
)

// YSF is a frame from a YSF client, to play to WIRES-X.
type YSF struct {
	Source [10]byte
	Frame  []byte
	End    bool
}

// Proxy connects WIRES-X (Downstream, the USB gadget's tty) to an HRI-200
// (Upstream, when it is attached), as two state machines that never see each
// other's messages: the face answers WIRES-X, the master drives the radio,
// and the router carries content between them (face.go, master.go,
// router.go).
type Proxy struct {
	Downstream string                 // tty WIRES-X talks to, such as /dev/ttyGS0
	Upstream   func() (string, error) // finds the HRI-200's tty, or errors if absent
	Open       func(path string) (*os.File, error)
	Cache      *Cache
	Bridge     *bridge.Bridge
	Log        func(format string, args ...any)
	Verbose    bool
	// Transmit also transmits clients' overs on the HRI-200's radio while it
	// is attached, so the repeater hears them.
	Transmit bool
	// Usage, if set, times the repeater's overs from the HRI-200's states.
	Usage *usage.Tracker
	// PTT, if set, carries a packet modem's PTT: the HRI-200's radio is
	// switched to Analog and keyed while it is on (see master_analog.go).
	PTT <-chan bool
	// Sessions, if set, counts the AX.25 sessions open on the radio's
	// channel: room traffic stays off the repeater while there are any, with
	// the packet priority.
	Sessions func(now time.Time) int
	// TXInhibit, if set, is told whenever the channel turns busy or clear for
	// the packet modem (direwolf's TXINH).
	TXInhibit func(on bool)
	// HangZero, with PTT: no repeater hang time while in Analog for packet.
	HangZero bool
	// Policy is the channel's sharing rules (policy.go).
	Policy Policy
	// Recover, if set, resets the HRI-200 when it stops behaving (health.go):
	// level 1 its USB device, 2 its port's power.
	Recover func(level int) error
}

func (p *Proxy) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

type msg struct {
	text string
	at   time.Time
}

// Run proxies until ctx ends. ysfIn carries frames from YSF clients.
func (p *Proxy) Run(ctx context.Context, ysfIn <-chan YSF) error {
	down, err := p.Open(p.Downstream)
	if err != nil {
		return err
	}
	defer down.Close()

	downMsgs := make(chan msg, 64)
	downDone := make(chan struct{})
	go readMessages(down, downMsgs, downDone)

	outs := make(chan Out, 256)
	go writeOuts(ctx, down, outs, p.logf)

	upMsgs := make(chan msg, 64)
	upEvents := make(chan *os.File)
	go p.watchUpstream(ctx, upEvents, upMsgs)

	r := &router{
		face:     &Face{Cache: p.Cache, Log: p.Log},
		bridge:   p.Bridge,
		log:      p.Log,
		transmit: p.Transmit,
		usage:    p.Usage,
		policy:   p.Policy,
	}
	r.face.Forward = func(text string, now time.Time) {
		if r.master != nil {
			r.master.Query(text)
		}
	}
	ladder := &Ladder{}
	send := func(o []Out) {
		for _, x := range o {
			if p.Verbose {
				p.logf("to WIRES-X: %.60s", x.Text)
			}
			outs <- x
		}
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	lastSave := time.Now()
	defer p.Cache.Save()
	defer func() {
		if r.master != nil {
			r.master.Stop()
			time.Sleep(300 * time.Millisecond)
		}
	}()

	// The packet modem's TX inhibit, written only when it changes.
	inhibited, inhibitKnown := false, false
	inhibit := func(now time.Time) {
		if p.TXInhibit == nil {
			return
		}
		on := r.TXInhibited(now)
		if inhibitKnown && on == inhibited {
			return
		}
		inhibited, inhibitKnown = on, true
		if p.Verbose {
			p.logf("proxy: TX inhibit %v", on)
		}
		p.TXInhibit(on)
	}
	defer func() {
		if p.TXInhibit != nil {
			p.TXInhibit(true)
		}
	}()

	p.logf("proxy: WIRES-X on %s, %d cached replies, %s", p.Downstream, p.Cache.Len(), p.Cache.Describe())
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-downDone:
			return errors.New("proxy: lost " + p.Downstream)

		case f := <-upEvents:
			now := time.Now()
			if f == nil {
				r.detach(now)
				p.logf("proxy: HRI-200 gone; WIRES-X sees no difference")
				_ = p.Cache.Save()
				continue
			}
			radio := f
			m := &Master{
				Write: func(text string) {
					if p.Verbose {
						p.logf("to radio: %.60s", text)
					}
					if _, err := radio.Write(hri.Wrap(text)); err != nil {
						p.logf("proxy: writing to HRI-200: %v", err)
					}
				},
				Log:      p.Log,
				Cache:    p.Cache,
				Recover:  p.Recover,
				Ladder:   ladder,
				HangZero: p.HangZero,
				Policy:   p.Policy,
				Sessions: p.Sessions,
			}
			r.attach(m, now)
			p.logf("proxy: HRI-200 attached (%s)", f.Name())

		case m := <-downMsgs:
			if p.Verbose {
				p.logf("WIRES-X: %.60s", m.text)
			}
			send(r.FromWIRESX(m.text, m.at))

		case m := <-upMsgs:
			if p.Verbose {
				p.logf("radio: %.60s", m.text)
			}
			send(r.FromRadio(m.text, m.at))
			inhibit(m.at)

		case on := <-p.PTT:
			r.PTT(on, time.Now())

		case y := <-ysfIn:
			send(r.FromClient(y, time.Now()))

		case now := <-tick.C:
			out, packets := r.Tick(now)
			send(out)
			for _, pk := range packets {
				if p.Bridge.Out != nil {
					p.Bridge.Out(pk)
				}
			}
			p.Bridge.Tick(now)
			inhibit(now)
			if now.Sub(lastSave) > 10*time.Second {
				if err := p.Cache.Save(); err != nil {
					p.logf("proxy: saving cache: %v", err)
				}
				lastSave = now
			}
		}
	}
}

// watchUpstream opens the HRI-200 whenever it is present, reports it on events
// (nil when it goes away), and reads its messages.
func (p *Proxy) watchUpstream(ctx context.Context, events chan<- *os.File, msgs chan<- msg) {
	for ctx.Err() == nil {
		path, err := p.Upstream()
		if err != nil {
			sleep(ctx, 2*time.Second)
			continue
		}
		f, err := p.Open(path)
		if err != nil {
			p.logf("proxy: %v", err)
			sleep(ctx, 2*time.Second)
			continue
		}
		events <- f
		done := make(chan struct{})
		go readMessages(f, msgs, done)
		select {
		case <-done:
		case <-ctx.Done():
			// Left open: Run may still write to it as it stops (unkeying,
			// switching the radio back to Digital). It closes with the process.
			return
		}
		f.Close()
		events <- nil
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// readMessages splits a tty's bytes into messages until it fails.
func readMessages(r io.Reader, out chan<- msg, done chan<- struct{}) {
	if done != nil {
		defer close(done)
	}
	var s hri.Splitter
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		now := time.Now()
		for _, t := range s.Write(buf[:n]) {
			out <- msg{t, now}
		}
		if err != nil {
			return
		}
	}
}

// outQueue holds messages for WIRES-X in order of their time, and of arrival
// among equal times.
type outQueue []Out

func (q *outQueue) push(o Out) {
	i := sort.Search(len(*q), func(i int) bool { return (*q)[i].At.After(o.At) })
	*q = append(*q, Out{})
	copy((*q)[i+1:], (*q)[i:])
	(*q)[i] = o
}

// writeOuts writes messages to WIRES-X, each at its time: a poll's answer goes
// out at once even while a state sequence is still unfolding.
func writeOuts(ctx context.Context, w io.Writer, outs <-chan Out, logf func(string, ...any)) {
	var q outQueue
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		for len(q) > 0 && !time.Now().Before(q[0].At) {
			if _, err := w.Write(hri.Wrap(q[0].Text)); err != nil {
				logf("proxy: writing to WIRES-X: %v", err)
			}
			q = q[1:]
		}
		wait := time.Hour
		if len(q) > 0 {
			wait = time.Until(q[0].At)
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case o := <-outs:
			q.push(o)
		case <-timer.C:
		}
	}
}
