// Package reflector is a send-only YSF reflector: clients (YSFGateway, or
// MMDVMHost directly) link with polls and receive every transmission the bridge
// produces. Audio sent by clients is dropped.
package reflector

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lweith/hri200-ysf/internal/bridge"
)

// Expiry drops a client that hasn't polled for this long. Clients poll every 5 s.
const Expiry = 60 * time.Second

// Reflector serves YSF clients on one UDP socket.
type Reflector struct {
	Gateway     string // callsign in the packets' gateway field
	ID          int    // five digits, in status replies
	Name        string // 16 characters, in status replies
	Description string // 14 characters, in status replies
	Log         func(format string, args ...any)

	conn    *net.UDPConn
	mu      sync.Mutex
	clients map[string]*client
}

type client struct {
	addr     *net.UDPAddr
	callsign string
	last     time.Time
}

func (r *Reflector) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

// Listen opens the socket.
func (r *Reflector) Listen(addr string) error {
	a, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	r.conn, err = net.ListenUDP("udp", a)
	r.clients = map[string]*client{}
	return err
}

// Addr is the listening address.
func (r *Reflector) Addr() net.Addr { return r.conn.LocalAddr() }

// Serve handles client packets until the socket is closed.
func (r *Reflector) Serve() error {
	buf := make([]byte, 512)
	dropped := 0
	for {
		n, addr, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			return err
		}
		if n < 4 {
			continue
		}
		p := buf[:n]
		switch string(p[:4]) {
		case "YSFP":
			r.poll(addr, callsign(p))
		case "YSFU":
			r.unlink(addr)
		case "YSFS":
			r.status(addr)
		case "YSFD":
			if dropped%100 == 0 {
				r.logf("dropping audio from %s: this reflector only sends", addr)
			}
			dropped++
		}
	}
}

func callsign(p []byte) string {
	if len(p) < 14 {
		return ""
	}
	return string(p[4:14])
}

func (r *Reflector) poll(addr *net.UDPAddr, call string) {
	r.mu.Lock()
	c, ok := r.clients[addr.String()]
	if !ok {
		c = &client{addr: addr, callsign: call}
		r.clients[addr.String()] = c
		r.logf("linked: %s (%s)", call, addr)
	}
	c.last = time.Now()
	r.mu.Unlock()
	reply := append([]byte("YSFP"), pad("REFLECTOR", 10)...)
	_, _ = r.conn.WriteToUDP(reply, addr)
}

func (r *Reflector) unlink(addr *net.UDPAddr) {
	r.mu.Lock()
	if c, ok := r.clients[addr.String()]; ok {
		r.logf("unlinked: %s (%s)", c.callsign, addr)
		delete(r.clients, addr.String())
	}
	r.mu.Unlock()
}

func (r *Reflector) status(addr *net.UDPAddr) {
	r.mu.Lock()
	count := len(r.clients)
	r.mu.Unlock()
	reply := fmt.Sprintf("YSFS%05d%s%s%03d", r.ID%100000, pad(r.Name, 16), pad(r.Description, 14), count%1000)
	_, _ = r.conn.WriteToUDP([]byte(reply), addr)
}

// Send sends a frame to every linked client.
func (r *Reflector) Send(p bridge.Packet) {
	pkt := make([]byte, 155)
	copy(pkt, "YSFD")
	copy(pkt[4:14], pad(r.Gateway, 10))
	copy(pkt[14:24], p.Source[:])
	copy(pkt[24:34], p.Dest[:])
	pkt[34] = p.Seq << 1
	if p.End {
		pkt[34] |= 1
	}
	copy(pkt[35:], p.Frame)

	r.mu.Lock()
	defer r.mu.Unlock()
	for k, c := range r.clients {
		if time.Since(c.last) > Expiry {
			r.logf("expired: %s (%s)", c.callsign, c.addr)
			delete(r.clients, k)
			continue
		}
		_, _ = r.conn.WriteToUDP(pkt, c.addr)
	}
}

// Close closes the socket, which ends Serve.
func (r *Reflector) Close() error { return r.conn.Close() }

func pad(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s + fmt.Sprintf("%*s", n-len(s), "")
}
