package capabilities

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

type triggerEventRecord struct {
	id, kind, at, subject, aggregateType, aggregateID string
	cursor                                            int64
	payload                                           []byte
}

func insertTriggerDefinitions(ctx context.Context, tx *sql.Tx, deployment, digest, grant string, deploymentVersion int64, configRef, configHash string, definitions []contracts.CapabilityTrigger, now time.Time) error {
	for _, definition := range definitions {
		body, err := json.Marshal(definition)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		triggerKey := hex.EncodeToString(sum[:])
		scopes, _ := json.Marshal(uniqueSorted(definition.Scopes))
		filters, _ := json.Marshal(definition.Filters)
		nextRun := ""
		if definition.Kind == "schedule" {
			nextRun = now.UTC().Add(time.Duration(definition.IntervalSeconds) * time.Second).Format(time.RFC3339Nano)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO capability_trigger_definitions(trigger_id,deployment_id,release_digest,grant_id,trigger_key,kind,source,mode,scopes,filters,interval_seconds,missed_run_policy,enabled,next_run_at,expected_deployment_version,config_ref,config_sha256,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0,?,?,?,?,?,?)`, uuid.NewString(), deployment, digest, grant, triggerKey, definition.Kind, definition.Source, definition.Mode, scopes, filters, definition.IntervalSeconds, definition.MissedRunPolicy, nullIfEmpty(nextRun), deploymentVersion, configRef, configHash, now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	return nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Service) enableDeploymentTriggers(ctx context.Context, tx *sql.Tx, deployment string, enabled bool, now time.Time) error {
	at := now.UTC().Format(time.RFC3339Nano)
	// Always turn off old definitions first. A deployment can retain grant and
	// release history, so enabling by deployment alone can revive obsolete code.
	if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_definitions SET enabled=0,updated_at=? WHERE deployment_id=? AND enabled=1`, at, deployment); err != nil {
		return err
	}
	if !enabled {
		_, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='canceled',updated_at=? WHERE deployment_id=? AND state='queued'`, at, deployment)
		return err
	}
	var grantID, digest, configRef, configHash, desired, observed, expires string
	var revokedAt sql.NullString
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT d.grant_id,d.release_digest,d.config_ref,d.config_sha256,d.version,d.desired_state,d.observed_state,g.expires_at,d.revoked_at FROM capability_deployments d JOIN capability_grants g ON g.grant_id=d.grant_id AND g.deployment_id=d.deployment_id AND g.release_digest=d.release_digest WHERE d.deployment_id=?`, deployment).Scan(&grantID, &digest, &configRef, &configHash, &version, &desired, &observed, &expires, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		_, cancelErr := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='canceled',updated_at=? WHERE deployment_id=? AND state='queued'`, at, deployment)
		return cancelErr
	}
	if err != nil {
		return err
	}
	expiry, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return err
	}
	if desired != "running" || observed != "running" || !expiry.After(now.UTC()) || grantID == "" || revokedAt.Valid {
		_, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='canceled',updated_at=? WHERE deployment_id=? AND state='queued'`, at, deployment)
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT trigger_id,kind,interval_seconds FROM capability_trigger_definitions WHERE deployment_id=? AND grant_id=? AND release_digest=? AND expected_deployment_version=? AND config_ref=? AND config_sha256=?`, deployment, grantID, digest, version, configRef, configHash)
	if err != nil {
		return err
	}
	type trigger struct {
		id, kind string
		interval int64
	}
	var triggers []trigger
	for rows.Next() {
		var item trigger
		if err := rows.Scan(&item.id, &item.kind, &item.interval); err != nil {
			rows.Close()
			return err
		}
		triggers = append(triggers, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range triggers {
		var next any
		if enabled && item.kind == "schedule" {
			next = now.UTC().Add(time.Duration(item.interval) * time.Second).Format(time.RFC3339Nano)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_definitions SET enabled=1,next_run_at=?,updated_at=? WHERE trigger_id=?`, next, at, item.id); err != nil {
			return err
		}
	}
	// Queued work created by any superseded trigger remains auditable but may
	// not run under the newly active deployment identity.
	_, err = tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='canceled',updated_at=? WHERE deployment_id=? AND state='queued' AND trigger_id NOT IN (SELECT trigger_id FROM capability_trigger_definitions WHERE deployment_id=? AND enabled=1)`, at, deployment, deployment)
	return err
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Service) claimTrigger(w http.ResponseWriter, r *http.Request) {
	runner, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok || !runnerHasCapabilityScope(runner, "capability:deploy") {
		control.WriteError(w, control.NewAPIError(401, "unauthenticated", "capability:deploy runner authentication required", nil))
		return
	}
	if s.runtimeBackend == "" {
		control.WriteError(w, control.NewAPIError(503, "capability_runner_unavailable", "capability runtime backend is disabled", nil))
		return
	}
	var claim *contracts.CapabilityTriggerClaim
	err := s.server.RunnerTransaction(r.Context(), func(tx *sql.Tx, runner contracts.RunnerPrincipal) error {
		if current, err := s.activeTriggerClaim(r.Context(), tx, runner); err != nil {
			return err
		} else if current != nil {
			claim = current
			return nil
		}
		now := time.Now().UTC()
		if err := s.processTriggerAdmissions(r.Context(), tx, runner, now); err != nil {
			return err
		}
		if !control.HasRunnerScope(runner, "capability:deploy") {
			return nil
		}
		var err error
		claim, err = s.claimQueuedProgramTrigger(r.Context(), tx, runner, now)
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

type capabilityTriggerAdmissionStatus struct {
	PendingProgramTriggers int    `json:"pending_program_triggers"`
	ProgramExecution       string `json:"program_execution"`
	ProgramReasonCode      string `json:"program_reason_code,omitempty"`
}

// admitTriggerTasks advances event/schedule cursors and admits worker/super
// triggers through the trusted task source hook. It does not claim program
// triggers: there is no bounded invocation executor for their manifest/event
// contract yet, so they remain queued for a future compatible runner.
func (s *Service) admitTriggerTasks(w http.ResponseWriter, r *http.Request) {
	runner, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok || !runnerHasCapabilityScope(runner, "capability:deploy") {
		control.WriteError(w, control.NewAPIError(401, "unauthenticated", "capability:deploy runner authentication required", nil))
		return
	}
	if s.runtimeBackend == "" {
		control.WriteError(w, control.NewAPIError(503, "capability_runner_unavailable", "capability runtime backend is disabled", nil))
		return
	}
	var status capabilityTriggerAdmissionStatus
	err := s.server.RunnerTransaction(r.Context(), func(tx *sql.Tx, runner contracts.RunnerPrincipal) error {
		now := time.Now().UTC()
		if err := s.processTriggerAdmissions(r.Context(), tx, runner, now); err != nil {
			return err
		}
		if err := s.reapInactiveServiceTasks(r.Context(), tx, now); err != nil {
			return err
		}
		var countErr error
		status.PendingProgramTriggers, countErr = countPendingProgramTriggers(r.Context(), tx, now)
		if countErr != nil {
			return countErr
		}
		if status.PendingProgramTriggers > 0 {
			status.ProgramExecution = "unsupported"
			status.ProgramReasonCode = "program_trigger_executor_unavailable"
		} else {
			status.ProgramExecution = "ready"
		}
		return nil
	})
	if err != nil {
		control.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Service) processTriggerAdmissions(ctx context.Context, tx *sql.Tx, runner contracts.RunnerPrincipal, now time.Time) error {
	if !control.HasRunnerScope(runner, "capability:deploy") {
		return nil
	}
	if err := s.processTriggerEvents(ctx, tx, now); err != nil {
		return err
	}
	if err := s.enqueueDueSchedules(ctx, tx, now); err != nil {
		return err
	}
	if err := s.admitQueuedTaskTriggers(ctx, tx, now); err != nil {
		return err
	}
	return nil
}

func countPendingProgramTriggers(ctx context.Context, tx *sql.Tx, now time.Time) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM capability_trigger_runs r JOIN capability_trigger_definitions t ON t.trigger_id=r.trigger_id JOIN capability_deployments d ON d.deployment_id=r.deployment_id JOIN capability_grants g ON g.grant_id=t.grant_id AND g.deployment_id=d.deployment_id AND g.release_digest=t.release_digest WHERE r.mode='program' AND r.state='queued' AND t.enabled=1 AND d.revoked_at IS NULL AND d.grant_id=t.grant_id AND d.release_digest=t.release_digest AND d.desired_state='running' AND d.observed_state='running' AND g.revoked_at IS NULL AND g.expires_at>?`, now.UTC().Format(time.RFC3339Nano)).Scan(&count)
	return count, err
}

func (s *Service) currentTriggerClaim(w http.ResponseWriter, r *http.Request) {
	runner, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok || !runnerHasCapabilityScope(runner, "capability:deploy") {
		control.WriteError(w, control.NewAPIError(401, "unauthenticated", "capability:deploy runner authentication required", nil))
		return
	}
	var claim *contracts.CapabilityTriggerClaim
	err := s.server.RunnerTransaction(r.Context(), func(tx *sql.Tx, runner contracts.RunnerPrincipal) error {
		var err error
		claim, err = s.activeTriggerClaim(r.Context(), tx, runner)
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

func (s *Service) activeTriggerClaim(ctx context.Context, tx *sql.Tx, runner contracts.RunnerPrincipal) (*contracts.CapabilityTriggerClaim, error) {
	rows, err := tx.QueryContext(ctx, `SELECT claim_json,state FROM capability_trigger_runs WHERE runner_id=? AND state IN ('claimed','running','uncertain') ORDER BY updated_at DESC LIMIT 2`, runner.RunnerID)
	if err != nil {
		return nil, err
	}
	type active struct {
		body  []byte
		state string
	}
	var items []active
	for rows.Next() {
		var item active
		if err := rows.Scan(&item.body, &item.state); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(items) > 1 {
		return nil, control.NewAPIError(409, "multiple_active_triggers", "runner has multiple unresolved trigger effects", nil)
	}
	if len(items) == 0 {
		return nil, nil
	}
	item := items[0]
	if !control.HasRunnerScope(runner, "capability:deploy") {
		return nil, control.NewAPIError(403, "forbidden", "runner cannot access the current trigger claim", nil)
	}
	var claim contracts.CapabilityTriggerClaim
	if err := json.Unmarshal(item.body, &claim); err != nil {
		return nil, err
	}
	if err := validateManifest(&claim.Manifest, s.limits); err != nil {
		return nil, control.NewAPIError(500, "invalid_claim_snapshot", "trigger claim manifest failed immutable validation", nil)
	}
	return &claim, nil
}

func (s *Service) processTriggerEvents(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO capability_trigger_cursors(singleton,cursor) VALUES(1,0) ON CONFLICT(singleton) DO NOTHING`); err != nil {
		return err
	}
	var cursor int64
	if err := tx.QueryRowContext(ctx, `SELECT cursor FROM capability_trigger_cursors WHERE singleton=1`).Scan(&cursor); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT event_id,cursor,event_type,occurred_at,subject_id,aggregate_type,aggregate_id,payload FROM events WHERE cursor>? ORDER BY cursor LIMIT ?`, cursor, s.limits.Int("capability_trigger_page_rows"))
	if err != nil {
		return err
	}
	var events []triggerEventRecord
	for rows.Next() {
		var event triggerEventRecord
		if err := rows.Scan(&event.id, &event.cursor, &event.kind, &event.at, &event.subject, &event.aggregateType, &event.aggregateID, &event.payload); err != nil {
			rows.Close()
			return err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, event := range events {
		if event.aggregateType == "run" && strings.HasPrefix(event.kind, "run.") {
			var body struct {
				Payload struct {
					TaskID     string `json:"task_id"`
					TaskStatus string `json:"task_status"`
				} `json:"payload"`
			}
			if err := json.Unmarshal(event.payload, &body); err != nil {
				return err
			}
			if body.Payload.TaskID != "" {
				if err := reconcileTriggerRunTaskStatus(ctx, tx, body.Payload.TaskID, body.Payload.TaskStatus, now); err != nil {
					return err
				}
			}
		}
		if event.aggregateType == "task" && event.kind == "task.completed" {
			if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='completed',updated_at=? WHERE task_id=? AND state='running'`, now.Format(time.RFC3339Nano), event.aggregateID); err != nil {
				return err
			}
		}
		if event.aggregateType == "task" && event.kind == "task.result_decided" {
			var body struct {
				Payload struct {
					Status string `json:"status"`
				} `json:"payload"`
			}
			if err := json.Unmarshal(event.payload, &body); err != nil {
				return err
			}
			switch body.Payload.Status {
			case "complete":
				if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='completed',updated_at=? WHERE task_id=? AND state IN ('running','uncertain','waiting')`, now.Format(time.RFC3339Nano), event.aggregateID); err != nil {
					return err
				}
			case "waiting":
				if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='waiting',updated_at=? WHERE task_id=? AND state='running'`, now.Format(time.RFC3339Nano), event.aggregateID); err != nil {
					return err
				}
			}
		}
		if event.aggregateType == "task" && event.kind == "task.failed" {
			if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='failed',last_error_code='task_failed',updated_at=? WHERE task_id=? AND state='running'`, now.Format(time.RFC3339Nano), event.aggregateID); err != nil {
				return err
			}
		}
		if event.aggregateType == "task" && event.kind == "task.canceled" {
			if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='canceled',updated_at=? WHERE task_id=? AND state IN ('running','uncertain','waiting')`, now.Format(time.RFC3339Nano), event.aggregateID); err != nil {
				return err
			}
		}
		if err := s.enqueueEventRuns(ctx, tx, event, now); err != nil {
			return err
		}
		cursor = event.cursor
	}
	if len(events) > 0 {
		_, err = tx.ExecContext(ctx, `UPDATE capability_trigger_cursors SET cursor=? WHERE singleton=1`, cursor)
	}
	return err
}

// reconcileTriggerRunTaskStatus consumes terminal task state carried by run
// observations. Core runner events aggregate on the run, so there is no
// corresponding task.failed/task.canceled event to release the trigger slot.
func reconcileTriggerRunTaskStatus(ctx context.Context, tx *sql.Tx, taskID, taskStatus string, now time.Time) error {
	state, reason := "", ""
	switch taskStatus {
	case "complete":
		state = "completed"
	case "awaiting_user", "waiting":
		state = "waiting"
	case "failed":
		state, reason = "failed", "task_failed"
	case "canceled":
		state = "canceled"
	default:
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state=?,last_error_code=CASE WHEN ?='' THEN last_error_code ELSE ? END,updated_at=? WHERE task_id=? AND state IN ('running','uncertain','waiting')`, state, reason, reason, now.UTC().Format(time.RFC3339Nano), taskID)
	return err
}

func (s *Service) enqueueEventRuns(ctx context.Context, tx *sql.Tx, event triggerEventRecord, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT trigger_id,deployment_id,trigger_key,mode,filters FROM capability_trigger_definitions WHERE enabled=1 AND kind='event' AND source=? ORDER BY trigger_id`, event.kind)
	if err != nil {
		return err
	}
	type candidate struct {
		id, deployment, key, mode string
		filters                   []byte
	}
	var matches []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.deployment, &item.key, &item.mode, &item.filters); err != nil {
			rows.Close()
			return err
		}
		matches = append(matches, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	eventValues := eventFilterValues(event)
	for _, item := range matches {
		if item.mode != "program" && event.kind == "task.created" && isCapabilityServiceTaskEvent(event.payload) {
			continue
		}
		var filters []contracts.CapabilityTriggerFilter
		if err := json.Unmarshal(item.filters, &filters); err != nil {
			return err
		}
		if !matchesTriggerFilters(filters, eventValues) {
			continue
		}
		payload := triggerEventPayload(event.payload)
		scopedEvent := event
		scopedEvent.payload = payload
		allowed, err := eventWithinGrantScope(ctx, tx, item.id, scopedEvent)
		if err != nil {
			return err
		}
		if !allowed {
			continue
		}
		if err := deploymentTriggerAdmission(ctx, tx, item.id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		state, code := "queued", ""
		if len(payload) > s.limits.Int("capability_trigger_payload_max_bytes") {
			payload = []byte("{}")
			state, code = "failed", "event_payload_too_large"
		}
		dedupe := "event:" + event.id
		_, err = tx.ExecContext(ctx, `INSERT INTO capability_trigger_runs(trigger_run_id,trigger_id,deployment_id,trigger_key,dedupe_key,event_id,event_payload,mode,state,last_error_code,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(trigger_id,dedupe_key) DO NOTHING`, uuid.NewString(), item.id, item.deployment, item.key, dedupe, event.id, payload, item.mode, state, code, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	return nil
}

func eventWithinGrantScope(ctx context.Context, tx *sql.Tx, triggerID string, event triggerEventRecord) (bool, error) {
	var kind, scopeID, principalID string
	err := tx.QueryRowContext(ctx, `SELECT g.scope_kind,g.scope_id,g.principal_id FROM capability_trigger_definitions t JOIN capability_grants g ON g.grant_id=t.grant_id WHERE t.trigger_id=?`, triggerID).Scan(&kind, &scopeID, &principalID)
	if err != nil {
		return false, err
	}
	if kind == "instance" {
		return true, nil
	}
	var payload map[string]any
	_ = json.Unmarshal(event.payload, &payload)
	value := func(key string) string {
		if v, ok := payload[key]; ok {
			return fmt.Sprint(v)
		}
		return ""
	}
	taskID, projectID := value("task_id"), ""
	if event.aggregateType == "task" && taskID == "" {
		taskID = event.aggregateID
	}
	if event.aggregateType == "project" && projectID == "" {
		projectID = event.aggregateID
	}
	switch kind {
	case "task":
		return scopeID != "" && scopeID == taskID, nil
	case "project":
		if event.aggregateType == "project" {
			projectID = event.aggregateID
		} else if taskID != "" {
			var project sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id=?`, taskID).Scan(&project)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}
			if project.Valid {
				projectID = project.String
			}
		}
		return scopeID != "" && scopeID == projectID, nil
	case "personal":
		if principalID == "" {
			return false, nil
		}
		if taskID != "" {
			var owner string
			err := tx.QueryRowContext(ctx, `SELECT owner_subject_id FROM tasks WHERE id=?`, taskID).Scan(&owner)
			if err == nil && owner == principalID {
				return true, nil
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}
		}
		if event.aggregateType == "project" {
			projectID = event.aggregateID
		}
		if projectID != "" {
			var owner string
			err := tx.QueryRowContext(ctx, `SELECT owner_subject_id FROM projects WHERE id=?`, projectID).Scan(&owner)
			if err == nil && owner == principalID {
				return true, nil
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}
		}
		return false, nil
	default:
		return false, nil
	}
}

func eventFilterValues(event triggerEventRecord) map[string]string {
	values := map[string]string{"event_type": event.kind, "aggregate_type": event.aggregateType, "aggregate_id": event.aggregateID, "subject_id": event.subject}
	var payload map[string]any
	if json.Unmarshal(triggerEventPayload(event.payload), &payload) == nil {
		for _, field := range []string{"project_id", "task_id", "status"} {
			value, ok := payload[field]
			if nested, nestedOK := payload["task"].(map[string]any); nestedOK && !ok {
				value, ok = nested[field]
			}
			if ok {
				values[field] = fmt.Sprint(value)
			}
		}
	}
	if event.aggregateType == "task" && values["task_id"] == "" {
		values["task_id"] = event.aggregateID
	}
	return values
}

func triggerEventPayload(raw []byte) []byte {
	var envelope struct {
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(raw, &envelope) == nil && len(envelope.Payload) > 0 {
		return envelope.Payload
	}
	return raw
}

func matchesTriggerFilters(filters []contracts.CapabilityTriggerFilter, values map[string]string) bool {
	for _, filter := range filters {
		value, exists := values[filter.Field]
		switch filter.Operator {
		case "eq":
			if !exists || value != filter.Value {
				return false
			}
		case "neq":
			if exists && value == filter.Value {
				return false
			}
		case "exists":
			want := filter.Value != "false"
			if exists != want {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func deploymentTriggerAdmission(ctx context.Context, tx *sql.Tx, triggerID string) error {
	var desired, observed, expires string
	err := tx.QueryRowContext(ctx, `SELECT d.desired_state,d.observed_state,g.expires_at FROM capability_trigger_definitions t JOIN capability_deployments d ON d.deployment_id=t.deployment_id AND d.grant_id=t.grant_id AND d.release_digest=t.release_digest AND d.version=t.expected_deployment_version AND d.config_ref=t.config_ref AND d.config_sha256=t.config_sha256 JOIN capability_grants g ON g.grant_id=t.grant_id AND g.deployment_id=d.deployment_id AND g.release_digest=d.release_digest WHERE t.trigger_id=? AND t.enabled=1`, triggerID).Scan(&desired, &observed, &expires)
	if err != nil {
		return err
	}
	if desired != "running" || observed != "running" {
		return sql.ErrNoRows
	}
	expiry, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return err
	}
	if !expiry.After(time.Now().UTC()) {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Service) enqueueDueSchedules(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT t.trigger_id,t.deployment_id,t.trigger_key,t.interval_seconds,t.missed_run_policy,t.next_run_at,t.mode FROM capability_trigger_definitions t JOIN capability_deployments d ON d.deployment_id=t.deployment_id AND d.grant_id=t.grant_id AND d.release_digest=t.release_digest AND d.version=t.expected_deployment_version AND d.config_ref=t.config_ref AND d.config_sha256=t.config_sha256 JOIN capability_grants g ON g.grant_id=t.grant_id AND g.deployment_id=d.deployment_id AND g.release_digest=d.release_digest WHERE t.enabled=1 AND t.kind='schedule' AND t.next_run_at<=? AND d.desired_state='running' AND d.observed_state='running' AND g.expires_at>? ORDER BY t.next_run_at,t.trigger_id LIMIT ?`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), s.limits.Int("capability_trigger_page_rows"))
	if err != nil {
		return err
	}
	type due struct {
		id, deployment, key, policy, next, mode string
		interval                                int64
	}
	var items []due
	for rows.Next() {
		var item due
		if err := rows.Scan(&item.id, &item.deployment, &item.key, &item.interval, &item.policy, &item.next, &item.mode); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range items {
		dueAt, err := time.Parse(time.RFC3339Nano, item.next)
		if err != nil {
			return err
		}
		interval := time.Duration(item.interval) * time.Second
		if interval <= 0 {
			return fmt.Errorf("invalid persisted trigger interval")
		}
		missed := int64(now.Sub(dueAt) / interval)
		next := dueAt.Add(time.Duration(missed+1) * interval)
		if item.policy == "coalesce" || missed == 0 {
			dedupe := "schedule:" + dueAt.UTC().Format(time.RFC3339Nano)
			_, err = tx.ExecContext(ctx, `INSERT INTO capability_trigger_runs(trigger_run_id,trigger_id,deployment_id,trigger_key,dedupe_key,scheduled_for,mode,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(trigger_id,dedupe_key) DO NOTHING`, uuid.NewString(), item.id, item.deployment, item.key, dedupe, dueAt.UTC().Format(time.RFC3339Nano), item.mode, "queued", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
			if err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_definitions SET next_run_at=?,updated_at=? WHERE trigger_id=? AND next_run_at=?`, next.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), item.id, item.next); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) claimQueuedProgramTrigger(ctx context.Context, tx *sql.Tx, runner contracts.RunnerPrincipal, now time.Time) (*contracts.CapabilityTriggerClaim, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.trigger_run_id,r.trigger_id,r.deployment_id,r.trigger_key,r.event_id,r.scheduled_for,r.event_payload,r.mode,t.release_digest,t.grant_id,t.scopes,d.service_id,d.config_ref,d.config_sha256,g.scope_kind,g.scope_id,g.principal_id,g.actions,g.budget,g.expires_at,rel.manifest FROM capability_trigger_runs r JOIN capability_trigger_definitions t ON t.trigger_id=r.trigger_id JOIN capability_deployments d ON d.deployment_id=r.deployment_id AND d.grant_id=t.grant_id AND d.release_digest=t.release_digest AND d.version=t.expected_deployment_version AND d.config_ref=t.config_ref AND d.config_sha256=t.config_sha256 JOIN capability_grants g ON g.grant_id=t.grant_id AND g.deployment_id=d.deployment_id AND g.release_digest=d.release_digest JOIN capability_releases rel ON rel.digest=t.release_digest WHERE r.state='queued' AND r.mode='program' AND t.enabled=1 AND d.revoked_at IS NULL AND d.desired_state='running' AND d.observed_state='running' AND g.revoked_at IS NULL AND g.expires_at>? AND (SELECT count(*) FROM capability_trigger_runs active WHERE active.trigger_id=t.trigger_id AND active.state IN ('claimed','running','uncertain','waiting')) < CAST(json_extract(g.budget,'$.concurrency') AS INTEGER) ORDER BY r.created_at,r.trigger_run_id LIMIT ?`, now.Format(time.RFC3339Nano), s.limits.Int("capability_trigger_page_rows"))
	if err != nil {
		return nil, err
	}
	type queued struct {
		runID, triggerID, deployment, key, eventID, scheduled, mode, digest, grantID, serviceID, configRef, configHash, scopeKind, scopeID, principalID, expires string
		scopesRaw, actionsRaw, budgetRaw, payload, manifestRaw                                                                                                   []byte
	}
	var items []queued
	for rows.Next() {
		var item queued
		if err := rows.Scan(&item.runID, &item.triggerID, &item.deployment, &item.key, &item.eventID, &item.scheduled, &item.payload, &item.mode, &item.digest, &item.grantID, &item.scopesRaw, &item.serviceID, &item.configRef, &item.configHash, &item.scopeKind, &item.scopeID, &item.principalID, &item.actionsRaw, &item.budgetRaw, &item.expires, &item.manifestRaw); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, item := range items {
		if !control.HasRunnerScope(runner, "capability:deploy") {
			continue
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM capability_trigger_runs WHERE trigger_id=? AND state IN ('claimed','running','uncertain','waiting')`, item.triggerID).Scan(&active); err != nil {
			return nil, err
		}
		var budget contracts.CapabilityResourceBudget
		if err := json.Unmarshal(item.budgetRaw, &budget); err != nil {
			return nil, err
		}
		if active >= budget.Concurrency {
			continue
		}
		var manifest contracts.CapabilityManifest
		var grantActions, scopes []string
		if err := json.Unmarshal(item.manifestRaw, &manifest); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(item.actionsRaw, &grantActions); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(item.scopesRaw, &scopes); err != nil {
			return nil, err
		}
		if err := validateManifest(&manifest, s.limits); err != nil {
			return nil, control.NewAPIError(500, "invalid_claim_snapshot", "trigger claim manifest failed immutable validation", nil)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_definitions SET fence_counter=fence_counter+1 WHERE trigger_id=?`, item.triggerID); err != nil {
			return nil, err
		}
		var fence int64
		if err := tx.QueryRowContext(ctx, `SELECT fence_counter FROM capability_trigger_definitions WHERE trigger_id=?`, item.triggerID).Scan(&fence); err != nil {
			return nil, err
		}
		attempt := uuid.NewString()
		claim := contracts.CapabilityTriggerClaim{DeploymentID: item.deployment, ServiceID: item.serviceID, TriggerID: item.triggerID, TriggerRunID: item.runID, TriggerKey: item.key, EventID: item.eventID, AttemptID: attempt, Fence: fence, ReleaseDigest: item.digest, Manifest: manifest, Mode: item.mode, Scopes: uniqueSorted(scopes), Grant: contracts.CapabilityGrant{GrantID: item.grantID, ScopeKind: item.scopeKind, ScopeID: item.scopeID, PrincipalID: item.principalID, Actions: uniqueSorted(grantActions), Budget: budget}, ConfigRef: item.configRef, ConfigSHA256: item.configHash, EventPayload: item.payload, ScopeKind: item.scopeKind, ScopeID: item.scopeID}
		claim.Grant.ExpiresAt, err = time.Parse(time.RFC3339Nano, item.expires)
		if err != nil {
			return nil, err
		}
		if item.scheduled != "" {
			claim.ScheduledFor, _ = time.Parse(time.RFC3339Nano, item.scheduled)
		}
		body, err := json.Marshal(claim)
		if err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE capability_trigger_runs SET state='claimed',attempt_id=?,fence=?,runner_id=?,claim_json=?,updated_at=? WHERE trigger_run_id=? AND state='queued'`, attempt, fence, runner.RunnerID, body, now.Format(time.RFC3339Nano), item.runID)
		if err != nil {
			return nil, err
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE capability_trigger_definitions SET last_run_at=?,last_run_id=?,updated_at=? WHERE trigger_id=?`, now.Format(time.RFC3339Nano), item.runID, now.Format(time.RFC3339Nano), item.triggerID); err != nil {
			return nil, err
		}
		return &claim, nil
	}
	return nil, nil
}

func (s *Service) observeTrigger(w http.ResponseWriter, r *http.Request) {
	var obs contracts.CapabilityTriggerObservation
	if err := decodeRequest(r, &obs); err != nil {
		control.WriteError(w, control.NewAPIError(400, "invalid_request", "trigger observation JSON is invalid", nil))
		return
	}
	if obs.TriggerRunID != r.PathValue("trigger_run_id") || obs.AttemptID == "" || obs.Fence < 1 || obs.ObservationID == "" || obs.ObservedAt.IsZero() || !validTriggerRunState(obs.State) {
		control.WriteError(w, control.NewAPIError(400, "invalid_observation", "fenced trigger observation is incomplete", nil))
		return
	}
	err := s.server.RunnerTransaction(r.Context(), func(tx *sql.Tx, runner contracts.RunnerPrincipal) error {
		var state, claimedRunner, attempt string
		var fence int64
		var priorBody []byte
		if err := tx.QueryRowContext(r.Context(), `SELECT state,runner_id,attempt_id,fence,observation_json FROM capability_trigger_runs WHERE trigger_run_id=?`, obs.TriggerRunID).Scan(&state, &claimedRunner, &attempt, &fence, &priorBody); errors.Is(err, sql.ErrNoRows) {
			return control.NewAPIError(404, "trigger_run_not_found", "trigger run not found", nil)
		} else if err != nil {
			return err
		}
		if !control.HasRunnerScope(runner, "capability:deploy") {
			return control.NewAPIError(403, "forbidden", "capability:deploy runner scope required", nil)
		}
		if runner.RunnerID != claimedRunner {
			return control.NewAPIError(403, "runner_mismatch", "only the runner that claimed this trigger run may report it", nil)
		}
		body, _ := json.Marshal(obs)
		if obs.ObservationID == observationID(priorBody) && string(priorBody) == string(body) {
			return nil
		}
		if state != "claimed" && state != "running" && state != "uncertain" {
			return control.NewAPIError(409, "stale_trigger_fence", "trigger run is no longer active", nil)
		}
		if attempt != obs.AttemptID || fence != obs.Fence {
			return control.NewAPIError(409, "stale_trigger_fence", "observation does not match active trigger attempt", nil)
		}
		if priorBody != nil {
			var previous contracts.CapabilityTriggerObservation
			if err := json.Unmarshal(priorBody, &previous); err != nil {
				return err
			}
			if previous.ObservationID == obs.ObservationID {
				return control.NewAPIError(409, "observation_conflict", "observation ID was reused with different content", nil)
			}
		}
		_, err := tx.ExecContext(r.Context(), `UPDATE capability_trigger_runs SET state=?,task_id=?,run_id=?,observation_id=?,observation_json=?,last_error_code=?,updated_at=? WHERE trigger_run_id=? AND attempt_id=? AND fence=?`, obs.State, obs.TaskID, obs.RunID, obs.ObservationID, body, obs.ErrorCode, obs.ObservedAt.UTC().Format(time.RFC3339Nano), obs.TriggerRunID, obs.AttemptID, obs.Fence)
		return err
	})
	if err != nil {
		control.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func validTriggerRunState(state string) bool {
	return state == "running" || state == "waiting" || state == "completed" || state == "failed" || state == "uncertain"
}

func observationID(body []byte) string {
	var previous contracts.CapabilityTriggerObservation
	if json.Unmarshal(body, &previous) != nil {
		return ""
	}
	return previous.ObservationID
}
