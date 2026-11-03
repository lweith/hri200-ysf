// hri200-ysf sniffs the serial traffic between WIRES-X and a Yaesu HRI-200
// with usbmon and serves it as a send-only YSF reflector. It never writes to
// the HRI-200.
//
//	hri200-ysf                          live, from the HRI-200's usbmon bus (root)
//	hri200-ysf -pcap capture.pcap       replay a tcpdump capture in real time
package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/lweith/hri200-ysf/internal/bridge"
	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/reflector"
	"github.com/lweith/hri200-ysf/internal/usbmon"
)

type source interface {
	Next() (usbmon.Event, error)
	Close() error
}

func main() {
	var (
		device  = flag.String("usbmon", "auto", "usbmon device, or auto to find the HRI-200's bus")
		pcap    = flag.String("pcap", "", "replay this usbmon capture instead of sniffing live")
		listen  = flag.String("listen", ":42000", "UDP address to serve YSF clients on")
		gateway = flag.String("gateway", "HRI200", "callsign in the packets' gateway field")
		id      = flag.Int("id", 99999, "reflector ID, in status replies")
		name    = flag.String("name", "HRI200", "reflector name, in status replies")
		desc    = flag.String("description", "HRI-200 sniff", "reflector description, in status replies")
		dump    = flag.String("dump", "", "also write every frame as hex to this file")
		fast    = flag.Bool("fast", false, "replay as fast as possible, keeping the captured timing (for -dump)")
		verbose = flag.Bool("v", false, "log every HRI-200 message")
	)
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)

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

	refl := &reflector.Reflector{Gateway: *gateway, ID: *id, Name: *name, Description: *desc, Log: log.Printf}
	if err := refl.Listen(*listen); err != nil {
		log.Fatal(err)
	}
	log.Printf("serving YSF on %s", refl.Addr())
	go func() { log.Fatal(refl.Serve()) }()

	var dumpFile io.Writer
	if *dump != "" {
		f, err := os.Create(*dump)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		dumpFile = f
	}

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

	b := &bridge.Bridge{Log: log.Printf, Out: func(p bridge.Packet) {
		refl.Send(p)
		if dumpFile != nil {
			fmt.Fprintf(dumpFile, "%d %t %s\n", p.Seq, p.End, hex.EncodeToString(p.Frame))
		}
	}}
	splitters := map[[2]byte]*hri.Splitter{}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
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
				m, ok := hri.Parse(text)
				if *verbose {
					log.Printf("%s: %.60s", dir, text)
				}
				if ok {
					b.Handle(dir, m, e.Time)
				}
			}
		case now := <-tick.C:
			if !*fast {
				b.Tick(now)
			}
		}
	}
}
