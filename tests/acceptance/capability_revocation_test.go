package acceptance

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

func TestCapabilityRevocationFencesClaimsAndRequiresProvenCleanup(t *testing.T) {
	store := openStore(t, ":memory:")
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	bootstrap, deviceToken, runnerToken := reviewToken(t), reviewToken(t), reviewToken(t)
	server, err := control.NewServer(store, control.Config{
		InstanceScopes: []string{"client:read", "client:command", "capability:read", "capability:release:submit", "capability:deployment:request", "capability:deployment:authorize", "capability:deployment:revoke"},
		RunnerToken:    runnerToken, RunnerID: "revocation-runner", RunnerScopes: []string{"capability:deploy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = capabilities.Register(server, store, capabilities.Config{AvailableCapabilityIDs: []string{"mail.read"}, RuntimeBackend: "docker"}); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterRunnerCredentialIfMissing(t.Context(), store, runnerToken, "revocation-runner", []string{"capability:deploy"}); err != nil {
		t.Fatal(err)
	}
	handler := bindReviewHandler(server.Handler(), store)
	reviewInitializeInstance(t, handler, bootstrap, deviceToken)

	digest := "sha256:" + strings.Repeat("b", 64)
	manifest := contracts.CapabilityManifest{
		SchemaVersion: 1, ArtifactRef: "oci://registry.example/service@" + digest, ArtifactDigest: digest,
		Entrypoint: "/service", Capabilities: []string{"mail.read"},
		Budget:      contracts.CapabilityResourceBudget{CPUMillis: 100, MemoryBytes: 1 << 20, DiskBytes: 1 << 20, TimeoutSeconds: 60, Concurrency: 1},
		Network:     contracts.CapabilityNetworkPolicy{Mode: "none"},
		HealthCheck: &contracts.CapabilityHealthCheck{Kind: "http", Target: "/health", Port: 8080, IntervalSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 2},
		Triggers:    []contracts.CapabilityTrigger{{Kind: "schedule", Source: "schedule.interval", Mode: "program", IntervalSeconds: 60, MissedRunPolicy: "skip"}},
	}
	var release contracts.CommandReceipt
	releaseResponse := reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "rev-release", contracts.CommandRequest{Kind: "capability.release.submit", Payload: mustJSON(t, map[string]any{"manifest": manifest})})
	if releaseResponse.Code != http.StatusOK || json.Unmarshal(releaseResponse.Body.Bytes(), &release) != nil {
		t.Fatalf("release submit=%d %s", releaseResponse.Code, releaseResponse.Body.String())
	}
	var page struct {
		Items []contracts.CapabilityRelease `json:"items"`
	}
	releases := reviewRequest(handler, http.MethodGet, "/v1/capabilities/releases", deviceToken, "", nil)
	if releases.Code != http.StatusOK || json.Unmarshal(releases.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
		t.Fatalf("release list=%d %s", releases.Code, releases.Body.String())
	}
	releaseDigest := page.Items[0].Digest

	request := contracts.CommandRequest{Kind: "capability.deployment.request", Payload: mustJSON(t, map[string]any{"service_id": "revocable", "release_digest": releaseDigest, "operation": "start"})}
	requested := reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "rev-request", request)
	var receipt contracts.CommandReceipt
	if requested.Code != http.StatusOK || json.Unmarshal(requested.Body.Bytes(), &receipt) != nil {
		t.Fatalf("deployment request=%d %s", requested.Code, requested.Body.String())
	}
	deploymentID, version := receipt.ResourceID, receipt.ResourceVersion
	budget := manifest.Budget
	// Reproduce a previously stopped deployment whose new start has been
	// claimed but not yet executed when the owner revokes the grant.
	if _, err = store.DB().ExecContext(t.Context(), `UPDATE capability_deployments SET observed_state='stopped' WHERE deployment_id=?`, deploymentID); err != nil {
		t.Fatal(err)
	}
	authorized := contracts.CommandRequest{Kind: "capability.deployment.authorize", ExpectedVersion: &version, Payload: mustJSON(t, map[string]any{"deployment_id": deploymentID, "scope_kind": "workspace", "actions": []string{"start"}, "budget": budget, "expires_at": time.Now().UTC().Add(time.Hour)})}
	authorizeResponse := reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "rev-authorize", authorized)
	if authorizeResponse.Code != http.StatusOK || json.Unmarshal(authorizeResponse.Body.Bytes(), &receipt) != nil {
		t.Fatalf("deployment authorize=%d %s", authorizeResponse.Code, authorizeResponse.Body.String())
	}
	version = receipt.ResourceVersion
	var oldGrant string
	if err = store.DB().QueryRowContext(t.Context(), `SELECT grant_id FROM capability_deployments WHERE deployment_id=?`, deploymentID).Scan(&oldGrant); err != nil {
		t.Fatal(err)
	}
	var triggerID, triggerKey string
	if err = store.DB().QueryRowContext(t.Context(), `SELECT trigger_id,trigger_key FROM capability_trigger_definitions WHERE deployment_id=?`, deploymentID).Scan(&triggerID, &triggerKey); err != nil {
		t.Fatal(err)
	}
	triggerAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = store.DB().ExecContext(t.Context(), `INSERT INTO capability_trigger_runs(trigger_run_id,trigger_id,workspace_id,deployment_id,trigger_key,dedupe_key,event_payload,mode,state,created_at,updated_at) VALUES('queued-before-revoke',?,?,?,?,'dedupe-before-revoke','{}','program','queued',?,?)`, triggerID, "default", deploymentID, triggerKey, triggerAt, triggerAt); err != nil {
		t.Fatal(err)
	}
	ownerRevoke := contracts.CommandRequest{Kind: "capability.deployment.revoke", ExpectedVersion: &version, Payload: mustJSON(t, map[string]any{"deployment_id": deploymentID})}
	_, err = server.ExecuteCommand(t.Context(), contracts.Principal{WorkspaceID: "default", SubjectID: "different-owner", DeviceID: "other-device", Scopes: []string{"client:command", "capability:deployment:revoke"}}, "rev-not-owner", ownerRevoke, []byte("owner-negative"))
	if apiErr, ok := err.(*control.APIError); !ok || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("non-owner revocation error=%v, want unauthenticated owner session", err)
	}
	_, err = server.ExecuteCommand(t.Context(), contracts.Principal{WorkspaceID: "default", SubjectID: "different-subject", DeviceID: "other-device", Scopes: []string{"client:command"}}, "rev-no-scope", ownerRevoke, []byte("scope-negative"))
	if apiErr, ok := err.(*control.APIError); !ok || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("revocation without scope error=%v, want unauthenticated owner session", err)
	}
	claimResponse := reviewRequest(handler, http.MethodPost, "/runner/v1/deployments/claims", runnerToken, "", nil)
	var active contracts.DeploymentClaim
	if claimResponse.Code != http.StatusOK || json.Unmarshal(claimResponse.Body.Bytes(), &active) != nil || active.DesiredOp != "start" {
		t.Fatalf("install claim=%d %s", claimResponse.Code, claimResponse.Body.String())
	}
	// Make the durable grant expired before revocation. Cleanup remains allowed
	// only for the exact deployment stop claim; it receives no renewed grant.
	if _, err = store.DB().ExecContext(t.Context(), `UPDATE capability_grants SET expires_at=? WHERE grant_id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), oldGrant); err != nil {
		t.Fatal(err)
	}

	version = receipt.ResourceVersion
	revoke := contracts.CommandRequest{Kind: "capability.deployment.revoke", ExpectedVersion: &version, Payload: mustJSON(t, map[string]any{"deployment_id": deploymentID})}
	revoked := reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "rev-revoke", revoke)
	if revoked.Code != http.StatusOK || json.Unmarshal(revoked.Body.Bytes(), &receipt) != nil {
		t.Fatalf("revoke=%d %s", revoked.Code, revoked.Body.String())
	}
	retryRevoke := reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "rev-revoke", revoke)
	var retriedReceipt contracts.CommandReceipt
	if retryRevoke.Code != http.StatusOK || json.Unmarshal(retryRevoke.Body.Bytes(), &retriedReceipt) != nil || retriedReceipt.CommandID != receipt.CommandID || retriedReceipt.ResourceVersion != receipt.ResourceVersion {
		t.Fatalf("same-key revoke retry changed receipt: status=%d receipt=%+v original=%+v", retryRevoke.Code, retriedReceipt, receipt)
	}
	version = receipt.ResourceVersion
	if version != 3 {
		t.Fatalf("revocation version=%d, want 3", version)
	}
	secondRevoke := reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "rev-revoke-again", contracts.CommandRequest{Kind: "capability.deployment.revoke", ExpectedVersion: &version, Payload: mustJSON(t, map[string]any{"deployment_id": deploymentID})})
	if secondRevoke.Code != http.StatusConflict {
		t.Fatalf("second revoke status=%d %s, want conflict without another cleanup job", secondRevoke.Code, secondRevoke.Body.String())
	}
	var cleanupJobs int
	if err = store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM capability_deployment_jobs WHERE deployment_id=? AND cleanup_only=1`, deploymentID).Scan(&cleanupJobs); err != nil || cleanupJobs != 1 {
		t.Fatalf("second revoke created another cleanup job: count=%d err=%v", cleanupJobs, err)
	}
	staleVersion := version - 1
	staleRevoke := reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "rev-stale-version", contracts.CommandRequest{Kind: "capability.deployment.revoke", ExpectedVersion: &staleVersion, Payload: mustJSON(t, map[string]any{"deployment_id": deploymentID})})
	if staleRevoke.Code != http.StatusConflict {
		t.Fatalf("stale revocation version status=%d body=%s, want conflict", staleRevoke.Code, staleRevoke.Body.String())
	}
	stateResponse := reviewRequest(handler, http.MethodGet, "/v1/capabilities/deployments/"+deploymentID, deviceToken, "", nil)
	var state map[string]any
	if stateResponse.Code != http.StatusOK || json.Unmarshal(stateResponse.Body.Bytes(), &state) != nil || state["revoked_at"] == nil || state["desired_state"] != "stopped" || state["observed_state"] != "pending" || state["revocation_cleanup_state"] != "pending" {
		t.Fatalf("revoked state did not preserve pending cleanup: %d %s", stateResponse.Code, stateResponse.Body.String())
	}
	var triggerEnabled int
	var triggerRunState string
	if err = store.DB().QueryRowContext(t.Context(), `SELECT enabled FROM capability_trigger_definitions WHERE trigger_id=?`, triggerID).Scan(&triggerEnabled); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(t.Context(), `SELECT state FROM capability_trigger_runs WHERE trigger_run_id='queued-before-revoke'`).Scan(&triggerRunState); err != nil {
		t.Fatal(err)
	}
	if triggerEnabled != 0 || triggerRunState != "canceled" {
		t.Fatalf("revocation left trigger active: enabled=%d queued-state=%s", triggerEnabled, triggerRunState)
	}
	if cleanup, err := store.DB().QueryContext(t.Context(), `SELECT revoked_at FROM capability_grants WHERE grant_id=?`, oldGrant); err != nil {
		t.Fatal(err)
	} else {
		if !cleanup.Next() {
			_ = cleanup.Close()
			t.Fatal("revoked grant disappeared")
		}
		var revokedAt sql.NullString
		if err := cleanup.Scan(&revokedAt); err != nil || !revokedAt.Valid {
			_ = cleanup.Close()
			t.Fatalf("grant revocation audit missing: %v", err)
		}
		if err := cleanup.Close(); err != nil {
			t.Fatal(err)
		}
	}

	blocked := reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "rev-blocked-request", contracts.CommandRequest{Kind: "capability.deployment.request", ExpectedVersion: &version, Payload: mustJSON(t, map[string]any{"service_id": "revocable", "release_digest": releaseDigest, "operation": "install"})})
	if blocked.Code != http.StatusConflict {
		t.Fatalf("new deployment request during cleanup=%d %s, want conflict", blocked.Code, blocked.Body.String())
	}
	// Revocation disabled queued triggers synchronously, before a cleanup claim
	// has had a chance to run.
	triggerClaim := reviewRequest(handler, http.MethodPost, "/runner/v1/capability-triggers/claims", runnerToken, "", nil)
	if triggerClaim.Code != http.StatusNoContent {
		t.Fatalf("revoked trigger was claimed: %d %s", triggerClaim.Code, triggerClaim.Body.String())
	}

	cleanupResponse := reviewRequest(handler, http.MethodPost, "/runner/v1/deployments/claims", runnerToken, "", nil)
	var cleanupClaim contracts.DeploymentClaim
	if cleanupResponse.Code != http.StatusOK || json.Unmarshal(cleanupResponse.Body.Bytes(), &cleanupClaim) != nil || !cleanupClaim.CleanupOnly || cleanupClaim.DesiredOp != "stop" || cleanupClaim.Fence <= active.Fence || cleanupClaim.Grant.ExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("cleanup claim did not preserve narrow stop authority: %d %+v %s", cleanupResponse.Code, cleanupClaim, cleanupResponse.Body.String())
	}
	failedCleanup := contracts.DeploymentObservation{DeploymentID: deploymentID, AttemptID: cleanupClaim.AttemptID, Fence: cleanupClaim.Fence, ObservationID: "cleanup-failed", Operation: "stop", State: "failed", ErrorCode: "docker_unavailable", ObservedAt: time.Now().UTC()}
	failedResponse := reviewRequest(handler, http.MethodPost, "/runner/v1/deployments/"+deploymentID+"/observations", runnerToken, "", failedCleanup)
	if failedResponse.Code != http.StatusBadRequest {
		t.Fatalf("unproven cleanup failure was accepted as terminal: %d %s", failedResponse.Code, failedResponse.Body.String())
	}
	uncertain := contracts.DeploymentObservation{DeploymentID: deploymentID, AttemptID: cleanupClaim.AttemptID, Fence: cleanupClaim.Fence, ObservationID: "cleanup-uncertain", Operation: "stop", State: "uncertain", ErrorCode: "docker_outcome_unverified", ObservedAt: time.Now().UTC()}
	uncertainResponse := reviewRequest(handler, http.MethodPost, "/runner/v1/deployments/"+deploymentID+"/observations", runnerToken, "", uncertain)
	if uncertainResponse.Code != http.StatusAccepted {
		t.Fatalf("uncertain cleanup report=%d %s", uncertainResponse.Code, uncertainResponse.Body.String())
	}
	stateResponse = reviewRequest(handler, http.MethodGet, "/v1/capabilities/deployments/"+deploymentID, deviceToken, "", nil)
	if stateResponse.Code != http.StatusOK || json.Unmarshal(stateResponse.Body.Bytes(), &state) != nil || state["revocation_cleanup_state"] != "uncertain" || state["observed_state"] != "uncertain" {
		t.Fatalf("uncertain cleanup state not exposed without false confirmation: %d %s", stateResponse.Code, stateResponse.Body.String())
	}
	blocked = reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "rev-still-blocked", contracts.CommandRequest{Kind: "capability.deployment.request", ExpectedVersion: &version, Payload: mustJSON(t, map[string]any{"service_id": "revocable", "release_digest": releaseDigest, "operation": "install"})})
	if blocked.Code != http.StatusConflict {
		t.Fatalf("new request after uncertain cleanup=%d, want conflict", blocked.Code)
	}

	stopped := contracts.DeploymentObservation{DeploymentID: deploymentID, AttemptID: cleanupClaim.AttemptID, Fence: cleanupClaim.Fence, ObservationID: "cleanup-stopped", Operation: "stop", State: "stopped", ExternalEffectRef: []string{"docker:zoe-cap-revocable"}, TerminationProof: &contracts.CapabilityTerminationProof{EffectRef: "docker:zoe-cap-revocable", TerminatedAt: time.Now().UTC(), CleanupComplete: true}, ObservedAt: time.Now().UTC()}
	stoppedResponse := reviewRequest(handler, http.MethodPost, "/runner/v1/deployments/"+deploymentID+"/observations", runnerToken, "", stopped)
	if stoppedResponse.Code != http.StatusAccepted {
		t.Fatalf("confirmed cleanup report=%d %s", stoppedResponse.Code, stoppedResponse.Body.String())
	}
	stateResponse = reviewRequest(handler, http.MethodGet, "/v1/capabilities/deployments/"+deploymentID, deviceToken, "", nil)
	if stateResponse.Code != http.StatusOK || json.Unmarshal(stateResponse.Body.Bytes(), &state) != nil || state["revocation_cleanup_state"] != "confirmed" {
		t.Fatalf("trusted cleanup proof was not exposed as confirmed: %d %s", stateResponse.Code, stateResponse.Body.String())
	}
	// Confirmation fences the still-claimed start. A late report cannot
	// overwrite the stopped proof or reopen the old attempt.
	oldObservation := contracts.DeploymentObservation{DeploymentID: active.DeploymentID, AttemptID: active.AttemptID, Fence: active.Fence, ObservationID: "old-start-completed", Operation: "start", State: "running", Health: contracts.CapabilityHealth{State: "healthy", CheckedAt: time.Now().UTC(), EvidenceRef: "health:old-start"}, ObservedAt: time.Now().UTC()}
	oldReport := reviewRequest(handler, http.MethodPost, "/runner/v1/deployments/"+deploymentID+"/observations", runnerToken, "", oldObservation)
	if oldReport.Code != http.StatusConflict {
		t.Fatalf("late old start report=%d %s, want stale conflict", oldReport.Code, oldReport.Body.String())
	}
	newRequest := reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "rev-new-request", contracts.CommandRequest{Kind: "capability.deployment.request", ExpectedVersion: &version, Payload: mustJSON(t, map[string]any{"service_id": "revocable", "release_digest": releaseDigest, "operation": "install"})})
	if newRequest.Code != http.StatusOK || json.Unmarshal(newRequest.Body.Bytes(), &receipt) != nil {
		t.Fatalf("new request after proven cleanup=%d %s", newRequest.Code, newRequest.Body.String())
	}
	stateResponse = reviewRequest(handler, http.MethodGet, "/v1/capabilities/deployments/"+deploymentID, deviceToken, "", nil)
	if stateResponse.Code != http.StatusOK || json.Unmarshal(stateResponse.Body.Bytes(), &state) != nil || state["revocation_cleanup_state"] != nil || state["revoked_at"] != nil {
		t.Fatalf("new request did not clear revocation state: %d %s", stateResponse.Code, stateResponse.Body.String())
	}
	cleanupReceiptReplay := reviewRequest(handler, http.MethodPost, "/runner/v1/deployments/"+deploymentID+"/observations", runnerToken, "", stopped)
	if cleanupReceiptReplay.Code != http.StatusAccepted {
		t.Fatalf("exact cleanup receipt replay after fresh authorization=%d %s, want accepted", cleanupReceiptReplay.Code, cleanupReceiptReplay.Body.String())
	}
	version = receipt.ResourceVersion
	newAuthorize := reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "rev-new-authorize", contracts.CommandRequest{Kind: "capability.deployment.authorize", ExpectedVersion: &version, Payload: mustJSON(t, map[string]any{"deployment_id": deploymentID, "scope_kind": "workspace", "actions": []string{"install"}, "budget": budget, "expires_at": time.Now().UTC().Add(time.Hour)})})
	if newAuthorize.Code != http.StatusOK || json.Unmarshal(newAuthorize.Body.Bytes(), &receipt) != nil {
		t.Fatalf("fresh grant authorization=%d %s", newAuthorize.Code, newAuthorize.Body.String())
	}
	var newGrant string
	if err = store.DB().QueryRowContext(t.Context(), `SELECT grant_id FROM capability_deployments WHERE deployment_id=?`, deploymentID).Scan(&newGrant); err != nil || newGrant == oldGrant {
		t.Fatalf("fresh authorization reused revoked grant %q (old %q): %v", newGrant, oldGrant, err)
	}
}
