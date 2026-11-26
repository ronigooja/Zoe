package control

import (
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
	writeClientSocket(t, c, `{"channel":"model_monitor","type":"unsubscribe","subscription_id":"sub-1"}`)
	if f := readClientSocket(t, c); string(f["type"]) != `"unsubscribed"` {
		t.Fatalf("unsubscribe: %s", f)
	}
	writeClientSocket(t, c, `{"channel":"model_monitor","type":"subscribe","subscription_id":"bad id"}`)
	if f := readClientSocket(t, c); string(f["type"]) != `"error"` {
		t.Fatalf("invalid id: %s", f)
	}
}
