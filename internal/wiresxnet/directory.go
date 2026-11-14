package wiresxnet

import (
	"bytes"
	"sort"
	"strings"
	"sync"
	"time"
)

// recordSep is the record separator as it appears in a page's ASCII-hex TLV
// stream: tag 0xFE, length 0x02. Records begin at the first one; the bytes
// before it are the page's verb and sequence header, which this cache skips.
var recordSep = []byte("FE02")

// The directory is the global list of nodes and rooms. The node pulls it at
// connect by polling (@e) and the server answers one ~1 KB page per poll (@E),
// for hundreds of pages; in a cold-connect capture this dominates the session.
// It changes slowly, so the daemon caches it: a client (or the HRI-200 side)
// gets it from memory at once instead of waiting out the page-by-page dribble,
// and the cache is refreshed on the daemon's own cadence.
//
// A directory page is a TLV stream of records separated by the 0xFE tag. Each
// record carries a node/room number (0xA), a name (0x4), optionally a
// city/description, an IP, and for rooms a connected-node count (0x2C). The
// record is keyed by its number.

// Entry is one node or room as the directory lists it. Fields not present in a
// record are left zero.
type Entry struct {
	Number  string `json:"number"`            // node/room number, zero-padded (tag 0x0A)
	Name    string `json:"name"`              // tag 0x04 (short call / room name)
	Call    string `json:"call,omitempty"`    // tag 0x03 (long call), when present
	City    string `json:"city,omitempty"`    // tag 0x06
	Country string `json:"country,omitempty"` // tag 0x07
	Desc    string `json:"desc,omitempty"`    // tag 0x08
	IP      string `json:"ip,omitempty"`      // tag 0x02 or 0x0D
	Members int    `json:"members"`           // tag 0x2C, rooms only; connected-node count
	IsRoom  bool   `json:"is_room"`           // a Members/0x2C field was present
}

// rawRec accumulates one record's fields during a page walk, keeping text as
// raw bytes until the record ends so decodeText can use the country as a charset
// hint. finish decodes it into an Entry.
type rawRec struct {
	number       string
	call, name   []byte
	city, desc   []byte
	country, ip  string
	members      int
	isRoom, have bool
}

func (r rawRec) finish() Entry {
	return Entry{
		Number:  r.number,
		Name:    decodeText(r.name, r.country),
		Call:    decodeText(r.call, r.country),
		City:    decodeText(r.city, r.country),
		Country: r.country,
		Desc:    decodeText(r.desc, r.country),
		IP:      r.ip,
		Members: r.members,
		IsRoom:  r.isRoom,
	}
}

// Directory is a concurrency-safe cache of the global node/room list, built by
// ingesting @E pages and queryable as a whole.
type Directory struct {
	mu      sync.RWMutex
	entries map[string]Entry
	updated time.Time
	metrics *Metrics // optional; occupancy gauges are set on ingest
}

// NewDirectory returns an empty cache. metrics may be nil.
func NewDirectory(metrics *Metrics) *Directory {
	return &Directory{entries: make(map[string]Entry), metrics: metrics}
}

// IngestPage parses one @E page's payload and merges its records. It accepts
// the Message payload (verb included) or the fields after it; it walks TLVs and
// groups them into records at each 0xFE separator. Records without a number are
// dropped (page headers, trailers). It returns the number of records merged.
func (d *Directory) IngestPage(payload []byte) (int, error) {
	// Skip the page's verb+sequence header: records start at the first
	// separator. A payload already trimmed to records (as a caller may pass)
	// has no header and is parsed whole.
	if i := bytes.Index(payload, recordSep); i > 0 {
		payload = payload[i:]
	}
	var recs []Entry
	// Text fields are held raw until the record ends, because the country tag
	// (the charset hint) may arrive after the name and city; decodeText runs at
	// flush when the whole record, country included, is known.
	var cur rawRec
	flush := func() {
		if cur.have && cur.number != "" {
			recs = append(recs, cur.finish())
		}
		cur = rawRec{}
	}
	err := Fields(payload, func(f Field) bool {
		switch f.Tag {
		case TagSepFE:
			flush()
		case TagNodeID:
			// A number begins (or continues) a record.
			if cur.number != "" {
				flush()
			}
			cur.number = string(f.Value)
			cur.have = true
		case TagName:
			cur.call = append([]byte(nil), f.Value...)
			cur.have = true
		case TagShort:
			cur.name = append([]byte(nil), f.Value...)
			cur.have = true
		case TagCity:
			cur.city = append([]byte(nil), f.Value...)
		case TagCountry:
			cur.country = string(f.Value)
		case TagDesc:
			cur.desc = append([]byte(nil), f.Value...)
		case TagIP, TagAltIP:
			if cur.ip == "" {
				cur.ip = string(f.Value)
			}
		case TagMembers:
			cur.members = hexInt(f.Value)
			cur.isRoom = true
		}
		return true
	})
	flush()
	if err != nil {
		// Return what parsed cleanly before the malformed TLV, with the error.
		d.merge(recs)
		return len(recs), err
	}
	d.merge(recs)
	return len(recs), nil
}

// merge folds records into the cache, updates the timestamp, and refreshes the
// per-room occupancy gauges.
func (d *Directory) merge(recs []Entry) {
	if len(recs) == 0 {
		return
	}
	d.mu.Lock()
	for _, e := range recs {
		d.entries[e.Number] = e
	}
	d.updated = time.Now()
	d.mu.Unlock()
	if d.metrics != nil {
		for _, e := range recs {
			if e.IsRoom {
				d.metrics.SetRoomMembers(e.Number, e.Name, e.Members)
			}
		}
		d.metrics.SetDirectorySize(d.Len())
	}
}

// Lookup returns the entry for a number and whether it was found.
func (d *Directory) Lookup(number string) (Entry, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	e, ok := d.entries[number]
	return e, ok
}

// Rooms returns the room entries, sorted by number.
func (d *Directory) Rooms() []Entry {
	return d.filter(func(e Entry) bool { return e.IsRoom })
}

// Nodes returns the non-room entries, sorted by number.
func (d *Directory) Nodes() []Entry {
	return d.filter(func(e Entry) bool { return !e.IsRoom })
}

func (d *Directory) filter(keep func(Entry) bool) []Entry {
	d.mu.RLock()
	out := make([]Entry, 0, len(d.entries))
	for _, e := range d.entries {
		if keep(e) {
			out = append(out, e)
		}
	}
	d.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// Search returns entries whose number, name, call, city or country contains q
// (case-insensitive), sorted by number, capped at limit (<=0 means no cap).
func (d *Directory) Search(q string, limit int) []Entry {
	q = strings.ToLower(strings.TrimSpace(q))
	out := d.filter(func(e Entry) bool {
		if q == "" {
			return true
		}
		for _, f := range []string{e.Number, e.Name, e.Call, e.City, e.Country} {
			if strings.Contains(strings.ToLower(f), q) {
				return true
			}
		}
		return false
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Len is the number of cached entries.
func (d *Directory) Len() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.entries)
}

// Updated is when the cache last changed.
func (d *Directory) Updated() time.Time {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.updated
}

// hexInt parses a hex-text value (e.g. a 0x2C member count) to an int, treating
// a non-hex value as zero. Directory counts are small and always hex on the wire.
func hexInt(b []byte) int {
	n := 0
	for _, c := range b {
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		default:
			return 0
		}
		n = n*16 + d
	}
	return n
}
