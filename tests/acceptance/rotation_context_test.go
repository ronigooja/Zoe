package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memory"
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
	if failingAssembler {
		store, handler = memoryRunnerServerWithAssembler(t, rotationContextConfig(token, []string{"runner:claim", "runner:observe", "runner:control:read"}), func(context.Context, contracts.Principal, string, string, string, int) (contracts.MemoryContext, error) {
			return contracts.MemoryContext{}, errors.New("injected assembler failure")
		})
	} else {
		store, handler = memoryRunnerServer(t, rotationContextConfig(token, []string{"runner:claim", "runner:observe", "runner:control:read"}))
	}
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

func rememberRotationMemory(t *testing.T, f *rotationContextFixture, content string) (string, int64) {
	t.Helper()
	w := reviewRequest(f.h, http.MethodPost, "/v1/commands", f.device, "rotation-memory", contracts.CommandRequest{Kind: "memory.remember", Payload: mustJSON(t, map[string]any{"scope": map[string]string{"kind": "personal"}, "subject": "rotation", "key": "context", "content": content, "source_kind": "user_note"})})
	if w.Code != http.StatusOK {
		t.Fatalf("memory remember=%d %s", w.Code, w.Body.String())
	}
	var receipt contracts.CommandReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt.ResourceID, receipt.ResourceVersion
}

func TestRotationContextReturnsOrderedInstructionsAndMemory(t *testing.T) {
	f := newRotationContextFixture(t, false)
	rememberRotationMemory(t, f, "remembered rotation context")
	got, err := f.client.RotationContext(t.Context(), f.claim.RunID, f.claim.AttemptID, f.claim.Fence)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Instructions, []string{"first", "second", "third"}) {
		t.Fatalf("instructions=%v", got.Instructions)
	}
	if len(got.Memory.Items) != 1 || got.Memory.Items[0].Content != "remembered rotation context" {
		t.Fatalf("memory=%+v", got.Memory)
	}
	if got.Memory.Manifest.ID == "" || len(got.Memory.Manifest.Entries) != 1 {
		t.Fatalf("manifest=%+v", got.Memory.Manifest)
	}
}

func TestRotationContextOmitsDeletedMemory(t *testing.T) {
	f := newRotationContextFixture(t, false)
	memoryID, revision := rememberRotationMemory(t, f, "delete this rotation memory")
	deleted := reviewRequest(f.h, http.MethodPost, "/v1/commands", f.device, "rotation-memory-delete", contracts.CommandRequest{Kind: "memory.delete", Payload: mustJSON(t, map[string]any{"memory_id": memoryID, "expected_revision": revision, "reason": "user_requested"})})
	if deleted.Code != http.StatusOK {
		t.Fatalf("memory delete=%d %s", deleted.Code, deleted.Body.String())
	}
	got, err := f.client.RotationContext(t.Context(), f.claim.RunID, f.claim.AttemptID, f.claim.Fence)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range got.Memory.Items {
		if item.Content == "delete this rotation memory" {
			t.Fatalf("deleted memory returned: %+v", got.Memory.Items)
		}
	}
}

func TestRotationContextMemoryFailureReturnsUnavailableNotStale(t *testing.T) {
	f := newRotationContextFixture(t, true)
	got, err := f.client.RotationContext(t.Context(), f.claim.RunID, f.claim.AttemptID, f.claim.Fence)
	if err != nil {
		t.Fatal(err)
	}
	if !got.MemoryUnavailable || len(got.Memory.Items) != 0 || !reflect.DeepEqual(got.Instructions, []string{"first", "second", "third"}) {
		t.Fatalf("rotation context=%+v", got)
	}
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
		if err := f.client.Observe(t.Context(), f.claim.RunID, contracts.RunnerObservation{ObservationID: "terminal-failure", RunID: f.claim.RunID, AttemptID: f.claim.AttemptID, Fence: f.claim.Fence, Kind: "failure", Error: "terminal test failure"}); err != nil {
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

func memoryRunnerServerWithAssembler(t *testing.T, cfg control.Config, assembler control.ContextAssembler) (*storage.Store, http.Handler) {
	t.Helper()
	store := openStore(t, filepath.Join(t.TempDir(), "memory-runner.db"))
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	srv, err := control.NewServer(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.Register(srv, store); err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterContextAssembler(assembler); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterRunnerCredential(context.Background(), cfg.RunnerToken, cfg.RunnerID, cfg.RunnerWorkspaceIDs, cfg.RunnerScopes); err != nil {
		t.Fatal(err)
	}
	return store, bindReviewHandler(srv.Handler(), store)
}
