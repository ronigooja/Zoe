package control

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/modelmonitor"
	"github.com/ronigooja/Zoe/internal/modelmonitor/diagnostics"
)

func TestModelMonitorSubscription(t *testing.T) {
	_, base, key := clientSocketFixture(t, nil)
	c := dialClientSocket(t, base, key, "")
	writeClientSocket(t, c, `{"channel":"model_monitor","type":"subscribe","subscription_id":"sub-1"}`)
	if f := readClientSocket(t, c); string(f["type"]) != `"subscribed"` {
		t.Fatalf("ack: %s", f)
	}
	modelmonitor.Publish(modelmonitor.Event{Component: "worker", CallID: "call", Time: time.Now().UTC(), Type: "request", Payload: json.RawMessage(`{"a":1}`)})
	f := readClientSocket(t, c)
	if string(f["type"]) != `"event"` || string(f["sequence"]) != "1" {
		t.Fatalf("event: %s", f)
	}
	modelmonitor.GapFor(modelmonitor.Generation())
	if f := readClientSocket(t, c); string(f["type"]) != `"gap"` || string(f["subscription_id"]) != `"sub-1"` {
		t.Fatalf("gap: %s", f)
	}
	modelmonitor.Publish(modelmonitor.Event{Component: "worker", CallID: "call", Time: time.Now().UTC(), Type: "response"})
	if f := readClientSocket(t, c); string(f["type"]) != `"event"` || string(f["sequence"]) != "2" {
		t.Fatalf("event after gap: %s", f)
	}
	writeClientSocket(t, c, `{"channel":"model_monitor","type":"unsubscribe","subscription_id":"sub-1"}`)
	if f := readClientSocket(t, c); string(f["type"]) != `"unsubscribed"` {
		t.Fatalf("unsubscribe: %s", f)
	}
	writeClientSocket(t, c, `{"channel":"model_monitor","type":"subscribe","subscription_id":"bad id"}`)
	if f := readClientSocket(t, c); string(f["type"]) != `"error"` {
		t.Fatalf("invalid id: %s", f)
	}
}

func TestModelMonitorOversizeEndsSubscription(t *testing.T) {
	_, base, key := clientSocketFixture(t, nil)
	c := dialClientSocket(t, base, key, "")
	writeClientSocket(t, c, `{"channel":"model_monitor","type":"subscribe","subscription_id":"sub"}`)
	_ = readClientSocket(t, c)
	modelmonitor.Publish(modelmonitor.Event{Component: "worker", CallID: "call", Time: time.Now().UTC(), Type: "request", Payload: json.RawMessage(`"` + strings.Repeat("x", 5000) + `"`)})
	if f := readClientSocket(t, c); string(f["type"]) != `"gap"` {
		t.Fatalf("gap: %s", f)
	}
	if f := readClientSocket(t, c); string(f["type"]) != `"error"` || string(f["error"]) != `{"code":"subscription_ended"}` {
		t.Fatalf("terminal: %s", f)
	}
}

func TestModelMonitorContentionShedsWithoutClosingSocket(t *testing.T) {
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
	s, base, key := clientSocketFixture(t, nil)
	c := dialClientSocket(t, base, key, "")
	writeClientSocket(t, c, `{"channel":"model_monitor","type":"subscribe","subscription_id":"sub"}`)
	_ = readClientSocket(t, c)
	s.clientSockets.mu.Lock()
	socket := s.clientSockets.devices["instance"]
	s.clientSockets.mu.Unlock()
	socket.writeMu.Lock()
	modelmonitor.Publish(modelmonitor.Event{Component: "worker", CallID: "call", Time: time.Now().UTC(), Type: "request", Payload: json.RawMessage(`"private-request"`)})
	// Wait for the monitor to consume the queued event while the writer is busy.
	time.Sleep(100 * time.Millisecond)
	done := make(chan error, 1)
	go func() {
		done <- socket.write(context.Background(), map[string]string{"channel": "mcp", "type": "call"})
	}()
	socket.writeMu.Unlock()
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		f := readClientSocket(t, c)
		seen[string(f["channel"])+":"+string(f["type"])] = true
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !seen[`"model_monitor":"gap"`] || !seen[`"mcp":"call"`] {
		t.Fatalf("frames: %v", seen)
	}
	diagnostics.Flush()
	if !strings.Contains(output.String(), "stage=writer reason=admission_blocked") || strings.Contains(output.String(), "private-request") {
		t.Fatal("missing or unsafe monitor writer diagnostic")
	}
	modelmonitor.Publish(modelmonitor.Event{Component: "worker", CallID: "call", Time: time.Now().UTC(), Type: "response"})
	if f := readClientSocket(t, c); string(f["type"]) != `"event"` || string(f["sequence"]) != "2" {
		t.Fatalf("subscription did not survive: %s", f)
	}
	writeClientSocket(t, c, `{"channel":"unknown"}`)
	if f := readClientSocket(t, c); string(f["channel"]) != `"transport"` {
		t.Fatalf("socket unusable: %s", f)
	}
}
