package wiresxnet

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Relay-server discovery. Before a node connects, WIRES-X asks a relay host over
// plain HTTP which servers are up and how loaded they are, and what its own
// public IP is. Both are GET /relay/get, distinguished by port: the pool service
// (seen on :10080) returns a list, the address service (:10082) returns one IP.
//
// Pool reply (text/html), first line a timestamp, then one line per server:
//
//	20261114113959
//	153.246.44.26 / 2400:4050:... / 8310 / 20261114113859 / 277
//	<IPv4> / <IPv6> / <port> / <last-seen UTC> / <load>
//
// Address reply: a timestamp line and the caller's public IPv4.
//
// relayUA is the User-Agent WIRES-X sends; some relays may expect it. It carries
// no identifying information.
const relayUA = "WIRES-X"

// Relay is one advertised relay server endpoint.
type Relay struct {
	IPv4     string
	IPv6     string
	Port     int
	LastSeen string // UTC stamp as sent, YYYYMMDDHHMMSS
	Load     int    // lower is less loaded
}

// Addr returns the host:port to dial this relay.
func (r Relay) Addr() string { return fmt.Sprintf("%s:%d", r.IPv4, r.Port) }

// DiscoverRelays fetches the server pool from a relay host (e.g.
// "153.246.44.8:10080"). Lines that do not parse are skipped.
func DiscoverRelays(ctx context.Context, host string) ([]Relay, error) {
	body, err := relayGet(ctx, host)
	if err != nil {
		return nil, err
	}
	var out []Relay
	for _, line := range strings.Split(body, "\n") {
		f := strings.Split(line, "/")
		if len(f) != 5 {
			continue // timestamp line or blank
		}
		port, err := strconv.Atoi(strings.TrimSpace(f[2]))
		if err != nil {
			continue
		}
		load, _ := strconv.Atoi(strings.TrimSpace(f[4]))
		out = append(out, Relay{
			IPv4:     strings.TrimSpace(f[0]),
			IPv6:     strings.TrimSpace(f[1]),
			Port:     port,
			LastSeen: strings.TrimSpace(f[3]),
			Load:     load,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("wiresxnet: no relays in pool reply")
	}
	return out, nil
}

// OwnPublicIP fetches the caller's public IP from the address service (e.g.
// "153.246.44.8:10082"): the last non-empty, non-timestamp line.
func OwnPublicIP(ctx context.Context, host string) (string, error) {
	body, err := relayGet(ctx, host)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		// A dotted IPv4 has dots and no all-digit (timestamp) shape.
		if strings.Count(line, ".") == 3 {
			return line, nil
		}
	}
	return "", fmt.Errorf("wiresxnet: no IP in address reply")
}

// PickRelay returns the least-loaded relay offering port, and whether one was
// found. Ties break on the server address for determinism.
func PickRelay(relays []Relay, port int) (Relay, bool) {
	cand := make([]Relay, 0, len(relays))
	for _, r := range relays {
		if r.Port == port {
			cand = append(cand, r)
		}
	}
	if len(cand) == 0 {
		return Relay{}, false
	}
	sort.Slice(cand, func(i, j int) bool {
		if cand[i].Load != cand[j].Load {
			return cand[i].Load < cand[j].Load
		}
		return cand[i].IPv4 < cand[j].IPv4
	})
	return cand[0], true
}

// relayGet performs the GET /relay/get request against a host:port.
func relayGet(ctx context.Context, host string) (string, error) {
	url := "http://" + host + "/relay/get"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", relayUA)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("wiresxnet: relay %s: HTTP %d", host, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
