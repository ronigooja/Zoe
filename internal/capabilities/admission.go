package capabilities

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

type capabilityTriggerSourceRef struct {
	TriggerRunID string `json:"trigger_run_id"`
	TriggerID    string `json:"trigger_id"`
	GrantID      string `json:"grant_id"`
	DedupeKey    string `json:"dedupe_key"`
}

type capabilityGrantAdmissionSnapshot struct {
	WorkspaceID          string   `json:"workspace_id"`
	DeploymentID         string   `json:"deployment_id"`
	ReleaseDigest        string   `json:"release_digest"`
	TriggerID            string   `json:"trigger_id"`
	TriggerRunID         string   `json:"trigger_run_id"`
	GrantID              string   `json:"grant_id"`
	Mode                 string   `json:"mode"`
	ScopeKind            string   `json:"scope_kind"`
	ScopeID              string   `json:"scope_id,omitempty"`
	PrincipalID          string   `json:"principal_id,omitempty"`
	AuthorizedBy         string   `json:"authorized_by"`
	AuthorizedByDeviceID string   `json:"authorized_by_device_id"`
	Actions              []string `json:"actions"`
	CapabilityScopes     []string `json:"capability_scopes"`
	ExpiresAt            string   `json:"expires_at"`
}

type triggerAdmissionRow struct {
	workspace, deployment, triggerID, triggerRunID, triggerKey    string
	grantID, digest, mode, kind, source, serviceID                string
	deploymentGrantID, deploymentDigest                           string
	scopeKind, scopeID, principalID, authorizedBy, deviceID       string
	state, eventID, scheduledFor, dedupeKey                       string
	expiresAt, desiredState, observedState, configRef, configHash string
	triggerConfigRef, triggerConfigHash                           string
	triggerVersion, deploymentVersion                             int64
	triggerEnabled                                                bool
	eventPayload, actionsRaw, triggerScopesRaw                    []byte
}

func (s *Service) registerTaskSource() error {
	if err := s.server.RegisterTaskSourceHandler("service", s.validateCapabilityTaskSource); err != nil {
		return err
	}
	return s.server.RegisterTaskDispatchGuard("service", s.authorizeCapabilityTaskDispatch)
}

// authorizeCapabilityTaskDispatch rechecks the durable trigger grant inside
// the runner claim or model-call reservation transaction. Service tasks fail
// closed when their source run or its current grant cannot be proven active.
func (s *Service) authorizeCapabilityTaskDispatch(ctx context.Context, tx *sql.Tx, workspaceID, taskID string, now time.Time) (string, error) {
	var triggerRunID, runState, linkedTask, triggerGrant, triggerDigest string
	var triggerEnabled bool
	var deploymentGrant, deploymentDigest, desired, observed string
	var deploymentRevoked, grantRevoked sql.NullString
	var expiresAt string
	err := tx.QueryRowContext(ctx, `SELECT r.trigger_run_id,r.state,r.task_id,t.enabled,t.grant_id,t.release_digest,d.grant_id,d.release_digest,d.desired_state,d.observed_state,d.revoked_at,g.revoked_at,g.expires_at FROM capability_trigger_runs r JOIN capability_trigger_definitions t ON t.trigger_id=r.trigger_id AND t.workspace_id=r.workspace_id JOIN capability_deployments d ON d.deployment_id=r.deployment_id AND d.workspace_id=r.workspace_id JOIN capability_grants g ON g.grant_id=t.grant_id AND g.deployment_id=d.deployment_id AND g.release_digest=t.release_digest WHERE r.workspace_id=? AND r.task_id=?`, workspaceID, taskID).Scan(&triggerRunID, &runState, &linkedTask, &triggerEnabled, &triggerGrant, &triggerDigest, &deploymentGrant, &deploymentDigest, &desired, &observed, &deploymentRevoked, &grantRevoked, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "service_trigger_missing", nil
	}
	if err != nil {
		return "", err
	}
	reason := ""
	if linkedTask != taskID || (runState != "running" && runState != "uncertain" && runState != "waiting") {
		reason = "service_trigger_inactive"
	} else if !triggerEnabled || triggerGrant == "" || triggerGrant != deploymentGrant || triggerDigest == "" || triggerDigest != deploymentDigest {
		reason = "service_trigger_stale"
	} else if deploymentRevoked.Valid || grantRevoked.Valid {
		reason = "grant_revoked"
	} else if desired != "running" || observed != "running" {
		reason = "deployment_not_running"
	} else if expires, parseErr := time.Parse(time.RFC3339Nano, expiresAt); parseErr != nil || !expires.After(now.UTC()) {
		reason = "grant_expired"
	}
	if reason == "" {
		return "", nil
	}
	var taskStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id=? AND workspace_id=?`, taskID, workspaceID).Scan(&taskStatus); err != nil {
		return "", err
	}
	triggerState := "uncertain"
	if taskStatus == "queued" {
		triggerState = "canceled"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state=?,last_error_code=?,updated_at=? WHERE trigger_run_id=? AND state IN ('running','uncertain','waiting') AND task_id=?`, triggerState, reason, now.UTC().Format(time.RFC3339Nano), triggerRunID, taskID); err != nil {
		return "", err
	}
	return reason, nil
}

// validateCapabilityTaskSource is the task-core hook for the service source.
// It never trusts the request to identify a grant: the run, trigger, active
// deployment and authorizing identity are all reloaded from this transaction.
func (s *Service) validateCapabilityTaskSource(ctx context.Context, tx *sql.Tx, principal contracts.Principal, taskID string, request *control.TaskSourceRequest, sourceRef json.RawMessage, now time.Time) ([]contracts.Event, error) {
	var ref capabilityTriggerSourceRef
	if err := decode(sourceRef, &ref); err != nil || ref.TriggerRunID == "" || ref.TriggerID == "" || ref.GrantID == "" || ref.DedupeKey == "" {
		return nil, control.NewAPIError(403, "invalid_service_source", "capability trigger source reference is invalid", nil)
	}
	if request == nil || !request.TrustedSource || len(request.GrantSnapshot) == 0 {
		return nil, control.NewAPIError(403, "untrusted_service_source", "capability task source requires a trusted grant snapshot", nil)
	}
	row, err := loadTriggerAdmissionRow(ctx, tx, ref.TriggerRunID)
	if err != nil {
		return nil, err
	}
	if row.triggerID != ref.TriggerID || row.grantID != ref.GrantID || row.dedupeKey != ref.DedupeKey || row.mode != "worker" && row.mode != "super" || row.state != "queued" {
		return nil, control.NewAPIError(403, "stale_service_source", "capability trigger is no longer eligible for task admission", nil)
	}
	if !triggerAdmissionCurrent(row, now) {
		return nil, control.NewAPIError(403, "stale_service_source", "capability grant or deployment is no longer active", nil)
	}
	owner := row.authorizedBy
	if row.scopeKind == "personal" {
		owner = row.principalID
	}
	if owner == "" || row.deviceID == "" || principal.WorkspaceID != row.workspace || principal.SubjectID != owner || principal.DeviceID != row.deviceID || !sameStrings(principal.Scopes, decodeActions(row.actionsRaw)) {
		return nil, control.NewAPIError(403, "grant_principal_mismatch", "capability task principal does not match the durable grant", nil)
	}
	want := row.grantSnapshot()
	wantJSON, err := json.Marshal(want)
	if err != nil {
		return nil, err
	}
	var got capabilityGrantAdmissionSnapshot
	if err := json.Unmarshal(request.GrantSnapshot, &got); err != nil || !sameJSONValue(got, want) {
		return nil, control.NewAPIError(403, "grant_snapshot_mismatch", "capability task grant snapshot does not match the active grant", nil)
	}
	if !bytes.Equal(bytes.TrimSpace(request.GrantSnapshot), wantJSON) {
		// Semantic equality above prevents field confusion; canonical equality
		// keeps the task snapshot stable for audit and later reconciliation.
		return nil, control.NewAPIError(403, "grant_snapshot_mismatch", "capability task grant snapshot is not canonical", nil)
	}
	if taskID == "" {
		return nil, errors.New("capability task source requires an admitted task ID")
	}
	return []contracts.Event{{Type: "capability.trigger.task_admitted", AggregateType: "capability_trigger_run", AggregateID: ref.TriggerRunID, AggregateVersion: 1, Payload: mustJSON(map[string]any{"trigger_run_id": ref.TriggerRunID, "task_id": taskID, "grant_id": row.grantID, "grant_snapshot": want})}}, nil
}

func loadTriggerAdmissionRow(ctx context.Context, tx *sql.Tx, triggerRunID string) (triggerAdmissionRow, error) {
	var row triggerAdmissionRow
	err := tx.QueryRowContext(ctx, `SELECT r.workspace_id,r.deployment_id,r.trigger_id,r.trigger_run_id,r.trigger_key,r.state,r.event_id,r.scheduled_for,r.dedupe_key,r.event_payload,r.mode,t.kind,t.source,t.enabled,t.grant_id,t.release_digest,t.expected_deployment_version,t.config_ref,t.config_sha256,t.scopes,d.service_id,d.grant_id,d.release_digest,d.version,d.desired_state,d.observed_state,d.config_ref,d.config_sha256,g.scope_kind,g.scope_id,g.principal_id,g.authorized_by,coalesce(g.authorized_by_device_id,''),g.actions,g.expires_at FROM capability_trigger_runs r JOIN capability_trigger_definitions t ON t.trigger_id=r.trigger_id AND t.workspace_id=r.workspace_id JOIN capability_deployments d ON d.deployment_id=r.deployment_id AND d.workspace_id=r.workspace_id JOIN capability_grants g ON g.grant_id=t.grant_id AND g.deployment_id=d.deployment_id AND g.release_digest=t.release_digest WHERE r.trigger_run_id=?`, triggerRunID).Scan(&row.workspace, &row.deployment, &row.triggerID, &row.triggerRunID, &row.triggerKey, &row.state, &row.eventID, &row.scheduledFor, &row.dedupeKey, &row.eventPayload, &row.mode, &row.kind, &row.source, &row.triggerEnabled, &row.grantID, &row.digest, &row.triggerVersion, &row.triggerConfigRef, &row.triggerConfigHash, &row.triggerScopesRaw, &row.serviceID, &row.deploymentGrantID, &row.deploymentDigest, &row.deploymentVersion, &row.desiredState, &row.observedState, &row.configRef, &row.configHash, &row.scopeKind, &row.scopeID, &row.principalID, &row.authorizedBy, &row.deviceID, &row.actionsRaw, &row.expiresAt)
	if err != nil {
		return triggerAdmissionRow{}, err
	}
	// Current state is checked separately below. These columns are included in
	// the joined snapshot to prevent a trigger from outliving a grant rollover.
	if row.grantID == "" || row.digest == "" {
		return triggerAdmissionRow{}, control.NewAPIError(403, "stale_service_source", "capability trigger grant is unavailable", nil)
	}
	return row, nil
}

func (r triggerAdmissionRow) grantSnapshot() capabilityGrantAdmissionSnapshot {
	var scopes []string
	_ = json.Unmarshal(r.triggerScopesRaw, &scopes)
	return capabilityGrantAdmissionSnapshot{WorkspaceID: r.workspace, DeploymentID: r.deployment, ReleaseDigest: r.digest, TriggerID: r.triggerID, TriggerRunID: r.triggerRunID, GrantID: r.grantID, Mode: r.mode, ScopeKind: r.scopeKind, ScopeID: r.scopeID, PrincipalID: r.principalID, AuthorizedBy: r.authorizedBy, AuthorizedByDeviceID: r.deviceID, Actions: decodeActions(r.actionsRaw), CapabilityScopes: uniqueSorted(scopes), ExpiresAt: r.expiresAt}
}

func triggerAdmissionCurrent(r triggerAdmissionRow, now time.Time) bool {
	expires, err := time.Parse(time.RFC3339Nano, r.expiresAt)
	if err != nil || !expires.After(now.UTC()) {
		return false
	}
	return r.triggerEnabled && r.grantID == r.deploymentGrantID && r.digest == r.deploymentDigest && r.desiredState == "running" && r.observedState == "running" && r.configRef == r.triggerConfigRef && r.configHash == r.triggerConfigHash && r.triggerVersion == r.deploymentVersion
}

func decodeActions(raw []byte) []string {
	var values []string
	_ = json.Unmarshal(raw, &values)
	return uniqueSorted(values)
}

func sameStrings(left, right []string) bool {
	left, right = uniqueSorted(left), uniqueSorted(right)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func sameJSONValue(left, right capabilityGrantAdmissionSnapshot) bool {
	return left.WorkspaceID == right.WorkspaceID && left.DeploymentID == right.DeploymentID && left.ReleaseDigest == right.ReleaseDigest && left.TriggerID == right.TriggerID && left.TriggerRunID == right.TriggerRunID && left.GrantID == right.GrantID && left.Mode == right.Mode && left.ScopeKind == right.ScopeKind && left.ScopeID == right.ScopeID && left.PrincipalID == right.PrincipalID && left.AuthorizedBy == right.AuthorizedBy && left.AuthorizedByDeviceID == right.AuthorizedByDeviceID && left.ExpiresAt == right.ExpiresAt && sameStrings(left.Actions, right.Actions) && sameStrings(left.CapabilityScopes, right.CapabilityScopes)
}

func (s *Service) admitQueuedTaskTriggers(ctx context.Context, tx *sql.Tx, workspace string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='canceled',last_error_code='grant_expired',updated_at=? WHERE workspace_id=? AND state='queued' AND mode IN ('worker','super') AND trigger_id IN (SELECT t.trigger_id FROM capability_trigger_definitions t JOIN capability_grants g ON g.grant_id=t.grant_id WHERE t.workspace_id=? AND g.expires_at<=?)`, now.UTC().Format(time.RFC3339Nano), workspace, workspace, now.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.trigger_run_id,r.trigger_id,r.deployment_id,r.mode,t.grant_id,t.release_digest,t.scopes,d.service_id,g.scope_kind,g.scope_id,g.principal_id,g.authorized_by,g.authorized_by_device_id,g.actions,g.budget,g.expires_at FROM capability_trigger_runs r JOIN capability_trigger_definitions t ON t.trigger_id=r.trigger_id JOIN capability_deployments d ON d.deployment_id=r.deployment_id AND d.grant_id=t.grant_id AND d.release_digest=t.release_digest AND d.version=t.expected_deployment_version AND d.config_ref=t.config_ref AND d.config_sha256=t.config_sha256 JOIN capability_grants g ON g.grant_id=t.grant_id AND g.deployment_id=d.deployment_id AND g.release_digest=d.release_digest WHERE r.workspace_id=? AND r.state='queued' AND r.mode IN ('worker','super') AND t.enabled=1 AND d.desired_state='running' AND d.observed_state='running' AND g.expires_at>? ORDER BY r.created_at,r.trigger_run_id LIMIT ?`, workspace, now.UTC().Format(time.RFC3339Nano), s.limits.Int("capability_trigger_page_rows"))
	if err != nil {
		return err
	}
	type candidate struct {
		runID, triggerID, deployment, mode, grantID, digest, serviceID, scopeKind, scopeID, principalID, authorizedBy, deviceID, expires string
		scopesRaw, actionsRaw, budgetRaw                                                                                                 []byte
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.runID, &item.triggerID, &item.deployment, &item.mode, &item.grantID, &item.digest, &item.scopesRaw, &item.serviceID, &item.scopeKind, &item.scopeID, &item.principalID, &item.authorizedBy, &item.deviceID, &item.actionsRaw, &item.budgetRaw, &item.expires); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range candidates {
		var budget contracts.CapabilityResourceBudget
		if err := json.Unmarshal(item.budgetRaw, &budget); err != nil {
			return err
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM capability_trigger_runs WHERE trigger_id=? AND state IN ('claimed','running','uncertain','waiting')`, item.triggerID).Scan(&active); err != nil {
			return err
		}
		if active >= budget.Concurrency {
			continue
		}
		profileID, ok := s.server.ProfileForRole(item.mode)
		if !ok {
			if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='failed',last_error_code='profile_unavailable',updated_at=? WHERE trigger_run_id=? AND state='queued'`, now.UTC().Format(time.RFC3339Nano), item.runID); err != nil {
				return err
			}
			continue
		}
		if err := s.admitTaskTrigger(ctx, tx, item.runID, profileID, now); err != nil {
			var apiErr *control.APIError
			if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
				if _, updateErr := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='failed',last_error_code=?,updated_at=? WHERE trigger_run_id=? AND state='queued'`, apiErr.Code, now.UTC().Format(time.RFC3339Nano), item.runID); updateErr != nil {
					return updateErr
				}
				continue
			}
			return err
		}
	}
	return nil
}

// reapInactiveServiceTasks cancels source tasks that have not been claimed and
// requests a fenced stop for active runs whose grant expired or was revoked.
// It uses only local SQLite and emits no model work.
func (s *Service) reapInactiveServiceTasks(ctx context.Context, tx *sql.Tx, workspaces []string, now time.Time) error {
	for _, workspace := range workspaces {
		rows, err := tx.QueryContext(ctx, `SELECT t.id,t.owner_subject_id,t.status,t.version FROM tasks t JOIN capability_trigger_runs r ON r.task_id=t.id AND r.workspace_id=t.workspace_id WHERE t.workspace_id=? AND t.source='service' AND r.state IN ('running','uncertain','waiting') AND t.status IN ('queued','active','waiting','awaiting_user') ORDER BY t.created_at,t.id`, workspace)
		if err != nil {
			return err
		}
		type task struct {
			id, owner, status string
			version           int64
		}
		var tasks []task
		for rows.Next() {
			var item task
			if err := rows.Scan(&item.id, &item.owner, &item.status, &item.version); err != nil {
				rows.Close()
				return err
			}
			tasks = append(tasks, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range tasks {
			reason, err := s.authorizeCapabilityTaskDispatch(ctx, tx, workspace, item.id, now)
			if err != nil {
				return err
			}
			at := now.UTC().Format(time.RFC3339Nano)
			if item.status == "awaiting_user" {
				// Preserve the already-produced result for the user's decision. A
				// trigger grant governs further dispatch, not accepting evidence.
				if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='waiting',last_error_code=?,updated_at=? WHERE workspace_id=? AND task_id=? AND state IN ('running','uncertain','waiting')`, reason, at, workspace, item.id); err != nil {
					return err
				}
				continue
			}
			if reason == "" {
				continue
			}
			if item.status == "active" {
				if _, err := tx.ExecContext(ctx, `UPDATE runs SET desired_control='stop',control_version=control_version+CASE WHEN desired_control='stop' THEN 0 ELSE 1 END WHERE task_id=? AND workspace_id=? AND status IN ('dispatching','active','uncertain')`, item.id, workspace); err != nil {
					return err
				}
				continue
			}
			result, err := tx.ExecContext(ctx, `UPDATE tasks SET status='canceled',version=version+1,finished_at=?,updated_at=? WHERE id=? AND workspace_id=? AND status=? AND version=?`, at, at, item.id, workspace, item.status, item.version)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if changed != 1 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='canceled',last_error_code=?,updated_at=? WHERE workspace_id=? AND task_id=? AND state IN ('running','uncertain','waiting')`, reason, at, workspace, item.id); err != nil {
				return err
			}
			event := contracts.Event{Type: "task.canceled", OccurredAt: now.UTC(), WorkspaceID: workspace, AggregateType: "task", AggregateID: item.id, AggregateVersion: item.version + 1, Payload: mustJSON(map[string]any{"task_id": item.id, "reason_code": reason})}
			if err := s.server.AppendEventsTx(ctx, tx, contracts.Principal{WorkspaceID: workspace, SubjectID: item.owner}, []contracts.Event{event}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) admitTaskTrigger(ctx context.Context, tx *sql.Tx, runID, profileID string, now time.Time) error {
	// A savepoint keeps a rejected admission from leaving a task behind while
	// allowing its trigger run to record the stable rejection and advance cursors.
	if _, err := tx.ExecContext(ctx, `SAVEPOINT capability_task_admission`); err != nil {
		return err
	}
	finishSavepoint := func(rollback bool) error {
		if rollback {
			if _, err := tx.ExecContext(ctx, `ROLLBACK TO capability_task_admission`); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `RELEASE capability_task_admission`)
		return err
	}
	row, err := loadTriggerAdmissionRow(ctx, tx, runID)
	if err != nil {
		_ = finishSavepoint(true)
		return err
	}
	if !triggerAdmissionCurrent(row, now) || row.mode != "worker" && row.mode != "super" || row.state != "queued" {
		_ = finishSavepoint(true)
		return control.NewAPIError(403, "stale_service_source", "capability trigger is no longer eligible for task admission", nil)
	}
	owner := row.authorizedBy
	if row.scopeKind == "personal" {
		owner = row.principalID
	}
	if owner == "" || row.deviceID == "" {
		_ = finishSavepoint(true)
		return control.NewAPIError(403, "grant_principal_unavailable", "capability grant has no durable authorizing principal or device", nil)
	}
	principal := contracts.Principal{WorkspaceID: row.workspace, SubjectID: owner, DeviceID: row.deviceID, Scopes: decodeActions(row.actionsRaw)}
	ref := capabilityTriggerSourceRef{TriggerRunID: row.triggerRunID, TriggerID: row.triggerID, GrantID: row.grantID, DedupeKey: row.dedupeKey}
	sourceRef, err := json.Marshal(ref)
	if err != nil {
		_ = finishSavepoint(true)
		return err
	}
	grantSnapshot, err := json.Marshal(row.grantSnapshot())
	if err != nil {
		_ = finishSavepoint(true)
		return err
	}
	projectID, err := triggerTaskProject(ctx, tx, row)
	if err != nil {
		_ = finishSavepoint(true)
		return err
	}
	title := strings.TrimSpace(row.serviceID) + " trigger"
	if title == "trigger" {
		title = "Capability trigger"
	}
	goal, initial := triggerTaskContent(row)
	admitted, err := s.server.AdmitTaskTx(ctx, tx, control.AdmissionContext{Principal: principal, Source: "service", SourceRef: sourceRef, IdempotencyKey: "capability-trigger:" + row.triggerRunID, GrantSnapshot: grantSnapshot, ProfileID: profileID, ProjectID: projectID, Title: title, Goal: goal, InitialMessage: initial, TrustedSource: true, AcceptedAt: now.UTC()})
	if err != nil {
		if rollbackErr := finishSavepoint(true); rollbackErr != nil {
			return rollbackErr
		}
		return err
	}
	if err := s.server.AppendEventsTx(ctx, tx, principal, admitted.Events); err != nil {
		_ = finishSavepoint(true)
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='running',task_id=?,updated_at=? WHERE trigger_run_id=? AND state='queued'`, admitted.Task.ID, now.UTC().Format(time.RFC3339Nano), row.triggerRunID)
	if err != nil {
		_ = finishSavepoint(true)
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		if rollbackErr := finishSavepoint(true); rollbackErr != nil {
			return rollbackErr
		}
		return control.NewAPIError(409, "trigger_run_conflict", "capability trigger run changed during task admission", nil)
	}
	if err := finishSavepoint(false); err != nil {
		return err
	}
	return nil
}

func triggerTaskProject(ctx context.Context, tx *sql.Tx, row triggerAdmissionRow) (string, error) {
	switch row.scopeKind {
	case "project":
		return row.scopeID, nil
	case "task":
		var project sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE workspace_id=? AND id=?`, row.workspace, row.scopeID).Scan(&project)
		if errors.Is(err, sql.ErrNoRows) {
			return "", control.NewAPIError(403, "grant_scope_unavailable", "capability task grant target is unavailable", nil)
		}
		if err != nil {
			return "", err
		}
		return project.String, nil
	}
	if row.eventID == "" || row.scheduledFor != "" {
		return "", nil
	}
	event := triggerEventRecord{id: row.eventID, payload: row.eventPayload}
	values := eventFilterValues(event)
	if project := values["project_id"]; project != "" {
		return project, nil
	}
	if taskID := values["task_id"]; taskID != "" {
		var project sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE workspace_id=? AND id=?`, row.workspace, taskID).Scan(&project)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		return project.String, nil
	}
	return "", nil
}

func triggerTaskContent(row triggerAdmissionRow) (string, string) {
	if row.scheduledFor != "" {
		return fmt.Sprintf("Run scheduled capability trigger for %s at %s.", row.serviceID, row.scheduledFor), "Scheduled trigger time: " + row.scheduledFor
	}
	if len(row.eventPayload) > 0 {
		return fmt.Sprintf("Handle capability trigger for %s after %s.", row.serviceID, row.source), string(row.eventPayload)
	}
	return fmt.Sprintf("Handle capability trigger for %s.", row.serviceID), ""
}

func isCapabilityServiceTaskEvent(payload []byte) bool {
	var body struct {
		Task struct {
			Source string `json:"source"`
		} `json:"task"`
	}
	return json.Unmarshal(triggerEventPayload(payload), &body) == nil && body.Task.Source == "service"
}

func mustJSON(value any) json.RawMessage {
	body, _ := json.Marshal(value)
	return body
}
