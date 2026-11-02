package acceptance

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

func TestConfiguredRunCapKeepsUnknownOutcomeFenced(t *testing.T) {
	code, deviceToken := reviewToken(t), reviewToken(t)
	runnerTokens := []string{reviewToken(t), reviewToken(t), reviewToken(t), reviewToken(t), reviewToken(t)}
	profile := control.RuntimeProfile{ID: "worker", RuntimeRouteID: "native", ModelRouteID: "model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1"}
	_, h := reviewServer(t, control.Config{DefaultProfileID: "worker", MaxRuns: 4, Profiles: map[string]control.RuntimeProfile{"worker": profile}}, func(s *control.Server) {
		for i, token := range runnerTokens {
			if err := s.RegisterRunnerCredential(context.Background(), token, "runner-"+strconv.Itoa(i), []string{"default"}, []string{"runner:claim", "runner:observe", "runner:control:read"}); err != nil {
				t.Fatal(err)
			}
		}
	})
	reviewInitializeInstance(t, h, code, deviceToken)
	for i := 0; i < 5; i++ {
		n := strconv.Itoa(i + 1)
		body := json.RawMessage(`{"title":"task ` + n + `","goal":"goal","source":"user"}`)
		response := reviewRequest(h, http.MethodPost, "/v1/commands", deviceToken, "task-"+n, contracts.CommandRequest{Kind: "task.create", Payload: body})
		if response.Code != http.StatusOK {
			t.Fatalf("create task %d=%d %s", i, response.Code, response.Body.String())
		}
	}
	claims := make([]contracts.JobClaim, 0, 4)
	for i := 0; i < 4; i++ {
		response := reviewRequest(h, http.MethodPost, "/runner/v1/claims", runnerTokens[i], "", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("claim %d=%d", i, response.Code)
		}
		var claim contracts.JobClaim
		if err := json.Unmarshal(response.Body.Bytes(), &claim); err != nil {
			t.Fatal(err)
		}
		claims = append(claims, claim)
	}
	unknown := contracts.RunnerObservation{ObservationID: "unknown-observation", RunID: claims[0].RunID, AttemptID: claims[0].AttemptID, Fence: claims[0].Fence, Kind: "effect_unknown"}
	if response := reviewRequest(h, http.MethodPost, "/runner/v1/runs/"+claims[0].RunID+"/observations", runnerTokens[0], "effect-unknown", unknown); response.Code != http.StatusOK {
		t.Fatalf("unknown effect observation=%d %s", response.Code, response.Body.String())
	}
	if response := reviewRequest(h, http.MethodPost, "/runner/v1/claims", runnerTokens[4], "", nil); response.Code != http.StatusNoContent {
		t.Fatalf("fifth claim escaped global cap after unknown effect: %d", response.Code)
	}
}
