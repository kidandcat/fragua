package core

import "sync"

// EventKind classifies project change events.
type EventKind string

const (
	EventProjectChanged   EventKind = "project_changed"
	EventBoardChanged     EventKind = "board_changed"
	EventSchematicChanged EventKind = "schematic_changed"
	EventActivity         EventKind = "activity"
	EventSaved            EventKind = "saved"
)

// ActivityLevel is the severity of an activity log line.
type ActivityLevel string

const (
	ActivityInfo    ActivityLevel = "info"
	ActivityWarn    ActivityLevel = "warn"
	ActivityError   ActivityLevel = "error"
	ActivitySuccess ActivityLevel = "success"
)

// Event is a change notification for UI / agents.
type Event struct {
	Kind    EventKind     `json:"kind"`
	Level   ActivityLevel `json:"level,omitempty"`
	Message string        `json:"message,omitempty"`
	Path    string        `json:"path,omitempty"`
}

// EventBus is a fan-out broadcaster for project events.
type EventBus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewEventBus creates an empty bus.
func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[chan Event]struct{})}
}

// Subscribe returns a channel of events. Caller must Unsubscribe.
func (b *EventBus) Subscribe(buf int) chan Event {
	if buf < 1 {
		buf = 16
	}
	ch := make(chan Event, buf)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber and closes the channel.
func (b *EventBus) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}

// Publish sends e to all subscribers (non-blocking; drops if full).
func (b *EventBus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
			// drop if slow consumer
		}
	}
}
