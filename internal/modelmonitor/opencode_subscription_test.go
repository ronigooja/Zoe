package modelmonitor

import (
	"testing"
	"time"
)

func TestOpenCodeContextOptInDoesNotFillLegacyQueue(t *testing.T) {
	legacy := Subscribe()
	defer legacy.Close()
	modern := SubscribeWithOpenCodeContext()
	defer modern.Close()
	e := Event{Component: "opencode_context", CallID: "call", Time: time.Now(), Type: "request"}
	for i := 0; i < queueSize+1; i++ {
		Publish(e)
	}
	select {
	case <-legacy.Events:
		t.Fatal("legacy received context")
	case <-legacy.Gap:
		t.Fatal("context filled legacy queue")
	default:
	}
	select {
	case <-modern.Gap:
	default:
		t.Fatal("opt-in queue did not receive context")
	}
	Publish(Event{Component: "worker", CallID: "call", Time: time.Now(), Type: "request"})
	select {
	case item := <-legacy.Events:
		if item.Event.Component != "worker" {
			t.Fatal("worker changed")
		}
	default:
		t.Fatal("worker missing")
	}
}
