package memory

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
	"github.com/ronigooja/Zoe/internal/storage"
)

type rememberPayload struct {
	Scope      Scope      `json:"scope"`
	Subject    string     `json:"subject"`
	Key        string     `json:"key"`
	Content    string     `json:"content"`
	SourceKind string     `json:"source_kind"`
	SourceRef  string     `json:"source_ref,omitempty"`
	ValidFrom  *time.Time `json:"valid_from,omitempty"`
	ValidTo    *time.Time `json:"valid_to,omitempty"`
}

type deletePayload struct {
	MemoryID         string `json:"memory_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

type proposalPayload struct {
	ProposalID string `json:"proposal_id"`
}

type runnerProposalRequest struct {
	WorkspaceID      string `json:"workspace_id"`
	SourceID         string `json:"source_id"`
	SourceVersion    int64  `json:"source_version"`
	ExtractorVersion string `json:"extractor_version"`
	Scope            Scope  `json:"scope"`
	Subject          string `json:"subject"`
	Key              string `json:"key"`
	Content          string `json:"content"`
}

type memoryEvent struct {
	MemoryID          string      `json:"memory_id"`
	Revision          int64       `json:"revision"`
	Status            ClaimStatus `json:"status"`
	InstanceLineageID string      `json:"instance_lineage_id,omitempty"`
	Generation        int64       `json:"generation,omitempty"`
	DeletionSequence  string      `json:"deletion_sequence,omitempty"`
}

func (s *Service) commandRemember(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	var payload rememberPayload
	if err := decodePayload(req.Payload, &payload); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if strings.TrimSpace(payload.Subject) == "" || len(payload.Subject) > 200 || strings.TrimSpace(payload.Key) == "" || len(payload.Key) > 200 || strings.TrimSpace(payload.Content) == "" || len(payload.Content) > 20_000 {
		return contracts.CommandResult{}, nil, featureError(http.StatusBadRequest, "invalid_request", "subject, key, and content are required within the size limits", nil)
	}
	scope, err := resolveWriteScope(ctx, tx, cmd.Principal, payload.Scope)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	allowed, err := mayWriteAccessScope(ctx, tx, cmd.Principal, scope)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if !mayWriteScope(cmd.Principal, scope) || !allowed {
		return contracts.CommandResult{}, nil, featureError(http.StatusForbidden, "forbidden", "memory write scope is required for the requested scope", nil)
	}
	source, err := resolveUserSource(ctx, tx, cmd, payload, scope)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	claim := Claim{WorkspaceID: cmd.Principal.WorkspaceID, Scope: scope, Subject: strings.TrimSpace(payload.Subject), Key: strings.TrimSpace(payload.Key), Content: strings.TrimSpace(payload.Content), Authority: AuthorityUserConfirmed, Status: ClaimActive, Revision: 1, ValidFrom: payload.ValidFrom, ValidTo: payload.ValidTo, Sources: []Source{source}}
	if req.ExpectedVersion != nil && *req.ExpectedVersion <= 0 {
		return contracts.CommandResult{}, nil, featureError(http.StatusBadRequest, "invalid_request", "expected_version must be positive", nil)
	}
	written, err := s.repo.PutTx(ctx, tx, WriteInput{WorkspaceID: cmd.Principal.WorkspaceID, ActorID: cmd.Principal.SubjectID, Claim: claim, Source: source, Expected: expectedVersion(req.ExpectedVersion)})
	if err != nil {
		return contracts.CommandResult{}, nil, mapMemoryError(err)
	}
	event := contracts.Event{Type: "memory.revised", AggregateType: "memory", AggregateID: written.ID, AggregateVersion: written.Revision, Payload: mustJSON(memoryEvent{MemoryID: written.ID, Revision: written.Revision, Status: written.Status})}
	return contracts.CommandResult{ResourceType: "memory", ResourceID: written.ID, ResourceVersion: written.Revision}, []contracts.Event{event}, nil
}

func (s *Service) commandDelete(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	var payload deletePayload
	if err := decodePayload(req.Payload, &payload); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if payload.MemoryID == "" || payload.ExpectedRevision < 1 || payload.Reason != "user_requested" {
		return contracts.CommandResult{}, nil, featureError(http.StatusBadRequest, "invalid_request", "memory_id, expected_revision, and user_requested reason are required", nil)
	}
	if req.ExpectedVersion != nil && *req.ExpectedVersion != payload.ExpectedRevision {
		return contracts.CommandResult{}, nil, featureError(http.StatusConflict, "version_conflict", "expected_version and expected_revision differ", nil)
	}
	var scope Scope
	if err := tx.QueryRowContext(ctx, `SELECT scope_kind,scope_id,principal_id FROM memory_claims WHERE workspace_id=? AND id=? AND deleted_at IS NULL`, cmd.Principal.WorkspaceID, payload.MemoryID).Scan(&scope.Kind, &scope.ID, &scope.PrincipalID); errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, featureError(http.StatusNotFound, "not_found", "memory record not found", nil)
	} else if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	allowed, err := mayWriteAccessScope(ctx, tx, cmd.Principal, scope)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if !mayDeleteScope(cmd.Principal, scope) || !allowed {
		return contracts.CommandResult{}, nil, featureError(http.StatusForbidden, "forbidden", "memory record is outside the caller's scope", nil)
	}
	deleted, err := s.repo.DeleteTx(ctx, tx, DeleteInput{WorkspaceID: cmd.Principal.WorkspaceID, ActorID: cmd.Principal.SubjectID, ClaimID: payload.MemoryID, Expected: payload.ExpectedRevision, ReasonCode: payload.Reason})
	if err != nil {
		return contracts.CommandResult{}, nil, mapMemoryError(err)
	}
	var lineage string
	var generation, deletionSequence int64
	if err := tx.QueryRowContext(ctx, `SELECT lineage_id,generation,deletion_sequence FROM zoe_instance_state WHERE singleton=1`).Scan(&lineage, &generation, &deletionSequence); err != nil {
		if !strings.Contains(err.Error(), "no such table") {
			return contracts.CommandResult{}, nil, err
		}
		lineage, generation, deletionSequence = "legacy-memory-test", 1, 0
	}
	event := contracts.Event{Type: "memory.deleted", AggregateType: "memory", AggregateID: deleted.ID, AggregateVersion: deleted.Revision, Payload: mustJSON(memoryEvent{MemoryID: deleted.ID, Revision: deleted.Revision, Status: deleted.Status, InstanceLineageID: lineage, Generation: generation, DeletionSequence: fmt.Sprint(deletionSequence)})}
	return contracts.CommandResult{ResourceType: "memory", ResourceID: deleted.ID, ResourceVersion: deleted.Revision}, []contracts.Event{event}, nil
}

func (s *Service) commandApproveProposal(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	var payload proposalPayload
	if err := decodePayload(req.Payload, &payload); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if payload.ProposalID == "" {
		return contracts.CommandResult{}, nil, featureError(http.StatusBadRequest, "invalid_request", "proposal_id is required", nil)
	}
	written, err := s.repo.ApproveProposalTx(ctx, tx, cmd.Principal, cmd.CommandID, payload.ProposalID, expectedVersion(req.ExpectedVersion), cmd.AcceptedAt)
	if err != nil {
		return contracts.CommandResult{}, nil, mapMemoryError(err)
	}
	event := contracts.Event{Type: "memory.proposal_accepted", AggregateType: "memory", AggregateID: written.ID, AggregateVersion: written.Revision, Payload: mustJSON(memoryEvent{MemoryID: written.ID, Revision: written.Revision, Status: written.Status})}
	return contracts.CommandResult{ResourceType: "memory", ResourceID: written.ID, ResourceVersion: written.Revision}, []contracts.Event{event}, nil
}

func (s *Service) handleRunnerProposal(w http.ResponseWriter, r *http.Request) {
	principal, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, featureError(http.StatusUnauthorized, "unauthenticated", "runner authentication required", nil))
		return
	}
	var payload runnerProposalRequest
	if err := decodeJSON(r, &payload); err != nil {
		control.WriteError(w, err)
		return
	}
	if payload.WorkspaceID == "" || !control.HasRunnerScope(principal, "memory:propose", payload.WorkspaceID) {
		control.WriteError(w, featureError(http.StatusForbidden, "forbidden", "runner memory:propose scope required for workspace", nil))
		return
	}
	proposal, err := s.repo.CreateProposal(r.Context(), principal, payload)
	if err != nil {
		control.WriteError(w, mapMemoryError(err))
		return
	}
	writeJSON(w, http.StatusAccepted, proposal)
}

type runnerProposalView struct {
	ID               string    `json:"id"`
	Status           string    `json:"status"`
	SourceID         string    `json:"source_id"`
	SourceVersion    int64     `json:"source_version"`
	ExtractorVersion string    `json:"extractor_version"`
	Scope            Scope     `json:"scope"`
	Subject          string    `json:"subject"`
	Key              string    `json:"key"`
	Content          string    `json:"content"`
	CreatedAt        time.Time `json:"created_at"`
}

func resolveWriteScope(ctx context.Context, tx *sql.Tx, p contracts.Principal, requested Scope) (Scope, error) {
	if p.WorkspaceID == "" || p.SubjectID == "" {
		return Scope{}, featureError(http.StatusUnauthorized, "unauthenticated", "authenticated workspace and subject are required", nil)
	}
	switch requested.Kind {
	case ScopePersonal:
		if requested.PrincipalID != "" && requested.PrincipalID != p.SubjectID {
			return Scope{}, featureError(http.StatusForbidden, "forbidden", "personal scope belongs to the authenticated subject", nil)
		}
		return Scope{Kind: ScopePersonal, PrincipalID: p.SubjectID}, nil
	case ScopeWorkspace:
		return Scope{Kind: ScopeWorkspace}, nil
	case ScopeProject:
		allowed, err := control.PrincipalCanAccessProject(ctx, tx, p, requested.ID)
		if requested.ID == "" || err != nil || !allowed {
			return Scope{}, featureError(http.StatusNotFound, "not_found", "project not found in the authenticated workspace", nil)
		}
		return Scope{Kind: ScopeProject, ID: requested.ID}, nil
	case ScopeTask:
		allowed, err := control.PrincipalCanAccessTask(ctx, tx, p, requested.ID)
		if requested.ID == "" || err != nil || !allowed {
			return Scope{}, featureError(http.StatusNotFound, "not_found", "task not found in the authenticated workspace", nil)
		}
		return Scope{Kind: ScopeTask, ID: requested.ID}, nil
	default:
		return Scope{}, featureError(http.StatusBadRequest, "invalid_request", "unsupported memory scope", nil)
	}
}

func resolveUserSource(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, p rememberPayload, claimScope Scope) (Source, error) {
	now := cmd.AcceptedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	source := Source{ID: uuid.NewString(), Version: 1, Scope: claimScope, Kind: p.SourceKind, RecordedAt: now.UTC(), Producer: "user"}
	switch p.SourceKind {
	case "user_note":
		if p.SourceRef != "" && p.SourceRef != "user_note" {
			return Source{}, featureError(http.StatusBadRequest, "invalid_source", "user_note source_ref must be omitted or user_note", nil)
		}
		source.Reference = "command:" + cmd.CommandID
	case "task_entry":
		var taskID, content string
		err := tx.QueryRowContext(ctx, `SELECT task_id,content FROM task_entries WHERE id=? AND workspace_id=?`, p.SourceRef, cmd.Principal.WorkspaceID).Scan(&taskID, &content)
		if errors.Is(err, sql.ErrNoRows) {
			return Source{}, featureError(http.StatusNotFound, "invalid_source", "task entry is not accessible", nil)
		}
		if err != nil {
			return Source{}, err
		}
		source.Scope = Scope{Kind: ScopeTask, ID: taskID}
		source.Reference = "task_entry:" + p.SourceRef
		source.ContentHash = hashString(content)
	case "event":
		var subject, aggregateType, aggregateID string
		var payload []byte
		err := tx.QueryRowContext(ctx, `SELECT coalesce(subject_id,''),aggregate_type,aggregate_id,payload FROM events WHERE event_id=? AND workspace_id=?`, p.SourceRef, cmd.Principal.WorkspaceID).Scan(&subject, &aggregateType, &aggregateID, &payload)
		if errors.Is(err, sql.ErrNoRows) {
			return Source{}, featureError(http.StatusNotFound, "invalid_source", "event is not accessible", nil)
		}
		if err != nil {
			return Source{}, err
		}
		if subject != "" && subject != cmd.Principal.SubjectID {
			return Source{}, featureError(http.StatusForbidden, "invalid_source", "event belongs to another subject", nil)
		}
		source.Scope = Scope{Kind: ScopeWorkspace}
		if aggregateType == "project" {
			source.Scope = Scope{Kind: ScopeProject, ID: aggregateID}
		} else if aggregateType == "task" {
			source.Scope = Scope{Kind: ScopeTask, ID: aggregateID}
		}
		source.Reference = "event:" + p.SourceRef
		source.ContentHash = hashBytes(payload)
	case "evidence":
		var taskID, summary string
		err := tx.QueryRowContext(ctx, `SELECT task_id,summary FROM evidence WHERE id=? AND workspace_id=?`, p.SourceRef, cmd.Principal.WorkspaceID).Scan(&taskID, &summary)
		if errors.Is(err, sql.ErrNoRows) {
			return Source{}, featureError(http.StatusNotFound, "invalid_source", "evidence is not accessible", nil)
		}
		if err != nil {
			return Source{}, err
		}
		source.Scope = Scope{Kind: ScopeTask, ID: taskID}
		source.Reference = "evidence:" + p.SourceRef
		source.ContentHash = hashString(summary)
	default:
		return Source{}, featureError(http.StatusBadRequest, "invalid_source", "source_kind must be user_note, task_entry, event, or evidence", nil)
	}
	if !sourceContains(source.Scope, claimScope) {
		return Source{}, featureError(http.StatusForbidden, "invalid_source", "memory scope cannot be broader than its source", nil)
	}
	return source, nil
}

func resolveCommandSource(ctx context.Context, tx *sql.Tx, p contracts.Principal, sourceID string, version int64) (Source, error) {
	var source Source
	var scopeKind string
	var recorded string
	err := tx.QueryRowContext(ctx, `SELECT id,version,scope_kind,scope_id,principal_id,kind,reference,content_hash,recorded_at,producer FROM memory_sources WHERE workspace_id=? AND id=? AND version=? AND deleted_at IS NULL`, p.WorkspaceID, sourceID, version).Scan(&source.ID, &source.Version, &scopeKind, &source.Scope.ID, &source.Scope.PrincipalID, &source.Kind, &source.Reference, &source.ContentHash, &recorded, &source.Producer)
	if errors.Is(err, sql.ErrNoRows) {
		return Source{}, ErrInvalidSource
	}
	if err != nil {
		return Source{}, err
	}
	source.Scope.Kind = ScopeKind(scopeKind)
	source.RecordedAt, _ = time.Parse(time.RFC3339Nano, recorded)
	if !mayReadScope(p, source.Scope) {
		return Source{}, ErrInvalidSource
	}
	allowed, err := mayAccessScope(ctx, tx, p, source.Scope)
	if err != nil {
		return Source{}, err
	}
	if !allowed {
		return Source{}, ErrInvalidSource
	}
	valid, err := sourceStillAvailable(ctx, tx, p.WorkspaceID, source)
	if err != nil {
		return Source{}, err
	}
	if !valid {
		return Source{}, ErrInvalidSource
	}
	return source, nil
}

func expectedVersion(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func decodePayload(raw json.RawMessage, dst any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return featureError(http.StatusBadRequest, "invalid_request", "command payload is invalid", nil)
	}
	return nil
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return featureError(http.StatusBadRequest, "invalid_request", "invalid JSON request", nil)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func featureError(status int, code, message string, details any) error {
	return &control.APIError{Status: status, Code: code, Message: message, Details: details}
}

func mapMemoryError(err error) error {
	switch {
	case errors.Is(err, storage.ErrRestoreDeletionReconciliationRequired):
		return featureError(http.StatusServiceUnavailable, "restore_deletion_reconciliation_required", "restored instance is closed until deletion reconciliation is completed", nil)
	case errors.Is(err, ErrVersionConflict):
		return featureError(http.StatusConflict, "version_conflict", "memory revision is stale", nil)
	case errors.Is(err, ErrContextStale):
		return featureError(http.StatusConflict, "context_stale", "memory changed while context was being assembled; retry the request", nil)
	case errors.Is(err, ErrNotFound):
		return featureError(http.StatusNotFound, "not_found", "memory record not found", nil)
	case errors.Is(err, ErrForbidden):
		return featureError(http.StatusForbidden, "forbidden", "memory access forbidden", nil)
	default:
		return err
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func hashString(value string) string { return hashBytes([]byte(value)) }
func hashBytes(value []byte) string {
	h := sha256.Sum256(value)
	return hex.EncodeToString(h[:])
}

func (s *Service) handleSearch(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok || !hasAnyMemoryAction(p, "read") {
		control.WriteError(w, featureError(http.StatusForbidden, "forbidden", "memory read scope is required", nil))
		return
	}
	filter := SearchFilter{ScopeKind: ScopeKind(r.URL.Query().Get("scope_kind")), ScopeID: strings.TrimSpace(r.URL.Query().Get("scope_id")), TaskID: strings.TrimSpace(r.URL.Query().Get("task_id"))}
	if value := r.URL.Query().Get("limit"); value != "" {
		if _, err := fmt.Sscanf(value, "%d", &filter.Limit); err != nil {
			control.WriteError(w, featureError(http.StatusBadRequest, "invalid_request", "limit must be an integer", nil))
			return
		}
	}
	result, err := s.repo.Search(r.Context(), p, r.URL.Query().Get("q"), filter)
	if err != nil {
		control.WriteError(w, mapMemoryError(err))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Service) handleContext(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok || !hasAnyMemoryAction(p, "read") {
		control.WriteError(w, featureError(http.StatusForbidden, "forbidden", "memory context scope is required", nil))
		return
	}
	taskID, recipientID := strings.TrimSpace(r.URL.Query().Get("task_id")), strings.TrimSpace(r.URL.Query().Get("recipient_id"))
	if recipientID == "" {
		recipientID = p.SubjectID
	}
	if recipientID != p.SubjectID && !control.HasScope(p, "memory:context:any") {
		control.WriteError(w, featureError(http.StatusForbidden, "forbidden", "recipient is outside the authenticated principal", nil))
		return
	}
	budget := 2048
	if raw := r.URL.Query().Get("budget"); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &budget); err != nil || budget < 1 || budget > contracts.MemoryContextMaxBudgetTokens {
			control.WriteError(w, featureError(http.StatusBadRequest, "invalid_request", "budget must be between 1 and 32000 tokens", nil))
			return
		}
	}
	manifest, claims, err := s.repo.AssembleContext(r.Context(), p, taskID, recipientID, r.URL.Query().Get("q"), budget)
	if err != nil {
		control.WriteError(w, mapMemoryError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"manifest": manifest, "items": claims})
}

func (r runnerProposalRequest) validate() error {
	if r.WorkspaceID == "" || r.SourceID == "" || r.SourceVersion < 1 || r.ExtractorVersion == "" || r.Subject == "" || r.Key == "" || r.Content == "" {
		return errors.New("proposal metadata is incomplete")
	}
	if len(r.Content) > 20_000 {
		return errors.New("proposal content exceeds limits")
	}
	return r.Scope.Validate()
}

func parseIntHeader(r *http.Request, name string) int64 {
	var version int64
	_, _ = fmt.Sscan(r.Header.Get(name), &version)
	return version
}
