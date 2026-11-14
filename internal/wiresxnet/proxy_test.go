package wiresxnet

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A faithful /relay/get pool reply from captures/2026-11-13.
const relaySample = `20261114113959
153.246.44.26 / 2400:4050:2c82:b400:60d0:b593:7e54:abb7 / 8310 / 20261114113859 / 277
153.246.53.180 / 2400:4053:3e45:2300:bdd0:46ca:bf67:55a0 / 8300 / 20261114113908 / 277
153.246.53.186 / 2400:4053:3e45:2300:8278:4d52:cb51:dccd / 8300 / 20261114113908 / 120
153.246.44.26 / 2400:4050:2c82:b400:60d0:b593:7e54:abb7 / 8300 / 20261114113930 / 281
`

func TestDiscoverRelays(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/relay/get" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, relaySample)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	relays, err := DiscoverRelays(context.Background(), host)
	if err != nil {
		t.Fatalf("DiscoverRelays: %v", err)
	}
	if len(relays) != 4 {
		t.Fatalf("got %d relays, want 4", len(relays))
	}
	// Least-loaded :8300 is .186 with load 120.
	r, ok := PickRelay(relays, 8300)
	if !ok || r.IPv4 != "153.246.53.186" || r.Load != 120 {
		t.Fatalf("PickRelay(8300) = %+v ok=%v", r, ok)
	}
	if r.Addr() != "153.246.53.186:8300" {
		t.Errorf("Addr = %q", r.Addr())
	}
}

func TestOwnPublicIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "20261114113959\n134.228.212.206\n")
	}))
	defer srv.Close()
	ip, err := OwnPublicIP(context.Background(), strings.TrimPrefix(srv.URL, "http://"))
	if err != nil || ip != "134.228.212.206" {
		t.Fatalf("OwnPublicIP = %q, %v", ip, err)
	}
}

// frame builds the wire bytes of a server directory-page message carrying one
// room, for the passthrough test.
func dirPageFrame() []byte {
	page := "@E00010001" + "FE0200" + "0A070004480" + "040ABloomer-RM" +
		"0611Bloomer,Wisconsin" + "0703USA" + "2C040003"
	body := "&R" + fmt.Sprintf("%04X", len(page)) + page
	return Frame{Body: []byte(body)}.Encode()
}

func TestProxyPassthrough(t *testing.T) {
	sent := dirPageFrame()

	// Fake upstream: on connect, send the frame, then drain the client's bytes
	// into gotUp so we can check the up direction too.
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upLn.Close()
	gotUp := make(chan []byte, 1)
	go func() {
		c, err := upLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.Write(sent)
		b, _ := io.ReadAll(c)
		gotUp <- b
	}()

	m := NewMetrics()
	p := &Proxy{Upstream: upLn.Addr().String(), Dir: NewDirectory(m), Metrics: m}
	pxLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pxLn.Close()
	go p.Serve(pxLn)

	client, err := net.Dial("tcp", pxLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// Send a poll up through the proxy, then read the page coming down.
	poll := []byte("hello-up")
	client.Write(poll)

	got := make([]byte, len(sent))
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read down: %v", err)
	}
	client.(*net.TCPConn).CloseWrite()

	// Down direction byte-exact.
	if string(got) != string(sent) {
		t.Fatalf("down bytes altered:\n got %q\nwant %q", got, sent)
	}
	// Up direction byte-exact.
	select {
	case up := <-gotUp:
		if string(up) != string(poll) {
			t.Fatalf("up bytes altered: got %q want %q", up, poll)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the up bytes")
	}

	// The observer on the down direction populated the directory.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := p.Dir.Lookup("0004480"); ok {
			client.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("proxy observer did not ingest the directory page")
}
