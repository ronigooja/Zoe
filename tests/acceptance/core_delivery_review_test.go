package acceptance

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

func reviewRuntimeConfig(code, runnerToken string) control.Config {
	return control.Config{RunnerToken: runnerToken, RunnerID: "review", RunnerScopes: []string{"runner:claim", "runner:control:read"}, DefaultProfileID: "review", Profiles: map[string]control.RuntimeProfile{"review": {ID: "review", RuntimeRouteID: "review", ModelRouteID: "review", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1"}}}
}

func TestReviewExplicitEmptyInstanceScopesRemainEmpty(t *testing.T) {
	code, token := reviewToken(t), reviewToken(t)
	_, h := reviewServer(t, control.Config{InstanceScopes: []string{}}, nil)
	reviewInitializeInstance(t, h, code, token)
	for _, path := range []string{"/v1/snapshot", "/v1/commands", "/v1/notifications"} {
		w := reviewRequest(h, "GET", path, token, "", nil)
		if w.Code != 403 {
			t.Fatalf("empty scopes admitted %s: %d", path, w.Code)
		}
	}
}

func TestReviewClaimAuthorizationSurvivesServerReconfiguration(t *testing.T) {
	code, token, rt := reviewToken(t), reviewToken(t), reviewToken(t)
	cfg := reviewRuntimeConfig(code, rt)
	p := cfg.Profiles["review"]
	p.Scopes = []string{"task:read"}
	p.CapabilityIDs = []string{"read"}
	cfg.Profiles["review"] = p
	store, h := reviewServer(t, cfg, nil)
	reviewInitializeInstance(t, h, code, token)
	w := reviewRequest(h, "POST", "/v1/commands", token, "create", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"task","goal":"goal","source":"user"}`)})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = reviewRequest(h, "POST", "/runner/v1/claims", rt, "", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var original contracts.JobClaim
	if err := json.Unmarshal(w.Body.Bytes(), &original); err != nil {
		t.Fatal(err)
	}
	cfg2 := reviewRuntimeConfig(code, rt)
	p = cfg2.Profiles["review"]
	p.Scopes = []string{"*"}
	p.CapabilityIDs = []string{"admin"}
	p.RuntimeRouteID = "changed"
	cfg2.Profiles["review"] = p
	server, err := control.NewServer(store, cfg2)
	if err != nil {
		t.Fatal(err)
	}
	w = reviewRequest(server.Handler(), "GET", "/runner/v1/claims/current", rt, "", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var recovery contracts.ClaimRecovery
	if err = json.Unmarshal(w.Body.Bytes(), &recovery); err != nil {
		t.Fatal(err)
	}
	if len(recovery.Claims) != 1 || !reflect.DeepEqual(original, recovery.Claims[0]) {
		t.Fatalf("admitted claim changed across server config: before=%#v after=%#v", original, recovery)
	}
}

func TestReviewAuthoritativeAccessRequiresActiveMembership(t *testing.T) {
	code, token := reviewToken(t), reviewToken(t)
	store, h := reviewServer(t, reviewRuntimeConfig(code, ""), nil)
	reviewInitializeInstance(t, h, code, token)
	w := reviewRequest(h, "POST", "/v1/commands", token, "project", contracts.CommandRequest{Kind: "project.create", Payload: json.RawMessage(`{"name":"private"}`)})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var project contracts.CommandReceipt
	json.Unmarshal(w.Body.Bytes(), &project)
	w = reviewRequest(h, "POST", "/v1/commands", token, "task", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"task","goal":"goal","source":"user","project_id":"` + project.ResourceID + `"}`)})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var task contracts.CommandReceipt
	json.Unmarshal(w.Body.Bytes(), &task)
	ctx := context.Background()
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	peer := contracts.Principal{SubjectID: "review-peer"}
	assert := func(want bool) {
		t.Helper()
		got, err := control.PrincipalCanAccessProject(ctx, tx, peer, project.ResourceID)
		if err != nil || got != want {
			t.Fatalf("project access=%v/%v want%v", got, err, want)
		}
		got, err = control.PrincipalCanAccessTask(ctx, tx, peer, task.ResourceID)
		if err != nil || got != want {
			t.Fatalf("task access=%v/%v want%v", got, err, want)
		}
	}
	assert(false)
	peer.SubjectID = "owner"
	assert(true)
}
