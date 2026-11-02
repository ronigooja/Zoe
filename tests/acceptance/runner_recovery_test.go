package acceptance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

func TestCurrentClaimRecoveryDoesNotDeadlockOrChangeSnapshot(t *testing.T) {
	bootstrapCode, deviceToken, runnerToken := reviewToken(t), reviewToken(t), reviewToken(t)
	profile := control.RuntimeProfile{ID: "worker", RuntimeRouteID: "native", ModelRouteID: "model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1", CapabilityIDs: []string{"task.read"}, Scopes: []string{"task:read"}}
	profiles := map[string]control.RuntimeProfile{"worker": profile}
	store, handler := reviewServer(t, control.Config{RunnerToken: runnerToken, RunnerID: "runner", RunnerWorkspaceIDs: []string{"default"}, RunnerScopes: []string{"runner:claim", "runner:control:read"}, DefaultProfileID: "worker", Profiles: profiles}, nil)
	reviewInitializeInstance(t, handler, bootstrapCode, deviceToken)
	created := reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "create", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"original title","goal":"original goal","source":"user"}`)})
	if created.Code != http.StatusOK {
		t.Fatalf("task creation=%d %s", created.Code, created.Body.String())
	}
	live := httptest.NewServer(handler)
	defer live.Close()
	client := live.Client()
	request := func(method, path, bearer string) (int, []byte, error) {
		// This is only a generous upper bound for a deadlock. The assertions below
		// verify the response and immutable snapshot; elapsed time is not part of
		// the behavior under test.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, method, live.URL+path, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		response, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil {
			return 0, nil, readErr
		}
		if closeErr != nil {
			return 0, nil, closeErr
		}
		return response.StatusCode, body, nil
	}
	claimedStatus, claimedBody, err := request(http.MethodPost, "/runner/v1/claims", runnerToken)
	if err != nil {
		t.Fatal(err)
	}
	if claimedStatus != http.StatusOK {
		t.Fatalf("claim status=%d", claimedStatus)
	}
	var claim contracts.JobClaim
	if err = json.Unmarshal(claimedBody, &claim); err != nil {
		t.Fatal(err)
	}
	if claim.IsolationPolicy != "opencode-workspace-write-v1" || claim.Harness != "opencode" || claim.TaskVersion != 2 {
		t.Fatalf("claim missing server profile or initial task snapshot: %+v", claim)
	}
	if _, err = store.DB().Exec(`UPDATE tasks SET title='changed',goal='changed',version=version+1 WHERE id=?`, claim.TaskID); err != nil {
		t.Fatal(err)
	}
	profiles["worker"] = control.RuntimeProfile{ID: "worker", RuntimeRouteID: "changed-route", ModelRouteID: "changed-model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1-changed", CapabilityIDs: []string{"admin"}, Scopes: []string{"*"}}
	recoveredStatus, recoveredBody, err := request(http.MethodGet, "/runner/v1/claims/current", runnerToken)
	if err != nil {
		t.Fatalf("current claim recovery blocked on its own query rows: %v", err)
	}
	if recoveredStatus != http.StatusOK {
		t.Fatalf("recovery status=%d", recoveredStatus)
	}
	var recovery contracts.ClaimRecovery
	if err = json.Unmarshal(recoveredBody, &recovery); err != nil {
		t.Fatal(err)
	}
	if len(recovery.Claims) != 1 || recovery.Claims[0].TaskVersion != claim.TaskVersion || recovery.Claims[0].Goal != "original goal" || len(recovery.Claims[0].Authorization.Scopes) != 1 || recovery.Claims[0].Authorization.Scopes[0] != "task:read" || recovery.Claims[0].RuntimeRouteID != "native" || recovery.Claims[0].IsolationPolicy != "opencode-workspace-write-v1" || recovery.Claims[0].Harness != "opencode" {
		t.Fatalf("recovery did not return immutable claim: %+v", recovery)
	}
}

func TestRuntimeProfileRejectsUnknownOrMismatchedHarness(t *testing.T) {
	store := openStore(t, t.TempDir()+"/profile.db")
	defer store.Close()
	for _, tc := range []struct {
		name    string
		profile control.RuntimeProfile
	}{
		{name: "unknown harness", profile: control.RuntimeProfile{ID: "worker", Harness: "aider", RuntimeRouteID: "runtime", ModelRouteID: "model", Role: "worker", IsolationPolicy: "policy"}},
		{name: "OpenCode mismatch", profile: control.RuntimeProfile{ID: "worker", Harness: "opencode", RuntimeRouteID: "runtime", ModelRouteID: "model", Role: "worker", IsolationPolicy: "other-policy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := control.NewServer(store, control.Config{Profiles: map[string]control.RuntimeProfile{"worker": tc.profile}})
			if err == nil {
				t.Fatal("invalid harness profile accepted")
			}
		})
	}
}

func TestQueuedHarnessSnapshotSurvivesConfigurationChange(t *testing.T) {
	bootstrapCode, deviceToken, runnerToken := reviewToken(t), reviewToken(t), reviewToken(t)
	profile := control.RuntimeProfile{ID: "worker", Harness: "opencode", RuntimeRouteID: "opencode", ModelRouteID: "model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1"}
	store, handler := reviewServer(t, control.Config{RunnerToken: runnerToken, RunnerID: "runner", RunnerWorkspaceIDs: []string{"default"}, RunnerScopes: []string{"runner:claim"}, DefaultProfileID: "worker", Profiles: map[string]control.RuntimeProfile{"worker": profile}}, nil)
	reviewInitializeInstance(t, handler, bootstrapCode, deviceToken)
	created := reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "create", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"original title","goal":"original goal","source":"user"}`)})
	if created.Code != http.StatusOK {
		t.Fatalf("task creation=%d %s", created.Code, created.Body.String())
	}
	if _, err := store.DB().Exec(`UPDATE tasks SET harness='third-party' WHERE source='user'`); err != nil {
		t.Fatal(err)
	}
	restarted, err := control.NewServer(store, control.Config{RunnerToken: runnerToken, RunnerID: "runner", RunnerWorkspaceIDs: []string{"default"}, RunnerScopes: []string{"runner:claim"}, DefaultProfileID: "worker", Profiles: map[string]control.RuntimeProfile{"worker": profile}})
	if err != nil {
		t.Fatal(err)
	}
	claimed := reviewRequest(restarted.Handler(), http.MethodPost, "/runner/v1/claims", runnerToken, "", nil)
	if claimed.Code != http.StatusNoContent {
		t.Fatalf("task with an unsupported stored harness was claimed: %d %s", claimed.Code, claimed.Body.String())
	}
	var status string
	if err = store.DB().QueryRow(`SELECT status FROM tasks WHERE source='user'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("mismatched queued task status=%q, want queued", status)
	}
}

func TestTerminationProofMethodIsBoundToRunHarness(t *testing.T) {
	for _, tc := range []struct{ name, harness, storedHarness, policy, wrongMethod string }{
		{name: "opencode rejects legacy proof", harness: "opencode", policy: "opencode-workspace-write-v1", wrongMethod: "native_process_exit_waited"},
		{name: "unknown persisted harness rejects empty proof", harness: "opencode", storedHarness: "third-party", policy: "opencode-workspace-write-v1", wrongMethod: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bootstrapCode, deviceToken, runnerToken := reviewToken(t), reviewToken(t), reviewToken(t)
			profile := control.RuntimeProfile{ID: "worker", Harness: tc.harness, RuntimeRouteID: tc.harness, ModelRouteID: "model", Role: "worker", IsolationPolicy: tc.policy}
			store, handler := reviewServer(t, control.Config{RunnerToken: runnerToken, RunnerID: "runner", RunnerWorkspaceIDs: []string{"default"}, RunnerScopes: []string{"runner:claim", "runner:observe"}, DefaultProfileID: "worker", Profiles: map[string]control.RuntimeProfile{"worker": profile}}, nil)
			reviewInitializeInstance(t, handler, bootstrapCode, deviceToken)
			created := reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "create", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"stop test","goal":"goal","source":"user"}`)})
			if created.Code != http.StatusOK {
				t.Fatalf("task creation=%d %s", created.Code, created.Body.String())
			}
			claimResponse := reviewRequest(handler, http.MethodPost, "/runner/v1/claims", runnerToken, "", nil)
			if claimResponse.Code != http.StatusOK {
				t.Fatalf("claim=%d %s", claimResponse.Code, claimResponse.Body.String())
			}
			var claim contracts.JobClaim
			if err := json.Unmarshal(claimResponse.Body.Bytes(), &claim); err != nil {
				t.Fatal(err)
			}
			storedHarness := tc.storedHarness
			if storedHarness == "" {
				storedHarness = tc.harness
			}
			if _, err := store.DB().Exec(`UPDATE runs SET desired_control='stop',harness=?,runtime_session_id='session-1' WHERE id=?`, storedHarness, claim.RunID); err != nil {
				t.Fatal(err)
			}
			observation := contracts.RunnerObservation{ObservationID: "wrong-proof", RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "stop_confirmed", RuntimeSessionID: "session-1", TerminationProof: &contracts.TerminationProof{Source: "runner_container_removal", Method: tc.wrongMethod, RuntimeSessionID: "session-1", RuntimeContainerID: "container-1", VerifiedAt: time.Now().UTC(), CleanupComplete: true}}
			response := reviewRequest(handler, http.MethodPost, "/runner/v1/runs/"+claim.RunID+"/observations", runnerToken, "wrong-proof", observation)
			if response.Code != http.StatusConflict {
				t.Fatalf("cross-harness proof accepted: %d %s", response.Code, response.Body.String())
			}
		})
	}
}
