// Command wiresxnet-proxy is the standalone WIRES-X splice: a byte-exact
// passthrough TCP proxy between a node and a Yaesu relay that fills the directory
// cache, talker stats and events from the live stream, and serves the query API.
//
// It is NOT wired inline. Nothing redirects the real WIRES-X.exe through it;
// you point a client at -listen yourself, or leave it running to prove the
// splice. Interposing it in the live path (DNAT/TPROXY on the host) and turning
// on frame rewriting is a separate, deliberate step.
//
//	# dial a specific relay
//	wiresxnet-proxy -listen :8300 -upstream 153.246.44.7:8300 -serve :8080
//
//	# or discover the least-loaded relay for a port
//	wiresxnet-proxy -listen :8300 -discover 153.246.44.8:10080 -port 8300 -serve :8080
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/lweith/hri200-ysf/internal/wiresxnet"
)

func main() {
	listen := flag.String("listen", ":8300", "address to accept the node's connection on")
	upstream := flag.String("upstream", "", "relay to dial, host:port (e.g. 153.246.44.7:8300)")
	discover := flag.String("discover", "", "relay pool host for discovery (e.g. 153.246.44.8:10080)")
	port := flag.Int("port", 8300, "relay port to pick when discovering")
	serve := flag.String("serve", "", "also serve the query API on this address (e.g. :8080)")
	flag.Parse()

	up := *upstream
	if up == "" {
		if *discover == "" {
			fmt.Fprintln(os.Stderr, "need -upstream or -discover")
			os.Exit(2)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		relays, err := wiresxnet.DiscoverRelays(ctx, *discover)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "discover:", err)
			os.Exit(1)
		}
		r, ok := wiresxnet.PickRelay(relays, *port)
		if !ok {
			fmt.Fprintf(os.Stderr, "no relay offering port %d\n", *port)
			os.Exit(1)
		}
		up = r.Addr()
		fmt.Fprintf(os.Stderr, "picked relay %s (load %d) from %d candidates\n", up, r.Load, len(relays))
	}

	m := wiresxnet.NewMetrics()
	p := &wiresxnet.Proxy{
		Upstream: up,
		Dir:      wiresxnet.NewDirectory(m),
		Metrics:  m,
		Broker:   wiresxnet.NewBroker(),
	}

	if *serve != "" {
		srv := &wiresxnet.Server{Dir: p.Dir, Metrics: m, Broker: p.Broker}
		go func() {
			fmt.Fprintf(os.Stderr, "query API on %s\n", *serve)
			if err := http.ListenAndServe(*serve, srv.Handler()); err != nil {
				fmt.Fprintln(os.Stderr, "serve:", err)
			}
		}()
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "splice: %s -> %s (passthrough, not inline)\n", *listen, up)
	if err := p.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}
