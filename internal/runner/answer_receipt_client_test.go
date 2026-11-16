package runner

import (
	"context"
	"encoding/json"
	"github.com/ronigooja/Zoe/contracts"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnswerReceiptClientIdentityOnly(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/runner/v1/runs/run/entry-deliveries" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body) != 5 || body["entry_id"] != "entry" || body["question_id"] != "question" || body["action_digest"] != "digest" || body["attempt_id"] != "attempt" || body["fence"] != float64(7) {
			t.Errorf("receipt=%v", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := NewControlClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	receipt := contracts.EntryDelivery{AttemptID: "attempt", Fence: 7, EntryID: "entry", QuestionID: "question", ActionDigest: "digest"}
	for i := 0; i < 2; i++ {
		if err := client.AcknowledgeEntry(context.Background(), "run", receipt); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}
