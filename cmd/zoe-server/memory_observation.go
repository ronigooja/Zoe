package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/modelmonitor"
)

// A lease is unique per monitor generation and process. Failed or unavailable
// observation never affects normal memory calls or health.
func runMemoryObservation(stop <-chan struct{}, client *memoryapi.Client) {
	if client == nil { return }
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var generation uint64
	var lease string
	for {
		select {
		case <-stop: return
		case <-ticker.C:
		}
		current := modelmonitor.Generation()
		if current == 0 { generation, lease = 0, ""; continue }
		if current != generation {
			var token [16]byte
			if _, err := rand.Read(token[:]); err != nil { generation, lease = 0, ""; continue }
			generation, lease = current, hex.EncodeToString(token[:])
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		events, err := client.DrainObservations(ctx, lease)
		cancel()
		if err != nil { continue }
		for _, e := range events {
			if (e.Component != "memory_write" && e.Component != "memory_ingestion") || (e.Type != "request" && e.Type != "response") || len(e.Payload) > 16384 { continue }
			modelmonitor.PublishFor(generation, modelmonitor.Event{Component: e.Component, CallID: e.CallID, Time: e.Time, Type: e.Type, Payload: e.Payload, Truncated: e.Truncated})
		}
	}
}
