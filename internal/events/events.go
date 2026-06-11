// Package events is the daemon's in-process event bus: lifecycle changes
// (sessions, jobs, approvals, images) fan out to subscribed watchers - the
// WatchEvents RPC that lets a client update live instead of polling.
// In-process by design: one daemon, one box, and events are hints (a client
// re-fetches the entity it cares about), so nothing here is load-bearing for
// state. The embedded-NATS option (DESIGN.md §9) stays open if cross-process
// consumers ever appear.
package events

import (
	"sync"
	"time"
)

// Entity types an Event describes.
const (
	TypeSession  = "session"
	TypeJob      = "job"
	TypeApproval = "approval"
	TypeImage    = "image"
	TypeVolume   = "volume"
)

// Event is one lifecycle change. Content-light on purpose: subscribers fetch
// current state over the normal RPCs; the event just says what moved.
type Event struct {
	// Type is the entity kind (the Type* constants).
	Type string
	// Action is what happened, in the entity's own vocabulary (e.g. a session's
	// "running"/"stopped"/"deleted", an approval's "created"/"approved").
	Action string
	// ID and Name identify the entity (either may be empty when not applicable).
	ID   string
	Name string
	// At is when it happened.
	At time.Time
}

// Sink is the publishing side of the bus, what event producers depend on.
type Sink interface {
	Publish(e Event)
}

// subscriberBuffer bounds each subscriber's queue. Events are hints, so a
// slow subscriber overflowing its buffer loses events rather than blocking
// publishers; a client that cares re-syncs by re-listing.
const subscriberBuffer = 64

// Bus fans events out to subscribers. The zero value is not usable; NewBus.
type Bus struct {
	mu   sync.Mutex
	subs map[int]chan Event
	next int
}

// NewBus constructs an empty bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[int]chan Event)}
}

// Publish delivers e to every subscriber. Never blocks: a full subscriber
// buffer drops the event for that subscriber.
func (b *Bus) Publish(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Subscribe registers a new subscriber, returning its channel and a cancel
// func that must be called when done (e.g. deferred in the stream handler).
func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan Event, subscriberBuffer)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs, id)
	}
}
