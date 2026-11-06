package capabilities

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/agents/opencode"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/runner"
	"github.com/ronigooja/Zoe/internal/storage"
)

func TestCapabilityReleaseRequiresExplicitGrantAndAuthorizationBeforeJob(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = RegisterMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	insertCapabilityTestDevice(t, store, "device", "owner")
	server, err := control.NewServer(store, control.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{db: store.DB(), server: server, availableCapabilityIDs: map[string]struct{}{"mail.read": {}}}
	manifest := contracts.CapabilityManifest{SchemaVersion: 1, ArtifactRef: "oci://registry.example/zoe/mail@sha256:" + strings.Repeat("a", 64), ArtifactDigest: "sha256:" + strings.Repeat("a", 64), Entrypoint: "/app/run", Capabilities: []string{"mail.read"}, Budget: contracts.CapabilityResourceBudget{CPUMillis: 500, MemoryBytes: 1 << 20, DiskBytes: 1 << 20, TimeoutSeconds: 60, Concurrency: 1}, Network: contracts.CapabilityNetworkPolicy{Mode: "none"}, HealthCheck: &contracts.CapabilityHealthCheck{Kind: "http", Target: "/health", Port: 8080, IntervalSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 2}}
	principal := contracts.Principal{WorkspaceID: "default", SubjectID: "owner", DeviceID: "device", Scopes: []string{"client:command"}}
	command := control.CommandContext{CommandID: "cmd", Principal: principal, AcceptedAt: time.Now().UTC()}
	payload, _ := json.Marshal(submitReleasePayload{Manifest: manifest})
	tx := begin(t, store.DB())
	_, _, err = svc.submitRelease(ctx, tx, command, contracts.CommandRequest{Payload: payload})
	_ = tx.Rollback()
	var apiErr *control.APIError
	if err == nil || !asAPI(err, &apiErr) || apiErr.Status != 403 {
		t.Fatalf("generic command grant submitted capability release: %v", err)
	}
	principal.Scopes = []string{"capability:release:submit", "capability:deployment:request", "capability:deployment:authorize"}
	command.Principal = principal
	tx = begin(t, store.DB())
	result, _, err := svc.submitRelease(ctx, tx, command, contracts.CommandRequest{Payload: payload})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	unregistered := manifest
	unregistered.Capabilities = []string{"mail.send"}
	tx = begin(t, store.DB())
	_, _, err = svc.submitRelease(ctx, tx, command, contracts.CommandRequest{Payload: marshal(submitReleasePayload{Manifest: unregistered})})
	_ = tx.Rollback()
	var registryErr *control.APIError
	if !asAPI(err, &registryErr) || registryErr.Code != "capability_not_registered" {
		t.Fatalf("unregistered capability ID was accepted: %v", err)
	}
	var digest string
	if err = store.DB().QueryRowContext(ctx, `SELECT digest FROM capability_releases WHERE release_id=?`, result.ResourceID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(deploymentRequestPayload{ServiceID: "mail-monitor", ReleaseDigest: digest, Operation: "install"})
	tx = begin(t, store.DB())
	deployment, _, err := svc.requestDeployment(ctx, tx, command, contracts.CommandRequest{Payload: request})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM capability_deployment_jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("deployment scheduled before grant: jobs=%d err=%v", jobs, err)
	}
	authorize, _ := json.Marshal(authorizePayload{DeploymentID: deployment.ResourceID, ScopeKind: "workspace", Actions: []string{"install", "start", "stop"}, Budget: manifest.Budget, ExpiresAt: time.Now().UTC().Add(time.Hour)})
	expected := int64(1)
	command.Principal.DeviceID = ""
	tx = begin(t, store.DB())
	_, _, err = svc.authorizeDeployment(ctx, tx, command, contracts.CommandRequest{ExpectedVersion: &expected, Payload: authorize})
	_ = tx.Rollback()
	var deviceErr *control.APIError
	if !asAPI(err, &deviceErr) || deviceErr.Code != "device_required" {
		t.Fatalf("grant authorization without authenticated device was accepted: %v", err)
	}
	command.Principal.DeviceID = "device"
	tx = begin(t, store.DB())
	_, _, err = svc.authorizeDeployment(ctx, tx, command, contracts.CommandRequest{ExpectedVersion: &expected, Payload: authorize})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var authorizedBy, authorizedByDevice string
	if err = store.DB().QueryRowContext(ctx, `SELECT authorized_by,authorized_by_device_id FROM capability_grants WHERE deployment_id=?`, deployment.ResourceID).Scan(&authorizedBy, &authorizedByDevice); err != nil {
		t.Fatal(err)
	}
	if authorizedBy != principal.SubjectID || authorizedByDevice != principal.DeviceID {
		t.Fatalf("grant authorization audit provenance=(%q,%q), want (%q,%q)", authorizedBy, authorizedByDevice, principal.SubjectID, principal.DeviceID)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM capability_deployment_jobs WHERE deployment_id=? AND status='pending'`, deployment.ResourceID).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("authorized deployment job not durably queued: jobs=%d err=%v", jobs, err)
	}
	var desired string
	if err = store.DB().QueryRowContext(ctx, `SELECT desired_state FROM capability_deployments WHERE deployment_id=?`, deployment.ResourceID).Scan(&desired); err != nil || desired != "installing" {
		t.Fatalf("desired state not set: %q err=%v", desired, err)
	}
}

func TestManifestRejectsUnsafeAndOverbroadInputs(t *testing.T) {
	base := contracts.CapabilityManifest{SchemaVersion: 1, ArtifactRef: "oci://registry.example/app@sha256:" + strings.Repeat("b", 64), ArtifactDigest: "sha256:" + strings.Repeat("b", 64), Entrypoint: "/app/run", Capabilities: []string{"mail.read"}, HealthCheck: &contracts.CapabilityHealthCheck{Kind: "http", Target: "/health", Port: 8080, IntervalSeconds: 30, TimeoutSeconds: 5, FailureThreshold: 3}, Budget: contracts.CapabilityResourceBudget{CPUMillis: 1, MemoryBytes: 1, DiskBytes: 1, TimeoutSeconds: 1, Concurrency: 1}, Network: contracts.CapabilityNetworkPolicy{Mode: "none"}}
	if err := validateManifest(&base); err != nil {
		t.Fatal(err)
	}
	base.Triggers = []contracts.CapabilityTrigger{{Kind: "event", Source: "task.created", Mode: "worker"}}
	if err := validateManifest(&base); err != nil {
		t.Fatalf("worker trigger rejected: %v", err)
	}
	base.Triggers = []contracts.CapabilityTrigger{{Kind: "event", Source: "task.created", Mode: "super"}}
	if err := validateManifest(&base); err != nil {
		t.Fatalf("super trigger rejected: %v", err)
	}
	base.Triggers = nil
	base.HealthCheck = nil
	if err := validateManifest(&base); err == nil {
		t.Fatal("manifest without an executable health check was accepted")
	}
	base.HealthCheck = &contracts.CapabilityHealthCheck{Kind: "http", Target: "/health", Port: 8080, IntervalSeconds: 30, TimeoutSeconds: 5, FailureThreshold: 3}
	base.HealthCheck = &contracts.CapabilityHealthCheck{Kind: "exec", Target: "/bin/sh", Args: []string{"-c", "true"}, IntervalSeconds: 30, TimeoutSeconds: 5, FailureThreshold: 3}
	if err := validateManifest(&base); err == nil {
		t.Fatal("shell or host-path health check accepted")
	}
	base.HealthCheck = nil
	base.Mounts = []contracts.CapabilityMount{{VolumeID: "user-volume", Target: "/../../host", ReadOnly: false}}
	if err := validateManifest(&base); err == nil {
		t.Fatal("unsafe mount target accepted")
	}
}

func TestRunnerDeploymentClaimAndFencedObservation(t *testing.T) {
	ctx := context.Background()
	runnerToken := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	server, err := control.NewServer(store, control.Config{RunnerToken: runnerToken, RunnerID: "runner-test", RunnerWorkspaceIDs: []string{"default"}, RunnerScopes: []string{"capability:deploy", "runner:observe", "runner:claim", "runner:control:read"}, Profiles: map[string]control.RuntimeProfile{
		"worker-profile": {ID: "worker-profile", Harness: "opencode", RuntimeRouteID: "runtime", ModelRouteID: "model", Role: "worker", IsolationPolicy: runner.OpenCodeWorkspaceWritePolicy},
		"super-profile":  {ID: "super-profile", Harness: "opencode", RuntimeRouteID: "runtime", ModelRouteID: "model", Role: "super", IsolationPolicy: runner.OpenCodeWorkspaceWritePolicy},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = Register(server, store, Config{AvailableCapabilityIDs: []string{"mail.read"}, RuntimeBackend: runtimeBackendDocker}); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	insertCapabilityTestDevice(t, store, "device", "owner")
	if err := server.RegisterRunnerCredential(ctx, runnerToken, "runner-test", []string{"default"}, []string{"capability:deploy", "runner:observe", "runner:claim", "runner:control:read"}); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	svc := &Service{db: store.DB(), server: server, availableCapabilityIDs: map[string]struct{}{"mail.read": {}}, runtimeBackend: runtimeBackendDocker}
	manifest := contracts.CapabilityManifest{SchemaVersion: 1, ArtifactRef: "oci://registry.example/app@sha256:" + strings.Repeat("c", 64), ArtifactDigest: "sha256:" + strings.Repeat("c", 64), Entrypoint: "/app/run", Capabilities: []string{"mail.read"}, Triggers: []contracts.CapabilityTrigger{{Kind: "event", Source: "task.created", Mode: "program", Filters: []contracts.CapabilityTriggerFilter{{Field: "task_id", Operator: "eq", Value: "task-1"}}}, {Kind: "event", Source: "task.created", Mode: "worker", Scopes: []string{"mail.read"}}, {Kind: "event", Source: "task.created", Mode: "super", Scopes: []string{"mail.read"}}, {Kind: "schedule", Source: "schedule.interval", Mode: "program", IntervalSeconds: 60, MissedRunPolicy: "coalesce"}, {Kind: "schedule", Source: "schedule.interval", Mode: "super", IntervalSeconds: 60, MissedRunPolicy: "coalesce"}}, Budget: contracts.CapabilityResourceBudget{CPUMillis: 50, MemoryBytes: 100, DiskBytes: 100, TimeoutSeconds: 10, Concurrency: 1}, Network: contracts.CapabilityNetworkPolicy{Mode: "none"}, HealthCheck: &contracts.CapabilityHealthCheck{Kind: "http", Target: "/health", Port: 8080, IntervalSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 2}}
	principal := contracts.Principal{WorkspaceID: "default", SubjectID: "owner", DeviceID: "device", Scopes: []string{"capability:release:submit", "capability:deployment:request", "capability:deployment:authorize"}}
	command := control.CommandContext{CommandID: "cmd", Principal: principal, AcceptedAt: time.Now().UTC()}
	tx := begin(t, store.DB())
	release, _, err := svc.submitRelease(ctx, tx, command, contracts.CommandRequest{Payload: marshal(submitReleasePayload{Manifest: manifest})})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var digest string
	if err = store.DB().QueryRowContext(ctx, `SELECT digest FROM capability_releases WHERE release_id=?`, release.ResourceID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	tx = begin(t, store.DB())
	dep, _, err := svc.requestDeployment(ctx, tx, command, contracts.CommandRequest{Payload: marshal(deploymentRequestPayload{ServiceID: "service", ReleaseDigest: digest, Operation: "install"})})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx = begin(t, store.DB())
	exp := int64(1)
	_, _, err = svc.authorizeDeployment(ctx, tx, command, contracts.CommandRequest{ExpectedVersion: &exp, Payload: marshal(authorizePayload{DeploymentID: dep.ResourceID, ScopeKind: "workspace", Actions: []string{"install"}, Budget: manifest.Budget, ExpiresAt: time.Now().UTC().Add(time.Hour)})})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	claimReq := httptest.NewRequest(http.MethodPost, "/runner/v1/deployments/claims", nil)
	claimReq.Header.Set("Authorization", "Bearer "+runnerToken)
	claimResp := httptest.NewRecorder()
	handler.ServeHTTP(claimResp, claimReq)
	if claimResp.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", claimResp.Code, claimResp.Body.String())
	}
	var claim contracts.DeploymentClaim
	if err = json.Unmarshal(claimResp.Body.Bytes(), &claim); err != nil {
		t.Fatal(err)
	}
	if claim.DeploymentID != dep.ResourceID || claim.Fence != 1 || claim.DesiredOp != "install" {
		t.Fatalf("bad deployment claim: %#v", claim)
	}
	currentReq := httptest.NewRequest(http.MethodGet, "/runner/v1/deployments/claims/current", nil)
	currentReq.Header.Set("Authorization", "Bearer "+runnerToken)
	currentResp := httptest.NewRecorder()
	handler.ServeHTTP(currentResp, currentReq)
	if currentResp.Code != http.StatusOK {
		t.Fatalf("current claim status=%d body=%s", currentResp.Code, currentResp.Body.String())
	}
	var current contracts.DeploymentClaim
	if err = json.Unmarshal(currentResp.Body.Bytes(), &current); err != nil || current.AttemptID != claim.AttemptID || current.Fence != claim.Fence || current.ClaimState != "claimed" {
		t.Fatalf("current claim changed identity: current=%#v err=%v", current, err)
	}
	retryClaimReq := httptest.NewRequest(http.MethodPost, "/runner/v1/deployments/claims", nil)
	retryClaimReq.Header.Set("Authorization", "Bearer "+runnerToken)
	retryClaimResp := httptest.NewRecorder()
	handler.ServeHTTP(retryClaimResp, retryClaimReq)
	var retried contracts.DeploymentClaim
	if retryClaimResp.Code != http.StatusOK || json.Unmarshal(retryClaimResp.Body.Bytes(), &retried) != nil || retried.AttemptID != claim.AttemptID || retried.Fence != claim.Fence {
		t.Fatalf("claim retry created another attempt: status=%d body=%s", retryClaimResp.Code, retryClaimResp.Body.String())
	}
	obs := contracts.DeploymentObservation{DeploymentID: claim.DeploymentID, AttemptID: claim.AttemptID, Fence: claim.Fence, ObservationID: "obs-one", Operation: "install", State: "running", ExternalEffectRef: []string{"container:test-id"}, Health: contracts.CapabilityHealth{State: "healthy", CheckedAt: time.Now().UTC(), EvidenceRef: "health:test"}, ObservedAt: time.Now().UTC()}
	body, _ := json.Marshal(obs)
	obsReq := httptest.NewRequest(http.MethodPost, "/runner/v1/deployments/"+claim.DeploymentID+"/observations", strings.NewReader(string(body)))
	obsReq.Header.Set("Authorization", "Bearer "+runnerToken)
	obsResp := httptest.NewRecorder()
	handler.ServeHTTP(obsResp, obsReq)
	if obsResp.Code != http.StatusAccepted {
		t.Fatalf("observation status=%d body=%s", obsResp.Code, obsResp.Body.String())
	}
	retryReq := httptest.NewRequest(http.MethodPost, "/runner/v1/deployments/"+claim.DeploymentID+"/observations", strings.NewReader(string(body)))
	retryReq.Header.Set("Authorization", "Bearer "+runnerToken)
	retryResp := httptest.NewRecorder()
	handler.ServeHTTP(retryResp, retryReq)
	if retryResp.Code != http.StatusAccepted {
		t.Fatalf("identical observation retry status=%d body=%s", retryResp.Code, retryResp.Body.String())
	}
	var observations int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM capability_observations WHERE attempt_id=?`, claim.AttemptID).Scan(&observations); err != nil || observations != 1 {
		t.Fatalf("observation retry duplicated persistence: count=%d err=%v", observations, err)
	}
	var state string
	if err = store.DB().QueryRowContext(ctx, `SELECT observed_state FROM capability_deployments WHERE deployment_id=?`, claim.DeploymentID).Scan(&state); err != nil || state != "running" {
		t.Fatalf("observed state not persisted: %q err=%v", state, err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE event_counters SET current_cursor=2 WHERE workspace_id='default'`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO events(event_id,workspace_id,cursor,schema_version,event_type,occurred_at,subject_id,aggregate_type,aggregate_id,aggregate_version,payload) VALUES('trigger-event-1','default',1,1,'task.created',?,'owner','task','task-1',1,'{"type":"task.created","payload":{"task":{"task_id":"task-1"}}}'),('trigger-event-2','default',2,1,'task.created',?,'owner','task','task-2',1,'{"type":"task.created","payload":{"task":{"task_id":"task-2"}}}')`, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE capability_trigger_definitions SET next_run_at=? WHERE kind='schedule'`, time.Now().UTC().Add(10*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	var enabledTriggers int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM capability_trigger_definitions WHERE enabled=1`).Scan(&enabledTriggers); err != nil || enabledTriggers != 5 {
		t.Fatalf("triggers were not enabled: count=%d err=%v", enabledTriggers, err)
	}
	var admissionStatus capabilityTriggerAdmissionStatus
	for attempt := 0; attempt < 2; attempt++ {
		admissionRequest := httptest.NewRequest(http.MethodPost, "/runner/v1/capability-triggers/admissions", nil)
		admissionRequest.Header.Set("Authorization", "Bearer "+runnerToken)
		admissionResponse := httptest.NewRecorder()
		handler.ServeHTTP(admissionResponse, admissionRequest)
		if admissionResponse.Code != http.StatusOK || json.Unmarshal(admissionResponse.Body.Bytes(), &admissionStatus) != nil {
			t.Fatalf("trigger admission attempt %d status=%d body=%s", attempt, admissionResponse.Code, admissionResponse.Body.String())
		}
	}
	if admissionStatus.ProgramExecution != "unsupported" || admissionStatus.PendingProgramTriggers < 1 || admissionStatus.ProgramReasonCode != "program_trigger_executor_unavailable" {
		t.Fatalf("program trigger support status was not explicit: %+v", admissionStatus)
	}
	var idempotentAdmissionTasks, idempotentAdmissionRuns int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE source='service'`).Scan(&idempotentAdmissionTasks); err != nil || idempotentAdmissionTasks != 2 {
		t.Fatalf("repeated trigger polls created %d tasks, want 2 (err=%v)", idempotentAdmissionTasks, err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM capability_trigger_runs WHERE mode IN ('worker','super') AND state='running' AND task_id<>''`).Scan(&idempotentAdmissionRuns); err != nil || idempotentAdmissionRuns != 2 {
		t.Fatalf("repeated trigger polls admitted %d worker/super runs, want 2 (err=%v)", idempotentAdmissionRuns, err)
	}
	triggerClaimRequest := httptest.NewRequest(http.MethodPost, "/runner/v1/capability-triggers/claims", nil)
	triggerClaimRequest.Header.Set("Authorization", "Bearer "+runnerToken)
	triggerClaimResponse := httptest.NewRecorder()
	handler.ServeHTTP(triggerClaimResponse, triggerClaimRequest)
	if triggerClaimResponse.Code != http.StatusOK {
		t.Fatalf("trigger claim status=%d body=%s", triggerClaimResponse.Code, triggerClaimResponse.Body.String())
	}
	var triggerClaim contracts.CapabilityTriggerClaim
	if err = json.Unmarshal(triggerClaimResponse.Body.Bytes(), &triggerClaim); err != nil || triggerClaim.EventID != "trigger-event-1" || triggerClaim.Mode != "program" || string(triggerClaim.EventPayload) != `{"task":{"task_id":"task-1"}}` {
		t.Fatalf("bad trigger claim: %#v err=%v", triggerClaim, err)
	}
	var admittedTasks int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE source='service'`).Scan(&admittedTasks); err != nil || admittedTasks != 2 {
		t.Fatalf("worker/super event triggers admitted %d tasks, want 2 (err=%v)", admittedTasks, err)
	}
	var workerTasks, superTasks int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE source='service' AND profile_id='worker-profile'`).Scan(&workerTasks); err != nil || workerTasks != 1 {
		t.Fatalf("worker trigger profile count=%d err=%v", workerTasks, err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE source='service' AND profile_id='super-profile'`).Scan(&superTasks); err != nil || superTasks != 1 {
		t.Fatalf("super trigger profile count=%d err=%v", superTasks, err)
	}
	var admittedRuns, admissionEvents int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM capability_trigger_runs WHERE mode IN ('worker','super') AND state='running' AND task_id<>''`).Scan(&admittedRuns); err != nil || admittedRuns != 2 {
		t.Fatalf("worker/super run-to-task links=%d err=%v", admittedRuns, err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE event_type='capability.trigger.task_admitted'`).Scan(&admissionEvents); err != nil || admissionEvents != 2 {
		t.Fatalf("trusted task admission events=%d err=%v", admissionEvents, err)
	}
	var queuedTaskRuns int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM capability_trigger_runs WHERE mode IN ('worker','super') AND state='queued'`).Scan(&queuedTaskRuns); err != nil || queuedTaskRuns != 2 {
		t.Fatalf("concurrency limit did not leave one run queued per role: count=%d err=%v", queuedTaskRuns, err)
	}
	triggerCurrentRequest := httptest.NewRequest(http.MethodGet, "/runner/v1/capability-triggers/claims/current", nil)
	triggerCurrentRequest.Header.Set("Authorization", "Bearer "+runnerToken)
	triggerCurrentResponse := httptest.NewRecorder()
	handler.ServeHTTP(triggerCurrentResponse, triggerCurrentRequest)
	var triggerCurrent contracts.CapabilityTriggerClaim
	if triggerCurrentResponse.Code != http.StatusOK || json.Unmarshal(triggerCurrentResponse.Body.Bytes(), &triggerCurrent) != nil || triggerCurrent.AttemptID != triggerClaim.AttemptID || triggerCurrent.Fence != triggerClaim.Fence {
		t.Fatalf("trigger replay changed claim: status=%d body=%s", triggerCurrentResponse.Code, triggerCurrentResponse.Body.String())
	}
	triggerObservation := contracts.CapabilityTriggerObservation{TriggerRunID: triggerClaim.TriggerRunID, AttemptID: triggerClaim.AttemptID, Fence: triggerClaim.Fence, ObservationID: "trigger-completed", State: "completed", ObservedAt: time.Now().UTC()}
	triggerObservationRequest := httptest.NewRequest(http.MethodPost, "/runner/v1/capability-triggers/"+triggerClaim.TriggerRunID+"/observations", strings.NewReader(string(marshal(triggerObservation))))
	triggerObservationRequest.Header.Set("Authorization", "Bearer "+runnerToken)
	triggerObservationResponse := httptest.NewRecorder()
	handler.ServeHTTP(triggerObservationResponse, triggerObservationRequest)
	if triggerObservationResponse.Code != http.StatusAccepted {
		t.Fatalf("trigger observation status=%d body=%s", triggerObservationResponse.Code, triggerObservationResponse.Body.String())
	}
	triggerDuplicateRequest := httptest.NewRequest(http.MethodPost, "/runner/v1/capability-triggers/claims", nil)
	triggerDuplicateRequest.Header.Set("Authorization", "Bearer "+runnerToken)
	triggerDuplicateResponse := httptest.NewRecorder()
	handler.ServeHTTP(triggerDuplicateResponse, triggerDuplicateRequest)
	if triggerDuplicateResponse.Code != http.StatusNoContent {
		t.Fatalf("duplicate event produced another claim: status=%d body=%s", triggerDuplicateResponse.Code, triggerDuplicateResponse.Body.String())
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE source='service'`).Scan(&admittedTasks); err != nil || admittedTasks != 2 {
		t.Fatalf("replaying trigger events duplicated admitted tasks: count=%d err=%v", admittedTasks, err)
	}
	var acceptedTaskID string
	if err = store.DB().QueryRowContext(ctx, `SELECT task_id FROM capability_trigger_runs WHERE mode IN ('worker','super') AND state='running' AND task_id<>'' ORDER BY task_id LIMIT 1`).Scan(&acceptedTaskID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE tasks SET status='complete',finished_at=?,updated_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), acceptedTaskID); err != nil {
		t.Fatal(err)
	}
	var resultEventCursor int64
	if err = store.DB().QueryRowContext(ctx, `SELECT coalesce(max(cursor),0)+1 FROM events WHERE workspace_id='default'`).Scan(&resultEventCursor); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE event_counters SET current_cursor=? WHERE workspace_id='default'`, resultEventCursor); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO events(event_id,workspace_id,cursor,schema_version,event_type,occurred_at,subject_id,aggregate_type,aggregate_id,aggregate_version,payload) VALUES('task-result-decided','default',?,1,'task.result_decided',?,'owner','task',?,2,'{"type":"task.result_decided","payload":{"status":"complete"}}')`, resultEventCursor, time.Now().UTC().Format(time.RFC3339Nano), acceptedTaskID); err != nil {
		t.Fatal(err)
	}
	acceptanceAdmissionRequest := httptest.NewRequest(http.MethodPost, "/runner/v1/capability-triggers/admissions", nil)
	acceptanceAdmissionRequest.Header.Set("Authorization", "Bearer "+runnerToken)
	acceptanceAdmissionResponse := httptest.NewRecorder()
	handler.ServeHTTP(acceptanceAdmissionResponse, acceptanceAdmissionRequest)
	if acceptanceAdmissionResponse.Code != http.StatusOK {
		t.Fatalf("acceptance event processing status=%d body=%s", acceptanceAdmissionResponse.Code, acceptanceAdmissionResponse.Body.String())
	}
	var acceptedTriggerState string
	if err = store.DB().QueryRowContext(ctx, `SELECT state FROM capability_trigger_runs WHERE task_id=?`, acceptedTaskID).Scan(&acceptedTriggerState); err != nil || acceptedTriggerState != "completed" {
		t.Fatalf("user-accepted service task trigger state=%q err=%v", acceptedTriggerState, err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE capability_trigger_definitions SET next_run_at=? WHERE kind='schedule'`, time.Now().UTC().Add(-10*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	scheduleRequest := httptest.NewRequest(http.MethodPost, "/runner/v1/capability-triggers/claims", nil)
	scheduleRequest.Header.Set("Authorization", "Bearer "+runnerToken)
	scheduleResponse := httptest.NewRecorder()
	handler.ServeHTTP(scheduleResponse, scheduleRequest)
	var scheduleClaim contracts.CapabilityTriggerClaim
	if scheduleResponse.Code != http.StatusOK || json.Unmarshal(scheduleResponse.Body.Bytes(), &scheduleClaim) != nil || scheduleClaim.ScheduledFor.IsZero() {
		t.Fatalf("coalesced schedule claim=%d body=%s", scheduleResponse.Code, scheduleResponse.Body.String())
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE source='service'`).Scan(&admittedTasks); err != nil || admittedTasks != 4 {
		t.Fatalf("schedule and backlog admission task count=%d, want 4 (err=%v)", admittedTasks, err)
	}
	scheduleObservation := contracts.CapabilityTriggerObservation{TriggerRunID: scheduleClaim.TriggerRunID, AttemptID: scheduleClaim.AttemptID, Fence: scheduleClaim.Fence, ObservationID: "schedule-completed", State: "completed", ObservedAt: time.Now().UTC()}
	scheduleObservationRequest := httptest.NewRequest(http.MethodPost, "/runner/v1/capability-triggers/"+scheduleClaim.TriggerRunID+"/observations", strings.NewReader(string(marshal(scheduleObservation))))
	scheduleObservationRequest.Header.Set("Authorization", "Bearer "+runnerToken)
	scheduleObservationResponse := httptest.NewRecorder()
	handler.ServeHTTP(scheduleObservationResponse, scheduleObservationRequest)
	if scheduleObservationResponse.Code != http.StatusAccepted {
		t.Fatalf("schedule observation=%d body=%s", scheduleObservationResponse.Code, scheduleObservationResponse.Body.String())
	}
	// Saturated old work must not consume the bounded candidate page and hide
	// an eligible trigger later in the workspace queue.
	var eventTrigger, activeGrant, activeDigest, activeConfigRef, activeConfigHash string
	var activeVersion int64
	if err = store.DB().QueryRowContext(ctx, `SELECT trigger_id FROM capability_trigger_definitions WHERE deployment_id=? AND kind='event'`, dep.ResourceID).Scan(&eventTrigger); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT grant_id,release_digest,config_ref,config_sha256,version FROM capability_deployments WHERE deployment_id=?`, dep.ResourceID).Scan(&activeGrant, &activeDigest, &activeConfigRef, &activeConfigHash, &activeVersion); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE capability_trigger_runs SET state='running',runner_id='',claim_json=NULL WHERE trigger_run_id=(SELECT trigger_run_id FROM capability_trigger_runs WHERE trigger_id=? ORDER BY trigger_run_id LIMIT 1)`, eventTrigger); err != nil {
		t.Fatal(err)
	}
	var eventKey string
	if err = store.DB().QueryRowContext(ctx, `SELECT trigger_key FROM capability_trigger_definitions WHERE trigger_id=?`, eventTrigger).Scan(&eventKey); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < triggerEventPageSize; i++ {
		id := fmt.Sprintf("blocked-run-%03d", i)
		if _, err = store.DB().ExecContext(ctx, `INSERT INTO capability_trigger_runs(trigger_run_id,trigger_id,workspace_id,deployment_id,trigger_key,dedupe_key,mode,state,created_at,updated_at) VALUES(?,?,?,?,?,?,'program','queued',?,?)`, id, eventTrigger, "default", dep.ResourceID, eventKey, "blocked-"+id, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO capability_deployments(deployment_id,workspace_id,service_id,release_digest,requested_operation,desired_state,observed_state,grant_id,config_ref,config_sha256,version,created_by,created_at,updated_at) VALUES('fair-deployment','default','fair-service',?,'install','running','running','fair-grant',?,?,?,'owner',?,?)`, activeDigest, activeConfigRef, activeConfigHash, activeVersion, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO capability_grants(grant_id,workspace_id,deployment_id,release_digest,scope_kind,scope_id,principal_id,actions,budget,expires_at,authorized_by,created_at) SELECT 'fair-grant',workspace_id,'fair-deployment',release_digest,scope_kind,scope_id,principal_id,actions,budget,expires_at,authorized_by,created_at FROM capability_grants WHERE grant_id=?`, activeGrant); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO capability_trigger_definitions(trigger_id,workspace_id,deployment_id,release_digest,grant_id,trigger_key,kind,source,mode,scopes,filters,interval_seconds,missed_run_policy,enabled,expected_deployment_version,config_ref,config_sha256,created_at,updated_at) SELECT 'fair-trigger',workspace_id,'fair-deployment',release_digest,'fair-grant',trigger_key,kind,source,mode,scopes,filters,interval_seconds,missed_run_policy,1,expected_deployment_version,config_ref,config_sha256,?,? FROM capability_trigger_definitions WHERE trigger_id=?`, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), eventTrigger); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO capability_trigger_runs(trigger_run_id,trigger_id,workspace_id,deployment_id,trigger_key,dedupe_key,mode,state,created_at,updated_at) SELECT 'fair-eligible-run','fair-trigger',workspace_id,'fair-deployment',trigger_key,'fair-eligible','program','queued',?,? FROM capability_trigger_definitions WHERE trigger_id='fair-trigger'`, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	fairClaimRequest := httptest.NewRequest(http.MethodPost, "/runner/v1/capability-triggers/claims", nil)
	fairClaimRequest.Header.Set("Authorization", "Bearer "+runnerToken)
	fairClaimResponse := httptest.NewRecorder()
	handler.ServeHTTP(fairClaimResponse, fairClaimRequest)
	var fairClaim contracts.CapabilityTriggerClaim
	if fairClaimResponse.Code != http.StatusOK || json.Unmarshal(fairClaimResponse.Body.Bytes(), &fairClaim) != nil || fairClaim.DeploymentID != "fair-deployment" {
		t.Fatalf("eligible trigger behind saturated queue not claimed: status=%d body=%s", fairClaimResponse.Code, fairClaimResponse.Body.String())
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE capability_grants SET expires_at=? WHERE grant_id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), activeGrant); err != nil {
		t.Fatal(err)
	}
	tx = begin(t, store.DB())
	if err = svc.admitQueuedTaskTriggers(ctx, tx, "default", time.Now().UTC()); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var expiredTaskRuns, serviceTasks int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM capability_trigger_runs WHERE deployment_id=? AND mode IN ('worker','super') AND state='queued'`, dep.ResourceID).Scan(&expiredTaskRuns); err != nil || expiredTaskRuns != 0 {
		t.Fatalf("expired grant left queued task triggers: count=%d err=%v", expiredTaskRuns, err)
	}
	var canceledExpiredRuns int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM capability_trigger_runs WHERE deployment_id=? AND mode IN ('worker','super') AND state='canceled' AND last_error_code='grant_expired'`, dep.ResourceID).Scan(&canceledExpiredRuns); err != nil || canceledExpiredRuns != 1 {
		t.Fatalf("expired worker/super runs were not durably canceled: count=%d err=%v", canceledExpiredRuns, err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE source='service'`).Scan(&serviceTasks); err != nil || serviceTasks != 4 {
		t.Fatalf("expired grant admitted another task: count=%d err=%v", serviceTasks, err)
	}
	tx = begin(t, store.DB())
	if err = svc.reapInactiveServiceTasks(ctx, tx, []string{"default"}, time.Now().UTC()); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var canceledServiceTasks, canceledTaskEvents int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE source='service' AND status='canceled'`).Scan(&canceledServiceTasks); err != nil || canceledServiceTasks != 3 {
		t.Fatalf("expired queued service tasks not canceled: count=%d err=%v", canceledServiceTasks, err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE event_type='task.canceled' AND aggregate_type='task'`).Scan(&canceledTaskEvents); err != nil || canceledTaskEvents != 3 {
		t.Fatalf("canceled service task events=%d, want 3 (err=%v)", canceledTaskEvents, err)
	}
	// A new grant for the same deployment must not reactivate definitions that
	// were created under its previous grant, even when the deployment is running.
	var currentGrant, currentDigest string
	if err = store.DB().QueryRowContext(ctx, `SELECT grant_id,release_digest FROM capability_deployments WHERE deployment_id=?`, dep.ResourceID).Scan(&currentGrant, &currentDigest); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO capability_grants(grant_id,workspace_id,deployment_id,release_digest,scope_kind,scope_id,principal_id,actions,budget,expires_at,authorized_by,created_at) SELECT 'grant-rollover','default',deployment_id,release_digest,scope_kind,scope_id,principal_id,actions,budget,expires_at,authorized_by,created_at FROM capability_grants WHERE grant_id=?`, currentGrant); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE capability_deployments SET grant_id='grant-rollover' WHERE deployment_id=?`, dep.ResourceID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO capability_trigger_runs(trigger_run_id,trigger_id,workspace_id,deployment_id,trigger_key,dedupe_key,mode,state,created_at,updated_at) SELECT 'obsolete-queued-run',trigger_id,workspace_id,deployment_id,trigger_key,'rollover-test',mode,'queued',?,? FROM capability_trigger_definitions WHERE deployment_id=? LIMIT 1`, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), dep.ResourceID); err != nil {
		t.Fatal(err)
	}
	tx = begin(t, store.DB())
	if err = svc.enableDeploymentTriggers(ctx, tx, dep.ResourceID, true, time.Now().UTC()); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM capability_trigger_definitions WHERE deployment_id=? AND enabled=1`, dep.ResourceID).Scan(&enabledTriggers); err != nil || enabledTriggers != 0 {
		t.Fatalf("old grant definitions were reactivated: count=%d err=%v", enabledTriggers, err)
	}
	var obsoleteState string
	if err = store.DB().QueryRowContext(ctx, `SELECT state FROM capability_trigger_runs WHERE trigger_run_id='obsolete-queued-run'`).Scan(&obsoleteState); err != nil || obsoleteState != "canceled" {
		t.Fatalf("queued work from old grant was not canceled: state=%q err=%v", obsoleteState, err)
	}
	obs.Fence++
	body, _ = json.Marshal(obs)
	badReq := httptest.NewRequest(http.MethodPost, "/runner/v1/deployments/"+claim.DeploymentID+"/observations", strings.NewReader(string(body)))
	badReq.Header.Set("Authorization", "Bearer "+runnerToken)
	badResp := httptest.NewRecorder()
	handler.ServeHTTP(badResp, badReq)
	if badResp.Code != http.StatusConflict {
		t.Fatalf("stale fence status=%d body=%s", badResp.Code, badResp.Body.String())
	}
	for i, fixture := range []struct {
		id, eventKind string
	}{
		{"rejected", "run.create_rejected"},
		{"runtime-failed", "run.runtime_terminated"},
		{"stopped", "run.stop_confirmed"},
		{"awaiting-user", "run.runtime_terminated"},
	} {
		taskID := "service-task-" + fixture.id
		triggerRunID := "service-trigger-" + fixture.id
		runID := "run-" + fixture.id
		fence := uint64(i + 2)
		insertCapabilityTerminalTriggerFixture(t, store, dep.ResourceID, taskID, triggerRunID, runID, fence)
		if fixture.eventKind == "run.stop_confirmed" {
			if _, err = store.DB().ExecContext(ctx, `UPDATE runs SET desired_control='stop' WHERE id=?`, runID); err != nil {
				t.Fatal(err)
			}
		}
		postCapabilityRunTerminalObservation(t, handler, runnerToken, runID, fixture.eventKind, fence)
	}
	tx = begin(t, store.DB())
	if err = processTriggerEvents(ctx, tx, "default", time.Now().UTC()); err != nil {
		tx.Rollback()
		t.Fatalf("terminal run event processing: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		id, want string
	}{
		{"rejected", "failed"},
		{"runtime-failed", "failed"},
		{"stopped", "canceled"},
		{"awaiting-user", "completed"},
	} {
		var state string
		if err = store.DB().QueryRowContext(ctx, `SELECT state FROM capability_trigger_runs WHERE trigger_run_id=?`, "service-trigger-"+fixture.id).Scan(&state); err != nil || state != fixture.want {
			t.Fatalf("run terminal event %s mapped trigger state=%q, want %q (err=%v)", fixture.id, state, fixture.want, err)
		}
	}
	var activeSlots int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM capability_trigger_runs WHERE trigger_id=(SELECT trigger_id FROM capability_trigger_runs WHERE trigger_run_id=?) AND state IN ('claimed','running','uncertain','waiting')`, "service-trigger-awaiting-user").Scan(&activeSlots); err != nil || activeSlots != 0 {
		t.Fatalf("completed result retained concurrency slot: active=%d err=%v", activeSlots, err)
	}
}

func insertCapabilityTerminalTriggerFixture(t *testing.T, store *storage.Store, deployment, taskID, triggerRunID, runID string, fence uint64) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id) SELECT ?,workspace_id,'terminal fixture','terminal fixture','service',profile_id,'active',1,?,?,created_by_device_id,?,owner_subject_id FROM tasks WHERE source='service' ORDER BY created_at LIMIT 1`, taskID, at, at, "terminal-command-"+taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO capability_trigger_runs(trigger_run_id,trigger_id,workspace_id,deployment_id,trigger_key,dedupe_key,mode,state,task_id,created_at,updated_at) SELECT ?,trigger_id,workspace_id,deployment_id,trigger_key,?,'worker','uncertain',?,?,? FROM capability_trigger_definitions WHERE deployment_id=? AND mode='worker' LIMIT 1`, triggerRunID, "terminal-dedupe-"+taskID, taskID, at, at, deployment); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,fence,runner_id,authorized_device_id,command_id,goal,profile_id,runtime_route_id,model_route_id,role,harness,started_at) VALUES(?,?,?,?,'dispatching',?,'runner-test','device',?,'terminal trigger fixture','worker-profile','runtime','model','worker','opencode',?)`, runID, "default", taskID, "attempt-"+runID, fence, "command-"+runID, at); err != nil {
		t.Fatal(err)
	}
}

func postCapabilityRunTerminalObservation(t *testing.T, handler http.Handler, token, runID, eventKind string, fence uint64) {
	t.Helper()
	post := func(obs contracts.RunnerObservation) {
		body, err := json.Marshal(obs)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/runner/v1/runs/"+runID+"/observations", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", obs.ObservationID)
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("runner observation %s status=%d body=%s", obs.Kind, resp.Code, resp.Body.String())
		}
	}
	base := contracts.RunnerObservation{RunID: runID, AttemptID: "attempt-" + runID, Fence: fence, OccurredAt: time.Now().UTC(), Evidence: []contracts.Evidence{}, ExternalEffects: []contracts.ExternalEffect{}}
	started := base
	started.ObservationID = "obs-start-" + runID
	started.Kind = "dispatch_started"
	post(started)
	if eventKind == "run.create_rejected" {
		terminal := base
		terminal.ObservationID = "obs-terminal-" + runID
		terminal.Kind = "create_rejected"
		terminal.CreateRejection = &contracts.CreateRejectionProof{Code: "invalid_request"}
		post(terminal)
		return
	}
	bound := base
	bound.ObservationID = "obs-bound-" + runID
	bound.Kind = "dispatch_bound"
	bound.RuntimeSessionID = "runtime-session-" + runID
	post(bound)
	if eventKind == "run.runtime_terminated" {
		if strings.Contains(runID, "awaiting-user") {
			result := base
			result.ObservationID = "obs-result-" + runID
			result.Kind = "result"
			result.RuntimeSessionID = bound.RuntimeSessionID
			result.Result = json.RawMessage(`{"answer":"ready for review"}`)
			post(result)
		}
		terminal := base
		terminal.ObservationID = "obs-terminal-" + runID
		terminal.Kind = "runtime_terminated"
		terminal.RuntimeSessionID = bound.RuntimeSessionID
		terminal.TerminationProof = &contracts.TerminationProof{Source: "runner_container_removal", Method: "container_removed", RuntimeSessionID: bound.RuntimeSessionID, RuntimeContainerID: "runtime-container", VerifiedAt: time.Now().UTC(), CleanupComplete: true}
		post(terminal)
		return
	}
	terminal := base
	terminal.ObservationID = "obs-terminal-" + runID
	terminal.Kind = "stop_confirmed"
	terminal.RuntimeSessionID = bound.RuntimeSessionID
	terminal.TerminationProof = &contracts.TerminationProof{Source: "runner_container_removal", Method: "container_removed", RuntimeSessionID: bound.RuntimeSessionID, RuntimeContainerID: "runtime-container", VerifiedAt: time.Now().UTC(), CleanupComplete: true}
	post(terminal)
}

type capabilityIntegrationIsolation struct{}

func (capabilityIntegrationIsolation) Check(context.Context, contracts.JobClaim) error { return nil }

type capabilityIntegrationRuntime struct {
	created, terminated int
}

func (r *capabilityIntegrationRuntime) Create(context.Context, runner.RuntimeSpec) (runner.RuntimeBinding, error) {
	r.created++
	return runner.RuntimeBinding{SessionID: "runtime-session"}, nil
}
func (*capabilityIntegrationRuntime) Prompt(context.Context, runner.RuntimeBinding, string) error {
	return nil
}
func (*capabilityIntegrationRuntime) Status(context.Context, runner.RuntimeBinding) (map[string]opencode.Status, error) {
	return map[string]opencode.Status{"runtime-session": {Type: "idle"}}, nil
}
func (*capabilityIntegrationRuntime) Messages(context.Context, runner.RuntimeBinding) ([]opencode.Message, error) {
	var message opencode.Message
	message.Info.ID = "message-1"
	message.Info.Role = "assistant"
	message.Info.Time.Completed = time.Now().UnixMilli()
	message.Info.Finish = "stop"
	message.Parts = []map[string]any{{"type": "tool", "tool": "capability_lookup", "callID": "call-1", "state": map[string]any{"status": "completed", "title": "lookup"}}}
	return []opencode.Message{message}, nil
}
func (*capabilityIntegrationRuntime) Events(context.Context, runner.RuntimeBinding) (<-chan opencode.Event, <-chan error) {
	events := make(chan opencode.Event)
	errs := make(chan error)
	close(events)
	close(errs)
	return events, errs
}
func (*capabilityIntegrationRuntime) Abort(context.Context, runner.RuntimeBinding) error { return nil }
func (r *capabilityIntegrationRuntime) Terminate(context.Context, runner.RuntimeBinding) (string, error) {
	r.terminated++
	return "runtime-container", nil
}

func TestCapabilityTriggerRunnerOpenCodeResultChain(t *testing.T) {
	ctx := context.Background()
	runnerToken := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	server, err := control.NewServer(store, control.Config{RunnerToken: runnerToken, RunnerID: "runner-chain", RunnerWorkspaceIDs: []string{"default"}, RunnerScopes: []string{"runner:claim", "runner:observe", "runner:control:read", "capability:deploy"}, Profiles: map[string]control.RuntimeProfile{
		"worker-profile": {ID: "worker-profile", Harness: "opencode", RuntimeRouteID: "runtime", ModelRouteID: "model", Role: "worker", IsolationPolicy: runner.OpenCodeWorkspaceWritePolicy},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = Register(server, store, Config{AvailableCapabilityIDs: []string{"mail.read"}, RuntimeBackend: runtimeBackendDocker}); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	insertCapabilityTestDevice(t, store, "device", "owner")
	if err := server.RegisterRunnerCredential(ctx, runnerToken, "runner-chain", []string{"default"}, []string{"capability:deploy", "runner:observe", "runner:claim", "runner:control:read"}); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	svc := &Service{db: store.DB(), server: server, availableCapabilityIDs: map[string]struct{}{"mail.read": {}}, runtimeBackend: runtimeBackendDocker}
	manifest := contracts.CapabilityManifest{SchemaVersion: 1, ArtifactRef: "oci://registry.example/app@sha256:" + strings.Repeat("c", 64), ArtifactDigest: "sha256:" + strings.Repeat("c", 64), Entrypoint: "/app/run", Capabilities: []string{"mail.read"}, Triggers: []contracts.CapabilityTrigger{{Kind: "event", Source: "task.created", Mode: "worker", Scopes: []string{"mail.read"}}}, Budget: contracts.CapabilityResourceBudget{CPUMillis: 50, MemoryBytes: 100, DiskBytes: 100, TimeoutSeconds: 30, Concurrency: 1}, Network: contracts.CapabilityNetworkPolicy{Mode: "none"}, HealthCheck: &contracts.CapabilityHealthCheck{Kind: "http", Target: "/health", Port: 8080, IntervalSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 2}}
	principal := contracts.Principal{WorkspaceID: "default", SubjectID: "owner", DeviceID: "device", Scopes: []string{"capability:release:submit", "capability:deployment:request", "capability:deployment:authorize"}}
	command := control.CommandContext{CommandID: "trigger-chain", Principal: principal, AcceptedAt: time.Now().UTC()}
	tx := begin(t, store.DB())
	release, _, err := svc.submitRelease(ctx, tx, command, contracts.CommandRequest{Payload: marshal(submitReleasePayload{Manifest: manifest})})
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var digest string
	if err = store.DB().QueryRowContext(ctx, `SELECT digest FROM capability_releases WHERE release_id=?`, release.ResourceID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	tx = begin(t, store.DB())
	deployment, _, err := svc.requestDeployment(ctx, tx, command, contracts.CommandRequest{Payload: marshal(deploymentRequestPayload{ServiceID: "mail", ReleaseDigest: digest, Operation: "install"})})
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	expectedVersion := int64(1)
	authorize := authorizePayload{DeploymentID: deployment.ResourceID, ScopeKind: "workspace", Actions: []string{"install"}, Budget: manifest.Budget, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	tx = begin(t, store.DB())
	if _, _, err = svc.authorizeDeployment(ctx, tx, command, contracts.CommandRequest{ExpectedVersion: &expectedVersion, Payload: marshal(authorize)}); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	controlHTTP := httptest.NewServer(handler)
	defer controlHTTP.Close()
	controlClient, err := runner.NewControlClient(controlHTTP.URL, runnerToken, controlHTTP.Client())
	if err != nil {
		t.Fatal(err)
	}
	deploymentClaim, err := controlClient.ClaimDeployment(ctx)
	if err != nil || deploymentClaim == nil {
		t.Fatalf("claim deployment for trigger setup: claim=%#v err=%v", deploymentClaim, err)
	}
	if err = controlClient.ObserveDeployment(ctx, contracts.DeploymentObservation{DeploymentID: deploymentClaim.DeploymentID, AttemptID: deploymentClaim.AttemptID, Fence: deploymentClaim.Fence, ObservationID: "trigger-chain-deployment-running", Operation: "install", State: "running", ExternalEffectRef: []string{"fixture-container"}, Health: contracts.CapabilityHealth{State: "healthy", CheckedAt: time.Now().UTC(), EvidenceRef: "fixture-health"}, ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("record running deployment observation: %v", err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE event_counters SET current_cursor=2 WHERE workspace_id='default'`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO events(event_id,workspace_id,cursor,schema_version,event_type,occurred_at,subject_id,aggregate_type,aggregate_id,aggregate_version,payload) VALUES('chain-source-1','default',1,1,'task.created',?,'owner','task','source-task-1',1,'{"type":"task.created","payload":{"task":{"task_id":"source-task-1"}}}'),('chain-source-2','default',2,1,'task.created',?,'owner','task','source-task-2',1,'{"type":"task.created","payload":{"task":{"task_id":"source-task-2"}}}')`, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	postAdmissions := func() {
		req := httptest.NewRequest(http.MethodPost, "/runner/v1/capability-triggers/admissions", nil)
		req.Header.Set("Authorization", "Bearer "+runnerToken)
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("trigger admission poll status=%d body=%s", resp.Code, resp.Body.String())
		}
	}
	postAdmissions()
	var admitted, queued int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM capability_trigger_runs WHERE mode='worker' AND state='running'`).Scan(&admitted); err != nil || admitted != 1 {
		t.Fatalf("trigger grant did not admit exactly one worker: count=%d err=%v", admitted, err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM capability_trigger_runs WHERE mode='worker' AND state='queued'`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("concurrency one did not retain second trigger queued: count=%d err=%v", queued, err)
	}

	runtime := &capabilityIntegrationRuntime{}
	journal, err := runner.OpenJournal(ctx, filepath.Join(t.TempDir(), "runner.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	engine := &runner.Engine{Control: controlClient, Runtime: runtime, Journal: journal, Routes: map[string]runner.Route{"worker-profile": {ProfileID: "worker-profile", RuntimeRouteID: "runtime", ModelRouteID: "model", IsolationPolicy: runner.OpenCodeWorkspaceWritePolicy, Harness: "opencode", Model: "fixture-model", ApprovalMode: "accept-edits"}}, Isolation: capabilityIntegrationIsolation{}, PollInterval: time.Millisecond}

	claim, err := controlClient.Claim(ctx)
	if err != nil || claim == nil || claim.Role != "worker" {
		t.Fatalf("ControlClient claim for admitted trigger: claim=%#v err=%v", claim, err)
	}
	if err := engine.Process(ctx, *claim); err != nil {
		t.Fatalf("runner Engine OpenCode execution: %v", err)
	}
	if runtime.created != 1 || runtime.terminated != 1 {
		t.Fatalf("OpenCode runtime lifecycle create=%d terminate=%d, want one each", runtime.created, runtime.terminated)
	}
	var runStatus, taskStatus string
	if err := store.DB().QueryRowContext(ctx, `SELECT r.status,t.status FROM runs r JOIN tasks t ON t.id=r.task_id WHERE r.id=?`, claim.RunID).Scan(&runStatus, &taskStatus); err != nil || runStatus != "verified" || taskStatus != "complete" {
		t.Fatalf("runner result did not complete automatically: run=%q task=%q err=%v", runStatus, taskStatus, err)
	}
	var toolEvidence int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM evidence WHERE run_id=? AND kind='mcp_tool'`, claim.RunID).Scan(&toolEvidence); err != nil || toolEvidence != 1 {
		t.Fatalf("runtime tool evidence was not persisted: count=%d err=%v", toolEvidence, err)
	}
	var triggerState string
	postAdmissions()
	if err = store.DB().QueryRowContext(ctx, `SELECT state FROM capability_trigger_runs WHERE task_id=?`, claim.TaskID).Scan(&triggerState); err != nil || triggerState != "completed" {
		t.Fatalf("completed result did not terminally release trigger: state=%q err=%v", triggerState, err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE source='service'`).Scan(&admitted); err != nil || admitted != 2 {
		t.Fatalf("automatic completion did not free worker slot for second trigger: tasks=%d err=%v", admitted, err)
	}
	// Replay the original terminal observation after slot reconciliation; it
	// must not emit another release decision or admit a third worker.
	var terminalJSON []byte
	if err = store.DB().QueryRowContext(ctx, `SELECT body FROM runner_observations WHERE run_id=? AND kind='runtime_terminated'`, claim.RunID).Scan(&terminalJSON); err != nil {
		t.Fatal(err)
	}
	var terminal contracts.RunnerObservation
	if err = json.Unmarshal(terminalJSON, &terminal); err != nil {
		t.Fatal(err)
	}
	if err = controlClient.Observe(ctx, claim.RunID, terminal); err != nil {
		t.Fatal(err)
	}
	postAdmissions()
	var decisions int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE event_type='task.result_decided' AND aggregate_id=?`, claim.TaskID).Scan(&decisions); err != nil || decisions != 1 {
		t.Fatalf("release decisions=%d err=%v", decisions, err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE source='service'`).Scan(&admitted); err != nil || admitted != 2 {
		t.Fatalf("replay admitted extra tasks=%d err=%v", admitted, err)
	}

}

func begin(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func insertCapabilityTestDevice(t *testing.T, store *storage.Store, deviceID, subjectID string) {
	t.Helper()
	if _, err := store.DB().ExecContext(context.Background(), `INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES(?, 'default', ?, 'capability test', 'approved', ?)`, deviceID, subjectID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

func asAPI(err error, target **control.APIError) bool {
	if err == nil {
		return false
	}
	if v, ok := err.(*control.APIError); ok {
		*target = v
		return true
	}
	return false
}

func marshal(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func mustCapabilityInstanceKey(t *testing.T) string {
	t.Helper()
	key, err := control.GenerateInstanceKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}
