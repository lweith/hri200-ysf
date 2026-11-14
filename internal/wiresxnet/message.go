package wiresxnet

import (
	"encoding/hex"
	"errors"
	"fmt"
)

// A body is a type letter, a 4-hex inner length, and a payload of that length:
//
//	& <type>  <innerlen 4 hex>  <payload>
//
// The payload begins with a verb and is followed by fields. The verb is
// type-specific: @e/@E for polls and directory pages, &c for a wrapped connect
// ack, a 2-hex session byte for &D room control, and bare fields for &C.
//
// Message types (the type letter after '&'):
const (
	TypeNodePoll  = 'L' // node -> server: keepalive and directory-pull poll (@e)
	TypeServerRep = 'R' // server -> node: reply, directory page (@E), wrapped ack (&c)
	TypeNodeData  = 'D' // node -> room host: connect/announce/presence/disconnect
	TypeConnect   = 'C' // node -> server: open the main session on 8300
)

// Known verbs (the two payload bytes after the inner length). &D record verbs
// come after the session byte and routing header; see parseDataRecord.
const (
	VerbPoll       = "@e" // node poll / directory-pull request (carries a seq)
	VerbPage       = "@E" // server directory page (carries the matching seq)
	VerbVoice      = "RR" // server room stream: voice frames and talker/room records
	VerbAck        = "&c" // server connect acknowledgement, wrapped in an &R
	DataConnect    = "CC" // &D: connect to a room (full room metadata)
	DataChat       = "CR" // &D: announce text into a room
	DataPresence   = "CP" // &D: connected-presence poll, ~every 2 s while joined
	DataDisconnect = "CU" // &D: disconnect / unlink (sent to room server and home)
)

// ErrBadBody reports a malformed body.
var ErrBadBody = errors.New("wiresxnet: bad body")

// Message is a parsed body: its type, the inner payload, and (when the type
// carries one up front) the verb. Fields are left in Payload for the caller to
// walk with Fields; the per-verb record layouts live with their consumers
// (directory.go for @E).
type Message struct {
	Type    byte   // one of the Type* constants
	Verb    string // "@e", "@E", "&c", ""; "" when the type has no leading verb
	Payload []byte // everything after the inner length (verb included)
}

// ParseBody splits an envelope body into a Message. It does not walk fields.
func ParseBody(body []byte) (Message, error) {
	if len(body) < 6 || body[0] != '&' {
		return Message{}, fmt.Errorf("%w: short or no '&': %q", ErrBadBody, body)
	}
	typ := body[1]
	inner, err := hexU16(body[2:6])
	if err != nil {
		return Message{}, fmt.Errorf("%w: inner length: %v", ErrBadBody, err)
	}
	if 6+inner > len(body) {
		return Message{}, fmt.Errorf("%w: inner length %d exceeds body %d", ErrBadBody, inner, len(body))
	}
	payload := body[6 : 6+inner]
	m := Message{Type: typ, Payload: payload}
	// L/R carry a two-byte verb up front (@e, @E, &c). D and C do not: D opens
	// with a session byte and routing header, C with fields.
	if (typ == TypeNodePoll || typ == TypeServerRep) && len(payload) >= 2 {
		m.Verb = string(payload[:2])
	}
	return m, nil
}

// dataVerbs are the &D record codes, searched for in a room-control payload.
var dataVerbs = []string{DataConnect, DataDisconnect, DataChat, DataPresence}

// parseDataVerb finds the record verb in an &D payload and a room label for it.
//
// An &D payload opens with a session byte, a space-padded destination IP, an
// "RR000000" marker, the node/room ids and the per-node token, and only then the
// two-character record code (CC/CU/CR/CP). That header is not a clean TLV run
// and its field widths vary with IP length, so rather than parse it positionally
// this scans for the first known record code. The room label is the first
// dotted-quad in the payload (the room's server IP); mapping that to the room
// number via the directory or the CC record's fields is left to the caller.
//
// Returns ("&D", "") when no known record code is present.
func parseDataVerb(payload []byte) (verb, room string) {
	verb = "&D"
	best := len(payload)
	for _, v := range dataVerbs {
		if i := indexAt(payload, v); i >= 0 && i < best {
			best, verb = i, v
		}
	}
	return verb, firstIPv4(string(payload))
}

// indexAt returns the index of sub in b, or -1.
func indexAt(b []byte, sub string) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == sub {
			return i
		}
	}
	return -1
}

// firstIPv4 returns the first dotted-decimal run in s, or "".
func firstIPv4(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			continue
		}
		j, dots := i, 0
		for j < len(s) && (s[j] == '.' || (s[j] >= '0' && s[j] <= '9')) {
			if s[j] == '.' {
				dots++
			}
			j++
		}
		if dots == 3 {
			return s[i:j]
		}
		i = j
	}
	return ""
}

// Field is one TLV: a one-byte tag, and its value. Tags and lengths are two hex
// digits each on the wire; the value is len bytes of (usually) ASCII.
type Field struct {
	Tag   byte
	Value []byte
}

// Known field tags, for logging and for the directory parser.
const (
	TagIP      = 0x02 // server/room IP, as text (also seen as 0x0D in some records)
	TagName    = 0x03 // long name / callsign
	TagShort   = 0x04 // short callsign or room name
	TagCity    = 0x06 // "City,State"
	TagCountry = 0x07 // country
	TagDesc    = 0x08 // free-text description
	TagNodeID  = 0x0A // node/room number, zero-padded
	TagAltIP   = 0x0D // IP, as text
	TagMembers = 0x2C // connected-node count for a room (4 hex)
	TagSepFE   = 0xFE // record separator in directory pages (value 00)
)

// Fields walks a TLV stream and calls fn for each field. It stops and returns
// an error at the first malformed TLV. Values alias into b; copy to retain.
func Fields(b []byte, fn func(Field) bool) error {
	for len(b) > 0 {
		if len(b) < 4 {
			return fmt.Errorf("%w: trailing %d bytes, need tag+len", ErrBadBody, len(b))
		}
		var tl [2]byte
		if _, err := hex.Decode(tl[:], b[:4]); err != nil {
			return fmt.Errorf("%w: tag/len not hex: %q", ErrBadBody, b[:4])
		}
		tag, n := tl[0], int(tl[1])
		if 4+n > len(b) {
			return fmt.Errorf("%w: field len %d past end (%d left)", ErrBadBody, n, len(b)-4)
		}
		if !fn(Field{Tag: tag, Value: b[4 : 4+n]}) {
			return nil
		}
		b = b[4+n:]
	}
	return nil
}
