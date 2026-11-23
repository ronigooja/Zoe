package capabilities

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

func capabilityRecoveryFixture(t *testing.T) (*storage.Store, http.Handler, string, contracts.DeploymentClaim) {
	t.Helper()
	ctx := context.Background()
	raw := make([]byte, 32)
	if _, e := rand.Read(raw); e != nil {
		t.Fatal(e)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	store, e := storage.Open(ctx, storage.Config{Path: filepath.Join(t.TempDir(), "authority.db")})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.Close() })
	if e = storage.RegisterCoreMigrations(store); e != nil {
		t.Fatal(e)
	}
	if e = control.RegisterInstanceMaintenanceMigration(store); e != nil {
		t.Fatal(e)
	}
	server, e := control.NewServer(store, control.Config{RunnerToken: token, RunnerID: "review", RunnerScopes: []string{"capability:deploy"}})
	if e != nil {
		t.Fatal(e)
	}
	if e = Register(server, store, Config{AvailableCapabilityIDs: []string{"mail.read"}, RuntimeBackend: runtimeBackendDocker}); e != nil {
		t.Fatal(e)
	}
	if e = store.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if err := server.RegisterRunnerCredential(ctx, token, "review", []string{"capability:deploy"}); err != nil {
		t.Fatal(err)
	}
	insertCapabilityTestDevice(t, store, "device", "owner")
	svc := &Service{db: store.DB(), server: server, availableCapabilityIDs: map[string]struct{}{"mail.read": {}}, runtimeBackend: runtimeBackendDocker}
	manifest := contracts.CapabilityManifest{SchemaVersion: 1, ArtifactRef: "oci://registry.example/app@sha256:" + strings.Repeat("a", 64), ArtifactDigest: "sha256:" + strings.Repeat("a", 64), Entrypoint: "/app/run", Capabilities: []string{"mail.read"}, Budget: contracts.CapabilityResourceBudget{CPUMillis: 50, MemoryBytes: 100, DiskBytes: 100, TimeoutSeconds: 10, Concurrency: 1}, Network: contracts.CapabilityNetworkPolicy{Mode: "none"}, HealthCheck: &contracts.CapabilityHealthCheck{Kind: "http", Target: "/health", Port: 8080, IntervalSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 2}}
	cmd := control.CommandContext{CommandID: "fixture", Principal: contracts.Principal{SubjectID: "owner", DeviceID: "device", Scopes: []string{"capability:release:submit", "capability:deployment:request", "capability:deployment:authorize"}}, AcceptedAt: time.Now().UTC()}
	tx := begin(t, store.DB())
	release, _, e := svc.submitRelease(ctx, tx, cmd, contracts.CommandRequest{Payload: marshal(submitReleasePayload{Manifest: manifest})})
	if e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	var digest string
	if e = store.DB().QueryRow("SELECT digest FROM capability_releases WHERE release_id=?", release.ResourceID).Scan(&digest); e != nil {
		t.Fatal(e)
	}
	tx = begin(t, store.DB())
	dep, _, e := svc.requestDeployment(ctx, tx, cmd, contracts.CommandRequest{Payload: marshal(deploymentRequestPayload{ServiceID: "review", ReleaseDigest: digest, Operation: "install"})})
	if e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	tx = begin(t, store.DB())
	version := int64(1)
	_, _, e = svc.authorizeDeployment(ctx, tx, cmd, contracts.CommandRequest{ExpectedVersion: &version, Payload: marshal(authorizePayload{DeploymentID: dep.ResourceID, ScopeKind: "instance", Actions: []string{"install", "stop"}, Budget: manifest.Budget, ExpiresAt: time.Now().Add(time.Hour)})})
	if e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	h := server.Handler()
	w := capabilityReviewHTTP(h, "POST", "/runner/v1/deployments/claims", token, "")
	if w.Code != 200 {
		t.Fatalf("claim status=%d body=%s", w.Code, w.Body.String())
	}
	var claim contracts.DeploymentClaim
	if e = json.Unmarshal(w.Body.Bytes(), &claim); e != nil {
		t.Fatal(e)
	}
	return store, h, token, claim
}
func capabilityReviewHTTP(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestReviewCapabilityRecoveryPreservesImmutableClaim(t *testing.T) {
	store, h, token, claim := capabilityRecoveryFixture(t)
	// Recovery must use the admitted snapshot even if mutable source records change.
	if _, e := store.DB().Exec("UPDATE capability_grants SET actions='[\"stop\"]',expires_at='2000-01-01T00:00:00Z'; UPDATE capability_releases SET manifest='{}'"); e != nil {
		t.Fatal(e)
	}
	for _, method := range []string{"GET", "POST"} {
		path := "/runner/v1/deployments/claims"
		if method == "GET" {
			path += "/current"
		}
		w := capabilityReviewHTTP(h, method, path, token, "")
		if w.Code != 200 {
			t.Fatalf("recover %s=%d", method, w.Code)
		}
		var got contracts.DeploymentClaim
		if e := json.Unmarshal(w.Body.Bytes(), &got); e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(got, claim) {
			t.Errorf("%s changed immutable claim", method)
		}
	}
}
func TestReviewCapabilityRecoveredUncertainClaimAcceptsReconciliation(t *testing.T) {
	_, h, token, claim := capabilityRecoveryFixture(t)
	obs := contracts.DeploymentObservation{DeploymentID: claim.DeploymentID, AttemptID: claim.AttemptID, Fence: claim.Fence, ObservationID: "unknown", Operation: "install", State: "uncertain", ObservedAt: time.Now().UTC()}
	body, _ := json.Marshal(obs)
	path := "/runner/v1/deployments/" + claim.DeploymentID + "/observations"
	if w := capabilityReviewHTTP(h, "POST", path, token, string(body)); w.Code != 202 {
		t.Fatalf("unknown observation=%d", w.Code)
	}
	w := capabilityReviewHTTP(h, "GET", "/runner/v1/deployments/claims/current", token, "")
	if w.Code != 200 {
		t.Fatalf("recovery=%d", w.Code)
	}
	var current contracts.DeploymentClaim
	if e := json.Unmarshal(w.Body.Bytes(), &current); e != nil {
		t.Fatal(e)
	}
	if current.ClaimState != "uncertain" {
		t.Fatalf("recovered state=%s", current.ClaimState)
	}
	obs.ObservationID = "reconciled"
	obs.State = "running"
	obs.Health = contracts.CapabilityHealth{State: "healthy", CheckedAt: time.Now().UTC(), EvidenceRef: "fixture:probe"}
	body, _ = json.Marshal(obs)
	if w := capabilityReviewHTTP(h, "POST", path, token, string(body)); w.Code != 202 {
		t.Errorf("same owned attempt cannot reconcile unknown result: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestReviewUncertainDeploymentsDoNotBlockIndependentClaimsOrCurrentRecovery(t *testing.T) {
	ctx := context.Background()
	store, h, token, first := capabilityRecoveryFixture(t)
	postUncertain := func(claim contracts.DeploymentClaim, observationID string) {
		t.Helper()
		observation := contracts.DeploymentObservation{DeploymentID: claim.DeploymentID, AttemptID: claim.AttemptID, Fence: claim.Fence, ObservationID: observationID, Operation: claim.DesiredOp, State: "uncertain", ObservedAt: time.Now().UTC()}
		body, _ := json.Marshal(observation)
		path := "/runner/v1/deployments/" + claim.DeploymentID + "/observations"
		if response := capabilityReviewHTTP(h, http.MethodPost, path, token, string(body)); response.Code != http.StatusAccepted {
			t.Fatalf("uncertain observation status=%d body=%s", response.Code, response.Body.String())
		}
	}
	postUncertain(first, "uncertain-first")

	cmd := control.CommandContext{CommandID: "second-deployment", Principal: contracts.Principal{SubjectID: "owner", DeviceID: "device", Scopes: []string{"capability:deployment:request", "capability:deployment:authorize"}}, AcceptedAt: time.Now().UTC()}
	tx := begin(t, store.DB())
	requested, _, err := (&Service{db: store.DB()}).requestDeployment(ctx, tx, cmd, contracts.CommandRequest{Payload: marshal(deploymentRequestPayload{ServiceID: "independent", ReleaseDigest: first.ReleaseDigest, Operation: "install"})})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	version := requested.ResourceVersion
	tx = begin(t, store.DB())
	_, _, err = (&Service{db: store.DB()}).authorizeDeployment(ctx, tx, cmd, contracts.CommandRequest{ExpectedVersion: &version, Payload: marshal(authorizePayload{DeploymentID: requested.ResourceID, ScopeKind: "instance", Actions: []string{"install"}, Budget: first.Manifest.Budget, ExpiresAt: time.Now().UTC().Add(time.Hour)})})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	claimResponse := capabilityReviewHTTP(h, http.MethodPost, "/runner/v1/deployments/claims", token, "")
	if claimResponse.Code != http.StatusOK {
		t.Fatalf("independent pending claim blocked by uncertain deployment: %d %s", claimResponse.Code, claimResponse.Body.String())
	}
	var second contracts.DeploymentClaim
	if err := json.Unmarshal(claimResponse.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.DeploymentID == first.DeploymentID {
		t.Fatalf("claim endpoint returned uncertain deployment %q instead of independent pending work", first.DeploymentID)
	}
	postUncertain(second, "uncertain-second")
	listResponse := capabilityReviewHTTP(h, http.MethodGet, "/runner/v1/deployments/claims/unresolved", token, "")
	if listResponse.Code != http.StatusOK {
		t.Fatalf("unresolved claim list=%d %s", listResponse.Code, listResponse.Body.String())
	}
	var unresolved []contracts.DeploymentClaim
	if err := json.Unmarshal(listResponse.Body.Bytes(), &unresolved); err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 2 {
		t.Fatalf("unresolved claim count=%d, want both claims", len(unresolved))
	}
	currentResponse := capabilityReviewHTTP(h, http.MethodGet, "/runner/v1/deployments/claims/current", token, "")
	if currentResponse.Code != http.StatusOK {
		t.Fatalf("multiple uncertain deployments returned %d: %s", currentResponse.Code, currentResponse.Body.String())
	}
	var current contracts.DeploymentClaim
	if err := json.Unmarshal(currentResponse.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.ClaimState != "uncertain" {
		t.Fatalf("current recovery state=%q", current.ClaimState)
	}
	claimResponse = capabilityReviewHTTP(h, http.MethodPost, "/runner/v1/deployments/claims", token, "")
	if claimResponse.Code != http.StatusNoContent {
		t.Fatalf("no pending work with multiple uncertain effects status=%d body=%s", claimResponse.Code, claimResponse.Body.String())
	}
}
