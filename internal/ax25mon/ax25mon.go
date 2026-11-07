// Package ax25mon follows AX.25 connected-mode sessions on a channel from the
// frames direwolf receives (its AGW port in raw mode, 'k'), so that room
// traffic can be kept off the channel while a session is open.
//
// A session between two stations opens with SABM or SABME, or is assumed
// open on any I or S frame between them (one that began before the tracker
// started), and closes with DISC or DM. direwolf reports only what it
// receives, not what it sends itself, so a session also closes after Idle
// without a frame from the far end: its RR polls keep a live one going.
package ax25mon

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lweith/hri200-ysf/internal/agw"
)

// Idle closes a session the far end has sent nothing in for this long.
const Idle = 10 * time.Minute

// Frame types (control field, P/F bit clear).
const (
	ctlSABM  = 0x2f
	ctlSABME = 0x6f
	ctlDISC  = 0x43
	ctlDM    = 0x0f
	ctlUA    = 0x63
)

// Tracker is the set of open sessions.
type Tracker struct {
	Log func(format string, args ...any)

	mu       sync.Mutex
	sessions map[string]time.Time // "A<>B" (sorted) -> last frame
}

func (t *Tracker) logf(format string, args ...any) {
	if t.Log != nil {
		t.Log(format, args...)
	}
}

func call(a []byte) string {
	var b strings.Builder
	for _, c := range a[:6] {
		if c>>1 != ' ' {
			b.WriteByte(c >> 1)
		}
	}
	if ssid := (a[6] >> 1) & 0x0f; ssid != 0 {
		fmt.Fprintf(&b, "-%d", ssid)
	}
	return b.String()
}

// Parse returns a frame's destination, source and control byte. The frame is
// AX.25 without its FCS.
func Parse(f []byte) (dst, src string, ctl byte, ok bool) {
	i := 0
	for i+7 <= len(f) {
		last := f[i+6]&1 == 1
		i += 7
		if last {
			break
		}
	}
	if i < 14 || i >= len(f) || f[i-1]&1 == 0 {
		return "", "", 0, false
	}
	return call(f[0:7]), call(f[7:14]), f[i], true
}

func key(a, b string) string {
	s := []string{a, b}
	sort.Strings(s)
	return s[0] + "<>" + s[1]
}

// Frame notes one received frame.
func (t *Tracker) Frame(f []byte, now time.Time) {
	dst, src, ctl, ok := Parse(f)
	if !ok {
		return
	}
	k := key(src, dst)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sessions == nil {
		t.sessions = map[string]time.Time{}
	}
	_, open := t.sessions[k]
	u := ctl &^ 0x10
	switch {
	case u == ctlSABM || u == ctlSABME:
		if !open {
			t.logf("ax25: session %s opening (%s)", k, src)
		}
		t.sessions[k] = now
	case u == ctlDISC || u == ctlDM:
		if open {
			t.logf("ax25: session %s closed (%s)", k, src)
			delete(t.sessions, k)
		}
	case ctl&1 == 0 || ctl&3 == 1 || u == ctlUA: // I, S, UA
		if !open && u != ctlUA {
			t.logf("ax25: session %s in progress", k)
		}
		if open || u != ctlUA {
			t.sessions[k] = now
		}
	}
}

// Active reports how many sessions are open.
func (t *Tracker) Active(now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, last := range t.sessions {
		if now.Sub(last) > Idle {
			t.logf("ax25: session %s idle %v, assumed closed", k, Idle)
			delete(t.sessions, k)
		}
	}
	return len(t.sessions)
}

// Run follows the frames direwolf receives on an AGW port until it fails,
// reconnecting every 10 s (direwolf may start after this).
func (t *Tracker) Run(addr string) {
	logged := false
	for {
		err := t.run(addr)
		if !logged {
			t.logf("ax25: monitor on %s: %v (retrying)", addr, err)
			logged = true
		}
		time.Sleep(10 * time.Second)
	}
}

func (t *Tracker) run(addr string) error {
	c, err := agw.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	// 'k': raw received frames. 'm': monitoring, for 'T', what direwolf
	// itself transmits (as text; its received frames come as text too, and
	// are ignored here).
	for _, k := range []byte{'k', 'm'} {
		if err := c.Send(agw.Frame{Kind: k}); err != nil {
			return err
		}
	}
	t.logf("ax25: following sessions on %s", addr)
	for {
		f, err := c.Recv()
		if err != nil {
			return err
		}
		switch {
		case f.Kind == 'K' && len(f.Data) > 1:
			// Raw: a KISS command byte (port << 4), then the frame.
			t.Frame(f.Data[1:], time.Now())
		case f.Kind == 'T':
			t.Own(f.From, f.To, string(f.Data))
		}
	}
}

// Own notes a frame direwolf transmitted, from its monitor text ("1:Fm A To B
// <DISC P>..."): its DISC or DM ends a session, which the far end's UA
// doesn't show.
func (t *Tracker) Own(from, to, text string) {
	if !strings.Contains(text, "<DISC") && !strings.Contains(text, "<DM") {
		return
	}
	k := key(strings.ToUpper(from), strings.ToUpper(to))
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, open := t.sessions[k]; open {
		t.logf("ax25: session %s closed (%s)", k, from)
		delete(t.sessions, k)
	}
}
