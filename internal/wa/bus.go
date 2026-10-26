package wa

import "sync"

// Event is a change notification for live clients (the web UI).
type Event struct {
	Type string `json:"type"` // status, chat, message, sync, presence, typing
	Chat string `json:"chat,omitempty"`
	ID   string `json:"id,omitempty"`
	Data any    `json:"data,omitempty"`
}

// Bus fans events out to subscribers. Slow subscribers drop events rather
// than block the WhatsApp event loop.
type Bus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewBus creates an event bus.
func NewBus() *Bus { return &Bus{subs: map[chan Event]struct{}{}} }

// Subscribe returns a channel of events and a function to unsubscribe.
func (b *Bus) Subscribe() (<-chan Event, func()) {
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

// Publish delivers e to every subscriber that has room for it.
func (b *Bus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}
