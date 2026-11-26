package control

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
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
