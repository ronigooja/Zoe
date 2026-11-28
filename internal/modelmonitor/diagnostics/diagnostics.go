// Package diagnostics records bounded, content-free monitor loss counters.
package diagnostics

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/ronigooja/Zoe/internal/logging"
)

type key struct {
	stage, reason, id, event string
	status                   int
}

var counters = struct {
	sync.Mutex
	values map[key]uint64
	timer  *time.Timer
}{values: make(map[key]uint64)}

// Record accepts fixed stage/reason values from call sites, never error strings.
// Identifiers are hashed and event types are allowlisted before logging.
func Record(stage, reason, id, event string, status int) {
	switch event {
	case "request", "response", "tool_call", "tool_result", "error", "gap":
	default:
		event = "other"
	}
	if id != "" {
		sum := sha256.Sum256([]byte(id))
		id = fmt.Sprintf("%x", sum[:8])
	}
	if status < 100 || status > 599 {
		status = 0
	}
	k := key{stage, reason, id, event, status}
	counters.Lock()
	if len(counters.values) >= 128 {
		if _, ok := counters.values[k]; !ok {
			k = key{stage: "monitor", reason: "aggregate_overflow", event: "other"}
		}
	}
	counters.values[k]++
	if counters.timer == nil {
		counters.timer = time.AfterFunc(time.Second, Flush)
	}
	counters.Unlock()
}

// Flush emits counters even when a job or subscription ends before the timer.
func Flush() {
	counters.Lock()
	if counters.timer != nil {
		counters.timer.Stop()
		counters.timer = nil
	}
	values := counters.values
	counters.values = make(map[key]uint64)
	counters.Unlock()
	for k, n := range values {
		logging.Infof("model_monitor_loss stage=%s reason=%s correlation_hash=%s event_type=%s status=%d count=%d", k.stage, k.reason, k.id, k.event, k.status, n)
	}
}
