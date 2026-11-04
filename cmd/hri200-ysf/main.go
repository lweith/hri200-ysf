// hri200-ysf turns the serial traffic between WIRES-X and a Yaesu HRI-200 into
// YSF, served as a small YSF reflector.
//
// Sniffer (default): reads the traffic with usbmon and never writes to the
// HRI-200. With -cache it also learns the HRI-200's replies, for the proxy.
//
//	hri200-ysf                          live, from the HRI-200's usbmon bus (root)
//	hri200-ysf -pcap capture.pcap       replay a tcpdump capture in real time
//
// Proxy (-proxy): sits between WIRES-X, which talks to a USB gadget standing in
// for the HRI-200, and the HRI-200 when it is attached. Attached, everything
// passes through and the replies are cached; absent, the proxy answers WIRES-X
// itself. Either way it plays YSF from -allow'ed clients to WIRES-X as
// received audio, while attached only when the repeater is idle.
//
//	hri200-ysf -proxy -cache /var/lib/hri200-ysf/cache.json -allow 10.99.0.0/24
//
// With -metrics the proxy also serves how the repeater is used, timed from the
// HRI-200's states: overs received off the air (analog or digital) and
// transmitted for WIRES-X or for clients.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lweith/hri200-ysf/internal/bridge"
	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/proxy"
	"github.com/lweith/hri200-ysf/internal/reflector"
	"github.com/lweith/hri200-ysf/internal/serial"
	"github.com/lweith/hri200-ysf/internal/usage"
	"github.com/lweith/hri200-ysf/internal/usbmon"
)

type source interface {
	Next() (usbmon.Event, error)
	Close() error
}

var (
	device     = flag.String("usbmon", "auto", "usbmon device, or auto to find the HRI-200's bus")
	pcap       = flag.String("pcap", "", "replay this usbmon capture instead of sniffing live")
	listen     = flag.String("listen", ":42000", "UDP address to serve YSF clients on")
	gateway    = flag.String("gateway", "HRI200", "callsign in the packets' gateway field")
	id         = flag.Int("id", 99999, "reflector ID, in status replies")
	name       = flag.String("name", "HRI200", "reflector name, in status replies")
	desc       = flag.String("description", "HRI-200 sniff", "reflector description, in status replies")
	dump       = flag.String("dump", "", "also write every frame as hex to this file")
	fast       = flag.Bool("fast", false, "replay as fast as possible, keeping the captured timing (for -dump)")
	oversDir   = flag.String("overs", "", "keep every transmission's frames under this directory, a subdirectory per day")
	keep       = flag.Int("keep", 7, "with -overs: days of transmissions to keep")
	verbose    = flag.Bool("v", false, "log every HRI-200 message")
	cachePath  = flag.String("cache", "", "file to keep the HRI-200's replies in (learned in both modes)")
	proxyMode  = flag.Bool("proxy", false, "proxy between WIRES-X (-downstream) and the HRI-200, instead of sniffing")
	downstream = flag.String("downstream", "/dev/ttyGS0", "with -proxy: the tty WIRES-X talks to (the USB gadget)")
	upstream   = flag.String("upstream", "auto", "with -proxy: the HRI-200's tty, or auto to find it by USB ID")
	allow      = flag.String("allow", "", "with -proxy: comma-separated networks whose YSF audio is played to WIRES-X")
	transmit   = flag.Bool("transmit", false, "with -proxy: also transmit -allow'ed clients' overs on the HRI-200's radio, so the repeater hears them")
	metrics    = flag.String("metrics", "", "with -proxy: serve the repeater's usage as Prometheus metrics on this TCP address, at /metrics")
)

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	cache, err := proxy.LoadCache(*cachePath)
	if err != nil {
		log.Fatal(err)
	}
	if *cachePath != "" {
		log.Printf("cache %s: %d replies", *cachePath, cache.Len())
	}

	refl := &reflector.Reflector{Gateway: *gateway, ID: *id, Name: *name, Description: *desc, Log: log.Printf}
	if err := refl.Listen(*listen); err != nil {
		log.Fatal(err)
	}
	log.Printf("serving YSF on %s", refl.Addr())

	var dumpFile io.Writer
	if *dump != "" {
		f, err := os.Create(*dump)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		dumpFile = f
	}
	var overs *overLog
	if *oversDir != "" {
		overs = &overLog{dir: *oversDir, keep: *keep, log: log.Printf}
	}
	b := &bridge.Bridge{Log: log.Printf, Out: func(p bridge.Packet) {
		refl.Send(p)
		if dumpFile != nil {
			fmt.Fprintf(dumpFile, "%d %t %s\n", p.Seq, p.End, hex.EncodeToString(p.Frame))
		}
		if overs != nil {
			overs.packet(p, time.Now())
		}
	}}

	if *proxyMode {
		runProxy(refl, b, cache)
		return
	}
	go func() { log.Fatal(refl.Serve()) }()
	sniff(b, cache)
}

func runProxy(refl *reflector.Reflector, b *bridge.Bridge, cache *proxy.Cache) {
	for _, s := range strings.Split(*allow, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			log.Fatalf("-allow: %v", err)
		}
		refl.Allow = append(refl.Allow, p)
	}
	ysfIn := make(chan proxy.YSF, 64)
	refl.In = func(f reflector.Frame) {
		select {
		case ysfIn <- proxy.YSF{Source: f.Source, Frame: f.Frame, End: f.End}:
		default: // never block the reflector
		}
	}
	go func() { log.Fatal(refl.Serve()) }()

	find := func() (string, error) {
		if *upstream != "auto" {
			if _, err := os.Stat(*upstream); err != nil {
				return "", err
			}
			return *upstream, nil
		}
		return serial.FindTTY("26aa", "0002")
	}
	p := &proxy.Proxy{
		Downstream: *downstream,
		Upstream:   find,
		Open:       serial.Open,
		Cache:      cache,
		Bridge:     b,
		Log:        log.Printf,
		Verbose:    *verbose,
		Transmit:   *transmit,
	}
	if *metrics != "" {
		p.Usage = &usage.Tracker{}
		mux := http.NewServeMux()
		mux.Handle("/metrics", p.Usage)
		ln, err := net.Listen("tcp", *metrics)
		if err != nil {
			log.Fatalf("-metrics: %v", err)
		}
		log.Printf("serving metrics on http://%s/metrics", ln.Addr())
		go func() { log.Fatal(http.Serve(ln, mux)) }()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := p.Run(ctx, ysfIn); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func sniff(b *bridge.Bridge, cache *proxy.Cache) {
	var src source
	var err error
	live := *pcap == ""
	if live {
		path := *device
		if path == "auto" {
			bus, err := usbmon.FindBus("26aa", "0002")
			if err != nil {
				log.Fatal(err)
			}
			path = fmt.Sprintf("/dev/usbmon%d", bus)
		}
		src, err = usbmon.OpenLive(path)
		log.Printf("sniffing %s", path)
	} else {
		src, err = usbmon.OpenPcap(*pcap)
		log.Printf("replaying %s", *pcap)
	}
	if err != nil {
		log.Fatal(err)
	}
	defer src.Close()
	defer func() {
		if err := cache.Save(); err != nil {
			log.Printf("saving cache: %v", err)
		}
	}()

	events := make(chan usbmon.Event, 1024)
	go func() {
		defer close(events)
		var first, start time.Time
		for {
			e, err := src.Next()
			if errors.Is(err, io.EOF) {
				log.Printf("end of capture")
				return
			}
			if err != nil {
				log.Printf("reading: %v", err)
				return
			}
			if e.Payload() == nil {
				continue
			}
			if *fast {
				events <- e
				continue
			}
			if !live {
				// Replay at the captured pace, stamped with the current time.
				if first.IsZero() {
					first, start = e.Time, time.Now()
				}
				time.Sleep(time.Until(start.Add(e.Time.Sub(first))))
			}
			e.Time = time.Now()
			events <- e
		}
	}()

	var match proxy.Matcher
	splitters := map[[2]byte]*hri.Splitter{}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	lastSave := time.Now()
	for {
		select {
		case e, ok := <-events:
			if !ok {
				b.Tick(time.Now().Add(time.Hour))
				time.Sleep(200 * time.Millisecond) // let the last packets go out
				return
			}
			if *fast {
				b.Tick(e.Time)
			}
			key := [2]byte{e.Device, e.Endpoint}
			s := splitters[key]
			if s == nil {
				s = &hri.Splitter{}
				splitters[key] = s
			}
			dir := bridge.Network
			if e.Endpoint&0x80 != 0 {
				dir = bridge.Radio
			}
			for _, text := range s.Write(e.Payload()) {
				if *verbose {
					log.Printf("%s: %.60s", dir, text)
				}
				if dir == bridge.Network {
					match.Request(text, e.Time)
				} else if req, ok := match.Reply(text, e.Time); ok {
					cache.Learn(req, text)
				}
				if m, ok := hri.Parse(text); ok {
					b.Handle(dir, m, e.Time)
				}
			}
		case now := <-tick.C:
			if !*fast {
				b.Tick(now)
			}
			if now.Sub(lastSave) > 10*time.Second {
				if err := cache.Save(); err != nil {
					log.Printf("saving cache: %v", err)
				}
				lastSave = now
			}
		}
	}
}
