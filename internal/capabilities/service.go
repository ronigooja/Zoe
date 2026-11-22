package capabilities

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ronigooja/Zoe/internal/limits"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

type Service struct {
	limits                 limits.Set
	db                     *sql.DB
	server                 *control.Server
	availableCapabilityIDs map[string]struct{}
	runtimeBackend         string
}

// Config is deployment-provided capability policy. An empty capability
// registry rejects every manifest capability. RuntimeBackend stays empty
// unless an independently configured runner adapter is available.
type Config struct {
	Limits                 limits.Set
	AvailableCapabilityIDs []string
	RuntimeBackend         string
}

const runtimeBackendDocker = "docker"

type submitReleasePayload struct {
	Manifest contracts.CapabilityManifest `json:"manifest"`
}
type deploymentRequestPayload struct {
	ServiceID     string `json:"service_id"`
	ReleaseDigest string `json:"release_digest"`
	Operation     string `json:"operation"`
	ConfigRef     string `json:"config_ref,omitempty"`
	ConfigSHA256  string `json:"config_sha256,omitempty"`
}
type authorizePayload struct {
	DeploymentID string                             `json:"deployment_id"`
	ScopeKind    string                             `json:"scope_kind"`
	ScopeID      string                             `json:"scope_id,omitempty"`
	Actions      []string                           `json:"actions"`
	Budget       contracts.CapabilityResourceBudget `json:"budget"`
	ExpiresAt    time.Time                          `json:"expires_at"`
}
type revokePayload struct {
	DeploymentID string `json:"deployment_id"`
}

func Register(server *control.Server, store *storage.Store, cfg Config) error {
	if server == nil || store == nil || store.DB() == nil {
		return errors.New("capability registration requires control server and storage")
	}
	runtimeBackend := strings.TrimSpace(cfg.RuntimeBackend)
	if runtimeBackend != "" && runtimeBackend != runtimeBackendDocker {
		return fmt.Errorf("unsupported capability runtime backend %q", runtimeBackend)
	}
	available := make(map[string]struct{}, len(cfg.AvailableCapabilityIDs))
	for _, capabilityID := range cfg.AvailableCapabilityIDs {
		capabilityID = strings.TrimSpace(capabilityID)
		if capabilityID == "" || strings.ContainsAny(capabilityID, "/\\*") {
			return errors.New("available capability IDs must be explicit opaque identifiers")
		}
		available[capabilityID] = struct{}{}
	}
	if err := RegisterMigrations(store); err != nil {
		return err
	}
	svc := &Service{limits: cfg.Limits, db: store.DB(), server: server, availableCapabilityIDs: available, runtimeBackend: runtimeBackend}
	if err := svc.registerTaskSource(); err != nil {
		return err
	}
	for kind, handler := range map[string]control.CommandHandler{
		"capability.release.submit":       svc.submitRelease,
		"capability.deployment.request":   svc.requestDeployment,
		"capability.deployment.authorize": svc.authorizeDeployment,
		"capability.deployment.revoke":    svc.revokeDeployment,
	} {
		if err := server.RegisterCommand(kind, handler); err != nil {
			return err
		}
	}
	if err := server.RegisterAuthenticatedRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /v1/capabilities/releases", svc.listReleases)
		mux.HandleFunc("GET /v1/capabilities/deployments", svc.listDeployments)
		mux.HandleFunc("GET /v1/capabilities/deployments/{deployment_id}", svc.getDeployment)
	}); err != nil {
		return err
	}
	return server.RegisterRunnerRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("POST /runner/v1/capability-triggers/admissions", svc.admitTriggerTasks)
		mux.HandleFunc("POST /runner/v1/deployments/claims", svc.claimDeployment)
		mux.HandleFunc("GET /runner/v1/deployments/claims/current", svc.currentDeploymentClaim)
		mux.HandleFunc("GET /runner/v1/deployments/claims/unresolved", svc.unresolvedDeploymentClaims)
		mux.HandleFunc("POST /runner/v1/deployments/{deployment_id}/observations", svc.observeDeployment)
		mux.HandleFunc("POST /runner/v1/capability-triggers/claims", svc.claimTrigger)
		mux.HandleFunc("GET /runner/v1/capability-triggers/claims/current", svc.currentTriggerClaim)
		mux.HandleFunc("POST /runner/v1/capability-triggers/{trigger_run_id}/observations", svc.observeTrigger)
	})
}

func (s *Service) submitRelease(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if !control.HasScope(cmd.Principal, "capability:release:submit") {
		return contracts.CommandResult{}, nil, control.NewAPIError(403, "forbidden", "capability release submit scope required", nil)
	}
	var p submitReleasePayload
	if err := decode(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if err := validateManifest(&p.Manifest, s.limits); err != nil {
		return contracts.CommandResult{}, nil, control.NewAPIError(400, "invalid_manifest", err.Error(), nil)
	}
	for _, capabilityID := range p.Manifest.Capabilities {
		if _, ok := s.availableCapabilityIDs[capabilityID]; !ok {
			return contracts.CommandResult{}, nil, control.NewAPIError(400, "capability_not_registered", "manifest requests a capability that is not enabled by the server registry", map[string]string{"capability_id": capabilityID})
		}
	}
	body, _ := json.Marshal(p.Manifest)
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	id := uuid.NewString()
	at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
	_, err := tx.ExecContext(ctx, `INSERT INTO capability_releases(release_id,workspace_id,digest,manifest,submitted_by,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(workspace_id,digest) DO NOTHING`, id, cmd.Principal.WorkspaceID, digest, body, cmd.Principal.SubjectID, at)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	var releaseID string
	if err = tx.QueryRowContext(ctx, `SELECT release_id FROM capability_releases WHERE workspace_id=? AND digest=?`, cmd.Principal.WorkspaceID, digest).Scan(&releaseID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	payload, _ := json.Marshal(map[string]string{"release_digest": digest, "release_id": releaseID})
	event := contracts.Event{Type: "capability.release.submitted", AggregateType: "capability_release", AggregateID: releaseID, AggregateVersion: 1, Payload: payload}
	return contracts.CommandResult{ResourceType: "capability_release", ResourceID: releaseID, ResourceVersion: 1}, []contracts.Event{event}, nil
}

func (s *Service) requestDeployment(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if !control.HasScope(cmd.Principal, "capability:deployment:request") {
		return contracts.CommandResult{}, nil, control.NewAPIError(403, "forbidden", "capability deployment request scope required", nil)
	}
	var p deploymentRequestPayload
	if err := decode(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if p.ServiceID == "" || len(p.ServiceID) > 200 || !validOperation(p.Operation) {
		return contracts.CommandResult{}, nil, control.NewAPIError(400, "invalid_request", "service_id and supported operation are required", nil)
	}
	var manifest []byte
	if err := tx.QueryRowContext(ctx, `SELECT manifest FROM capability_releases WHERE workspace_id=? AND digest=?`, cmd.Principal.WorkspaceID, p.ReleaseDigest).Scan(&manifest); errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, control.NewAPIError(404, "release_not_found", "release digest not found", nil)
	} else if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	id := uuid.NewString()
	at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
	var oldVersion int64
	err := tx.QueryRowContext(ctx, `SELECT version FROM capability_deployments WHERE workspace_id=? AND service_id=?`, cmd.Principal.WorkspaceID, p.ServiceID).Scan(&oldVersion)
	if err == nil {
		if req.ExpectedVersion == nil || *req.ExpectedVersion != oldVersion {
			return contracts.CommandResult{}, nil, control.NewAPIError(409, "version_conflict", "deployment version is stale", map[string]any{"current_version": oldVersion})
		}
		var deploymentID string
		if err := tx.QueryRowContext(ctx, `SELECT deployment_id FROM capability_deployments WHERE workspace_id=? AND service_id=?`, cmd.Principal.WorkspaceID, p.ServiceID).Scan(&deploymentID); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		var cleanupUnresolved int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM capability_deployment_jobs WHERE deployment_id=? AND cleanup_only=1 AND status IN ('pending','claimed','uncertain','failed')`, deploymentID).Scan(&cleanupUnresolved); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if cleanupUnresolved != 0 {
			return contracts.CommandResult{}, nil, control.NewAPIError(409, "deployment_cleanup_pending", "revoked deployment must be confirmed stopped before a new request", nil)
		}
		// Never create a new external operation while an earlier operation has an
		// unknown outcome. Pending work has not left the control plane and may be
		// safely superseded; claimed work must be reconciled first.
		var unresolved int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM capability_deployment_jobs WHERE deployment_id=? AND status IN ('claimed','uncertain')`, deploymentID).Scan(&unresolved); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if unresolved != 0 {
			return contracts.CommandResult{}, nil, control.NewAPIError(409, "deployment_outcome_unknown", "an earlier deployment effect must be reconciled before another operation", nil)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE capability_deployment_jobs SET status='superseded',superseded_by=?,updated_at=? WHERE deployment_id=? AND status='pending'`, id, at, deploymentID); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE capability_deployments SET release_digest=?,requested_operation=?,desired_state='awaiting_authorization',config_ref=?,config_sha256=?,revoked_at=NULL,revoked_by='',revoked_by_device_id=NULL,revocation_cleanup_state=NULL,version=version+1,updated_at=? WHERE deployment_id=? AND version=?`, p.ReleaseDigest, p.Operation, p.ConfigRef, p.ConfigSHA256, cmd.AcceptedAt.UTC().Format(time.RFC3339Nano), deploymentID, oldVersion)
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
		payload, _ := json.Marshal(map[string]string{"deployment_id": deploymentID, "service_id": p.ServiceID, "release_digest": p.ReleaseDigest, "state": "awaiting_authorization", "operation": p.Operation})
		event := contracts.Event{Type: "capability.deployment.requested", AggregateType: "capability_deployment", AggregateID: deploymentID, AggregateVersion: oldVersion + 1, Payload: payload}
		return contracts.CommandResult{ResourceType: "capability_deployment", ResourceID: deploymentID, ResourceVersion: oldVersion + 1}, []contracts.Event{event}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO capability_deployments(deployment_id,workspace_id,service_id,release_digest,requested_operation,desired_state,observed_state,config_ref,config_sha256,version,created_by,created_at,updated_at) VALUES(?,?,?,?,?,'awaiting_authorization','unknown',?,?,1,?,?,?)`, id, cmd.Principal.WorkspaceID, p.ServiceID, p.ReleaseDigest, p.Operation, p.ConfigRef, p.ConfigSHA256, cmd.Principal.SubjectID, at, at)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	payload, _ := json.Marshal(map[string]string{"deployment_id": id, "service_id": p.ServiceID, "release_digest": p.ReleaseDigest, "state": "awaiting_authorization"})
	event := contracts.Event{Type: "capability.deployment.requested", AggregateType: "capability_deployment", AggregateID: id, AggregateVersion: 1, Payload: payload}
	return contracts.CommandResult{ResourceType: "capability_deployment", ResourceID: id, ResourceVersion: 1}, []contracts.Event{event}, nil
}

func (s *Service) revokeDeployment(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if !control.HasScope(cmd.Principal, "capability:deployment:revoke") {
		return contracts.CommandResult{}, nil, control.NewAPIError(403, "forbidden", "capability deployment revoke scope required", nil)
	}
	var p revokePayload
	if err := decode(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if p.DeploymentID == "" {
		return contracts.CommandResult{}, nil, control.NewAPIError(400, "invalid_request", "deployment_id is required", nil)
	}
	if cmd.Principal.DeviceID == "" {
		return contracts.CommandResult{}, nil, control.NewAPIError(403, "device_required", "deployment revocation requires an authenticated device", nil)
	}
	var workspace, owner, grantID, digest, configRef, configHash string
	var revokedAt sql.NullString
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT workspace_id,created_by,grant_id,release_digest,config_ref,config_sha256,version,revoked_at FROM capability_deployments WHERE deployment_id=? AND workspace_id=?`, p.DeploymentID, cmd.Principal.WorkspaceID).Scan(&workspace, &owner, &grantID, &digest, &configRef, &configHash, &version, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, control.NewAPIError(404, "not_found", "deployment not found", nil)
	}
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if owner != cmd.Principal.SubjectID {
		return contracts.CommandResult{}, nil, control.NewAPIError(403, "deployment_owner_required", "only the deployment owner may revoke its grant", nil)
	}
	if revokedAt.Valid {
		return contracts.CommandResult{}, nil, control.NewAPIError(409, "deployment_already_revoked", "deployment grant has already been revoked", map[string]any{"current_version": version})
	}
	if req.ExpectedVersion == nil || *req.ExpectedVersion != version {
		return contracts.CommandResult{}, nil, control.NewAPIError(409, "version_conflict", "deployment version is stale", map[string]any{"current_version": version})
	}
	at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
	if grantID != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE capability_grants SET revoked_at=?,revoked_by=?,revoked_by_device_id=? WHERE grant_id=? AND deployment_id=? AND revoked_at IS NULL`, at, cmd.Principal.SubjectID, cmd.Principal.DeviceID, grantID, p.DeploymentID); err != nil {
			return contracts.CommandResult{}, nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE capability_trigger_definitions SET enabled=0,updated_at=? WHERE deployment_id=?`, at, p.DeploymentID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='canceled',updated_at=? WHERE deployment_id=? AND state='queued'`, at, p.DeploymentID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE capability_deployment_jobs SET status='superseded',superseded_by='grant-revoked',updated_at=? WHERE deployment_id=? AND status='pending' AND cleanup_only=0`, at, p.DeploymentID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if grantID != "" {
		if err = s.enqueueCleanup(ctx, tx, workspace, p.DeploymentID, grantID, digest, configRef, configHash, at); err != nil {
			return contracts.CommandResult{}, nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE capability_deployments SET desired_state='stopped',revoked_at=?,revoked_by=?,revoked_by_device_id=?,revocation_cleanup_state='pending',version=version+1,updated_at=? WHERE deployment_id=? AND version=?`, at, cmd.Principal.SubjectID, cmd.Principal.DeviceID, at, p.DeploymentID, version); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	payload, _ := json.Marshal(map[string]string{"deployment_id": p.DeploymentID, "grant_id": grantID, "desired_state": "stopped", "revoked_at": at})
	event := contracts.Event{Type: "capability.deployment.revoked", AggregateType: "capability_deployment", AggregateID: p.DeploymentID, AggregateVersion: version + 1, Payload: payload}
	return contracts.CommandResult{ResourceType: "capability_deployment", ResourceID: p.DeploymentID, ResourceVersion: version + 1}, []contracts.Event{event}, nil
}

func (s *Service) authorizeDeployment(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if !control.HasScope(cmd.Principal, "capability:deployment:authorize") {
		return contracts.CommandResult{}, nil, control.NewAPIError(403, "forbidden", "capability deployment authorize scope required", nil)
	}
	var p authorizePayload
	if err := decode(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if p.DeploymentID == "" || !validScope(p.ScopeKind, p.ScopeID) || !p.ExpiresAt.After(cmd.AcceptedAt) || len(p.Actions) == 0 || !budgetPositive(p.Budget) {
		return contracts.CommandResult{}, nil, control.NewAPIError(400, "invalid_grant", "grant scope, actions, positive budget, and future expiry are required", nil)
	}
	var workspace, digest, op, state, configRef, configHash string
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT workspace_id,release_digest,requested_operation,desired_state,config_ref,config_sha256,version FROM capability_deployments WHERE deployment_id=? AND workspace_id=?`, p.DeploymentID, cmd.Principal.WorkspaceID).Scan(&workspace, &digest, &op, &state, &configRef, &configHash, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, control.NewAPIError(404, "not_found", "deployment not found", nil)
	}
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if state != "awaiting_authorization" || req.ExpectedVersion == nil || *req.ExpectedVersion != version {
		return contracts.CommandResult{}, nil, control.NewAPIError(409, "deployment_state_conflict", "deployment is not awaiting initial authorization", nil)
	}
	if !contains(p.Actions, op) {
		return contracts.CommandResult{}, nil, control.NewAPIError(400, "grant_missing_action", "grant must explicitly authorize the requested operation", nil)
	}
	var manifest contracts.CapabilityManifest
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT manifest FROM capability_releases WHERE workspace_id=? AND digest=?`, workspace, digest).Scan(&raw); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if !budgetFits(p.Budget, manifest.Budget) {
		return contracts.CommandResult{}, nil, control.NewAPIError(400, "grant_exceeds_manifest", "grant budget cannot exceed immutable manifest budget", nil)
	}
	if p.ScopeKind == "project" {
		allowed, e := control.PrincipalCanWriteProject(ctx, tx, cmd.Principal, p.ScopeID)
		if e != nil {
			return contracts.CommandResult{}, nil, e
		}
		if !allowed {
			return contracts.CommandResult{}, nil, control.NewAPIError(403, "grant_scope_forbidden", "principal cannot authorize this project scope", nil)
		}
	}
	if p.ScopeKind == "task" {
		allowed, e := control.PrincipalCanWriteTask(ctx, tx, cmd.Principal, p.ScopeID)
		if e != nil {
			return contracts.CommandResult{}, nil, e
		}
		if !allowed {
			return contracts.CommandResult{}, nil, control.NewAPIError(403, "grant_scope_forbidden", "principal cannot authorize this task scope", nil)
		}
	}
	if cmd.Principal.DeviceID == "" {
		return contracts.CommandResult{}, nil, control.NewAPIError(403, "device_required", "grant authorization requires an authenticated device", nil)
	}
	grantID := uuid.NewString()
	at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
	actions, _ := json.Marshal(uniqueSorted(p.Actions))
	budget, _ := json.Marshal(p.Budget)
	principalID := ""
	if p.ScopeKind == "personal" {
		principalID = cmd.Principal.SubjectID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO capability_grants(grant_id,workspace_id,deployment_id,release_digest,scope_kind,scope_id,principal_id,actions,budget,expires_at,authorized_by,authorized_by_device_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, grantID, workspace, p.DeploymentID, digest, p.ScopeKind, p.ScopeID, principalID, actions, budget, p.ExpiresAt.UTC().Format(time.RFC3339Nano), cmd.Principal.SubjectID, cmd.Principal.DeviceID, at)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE capability_trigger_definitions SET enabled=0,updated_at=? WHERE deployment_id=? AND enabled=1`, at, p.DeploymentID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='canceled',updated_at=? WHERE deployment_id=? AND state='queued'`, at, p.DeploymentID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if err = insertTriggerDefinitions(ctx, tx, workspace, p.DeploymentID, digest, grantID, version+1, configRef, configHash, manifest.Triggers, cmd.AcceptedAt.UTC()); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if err = s.enqueue(ctx, tx, workspace, p.DeploymentID, op, grantID, digest, configRef, configHash, at); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE capability_deployments SET grant_id=?,desired_state=?,version=version+1,updated_at=? WHERE deployment_id=? AND version=?`, grantID, desiredFor(op), at, p.DeploymentID, version)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	payload, _ := json.Marshal(map[string]string{"deployment_id": p.DeploymentID, "grant_id": grantID, "desired_state": desiredFor(op)})
	event := contracts.Event{Type: "capability.deployment.authorized", AggregateType: "capability_deployment", AggregateID: p.DeploymentID, AggregateVersion: version + 1, Payload: payload}
	return contracts.CommandResult{ResourceType: "capability_deployment", ResourceID: p.DeploymentID, ResourceVersion: version + 1}, []contracts.Event{event}, nil
}

func (s *Service) enqueue(ctx context.Context, tx *sql.Tx, workspace, deployment, op, grant, digest, configRef, configHash, at string) error {
	var fence int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(fence),0)+1 FROM capability_deployment_jobs WHERE deployment_id=?`, deployment).Scan(&fence); err != nil {
		return err
	}
	attempt := uuid.NewString()
	var serviceID string
	if err := tx.QueryRowContext(ctx, `SELECT service_id FROM capability_deployments WHERE deployment_id=? AND workspace_id=?`, deployment, workspace).Scan(&serviceID); err != nil {
		return err
	}
	var rawManifest, rawActions, rawBudget []byte
	var scopeKind, scopeID, principalID, expiresAt string
	if err := tx.QueryRowContext(ctx, `SELECT r.manifest,g.scope_kind,g.scope_id,g.principal_id,g.actions,g.budget,g.expires_at FROM capability_releases r JOIN capability_grants g ON g.release_digest=r.digest WHERE r.workspace_id=? AND r.digest=? AND g.grant_id=? AND g.deployment_id=?`, workspace, digest, grant, deployment).Scan(&rawManifest, &scopeKind, &scopeID, &principalID, &rawActions, &rawBudget, &expiresAt); err != nil {
		return err
	}
	var manifest contracts.CapabilityManifest
	var actions []string
	var budget contracts.CapabilityResourceBudget
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return err
	}
	if err := json.Unmarshal(rawActions, &actions); err != nil {
		return err
	}
	if err := json.Unmarshal(rawBudget, &budget); err != nil {
		return err
	}
	expiry, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return err
	}
	claim := contracts.DeploymentClaim{WorkspaceID: workspace, DeploymentID: deployment, AttemptID: attempt, Fence: fence, ServiceID: serviceID, ReleaseDigest: digest, Manifest: manifest, DesiredOp: op, Grant: contracts.CapabilityGrant{GrantID: grant, ScopeKind: scopeKind, ScopeID: scopeID, PrincipalID: principalID, Actions: actions, Budget: budget, ExpiresAt: expiry}, ConfigRef: configRef, ConfigSHA256: configHash, RuntimeBackend: s.runtimeBackend}
	for _, mount := range manifest.Mounts {
		claim.VolumeIDs = append(claim.VolumeIDs, mount.VolumeID)
	}
	claimJSON, err := json.Marshal(claim)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO capability_deployment_jobs(attempt_id,workspace_id,deployment_id,fence,operation,status,claim_json,created_at,updated_at) VALUES(?,?,?,?,?,'pending',?,?,?)`, attempt, workspace, deployment, fence, op, claimJSON, at, at)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE capability_deployments SET latest_attempt_id=?,latest_fence=?,updated_at=? WHERE deployment_id=?`, attempt, fence, at, deployment)
	return err
}

func (s *Service) enqueueCleanup(ctx context.Context, tx *sql.Tx, workspace, deployment, grant, digest, configRef, configHash, at string) error {
	var serviceID string
	if err := tx.QueryRowContext(ctx, `SELECT service_id FROM capability_deployments WHERE deployment_id=? AND workspace_id=?`, deployment, workspace).Scan(&serviceID); err != nil {
		return err
	}
	var rawManifest, rawActions, rawBudget []byte
	var scopeKind, scopeID, principalID, expiresAt string
	if err := tx.QueryRowContext(ctx, `SELECT r.manifest,g.scope_kind,g.scope_id,g.principal_id,g.actions,g.budget,g.expires_at FROM capability_releases r JOIN capability_grants g ON g.release_digest=r.digest WHERE r.workspace_id=? AND r.digest=? AND g.grant_id=? AND g.deployment_id=?`, workspace, digest, grant, deployment).Scan(&rawManifest, &scopeKind, &scopeID, &principalID, &rawActions, &rawBudget, &expiresAt); err != nil {
		return err
	}
	var manifest contracts.CapabilityManifest
	var actions []string
	var budget contracts.CapabilityResourceBudget
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return err
	}
	if err := json.Unmarshal(rawActions, &actions); err != nil {
		return err
	}
	if err := json.Unmarshal(rawBudget, &budget); err != nil {
		return err
	}
	expiry, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return err
	}
	var fence int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(fence),0)+1 FROM capability_deployment_jobs WHERE deployment_id=?`, deployment).Scan(&fence); err != nil {
		return err
	}
	attempt := uuid.NewString()
	claim := contracts.DeploymentClaim{WorkspaceID: workspace, DeploymentID: deployment, AttemptID: attempt, Fence: fence, ServiceID: serviceID, ReleaseDigest: digest, Manifest: manifest, DesiredOp: "stop", Grant: contracts.CapabilityGrant{GrantID: grant, ScopeKind: scopeKind, ScopeID: scopeID, PrincipalID: principalID, Actions: actions, Budget: budget, ExpiresAt: expiry}, ConfigRef: configRef, ConfigSHA256: configHash, RuntimeBackend: s.runtimeBackend, CleanupOnly: true}
	for _, mount := range manifest.Mounts {
		claim.VolumeIDs = append(claim.VolumeIDs, mount.VolumeID)
	}
	claimJSON, err := json.Marshal(claim)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO capability_deployment_jobs(attempt_id,workspace_id,deployment_id,fence,operation,status,claim_json,created_at,updated_at,cleanup_only) VALUES(?,?,?,?,?,'pending',?,?,?,1)`, attempt, workspace, deployment, fence, "stop", claimJSON, at, at); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE capability_deployments SET latest_attempt_id=?,latest_fence=?,updated_at=? WHERE deployment_id=?`, attempt, fence, at, deployment)
	return err
}

func (s *Service) claimDeployment(w http.ResponseWriter, r *http.Request) {
	if s.runtimeBackend != runtimeBackendDocker {
		control.WriteError(w, control.NewAPIError(503, "capability_runner_unavailable", "no capability runtime adapter is configured", nil))
		return
	}
	if _, ok := control.RunnerPrincipalFromContext(r.Context()); !ok {
		control.WriteError(w, control.NewAPIError(401, "unauthenticated", "runner authentication required", nil))
		return
	}
	var claim *contracts.DeploymentClaim
	err := s.server.RunnerTransaction(r.Context(), func(tx *sql.Tx, runner contracts.RunnerPrincipal) error {
		if !runnerHasCapabilityScope(runner, "capability:deploy") {
			return control.NewAPIError(403, "forbidden", "capability:deploy runner scope required", nil)
		}
		current, err := s.activeDeploymentClaim(r.Context(), tx, runner)
		if err != nil {
			return err
		}
		if current != nil && current.ClaimState != "uncertain" && (current.CleanupOnly || current.DesiredControl != "stopped") {
			claim = current
			return nil
		}
		rows, err := tx.QueryContext(r.Context(), `SELECT j.attempt_id,j.fence,j.operation,j.deployment_id,j.workspace_id,j.claim_json,j.cleanup_only,d.revoked_at FROM capability_deployment_jobs j JOIN capability_deployments d ON d.deployment_id=j.deployment_id WHERE j.status='pending' AND j.fence=d.latest_fence ORDER BY j.created_at LIMIT ?`, s.limits.Int("capability_deployment_page_rows"))
		if err != nil {
			return err
		}
		type candidate struct {
			attempt, op, deployment, workspace string
			fence                              int64
			claimJSON                          []byte
			cleanupOnly                        int
			revokedAt                          sql.NullString
		}
		var items []candidate
		for rows.Next() {
			var x candidate
			if err := rows.Scan(&x.attempt, &x.fence, &x.op, &x.deployment, &x.workspace, &x.claimJSON, &x.cleanupOnly, &x.revokedAt); err != nil {
				rows.Close()
				return err
			}
			items = append(items, x)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, x := range items {
			if !control.HasRunnerScope(runner, "capability:deploy") {
				continue
			}
			var snapshot contracts.DeploymentClaim
			if err := json.Unmarshal(x.claimJSON, &snapshot); err != nil {
				return err
			}
			if err := validateManifest(&snapshot.Manifest, s.limits); err != nil {
				return control.NewAPIError(500, "invalid_claim_snapshot", "deployment claim manifest failed immutable validation", nil)
			}
			if snapshot.WorkspaceID != x.workspace || snapshot.DeploymentID != x.deployment || snapshot.AttemptID != x.attempt || snapshot.Fence != x.fence || snapshot.DesiredOp != x.op || snapshot.CleanupOnly != (x.cleanupOnly == 1) {
				continue
			}
			if snapshot.CleanupOnly {
				if x.op != "stop" || !x.revokedAt.Valid {
					continue
				}
			} else if x.revokedAt.Valid || !snapshot.Grant.ExpiresAt.After(time.Now().UTC()) || !contains(snapshot.Grant.Actions, snapshot.DesiredOp) {
				continue
			}
			res, err := tx.ExecContext(r.Context(), `UPDATE capability_deployment_jobs SET status='claimed',runner_id=?,claimed_at=?,updated_at=? WHERE attempt_id=? AND fence=? AND status='pending'`, runner.RunnerID, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), x.attempt, x.fence)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				continue
			}
			_, err = tx.ExecContext(r.Context(), `UPDATE capability_deployments SET observed_state='pending',updated_at=? WHERE deployment_id=? AND latest_fence=? AND (revoked_at IS NULL OR ?=1)`, time.Now().UTC().Format(time.RFC3339Nano), x.deployment, x.fence, x.cleanupOnly)
			if err != nil {
				return err
			}
			snapshot.VolumeIDs = uniqueSorted(snapshot.VolumeIDs)
			snapshot.ClaimState = "claimed"
			if err := tx.QueryRowContext(r.Context(), `SELECT desired_state FROM capability_deployments WHERE deployment_id=?`, x.deployment).Scan(&snapshot.DesiredControl); err != nil {
				return err
			}
			claim = &snapshot
			break
		}
		return nil
	})
	if err != nil {
		control.WriteError(w, err)
		return
	}
	if claim == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, claim)
}

func (s *Service) currentDeploymentClaim(w http.ResponseWriter, r *http.Request) {
	runner, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok || !runnerHasCapabilityScope(runner, "capability:deploy") {
		control.WriteError(w, control.NewAPIError(401, "unauthenticated", "capability:deploy runner authentication required", nil))
		return
	}
	var claim *contracts.DeploymentClaim
	err := s.server.RunnerTransaction(r.Context(), func(tx *sql.Tx, runner contracts.RunnerPrincipal) error {
		var err error
		claim, err = s.activeDeploymentClaim(r.Context(), tx, runner)
		return err
	})
	if err != nil {
		control.WriteError(w, err)
		return
	}
	if claim == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, claim)
}

func (s *Service) unresolvedDeploymentClaims(w http.ResponseWriter, r *http.Request) {
	runner, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok || !runnerHasCapabilityScope(runner, "capability:deploy") {
		control.WriteError(w, control.NewAPIError(401, "unauthenticated", "capability:deploy runner authentication required", nil))
		return
	}
	var claims []contracts.DeploymentClaim
	err := s.server.RunnerTransaction(r.Context(), func(tx *sql.Tx, runner contracts.RunnerPrincipal) error {
		var err error
		claims, err = s.loadUnresolvedDeploymentClaims(r.Context(), tx, runner)
		return err
	})
	if err != nil {
		control.WriteError(w, err)
		return
	}
	if claims == nil {
		claims = []contracts.DeploymentClaim{}
	}
	writeJSON(w, http.StatusOK, claims)
}

func (s *Service) activeDeploymentClaim(ctx context.Context, tx *sql.Tx, runner contracts.RunnerPrincipal) (*contracts.DeploymentClaim, error) {
	claims, err := s.loadUnresolvedDeploymentClaims(ctx, tx, runner)
	if err != nil || len(claims) == 0 {
		return nil, err
	}
	return &claims[0], nil
}

func (s *Service) loadUnresolvedDeploymentClaims(ctx context.Context, tx *sql.Tx, runner contracts.RunnerPrincipal) ([]contracts.DeploymentClaim, error) {
	rows, err := tx.QueryContext(ctx, `SELECT j.claim_json,j.status,d.desired_state,j.workspace_id FROM capability_deployment_jobs j JOIN capability_deployments d ON d.deployment_id=j.deployment_id WHERE j.runner_id=? AND j.status IN ('claimed','uncertain') ORDER BY j.claimed_at ASC`, runner.RunnerID)
	if err != nil {
		return nil, err
	}
	type active struct {
		body                      []byte
		state, desired, workspace string
	}
	var claims []contracts.DeploymentClaim
	for rows.Next() {
		var item active
		if err := rows.Scan(&item.body, &item.state, &item.desired, &item.workspace); err != nil {
			rows.Close()
			return nil, err
		}
		if !control.HasRunnerScope(runner, "capability:deploy") {
			continue
		}
		var claim contracts.DeploymentClaim
		if err := json.Unmarshal(item.body, &claim); err != nil {
			rows.Close()
			return nil, err
		}
		if err := validateManifest(&claim.Manifest, s.limits); err != nil {
			rows.Close()
			return nil, control.NewAPIError(500, "invalid_claim_snapshot", "deployment claim manifest failed immutable validation", nil)
		}
		claim.ClaimState = item.state
		claim.DesiredControl = item.desired
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return claims, nil
}

func (s *Service) observeDeployment(w http.ResponseWriter, r *http.Request) {
	var obs contracts.DeploymentObservation
	if err := decodeRequest(r, &obs); err != nil {
		control.WriteError(w, control.NewAPIError(400, "invalid_request", "observation JSON is invalid", nil))
		return
	}
	if obs.DeploymentID != r.PathValue("deployment_id") || obs.AttemptID == "" || obs.Fence < 1 || obs.ObservationID == "" || obs.ObservedAt.IsZero() {
		control.WriteError(w, control.NewAPIError(400, "invalid_observation", "fenced deployment observation is incomplete", nil))
		return
	}
	err := s.server.RunnerTransaction(r.Context(), func(tx *sql.Tx, runner contracts.RunnerPrincipal) error {
		var workspace, jobStatus, operation, claimedRunner string
		var cleanupOnly int
		var revokedAt sql.NullString
		var fence int64
		if err := tx.QueryRowContext(r.Context(), `SELECT j.workspace_id,j.status,j.operation,j.fence,j.runner_id,j.cleanup_only,d.revoked_at FROM capability_deployment_jobs j JOIN capability_deployments d ON d.deployment_id=j.deployment_id WHERE j.attempt_id=? AND j.deployment_id=?`, obs.AttemptID, obs.DeploymentID).Scan(&workspace, &jobStatus, &operation, &fence, &claimedRunner, &cleanupOnly, &revokedAt); errors.Is(err, sql.ErrNoRows) {
			return control.NewAPIError(404, "deployment_attempt_not_found", "deployment attempt not found", nil)
		} else if err != nil {
			return err
		}
		if !control.HasRunnerScope(runner, "capability:deploy") {
			return control.NewAPIError(403, "forbidden", "runner cannot report this workspace", nil)
		}
		if runner.RunnerID != claimedRunner {
			return control.NewAPIError(403, "runner_mismatch", "only the runner that claimed this attempt may report it", nil)
		}
		if fence != obs.Fence || operation != obs.Operation {
			return control.NewAPIError(409, "stale_deployment_fence", "observation does not match active authorized attempt", nil)
		}
		if !validOperationObservedState(operation, obs.State) {
			return control.NewAPIError(400, "invalid_observation", "observed state is not valid for this operation", nil)
		}
		if obs.State == "running" && (obs.Health.State != "healthy" || obs.Health.CheckedAt.IsZero() || obs.Health.EvidenceRef == "") {
			return control.NewAPIError(400, "health_evidence_required", "running state requires timestamped healthy evidence", nil)
		}
		if (operation == "stop" && obs.State == "stopped") || (operation == "uninstall" && obs.State == "removed") {
			if obs.TerminationProof == nil || obs.TerminationProof.EffectRef == "" || obs.TerminationProof.TerminatedAt.IsZero() || !obs.TerminationProof.CleanupComplete || !contains(obs.ExternalEffectRef, obs.TerminationProof.EffectRef) {
				return control.NewAPIError(400, "termination_proof_required", "stop/removal requires runner termination proof", nil)
			}
		}
		body, _ := json.Marshal(obs)
		var prior []byte
		if err := tx.QueryRowContext(r.Context(), `SELECT body FROM capability_observations WHERE observation_id=?`, obs.ObservationID).Scan(&prior); err == nil {
			if string(prior) == string(body) {
				return nil
			}
			return control.NewAPIError(409, "observation_conflict", "observation ID was reused with different content", nil)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// An exact durable receipt replay remains acknowledgeable even if later
		// control-plane state has revoked or replaced the claim's authorization.
		if cleanupOnly == 1 && (!revokedAt.Valid || operation != "stop") {
			return control.NewAPIError(409, "cleanup_claim_invalid", "cleanup claim is no longer authorized", nil)
		}
		if cleanupOnly == 1 && obs.State == "failed" {
			return control.NewAPIError(400, "cleanup_must_reconcile", "cleanup failures must remain uncertain until stop is proven", nil)
		}
		if jobStatus != "claimed" && jobStatus != "uncertain" {
			return control.NewAPIError(409, "stale_deployment_fence", "observation does not match active authorized attempt", nil)
		}
		_, err := tx.ExecContext(r.Context(), `INSERT INTO capability_observations(observation_id,workspace_id,deployment_id,attempt_id,fence,body,observed_at) VALUES(?,?,?,?,?,?,?)`, obs.ObservationID, workspace, obs.DeploymentID, obs.AttemptID, obs.Fence, body, obs.ObservedAt.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		state := mapObserved(obs.State)
		status := jobStatus
		if isTerminalObservation(operation, state) {
			switch state {
			case "uncertain":
				status = "uncertain"
			case "failed":
				status = "failed"
			default:
				status = "observed"
			}
		}
		if _, err = tx.ExecContext(r.Context(), `UPDATE capability_deployment_jobs SET status=?,updated_at=? WHERE attempt_id=? AND fence=?`, status, obs.ObservedAt.UTC().Format(time.RFC3339Nano), obs.AttemptID, obs.Fence); err != nil {
			return err
		}
		_, err = tx.ExecContext(r.Context(), `UPDATE capability_deployments SET observed_state=?,last_error_code=?,updated_at=? WHERE deployment_id=? AND latest_fence=? AND (revoked_at IS NULL OR ?=1)`, state, obs.ErrorCode, obs.ObservedAt.UTC().Format(time.RFC3339Nano), obs.DeploymentID, obs.Fence, cleanupOnly)
		if err != nil {
			return err
		}
		if cleanupOnly == 1 {
			cleanupState := "uncertain"
			if state == "stopped" {
				cleanupState = "confirmed"
			}
			if _, err = tx.ExecContext(r.Context(), `UPDATE capability_deployments SET revocation_cleanup_state=? WHERE deployment_id=? AND revoked_at IS NOT NULL AND latest_fence=?`, cleanupState, obs.DeploymentID, obs.Fence); err != nil {
				return err
			}
			if cleanupState == "confirmed" {
				if _, err = tx.ExecContext(r.Context(), `UPDATE capability_deployment_jobs SET status='superseded',superseded_by=?,updated_at=? WHERE deployment_id=? AND attempt_id<>? AND fence<? AND status IN ('claimed','uncertain')`, obs.AttemptID, obs.ObservedAt.UTC().Format(time.RFC3339Nano), obs.DeploymentID, obs.AttemptID, obs.Fence); err != nil {
					return err
				}
			}
		}
		if isTerminalObservation(operation, state) && (cleanupOnly == 1 || !revokedAt.Valid) {
			desired := desiredStateForObservation(state)
			if desired != "" {
				if _, err := tx.ExecContext(r.Context(), `UPDATE capability_deployments SET desired_state=? WHERE deployment_id=? AND latest_fence=?`, desired, obs.DeploymentID, obs.Fence); err != nil {
					return err
				}
			}
			if cleanupOnly == 1 || revokedAt.Valid {
				return nil
			}
			return s.enableDeploymentTriggers(r.Context(), tx, obs.DeploymentID, state == "running", obs.ObservedAt)
		}
		return nil
	})
	if err != nil {
		control.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (s *Service) listReleases(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok || !control.HasScope(p, "capability:read") {
		control.WriteError(w, control.NewAPIError(403, "forbidden", "capability:read scope required", nil))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT release_id,digest,manifest,created_at FROM capability_releases WHERE workspace_id=? ORDER BY created_at DESC`, p.WorkspaceID)
	if err != nil {
		control.WriteError(w, err)
		return
	}
	defer rows.Close()
	items := []contracts.CapabilityRelease{}
	for rows.Next() {
		var v contracts.CapabilityRelease
		var raw []byte
		var at string
		if err := rows.Scan(&v.ReleaseID, &v.Digest, &raw, &at); err != nil {
			control.WriteError(w, err)
			return
		}
		_ = json.Unmarshal(raw, &v.Manifest)
		v.CreatedAt, _ = time.Parse(time.RFC3339Nano, at)
		items = append(items, v)
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Service) listDeployments(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok || !control.HasScope(p, "capability:read") {
		control.WriteError(w, control.NewAPIError(403, "forbidden", "capability:read scope required", nil))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT deployment_id,workspace_id,service_id,release_digest,requested_operation,desired_state,observed_state,grant_id,config_ref,config_sha256,version,created_by,created_at,updated_at,latest_attempt_id,latest_fence,last_error_code,revoked_at,revoked_by,revocation_cleanup_state FROM capability_deployments WHERE workspace_id=? ORDER BY updated_at DESC`, p.WorkspaceID)
	if err != nil {
		control.WriteError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		v, err := scanDeployment(rows)
		if err != nil {
			control.WriteError(w, err)
			return
		}
		items = append(items, v)
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Service) getDeployment(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok || !control.HasScope(p, "capability:read") {
		control.WriteError(w, control.NewAPIError(403, "forbidden", "capability:read scope required", nil))
		return
	}
	row := s.db.QueryRowContext(r.Context(), `SELECT deployment_id,workspace_id,service_id,release_digest,requested_operation,desired_state,observed_state,grant_id,config_ref,config_sha256,version,created_by,created_at,updated_at,latest_attempt_id,latest_fence,last_error_code,revoked_at,revoked_by,revocation_cleanup_state FROM capability_deployments WHERE workspace_id=? AND deployment_id=?`, p.WorkspaceID, r.PathValue("deployment_id"))
	v, err := scanDeployment(row)
	if err != nil {
		control.WriteError(w, control.NewAPIError(404, "not_found", "deployment not found", nil))
		return
	}
	writeJSON(w, 200, v)
}

type scanner interface{ Scan(...any) error }

func scanDeployment(row scanner) (map[string]any, error) {
	var id, ws, service, digest, op, desired, observed, grant, configRef, configHash, createdBy, created, updated, attempt, lastError, revokedBy string
	var revokedAt, cleanupState sql.NullString
	var version, fence int64
	err := row.Scan(&id, &ws, &service, &digest, &op, &desired, &observed, &grant, &configRef, &configHash, &version, &createdBy, &created, &updated, &attempt, &fence, &lastError, &revokedAt, &revokedBy, &cleanupState)
	if err != nil {
		return nil, err
	}
	return map[string]any{"deployment_id": id, "workspace_id": ws, "service_id": service, "release_digest": digest, "requested_operation": op, "desired_state": desired, "observed_state": observed, "grant_id": grant, "config_ref": configRef, "config_sha256": configHash, "version": version, "created_by": createdBy, "created_at": created, "updated_at": updated, "latest_attempt_id": attempt, "latest_fence": fence, "last_error_code": lastError, "revoked_at": nullableText(revokedAt), "revoked_by": nullableString(revokedBy), "revocation_cleanup_state": nullableText(cleanupState)}, nil
}

func nullableText(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func validateManifest(m *contracts.CapabilityManifest, sets ...limits.Set) error {
	l := limits.Resolve(sets)
	if m.SchemaVersion != 1 || !strings.HasPrefix(m.ArtifactRef, "oci://") && !strings.HasPrefix(m.ArtifactRef, "https://") || !validDigest(m.ArtifactDigest) || !strings.Contains(m.ArtifactRef, "@"+m.ArtifactDigest) || strings.TrimSpace(m.Entrypoint) == "" || len(m.Entrypoint) > l.Int("capability_manifest_entrypoint_max_bytes") || len(m.Args) > l.Int("capability_manifest_max_args") || len(m.Capabilities) == 0 || len(m.Capabilities) > l.Int("capability_manifest_max_capabilities") || len(m.Mounts) > l.Int("capability_manifest_max_mounts") || len(m.Triggers) > l.Int("capability_manifest_max_triggers") || !budgetPositive(m.Budget) || m.HealthCheck == nil {
		return errors.New("schema version, content-addressed artifact, entrypoint, capability IDs, health check, and positive resource budget are required")
	}
	for _, arg := range m.Args {
		if len(arg) > l.Int("capability_manifest_arg_max_bytes") || strings.ContainsAny(arg, "\x00\r\n") {
			return errors.New("entrypoint arguments exceed server limits")
		}
	}
	if m.Network.Mode != "none" && m.Network.Mode != "egress_allowlist" {
		return errors.New("network mode must be none or egress_allowlist")
	}
	if m.Network.Mode == "none" && len(m.Network.AllowedHosts) > 0 {
		return errors.New("allowed hosts require egress_allowlist mode")
	}
	if m.Budget.CPUMillis > 64_000 || m.Budget.MemoryBytes > 1<<40 || m.Budget.DiskBytes > 10*(1<<40) || m.Budget.TimeoutSeconds > 31_536_000 || m.Budget.Concurrency > 128 {
		return errors.New("resource budget exceeds server hard limits")
	}
	for _, h := range m.Network.AllowedHosts {
		if strings.ContainsAny(h, "/:@ ") {
			return errors.New("allowed hosts must be bare hostnames")
		}
	}
	for _, mount := range m.Mounts {
		if mount.VolumeID == "" || !strings.HasPrefix(mount.Target, "/") || strings.Contains(mount.Target, "..") || mount.Target == "/" {
			return errors.New("mounts require named volume IDs and safe absolute targets")
		}
	}
	if m.HealthCheck != nil {
		if err := validateHealthCheck(*m.HealthCheck); err != nil {
			return err
		}
	}
	for k, v := range m.SecretRefs {
		if k == "" || !strings.HasPrefix(v, "secret://") || strings.ContainsAny(v, "= \t\r\n") {
			return errors.New("secret references must be named references, not values")
		}
	}
	for _, trigger := range m.Triggers {
		if err := validateTrigger(trigger, m.Capabilities, l); err != nil {
			return err
		}
	}
	for _, id := range m.Capabilities {
		if id == "" || strings.ContainsAny(id, "/\\") {
			return errors.New("capability IDs must be opaque names")
		}
	}
	m.Capabilities = uniqueSorted(m.Capabilities)
	sort.Slice(m.Mounts, func(i, j int) bool { return m.Mounts[i].Target < m.Mounts[j].Target })
	sort.Slice(m.Triggers, func(i, j int) bool {
		return m.Triggers[i].Kind+"\x00"+m.Triggers[i].Source < m.Triggers[j].Kind+"\x00"+m.Triggers[j].Source
	})
	canonical, err := json.Marshal(m)
	if err != nil || len(canonical) > l.Int("capability_manifest_max_bytes") {
		return errors.New("manifest exceeds the admission byte limit")
	}
	return nil
}

func validateHealthCheck(check contracts.CapabilityHealthCheck) error {
	if check.IntervalSeconds < 5 || check.IntervalSeconds > 300 || check.TimeoutSeconds < 1 || check.TimeoutSeconds > 30 || check.FailureThreshold < 1 || check.FailureThreshold > 10 {
		return errors.New("health check interval, timeout, or failure threshold exceeds server bounds")
	}
	switch check.Kind {
	case "http":
		if !strings.HasPrefix(check.Target, "/") || strings.HasPrefix(check.Target, "//") || len(check.Target) > 1024 || strings.ContainsAny(check.Target, "\\\r\n\x00") || check.Port < 1 || check.Port > 65535 || len(check.Args) != 0 {
			return errors.New("HTTP health check requires a safe path and valid port")
		}
	case "tcp":
		if check.Port < 1 || check.Port > 65535 || check.Target != "" || len(check.Args) != 0 {
			return errors.New("TCP health check requires only a valid port")
		}
	case "exec":
		if !validExecutableName(check.Target) || check.Port != 0 || len(check.Args) > 32 {
			return errors.New("exec health check requires a bare executable name and at most 32 arguments")
		}
		for _, arg := range check.Args {
			if len(arg) > 1024 || strings.ContainsAny(arg, "\x00\r\n") {
				return errors.New("exec health check arguments contain invalid values")
			}
		}
	default:
		return errors.New("health check kind must be http, tcp, or exec")
	}
	return nil
}

func validExecutableName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._+-", r)) {
			return false
		}
	}
	return true
}

func validateTrigger(trigger contracts.CapabilityTrigger, capabilityIDs []string, l limits.Set) error {
	switch trigger.Mode {
	case "program", "worker", "super":
	default:
		return errors.New("trigger mode must be program, worker, or super")
	}
	if trigger.FilterJSON != "" {
		return errors.New("untyped trigger filters are not accepted")
	}
	if len(trigger.Scopes) > l.Int("capability_trigger_max_scopes") {
		return errors.New("trigger scope list exceeds limits")
	}
	if len(trigger.Filters) > l.Int("capability_trigger_max_filters") {
		return errors.New("trigger filter list exceeds limits")
	}
	for _, scope := range trigger.Scopes {
		if !contains(capabilityIDs, scope) {
			return fmt.Errorf("trigger scope %q is not granted by the manifest", scope)
		}
	}
	for _, filter := range trigger.Filters {
		if !validTriggerFilter(filter, l.Int("capability_filter_value_max_bytes")) {
			return fmt.Errorf("trigger filter %q/%q is unsupported", filter.Field, filter.Operator)
		}
	}
	switch trigger.Kind {
	case "event":
		if _, ok := supportedEventTypes[trigger.Source]; !ok || trigger.IntervalSeconds != 0 || trigger.MissedRunPolicy != "" {
			return errors.New("event trigger source or schedule fields are unsupported")
		}
	case "schedule":
		if trigger.Source != "schedule.interval" || trigger.IntervalSeconds < 60 || trigger.IntervalSeconds > 7*24*60*60 || (trigger.MissedRunPolicy != "coalesce" && trigger.MissedRunPolicy != "skip") || len(trigger.Filters) != 0 {
			return errors.New("schedule trigger requires an interval from 60 seconds to seven days and a coalesce or skip missed-run policy")
		}
	default:
		return errors.New("trigger kind must be event or schedule")
	}
	return nil
}

var supportedEventTypes = map[string]struct{}{
	"task.created": {}, "task.updated": {}, "task.completed": {}, "task.failed": {},
	"capability.health.changed": {}, "capability.run.completed": {}, "capability.run.failed": {},
}

func validTriggerFilter(filter contracts.CapabilityTriggerFilter, maxBytes int) bool {
	fields := map[string]struct{}{"event_type": {}, "aggregate_type": {}, "aggregate_id": {}, "subject_id": {}, "status": {}, "project_id": {}, "task_id": {}}
	if _, ok := fields[filter.Field]; !ok {
		return false
	}
	if filter.Operator != "eq" && filter.Operator != "neq" && filter.Operator != "exists" {
		return false
	}
	if filter.Operator == "exists" {
		return filter.Value == "" || filter.Value == "true" || filter.Value == "false"
	}
	return len(filter.Value) <= maxBytes && !strings.ContainsAny(filter.Value, "\x00\r\n")
}

func decode(raw json.RawMessage, dst any) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return control.NewAPIError(400, "invalid_request", "payload is invalid", nil)
	}
	return nil
}
func decodeRequest(r *http.Request, dst any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	return d.Decode(dst)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func validDigest(v string) bool {
	return len(v) == 71 && strings.HasPrefix(v, "sha256:") && func() bool { _, e := hex.DecodeString(strings.TrimPrefix(v, "sha256:")); return e == nil }()
}
func validOperation(v string) bool {
	switch v {
	case "install", "start", "stop", "pause", "resume", "update", "uninstall":
		return true
	}
	return false
}
func desiredFor(op string) string {
	switch op {
	case "install", "update":
		return "installing"
	case "start", "resume":
		return "running"
	case "stop":
		return "stopped"
	case "pause":
		return "paused"
	default:
		return "removing"
	}
}
func mapObserved(v string) string {
	switch v {
	case "pending", "installing", "running", "stopped", "paused", "failed", "uninstalling", "removed", "uncertain":
		return v
	}
	return "uncertain"
}

func desiredStateForObservation(state string) string {
	switch state {
	case "running", "stopped", "paused":
		return state
	case "removed":
		return "removing"
	default:
		return ""
	}
}

func validObservedState(v string) bool {
	switch v {
	case "pending", "installing", "running", "stopped", "paused", "failed", "uninstalling", "removed", "uncertain":
		return true
	}
	return false
}
func validOperationObservedState(operation, state string) bool {
	if !validObservedState(state) {
		return false
	}
	if state == "pending" || state == "failed" || state == "uncertain" {
		return true
	}
	switch operation {
	case "install", "update":
		return state == "installing" || state == "running"
	case "start", "resume":
		return state == "running"
	case "stop":
		return state == "stopped"
	case "pause":
		return state == "paused"
	case "uninstall":
		return state == "uninstalling" || state == "removed"
	default:
		return false
	}
}
func isTerminalObservation(operation, state string) bool {
	if state == "failed" || state == "uncertain" {
		return true
	}
	switch operation {
	case "install", "update", "start", "resume":
		return state == "running"
	case "stop":
		return state == "stopped"
	case "pause":
		return state == "paused"
	case "uninstall":
		return state == "removed"
	default:
		return false
	}
}
func runnerHasCapabilityScope(p contracts.RunnerPrincipal, want string) bool {
	for _, scope := range p.Scopes {
		if scope == want || scope == "*" {
			return true
		}
	}
	return false
}
func validScope(k, id string) bool {
	switch k {
	case "personal", "workspace":
		return id == ""
	case "project", "task":
		return id != ""
	}
	return false
}
func budgetPositive(b contracts.CapabilityResourceBudget) bool {
	return b.CPUMillis > 0 && b.MemoryBytes > 0 && b.DiskBytes > 0 && b.TimeoutSeconds > 0 && b.Concurrency > 0
}
func budgetFits(a, b contracts.CapabilityResourceBudget) bool {
	return a.CPUMillis <= b.CPUMillis && a.MemoryBytes <= b.MemoryBytes && a.DiskBytes <= b.DiskBytes && a.TimeoutSeconds <= b.TimeoutSeconds && a.Concurrency <= b.Concurrency
}
func contains(a []string, v string) bool {
	for _, x := range a {
		if x == v {
			return true
		}
	}
	return false
}
func uniqueSorted(a []string) []string {
	m := map[string]bool{}
	for _, v := range a {
		if v != "" {
			m[v] = true
		}
	}
	o := make([]string, 0, len(m))
	for v := range m {
		o = append(o, v)
	}
	sort.Strings(o)
	return o
}
