package voice

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/modelmonitor"
)

func TestRecallObservationPrivacyAndMembership(t *testing.T) {
	sub := modelmonitor.Subscribe()
	defer sub.Close()
	if startRecallObservation("other-owner", "private") != nil {
		t.Fatal("observed another owner")
	}
	o := startRecallObservation("owner", "password=secret")
	if o == nil {
		t.Fatal("missing observation")
	}
	request := recallEvent(t, sub)
	var payload map[string]string
	if err := json.Unmarshal(request.Payload, &payload); err != nil || strings.Contains(payload["input"], "secret") {
		t.Fatalf("request redaction failed: %v", err)
	}
	o.response(strings.Repeat("x", 8193))
	response := recallEvent(t, sub)
	if response.Component != "recall" || response.Type != "response" || response.CallID != request.CallID || !response.Truncated {
		t.Fatal("response identity or bound incorrect")
	}
	newSub := modelmonitor.Subscribe()
	defer newSub.Close()
	o.response("must not reach a new subscriber")
	select {
	case <-newSub.Events:
		t.Fatal("stale observation reached new subscriber")
	default:
	}
}

func recallEvent(t *testing.T, sub *modelmonitor.Subscription) modelmonitor.Event {
	t.Helper()
	select {
	case item := <-sub.Events:
		if item.Gap {
			t.Fatal("unexpected monitor gap")
		}
		return item.Event
	default:
		t.Fatal("missing monitor event")
		return modelmonitor.Event{}
	}
}

func TestRunRecallObservation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []map[string]string
		want  string
	}{
		{"multiline", []map[string]string{{"text": "first line\nsecond line"}, {"text": "another memory"}}, "first line\nsecond line\nanother memory"},
		{"empty", []map[string]string{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := workerClient(t, func(w http.ResponseWriter, req *http.Request) {
				var body map[string]string
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body["request"] != "remember our conversations" || req.URL.Path != "/v1/memory/search" {
					t.Errorf("unexpected search request: %v, %v", body, err)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"items": tc.items})
			})
			store, svc := recallRecoveryFixture(t, Config{MemoryAPI: worker}, 1)
			if _, err := store.DB().Exec(`UPDATE voice_recall_jobs SET status='running' WHERE id='job-0'`); err != nil {
				t.Fatal(err)
			}
			sub := modelmonitor.Subscribe()
			defer sub.Close()
			svc.runRecall("job-0")
			request, reply := recallEvent(t, sub), recallEvent(t, sub)
			if request.Component != "recall" || request.Type != "request" || reply.Component != "recall" || reply.Type != "response" || request.CallID == "" || reply.CallID != request.CallID {
				t.Fatal("incorrect event order or identity")
			}
			var input, output map[string]string
			if json.Unmarshal(request.Payload, &input) != nil || json.Unmarshal(reply.Payload, &output) != nil || input["input"] != "remember our conversations" || output["content"] != tc.want {
				t.Fatalf("unexpected payloads: %s %s", request.Payload, reply.Payload)
			}
			if _, ok := output["content"]; !ok {
				t.Fatal("missing response content")
			}
			select {
			case <-sub.Events:
				t.Fatal("unexpected extra event")
			default:
			}
		})
	}
}
