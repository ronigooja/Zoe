package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/modelmonitor"
)

func TestModelObservationAssignmentFence(t *testing.T) {
	s, closeStore := taskEntriesTestServer(t)
	defer closeStore()
	_, err := s.store.DB().Exec(`INSERT INTO runs(id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at) VALUES('entry-run','entries-task','attempt','active','run',1,1,'runner','entries-device','cmd','goal','worker','opencode','runtime','model','worker','2026-10-24T12:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		runner, scope, attempt, fence string
		want                          int
	}{
		{"runner", "runner:control:read", "attempt", "1", 200},
		{"other", "runner:control:read", "attempt", "1", 409},
		{"runner", "runner:observe", "attempt", "1", 403},
		{"runner", "runner:control:read", "other", "1", 409},
		{"runner", "runner:control:read", "attempt", "2", 409},
	} {
		r := httptest.NewRequest("GET", "/runner/v1/runs/entry-run/model-observation?attempt_id="+tc.attempt+"&fence="+tc.fence, nil)
		r.SetPathValue("run_id", "entry-run")
		r = r.WithContext(context.WithValue(r.Context(), runnerPrincipalKey{}, contracts.RunnerPrincipal{RunnerID: tc.runner, Scopes: []string{tc.scope}}))
		w := httptest.NewRecorder()
		s.handleModelObservation(w, r)
		if w.Code != tc.want {
			t.Fatalf("%+v: status %d", tc, w.Code)
		}
	}
}

func TestModelObservationPostGenerationAndShape(t *testing.T) {
	s, closeStore := taskEntriesTestServer(t)
	defer closeStore()
	_, err := s.store.DB().Exec(`INSERT INTO runs(id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at) VALUES('entry-run','entries-task','attempt','active','run',1,1,'runner','entries-device','cmd','goal','worker','opencode','runtime','model','worker','2026-10-24T12:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	sub := modelmonitor.Subscribe()
	defer sub.Close()
	generation := modelmonitor.Generation()
	post := func(in modelObservation, runner string) int {
		body, _ := json.Marshal(in)
		r := httptest.NewRequest("POST", "/", bytes.NewReader(body))
		r.SetPathValue("run_id", "entry-run")
		r = r.WithContext(context.WithValue(r.Context(), runnerPrincipalKey{}, contracts.RunnerPrincipal{RunnerID: runner, Scopes: []string{"runner:control:read"}}))
		w := httptest.NewRecorder()
		s.handleModelObservation(w, r)
		return w.Code
	}
	in := modelObservation{Boot: modelObservationBoot, Generation: generation, Attempt: "attempt", Fence: 1, CallID: strings.Repeat("a", 64), Source: "worker_rotation", Type: "response", Text: "Bearer abc123"}
	if code := post(in, "other"); code != 409 {
		t.Fatalf("wrong runner: %d", code)
	}
	if code := post(in, "runner"); code != 204 {
		t.Fatalf("valid: %d", code)
	}
	select {
	case item := <-sub.Events:
		if item.Event.Component != "worker_rotation" || bytes.Contains(item.Event.Payload, []byte("abc123")) {
			t.Fatal("incorrect source or redaction")
		}
	default:
		t.Fatal("missing event")
	}
	newSub := modelmonitor.Subscribe()
	defer newSub.Close()
	if code := post(in, "runner"); code != 204 {
		t.Fatalf("stale: %d", code)
	}
	select {
	case <-newSub.Events:
		t.Fatal("stale generation delivered")
	default:
	}
	in.Generation = modelmonitor.Generation()
	in.Source = "worker"
	in.Type = "tool_call"
	in.Text = "read arbitrary output"
	if code := post(in, "runner"); code != 400 {
		t.Fatalf("tool shape: %d", code)
	}
}
