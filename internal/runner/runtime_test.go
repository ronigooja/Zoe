package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/agents/opencode"
)

func TestOpenCodeRuntimeCreateSendsClaimAgentModelAndPermissions(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["agent"] != "zoe-worker" {
			t.Fatalf("agent=%v", body["agent"])
		}
		model := body["model"].(map[string]any)
		if model["id"] != "model" || model["providerID"] != "provider" {
			t.Fatalf("model=%v", model)
		}
		permissions := body["permissions"].([]any)
		if len(permissions) != 2 || permissions[0].(map[string]any)["effect"] != "deny" {
			t.Fatalf("permissions=%v", permissions)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": opencode.Session{ID: "session"}})
	}))
	defer s.Close()
	r := OpenCodeRuntime{BaseURL: s.URL, Password: "password"}
	_, err := r.Create(context.Background(), RuntimeSpec{RunID: "run", Agent: "zoe-worker", Model: "provider/model", Permissions: []contracts.PermissionRule{{Action: "*", Resource: "*", Effect: "deny"}, {Action: "read", Resource: "*", Effect: "allow"}}, Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
}
