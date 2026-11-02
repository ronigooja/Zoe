package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/profiles"
)

func (s *Server) handleClaims(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok {
		writeError(w, apiError(401, "unauthenticated", "runner authentication required", nil))
		return
	}
	if !runnerHasAnyWorkspaceScope(p, "runner:claim") {
		writeError(w, apiError(403, "forbidden", "runner lacks claim scope", nil))
		return
	}
	tx, err := s.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback()
	dispatchEnabled, err := DispatchEnabledTx(r.Context(), tx)
	if err != nil {
		writeError(w, apiError(503, "instance_fence_unavailable", "runner claims are disabled until the instance fence is initialized", nil))
		return
	}
	if !dispatchEnabled {
		writeError(w, apiError(503, "instance_dispatch_fenced", "runner claims are disabled by the instance fence", nil))
		return
	}
	// A lost claim response is recovered by returning the same dispatching assignment.
	var runID string
	err = tx.QueryRowContext(r.Context(), `SELECT id FROM runs WHERE runner_id=? AND status='dispatching' ORDER BY started_at LIMIT 1`, p.RunnerID).Scan(&runID)
	if err == nil {
		claim, e := loadClaim(r.Context(), tx, runID, s.cfg)
		if e != nil {
			writeError(w, e)
			return
		}
		if e = tx.Commit(); e != nil {
			writeError(w, e)
			return
		}
		writeJSON(w, 200, claim)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, err)
		return
	}
	var active int
	if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM runs WHERE status IN ('dispatching','active','uncertain')`).Scan(&active); err != nil {
		writeError(w, err)
		return
	}
	if !executionCapacityAvailable(active, s.cfg.MaxRuns) {
		_ = tx.Rollback()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	rows, err := tx.QueryContext(r.Context(), `SELECT id,workspace_id,title,goal,profile_id,harness,version,created_by_device_id,command_id,coalesce(lineage_root_task_id,id),coalesce(lineage_parent_task_id,''),lineage_depth,child_policy_json,pregrant_capabilities_json,source,interaction_state,interaction_expired_kind,coalesce(interaction_question,''),interaction_expected_version FROM tasks WHERE status='queued' AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.task_id=tasks.id AND r.status IN ('dispatching','active','uncertain')) AND NOT EXISTS (SELECT 1 FROM task_dependencies d WHERE d.task_id=tasks.id AND d.status<>'satisfied') ORDER BY created_at,id`)
	if err != nil {
		writeError(w, err)
		return
	}
	type candidate struct {
		id, workspace, title, goal, profile, harness, device, command string
		source                                                        string
		root, parent                                                  string
		depth                                                         int
		childPolicyJSON, pregrantJSON                                 []byte
		version                                                       int64
		interaction, interactionExpiredKind, interactionQuestion      string
		interactionExpectedVersion                                    int64
	}
	var chosen *candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.workspace, &c.title, &c.goal, &c.profile, &c.harness, &c.version, &c.device, &c.command, &c.root, &c.parent, &c.depth, &c.childPolicyJSON, &c.pregrantJSON, &c.source, &c.interaction, &c.interactionExpiredKind, &c.interactionQuestion, &c.interactionExpectedVersion); err != nil {
			rows.Close()
			writeError(w, err)
			return
		}
		if !HasRunnerScope(p, "runner:claim", c.workspace) {
			continue
		}
		profile, exists := s.cfg.Profiles[c.profile]
		if !exists {
			continue
		}
		if c.harness == "" || c.harness != "opencode" {
			continue
		}
		if profile.IsolationPolicy == "" || profile.Harness != c.harness {
			continue
		}
		chosen = &c
		break
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		writeError(w, err)
		return
	}
	rows.Close()
	if chosen == nil {
		_ = tx.Rollback()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if reason, guardErr := s.taskDispatchReasonTx(r.Context(), tx, chosen.source, chosen.workspace, chosen.id, time.Now().UTC()); guardErr != nil {
		writeError(w, guardErr)
		return
	} else if reason != "" {
		now := time.Now().UTC()
		at := now.Format(time.RFC3339Nano)
		result, updateErr := tx.ExecContext(r.Context(), `UPDATE tasks SET status='canceled',version=version+1,finished_at=?,updated_at=? WHERE id=? AND workspace_id=? AND status='queued' AND version=?`, at, at, chosen.id, chosen.workspace, chosen.version)
		if updateErr != nil {
			writeError(w, updateErr)
			return
		}
		changed, updateErr := result.RowsAffected()
		if updateErr != nil || changed != 1 {
			if updateErr == nil {
				updateErr = errors.New("task changed while dispatch authorization was being fenced")
			}
			writeError(w, updateErr)
			return
		}
		if err = appendEventTx(r.Context(), tx, contracts.Event{Type: "task.canceled", OccurredAt: now, WorkspaceID: chosen.workspace, AggregateType: "task", AggregateID: chosen.id, AggregateVersion: chosen.version + 1, Payload: raw(map[string]any{"task_id": chosen.id, "reason_code": reason})}); err != nil {
			writeError(w, err)
			return
		}
		if err = tx.Commit(); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	profile := s.cfg.Profiles[chosen.profile]
	budget := contracts.RunBudgetSnapshot{HardDeadline: time.Now().UTC().Add(4 * time.Hour), MaxModelCalls: 32, MaxInputUnits: 500_000, MaxOutputUnits: 100_000, MaxToolSteps: 128, MaxArtifactBytes: 16 << 20}
	budgetJSON, err := json.Marshal(budget)
	if err != nil {
		writeError(w, err)
		return
	}
	runID, err = randomID()
	if err != nil {
		writeError(w, err)
		return
	}
	attemptID, err := randomID()
	if err != nil {
		writeError(w, err)
		return
	}
	var fence uint64
	if err = tx.QueryRowContext(r.Context(), `SELECT coalesce(max(fence),0)+1 FROM runs`).Scan(&fence); err != nil {
		writeError(w, err)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	newVersion := chosen.version + 1
	policyBytes := chosen.childPolicyJSON
	if len(policyBytes) == 0 {
		policyBytes = []byte(`{}`)
	}
	pregrant := []string{}
	if err = json.Unmarshal(chosen.pregrantJSON, &pregrant); err != nil {
		writeError(w, apiError(500, "internal", "persisted child pregrant is invalid", nil))
		return
	}
	capabilityIDs := profile.CapabilityIDs
	if chosen.parent != "" {
		capabilityIDs = intersectStrings(capabilityIDs, pregrant)
	}
	rules, impact, err := profiles.EffectivePermissionSet(profiles.Profile{ID: profile.ID, PermissionTier: profile.PermissionTier}, profile.Scopes, nil)
	if err != nil {
		writeError(w, err)
		return
	}
	authorization := contracts.JobAuthorization{CapabilityIDs: append([]string{}, capabilityIDs...), Scopes: append([]string{}, profile.Scopes...), PermissionTier: profile.PermissionTier, PermissionRules: rules, ImpactLevel: impact}
	authorizationJSON, err := json.Marshal(authorization)
	if err != nil {
		writeError(w, err)
		return
	}
	_, err = tx.ExecContext(r.Context(), `INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,isolation_policy,started_at,task_version,authorization_json,child_policy_json,lineage_root_task_id,lineage_parent_task_id,lineage_depth,budget_snapshot_json) VALUES(?,?,?,?,'dispatching','run',1,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, runID, chosen.workspace, chosen.id, attemptID, fence, p.RunnerID, chosen.device, chosen.command, chosen.goal, profile.ID, chosen.harness, profile.RuntimeRouteID, profile.ModelRouteID, profile.Role, profile.IsolationPolicy, now, newVersion, authorizationJSON, policyBytes, chosen.root, nullString(chosen.parent), chosen.depth, budgetJSON)
	if err != nil {
		writeError(w, err)
		return
	}
	_, err = tx.ExecContext(r.Context(), `UPDATE tasks SET status='active',version=?,started_at=coalesce(started_at,?),updated_at=? WHERE id=? AND workspace_id=? AND status='queued' AND version=?`, newVersion, now, now, chosen.id, chosen.workspace, chosen.version)
	if err != nil {
		writeError(w, err)
		return
	}
	if err = appendEventTx(r.Context(), tx, contracts.Event{Type: "run.claimed", OccurredAt: time.Now().UTC(), WorkspaceID: chosen.workspace, SubjectID: "", AggregateType: "task", AggregateID: chosen.id, AggregateVersion: newVersion, Payload: raw(map[string]any{"task_id": chosen.id, "run_id": runID, "attempt_id": attemptID})}); err != nil {
		writeError(w, err)
		return
	}
	claim := contracts.JobClaim{RunID: runID, TaskID: chosen.id, AttemptID: attemptID, WorkspaceID: chosen.workspace, TaskVersion: newVersion, Goal: chosen.goal, ProfileID: profile.ID, Harness: chosen.harness, RuntimeRouteID: profile.RuntimeRouteID, ModelRouteID: profile.ModelRouteID, IsolationPolicy: profile.IsolationPolicy, Role: profile.Role, Agent: profile.Agent, PermissionTier: profile.PermissionTier, PermissionRules: rules, ImpactLevel: impact, Authorization: authorization, Fence: fence, LineageRootTaskID: chosen.root, LineageParentTaskID: chosen.parent, LineageDepth: chosen.depth, PregrantCapabilityIDs: append([]string{}, pregrant...), Budget: budget}
	if chosen.interactionExpiredKind != "" {
		claim.ResumeQuestion = chosen.interactionQuestion
		if chosen.interactionExpiredKind == "question" && chosen.version > chosen.interactionExpectedVersion {
			_ = tx.QueryRowContext(r.Context(), `SELECT coalesce(content,'') FROM task_entries WHERE task_id=? AND kind='user_message' ORDER BY created_at DESC,id DESC LIMIT 1`, chosen.id).Scan(&claim.ResumeAnswer)
		}
	}
	if err = tx.Commit(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, claim)
}

func runnerHasAnyWorkspaceScope(p contracts.RunnerPrincipal, scope string) bool {
	if len(p.WorkspaceIDs) == 0 {
		return false
	}
	for _, workspace := range p.WorkspaceIDs {
		if HasRunnerScope(p, scope, workspace) {
			return true
		}
	}
	return false
}

func intersectStrings(granted, requested []string) []string {
	out := make([]string, 0, len(requested))
	for _, value := range requested {
		if containsString(granted, value) && !containsString(out, value) {
			out = append(out, value)
		}
	}
	return out
}

func loadClaim(ctx context.Context, tx *sql.Tx, runID string, cfg Config) (contracts.JobClaim, error) {
	_ = cfg
	var c contracts.JobClaim
	var fence uint64
	var authJSON []byte
	var budgetJSON []byte
	err := tx.QueryRowContext(ctx, `SELECT r.id,r.task_id,r.attempt_id,r.workspace_id,r.task_version,r.goal,r.profile_id,r.harness,r.runtime_route_id,r.model_route_id,r.role,r.isolation_policy,r.runtime_session_id,r.fence,r.authorization_json,coalesce(r.lineage_root_task_id,''),coalesce(r.lineage_parent_task_id,''),r.lineage_depth,r.budget_snapshot_json FROM runs r WHERE r.id=?`, runID).Scan(&c.RunID, &c.TaskID, &c.AttemptID, &c.WorkspaceID, &c.TaskVersion, &c.Goal, &c.ProfileID, &c.Harness, &c.RuntimeRouteID, &c.ModelRouteID, &c.Role, &c.IsolationPolicy, &c.RuntimeSessionID, &fence, &authJSON, &c.LineageRootTaskID, &c.LineageParentTaskID, &c.LineageDepth, &budgetJSON)
	if err != nil {
		return c, err
	}
	if c.Harness == "" || c.Harness != "opencode" {
		return c, errors.New("persisted run has unsupported harness")
	}
	c.Fence = fence
	if err := json.Unmarshal(authJSON, &c.Authorization); err != nil {
		return c, fmt.Errorf("decode persisted run authorization: %w", err)
	}
	c.PermissionTier, c.PermissionRules, c.ImpactLevel = c.Authorization.PermissionTier, c.Authorization.PermissionRules, c.Authorization.ImpactLevel
	if profile, ok := cfg.Profiles[c.ProfileID]; ok {
		c.Agent = profile.Agent
	}
	if err := json.Unmarshal(budgetJSON, &c.Budget); err != nil {
		return c, fmt.Errorf("decode persisted run budget: %w", err)
	}
	if c.LineageParentTaskID != "" {
		c.PregrantCapabilityIDs = append([]string{}, c.Authorization.CapabilityIDs...)
	} else {
		c.PregrantCapabilityIDs = []string{}
	}
	var interaction string
	if err := tx.QueryRowContext(ctx, `SELECT interaction_state FROM tasks WHERE id=?`, c.TaskID).Scan(&interaction); err != nil {
		return c, err
	}
	var expiredKind string
	if err := tx.QueryRowContext(ctx, `SELECT interaction_expired_kind FROM tasks WHERE id=?`, c.TaskID).Scan(&expiredKind); err != nil {
		return c, err
	}
	if expiredKind != "" {
		var expected, current int64
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(interaction_question,''),interaction_expected_version,version FROM tasks WHERE id=?`, c.TaskID).Scan(&c.ResumeQuestion, &expected, &current); err != nil {
			return c, err
		}
		if current > expected {
			if err := tx.QueryRowContext(ctx, `SELECT coalesce(content,'') FROM task_entries WHERE task_id=? AND kind='user_message' ORDER BY created_at DESC,id DESC LIMIT 1`, c.TaskID).Scan(&c.ResumeAnswer); err != nil {
				return c, err
			}
		}
		if len(c.ResumeQuestion) > 4000 {
			c.ResumeQuestion = c.ResumeQuestion[:4000]
		}
		if len(c.ResumeAnswer) > 20000 {
			c.ResumeAnswer = c.ResumeAnswer[:20000]
		}
	}
	return c, nil
}

func (s *Server) handleCurrentClaims(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok || !runnerHasAnyWorkspaceScope(p, "runner:control:read") {
		writeError(w, apiError(403, "forbidden", "runner lacks current-claim read scope", nil))
		return
	}
	rows, err := s.store.DB().QueryContext(r.Context(), `SELECT id FROM runs WHERE runner_id=? AND status IN ('dispatching','active','uncertain') ORDER BY started_at`, p.RunnerID)
	if err != nil {
		writeError(w, err)
		return
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			writeError(w, err)
			return
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		writeError(w, err)
		return
	}
	if err = rows.Close(); err != nil {
		writeError(w, err)
		return
	}
	recovery := contracts.ClaimRecovery{Claims: []contracts.JobClaim{}}
	for _, id := range ids {
		var claim contracts.JobClaim
		var authJSON []byte
		var budgetJSON []byte
		err = s.store.DB().QueryRowContext(r.Context(), `SELECT r.id,r.task_id,r.attempt_id,r.workspace_id,r.task_version,r.goal,r.profile_id,r.harness,r.runtime_route_id,r.model_route_id,r.role,r.isolation_policy,r.runtime_session_id,r.fence,r.authorization_json,coalesce(r.lineage_root_task_id,''),coalesce(r.lineage_parent_task_id,''),r.lineage_depth,r.budget_snapshot_json FROM runs r WHERE r.id=?`, id).Scan(&claim.RunID, &claim.TaskID, &claim.AttemptID, &claim.WorkspaceID, &claim.TaskVersion, &claim.Goal, &claim.ProfileID, &claim.Harness, &claim.RuntimeRouteID, &claim.ModelRouteID, &claim.Role, &claim.IsolationPolicy, &claim.RuntimeSessionID, &claim.Fence, &authJSON, &claim.LineageRootTaskID, &claim.LineageParentTaskID, &claim.LineageDepth, &budgetJSON)
		if err != nil {
			writeError(w, err)
			return
		}
		if claim.Harness == "" || claim.Harness != "opencode" {
			writeError(w, apiError(409, "unsupported_harness", "persisted run has unsupported harness", nil))
			return
		}
		if err = json.Unmarshal(authJSON, &claim.Authorization); err != nil {
			writeError(w, apiError(500, "internal", "persisted run authorization is invalid", nil))
			return
		}
		claim.PermissionTier, claim.PermissionRules, claim.ImpactLevel = claim.Authorization.PermissionTier, claim.Authorization.PermissionRules, claim.Authorization.ImpactLevel
		if profile, ok := s.cfg.Profiles[claim.ProfileID]; ok {
			claim.Agent = profile.Agent
		}
		if err = json.Unmarshal(budgetJSON, &claim.Budget); err != nil {
			writeError(w, apiError(500, "internal", "persisted run budget is invalid", nil))
			return
		}
		if claim.LineageParentTaskID != "" {
			claim.PregrantCapabilityIDs = append([]string{}, claim.Authorization.CapabilityIDs...)
		} else {
			claim.PregrantCapabilityIDs = []string{}
		}
		recovery.Claims = append(recovery.Claims, claim)
	}
	writeJSON(w, 200, recovery)
}

func (s *Server) handleRunControl(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok || !runnerHasAnyWorkspaceScope(p, "runner:control:read") {
		writeError(w, apiError(403, "forbidden", "runner lacks control read scope", nil))
		return
	}
	var control contracts.RunControl
	err := s.store.DB().QueryRowContext(r.Context(), `SELECT id,desired_control,control_version FROM runs WHERE id=? AND runner_id=? AND status IN ('dispatching','active','uncertain')`, r.PathValue("run_id"), p.RunnerID).Scan(&control.RunID, &control.DesiredControl, &control.ControlVersion)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, apiError(404, "not_found", "active run not found", nil))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, control)
}

// handleRuntimeAuthorization is the only path that can mint a one-time
// authorization for a runner. The runner cannot self-authorize: this endpoint
// returns no record until a device command has created a pending action.
func (s *Server) handleRuntimeAuthorization(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok || !runnerHasAnyWorkspaceScope(p, "runner:control:read") {
		writeError(w, apiError(403, "forbidden", "runner lacks authorization read scope", nil))
		return
	}
	runID, requestID := r.PathValue("run_id"), r.PathValue("request_id")
	fence, err := strconv.ParseUint(r.URL.Query().Get("fence"), 10, 64)
	if err != nil || fence == 0 {
		writeError(w, apiError(400, "invalid_request", "fence is required", nil))
		return
	}
	var out contracts.RuntimeAuthorization
	var used int
	var expires string
	query := `SELECT a.run_id,a.request_id,a.fence,a.decision,a.expires_at,a.authorization_used,a.question FROM pending_runtime_actions a JOIN runs r ON r.id=a.run_id WHERE a.run_id=? AND a.fence=? AND r.runner_id=?`
	args := []any{runID, fence, p.RunnerID}
	if requestID != "" {
		query += ` AND a.request_id=?`
		args = append(args, requestID)
	} else {
		query += ` AND a.kind='worker_question'`
	}
	query += ` ORDER BY a.created_at LIMIT 1`
	var message string
	err = s.store.DB().QueryRowContext(r.Context(), query, args...).Scan(&out.RunID, &out.RequestID, &out.Fence, &out.Decision, &expires, &used, &message)
	out.Message = message
	if errors.Is(err, sql.ErrNoRows) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if used != 0 || out.Decision == "" || time.Now().UTC().After(parseTime(expires)) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	result, err := s.store.DB().ExecContext(r.Context(), `UPDATE pending_runtime_actions SET authorization_used=1 WHERE run_id=? AND request_id=? AND fence=? AND authorization_used=0`, runID, requestID, fence)
	if err != nil {
		writeError(w, err)
		return
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleObservation(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok || !runnerHasAnyWorkspaceScope(p, "runner:observe") {
		writeError(w, apiError(403, "forbidden", "runner lacks observation scope", nil))
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		writeError(w, apiError(400, "invalid_request", "Idempotency-Key is required", nil))
		return
	}
	var observation contracts.RunnerObservation
	if err := decodeJSON(r, &observation); err != nil {
		writeError(w, err)
		return
	}
	runID := r.PathValue("run_id")
	if observation.RunID == "" || observation.RunID != runID || observation.ObservationID == "" || observation.AttemptID == "" || observation.Fence == 0 {
		writeError(w, apiError(400, "invalid_request", "run_id, observation_id, attempt_id, and fence are required", nil))
		return
	}
	if !validObservationKind(observation.Kind) {
		writeError(w, apiError(400, "invalid_request", "observation kind is not supported", nil))
		return
	}
	canonical, err := json.Marshal(observation)
	if err != nil {
		writeError(w, err)
		return
	}
	bodyHash := sha256.Sum256(canonical)
	tx, err := s.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback()
	var workspace, taskID, attempt, runnerID, status, desired, boundSession, boundHarness, authorizedDevice string
	var fence uint64
	err = tx.QueryRowContext(r.Context(), `SELECT workspace_id,task_id,attempt_id,runner_id,status,desired_control,fence,runtime_session_id,harness,authorized_device_id FROM runs WHERE id=?`, runID).Scan(&workspace, &taskID, &attempt, &runnerID, &status, &desired, &fence, &boundSession, &boundHarness, &authorizedDevice)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, apiError(404, "not_found", "run not found", nil))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if runnerID != p.RunnerID || !HasRunnerScope(p, "runner:observe", workspace) {
		writeError(w, apiError(403, "forbidden", "runner does not own this run", nil))
		return
	}
	if observation.AttemptID != attempt || observation.Fence != fence {
		writeError(w, apiError(409, "fence_conflict", "observation does not match active attempt fence", map[string]any{"attempt_id": attempt, "fence": fence}))
		return
	}
	var oldHash []byte
	err = tx.QueryRowContext(r.Context(), `SELECT body_hash FROM runner_observations WHERE run_id=? AND runner_id=? AND idempotency_key=?`, runID, p.RunnerID, key).Scan(&oldHash)
	if err == nil {
		if !sameHash(oldHash, bodyHash[:]) {
			writeError(w, apiError(409, "idempotency_conflict", "observation key reused with different content", nil))
			return
		}
		if err = tx.Commit(); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "recorded"})
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, err)
		return
	}
	if status == "verified" || status == "stopped" || status == "failed" {
		writeError(w, apiError(409, "state_conflict", "run is terminal", map[string]any{"status": status}))
		return
	}
	if observation.Kind == "stop_confirmed" && desired != "stop" {
		writeError(w, apiError(409, "state_conflict", "stop confirmation requires desired_control=stop", nil))
		return
	}
	if observation.Kind == "stop_confirmed" || observation.Kind == "runtime_terminated" {
		if boundSession == "" || observation.RuntimeSessionID != boundSession {
			writeError(w, apiError(409, "session_conflict", "termination observation must match the bound runtime session", nil))
			return
		}
		proof := observation.TerminationProof
		_, offset := proofTimeZone(proof)
		if boundHarness == "" {
			boundHarness = "opencode"
		}
		hostProof := proof != nil && proof.Source == "runner_host_session" && proof.Method == "session_interrupted" && proof.RuntimeContainerID == ""
		legacyProof := proof != nil && proof.Source == "runner_container_removal" && proof.Method == "container_removed" && proof.RuntimeContainerID != ""
		if boundHarness != "opencode" || proof == nil || (!hostProof && !legacyProof) || proof.RuntimeSessionID != boundSession || !proof.CleanupComplete || proof.VerifiedAt.IsZero() || offset != 0 || proof.VerifiedAt.After(time.Now().UTC().Add(5*time.Minute)) {
			writeError(w, apiError(409, "termination_unproven", "termination requires verified host-session interruption for the bound session", nil))
			return
		}
	}
	if observation.Kind == "no_effect_terminal" {
		reconciled := observation.NoEffectProof != nil && observation.NoEffectProof.Reconciliation != nil
		if !reconciled && status != "dispatching" {
			writeError(w, apiError(409, "state_conflict", "no-effect termination requires a run that has not started dispatch", nil))
			return
		}
		proof := observation.NoEffectProof
		if proof == nil || (!reconciled && (proof.JournalPhase != "claimed" || proof.RuntimeCreateStarted)) || proof.RuntimeSessionID != "" || proof.RuntimeContainerID != "" || (proof.Outcome != "stopped" && proof.Outcome != "failed") || observation.RuntimeSessionID != "" || boundSession != "" || len(observation.ExternalEffects) != 0 || observation.TerminationProof != nil {
			writeError(w, apiError(409, "no_effect_unproven", "no-effect termination requires a claimed journal entry with no runtime binding or external effects", nil))
			return
		}
		if proof.Outcome == "stopped" && desired != "stop" {
			writeError(w, apiError(409, "state_conflict", "no-effect stop confirmation requires desired_control=stop", nil))
			return
		}
		var priorDispatch, priorEffects int
		if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM runner_observations WHERE run_id=? AND attempt_id=? AND fence=? AND kind IN ('dispatch_started','dispatch_bound','effect_unknown')`, runID, attempt, fence).Scan(&priorDispatch); err != nil {
			writeError(w, err)
			return
		}
		if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM external_effects WHERE run_id=?`, runID).Scan(&priorEffects); err != nil {
			writeError(w, err)
			return
		}
		if !reconciled && (priorDispatch != 0 || priorEffects != 0) {
			writeError(w, apiError(409, "no_effect_unproven", "run history contains dispatch or external-effect evidence", nil))
			return
		}
		if reconciled {
			evidence := proof.Reconciliation
			_, offset := evidence.VerifiedAt.Zone()
			_, journalOffset := evidence.JournalUpdatedAt.Zone()
			instance, instanceErr := GetInstanceState(r.Context(), tx)
			if instanceErr != nil {
				writeError(w, instanceErr)
				return
			}
			if evidence.JournalUpdatedAt.IsZero() || journalOffset != 0 || evidence.JournalUpdatedAt.After(evidence.VerifiedAt) || evidence.JournalUpdatedAt.After(time.Now().UTC()) || status != "uncertain" || proof.JournalPhase != "effect_unknown" || !proof.RuntimeCreateStarted || proof.Outcome != "failed" || evidence.RunID != runID || strings.TrimSpace(evidence.Actor) == "" || len(evidence.Actor) > 200 || strings.ContainsAny(evidence.Actor, "\r\n") || !evidence.Confirmed || evidence.VerifiedAt.IsZero() || offset != 0 || evidence.VerifiedAt.After(time.Now().UTC().Add(5*time.Minute)) || evidence.RuntimeQuery != "docker ps --filter io.zoe.run/attempt/fence" || !evidence.RuntimeInventoryComplete || evidence.RuntimeMatches != 0 || evidence.DockerQuery != "docker ps -aq --no-trunc; docker inspect all IDs" || !evidence.DockerInventoryComplete || evidence.DockerRuntimeCount != 0 || evidence.InstanceID != instance.InstanceID || evidence.InstanceEpoch != instance.Epoch {
				writeError(w, apiError(409, "no_effect_unproven", "explicit reconciliation requires complete runtime-label and Docker absence proof under the current instance fence", nil))
				return
			}
			var runStartedAtText string
			if err = tx.QueryRowContext(r.Context(), `SELECT started_at FROM runs WHERE id=?`, runID).Scan(&runStartedAtText); err != nil {
				writeError(w, err)
				return
			}
			runStartedAt, parseErr := time.Parse(time.RFC3339Nano, runStartedAtText)
			if parseErr != nil || !validReconciliationContainerEvidence(evidence, runStartedAt) {
				writeError(w, apiError(409, "no_effect_unproven", "Docker reconciliation requires two unchanged complete inventories and explicit verified preserved records", nil))
				return
			}
			var unknown, bound, unsafeEffects int
			if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM runner_observations WHERE run_id=? AND attempt_id=? AND fence=? AND kind='effect_unknown'`, runID, attempt, fence).Scan(&unknown); err != nil {
				writeError(w, err)
				return
			}
			if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM runner_observations WHERE run_id=? AND attempt_id=? AND fence=? AND kind NOT IN ('dispatch_started','effect_unknown')`, runID, attempt, fence).Scan(&bound); err != nil {
				writeError(w, err)
				return
			}
			if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM external_effects WHERE run_id=? AND (kind != 'runtime_create' OR status!='unknown' OR external_reference!='')`, runID).Scan(&unsafeEffects); err != nil {
				writeError(w, err)
				return
			}
			if unknown == 0 || bound != 0 || unsafeEffects != 0 {
				writeError(w, apiError(409, "no_effect_unproven", "reconciliation history contains binding or effects beyond unknown creation", nil))
				return
			}
			details, marshalErr := json.Marshal(proof)
			if marshalErr != nil {
				writeError(w, marshalErr)
				return
			}
			if _, err = tx.ExecContext(r.Context(), `UPDATE external_effects SET status=?,last_observed_at=?,details=? WHERE run_id=? AND kind='runtime_create' AND status='unknown' AND external_reference=''`, reconciliationEffectStatus(evidence), time.Now().UTC().Format(time.RFC3339Nano), details, runID); err != nil {
				writeError(w, err)
				return
			}
		}
	}

	if observation.Kind == "create_rejected" {
		proof := observation.CreateRejection
		if status != "active" || proof == nil || proof.Code == "" || boundSession != "" || observation.RuntimeSessionID != "" || len(observation.ExternalEffects) != 0 || observation.NoEffectProof != nil || observation.TerminationProof != nil || observation.BudgetReservationID != "" && observation.BudgetReservationID != "runtime-initial-prompt" {
			writeError(w, apiError(409, "create_rejection_unproven", "runtime create rejection requires an active unbound run and sanitized rejection proof", nil))
			return
		}
		if proof.Code != "invalid_request" && proof.Code != "session_create_rejected" && proof.Code != "image_missing" && proof.Code != "container_create_rejected" {
			writeError(w, apiError(409, "create_rejection_unproven", "runtime create rejection code is not allowlisted", nil))
			return
		}
		var started, bound, effects int
		if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM runner_observations WHERE run_id=? AND attempt_id=? AND fence=? AND kind='dispatch_started'`, runID, attempt, fence).Scan(&started); err != nil {
			writeError(w, err)
			return
		}
		if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM runner_observations WHERE run_id=? AND attempt_id=? AND fence=? AND kind IN ('dispatch_bound','effect_unknown')`, runID, attempt, fence).Scan(&bound); err != nil {
			writeError(w, err)
			return
		}
		if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM external_effects WHERE run_id=?`, runID).Scan(&effects); err != nil {
			writeError(w, err)
			return
		}
		if started == 0 || bound != 0 || effects != 0 {
			writeError(w, apiError(409, "create_rejection_unproven", "run history contains no dispatch start or contains a binding or external effect", nil))
			return
		}
		// This trusted, same-fence rejection is proof that the initial prompt
		// never reached the runtime. Release only its reserved call; unknown session work
		// and every other usage reservation remain charged conservatively.
		if observation.BudgetReservationID != "" {
			_, err = tx.ExecContext(r.Context(), `UPDATE run_budget_reservations SET status='settled',actual_json='{}',settlement_id=?,settlement_hash=?,updated_at=? WHERE run_id=? AND reservation_id=? AND attempt_id=? AND fence=? AND status IN ('reserved','unknown')`, "runtime-create-rejected:"+observation.ObservationID, bodyHash[:], time.Now().UTC().Format(time.RFC3339Nano), runID, observation.BudgetReservationID, attempt, fence)
			if err != nil {
				writeError(w, err)
				return
			}
		}
	}
	var taskVersion int64
	var interactionState string
	if err = tx.QueryRowContext(r.Context(), `SELECT interaction_state FROM tasks WHERE id=? AND workspace_id=?`, taskID, workspace).Scan(&interactionState); err != nil {
		writeError(w, err)
		return
	}
	nextRunStatus := status
	nextTaskStatus := ""
	if observation.Kind == "permission_requested" || observation.Kind == "worker_question" {
		now := time.Now().UTC()
		if err = tx.QueryRowContext(r.Context(), `SELECT version FROM tasks WHERE id=? AND workspace_id=?`, taskID, workspace).Scan(&taskVersion); err != nil {
			writeError(w, err)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(observation.Result, &payload); err != nil {
			writeError(w, apiError(400, "invalid_request", "pending action payload is invalid", nil))
			return
		}
		requestID, _ := payload["request_id"].(string)
		if requestID == "" {
			writeError(w, apiError(400, "invalid_request", "pending action request id is required", nil))
			return
		}
		action, _ := payload["action"].(string)
		question, _ := payload["question"].(string)
		resources := []string{}
		if values, ok := payload["resources"].([]any); ok {
			for _, value := range values {
				if text, ok := value.(string); ok {
					resources = append(resources, text)
				}
			}
		}
		canonical, _ := json.Marshal(map[string]any{"run_id": runID, "request_id": requestID, "action": action, "resources": resources, "question": question})
		digest := fmt.Sprintf("%x", sha256.Sum256(canonical))
		id, idErr := randomID()
		if idErr != nil {
			writeError(w, idErr)
			return
		}
		expires := time.Now().UTC().Add(s.cfg.WaitLimit).Format(time.RFC3339Nano)
		resourcesJSON, _ := json.Marshal(resources)
		_, err = tx.ExecContext(r.Context(), `INSERT INTO pending_runtime_actions(id,workspace_id,task_id,run_id,fence,request_id,kind,action,resources_json,question,action_digest,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id,request_id) DO NOTHING`, id, workspace, taskID, runID, fence, requestID, observation.Kind, action, resourcesJSON, question, digest, expires, now.Format(time.RFC3339Nano))
		if err != nil {
			writeError(w, err)
			return
		}
		// Keep the run active while its OpenCode session remains alive; the
		// pending-action row is the durable waiting marker.
		nextRunStatus, nextTaskStatus = "active", "awaiting_user"
		if _, err = tx.ExecContext(r.Context(), `UPDATE runs SET waiting_since=?,last_observation_at=? WHERE id=?`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), runID); err != nil {
			writeError(w, err)
			return
		}
		taskVersion++
		if _, err = tx.ExecContext(r.Context(), `UPDATE tasks SET status='awaiting_user',interaction_state=?,interaction_run_id=?,interaction_action_digest=?,interaction_expires_at=?,interaction_expected_version=?,interaction_question=?,version=version+1,updated_at=? WHERE id=? AND workspace_id=?`, map[bool]string{true: "explicit_confirmation", false: "awaiting_message"}[observation.Kind == "permission_requested"], runID, digest, expires, taskVersion, question, now.Format(time.RFC3339Nano), taskID, workspace); err != nil {
			writeError(w, err)
			return
		}
		taskVersion++
	}
	now := time.Now().UTC()
	if observation.OccurredAt.IsZero() {
		observation.OccurredAt = now
	}
	if observation.OccurredAt.After(now.Add(5 * time.Minute)) {
		writeError(w, apiError(400, "invalid_request", "observation timestamp is too far in the future", nil))
		return
	}
	_, err = tx.ExecContext(r.Context(), `INSERT INTO runner_observations(observation_id,idempotency_key,run_id,attempt_id,fence,runner_id,kind,occurred_at,body_hash,body) VALUES(?,?,?,?,?,?,?,?,?,?)`, observation.ObservationID, key, runID, attempt, fence, p.RunnerID, observation.Kind, now.Format(time.RFC3339Nano), bodyHash[:], canonical)
	if err != nil {
		writeError(w, err)
		return
	}
	if err = tx.QueryRowContext(r.Context(), `SELECT version FROM tasks WHERE id=? AND workspace_id=?`, taskID, workspace).Scan(&taskVersion); err != nil {
		writeError(w, err)
		return
	}
	eventKind := "run." + observation.Kind
	var taskResult string
	provenance, _ := json.Marshal(observation)
	for _, item := range observation.Evidence {
		eid, e := randomID()
		if e != nil {
			writeError(w, e)
			return
		}
		_, e = tx.ExecContext(r.Context(), `INSERT INTO evidence(id,workspace_id,task_id,run_id,kind,summary,source,provenance,created_at,verified,verifier_id) VALUES(?,?,?,?,?,?, 'runner',?,?,0,'')`, eid, workspace, taskID, runID, item.Kind, item.Summary, provenance, now.Format(time.RFC3339Nano))
		if e != nil {
			writeError(w, e)
			return
		}
	}
	if observation.Kind == "result" {
		summary := string(observation.Result)
		if len(summary) > 20000 {
			summary = summary[:20000]
		}
		if len(observation.Result) == 0 || !json.Valid(observation.Result) {
			writeError(w, apiError(400, "invalid_request", "result must be valid JSON", nil))
			return
		}
		// Runner results are structured for durable provenance, but the task
		// result shown in the native clients is the assistant's text. Keep the
		// structured payload in the evidence row and avoid exposing its JSON
		// envelope as the chat reply.
		taskResult = summary
		var resultFields map[string]json.RawMessage
		var assistantOutput string
		if json.Unmarshal(observation.Result, &resultFields) == nil {
			if raw, ok := resultFields["assistant_output"]; ok && json.Unmarshal(raw, &assistantOutput) == nil && assistantOutput != "" {
				taskResult = assistantOutput
			}
		}
		eid, e := randomID()
		if e != nil {
			writeError(w, e)
			return
		}
		_, e = tx.ExecContext(r.Context(), `INSERT INTO evidence(id,workspace_id,task_id,run_id,kind,summary,source,provenance,created_at,verified,verifier_id) VALUES(?,?,?,?, 'agent_result',?,'runner',?,?,0,'')`, eid, workspace, taskID, runID, summary, provenance, now.Format(time.RFC3339Nano))
		if e != nil {
			writeError(w, e)
			return
		}
		_, err = tx.ExecContext(r.Context(), `UPDATE tasks SET result=?,version=version+1,updated_at=? WHERE id=? AND workspace_id=?`, taskResult, now.Format(time.RFC3339Nano), taskID, workspace)
		if err != nil {
			writeError(w, err)
			return
		}
		taskVersion++
		if assistantOutput != "" {
			entryID, entryErr := randomID()
			if entryErr != nil {
				writeError(w, entryErr)
				return
			}
			if _, entryErr = tx.ExecContext(r.Context(), `INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES(?,?,?,'assistant_message',?,?,?)`, entryID, taskID, workspace, assistantOutput, authorizedDevice, now.Format(time.RFC3339Nano)); entryErr != nil {
				writeError(w, entryErr)
				return
			}
		}
	}
	if observation.Kind == "failure" {
		_, err = tx.ExecContext(r.Context(), `UPDATE tasks SET error=?,version=version+1,updated_at=? WHERE id=? AND workspace_id=?`, observation.Error, now.Format(time.RFC3339Nano), taskID, workspace)
		if err != nil {
			writeError(w, err)
			return
		}
		taskVersion++
	}
	if observation.Kind == "no_effect_terminal" && observation.NoEffectProof.Outcome == "failed" {
		_, err = tx.ExecContext(r.Context(), `UPDATE tasks SET error=?,version=version+1,updated_at=? WHERE id=? AND workspace_id=?`, observation.Error, now.Format(time.RFC3339Nano), taskID, workspace)
		if err != nil {
			writeError(w, err)
			return
		}
		taskVersion++
	}
	if observation.Kind == "create_rejected" {
		_, err = tx.ExecContext(r.Context(), `UPDATE tasks SET error=?,version=version+1,updated_at=? WHERE id=? AND workspace_id=?`, "runtime_create_rejected", now.Format(time.RFC3339Nano), taskID, workspace)
		if err != nil {
			writeError(w, err)
			return
		}
		taskVersion++
	}
	for _, effect := range observation.ExternalEffects {
		details, _ := json.Marshal(effect)
		if effect.ID == "" || effect.Kind == "" {
			writeError(w, apiError(400, "invalid_request", "external effect requires id and kind", nil))
			return
		}
		_, err = tx.ExecContext(r.Context(), `INSERT INTO external_effects(id,workspace_id,task_id,run_id,kind,status,external_reference,first_observed_at,last_observed_at,details) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status,external_reference=excluded.external_reference,last_observed_at=excluded.last_observed_at,details=excluded.details WHERE external_effects.run_id=excluded.run_id`, effect.ID, workspace, taskID, runID, effect.Kind, effect.Status, effect.Reference, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), details)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	switch observation.Kind {
	case "dispatch_started":
		if status != "dispatching" {
			writeError(w, apiError(409, "state_conflict", "dispatch_started requires dispatching run", nil))
			return
		}
		nextRunStatus = "active"
	case "dispatch_bound":
		if observation.RuntimeSessionID == "" {
			writeError(w, apiError(400, "invalid_request", "dispatch_bound requires runtime session ID", nil))
			return
		}
		_, err = tx.ExecContext(r.Context(), `UPDATE runs SET runtime_session_id=?,runtime_container_id=? WHERE id=?`, observation.RuntimeSessionID, observation.RuntimeContainerID, runID)
		if err != nil {
			writeError(w, err)
			return
		}
	case "progress":
	case "permission_requested", "worker_question":
	case "result", "failure":
	case "no_effect_terminal":
		if observation.NoEffectProof.Outcome == "stopped" {
			nextRunStatus = "stopped"
			nextTaskStatus = "canceled"
		} else {
			nextRunStatus = "failed"
			nextTaskStatus = "failed"
		}
	case "create_rejected":
		nextRunStatus = "failed"
		nextTaskStatus = "failed"
	case "effect_unknown":
		nextRunStatus = "uncertain"
	case "stop_confirmed":
		if desired != "stop" {
			writeError(w, apiError(409, "state_conflict", "stop confirmation requires requested stop", nil))
			return
		}
		// Runner emits this only after independent termination and cleanup confirmation.
		nextRunStatus = "stopped"
		nextTaskStatus = "canceled"
	case "runtime_terminated":
		nextRunStatus = "verification_pending"
		var resultCount int
		if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM evidence WHERE run_id=? AND kind='agent_result'`, runID).Scan(&resultCount); err != nil {
			writeError(w, err)
			return
		}
		if desired == "stop" {
			nextRunStatus = "stopped"
			nextTaskStatus = "canceled"
		} else if resultCount > 0 {
			nextTaskStatus = "awaiting_user"
		} else {
			nextRunStatus = "failed"
			nextTaskStatus = "failed"
		}
	}
	if nextRunStatus != status {
		stoppedAt := any(nil)
		if nextRunStatus == "stopped" || nextRunStatus == "verification_pending" || nextRunStatus == "failed" {
			stoppedAt = now.Format(time.RFC3339Nano)
		}
		_, err = tx.ExecContext(r.Context(), `UPDATE runs SET status=?,last_observation_at=?,stopped_at=coalesce(?,stopped_at) WHERE id=? AND fence=?`, nextRunStatus, now.Format(time.RFC3339Nano), stoppedAt, runID, fence)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	if nextTaskStatus != "" {
		if !validTaskTransition(statusForTask(status, nextTaskStatus), nextTaskStatus) { /* runner terminal transitions are validated by explicit observation kind */
		}
		taskVersion++
		finished := any(nil)
		if nextTaskStatus == "canceled" || nextTaskStatus == "failed" {
			finished = now.Format(time.RFC3339Nano)
		}
		_, err = tx.ExecContext(r.Context(), `UPDATE tasks SET status=?,version=?,updated_at=?,finished_at=coalesce(?,finished_at) WHERE id=? AND workspace_id=?`, nextTaskStatus, taskVersion, now.Format(time.RFC3339Nano), finished, taskID, workspace)
		if err != nil {
			writeError(w, err)
			return
		}
		if err = settleDependentChildren(r.Context(), tx, taskID, nextTaskStatus, now); err != nil {
			writeError(w, err)
			return
		}
		if err = cancelPendingChildProposals(r.Context(), tx, taskID, now); err != nil {
			writeError(w, err)
			return
		}
		_, err = tx.ExecContext(r.Context(), `UPDATE approvals SET status='canceled',version=version+1 WHERE task_id=? AND workspace_id=? AND status='pending'`, taskID, workspace)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	if observation.Kind == "runtime_terminated" && nextTaskStatus == "awaiting_user" {
		// Ordinary assistant results require conversational follow-up, not result
		// acceptance. Exceptional approvals use the explicit approval_required
		// interaction state and are created by their owning policy handler.
		if _, err = tx.ExecContext(r.Context(), `UPDATE tasks SET interaction_state='awaiting_message',interaction_scope='',interaction_run_id='',interaction_action_digest='',interaction_expires_at=NULL,interaction_expected_version=version WHERE id=? AND workspace_id=?`, taskID, workspace); err != nil {
			writeError(w, err)
			return
		}
	}
	if err = appendEventTx(r.Context(), tx, contracts.Event{Type: eventKind, OccurredAt: now, WorkspaceID: workspace, SubjectID: "", AggregateType: "run", AggregateID: runID, AggregateVersion: int64(fence), Payload: raw(map[string]any{"run_id": runID, "task_id": taskID, "attempt_id": attempt, "kind": observation.Kind, "task_status": nextTaskStatus, "result": taskResult})}); err != nil {
		writeError(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "recorded"})
}

func proofTimeZone(proof *contracts.TerminationProof) (string, int) {
	if proof == nil || proof.VerifiedAt.IsZero() {
		return "", 1
	}
	return proof.VerifiedAt.Zone()
}

func statusForTask(runStatus, taskStatus string) string {
	_ = runStatus
	switch taskStatus {
	case "canceled", "failed", "awaiting_user":
		return "active"
	}
	return "active"
}
func validObservationKind(k string) bool {
	switch k {
	case "dispatch_started", "dispatch_bound", "progress", "result", "failure", "effect_unknown", "stop_confirmed", "runtime_terminated", "no_effect_terminal", "create_rejected":
		return true
	case "permission_requested", "worker_question":
		return true
	}
	return false
}
func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func appendEventTx(ctx context.Context, tx *sql.Tx, e contracts.Event) error {
	if e.EventID == "" {
		id, err := randomID()
		if err != nil {
			return err
		}
		e.EventID = id
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	e.SchemaVersion = 1
	var cursor int64
	if err := tx.QueryRowContext(ctx, `UPDATE event_counters SET current_cursor=current_cursor+1 WHERE workspace_id=? RETURNING current_cursor`, e.WorkspaceID).Scan(&cursor); err != nil {
		return err
	}
	e.Cursor = strconv.FormatInt(cursor, 10)
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events(event_id,workspace_id,cursor,schema_version,event_type,occurred_at,subject_id,aggregate_type,aggregate_id,aggregate_version,payload) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, e.EventID, e.WorkspaceID, cursor, e.SchemaVersion, e.Type, e.OccurredAt.Format(time.RFC3339Nano), e.SubjectID, e.AggregateType, e.AggregateID, e.AggregateVersion, payload)
	if err != nil {
		return err
	}
	return enqueueEventNotification(ctx, tx, e)
}
func sameHash(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
func ensureAcceptanceApprovalPlaceholder(ctx context.Context, tx *sql.Tx, workspace, task string) error {
	return nil
}

var _ = fmt.Sprintf

func validReconciliationContainerEvidence(e *contracts.NoEffectReconciliation, runStartedAt time.Time) bool {
	if e.DockerInventoryChecks != 2 || e.DockerInventoryContainerCount != len(e.RetainedNeverStartedContainers)+len(e.PreexistingCapabilityContainers)+len(e.UnrelatedCapabilityContainerIDs)+len(e.UnmanagedContainerIDs) || len(e.RetainNeverStartedContainerIDs) != len(e.RetainedNeverStartedContainers) || len(e.UnrelatedPreexistingCapabilityContainerIDs) != len(e.PreexistingCapabilityContainers) {
		return false
	}
	all := map[string]bool{}
	retained := map[string]bool{}
	preexisting := map[string]bool{}
	for _, id := range e.RetainNeverStartedContainerIDs {
		if !contracts.ValidFullContainerID(id) || all[id] {
			return false
		}
		all[id] = true
		retained[id] = true
	}
	for _, id := range e.UnrelatedPreexistingCapabilityContainerIDs {
		if !contracts.ValidFullContainerID(id) || all[id] {
			return false
		}
		all[id] = true
		preexisting[id] = true
	}
	for _, id := range append(append([]string{}, e.UnrelatedCapabilityContainerIDs...), e.UnmanagedContainerIDs...) {
		if !contracts.ValidFullContainerID(id) || all[id] {
			return false
		}
		all[id] = true
	}
	for _, p := range e.RetainedNeverStartedContainers {
		if !retained[p.ID] || !p.Valid(e.JournalUpdatedAt, e.VerifiedAt) {
			return false
		}
		delete(retained, p.ID)
	}
	for _, p := range e.PreexistingCapabilityContainers {
		if !preexisting[p.ID] || !p.Valid(runStartedAt, e.JournalUpdatedAt, e.VerifiedAt) {
			return false
		}
		delete(preexisting, p.ID)
	}
	return len(retained) == 0 && len(preexisting) == 0
}

func reconciliationEffectStatus(e *contracts.NoEffectReconciliation) string {
	if len(e.RetainedNeverStartedContainers) > 0 {
		return "runtime_not_started"
	}
	return "no_effect"
}
