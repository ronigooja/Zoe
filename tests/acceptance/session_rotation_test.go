package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/runner"
	"github.com/ronigooja/Zoe/internal/storage"
)

type rotationRecoveryFixture struct {
	store   *storage.Store
	server  *httptest.Server
	client  *runner.ControlClient
	claim   contracts.JobClaim
	journal *runner.Journal
}

func newRotationRecoveryFixture(t *testing.T, normal bool, firstOld string) *rotationRecoveryFixture {
	t.Helper()
	token := reviewToken(t)
	profile := control.RuntimeProfile{ID: "worker", Harness: "opencode", RuntimeRouteID: "runtime", ModelRouteID: "model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1"}
	store, handler := reviewServer(t, control.Config{RunnerToken: token, RunnerID: "runner", RunnerWorkspaceIDs: []string{"default"}, RunnerScopes: []string{"runner:claim", "runner:observe"}, DefaultProfileID: "worker", Profiles: map[string]control.RuntimeProfile{"worker": profile}}, nil)
	live := httptest.NewServer(handler)
	t.Cleanup(live.Close)
	client, err := runner.NewControlClient(live.URL, token, live.Client())
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, device := reviewToken(t), reviewToken(t)
	reviewInitializeInstance(t, handler, bootstrap, device)
	if response := reviewRequest(handler, http.MethodPost, "/v1/commands", device, "create", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"rotation recovery","goal":"rotation recovery goal","source":"user"}`)}); response.Code != http.StatusOK {
		t.Fatalf("create=%d %s", response.Code, response.Body.String())
	}
	claim, err := client.Claim(t.Context())
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	observe := func(o contracts.RunnerObservation) {
		t.Helper()
		if err := client.Observe(t.Context(), claim.RunID, o); err != nil {
			t.Fatal(err)
		}
	}
	observe(contracts.RunnerObservation{ObservationID: "server-started", RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "dispatch_started"})
	observe(boundObservation(*claim, "server-bound", "A"))
	journal, err := runner.OpenJournal(context.Background(), filepath.Join(t.TempDir(), "runner.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	spec := runner.RuntimeSpec{RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: "1", Workspace: claim.WorkspaceID}
	if _, err := journal.SaveClaim(t.Context(), *claim, spec, "no_effect_terminal"); err != nil {
		t.Fatal(err)
	}
	if err := journal.SetSession(t.Context(), claim.RunID, claim.AttemptID, claim.Fence, "A", "bound"); err != nil {
		t.Fatal(err)
	}
	queue := func(o contracts.RunnerObservation) {
		t.Helper()
		if _, err := journal.QueueObservation(t.Context(), claim.RunID, o); err != nil {
			t.Fatal(err)
		}
	}
	queue(contracts.RunnerObservation{ObservationID: "server-started", RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "dispatch_started"})
	queue(boundObservation(*claim, "server-bound", "A"))
	for i, session := range []string{"S1", "S2", "S3"} {
		result := mustRotationJSON(i + 1)
		if i == 0 && firstOld != "" {
			result = json.RawMessage(`{"event":"context_rotated","rotation":1,"old_session_id":"` + firstOld + `","new_session_id":"S1"}`)
		}
		queue(contracts.RunnerObservation{ObservationID: "progress-" + session, RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "progress", RuntimeSessionID: session, Result: result})
	}
	queue(terminationObservation(*claim, "termination", "S3"))
	queue(contracts.RunnerObservation{ObservationID: "failure", RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "failure", RuntimeSessionID: "S3", Error: "runtime failed"})
	if normal {
		for i, pair := range [][2]string{{"A", "S1"}, {"S1", "S2"}, {"S2", "S3"}} {
			raw := json.RawMessage(`{"event":"context_rotated","rotation":1,"old_session_id":"` + pair[0] + `","new_session_id":"` + pair[1] + `"}`)
			observe(contracts.RunnerObservation{ObservationID: "normal-" + pair[0], RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "session_rotated", RuntimeSessionID: pair[1], Result: raw})
			if _, err := journal.QueueObservation(t.Context(), claim.RunID, contracts.RunnerObservation{ObservationID: "normal-" + pair[0], RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "session_rotated", RuntimeSessionID: pair[1], Result: raw}); err != nil {
				t.Fatal(err)
			}
			_ = i
		}
	}
	return &rotationRecoveryFixture{store: store, server: live, client: client, claim: *claim, journal: journal}
}

func boundObservation(claim contracts.JobClaim, id, session string) contracts.RunnerObservation {
	return contracts.RunnerObservation{ObservationID: id, RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "dispatch_bound", RuntimeSessionID: session, ExternalEffects: []contracts.ExternalEffect{{ID: claim.RunID, Kind: "runtime_create", Status: "created", Reference: session}}}
}

func mustRotationJSON(rotation int) json.RawMessage {
	return json.RawMessage(`{"event":"context_rotated","rotation":` + fmt.Sprint(rotation) + `}`)
}

func terminationObservation(claim contracts.JobClaim, id, session string) contracts.RunnerObservation {
	return contracts.RunnerObservation{ObservationID: id, RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Kind: "runtime_terminated", RuntimeSessionID: session, TerminationProof: &contracts.TerminationProof{Source: "runner_host_session", Method: "session_interrupted", RuntimeSessionID: session, VerifiedAt: time.Now().UTC(), CleanupComplete: true}}
}

func recoveryEngine(f *rotationRecoveryFixture) *runner.Engine {
	return &runner.Engine{Control: f.client, Journal: f.journal}
}

func journalPendingCount(t *testing.T, f *rotationRecoveryFixture) int {
	t.Helper()
	pending, err := f.journal.PendingObservations(t.Context(), f.claim.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return len(pending)
}

func serverSession(t *testing.T, f *rotationRecoveryFixture) string {
	t.Helper()
	var session string
	if err := f.store.DB().QueryRow(`SELECT runtime_session_id FROM runs WHERE id=?`, f.claim.RunID).Scan(&session); err != nil {
		t.Fatal(err)
	}
	return session
}

func serverRotationPairs(t *testing.T, f *rotationRecoveryFixture) []string {
	t.Helper()
	rows, err := f.store.DB().Query(`SELECT body FROM runner_observations WHERE run_id=? AND kind='session_rotated' ORDER BY occurred_at, observation_id`, f.claim.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var pairs []string
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var body struct {
			Old string `json:"old_session_id"`
			New string `json:"new_session_id"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		pairs = append(pairs, body.Old+"->"+body.New)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return pairs
}

func TestLegacyRotationRecoveryRebindsJournalChain(t *testing.T) {
	f := newRotationRecoveryFixture(t, false, "")
	if err := recoveryEngine(f).FlushPendingObservations(t.Context(), f.claim.RunID); err != nil {
		t.Fatal(err)
	}
	if got := serverSession(t, f); got != "S3" {
		t.Fatalf("server session=%q, want S3", got)
	}
	if got := serverRotationPairs(t, f); !reflect.DeepEqual(got, []string{"A->S1", "S1->S2", "S2->S3"}) {
		t.Fatalf("rotation pairs=%v", got)
	}
	if got := journalPendingCount(t, f); got != 0 {
		t.Fatalf("journal pending=%d, want 0", got)
	}
}

func TestAlreadyRotatedNormallyRecoveryEmitsNoDuplicates(t *testing.T) {
	f := newRotationRecoveryFixture(t, true, "")
	if err := recoveryEngine(f).FlushPendingObservations(t.Context(), f.claim.RunID); err != nil {
		t.Fatal(err)
	}
	if got := serverRotationPairs(t, f); !reflect.DeepEqual(got, []string{"A->S1", "S1->S2", "S2->S3"}) {
		t.Fatalf("rotation pairs=%v, want one each", got)
	}
	if got := journalPendingCount(t, f); got != 0 {
		t.Fatalf("journal pending=%d, want 0", got)
	}
}

func TestBrokenRotationChainLeavesBindingAndTerminationPending(t *testing.T) {
	f := newRotationRecoveryFixture(t, false, "wrong")
	if err := recoveryEngine(f).FlushPendingObservations(t.Context(), f.claim.RunID); err == nil || !strings.Contains(err.Error(), "session_conflict") {
		t.Fatalf("flush error=%v, want session_conflict", err)
	}
	if got := serverSession(t, f); got != "A" {
		t.Fatalf("server session=%q, want A", got)
	}
	pending, err := f.journal.PendingObservations(t.Context(), f.claim.RunID)
	if err != nil {
		t.Fatal(err)
	}
	foundTermination := false
	for _, q := range pending {
		foundTermination = foundTermination || q.Body.Kind == "runtime_terminated"
	}
	if !foundTermination {
		t.Fatal("termination was acknowledged after broken chain rejection")
	}
}

func TestRotationRecoveryReplayIsIdempotent(t *testing.T) {
	f := newRotationRecoveryFixture(t, false, "")
	if err := recoveryEngine(f).FlushPendingObservations(t.Context(), f.claim.RunID); err != nil {
		t.Fatal(err)
	}
	first := serverRotationPairs(t, f)
	if err := recoveryEngine(f).FlushPendingObservations(t.Context(), f.claim.RunID); err != nil {
		t.Fatal(err)
	}
	if got := serverRotationPairs(t, f); !reflect.DeepEqual(got, first) {
		t.Fatalf("second recovery pairs=%v, first=%v", got, first)
	}
}

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
