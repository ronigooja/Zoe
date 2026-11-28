package runner

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/agents/opencode"
	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/modelmonitor/diagnostics"
)

func TestObservationLossDiagnostics(t *testing.T) {
	diagnostics.Flush()
	var output bytes.Buffer
	old, level := log.Writer(), logging.CurrentLevel()
	log.SetOutput(&output)
	logging.SetLevel(logging.Info)
	t.Cleanup(func() {
		diagnostics.Flush()
		log.SetOutput(old)
		logging.SetLevel(level)
	})
	s := &observationSender{runID: "test-run-private", queue: make(chan observationItem, 1), seen: make(map[string]bool)}
	s.lease.Store(&observationLease{Boot: "boot", Generation: 1})
	ctx := context.WithValue(context.Background(), observationKey{}, s)
	observeMessages(ctx, "private-session", nil)
	observeModel(ctx, "private-session", "response", "a", "private-observation")
	observeModel(ctx, "private-session", "response", "b", "private-observation")
	recordObservationUploadFailure(ctx, s.runID, "request", &http.Response{StatusCode: 413}, nil)
	recordObservationUploadFailure(ctx, s.runID, "response", nil, context.DeadlineExceeded)
	recordObservationUploadFailure(ctx, s.runID, "gap", nil, errors.New("private-network-error"))
	diagnostics.Flush()
	for _, want := range []string{"reason=poll_baseline_unknown", "reason=queue_full", "reason=upload_timeout", "reason=http_rejected", "status=413", "reason=network"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(output.String(), "private-") {
		t.Fatal("diagnostics exposed raw identifiers, errors, or payloads")
	}
}

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
	observeMessages(ctx, "session", nil)
	m := opencode.Message{ID: "message", Type: "assistant", Parts: []map[string]any{{"type": "tool", "tool": "read", "input": "secret input", "output": "secret output"}}}
	m.Time.Created = time.Now().Add(time.Second).UnixMilli()
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

func TestRotationObservationContextAndContentFallback(t *testing.T) {
	s := &observationSender{queue: make(chan observationItem, 8), seen: make(map[string]bool)}
	s.lease.Store(&observationLease{Boot: "boot", Generation: 1})
	ctx := context.WithValue(context.Background(), observationKey{}, s)
	ctx = context.WithValue(ctx, rotationObservationKey{}, true)
	observeModel(ctx, "rotation-session", "request", "prompt", "summary prompt")
	observeMessages(ctx, "rotation-session", nil)
	m := opencode.Message{ID: "reply", Type: "assistant", Content: []map[string]any{{"type": "text", "text": "summary reply"}}}
	m.Time.Created = time.Now().Add(time.Second).UnixMilli()
	observeMessages(ctx, "rotation-session", []opencode.Message{m})
	for _, want := range []string{"request", "response"} {
		item := <-s.queue
		if item.source != "worker_rotation" || item.typ != want {
			t.Fatalf("unexpected event: %+v", item)
		}
	}
}

func TestPollingBaselinesPreventHistoryReplay(t *testing.T) {
	s := &observationSender{queue: make(chan observationItem, 8), seen: make(map[string]bool)}
	ctx := context.WithValue(context.Background(), observationKey{}, s)
	old := opencode.Message{ID: "old", Type: "assistant", Text: "private history"}
	old.Time.Created = time.Now().Add(-time.Hour).UnixMilli()
	observeMessages(ctx, "session", []opencode.Message{old})
	s.lease.Store(&observationLease{Boot: "boot", Generation: 1})
	observeMessages(ctx, "session", []opencode.Message{old})
	old.Text = "changed historical message"
	observeMessages(ctx, "session", []opencode.Message{old})
	s.lease.Store(&observationLease{Boot: "boot", Generation: 2})
	observeMessages(ctx, "session", []opencode.Message{old})
	observeMessages(ctx, "session", []opencode.Message{old})
	if len(s.queue) != 0 {
		t.Fatal("historical message replayed")
	}
	if s.gap.Load() == nil {
		t.Fatal("baseline loss not signalled")
	}
}

func TestGapRetryAndGenerationFence(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	c, err := NewControlClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	s := &observationSender{}
	lease := &observationLease{Boot: "boot", Generation: 1, expires: time.Now().Add(time.Minute)}
	s.lease.Store(lease)
	s.gap.Store(lease)
	claim := contracts.JobClaim{RunID: "r", AttemptID: "a", Fence: 1}
	s.flushGap(context.Background(), c, claim, "/gap")
	if s.gap.Load() == nil {
		t.Fatal("failed gap lost")
	}
	s.flushGap(context.Background(), c, claim, "/gap")
	if s.gap.Load() != nil || attempts != 2 {
		t.Fatal("gap not retried")
	}
	s.gap.Store(lease)
	s.lease.Store(&observationLease{Boot: "boot", Generation: 2, expires: time.Now().Add(time.Minute)})
	s.flushGap(context.Background(), c, claim, "/gap")
	if attempts != 2 || s.gap.Load() != nil {
		t.Fatal("stale gap retried")
	}
}

func TestObservationDoesNotDedupBeforeLeaseOrQueue(t *testing.T) {
	s := &observationSender{queue: make(chan observationItem, 1), seen: make(map[string]bool)}
	ctx := context.WithValue(context.Background(), observationKey{}, s)
	observeModel(ctx, "s", "response", "a", "a")
	s.lease.Store(&observationLease{Boot: "boot", Generation: 1})
	observeModel(ctx, "s", "response", "a", "a")
	observeModel(ctx, "s", "response", "b", "b")
	if s.gap.Load() == nil {
		t.Fatal("overflow not reported")
	}
	<-s.queue
	observeModel(ctx, "s", "response", "b", "b")
	if len(s.queue) != 1 {
		t.Fatal("overflow prematurely deduplicated")
	}
}
