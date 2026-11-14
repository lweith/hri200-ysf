package wiresxnet

import (
	"bytes"
	"regexp"
)

// Voice and talker tracking from the room stream.
//
// While the node is in a room, the server sends voice on the same 8300 channel
// as &R records whose payload begins "RR" (see ParseBody: a TypeServerRep whose
// Verb is "RR"). After a fixed header (the "RR000000", the room and node ids,
// and the per-node token) comes a one-byte record subtype:
//
//	0x10  talker/route record. A state marker 'v'/'u'/'t' precedes a separator:
//	      "v#<node> $<op> ~<relay>..." is a keyup, "u#..." an ongoing update, and
//	      "t~..." (separator '~', no node) a keydown. The '#' field is the
//	      relaying node/repeater; the '$' field is the actual operator behind it
//	      (e.g. "v#US-KCWIDE $KD3EAC" is the KC-WIDE link carrying operator
//	      KD3EAC from a bridged network). Confirmed from captures/2026-11-13.
//	0x00  a voice frame (AMBE audio), attributed to the current talker.
//	'Q'   (0x51) room id,  'M' (0x4D) member roster,  'C' (0x43) presence.
//
// Talker identity is therefore plaintext: no C4FM/FEC decode is needed to know
// who is talking, only to decode the audio itself. VoiceState turns that stream
// into the talk-time, keyup, conversation and current-talker metrics, crediting
// the operator ($) rather than the relaying node (#).

// token marks the end of the RR header; the record subtype is the next byte.
var tokenMark = []byte("04B42A")

// nodeRE pulls the relaying node from an active record: a 'v'/'u' marker, '#',
// then the node/repeater call. A 't' marker or a '~' separator means no active
// talker (a keydown), so it does not match and the room goes idle.
var nodeRE = regexp.MustCompile(`[vu]#([A-Z0-9][A-Z0-9/-]{1,13})`)

// srcRE pulls the operator behind the node: the "$<call>" field, present only on
// an active talker record (the relay chain uses '~', never '$').
var srcRE = regexp.MustCompile(`\$([A-Z0-9][A-Z0-9/-]{1,13})`)

// roomIDRE pulls a room name from a 'Q' (QB) record: "< <NAME> #<num> ...".
var roomIDRE = regexp.MustCompile(`< ([A-Za-z0-9][A-Za-z0-9 _/.-]{1,30}?) #`)

// VoiceState tracks the current room and talker to drive the talker metrics. It
// is updated from one direction's RR records and is not safe for concurrent use;
// the Metrics it writes to are.
type VoiceState struct {
	m      *Metrics
	broker *Broker // optional; receives talker/idle events
	room   string  // current room label (name from QB, else set by the caller)
	talker string  // currently keyed talker, "" when idle (for the gauge)
	last   string  // last talker seen, kept across idle (for conversation edges)
}

// NewVoiceState returns a tracker writing to m. room may be "" if unknown until
// a room-id record arrives.
func NewVoiceState(m *Metrics, room string) *VoiceState {
	return &VoiceState{m: m, room: room}
}

// Room returns the current room label.
func (v *VoiceState) Room() string { return v.room }

// HandleRR routes one RR record payload (everything after the &R inner length,
// i.e. starting "RR..."). Unknown or malformed records are ignored.
func (v *VoiceState) HandleRR(payload []byte) {
	i := bytes.Index(payload, tokenMark)
	if i < 0 || i+len(tokenMark) >= len(payload) {
		return
	}
	sub := payload[i+len(tokenMark)]
	body := payload[i+len(tokenMark)+1:]
	switch sub {
	case 0x10:
		if call, ok := talker(body); ok {
			v.setTalker(call)
		} else {
			v.Idle() // keydown (t~) or route-only record: no one keyed
		}
	case 0x00:
		if v.talker != "" && v.m != nil {
			v.m.CountTalkerFrame(v.room, v.talker)
		}
	case 'Q':
		if mm := roomIDRE.FindSubmatch(body); mm != nil {
			v.room = string(mm[1])
		}
	}
}

// talker returns the operator keyed in a 0x10 record and whether anyone is. An
// active record names a node after "[vu]#"; the operator is the "$" field when
// present (the human behind the relaying node), else the node itself. A keydown
// ("t~") or route-only record has no node and returns ok=false.
func talker(body []byte) (call string, ok bool) {
	nm := nodeRE.FindSubmatch(body)
	if nm == nil {
		return "", false
	}
	if sm := srcRE.FindSubmatch(body); sm != nil {
		return string(sm[1]), true // operator behind the node
	}
	return string(nm[1]), true // no operator field; credit the node
}

// setTalker handles a keyup: a new talker is an over, with a conversation edge
// from the previous one. The same talker re-asserting mid-over is not a new over.
func (v *VoiceState) setTalker(call string) {
	if call == v.talker {
		return
	}
	// The conversation edge is from the last talker (kept across the idle gap
	// between overs), not from v.talker (cleared on keydown).
	if v.m != nil {
		v.m.CountOver(v.room, v.last, call)
	}
	v.broker.Publish(Event{Type: EventTalker, Room: v.room, Call: call, From: v.last})
	v.talker = call
	v.last = call
}

// Idle marks the room as no longer keyed (e.g. on a silence timeout in the live
// daemon). The next keyup then counts as a fresh over from idle.
func (v *VoiceState) Idle() {
	if v.talker != "" {
		if v.m != nil {
			v.m.ClearTalker(v.room)
		}
		v.broker.Publish(Event{Type: EventIdle, Room: v.room})
	}
	v.talker = ""
}
