package control

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/modelmonitor"
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

func TestModelMonitorContentionShedsWithoutClosingSocket(t *testing.T) {
	s, base, key := clientSocketFixture(t, nil)
	c := dialClientSocket(t, base, key, "")
	writeClientSocket(t, c, `{"channel":"model_monitor","type":"subscribe","subscription_id":"sub"}`)
	_ = readClientSocket(t, c)
	s.clientSockets.mu.Lock()
	socket := s.clientSockets.devices["instance"]
	s.clientSockets.mu.Unlock()
	socket.writeMu.Lock()
	modelmonitor.Publish(modelmonitor.Event{Component: "worker", CallID: "call", Time: time.Now().UTC(), Type: "request"})
	// The monitor goroutine must reach gap delivery while the shared writer is busy.
	deadline := time.Now().Add(2 * time.Second)
	for socket.writeWaiters.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if socket.writeWaiters.Load() == 0 {
		socket.writeMu.Unlock()
		t.Fatal("monitor did not shed")
	}
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
	writeClientSocket(t, c, `{"channel":"unknown"}`)
	if f := readClientSocket(t, c); string(f["channel"]) != `"transport"` {
		t.Fatalf("socket unusable: %s", f)
	}
}
