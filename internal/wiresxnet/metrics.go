package wiresxnet

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Metrics holds the counters and gauges the daemon exposes, and renders them in
// Prometheus text exposition format. It is a plain exporter endpoint: the
// daemon serves WriteProm at /metrics and Prometheus scrapes it. No client
// library is pulled in; the format is small and stable.
//
// Everything here is derived by parsing frames the node already handles, so the
// stats cost nothing extra on the wire. Per-room occupancy comes from the
// directory (free for every room on the network); per-talker stats, when added,
// come from the C4FM CSD in the voice records of rooms this node is joined to.
type Metrics struct {
	mu sync.Mutex

	dirSize     int
	sessionUp   bool
	roomMembers map[string]roomGauge // keyed by room number
	verbTotal   map[string]uint64    // frames by verb, e.g. "@E", "CC"
	connects    map[string]uint64    // CC by room number
	disconnects map[string]uint64    // CU by room number

	// Tier-3 talker stats, from the plaintext callsigns in the voice stream.
	talkerFrames map[rk]uint64 // voice frames by {room,call} ~= talk time
	talkerOvers  map[rk]uint64 // keyups by {room,call}
	conv         map[ek]uint64 // conversation edges by {room,from,to}
	curTalker    map[string]string
}

// rk is a {room, callsign} metric key; ek adds the previous talker for edges.
type rk struct{ room, call string }
type ek struct{ room, from, to string }

type roomGauge struct {
	name    string
	members int
}

// NewMetrics returns an empty registry.
func NewMetrics() *Metrics {
	return &Metrics{
		roomMembers:  make(map[string]roomGauge),
		verbTotal:    make(map[string]uint64),
		connects:     make(map[string]uint64),
		disconnects:  make(map[string]uint64),
		talkerFrames: make(map[rk]uint64),
		talkerOvers:  make(map[rk]uint64),
		conv:         make(map[ek]uint64),
		curTalker:    make(map[string]string),
	}
}

// CountTalkerFrame adds one voice frame to a talker (~20 ms of audio each).
func (m *Metrics) CountTalkerFrame(room, call string) {
	m.mu.Lock()
	m.talkerFrames[rk{room, call}]++
	m.mu.Unlock()
}

// CountOver records a keyup by a talker, and the conversation edge from whoever
// held the room last (from may be "").
func (m *Metrics) CountOver(room, from, to string) {
	m.mu.Lock()
	m.talkerOvers[rk{room, to}]++
	if from != "" && from != to {
		m.conv[ek{room, from, to}]++
	}
	m.curTalker[room] = to
	m.mu.Unlock()
}

// ClearTalker marks a room idle (no one keyed), e.g. on a silence timeout.
func (m *Metrics) ClearTalker(room string) {
	m.mu.Lock()
	delete(m.curTalker, room)
	m.mu.Unlock()
}

// TalkerStat is one talker's totals, for the query API.
type TalkerStat struct {
	Room    string `json:"room"`
	Call    string `json:"call"`
	Frames  uint64 `json:"frames"`            // ~20 ms each
	Overs   uint64 `json:"overs"`             // keyups
	Current bool   `json:"current,omitempty"` // keyed right now
}

// Talkers returns a snapshot of per-talker totals, including who is currently
// keyed in each room.
func (m *Metrics) Talkers() []TalkerStat {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := make(map[rk]*TalkerStat)
	get := func(k rk) *TalkerStat {
		s := seen[k]
		if s == nil {
			s = &TalkerStat{Room: k.room, Call: k.call}
			seen[k] = s
		}
		return s
	}
	for k, n := range m.talkerFrames {
		get(k).Frames = n
	}
	for k, n := range m.talkerOvers {
		get(k).Overs = n
	}
	for room, call := range m.curTalker {
		get(rk{room, call}).Current = true
	}
	out := make([]TalkerStat, 0, len(seen))
	for _, s := range seen {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Room != out[j].Room {
			return out[i].Room < out[j].Room
		}
		return out[i].Frames > out[j].Frames // busiest first within a room
	})
	return out
}

// SetDirectorySize records the cached entry count.
func (m *Metrics) SetDirectorySize(n int) {
	m.mu.Lock()
	m.dirSize = n
	m.mu.Unlock()
}

// SetSessionUp records whether the node's main session is connected.
func (m *Metrics) SetSessionUp(up bool) {
	m.mu.Lock()
	m.sessionUp = up
	m.mu.Unlock()
}

// SetRoomMembers records a room's current connected-node count.
func (m *Metrics) SetRoomMembers(number, name string, members int) {
	m.mu.Lock()
	m.roomMembers[number] = roomGauge{name: name, members: members}
	m.mu.Unlock()
}

// CountVerb increments the frame counter for a verb.
func (m *Metrics) CountVerb(verb string) {
	m.mu.Lock()
	m.verbTotal[verb]++
	m.mu.Unlock()
}

// CountConnect increments the connect counter for a room.
func (m *Metrics) CountConnect(room string) {
	m.mu.Lock()
	m.connects[room]++
	m.mu.Unlock()
}

// CountDisconnect increments the disconnect counter for a room.
func (m *Metrics) CountDisconnect(room string) {
	m.mu.Lock()
	m.disconnects[room]++
	m.mu.Unlock()
}

// WriteProm renders the metrics in Prometheus text exposition format.
func (m *Metrics) WriteProm(w io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bw := &errWriter{w: w}

	bw.line("# HELP wiresx_directory_entries Cached node and room directory entries.")
	bw.line("# TYPE wiresx_directory_entries gauge")
	bw.linef("wiresx_directory_entries %d", m.dirSize)

	bw.line("# HELP wiresx_session_up 1 when the node's main session is connected.")
	bw.line("# TYPE wiresx_session_up gauge")
	bw.linef("wiresx_session_up %d", b2i(m.sessionUp))

	bw.line("# HELP wiresx_room_members Connected nodes per room, from the directory.")
	bw.line("# TYPE wiresx_room_members gauge")
	for _, num := range sortedKeys(m.roomMembers) {
		g := m.roomMembers[num]
		bw.linef("wiresx_room_members{room=%q,name=%q} %d", num, label(g.name), g.members)
	}

	bw.line("# HELP wiresx_frames_total Frames handled, by verb.")
	bw.line("# TYPE wiresx_frames_total counter")
	for _, v := range sortedCount(m.verbTotal) {
		bw.linef("wiresx_frames_total{verb=%q} %d", label(v), m.verbTotal[v])
	}

	bw.line("# HELP wiresx_room_connects_total Room connects, by room.")
	bw.line("# TYPE wiresx_room_connects_total counter")
	for _, r := range sortedCount(m.connects) {
		bw.linef("wiresx_room_connects_total{room=%q} %d", r, m.connects[r])
	}

	bw.line("# HELP wiresx_room_disconnects_total Room disconnects, by room.")
	bw.line("# TYPE wiresx_room_disconnects_total counter")
	for _, r := range sortedCount(m.disconnects) {
		bw.linef("wiresx_room_disconnects_total{room=%q} %d", r, m.disconnects[r])
	}

	bw.line("# HELP wiresx_talker_frames_total Voice frames per talker (~20 ms each).")
	bw.line("# TYPE wiresx_talker_frames_total counter")
	for _, k := range sortedRK(m.talkerFrames) {
		bw.linef("wiresx_talker_frames_total{room=%q,callsign=%q} %d", k.room, label(k.call), m.talkerFrames[k])
	}

	bw.line("# HELP wiresx_talker_overs_total Keyups per talker.")
	bw.line("# TYPE wiresx_talker_overs_total counter")
	for _, k := range sortedRK(m.talkerOvers) {
		bw.linef("wiresx_talker_overs_total{room=%q,callsign=%q} %d", k.room, label(k.call), m.talkerOvers[k])
	}

	bw.line("# HELP wiresx_conversation_total Keyups that followed another talker (who answers whom).")
	bw.line("# TYPE wiresx_conversation_total counter")
	for _, k := range sortedEK(m.conv) {
		bw.linef("wiresx_conversation_total{room=%q,from=%q,to=%q} %d", k.room, label(k.from), label(k.to), m.conv[k])
	}

	bw.line("# HELP wiresx_current_talker 1 for the talker currently keyed in a room.")
	bw.line("# TYPE wiresx_current_talker gauge")
	for _, room := range sortedStrVal(m.curTalker) {
		bw.linef("wiresx_current_talker{room=%q,callsign=%q} 1", room, label(m.curTalker[room]))
	}
	return bw.err
}

func sortedRK(m map[rk]uint64) []rk {
	out := make([]rk, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].room != out[j].room {
			return out[i].room < out[j].room
		}
		return out[i].call < out[j].call
	})
	return out
}

func sortedEK(m map[ek]uint64) []ek {
	out := make([]ek, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].room != out[j].room {
			return out[i].room < out[j].room
		}
		if out[i].from != out[j].from {
			return out[i].from < out[j].from
		}
		return out[i].to < out[j].to
	})
	return out
}

func sortedStrVal(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// errWriter writes lines until the first error, which it keeps.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) line(s string) {
	if e.err != nil {
		return
	}
	_, e.err = io.WriteString(e.w, s+"\n")
}

func (e *errWriter) linef(format string, a ...any) {
	e.line(fmt.Sprintf(format, a...))
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// label escapes a label value for the exposition format (backslash, quote,
// newline), per the Prometheus text spec.
func label(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func sortedKeys(m map[string]roomGauge) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCount(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
