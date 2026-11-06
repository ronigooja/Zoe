package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ronigooja/Zoe/internal/agents/opencode"
	"github.com/ronigooja/Zoe/internal/profiles"
)

func TestFullTierClaimRequiresResidentWorker(t *testing.T) {
	c := testClaim()
	c.ProfileID, c.Agent, c.Role, c.PermissionTier = "zoe-steward", "zoe-steward", "worker", "full"
	p, _ := profiles.ByID("zoe-steward")
	var err error
	c.PermissionRules, _, err = profiles.EffectivePermissionSet(p, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateClaim(c); err != nil {
		t.Fatalf("resident Worker rejected: %v", err)
	}
	c.ProfileID = "zoe-super"
	if err := validateClaim(c); err == nil {
		t.Fatal("unauthorized profile accepted full-tier claim")
	}
	c.ProfileID, c.Agent = "zoe-steward", "zoe-super"
	if err := validateClaim(c); err == nil {
		t.Fatal("mismatched agent accepted full-tier claim")
	}
}

func TestResidentWorkerSessionPermissionsSnapshot(t *testing.T) {
	p, _ := profiles.ByID("zoe-steward")
	rules, _, err := profiles.EffectivePermissionSet(p, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Agent       string              `json:"agent"`
			Permissions []map[string]string `json:"permissions"`
			Location    map[string]string   `json:"location"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		const snapshot = `[{"action":"*","resource":"*","effect":"deny"},{"action":"edit","resource":"*","effect":"allow"},{"action":"shell","resource":"*","effect":"allow"},{"action":"bash","resource":"*","effect":"allow"},{"action":"webfetch","resource":"*","effect":"allow"},{"action":"external_directory","resource":"*","effect":"allow"},{"action":"mcp*","resource":"*","effect":"allow"},{"action":"*","resource":"*","effect":"allow"}]`
		var want []map[string]string
		if err := json.Unmarshal([]byte(snapshot), &want); err != nil {
			t.Error(err)
		}
		if body.Agent != "zoe-steward" || !reflect.DeepEqual(body.Permissions, want) || body.Location["directory"] != "/workspace" {
			t.Errorf("Worker session snapshot mismatch: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": opencode.Session{ID: "session"}})
	}))
	defer s.Close()
	runtime := OpenCodeRuntime{BaseURL: s.URL, Password: "password"}
	if _, err := runtime.Create(context.Background(), RuntimeSpec{RunID: "run", Agent: p.ID, Model: "provider/model", Permissions: rules, Workspace: "/workspace"}); err != nil {
		t.Fatal(err)
	}
}
