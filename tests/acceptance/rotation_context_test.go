package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/runner"
	"github.com/ronigooja/Zoe/internal/storage"
)

const rotationContextProfile = "rotation-context-worker"

func rotationContextConfig(token string, scopes []string) control.Config {
	return control.Config{
		RunnerToken: token, RunnerID: "runner", RunnerWorkspaceIDs: []string{"default"},
		RunnerScopes: scopes, DefaultProfileID: rotationContextProfile,
		Profiles: map[string]control.RuntimeProfile{rotationContextProfile: {
			ID: rotationContextProfile, Harness: "opencode", RuntimeRouteID: "runtime", ModelRouteID: "model",
			Role: "worker", IsolationPolicy: "opencode-workspace-write-v1",
		}},
	}
}

type rotationContextFixture struct {
	store  *storage.Store
	h      http.Handler
	server *httptest.Server
	client *runner.ControlClient
	claim  *contracts.JobClaim
	device string
	token  string
}

func newRotationContextFixture(t *testing.T, failingAssembler bool) *rotationContextFixture {
	t.Helper()
	device, token := reviewToken(t), reviewToken(t)
	var store *storage.Store
	var handler http.Handler
	store, handler = runnerContextServer(t, rotationContextConfig(token, []string{"runner:claim", "runner:observe", "runner:control:read"}))
	reviewInitializeInstance(t, handler, reviewToken(t), device)
	created := reviewRequest(handler, http.MethodPost, "/v1/commands", device, "rotation-task", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"rotation context","goal":"context goal","source":"user","initial_message":"first"}`)})
	if created.Code != http.StatusOK {
		t.Fatalf("task create=%d %s", created.Code, created.Body.String())
	}
	var taskReceipt contracts.CommandReceipt
	if err := json.Unmarshal(created.Body.Bytes(), &taskReceipt); err != nil {
		t.Fatal(err)
	}
	for i, content := range []string{"second", "third"} {
		appended := reviewRequest(handler, http.MethodPost, "/v1/commands", device, fmt.Sprintf("rotation-append-%d", i), contracts.CommandRequest{Kind: "task.append", Payload: mustJSON(t, map[string]any{"task_id": taskReceipt.ResourceID, "content": content})})
		if appended.Code != http.StatusOK {
			t.Fatalf("task append=%d %s", appended.Code, appended.Body.String())
		}
	}
	live := httptest.NewServer(handler)
	t.Cleanup(live.Close)
	client, err := runner.NewControlClient(live.URL, token, live.Client())
	if err != nil {
		t.Fatal(err)
	}
	claim, err := client.Claim(t.Context())
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	return &rotationContextFixture{store: store, h: handler, server: live, client: client, claim: claim, device: device, token: token}
}

func TestRotationContextRejectsWrongRunnerFenceAttemptScopeAndTerminal(t *testing.T) {
	t.Run("wrong fence", func(t *testing.T) {
		f := newRotationContextFixture(t, false)
		assertRotationContextError(t, f.h, f.claim.RunID, f.claim.AttemptID, f.claim.Fence+1, f.token, http.StatusConflict, "fence_conflict")
	})
	t.Run("wrong attempt", func(t *testing.T) {
		f := newRotationContextFixture(t, false)
		assertRotationContextError(t, f.h, f.claim.RunID, "wrong-attempt", f.claim.Fence, f.token, http.StatusConflict, "fence_conflict")
	})
	t.Run("different runner", func(t *testing.T) {
		f := newRotationContextFixture(t, false)
		other := reviewToken(t)
		if err := control.RegisterRunnerCredentialIfMissing(t.Context(), f.store, other, "other-runner", []string{"default"}, []string{"runner:control:read"}); err != nil {
			t.Fatal(err)
		}
		assertRotationContextError(t, f.h, f.claim.RunID, f.claim.AttemptID, f.claim.Fence, other, http.StatusConflict, "fence_conflict")
	})
	t.Run("scope", func(t *testing.T) {
		f := newRotationContextFixture(t, false)
		other := reviewToken(t)
		if err := control.RegisterRunnerCredentialIfMissing(t.Context(), f.store, other, "runner-no-read", []string{"default"}, []string{"runner:observe"}); err != nil {
			t.Fatal(err)
		}
		assertRotationContextError(t, f.h, f.claim.RunID, f.claim.AttemptID, f.claim.Fence, other, http.StatusForbidden, "forbidden")
	})
	t.Run("missing parameters", func(t *testing.T) {
		f := newRotationContextFixture(t, false)
		assertRotationContextError(t, f.h, f.claim.RunID, "", 0, f.token, http.StatusBadRequest, "invalid_request")
	})
	t.Run("terminal", func(t *testing.T) {
		f := newRotationContextFixture(t, false)
		if err := f.client.Observe(t.Context(), f.claim.RunID, boundObservation(*f.claim, "terminal-bound", "terminal-session")); err != nil {
			t.Fatal(err)
		}
		if err := f.client.Observe(t.Context(), f.claim.RunID, contracts.RunnerObservation{ObservationID: "terminal-failure", RunID: f.claim.RunID, AttemptID: f.claim.AttemptID, Fence: f.claim.Fence, Kind: "failure", RuntimeSessionID: "terminal-session", Error: "terminal test failure"}); err != nil {
			t.Fatal(err)
		}
		if err := f.client.Observe(t.Context(), f.claim.RunID, terminationObservation(*f.claim, "terminal-proof", "terminal-session")); err != nil {
			t.Fatal(err)
		}
		assertRotationContextError(t, f.h, f.claim.RunID, f.claim.AttemptID, f.claim.Fence, f.token, http.StatusNotFound, "not_found")
	})
}

func assertRotationContextError(t *testing.T, h http.Handler, runID, attempt string, fence uint64, token string, status int, code string) {
	t.Helper()
	path := "/runner/v1/runs/" + runID + "/rotation-context"
	if attempt != "" {
		path += "?attempt_id=" + attempt
		if fence != 0 {
			path += fmt.Sprintf("&fence=%d", fence)
		}
	}
	w := reviewRequest(h, http.MethodGet, path, token, "", nil)
	if w.Code != status {
		t.Fatalf("status=%d body=%s, want %d", w.Code, w.Body.String(), status)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != code {
		t.Fatalf("error code=%q body=%s, want %q", body.Error.Code, w.Body.String(), code)
	}
}

func TestRotationContextInstructionCapKeepsNewestWithMarker(t *testing.T) {
	f := newRotationContextFixture(t, false)
	for i := 0; i < 3; i++ {
		content := fmt.Sprintf("old-%d-%s", i, strings.Repeat("x", 5000))
		w := reviewRequest(f.h, http.MethodPost, "/v1/commands", f.device, fmt.Sprintf("cap-%d", i), contracts.CommandRequest{Kind: "task.append", Payload: mustJSON(t, map[string]any{"task_id": taskIDForRotation(t, f), "content": content})})
		if w.Code != http.StatusOK {
			t.Fatalf("cap append=%d %s", w.Code, w.Body.String())
		}
	}
	got, err := f.client.RotationContext(t.Context(), f.claim.RunID, f.claim.AttemptID, f.claim.Fence)
	if err != nil {
		t.Fatal(err)
	}
	if got.Instructions[0] != "[Earlier user instructions omitted: rotation context capped; newest instructions retained.]" {
		t.Fatalf("marker=%q", got.Instructions[0])
	}
	if strings.Contains(strings.Join(got.Instructions, "\n"), "old-0-") || !strings.Contains(got.Instructions[len(got.Instructions)-1], "old-2-") {
		t.Fatalf("instructions did not retain newest entries: %v", got.Instructions)
	}
}

func taskIDForRotation(t *testing.T, f *rotationContextFixture) string {
	t.Helper()
	var taskID string
	if err := f.store.DB().QueryRow(`SELECT task_id FROM runs WHERE id=?`, f.claim.RunID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	return taskID
}
