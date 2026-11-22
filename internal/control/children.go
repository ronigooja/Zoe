package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

func (s *Server) handleChildProposal(w http.ResponseWriter, r *http.Request) {
	runner, ok := RunnerPrincipalFromContext(r.Context())
	if !ok {
		writeError(w, apiError(http.StatusUnauthorized, "unauthenticated", "runner authentication required", nil))
		return
	}
	if !runnerHasAnyWorkspaceScope(runner, "runner:claim") || !runnerHasAnyWorkspaceScope(runner, "runner:propose") {
		writeError(w, apiError(http.StatusForbidden, "forbidden", "runner lacks child proposal scope", nil))
		return
	}
	var req contracts.ChildTaskProposalRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	req.ProposalID = strings.TrimSpace(req.ProposalID)
	req.Title = strings.TrimSpace(req.Title)
	req.Goal = strings.TrimSpace(req.Goal)
	req.ProfileID = strings.TrimSpace(req.ProfileID)
	if req.ProposalID == "" || len(req.ProposalID) > 128 || req.Attempt == "" || len(req.Attempt) > 128 || req.Fence == 0 || req.Kind != "child_task" || req.Title == "" || req.Goal == "" || len(req.Title) > 300 || len(req.Goal) > s.cfg.Limits.Int("task_text_max_bytes") || req.ProfileID == "" || len(req.ProfileID) > 128 {
		writeError(w, apiError(http.StatusBadRequest, "invalid_request", "proposal_id, attempt, fence, child_task kind, title, goal, and profile_id are required", nil))
		return
	}
	if req.Budget.MaxChildRuns < 1 || req.Budget.MaxChildRuns > 64 || req.Budget.MaxDepth < 1 || req.Budget.MaxDepth > 8 {
		writeError(w, apiError(http.StatusBadRequest, "invalid_request", "child budget is outside supported bounds", nil))
		return
	}
	req.DependencyTaskIDs = uniqueNonEmpty(req.DependencyTaskIDs)
	req.RequestedCapabilityIDs = uniqueNonEmpty(req.RequestedCapabilityIDs)
	if len(req.DependencyTaskIDs) > 64 || len(req.RequestedCapabilityIDs) > 128 {
		writeError(w, apiError(http.StatusBadRequest, "invalid_request", "proposal dependency or capability list exceeds supported bounds", nil))
		return
	}
	canonical, err := json.Marshal(req)
	if err != nil {
		writeError(w, err)
		return
	}
	hash := sha256.Sum256(canonical)
	tx, err := s.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback()
	// Authenticate the durable run identity before looking up an idempotent
	// receipt. Otherwise a runner with proposal scope could replay another
	// runner's (run, attempt, proposal) tuple and learn its private task ID.
	var workspace, taskID, attemptID, status, desired, parentStatus, parentWorkspace, rootID string
	var subjectID, deviceID, projectID sql.NullString
	var fence uint64
	var depth, rootLimit int
	var policyJSON []byte
	err = tx.QueryRowContext(r.Context(), `SELECT r.workspace_id,r.task_id,r.attempt_id,r.fence,r.status,r.desired_control,t.status,t.workspace_id,t.owner_subject_id,t.created_by_device_id,t.project_id,coalesce(t.lineage_root_task_id,t.id),t.lineage_depth,t.root_child_limit,r.child_policy_json FROM runs r JOIN tasks t ON t.id=r.task_id WHERE r.id=? AND r.runner_id=?`, r.PathValue("run_id"), runner.RunnerID).Scan(&workspace, &taskID, &attemptID, &fence, &status, &desired, &parentStatus, &parentWorkspace, &subjectID, &deviceID, &projectID, &rootID, &depth, &rootLimit, &policyJSON)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (attemptID != req.Attempt || fence != req.Fence || parentWorkspace != workspace || !HasRunnerScope(runner, "runner:claim", workspace) || !HasRunnerScope(runner, "runner:propose", workspace)) {
		writeError(w, apiError(http.StatusNotFound, "not_found", "active run not found", nil))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	var oldHash, oldResponse []byte
	err = tx.QueryRowContext(r.Context(), `SELECT request_hash,response_json FROM child_proposals WHERE run_id=? AND attempt_id=? AND proposal_id=?`, r.PathValue("run_id"), req.Attempt, req.ProposalID).Scan(&oldHash, &oldResponse)
	if err == nil {
		if !sameHash(oldHash, hash[:]) {
			writeError(w, apiError(http.StatusConflict, "idempotency_conflict", "proposal identity was reused with different content", nil))
			return
		}
		var receipt contracts.ChildTaskProposalReceipt
		if err = json.Unmarshal(oldResponse, &receipt); err != nil {
			writeError(w, fmt.Errorf("decode durable child proposal receipt: %w", err))
			return
		}
		writeJSON(w, http.StatusOK, receipt)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, err)
		return
	}
	if status != "dispatching" && status != "active" || desired != "run" {
		writeError(w, apiError(http.StatusConflict, "state_conflict", "run is not accepting child proposals", map[string]string{"status": status}))
		return
	}
	if parentStatus != "active" {
		writeError(w, apiError(http.StatusConflict, "state_conflict", "parent task is not active and cannot create a child", map[string]string{"status": parentStatus}))
		return
	}
	if len(policyJSON) == 0 {
		writeError(w, apiError(http.StatusConflict, "child_policy_unavailable", "task has no trusted child policy snapshot", nil))
		return
	}
	var collision int
	if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM child_proposals WHERE workspace_id=? AND subject_id=? AND proposal_id=?`, workspace, subjectID.String, req.ProposalID).Scan(&collision); err != nil {
		writeError(w, err)
		return
	}
	if collision > 0 {
		writeError(w, apiError(http.StatusConflict, "proposal_id_conflict", "proposal_id is already in use by this subject", nil))
		return
	}
	var policy ChildPolicy
	if err = json.Unmarshal(policyJSON, &policy); err != nil || policy.MaxChildRuns < 1 || policy.MaxDepth < 1 {
		writeError(w, apiError(http.StatusConflict, "child_policy_unavailable", "task child policy snapshot is invalid", nil))
		return
	}
	if depth+1 > minInt(policy.MaxDepth, req.Budget.MaxDepth) {
		writeError(w, apiError(http.StatusConflict, "depth_exhausted", "child request exceeds the inherited lineage depth limit", nil))
		return
	}
	var reserved int
	if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM child_budget_reservations WHERE scope_task_id=?`, rootID).Scan(&reserved); err != nil {
		writeError(w, err)
		return
	}
	if reserved >= rootLimit {
		writeError(w, apiError(http.StatusConflict, "child_budget_exhausted", "root lineage child-run budget is exhausted", nil))
		return
	}
	profile, profileExists := s.cfg.Profiles[req.ProfileID]
	if !profileExists || profile.IsolationPolicy == "" || profile.Harness != "opencode" || (profile.IsolationPolicy != normalizeIsolationPolicy(profile.IsolationPolicy)) {
		writeError(w, apiError(http.StatusConflict, "profile_unavailable", "requested child profile is not configured for worker execution", nil))
		return
	}
	parentGrant := contracts.Principal{WorkspaceID: workspace, SubjectID: subjectID.String, DeviceID: deviceID.String}
	for _, dependencyID := range req.DependencyTaskIDs {
		if dependencyID == taskID || dependencyID == rootID {
			writeError(w, apiError(http.StatusBadRequest, "invalid_dependency", "child dependency cannot reference itself or its lineage root", nil))
			return
		}
		var depWorkspace, depOwner string
		depErr := tx.QueryRowContext(r.Context(), `SELECT workspace_id,owner_subject_id FROM tasks WHERE id=?`, dependencyID).Scan(&depWorkspace, &depOwner)
		if errors.Is(depErr, sql.ErrNoRows) || depErr == nil && (depWorkspace != workspace || depOwner != subjectID.String) {
			writeError(w, apiError(http.StatusBadRequest, "invalid_dependency", "dependency is inaccessible or cross-subject", nil))
			return
		}
		if depErr != nil {
			writeError(w, depErr)
			return
		}
		allowed, accessErr := PrincipalCanAccessTask(r.Context(), tx, parentGrant, dependencyID)
		if accessErr != nil {
			writeError(w, accessErr)
			return
		}
		if !allowed {
			writeError(w, apiError(http.StatusBadRequest, "invalid_dependency", "dependency is inaccessible", nil))
			return
		}
		isAncestor, ancestorErr := taskIsAncestor(r.Context(), tx, taskID, dependencyID)
		if ancestorErr != nil {
			writeError(w, ancestorErr)
			return
		}
		if isAncestor {
			writeError(w, apiError(http.StatusBadRequest, "invalid_dependency", "child dependency cannot reference its parent or an ancestor", nil))
			return
		}
	}
	outOfPolicy := !containsString(policy.AllowedProfileIDs, req.ProfileID) || !subset(req.RequestedCapabilityIDs, policy.CapabilityIDs) || !subset(req.RequestedCapabilityIDs, profile.CapabilityIDs) || req.Budget.MaxChildRuns > policy.MaxChildRuns || req.Budget.MaxDepth > policy.MaxDepth
	statusValue := "admitted"
	receipt := contracts.ChildTaskProposalReceipt{ProposalID: req.ProposalID, Status: statusValue, Version: 1}
	if outOfPolicy {
		receipt.Status = "pending"
	}
	if outOfPolicy {
		var pending int
		if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM child_proposals WHERE parent_task_id=? AND status='pending'`, taskID).Scan(&pending); err != nil {
			writeError(w, err)
			return
		}
		if pending >= 64 {
			writeError(w, apiError(http.StatusConflict, "proposal_queue_full", "parent has reached its pending child proposal limit", nil))
			return
		}
	}
	response, _ := json.Marshal(receipt)
	requestJSON, _ := json.Marshal(req)
	proposalRowID, err := randomID()
	if err != nil {
		writeError(w, err)
		return
	}
	now := time.Now().UTC()
	_, err = tx.ExecContext(r.Context(), `INSERT INTO child_proposals(id,run_id,attempt_id,proposal_id,workspace_id,subject_id,parent_task_id,root_task_id,status,version,request_hash,request_json,response_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,1,?,?,?,?)`, proposalRowID, r.PathValue("run_id"), req.Attempt, req.ProposalID, workspace, subjectID.String, taskID, rootID, receipt.Status, hash[:], requestJSON, response, now.Format(time.RFC3339Nano))
	if err != nil {
		writeError(w, err)
		return
	}
	var childTaskID string
	if !outOfPolicy {
		childTaskID, err = s.admitChildTaskTx(r.Context(), tx, req, proposalRowID, workspace, subjectID.String, deviceID.String, projectID, taskID, rootID, depth+1, rootLimit, policy)
		if err != nil {
			writeError(w, err)
			return
		}
		receipt.TaskID = childTaskID
		response, _ = json.Marshal(receipt)
		if _, err = tx.ExecContext(r.Context(), `UPDATE child_proposals SET child_task_id=?,response_json=? WHERE id=?`, childTaskID, response, proposalRowID); err != nil {
			writeError(w, err)
			return
		}
	}
	if err = appendEventTx(r.Context(), tx, contracts.Event{Type: "child.proposal." + receipt.Status, OccurredAt: now, WorkspaceID: workspace, SubjectID: subjectID.String, AggregateType: "child_proposal", AggregateID: proposalRowID, AggregateVersion: 1, Payload: raw(map[string]any{"proposal_id": req.ProposalID, "parent_task_id": taskID, "task_id": childTaskID, "status": receipt.Status})}); err != nil {
		writeError(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (s *Server) admitChildTaskTx(ctx context.Context, tx *sql.Tx, req contracts.ChildTaskProposalRequest, proposalRowID, workspace, subject, device string, project sql.NullString, parentTaskID, rootID string, depth, rootLimit int, inherited ChildPolicy) (string, error) {
	principal := contracts.Principal{WorkspaceID: workspace, SubjectID: subject, DeviceID: device}
	for _, dependencyID := range req.DependencyTaskIDs {
		var depWorkspace, depOwner string
		if err := tx.QueryRowContext(ctx, `SELECT workspace_id,owner_subject_id FROM tasks WHERE id=?`, dependencyID).Scan(&depWorkspace, &depOwner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", apiError(400, "invalid_dependency", "dependency is no longer available", nil)
			}
			return "", err
		}
		if depWorkspace != workspace || depOwner != subject {
			return "", apiError(400, "invalid_dependency", "dependency is cross-workspace or cross-subject", nil)
		}
		allowed, err := PrincipalCanAccessTask(ctx, tx, principal, dependencyID)
		if err != nil {
			return "", err
		}
		if !allowed {
			return "", apiError(400, "invalid_dependency", "dependency is no longer accessible", nil)
		}
		ancestor, err := taskIsAncestor(ctx, tx, parentTaskID, dependencyID)
		if err != nil {
			return "", err
		}
		if ancestor {
			return "", apiError(400, "invalid_dependency", "child dependency cannot reference its parent or an ancestor", nil)
		}
	}
	profile := s.cfg.Profiles[req.ProfileID]
	profilePolicy := profile.ChildPolicy
	childPolicy := ChildPolicy{
		AllowedProfileIDs: intersectStrings(inherited.AllowedProfileIDs, profilePolicy.AllowedProfileIDs),
		CapabilityIDs:     intersectStrings(inherited.CapabilityIDs, profilePolicy.CapabilityIDs),
		MaxChildRuns:      minInt(inherited.MaxChildRuns, profilePolicy.MaxChildRuns),
		MaxDepth:          minInt(inherited.MaxDepth, profilePolicy.MaxDepth),
	}
	if req.Budget.MaxChildRuns < childPolicy.MaxChildRuns {
		childPolicy.MaxChildRuns = req.Budget.MaxChildRuns
	}
	if req.Budget.MaxDepth < childPolicy.MaxDepth {
		childPolicy.MaxDepth = req.Budget.MaxDepth
	}
	policyJSON, err := json.Marshal(childPolicy)
	if err != nil {
		return "", err
	}
	pregrantJSON, err := json.Marshal(req.RequestedCapabilityIDs)
	if err != nil {
		return "", err
	}
	childID, err := randomID()
	if err != nil {
		return "", err
	}
	createdAt := time.Now().UTC()
	now := createdAt.Format(time.RFC3339Nano)
	depState := "satisfied"
	for _, dependencyID := range req.DependencyTaskIDs {
		var depStatus string
		if err = tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id=? AND workspace_id=?`, dependencyID, workspace).Scan(&depStatus); err != nil {
			return "", err
		}
		if depStatus != "complete" {
			depState = "waiting"
			if depStatus == "failed" || depStatus == "canceled" {
				depState = "failed"
			}
		}
	}
	taskStatus := "queued"
	if len(req.DependencyTaskIDs) > 0 && depState != "satisfied" {
		taskStatus = "waiting"
	}
	command := "child:" + proposalRowID
	_, err = tx.ExecContext(ctx, `INSERT INTO tasks(id,workspace_id,project_id,title,goal,source,profile_id,harness,status,version,created_at,updated_at,accepted_at,created_by_device_id,command_id,owner_subject_id,lineage_root_task_id,lineage_parent_task_id,lineage_depth,child_policy_json,root_child_limit,pregrant_capabilities_json) VALUES(?,?,?,?,?,'child',?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?)`, childID, workspace, nullableValue(project), req.Title, req.Goal, req.ProfileID, profile.Harness, taskStatus, now, now, now, device, command, subject, rootID, parentTaskID, depth, policyJSON, rootLimit, pregrantJSON)
	if err != nil {
		return "", err
	}
	for _, dependencyID := range req.DependencyTaskIDs {
		var depStatus string
		if err = tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id=? AND workspace_id=?`, dependencyID, workspace).Scan(&depStatus); err != nil {
			return "", err
		}
		state := "waiting"
		if depStatus == "complete" {
			state = "satisfied"
		} else if depStatus == "failed" || depStatus == "canceled" {
			state = "failed"
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_dependencies(task_id,dependency_task_id,workspace_id,status,created_at) VALUES(?,?,?,?,?)`, childID, dependencyID, workspace, state, now); err != nil {
			return "", err
		}
	}
	for scopeID := parentTaskID; scopeID != ""; {
		var nextParent string
		var scopePolicyJSON []byte
		if err = tx.QueryRowContext(ctx, `SELECT coalesce(lineage_parent_task_id,''),child_policy_json FROM tasks WHERE id=?`, scopeID).Scan(&nextParent, &scopePolicyJSON); err != nil {
			return "", err
		}
		var scopePolicy ChildPolicy
		if json.Unmarshal(scopePolicyJSON, &scopePolicy) != nil || scopePolicy.MaxChildRuns < 1 {
			return "", apiError(409, "child_policy_unavailable", "ancestor has no trusted child budget", nil)
		}
		var used int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM child_budget_reservations WHERE scope_task_id=?`, scopeID).Scan(&used); err != nil {
			return "", err
		}
		if used >= scopePolicy.MaxChildRuns {
			return "", apiError(409, "child_budget_exhausted", "an ancestor child-run budget is exhausted", nil)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO child_budget_reservations(scope_task_id,proposal_id,reserved_at) VALUES(?,?,?)`, scopeID, proposalRowID, now); err != nil {
			return "", err
		}
		scopeID = nextParent
	}
	if _, err = tx.ExecContext(ctx, `UPDATE child_proposals SET child_task_id=? WHERE id=?`, childID, proposalRowID); err != nil {
		return "", err
	}
	projectID := ""
	if project.Valid {
		projectID = project.String
	}
	childTask := contracts.Task{ID: childID, ProjectID: projectID, Title: req.Title, Goal: req.Goal, Source: "child", ProfileID: req.ProfileID, Status: taskStatus, Version: 1, CreatedAt: createdAt, UpdatedAt: createdAt, AcceptedAt: &createdAt, Evidence: []contracts.Evidence{}}
	if err = appendEventTx(ctx, tx, contracts.Event{Type: "task.created", OccurredAt: createdAt, WorkspaceID: workspace, SubjectID: subject, AggregateType: "task", AggregateID: childID, AggregateVersion: 1, Payload: raw(map[string]any{"task": childTask, "parent_task_id": parentTaskID, "lineage_root_task_id": rootID, "lineage_depth": depth})}); err != nil {
		return "", err
	}
	return childID, nil
}

type childDecisionPayload struct {
	ProposalID string `json:"proposal_id"`
}

func (s *Server) commandChildDecision(ctx context.Context, tx *sql.Tx, c CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if req.ExpectedVersion == nil {
		return contracts.CommandResult{}, nil, apiError(http.StatusBadRequest, "invalid_request", "expected_version is required", nil)
	}
	var p childDecisionPayload
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if p.ProposalID == "" {
		return contracts.CommandResult{}, nil, apiError(400, "invalid_request", "proposal_id is required", nil)
	}
	decision := "approved"
	if req.Kind == "child.reject" {
		decision = "rejected"
	}
	var rowID, status, workspace, parentID, rootID, subject, device string
	var version, depth, rootLimit int64
	var requestJSON, policyJSON []byte
	err := tx.QueryRowContext(ctx, `SELECT id,status,workspace_id,parent_task_id,root_task_id,subject_id,version,request_json FROM child_proposals WHERE proposal_id=? AND workspace_id=? AND subject_id=? ORDER BY created_at DESC LIMIT 1`, p.ProposalID, c.Principal.WorkspaceID, c.Principal.SubjectID).Scan(&rowID, &status, &workspace, &parentID, &rootID, &subject, &version, &requestJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(404, "not_found", "child proposal not found", nil)
	}
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if err = checkVersion(req.ExpectedVersion, version); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if status != "pending" {
		return contracts.CommandResult{}, nil, apiError(409, "state_conflict", "child proposal is no longer pending", map[string]string{"status": status})
	}
	allowed, err := PrincipalCanWriteTask(ctx, tx, c.Principal, parentID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if !allowed {
		return contracts.CommandResult{}, nil, apiError(404, "not_found", "child proposal not found", nil)
	}
	var parentStatus string
	err = tx.QueryRowContext(ctx, `SELECT status,created_by_device_id,coalesce(child_policy_json,''),lineage_depth,root_child_limit FROM tasks WHERE id=? AND workspace_id=? AND owner_subject_id=?`, parentID, workspace, subject).Scan(&parentStatus, &device, &policyJSON, &depth, &rootLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(404, "not_found", "parent task not found", nil)
	}
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if decision == "approved" && (parentStatus == "complete" || parentStatus == "failed" || parentStatus == "canceled") {
		return contracts.CommandResult{}, nil, apiError(409, "state_conflict", "proposal cannot revive a terminal parent task", map[string]string{"parent_status": parentStatus})
	}
	var childID string
	if decision == "approved" {
		var reqBody contracts.ChildTaskProposalRequest
		if err = json.Unmarshal(requestJSON, &reqBody); err != nil {
			return contracts.CommandResult{}, nil, fmt.Errorf("decode child proposal request: %w", err)
		}
		profile, exists := s.cfg.Profiles[reqBody.ProfileID]
		if !exists || profile.IsolationPolicy == "" || !subset(reqBody.RequestedCapabilityIDs, profile.CapabilityIDs) {
			return contracts.CommandResult{}, nil, apiError(409, "profile_unavailable", "child profile or requested capabilities are no longer authorized", nil)
		}
		var policy ChildPolicy
		if err = json.Unmarshal(policyJSON, &policy); err != nil || policy.MaxChildRuns < 1 {
			return contracts.CommandResult{}, nil, apiError(409, "child_policy_unavailable", "parent has no trusted child policy snapshot", nil)
		}
		var reserved int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM child_budget_reservations WHERE scope_task_id=?`, rootID).Scan(&reserved); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if int64(reserved) >= rootLimit {
			return contracts.CommandResult{}, nil, apiError(409, "child_budget_exhausted", "root lineage child-run budget is exhausted", nil)
		}
		var parentWorkspace string
		var project sql.NullString
		if err = tx.QueryRowContext(ctx, `SELECT workspace_id,project_id FROM tasks WHERE id=?`, parentID).Scan(&parentWorkspace, &project); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		childID, err = s.admitChildTaskTx(ctx, tx, reqBody, rowID, parentWorkspace, subject, device, project, parentID, rootID, int(depth)+1, int(rootLimit), policy)
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
	}
	newVersion := version + 1
	at := c.AcceptedAt.Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `UPDATE child_proposals SET status=?,version=?,child_task_id=coalesce(?,child_task_id),decided_at=?,decided_by_device_id=? WHERE id=? AND status='pending'`, decision, newVersion, nullableString(childID), at, c.Principal.DeviceID, rowID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if err = appendEventTx(ctx, tx, contracts.Event{Type: "child.proposal." + decision, OccurredAt: c.AcceptedAt, WorkspaceID: workspace, SubjectID: subject, AggregateType: "child_proposal", AggregateID: rowID, AggregateVersion: newVersion, Payload: raw(map[string]any{"proposal_id": p.ProposalID, "parent_task_id": parentID, "task_id": childID, "status": decision})}); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	return contracts.CommandResult{ResourceType: "child_proposal", ResourceID: p.ProposalID, ResourceVersion: newVersion}, []contracts.Event{}, nil
}

func subset(wanted, allowed []string) bool {
	for _, value := range wanted {
		if !containsString(allowed, value) {
			return false
		}
	}
	return true
}

func taskIsAncestor(ctx context.Context, tx *sql.Tx, taskID, possibleAncestor string) (bool, error) {
	current := taskID
	for current != "" {
		if current == possibleAncestor {
			return true, nil
		}
		var parent string
		err := tx.QueryRowContext(ctx, `SELECT coalesce(lineage_parent_task_id,'') FROM tasks WHERE id=?`, current).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		current = parent
	}
	return false, nil
}

func settleDependentChildren(ctx context.Context, tx *sql.Tx, dependencyTaskID, status string, at time.Time) error {
	dependencyState := "waiting"
	if status == "complete" {
		dependencyState = "satisfied"
	} else if status == "failed" || status == "canceled" {
		dependencyState = "failed"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_dependencies SET status=? WHERE dependency_task_id=?`, dependencyState, dependencyTaskID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT t.id,t.workspace_id,t.version,t.owner_subject_id FROM tasks t JOIN task_dependencies d ON d.task_id=t.id WHERE d.dependency_task_id=? AND t.status='waiting'`, dependencyTaskID)
	if err != nil {
		return err
	}
	type candidate struct {
		id, workspace, owner string
		version              int64
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.workspace, &c.version, &c.owner); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, c := range candidates {
		var unsatisfied int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM task_dependencies WHERE task_id=? AND status<>'satisfied'`, c.id).Scan(&unsatisfied); err != nil {
			return err
		}
		if unsatisfied != 0 {
			continue
		}
		version := c.version + 1
		if _, err = tx.ExecContext(ctx, `UPDATE tasks SET status='queued',version=?,updated_at=? WHERE id=? AND status='waiting'`, version, at.UTC().Format(time.RFC3339Nano), c.id); err != nil {
			return err
		}
		if err = appendEventTx(ctx, tx, contracts.Event{Type: "task.dependencies_satisfied", OccurredAt: at, WorkspaceID: c.workspace, SubjectID: c.owner, AggregateType: "task", AggregateID: c.id, AggregateVersion: version, Payload: raw(map[string]any{"task_id": c.id, "status": "queued"})}); err != nil {
			return err
		}
	}
	return nil
}

func cancelPendingChildProposals(ctx context.Context, tx *sql.Tx, taskID string, at time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,workspace_id,subject_id,proposal_id,version FROM child_proposals WHERE parent_task_id=? AND status='pending'`, taskID)
	if err != nil {
		return err
	}
	type proposal struct {
		id, workspace, subject, publicID string
		version                          int64
	}
	var pending []proposal
	for rows.Next() {
		var item proposal
		if err = rows.Scan(&item.id, &item.workspace, &item.subject, &item.publicID, &item.version); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, item := range pending {
		version := item.version + 1
		if _, err = tx.ExecContext(ctx, `UPDATE child_proposals SET status='canceled',version=?,decided_at=? WHERE id=? AND status='pending'`, version, at.UTC().Format(time.RFC3339Nano), item.id); err != nil {
			return err
		}
		if err = appendEventTx(ctx, tx, contracts.Event{Type: "child.proposal.canceled", OccurredAt: at, WorkspaceID: item.workspace, SubjectID: item.subject, AggregateType: "child_proposal", AggregateID: item.id, AggregateVersion: version, Payload: raw(map[string]any{"proposal_id": item.publicID, "parent_task_id": taskID, "status": "canceled"})}); err != nil {
			return err
		}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func nullableValue(value sql.NullString) any {
	if value.Valid {
		return value.String
	}
	return nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
