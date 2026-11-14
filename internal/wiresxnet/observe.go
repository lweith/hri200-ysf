package wiresxnet

import (
	"errors"
	"io"
)

// Running passively underneath the real WIRES-X.exe. The simplest and
// lowest-risk way to run this is to emit nothing: read the frames the live node
// already exchanges with Yaesu — from a tap, or spliced inline — and feed them
// through the same codec the active node would use. That alone fills the
// directory cache and the metrics, and (when a real over or an 8320 frame goes
// by) is how the still-unknown voice path gets learned: by watching, not by
// forging traffic.
//
// Observe consumes one direction of a reassembled TCP stream. Call it once per
// direction (node->server and server->node); a Directory and Metrics may be
// shared between the two calls. It routes each frame by type and verb:
// directory pages into the cache, room control into the connect/disconnect
// counters, everything into the per-verb frame counter. onFrame, if non-nil, is
// called for every parsed frame (for logging or to drive position-stripping).

// Observer is the shared state a passive tap updates.
type Observer struct {
	Dir     *Directory
	Metrics *Metrics
	// OnFrame, if set, is called for each parsed message, before routing. It is
	// the hook where a caller inspects or (in an inline proxy) rewrites frames,
	// e.g. to strip Mic-E position from a RAW voice record.
	OnFrame func(Message)
	// Voice tracks talkers from the room stream. Created on the first RR record
	// if nil, so a zero Observer works; set it to seed the room label.
	Voice *VoiceState
	// Broker, if set, receives live events (keyups, connects) for SSE clients.
	Broker *Broker
}

// Observe reads frames from one direction of a stream until EOF. A frame that
// fails to parse is skipped (counted under the "bad" verb) so a single bad frame
// or a mid-stream tap start does not stop the loop. It returns the first I/O
// error that is not io.EOF.
func (o *Observer) Observe(r io.Reader) error {
	rd := NewReader(r)
	for {
		f, err := rd.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, ErrBadFrame) {
				o.count("bad")
				continue
			}
			return err
		}
		o.handle(f)
	}
}

// handle parses and routes one frame.
func (o *Observer) handle(f Frame) {
	m, err := ParseBody(f.Body)
	if err != nil {
		o.count("bad")
		return
	}
	if o.OnFrame != nil {
		o.OnFrame(m)
	}
	switch {
	case m.Verb == VerbPage:
		o.count(VerbPage)
		if o.Dir != nil {
			o.Dir.IngestPage(m.Payload)
		}
	case m.Verb == VerbVoice:
		o.count(VerbVoice)
		if o.Voice == nil {
			o.Voice = NewVoiceState(o.Metrics, "")
			o.Voice.broker = o.Broker
		}
		o.Voice.HandleRR(m.Payload)
	case m.Verb == VerbPoll:
		o.count(VerbPoll)
	case m.Type == TypeNodeData:
		o.handleData(m)
	default:
		o.count(verbLabel(m))
	}
}

// handleData routes an &D room-control frame. The record verb (CC/CR/CP/CU)
// follows the session byte and routing header; parseDataVerb finds it.
func (o *Observer) handleData(m Message) {
	verb, room := parseDataVerb(m.Payload)
	o.count(verb)
	if o.Metrics == nil {
		return
	}
	switch verb {
	case DataConnect:
		o.Metrics.CountConnect(room)
		o.Broker.Publish(Event{Type: EventConnect, Room: room})
	case DataDisconnect:
		o.Metrics.CountDisconnect(room)
		o.Broker.Publish(Event{Type: EventDisconnect, Room: room})
	}
}

func (o *Observer) count(verb string) {
	if o.Metrics != nil {
		o.Metrics.CountVerb(verb)
	}
}

// verbLabel names a message for the frame counter when it has no leading verb.
func verbLabel(m Message) string {
	if m.Verb != "" {
		return m.Verb
	}
	switch m.Type {
	case TypeConnect:
		return "&C"
	case TypeNodeData:
		return "&D"
	default:
		return string(m.Type)
	}
}
