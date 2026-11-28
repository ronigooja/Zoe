package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/modelmonitor"
)

// A failure is uncertain only after a successful lease. Coalesce a continuous
// failure episode; a successful drain re-arms notification.
type memoryDrainState struct{ active, gapSent bool }

func (s *memoryDrainState) result(failed, overflow bool) bool {
	if failed {
		if s.active && !s.gapSent {
			s.gapSent = true
			return true
		}
		return false
	}
	s.active, s.gapSent = true, false
	return overflow
}

// A lease is unique per monitor generation and process. Failed or unavailable
// observation never affects normal memory calls or health.
func runMemoryObservation(stop <-chan struct{}, client *memoryapi.Client) {
	if client == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var generation uint64
	var lease string
	var state memoryDrainState
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		current := modelmonitor.Generation()
		if current == 0 {
			generation, lease, state = 0, "", memoryDrainState{}
			continue
		}
		if current != generation {
			var token [16]byte
			if _, err := rand.Read(token[:]); err != nil {
				generation, lease = 0, ""
				continue
			}
			generation, lease = current, hex.EncodeToString(token[:])
			state = memoryDrainState{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		events, overflow, err := client.DrainObservations(ctx, lease)
		cancel()
		if state.result(err != nil, overflow) {
			modelmonitor.GapFor(generation)
		}
		if err != nil {
			continue
		}
		for _, e := range events {
			if (e.Component != "memory_write" && e.Component != "memory_ingestion") || (e.Type != "request" && e.Type != "response") || len(e.Payload) > 16384 {
				continue
			}
			modelmonitor.PublishFor(generation, modelmonitor.Event{
				Component: e.Component, CallID: e.CallID, Time: e.Time, Type: e.Type,
				Payload: e.Payload, Truncated: e.Truncated, Incomplete: e.Incomplete,
				ConversationStart: e.Type == "request" && e.ConversationStart,
			})
		}
	}
}
