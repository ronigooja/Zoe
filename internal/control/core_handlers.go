package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

type projectCreatePayload struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}
type taskCreatePayload struct {
	ProjectID       string          `json:"project_id,omitempty"`
	Title           string          `json:"title"`
	Goal            string          `json:"goal"`
	Source          string          `json:"source"`
	ProfileID       string          `json:"profile_id,omitempty"`
	InitialMessage  string          `json:"initial_message,omitempty"`
	SourceReference json.RawMessage `json:"source_reference,omitempty"`
}
type taskIDPayload struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason,omitempty"`
}
type appendPayload struct {
	TaskID  string `json:"task_id"`
	Content string `json:"content"`
}

var explicitConfirmationPhrases = map[string]bool{
	"confirm": true, "confirm and continue": true, "yes, proceed": true,
	"确认": true, "确认并继续": true, "是，继续": true,
}

// IsExplicitConfirmation applies to exact server-owned confirmation phrases.
func IsExplicitConfirmation(text string) bool {
	return explicitConfirmationPhrases[strings.ToLower(strings.TrimSpace(text))]
}

// AppendTaskTx lets authenticated voice replies reuse task.append semantics.
func (s *Server) AppendTaskTx(ctx context.Context, tx *sql.Tx, c CommandContext, taskID, content string) (contracts.CommandResult, []contracts.Event, error) {
	payload, err := json.Marshal(appendPayload{TaskID: taskID, Content: content})
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	return s.commandTaskAppend(ctx, tx, c, contracts.CommandRequest{Payload: payload})
}

type approvePayload struct {
	TaskID     string `json:"task_id"`
	ApprovalID string `json:"approval_id"`
	Decision   string `json:"decision"`
	Comment    string `json:"comment,omitempty"`
}

func decodePayload(raw json.RawMessage, dst any) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return apiError(400, "invalid_request", "command payload is invalid", nil)
	}
	return nil
}
func event(kind, aggType, aggID string, version int64, payload any) contracts.Event {
	raw, _ := json.Marshal(payload)
	return contracts.Event{Type: kind, AggregateType: aggType, AggregateID: aggID, AggregateVersion: version, Payload: raw}
}
func (s *Server) commandProjectCreate(ctx context.Context, tx *sql.Tx, c CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	var p projectCreatePayload
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > 200 || len(p.Description) > 4000 {
		return contracts.CommandResult{}, nil, apiError(400, "invalid_request", "project name is required and fields exceed limits", nil)
	}
	id, err := randomID()
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	at := c.AcceptedAt.Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO projects(id,workspace_id,name,description,status,version,created_at,updated_at,owner_subject_id) VALUES(?,?,?,?,'active',1,?,?,?)`, id, c.Principal.WorkspaceID, p.Name, p.Description, at, at, c.Principal.SubjectID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	project := contracts.Project{ID: id, Name: p.Name, Description: p.Description, Status: "active", Version: 1, CreatedAt: c.AcceptedAt, UpdatedAt: c.AcceptedAt}
	return contracts.CommandResult{ResourceType: "project", ResourceID: id, ResourceVersion: 1}, []contracts.Event{event("project.created", "project", id, 1, map[string]any{"project": project})}, nil
}

func (s *Server) commandTaskCreate(ctx context.Context, tx *sql.Tx, c CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	var p taskCreatePayload
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	result, err := s.AdmitTaskTx(ctx, tx, AdmissionContext{Principal: c.Principal, Source: p.Source, SourceRef: p.SourceReference, IdempotencyKey: c.CommandID, ProfileID: p.ProfileID, ProjectID: p.ProjectID, Title: p.Title, Goal: p.Goal, InitialMessage: p.InitialMessage, AcceptedAt: c.AcceptedAt})
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	return contracts.CommandResult{ResourceType: "task", ResourceID: result.Task.ID, ResourceVersion: result.Task.Version}, result.Events, nil
}

// AdmitTaskTx creates a task using the same policy, project ACL, source-hook,
// lineage snapshot, and initial-entry rules as the user command path. The
// caller owns tx and must append result.Events before committing it.
func (s *Server) AdmitTaskTx(ctx context.Context, tx *sql.Tx, in AdmissionContext) (TaskAdmissionResult, error) {
	if tx == nil || in.IdempotencyKey == "" || in.Principal.WorkspaceID == "" || in.Principal.SubjectID == "" || in.Principal.DeviceID == "" {
		return TaskAdmissionResult{}, apiError(400, "invalid_admission", "transaction, stable idempotency key, and trusted principal are required", nil)
	}
	dispatchEnabled, err := DispatchEnabledTx(ctx, tx)
	if err != nil {
		return TaskAdmissionResult{}, apiError(503, "instance_fence_unavailable", "task admission is disabled until the instance fence is initialized", nil)
	}
	if !dispatchEnabled {
		return TaskAdmissionResult{}, apiError(503, "instance_dispatch_fenced", "task admission is disabled by the instance fence", nil)
	}
	in.Title = strings.TrimSpace(in.Title)
	in.Goal = strings.TrimSpace(in.Goal)
	if in.Title == "" || in.Goal == "" || len(in.Title) > 300 || len(in.Goal) > 20000 || len(in.InitialMessage) > 20000 {
		return TaskAdmissionResult{}, apiError(400, "invalid_request", "task title and goal are required and must fit limits", nil)
	}
	if len(in.GrantSnapshot) != 0 && !json.Valid(in.GrantSnapshot) {
		return TaskAdmissionResult{}, apiError(400, "invalid_admission", "grant snapshot must be valid JSON", nil)
	}
	switch in.Source {
	case "user":
		if len(in.SourceRef) != 0 {
			return TaskAdmissionResult{}, apiError(400, "invalid_request", "user task source cannot include a source reference", nil)
		}
	case "voice", "service", "system":
		if in.Source == "service" && !in.TrustedSource {
			return TaskAdmissionResult{}, apiError(403, "source_unavailable", "service task source is available only to trusted server features", nil)
		}
		if len(in.SourceRef) == 0 || !json.Valid(in.SourceRef) {
			return TaskAdmissionResult{}, apiError(400, "invalid_request", "non-user task source requires a server-validated source reference", nil)
		}
		s.mu.RLock()
		_, registered := s.taskSources[in.Source]
		s.mu.RUnlock()
		if !registered {
			return TaskAdmissionResult{}, apiError(403, "source_unavailable", "task source is not authorized by a registered server feature", map[string]string{"source": in.Source})
		}
	default:
		return TaskAdmissionResult{}, apiError(400, "invalid_request", "source is invalid", nil)
	}
	if in.ProjectID != "" {
		allowed, err := PrincipalCanWriteProject(ctx, tx, in.Principal, in.ProjectID)
		if err != nil {
			return TaskAdmissionResult{}, err
		}
		if !allowed {
			return TaskAdmissionResult{}, apiError(404, "not_found", "project not found", nil)
		}
	}
	profileID := in.ProfileID
	if profileID == "" {
		profileID = s.cfg.DefaultProfileID
	}
	profile, ok := s.cfg.Profiles[profileID]
	if !ok {
		return TaskAdmissionResult{}, apiError(409, "profile_unavailable", "requested execution profile is not configured and authorized", map[string]any{"profile_id": profileID})
	}
	id, err := randomID()
	if err != nil {
		return TaskAdmissionResult{}, err
	}
	at := time.Now().UTC()
	if !in.AcceptedAt.IsZero() {
		at = in.AcceptedAt.UTC()
	}
	policyJSON, err := json.Marshal(profile.ChildPolicy)
	if err != nil {
		return TaskAdmissionResult{}, err
	}
	childLimit := 0
	if len(profile.ChildPolicy.AllowedProfileIDs) > 0 {
		childLimit = profile.ChildPolicy.MaxChildRuns
	}
	stamp := at.Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO tasks(id,workspace_id,project_id,title,goal,source,profile_id,harness,status,version,created_at,updated_at,accepted_at,created_by_device_id,command_id,owner_subject_id,lineage_root_task_id,lineage_depth,child_policy_json,root_child_limit) VALUES(?,?,?,?,?,?,?,?, 'queued',1,?,?,?,?,?,?,?,0,?,?)`, id, in.Principal.WorkspaceID, nullString(in.ProjectID), in.Title, in.Goal, in.Source, profileID, profile.Harness, stamp, stamp, stamp, in.Principal.DeviceID, in.IdempotencyKey, in.Principal.SubjectID, id, policyJSON, childLimit)
	if err != nil {
		return TaskAdmissionResult{}, err
	}
	var sourceEvents []contracts.Event
	if in.Source != "user" {
		s.mu.RLock()
		handler := s.taskSources[in.Source]
		s.mu.RUnlock()
		if handler == nil {
			return TaskAdmissionResult{}, apiError(403, "source_unavailable", "task source is not authorized by a registered server feature", map[string]string{"source": in.Source})
		}
		sourceRequest := &TaskSourceRequest{Title: in.Title, Goal: in.Goal, InitialMessage: in.InitialMessage, TrustedSource: in.TrustedSource, GrantSnapshot: append(json.RawMessage(nil), in.GrantSnapshot...)}
		events, err := handler(ctx, tx, in.Principal, id, sourceRequest, in.SourceRef, at)
		if err != nil {
			return TaskAdmissionResult{}, err
		}
		if strings.TrimSpace(sourceRequest.Title) == "" || strings.TrimSpace(sourceRequest.Goal) == "" || len(sourceRequest.Title) > 300 || len(sourceRequest.Goal) > 20000 || len(sourceRequest.InitialMessage) > 20000 {
			return TaskAdmissionResult{}, apiError(400, "invalid_source", "validated task source returned invalid task content", nil)
		}
		in.Title, in.Goal, in.InitialMessage = sourceRequest.Title, sourceRequest.Goal, sourceRequest.InitialMessage
		if _, err = tx.ExecContext(ctx, `UPDATE tasks SET title=?,goal=? WHERE id=? AND workspace_id=?`, in.Title, in.Goal, id, in.Principal.WorkspaceID); err != nil {
			return TaskAdmissionResult{}, err
		}
		sourceEvents = events
	}
	task := contracts.Task{ID: id, ProjectID: in.ProjectID, Title: in.Title, Goal: in.Goal, Source: in.Source, ProfileID: profileID, Status: "queued", Version: 1, CreatedAt: at, UpdatedAt: at, AcceptedAt: &at, Evidence: []contracts.Evidence{}}
	events := []contracts.Event{event("task.created", "task", id, 1, map[string]any{"task": task})}
	if in.InitialMessage != "" {
		entryID, err := randomID()
		if err != nil {
			return TaskAdmissionResult{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES(?,?,?,'user_message',?,?,?)`, entryID, id, in.Principal.WorkspaceID, in.InitialMessage, in.Principal.DeviceID, stamp); err != nil {
			return TaskAdmissionResult{}, err
		}
		entry := contracts.TaskEntry{ID: entryID, Kind: "user_message", Content: in.InitialMessage, AuthorDeviceID: in.Principal.DeviceID, CreatedAt: at}
		events = append(events, event("task.entry_appended", "task", id, 1, map[string]any{"task_id": id, "entry": entry}))
	}
	events = append(events, sourceEvents...)
	return TaskAdmissionResult{Task: task, Events: events}, nil
}

func (s *Server) commandTaskAppend(ctx context.Context, tx *sql.Tx, c CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	var p appendPayload
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	p.Content = strings.TrimSpace(p.Content)
	if p.TaskID == "" || p.Content == "" || len(p.Content) > 20000 {
		return contracts.CommandResult{}, nil, apiError(400, "invalid_request", "task_id and content are required", nil)
	}
	version, status, err := taskVersionStatus(ctx, tx, c.Principal, p.TaskID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if err = checkVersion(req.ExpectedVersion, version); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if status == "complete" || status == "failed" || status == "canceled" {
		return contracts.CommandResult{}, nil, apiError(409, "state_conflict", "terminal task cannot accept entries", map[string]any{"status": status})
	}
	var interaction, expiredKind, scope, interactionRun, actionDigest, expires string
	var interactionVersion int64
	if err = tx.QueryRowContext(ctx, `SELECT interaction_state,interaction_expired_kind,interaction_scope,interaction_run_id,interaction_action_digest,coalesce(interaction_expires_at,''),interaction_expected_version FROM tasks WHERE id=? AND workspace_id=?`, p.TaskID, c.Principal.WorkspaceID).Scan(&interaction, &expiredKind, &scope, &interactionRun, &actionDigest, &expires, &interactionVersion); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if (status == "waiting" || status == "awaiting_user") && interaction == "none" {
		interaction = "awaiting_message"
	}
	liveInteractionRun := false
	if interactionRun != "" {
		var runStatus string
		err = tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE id=? AND task_id=? AND workspace_id=?`, interactionRun, p.TaskID, c.Principal.WorkspaceID).Scan(&runStatus)
		if err == nil {
			liveInteractionRun = runStatus == "dispatching" || runStatus == "active" || runStatus == "uncertain"
		} else if !errors.Is(err, sql.ErrNoRows) {
			return contracts.CommandResult{}, nil, err
		}
	}
	confirmed := false
	if interaction == "explicit_confirmation" {
		if interactionVersion != version || scope == "" || interactionRun == "" || actionDigest == "" || expires == "" {
			return contracts.CommandResult{}, nil, apiError(409, "confirmation_invalid", "confirmation context is incomplete or stale", nil)
		}
		deadline, parseErr := time.Parse(time.RFC3339Nano, expires)
		if parseErr != nil || !c.AcceptedAt.Before(deadline) {
			return contracts.CommandResult{}, nil, apiError(409, "confirmation_expired", "confirmation has expired", nil)
		}
		confirmed = explicitConfirmationPhrases[strings.ToLower(p.Content)]
		if !confirmed {
			if _, err = tx.ExecContext(ctx, `UPDATE pending_runtime_actions SET decision='reject' WHERE task_id=? AND run_id=? AND action_digest=? AND authorization_used=0`, p.TaskID, interactionRun, actionDigest); err != nil {
				return contracts.CommandResult{}, nil, err
			}
		}
	}
	id, err := randomID()
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	at := c.AcceptedAt.Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES(?,?,?,'user_message',?,?,?)`, id, p.TaskID, c.Principal.WorkspaceID, p.Content, c.Principal.DeviceID, at)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	version++
	nextStatus := status
	expiredInteraction := expiredKind == "question" || expiredKind == "permission"
	if (status == "waiting" || status == "awaiting_user") && (interaction == "awaiting_message" || expiredInteraction) && !liveInteractionRun {
		nextStatus = "queued"
	}
	// An expired permission is not an authorization to retry. Keep it
	// answerable until the user supplies a typed answer, then queue the new run.
	if expiredKind == "permission" && strings.TrimSpace(p.Content) != "" {
		if !liveInteractionRun {
			nextStatus = "queued"
		}
	}
	if interaction == "awaiting_message" && interactionRun != "" && actionDigest != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE pending_runtime_actions SET decision='answer',message=? WHERE task_id=? AND run_id=? AND action_digest=? AND authorization_used=0`, p.Content, p.TaskID, interactionRun, actionDigest); err != nil {
			return contracts.CommandResult{}, nil, err
		}
	}
	if interaction == "explicit_confirmation" && !expiredInteraction && confirmed {
		nextStatus = "queued"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tasks SET status=?,interaction_state=?,version=?,updated_at=? WHERE id=? AND workspace_id=?`, nextStatus, interaction, version, at, p.TaskID, c.Principal.WorkspaceID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE approvals SET status='canceled',version=version+1 WHERE task_id=? AND workspace_id=? AND status='pending'`, p.TaskID, c.Principal.WorkspaceID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	entry := contracts.TaskEntry{ID: id, Kind: "user_message", Content: p.Content, AuthorDeviceID: c.Principal.DeviceID, CreatedAt: c.AcceptedAt}
	events := []contracts.Event{event("task.entry_appended", "task", p.TaskID, version, map[string]any{"task_id": p.TaskID, "entry": entry, "interaction_state": interaction, "status": nextStatus})}
	if interaction == "explicit_confirmation" && !expiredInteraction && confirmed {
		confirmationID, genErr := randomID()
		if genErr != nil {
			return contracts.CommandResult{}, nil, genErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_confirmations(id,workspace_id,task_id,run_id,action_digest,task_version,scope,confirmed_text,device_id,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, confirmationID, c.Principal.WorkspaceID, p.TaskID, interactionRun, actionDigest, version, scope, p.Content, c.Principal.DeviceID, at, expires); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tasks SET interaction_state='none',interaction_expired_kind='',interaction_scope='',interaction_run_id='',interaction_action_digest='',interaction_expires_at=NULL,interaction_expected_version=0 WHERE id=? AND workspace_id=?`, p.TaskID, c.Principal.WorkspaceID); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE pending_runtime_actions SET decision='once' WHERE task_id=? AND run_id=? AND action_digest=? AND authorization_used=0`, p.TaskID, interactionRun, actionDigest); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		events = append(events, event("task.confirmation_recorded", "task", p.TaskID, version, map[string]any{"task_id": p.TaskID, "run_id": interactionRun, "action_digest": actionDigest, "scope": scope, "confirmed_text": p.Content, "device_id": c.Principal.DeviceID, "expires_at": expires}))
	}
	return contracts.CommandResult{ResourceType: "task", ResourceID: p.TaskID, ResourceVersion: version}, events, nil
}

func (s *Server) commandTaskStop(ctx context.Context, tx *sql.Tx, c CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	var p taskIDPayload
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	version, status, err := taskVersionStatus(ctx, tx, c.Principal, p.TaskID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if err = checkVersion(req.ExpectedVersion, version); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if status == "complete" || status == "failed" || status == "canceled" {
		return contracts.CommandResult{}, nil, apiError(409, "state_conflict", "terminal task cannot be stopped", map[string]any{"status": status})
	}
	at := c.AcceptedAt.Format(time.RFC3339Nano)
	if status == "queued" {
		version++
		_, err = tx.ExecContext(ctx, `UPDATE tasks SET status='canceled',version=?,updated_at=?,finished_at=? WHERE id=? AND workspace_id=?`, version, at, at, p.TaskID, c.Principal.WorkspaceID)
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if err = settleDependentChildren(ctx, tx, p.TaskID, "canceled", c.AcceptedAt); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if err = cancelPendingChildProposals(ctx, tx, p.TaskID, c.AcceptedAt); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE approvals SET status='canceled',version=version+1 WHERE task_id=? AND workspace_id=? AND status='pending'`, p.TaskID, c.Principal.WorkspaceID); err != nil {
			return contracts.CommandResult{}, nil, err
		}
	} else {
		if _, err = tx.ExecContext(ctx, `UPDATE runs SET desired_control='stop',control_version=control_version+1 WHERE task_id=? AND workspace_id=? AND status IN ('dispatching','active','uncertain')`, p.TaskID, c.Principal.WorkspaceID); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		version++
		_, err = tx.ExecContext(ctx, `UPDATE tasks SET version=?,updated_at=? WHERE id=? AND workspace_id=?`, version, at, p.TaskID, c.Principal.WorkspaceID)
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE approvals SET status='canceled',version=version+1 WHERE task_id=? AND workspace_id=? AND status='pending'`, p.TaskID, c.Principal.WorkspaceID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	return contracts.CommandResult{ResourceType: "task", ResourceID: p.TaskID, ResourceVersion: version}, []contracts.Event{event("task.stop_requested", "task", p.TaskID, version, map[string]any{"task_id": p.TaskID, "reason": p.Reason, "status": status})}, nil
}

func (s *Server) commandTaskApprove(ctx context.Context, tx *sql.Tx, c CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	var p approvePayload
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if p.TaskID == "" || p.ApprovalID == "" || (p.Decision != "approve" && p.Decision != "reject") {
		return contracts.CommandResult{}, nil, apiError(400, "invalid_request", "task_id, approval_id, and decision approve|reject are required", nil)
	}
	var taskID, status, boundStatus string
	var version, boundTaskVersion, currentTaskVersion int64
	var expires sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT task_id,status,version,task_version,expires_at FROM approvals WHERE id=? AND workspace_id=?`, p.ApprovalID, c.Principal.WorkspaceID).Scan(&taskID, &status, &version, &boundTaskVersion, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(404, "not_found", "approval not found", nil)
	}
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if taskID != p.TaskID {
		return contracts.CommandResult{}, nil, apiError(404, "not_found", "approval is not bound to this task", nil)
	}
	if err = checkVersion(req.ExpectedVersion, version); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if status != "pending" {
		return contracts.CommandResult{}, nil, apiError(409, "state_conflict", "approval is no longer pending", map[string]any{"status": status})
	}
	currentTaskVersion, boundStatus, err = taskVersionStatus(ctx, tx, c.Principal, p.TaskID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if boundStatus == "complete" || boundStatus == "failed" || boundStatus == "canceled" {
		return contracts.CommandResult{}, nil, apiError(409, "state_conflict", "approval cannot change a terminal task", map[string]any{"task_status": boundStatus})
	}
	if boundTaskVersion != currentTaskVersion {
		return contracts.CommandResult{}, nil, apiError(409, "version_conflict", "approval is bound to an older task version", map[string]any{"expected_version": boundTaskVersion, "current_version": currentTaskVersion})
	}
	if expires.Valid {
		deadline, e := time.Parse(time.RFC3339Nano, expires.String)
		if e == nil && c.AcceptedAt.After(deadline) {
			return contracts.CommandResult{}, nil, apiError(409, "state_conflict", "approval has expired", nil)
		}
	}
	next := "approved"
	if p.Decision == "reject" {
		next = "rejected"
	}
	at := c.AcceptedAt.Format(time.RFC3339Nano)
	version++
	_, err = tx.ExecContext(ctx, `UPDATE approvals SET status=?,version=?,decided_at=?,decided_by_device_id=? WHERE id=? AND status='pending'`, next, version, at, c.Principal.DeviceID, p.ApprovalID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	return contracts.CommandResult{ResourceType: "approval", ResourceID: p.ApprovalID, ResourceVersion: version}, []contracts.Event{event("task.approval_decided", "approval", p.ApprovalID, version, map[string]any{"task_id": p.TaskID, "approval_id": p.ApprovalID, "approval_version": version, "task_version": boundTaskVersion, "decision": p.Decision, "comment": p.Comment})}, nil
}

type acceptResultPayload struct {
	TaskID   string `json:"task_id"`
	RunID    string `json:"run_id"`
	Decision string `json:"decision"`
	Comment  string `json:"comment,omitempty"`
}

func (s *Server) commandTaskAcceptResult(ctx context.Context, tx *sql.Tx, c CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if !HasScope(c.Principal, "client:command") {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "forbidden", "client:command scope is required", nil)
	}
	var p acceptResultPayload
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if p.TaskID == "" || p.RunID == "" || (p.Decision != "accept" && p.Decision != "reject") {
		return contracts.CommandResult{}, nil, apiError(400, "invalid_request", "task_id, run_id, and decision accept|reject are required", nil)
	}
	version, status, err := taskVersionStatus(ctx, tx, c.Principal, p.TaskID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if req.ExpectedVersion == nil {
		return contracts.CommandResult{}, nil, apiError(400, "invalid_request", "expected_version is required for result acceptance", nil)
	}
	if err = checkVersion(req.ExpectedVersion, version); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if status != "awaiting_user" {
		return contracts.CommandResult{}, nil, apiError(409, "state_conflict", "task has no result awaiting user acceptance", map[string]any{"status": status})
	}
	var runStatus string
	err = tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE id=? AND task_id=? AND workspace_id=?`, p.RunID, p.TaskID, c.Principal.WorkspaceID).Scan(&runStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(404, "not_found", "task run not found", nil)
	}
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if runStatus != "verification_pending" {
		return contracts.CommandResult{}, nil, apiError(409, "state_conflict", "run termination is not confirmed", map[string]any{"run_status": runStatus})
	}
	var evidenceCount int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM evidence WHERE workspace_id=? AND task_id=? AND run_id=? AND kind='agent_result' AND source='runner' AND verified=0`, c.Principal.WorkspaceID, p.TaskID, p.RunID).Scan(&evidenceCount); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if evidenceCount == 0 {
		return contracts.CommandResult{}, nil, apiError(409, "state_conflict", "run has no unaccepted result evidence", nil)
	}
	next := "complete"
	if p.Decision == "reject" {
		next = "waiting"
	}
	version++
	if !validTaskTransition(status, next) {
		return contracts.CommandResult{}, nil, apiError(409, "state_conflict", "task transition is not allowed", map[string]any{"from": status, "to": next})
	}
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET status=?,version=?,updated_at=?,finished_at=CASE WHEN ?='complete' THEN ? ELSE NULL END WHERE id=? AND workspace_id=?`, next, version, c.AcceptedAt.Format(time.RFC3339Nano), next, c.AcceptedAt.Format(time.RFC3339Nano), p.TaskID, c.Principal.WorkspaceID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if next == "complete" {
		_, err = tx.ExecContext(ctx, `UPDATE runs SET status='verified' WHERE id=? AND status='verification_pending'`, p.RunID)
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if err = settleDependentChildren(ctx, tx, p.TaskID, "complete", c.AcceptedAt); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if err = cancelPendingChildProposals(ctx, tx, p.TaskID, c.AcceptedAt); err != nil {
			return contracts.CommandResult{}, nil, err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE approvals SET status='canceled',version=version+1 WHERE task_id=? AND workspace_id=? AND status='pending'`, p.TaskID, c.Principal.WorkspaceID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	return contracts.CommandResult{ResourceType: "task", ResourceID: p.TaskID, ResourceVersion: version}, []contracts.Event{event("task.result_decided", "task", p.TaskID, version, map[string]any{"task_id": p.TaskID, "run_id": p.RunID, "decision": p.Decision, "comment": p.Comment, "status": next})}, nil
}

func validTaskTransition(from, to string) bool {
	switch from {
	case "queued":
		return to == "active" || to == "canceled"
	case "active":
		return to == "waiting" || to == "awaiting_user" || to == "paused" || to == "canceled" || to == "failed"
	case "waiting":
		return to == "queued" || to == "awaiting_user" || to == "canceled" || to == "failed"
	case "awaiting_user":
		return to == "queued" || to == "waiting" || to == "complete" || to == "canceled" || to == "failed"
	case "paused":
		return to == "queued" || to == "canceled" || to == "failed"
	}
	return false
}

func taskVersionStatus(ctx context.Context, tx *sql.Tx, principal contracts.Principal, id string) (int64, string, error) {
	var v int64
	var status string
	err := tx.QueryRowContext(ctx, `SELECT version,status FROM tasks WHERE id=? AND workspace_id=?`, id, principal.WorkspaceID).Scan(&v, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", apiError(404, "not_found", "task not found", nil)
	}
	if err == nil {
		allowed, accessErr := PrincipalCanWriteTask(ctx, tx, principal, id)
		if accessErr != nil {
			return 0, "", accessErr
		}
		if !allowed {
			return 0, "", apiError(404, "not_found", "task not found", nil)
		}
	}
	return v, status, err
}
func checkVersion(expected *int64, current int64) error {
	if expected != nil && *expected != current {
		return apiError(409, "version_conflict", "resource version is stale", map[string]any{"expected_version": *expected, "current_version": current})
	}
	return nil
}
func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

var _ = fmt.Sprintf
var _ = http.StatusOK
