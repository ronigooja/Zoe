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
	Events chan Event
	Gap    chan struct{}
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

func advanceGeneration() {
	registry.generation++
	if registry.generation == 0 {
		registry.generation++
	}
}

func Subscribe() *Subscription {
	s := &Subscription{Events: make(chan Event, queueSize), Gap: make(chan struct{})}
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
	for s := range registry.subs {
		select {
		case s.Events <- e:
		default:
			delete(registry.subs, s)
			advanceGeneration()
			count.Add(-1)
			close(s.Gap)
		}
	}
}
