// Command wiresxnet-tap runs the passive WIRES-X observer over a reassembled
// TCP stream on stdin, prints what it learned, and optionally serves the query
// API so the snapshot can be explored.
//
// It is the passive posture made runnable: point it at one direction of a real
// node<->Yaesu stream and it fills the directory cache, talker and metrics state
// without emitting anything. To replay a capture, hand it the server->node bytes:
//
//	tshark -r cap.pcapng -q -z follow,tcp,raw,<stream> | \
//	  awk '/^\t/{gsub(/[^0-9a-f]/,"");print}' | xxd -r -p | wiresxnet-tap
//
// With -serve, after reading the stream it serves the JSON/SSE/metrics API:
//
//	... | wiresxnet-tap -serve :8080 &
//	curl -s localhost:8080/rooms/0004480
//	curl -s localhost:8080/talkers
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"

	"github.com/lweith/hri200-ysf/internal/wiresxnet"
)

func main() {
	serve := flag.String("serve", "", "after reading the stream, serve the query API on this address (e.g. :8080)")
	flag.Parse()

	m := wiresxnet.NewMetrics()
	obs := &wiresxnet.Observer{
		Dir:     wiresxnet.NewDirectory(m),
		Metrics: m,
		Broker:  wiresxnet.NewBroker(),
	}
	if err := obs.Observe(os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "observe:", err)
		os.Exit(1)
	}

	rooms := obs.Dir.Rooms()
	fmt.Printf("directory: %d entries (%d rooms, %d nodes)\n",
		obs.Dir.Len(), len(rooms), len(obs.Dir.Nodes()))

	sort.Slice(rooms, func(i, j int) bool { return rooms[i].Members > rooms[j].Members })
	fmt.Println("busiest rooms:")
	for i, r := range rooms {
		if i >= 10 || r.Members == 0 {
			break
		}
		fmt.Printf("  %-18s #%s  %d members  %s\n", r.Name, r.Number, r.Members, r.City)
	}

	if talkers := m.Talkers(); len(talkers) > 0 {
		fmt.Println("talkers:")
		for _, t := range talkers {
			fmt.Printf("  %-12s in %-18s  %d frames (~%ds)  %d overs\n",
				t.Call, t.Room, t.Frames, t.Frames/50, t.Overs)
		}
	}

	if *serve != "" {
		srv := &wiresxnet.Server{Dir: obs.Dir, Metrics: m, Broker: obs.Broker}
		fmt.Fprintf(os.Stderr, "serving query API on %s\n", *serve)
		if err := http.ListenAndServe(*serve, srv.Handler()); err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(1)
		}
	}
}
