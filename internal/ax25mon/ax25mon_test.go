package ax25mon

import (
	"testing"
	"time"
)

func addr(call string, ssid byte, last bool) []byte {
	b := make([]byte, 7)
	for i := range 6 {
		c := byte(' ')
		if i < len(call) {
			c = call[i]
		}
		b[i] = c << 1
	}
	b[6] = 0x60 | ssid<<1
	if last {
		b[6] |= 1
	}
	return b
}

func frame(dst string, dssid byte, src string, sssid byte, ctl byte) []byte {
	f := append(addr(dst, dssid, false), addr(src, sssid, true)...)
	return append(f, ctl)
}

var t0 = time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC)

func TestParse(t *testing.T) {
	dst, src, ctl, ok := Parse(frame("KN6FLN", 11, "KN6FLN", 0, 0x3f))
	if !ok || dst != "KN6FLN-11" || src != "KN6FLN" || ctl != 0x3f {
		t.Errorf("Parse = %q %q %#x %v", dst, src, ctl, ok)
	}
}

func TestSessionLifecycle(t *testing.T) {
	var tr Tracker
	tr.Frame(frame("KN6FLN", 11, "KN6FLN", 0, 0x7f), t0) // SABME, P
	if n := tr.Active(t0); n != 1 {
		t.Fatalf("after SABME: %d sessions", n)
	}
	tr.Frame(frame("KN6FLN", 11, "KN6FLN", 0, 0x00), t0.Add(time.Minute)) // I
	if n := tr.Active(t0.Add(Idle + 30*time.Second)); n != 1 {
		t.Fatalf("an I frame didn't keep the session: %d", n)
	}
	tr.Frame(frame("KN6FLN", 11, "KN6FLN", 0, 0x53), t0.Add(2*time.Minute)) // DISC, P
	if n := tr.Active(t0.Add(2 * time.Minute)); n != 0 {
		t.Fatalf("after DISC: %d sessions", n)
	}
}

func TestOwnDISCCloses(t *testing.T) {
	var tr Tracker
	tr.Frame(frame("KN6FLN", 11, "KN6FLN", 0, 0x7f), t0)
	tr.Own("KN6FLN-11", "KN6FLN", "1:Fm KN6FLN-11 To KN6FLN <I R0 S0>")
	if n := tr.Active(t0); n != 1 {
		t.Fatalf("an I frame closed it: %d", n)
	}
	tr.Own("KN6FLN-11", "KN6FLN", "1:Fm KN6FLN-11 To KN6FLN <DISC P>[01:32:47]\r")
	if n := tr.Active(t0); n != 0 {
		t.Errorf("after our DISC: %d sessions", n)
	}
}

func TestSessionIdlesOut(t *testing.T) {
	var tr Tracker
	tr.Frame(frame("KN6FLN", 11, "KN6FLN", 0, 0x2f), t0)
	if n := tr.Active(t0.Add(Idle + time.Second)); n != 0 {
		t.Errorf("idle session: %d", n)
	}
}

func TestUIIsNotASession(t *testing.T) {
	var tr Tracker
	tr.Frame(frame("APRS", 0, "KN6FLN", 0, 0x03), t0)
	tr.Frame(frame("KN6FLN", 11, "KN6FLN", 0, 0x73), t0) // UA, F, unopened
	if n := tr.Active(t0); n != 0 {
		t.Errorf("UI and a stray UA: %d sessions", n)
	}
}
