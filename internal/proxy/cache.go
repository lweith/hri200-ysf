// Package proxy sits between WIRES-X and an HRI-200. With the HRI-200
// attached it passes everything through and caches the HRI-200's replies.
// Without it, it answers WIRES-X from that cache and a small state machine, so
// the HRI-200 can be swapped in and out while WIRES-X keeps running. In both
// cases WIRES-X's transmissions become YSF; without the HRI-200, YSF from an
// allowed client is played to WIRES-X as if the radio had received it.
package proxy

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
)

// Cache maps each request WIRES-X has sent to the HRI-200's last reply to it.
type Cache struct {
	path string

	mu      sync.Mutex
	Replies map[string]string `json:"replies"`
	Updated time.Time         `json:"updated"`
	// Mode is the radio's WIRES Operation as it last echoed it in a D1M
	// ("0" Analog, "1" Digital), so that a radio left in Analog by a proxy
	// that stopped or crashed mid-session is switched back on the next start.
	Mode string `json:"mode,omitempty"`
	// SQLT is the repeater's hang time (ms, 4 digits) as it last echoed it.
	SQLT string `json:"sqlt,omitempty"`
	// HangRestore is the hang time to put back with the switch to Digital,
	// "" if none is owed: -hang-zero takes it to 0 while in Analog for packet.
	HangRestore string `json:"hang_restore,omitempty"`
	// Settings is the radio's last settings echo (a D1M), what a write the
	// cache has no echo for is echoed over (identity.go).
	Settings string `json:"settings,omitempty"`
	dirty    bool
}

// LastSettings is the radio's last settings echo, "" if none is known. A
// cache from before Settings was kept has its echo of the mode write.
func (c *Cache) LastSettings() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Settings != "" {
		return c.Settings
	}
	mode := byte(digitalMode)
	if c.Mode != "" {
		mode = c.Mode[0]
	}
	return c.Replies[SetMode(mode)]
}

// NoteMode records the WIRES Operation and the hang time from a D1M the
// radio sent.
func (c *Cache) NoteMode(text string) {
	m, ok := hri.Parse(text)
	if !ok || m.Cmd != 'M' || len(m.Payload) <= 67 {
		return
	}
	mode := m.Payload[67:68]
	if mode != "0" && mode != "1" {
		return
	}
	sqlt := ""
	if len(m.Payload) >= sqltAt+4 && isDigits(m.Payload[sqltAt:sqltAt+4]) {
		sqlt = m.Payload[sqltAt : sqltAt+4]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Settings != text && !strings.Contains(m.Payload, "FFFF") {
		c.Settings = text
		c.Updated = time.Now().UTC()
		c.dirty = true
	}
	if c.Mode != mode || sqlt != "" && c.SQLT != sqlt {
		c.Mode = mode
		if sqlt != "" {
			c.SQLT = sqlt
		}
		c.Updated = time.Now().UTC()
		c.dirty = true
	}
}

func isDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// LastSQLT is the hang time the radio last echoed, "" if never seen.
func (c *Cache) LastSQLT() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.SQLT
}

// HangOwed is the hang time owed back to the radio, "" if none.
func (c *Cache) HangOwed() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.HangRestore
}

// OweHang records the hang time to put back ("" when it is back).
func (c *Cache) OweHang(sqlt string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.HangRestore != sqlt {
		c.HangRestore = sqlt
		c.Updated = time.Now().UTC()
		c.dirty = true
	}
}

// LastMode is the WIRES Operation the radio last echoed, "" if never seen.
func (c *Cache) LastMode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Mode
}

// LoadCache reads the cache file; a missing file is an empty cache.
func LoadCache(path string) (*Cache, error) {
	c := &Cache{path: path, Replies: map[string]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if c.Replies == nil {
		c.Replies = map[string]string{}
	}
	return c, nil
}

// Learn records a reply. Poll and status replies are only kept while idle, so
// that what gets replayed is the idle answer.
func (c *Cache) Learn(request, reply string) {
	if strings.HasPrefix(reply, "B") && !strings.HasPrefix(reply, "B0") {
		return
	}
	if m, ok := hri.Parse(reply); ok && m.Cmd == 'C' {
		if s, ok := hri.ParseState(m); !ok || !s.Idle() {
			return
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Replies[request] != reply {
		c.Replies[request] = reply
		c.Updated = time.Now().UTC()
		c.dirty = true
	}
}

// Reply returns the cached reply to a request.
func (c *Cache) Reply(request string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.Replies[request]
	return r, ok
}

// Len is the number of cached replies.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.Replies)
}

// Save writes the cache if it changed, atomically.
func (c *Cache) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty || c.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(c, "", "\t")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".cache-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), c.path); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

// expectsReply reports whether the HRI-200 answers a request with a reply
// worth caching. Voice (D1E) and headers and pages (D1F) are only acked, and
// D1G00010 is WIRES-X's own ack of the radio's D1G.
func expectsReply(request string) bool {
	switch hri.Key(request) {
	case "D1E", "D1F", "":
		return false
	}
	return request != hri.Ack('G')
}

// Matcher pairs requests from WIRES-X with the HRI-200's replies, in order.
type Matcher struct {
	pending []pending
}

type pending struct {
	request string
	at      time.Time
}

// maxWait drops a request the HRI-200 never answered.
const maxWait = 2 * time.Second

// Request notes a request from WIRES-X.
func (m *Matcher) Request(request string, now time.Time) {
	if !expectsReply(request) {
		return
	}
	m.expire(now)
	m.pending = append(m.pending, pending{request, now})
}

// Reply returns the request a message from the HRI-200 answers, if any.
// Unsolicited messages (status, receive traffic) answer nothing.
func (m *Matcher) Reply(reply string, now time.Time) (string, bool) {
	m.expire(now)
	k := hri.Key(reply)
	for i, p := range m.pending {
		if hri.ReplyKey(p.request) == k {
			m.pending = append(m.pending[:i], m.pending[i+1:]...)
			return p.request, true
		}
	}
	return "", false
}

func (m *Matcher) expire(now time.Time) {
	i := 0
	for i < len(m.pending) && now.Sub(m.pending[i].at) > maxWait {
		i++
	}
	m.pending = m.pending[i:]
}
