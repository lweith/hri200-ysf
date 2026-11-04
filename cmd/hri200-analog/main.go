// hri200-analog records the analog FM transmissions a Yaesu HRI-200's radio
// receives, by reading its USB traffic with usbmon: the receive audio its
// audio device (26aa:0003) streams, and the receive states its serial device
// (26aa:0002) reports. Each transmission becomes an 8 kHz WAV file,
// -dir/YYYY-MM-DD/HHMMSS.wav (UTC). Digital transmissions are not recorded.
//
//	hri200-analog -dir /var/lib/hri200-analog       live (root)
//	hri200-analog -pcap capture.pcap -dir /tmp/out  from a tcpdump capture
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lweith/hri200-ysf/internal/analog"
	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/usbmon"
)

var (
	dir     = flag.String("dir", "/var/lib/hri200-analog", "directory to keep recordings in, a subdirectory per day")
	keep    = flag.Int("keep", 7, "days of recordings to keep")
	pcap    = flag.String("pcap", "", "read this usbmon capture instead of the live bus")
	verbose = flag.Bool("v", false, "log every radio state")
)

// audioEndpoint is the audio device's isochronous IN endpoint: receive audio,
// 48 kHz 16-bit mono.
const audioEndpoint = 0x81

type source interface {
	Next() (usbmon.Event, error)
	Close() error
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	flag.Parse()

	var src source
	var audioDev, serialDev int // 0: any device (a capture of one bus)
	if *pcap != "" {
		p, err := usbmon.OpenPcap(*pcap)
		if err != nil {
			log.Fatal(err)
		}
		src = p
	} else {
		audio, serial, err := findHRI()
		if err != nil {
			log.Fatal(err)
		}
		path := fmt.Sprintf("/dev/usbmon%d", audio.Bus)
		l, err := usbmon.OpenLive(path)
		if err != nil {
			log.Fatal(err)
		}
		src, audioDev, serialDev = l, audio.Number, serial.Number
		log.Printf("recording from %s: audio device %d, serial device %d", path, audioDev, serialDev)
		go watchDropped(l)
	}

	r := &analog.Recorder{Dir: *dir, Keep: *keep, Log: log.Printf}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		src.Close() // ends the loop below
	}()

	var split hri.Splitter
	var samples []int16
	for {
		e, err := src.Next()
		if err != nil {
			log.Printf("stopped: %v", err)
			break
		}
		if audioDev != 0 && int(e.Device) != audioDev && int(e.Device) != serialDev {
			continue
		}
		if e.Endpoint == audioEndpoint && (audioDev == 0 || int(e.Device) == audioDev) {
			if b := e.IsoData(); len(b) > 0 {
				samples = samples[:0]
				for i := 0; i+1 < len(b); i += 2 {
					samples = append(samples, int16(binary.LittleEndian.Uint16(b[i:])))
				}
				r.Audio(samples)
			}
			continue
		}
		if e.Endpoint&0x80 == 0 || (serialDev != 0 && int(e.Device) != serialDev) {
			continue
		}
		for _, text := range split.Write(e.Payload()) {
			m, ok := hri.Parse(text)
			if !ok {
				continue
			}
			if st, ok := hri.ParseState(m); ok {
				if *verbose {
					log.Printf("state %s", st)
				}
				r.State(st, e.Time)
			}
		}
	}
	r.Close()
}

// findHRI finds the HRI-200's audio device and the serial device on the same
// bus. (A USB gadget standing in for the serial device has the same ID, on
// another bus.)
func findHRI() (audio, serial usbmon.Device, err error) {
	audios := usbmon.FindDevices("26aa", "0003")
	if len(audios) == 0 {
		return audio, serial, fmt.Errorf("no HRI-200 audio device (26aa:0003)")
	}
	audio = audios[0]
	for _, s := range usbmon.FindDevices("26aa", "0002") {
		if s.Bus == audio.Bus {
			return audio, s, nil
		}
	}
	return audio, serial, fmt.Errorf("no HRI-200 serial device (26aa:0002) on bus %d", audio.Bus)
}

// watchDropped logs when the kernel drops events because the ring was full.
func watchDropped(l *usbmon.Live) {
	var last uint32
	for range time.Tick(time.Minute) {
		n, err := l.Dropped()
		if err != nil {
			return
		}
		if n != last {
			log.Printf("usbmon dropped %d events", n-last)
			last = n
		}
	}
}
