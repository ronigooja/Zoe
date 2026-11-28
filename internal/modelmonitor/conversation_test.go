package modelmonitor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStructuredRequestUsesExistingBudgetWithoutReplacingPayload(t *testing.T) {
	s := Subscribe()
	defer s.Close()
	for _, size := range []int{1024, 20 * 1024} {
		text := strings.Repeat("a", size)
		payload, _ := json.Marshal(text)
		messages := []RequestMessage{{Role: "user", Text: text}}
		Publish(Event{Component: "worker", CallID: "call", Type: "request", Time: time.Now(), Payload: payload, RequestMessages: messages})
		messages[0].Text = "overwritten after publish"
		e := (<-s.Events).Event
		if string(e.Payload) != string(payload) || e.Truncated || e.Incomplete {
			t.Fatal("optional metadata changed the payload")
		}
		if size == 1024 && (len(e.RequestMessages) != 1 || e.RequestMessages[0].Text != text) || size != 1024 && len(e.RequestMessages) != 0 {
			t.Fatal("structured request was not copied or budgeted")
		}
	}
}
