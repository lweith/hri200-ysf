package wiresxnet

import (
	"io"
	"net"
	"sync"
)

// The splice: a TCP proxy that sits between a WIRES-X node (WIRES-X.exe, or our
// own future node) and a Yaesu relay. For now it is a pure passthrough — it
// copies bytes verbatim in both directions and feeds a tee of each stream to an
// Observer, so the directory cache, talker stats and events fill from live
// traffic without altering a single byte. It never parses-and-re-emits, so it
// cannot corrupt the stream.
//
// This is deliberately NOT wired inline yet: nothing redirects the real node
// through it. Run it standalone (cmd/wiresxnet-proxy) and point a client at it,
// or leave it dormant. Interposing it in the live path (DNAT/TPROXY on the host)
// and turning on frame rewriting — serving the directory from cache, stripping
// Mic-E position, preferencing YSF — is a separate, deliberate step. The Rewrite
// hook below reserves the seam for it.
type Proxy struct {
	// Upstream is the relay to dial, host:port (e.g. "153.246.44.7:8300").
	Upstream string
	// Dir, Metrics, Broker are the shared state both directions update. Any may
	// be nil.
	Dir     *Directory
	Metrics *Metrics
	Broker  *Broker
	// Rewrite, if set, is called for every frame with its direction. It is an
	// observation hook only today (its return is ignored); it marks where inline
	// rewriting will attach. Direction is "up" (node->relay) or "down"
	// (relay->node).
	Rewrite func(dir string, m Message)
}

// Serve accepts connections on ln and proxies each to Upstream until ln closes.
func (p *Proxy) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go p.handle(c)
	}
}

// handle proxies one client connection to a freshly dialed upstream.
func (p *Proxy) handle(client net.Conn) {
	defer client.Close()
	up, err := net.Dial("tcp", p.Upstream)
	if err != nil {
		return
	}
	defer up.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.pump(up, client, "up") }()   // node -> relay
	go func() { defer wg.Done(); p.pump(client, up, "down") }() // relay -> node
	wg.Wait()
}

// pump copies src->dst byte-for-byte while an Observer reads a tee of the same
// bytes. The copy is never gated on the observer: a parse hiccup cannot stall or
// alter forwarded traffic. When src ends, dst's write side is half-closed so the
// peer sees EOF.
func (p *Proxy) pump(dst, src net.Conn, dir string) {
	pr, pw := io.Pipe()
	obs := &Observer{Dir: p.Dir, Metrics: p.Metrics, Broker: p.Broker}
	if p.Rewrite != nil {
		obs.OnFrame = func(m Message) { p.Rewrite(dir, m) }
	}
	done := make(chan struct{})
	go func() { _ = obs.Observe(pr); close(done) }()

	_, err := io.Copy(dst, io.TeeReader(src, pw))
	pw.CloseWithError(err) // unblocks the observer at the stream's end
	halfClose(dst)
	<-done
}

// halfClose shuts the write half of a TCP connection so the peer reads EOF while
// the other direction keeps flowing.
func halfClose(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}
