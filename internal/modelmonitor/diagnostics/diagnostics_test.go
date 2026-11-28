package diagnostics

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/logging"
)

func TestAggregateAndPrivacy(t *testing.T) {
	Flush()
	var output bytes.Buffer
	old := log.Writer()
	level := logging.CurrentLevel()
	log.SetOutput(&output)
	logging.SetLevel(logging.Info)
	t.Cleanup(func() { Flush(); log.SetOutput(old); logging.SetLevel(level) })
	Record("runner", "queue_full", "private-run-id", "private-event-text", 0)
	Record("runner", "queue_full", "private-run-id", "private-event-text", 0)
	Record("ingress", "assignment_mismatch", "private-run-id", "request", 409)
	Record("runner", "upload_timeout", "private-run-id", "response", 0)
	Record("runner", "network", "private-run-id", "response", 0)
	Record("runner", "http_rejected", "private-run-id", "response", 503)
	Record("source", "queue_full", "private-call-id", "request", 0)
	Record("writer", "admission_blocked", "private-subscription-id", "response", 0)
	Flush()
	got := output.String()
	for _, secret := range []string{"private-run-id", "private-event-text", "private-call-id", "private-subscription-id"} {
		if strings.Contains(got, secret) {
			t.Fatalf("diagnostic contains private input %q", secret)
		}
	}
	for _, want := range []string{"reason=queue_full", "event_type=other", "count=2", "reason=assignment_mismatch", "status=409", "reason=upload_timeout", "reason=network", "reason=http_rejected", "status=503", "stage=source", "stage=writer"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

func TestBoundedKeys(t *testing.T) {
	Flush()
	for i := 0; i < 200; i++ {
		Record("runner", "queue_full", strings.Repeat("x", i+1), "request", 0)
	}
	counters.Lock()
	count := len(counters.values)
	counters.Unlock()
	if count > 129 {
		t.Fatalf("unbounded aggregate: %d", count)
	}
	Flush()
}
