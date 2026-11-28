// Package modelmonitor provides a bounded, live-only process-local event feed.
package modelmonitor

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

type Event struct {
	Component string          `json:"component"`
	CallID    string          `json:"call_id"`
	Time      time.Time       `json:"time"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

// Bound memory even when callers supply an unexpectedly large payload.
const maxPayload = 32 * 1024
const queueSize = 32

type Subscription struct {
	Events                 chan Item
	Gap                    chan struct{}
	includeOpenCodeContext bool
}

// Item is either an event or an in-band source-loss notice.
type Item struct {
	Event Event
	Gap   bool
}

var registry = struct {
	sync.Mutex
	subs       map[*Subscription]struct{}
	generation uint64
}{subs: make(map[*Subscription]struct{})}
var count atomic.Int64

func Enabled() bool { return count.Load() != 0 }

// Generation identifies the current subscriber membership, or zero when empty.
func Generation() uint64 {
	registry.Lock()
	defer registry.Unlock()
	if len(registry.subs) == 0 {
		return 0
	}
	return registry.generation
}

// OpenCodeGeneration is zero unless a client has requested the dedicated feed.
func OpenCodeGeneration() uint64 {
	registry.Lock()
	defer registry.Unlock()
	for sub := range registry.subs {
		if sub.includeOpenCodeContext {
			return registry.generation
		}
	}
	return 0
}

func advanceGeneration() {
	registry.generation++
	if registry.generation == 0 {
		registry.generation++
	}
}

func Subscribe() *Subscription {
	return subscribe(false)
}

func SubscribeWithOpenCodeContext() *Subscription {
	return subscribe(true)
}

func subscribe(include bool) *Subscription {
	s := &Subscription{Events: make(chan Item, queueSize), Gap: make(chan struct{}), includeOpenCodeContext: include}
	registry.Lock()
	registry.subs[s] = struct{}{}
	advanceGeneration()
	count.Add(1)
	registry.Unlock()
	return s
}

func (s *Subscription) Close() {
	registry.Lock()
	if _, ok := registry.subs[s]; ok {
		delete(registry.subs, s)
		advanceGeneration()
		count.Add(-1)
	}
	registry.Unlock()
}

func Publish(e Event) {
	publish(0, false, e)
}

// PublishFor delivers only if membership has not changed since Generation.
func PublishFor(generation uint64, e Event) {
	if generation == 0 {
		return
	}
	publish(generation, true, e)
}

// GapFor reports source loss to the membership identified by generation.
func GapFor(generation uint64) {
	if generation == 0 {
		return
	}
	registry.Lock()
	defer registry.Unlock()
	if generation != registry.generation {
		return
	}
	enqueue(Item{Gap: true})
}

func publish(generation uint64, fenced bool, e Event) {
	if !Enabled() {
		return
	}
	if len(e.Component) > 128 || len(e.CallID) > 128 || e.Component == "" || e.CallID == "" || e.Time.IsZero() {
		return
	}
	switch e.Type {
	case "request", "response", "error", "tool_call", "tool_result":
	default:
		return
	}
	if len(e.Payload) > maxPayload || (len(e.Payload) > 0 && !json.Valid(e.Payload)) {
		e.Payload = nil
		e.Truncated = true
	} else {
		e.Payload = append(json.RawMessage(nil), e.Payload...)
	}
	registry.Lock()
	defer registry.Unlock()
	if fenced && generation != registry.generation {
		return
	}
	enqueue(Item{Event: e})
}

// enqueue runs under registry's lock, preserving event and gap ordering.
func enqueue(item Item) {
	for s := range registry.subs {
		if !item.Gap && item.Event.Component == "opencode_context" && !s.includeOpenCodeContext {
			continue
		}
		select {
		case s.Events <- item:
		default:
			delete(registry.subs, s)
			advanceGeneration()
			count.Add(-1)
			close(s.Gap)
		}
	}
}
