package wiresxnet

import (
	"context"
	"errors"
	"time"
)

// Node runs the local WIRES-X node over the network protocol, in place of the
// Windows WIRES-X.exe. It is a peer that registers with Yaesu's relay, keeps the
// session alive, pulls and caches the directory, and connects to and from
// rooms. (It is not a "client": it hosts a node and serves rooms.)
//
// This is the scaffold. The envelope codec (frame.go), the body/field grammar
// (message.go), the directory cache (directory.go) and the metrics exporter
// (metrics.go) are implemented and tested. The live transport below — relay
// discovery, login, keepalive, the directory-pull loop, room connect/disconnect,
// and the voice path — is stubbed pending the remaining captures (the 8320
// channel's role and whether/how voice RAW records ride the 8300 session).

// Config describes the node's identity on the network. These are the node's own
// registered values, read off its own traffic; the daemon only ever presents
// the node's own credentials.
type Config struct {
	NodeID    string // zero-padded node number, e.g. "0030401"
	Call      string // "W8HHF-RPT"
	ShortCall string // "W8HHF"
	Token     string // the persistent per-node token seen on login and room ops
	// RelayHost is the discovery host for GET /relay/get (pool and own-IP).
	RelayHost string
	// PollInterval is how often to send the keepalive/directory poll. Captures
	// show ~285 ms during directory sync and ~2 s for in-room presence.
	PollInterval time.Duration
}

// Node holds the running node's state.
type Node struct {
	cfg     Config
	dir     *Directory
	metrics *Metrics
}

// NewNode builds a node with a fresh directory cache and metrics registry.
func NewNode(cfg Config) *Node {
	m := NewMetrics()
	return &Node{cfg: cfg, dir: NewDirectory(m), metrics: m}
}

// Directory returns the node's directory cache (for serving a client or the
// HRI-200 side from memory).
func (n *Node) Directory() *Directory { return n.dir }

// Metrics returns the node's metrics registry (for the /metrics exporter).
func (n *Node) Metrics() *Metrics { return n.metrics }

// errNotImplemented marks the stubbed transport.
var errNotImplemented = errors.New("wiresxnet: live transport not yet implemented")

// Run connects and serves until ctx is cancelled. Stubbed: discovery, login,
// keepalive, directory-pull, and room control are pending the remaining
// captures.
func (n *Node) Run(ctx context.Context) error {
	// TODO:
	//   1. GET http://<RelayHost>:10080/relay/get -> pick a relay by load.
	//   2. GET http://<RelayHost>:10082/relay/get -> own public IP.
	//   3. Dial relay:8310, send &L..@e login with NodeID/Call/Token/own-IP,
	//      read the &R..@E active-node push and polls.
	//   4. Dial relay:8300, send &C open, read &R..&c ack.
	//   5. Poll @e on PollInterval; feed each &R..@E page to n.dir.IngestPage;
	//      count verbs and session state into n.metrics.
	//   6. Room connect/disconnect via &D CC/CU; presence via &D CP.
	//   7. Resolve YSF-vs-WIRES-X conflicts by preferencing YSF.
	_ = ctx
	return errNotImplemented
}
