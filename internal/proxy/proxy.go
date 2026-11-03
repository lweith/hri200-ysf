package proxy

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/lweith/hri200-ysf/internal/bridge"
	"github.com/lweith/hri200-ysf/internal/hri"
)

// YSF is a frame from a YSF client, to play to WIRES-X.
type YSF struct {
	Source [10]byte
	Frame  []byte
	End    bool
}

// Proxy connects WIRES-X (Downstream, the USB gadget's tty) to an HRI-200
// (Upstream, when it is attached).
type Proxy struct {
	Downstream string                 // tty WIRES-X talks to, such as /dev/ttyGS0
	Upstream   func() (string, error) // finds the HRI-200's tty, or errors if absent
	Open       func(path string) (*os.File, error)
	Cache      *Cache
	Bridge     *bridge.Bridge
	Log        func(format string, args ...any)
	Verbose    bool
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

// Run proxies until ctx ends. ysfIn carries frames to play to WIRES-X while
// the HRI-200 is absent.
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

	emu := &Emulator{Cache: p.Cache, Log: p.Log}
	var match Matcher
	var up *os.File
	var droppedYSF bool
	send := func(o []Out) {
		for _, x := range o {
			if p.Verbose {
				p.logf("emulated: %.60s", x.Text)
			}
			outs <- x
		}
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	lastSave := time.Now()
	defer p.Cache.Save()

	p.logf("proxy: WIRES-X on %s, %d cached replies", p.Downstream, p.Cache.Len())
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-downDone:
			return errors.New("proxy: lost " + p.Downstream)

		case f := <-upEvents:
			if f != nil {
				up = f
				p.logf("proxy: HRI-200 attached (%s): passing through", f.Name())
			} else {
				up = nil
				match = Matcher{}
				p.logf("proxy: HRI-200 gone: emulating from %d cached replies", p.Cache.Len())
				_ = p.Cache.Save()
			}

		case m := <-downMsgs:
			if p.Verbose {
				p.logf("network: %.60s", m.text)
			}
			if pm, ok := hri.Parse(m.text); ok {
				p.Bridge.Handle(bridge.Network, pm, m.at)
			}
			if up != nil {
				match.Request(m.text, m.at)
				if _, err := up.Write(hri.Wrap(m.text)); err != nil {
					p.logf("proxy: writing to HRI-200: %v", err)
				}
			} else {
				send(emu.Request(m.text, m.at))
			}

		case m := <-upMsgs:
			if p.Verbose {
				p.logf("radio: %.60s", m.text)
			}
			if pm, ok := hri.Parse(m.text); ok {
				p.Bridge.Handle(bridge.Radio, pm, m.at)
			}
			if req, ok := match.Reply(m.text, m.at); ok {
				p.Cache.Learn(req, m.text)
			}
			outs <- Out{m.text, m.at}

		case y := <-ysfIn:
			if up != nil {
				if !droppedYSF {
					p.logf("proxy: HRI-200 attached; YSF from clients is not played to WIRES-X")
					droppedYSF = true
				}
				continue
			}
			droppedYSF = false
			send(emu.Inject(y.Frame, y.Source, y.End, time.Now()))

		case now := <-tick.C:
			p.Bridge.Tick(now)
			if up == nil {
				send(emu.Tick(now))
			}
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

// writeOuts writes messages to WIRES-X in order, each no earlier than its time.
func writeOuts(ctx context.Context, w io.Writer, outs <-chan Out, logf func(string, ...any)) {
	for {
		select {
		case <-ctx.Done():
			return
		case o := <-outs:
			if d := time.Until(o.At); d > 0 {
				sleep(ctx, d)
			}
			if _, err := w.Write(hri.Wrap(o.Text)); err != nil {
				logf("proxy: writing to WIRES-X: %v", err)
			}
		}
	}
}
