package wiresxnet

import (
	"sync"
	"time"
)

// Live events for subscribers (an SSE client, a bot). The daemon already holds
// the authoritative state, so these are just notifications that something
// changed: a keyup, a room idle, a connect/disconnect. A subscriber reads the
// detail from the JSON endpoints or carries enough in the event itself.

// EventType names what happened.
const (
	EventTalker     = "talker"     // someone keyed up in a room
	EventIdle       = "idle"       // a room went quiet
	EventConnect    = "connect"    // the node joined a room
	EventDisconnect = "disconnect" // the node left a room
)

// Event is one notification.
type Event struct {
	Type string    `json:"type"`
	Room string    `json:"room,omitempty"`
	Call string    `json:"call,omitempty"` // the talker, for EventTalker
	From string    `json:"from,omitempty"` // the previous talker, for EventTalker
	Time time.Time `json:"time"`
}

// Broker fans out events to subscribers. Publish never blocks: a subscriber
// whose buffer is full misses the event rather than stalling the capture path.
// The zero value is not usable; call NewBroker.
type Broker struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewBroker returns an empty broker.
func NewBroker() *Broker {
	return &Broker{subs: make(map[chan Event]struct{})}
}

// Subscribe returns a channel of events and a function to unsubscribe (which
// closes the channel). The channel is buffered; drain it promptly.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}

// Publish delivers e to every current subscriber, dropping it for any whose
// buffer is full. It is safe to call on a nil Broker (a no-op), so callers on
// the capture path need not check.
func (b *Broker) Publish(e Event) {
	if b == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.Lock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default: // slow subscriber; drop rather than block
		}
	}
	b.mu.Unlock()
}
