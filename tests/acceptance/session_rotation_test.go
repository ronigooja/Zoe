package acceptance

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/runner"
)

func TestSessionRotationRebindsRunAndRejectsOldTerminationSession(t *testing.T) {
	bootstrap, device, token := reviewToken(t), reviewToken(t), reviewToken(t)
	profile := control.RuntimeProfile{ID: "worker", Harness: "opencode", RuntimeRouteID: "runtime", ModelRouteID: "model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1"}
	var server *control.Server
	store, handler := reviewServer(t, control.Config{RunnerToken: token, RunnerID: "runner", RunnerWorkspaceIDs: []string{"default"}, RunnerScopes: []string{"runner:claim", "runner:observe"}, DefaultProfileID: "worker", Profiles: map[string]control.RuntimeProfile{"worker": profile}}, func(s *control.Server) { server = s })
	_ = server
	reviewInitializeInstance(t, handler, bootstrap, device)
	created := reviewRequest(handler, http.MethodPost, "/v1/commands", device, "create", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"rotation","goal":"rotation goal","source":"user"}`)})
	if created.Code != http.StatusOK {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	live := httptest.NewServer(handler)
	defer live.Close()
	client, err := runner.NewControlClient(live.URL, token, live.Client())
	if err != nil {
		t.Fatal(err)
	}
	claim, err := client.Claim(t.Context())
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	observe := func(o contracts.RunnerObservation) error { return client.Observe(t.Context(), claim.RunID, o) }
	started := contracts.RunnerObservation{ObservationID: "started", RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "dispatch_started"}
	if err := observe(started); err != nil {
		t.Fatal(err)
	}
	bound := contracts.RunnerObservation{ObservationID: "bound", RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "dispatch_bound", RuntimeSessionID: "session-a", ExternalEffects: []contracts.ExternalEffect{{ID: claim.RunID, Kind: "runtime_create", Status: "created", Reference: "session-a"}}}
	if err := observe(bound); err != nil {
		t.Fatal(err)
	}
	rotated := contracts.RunnerObservation{ObservationID: "rotated", RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "session_rotated", RuntimeSessionID: "session-b", Result: json.RawMessage(`{"old_session_id":"session-a","new_session_id":"session-b"}`)}
	if err := observe(rotated); err != nil {
		t.Fatal(err)
	}
	var session string
	if err := store.DB().QueryRow(`SELECT runtime_session_id FROM runs WHERE id=?`, claim.RunID).Scan(&session); err != nil {
		t.Fatal(err)
	}
	if session != "session-b" {
		t.Fatalf("bound session=%q, want session-b", session)
	}
	proof := &contracts.TerminationProof{Source: "runner_host_session", Method: "session_interrupted", RuntimeSessionID: "session-b", VerifiedAt: time.Now().UTC(), CleanupComplete: true}
	accepted := contracts.RunnerObservation{ObservationID: "new-term", RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "runtime_terminated", RuntimeSessionID: "session-b", TerminationProof: proof}
	old := accepted
	old.ObservationID = "old-term"
	old.RuntimeSessionID = "session-a"
	old.TerminationProof = &contracts.TerminationProof{Source: "runner_host_session", Method: "session_interrupted", RuntimeSessionID: "session-a", VerifiedAt: time.Now().UTC(), CleanupComplete: true}
	err = observe(old)
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "session_conflict") {
		t.Fatalf("old session result=%v", err)
	}
	if err := observe(accepted); err != nil {
		t.Fatalf("new session termination rejected: %v", err)
	}
}

func TestSessionRotationReplayWrongOldAndFenceAreRejected(t *testing.T) {
	bootstrap, device, token := reviewToken(t), reviewToken(t), reviewToken(t)
	profile := control.RuntimeProfile{ID: "worker", Harness: "opencode", RuntimeRouteID: "runtime", ModelRouteID: "model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1"}
	store, handler := reviewServer(t, control.Config{RunnerToken: token, RunnerID: "runner", RunnerWorkspaceIDs: []string{"default"}, RunnerScopes: []string{"runner:claim", "runner:observe"}, DefaultProfileID: "worker", Profiles: map[string]control.RuntimeProfile{"worker": profile}}, nil)
	reviewInitializeInstance(t, handler, bootstrap, device)
	if response := reviewRequest(handler, http.MethodPost, "/v1/commands", device, "create", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"rotation","goal":"rotation goal","source":"user"}`)}); response.Code != http.StatusOK {
		t.Fatalf("create=%d", response.Code)
	}
	live := httptest.NewServer(handler)
	defer live.Close()
	client, _ := runner.NewControlClient(live.URL, token, live.Client())
	claim, err := client.Claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	started := contracts.RunnerObservation{ObservationID: "started", RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "dispatch_started"}
	if err = client.Observe(t.Context(), claim.RunID, started); err != nil {
		t.Fatal(err)
	}
	bound := contracts.RunnerObservation{ObservationID: "bound", RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "dispatch_bound", RuntimeSessionID: "session-a", ExternalEffects: []contracts.ExternalEffect{{ID: claim.RunID, Kind: "runtime_create", Status: "created", Reference: "session-a"}}}
	if err = client.Observe(t.Context(), claim.RunID, bound); err != nil {
		t.Fatal(err)
	}
	wrong := contracts.RunnerObservation{ObservationID: "wrong", RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "session_rotated", RuntimeSessionID: "session-b", Result: json.RawMessage(`{"old_session_id":"not-a","new_session_id":"session-b"}`)}
	if err = client.Observe(t.Context(), claim.RunID, wrong); err == nil || !strings.Contains(err.Error(), "session_conflict") {
		t.Fatalf("wrong old result=%v", err)
	}
	var session string
	if err = store.DB().QueryRow(`SELECT runtime_session_id FROM runs WHERE id=?`, claim.RunID).Scan(&session); err != nil {
		t.Fatal(err)
	}
	if session != "session-a" {
		t.Fatalf("wrong old rebound to %q", session)
	}
	rotated := wrong
	rotated.ObservationID = "rotated"
	rotated.Result = json.RawMessage(`{"old_session_id":"session-a","new_session_id":"session-b"}`)
	if err = client.Observe(t.Context(), claim.RunID, rotated); err != nil {
		t.Fatal(err)
	}
	if err = client.Observe(t.Context(), claim.RunID, rotated); err != nil {
		t.Fatalf("replay rejected: %v", err)
	}
	var count int
	if err = store.DB().QueryRow(`SELECT count(*) FROM runner_observations WHERE run_id=? AND kind='session_rotated'`, claim.RunID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("rotation rows=%d, want one", count)
	}
	badFence := rotated
	badFence.ObservationID = "bad-fence"
	badFence.Fence++
	if err = client.Observe(t.Context(), claim.RunID, badFence); err == nil || !strings.Contains(err.Error(), "fence_conflict") {
		t.Fatalf("bad fence result=%v", err)
	}
	terminal := rotated
	terminal.ObservationID = "terminal"
	terminal.Kind = "runtime_terminated"
	terminal.TerminationProof = &contracts.TerminationProof{Source: "runner_host_session", Method: "session_interrupted", RuntimeSessionID: "session-b", VerifiedAt: time.Now().UTC(), CleanupComplete: true}
	if err = client.Observe(t.Context(), claim.RunID, terminal); err != nil {
		t.Fatal(err)
	}
	nonActive := rotated
	nonActive.ObservationID = "non-active"
	if err = client.Observe(t.Context(), claim.RunID, nonActive); err == nil || !strings.Contains(err.Error(), "state_conflict") {
		t.Fatalf("non-active result=%v", err)
	}
	wrongAttempt := rotated
	wrongAttempt.ObservationID = "wrong-attempt"
	wrongAttempt.AttemptID = "other-attempt"
	if err = client.Observe(t.Context(), claim.RunID, wrongAttempt); err == nil || !strings.Contains(err.Error(), "fence_conflict") {
		t.Fatalf("wrong attempt result=%v", err)
	}
}
