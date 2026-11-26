package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/agents/opencode"
)

func TestModelObservationRedactionAndBound(t *testing.T) {
	text, truncated := redactObservation("Bearer abc123 password=hunter2 known-secret "+strings.Repeat("x", 9000), []string{"known-secret"})
	if !truncated || len(text) > 8192 || strings.Contains(text, "abc123") || strings.Contains(text, "hunter2") || strings.Contains(text, "known-secret") {
		t.Fatal("redaction or bound failed")
	}
}

func TestModelObservationDedupAndToolBoundary(t *testing.T) {
	s := &observationSender{queue: make(chan observationItem, 8), seen: make(map[string]bool)}
	s.lease.Store(&observationLease{Boot: "boot", Generation: 1})
	ctx := context.WithValue(context.Background(), observationKey{}, s)
	m := opencode.Message{ID: "message", Type: "assistant", Parts: []map[string]any{{"type": "tool", "tool": "read", "input": "secret input", "output": "secret output"}}}
	observeMessages(ctx, "session", []opencode.Message{m, m})
	if len(s.queue) != 1 {
		t.Fatalf("got %d observations", len(s.queue))
	}
	item := <-s.queue
	if item.typ != "tool_call" || item.text != "read" {
		t.Fatalf("unexpected tool observation: %+v", item)
	}
	s.lease.Store(&observationLease{Boot: "boot", Generation: 2})
	observeMessages(ctx, "session", []opencode.Message{m})
	if len(s.queue) != 0 {
		t.Fatal("new subscription replayed an old observation")
	}
}
