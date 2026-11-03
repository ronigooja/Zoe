package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

func (s *Server) registerCoreRoutes() {
	_ = s.RegisterCommand("project.create", s.commandProjectCreate)
	_ = s.RegisterCommand("task.create", s.commandTaskCreate)
	_ = s.RegisterCommand("task.append", s.commandTaskAppend)
	_ = s.RegisterCommand("task.stop", s.commandTaskStop)
	_ = s.RegisterCommand("task.approve", s.commandTaskApprove)
	_ = s.RegisterCommand("task.accept_result", s.commandTaskAcceptResult)
	_ = s.RegisterCommand("child.approve", s.commandChildDecision)
	_ = s.RegisterCommand("child.reject", s.commandChildDecision)
	_ = s.RegisterCommand("device.push_token.register", s.commandPushTokenRegister)
	_ = s.RegisterCommand("device.push_token.revoke", s.commandPushTokenRevoke)
	_ = s.RegisterCommand("notification.mark_read", s.commandNotificationMarkRead)
	s.device.HandleFunc("POST /v1/commands", s.handleCommand)
	s.device.HandleFunc("GET /v1/commands/{command_id}", s.handleGetCommand)
	s.device.HandleFunc("GET /v1/commands", s.handleGetCommandByKey)
	s.device.HandleFunc("GET /v1/snapshot", s.handleSnapshot)
	s.device.HandleFunc("GET /v1/screen", s.handleScreen)
	s.device.HandleFunc("GET /v1/tasks/{task_id}/entries", s.handleTaskEntries)
	s.device.HandleFunc("GET /v1/events", s.handleEvents)
	s.device.HandleFunc("GET /v1/notifications", s.handleNotifications)
	s.runner.HandleFunc("POST /runner/v1/claims", s.handleClaims)
	s.runner.HandleFunc("POST /runner/v1/runs/{run_id}/proposals", s.handleChildProposal)
	s.runner.HandleFunc("GET /runner/v1/claims/current", s.handleCurrentClaims)
	s.runner.HandleFunc("GET /runner/v1/runs/{run_id}", s.handleRunControl)
	s.runner.HandleFunc("GET /runner/v1/runs/{run_id}/rotation-context", s.handleRotationContext)
	s.runner.HandleFunc("GET /runner/v1/runs/{run_id}/authorizations/{request_id}", s.handleRuntimeAuthorization)
	s.runner.HandleFunc("POST /runner/v1/runs/{run_id}/dispatch-authorization", s.handleDispatchAuthorization)
	s.runner.HandleFunc("POST /runner/v1/runs/{run_id}/observations", s.handleObservation)
	s.runner.HandleFunc("POST /runner/v1/runs/{run_id}/budget/reservations", s.handleRunBudgetReserve)
	s.runner.HandleFunc("POST /runner/v1/runs/{run_id}/budget/reservations/{reservation_id}/settlement", s.handleRunBudgetSettle)
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, apiError(401, "unauthenticated", "device authentication required", nil))
		return
	}
	if !HasScope(p, "client:command") {
		writeError(w, apiError(http.StatusForbidden, "forbidden", "client:command scope is required", nil))
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		writeError(w, apiError(400, "invalid_request", "Idempotency-Key is required and must be at most 200 bytes", nil))
		return
	}
	var req contracts.CommandRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	req.Kind = strings.TrimSpace(req.Kind)
	if req.Kind == "" || len(req.Payload) == 0 {
		writeError(w, apiError(400, "invalid_request", "kind and payload are required", nil))
		return
	}
	canonical, err := canonicalCommand(req)
	if err != nil {
		writeError(w, apiError(400, "invalid_request", "payload must be a JSON object", nil))
		return
	}
	h := sha256.Sum256(canonical)
	receipt, err := s.ExecuteCommand(r.Context(), p, key, req, h[:])
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, receipt)
}

func canonicalCommand(req contracts.CommandRequest) ([]byte, error) {
	var payload any
	decoder := json.NewDecoder(bytes.NewReader(req.Payload))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	if _, ok := payload.(map[string]any); !ok {
		return nil, errors.New("payload must be object")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("payload must contain one JSON value")
	}
	req.Payload, _ = json.Marshal(payload)
	return json.Marshal(req)
}

func (s *Server) ExecuteCommand(ctx context.Context, p contracts.Principal, key string, req contracts.CommandRequest, requestHash []byte) (contracts.CommandReceipt, error) {
	s.mu.RLock()
	handler := s.commands[req.Kind]
	s.mu.RUnlock()
	if handler == nil {
		return contracts.CommandReceipt{}, apiError(400, "unknown_command", "command kind is not registered", map[string]any{"kind": req.Kind})
	}
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return contracts.CommandReceipt{}, err
	}
	defer tx.Rollback()
	if err = requireOwnerSessionTx(ctx, tx, p); err != nil {
		return contracts.CommandReceipt{}, err
	}

	var oldHash, receiptJSON []byte
	err = tx.QueryRowContext(ctx, `SELECT request_hash,receipt_json FROM command_receipts WHERE workspace_id=? AND subject_id=? AND idempotency_key=?`, p.WorkspaceID, p.SubjectID, key).Scan(&oldHash, &receiptJSON)
	if err == nil {
		if !equalBytes(oldHash, requestHash) {
			return contracts.CommandReceipt{}, apiError(409, "idempotency_conflict", "Idempotency-Key was already used with different command content", nil)
		}
		var receipt contracts.CommandReceipt
		if e := json.Unmarshal(receiptJSON, &receipt); e != nil {
			return receipt, fmt.Errorf("decode durable command receipt: %w", e)
		}
		return receipt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandReceipt{}, err
	}
	commandID, err := randomID()
	if err != nil {
		return contracts.CommandReceipt{}, err
	}
	accepted := time.Now().UTC()
	result, events, err := handler(ctx, tx, CommandContext{CommandID: commandID, Principal: p, IdempotencyKey: key, AcceptedAt: accepted}, req)
	if err != nil {
		return contracts.CommandReceipt{}, err
	}
	var cursor int64
	for i := range events {
		event := &events[i]
		event.EventID, _ = randomID()
		event.SchemaVersion = 1
		event.WorkspaceID = p.WorkspaceID
		event.SubjectID = p.SubjectID
		if event.OccurredAt.IsZero() {
			event.OccurredAt = accepted
		}
		var e error
		e = tx.QueryRowContext(ctx, `UPDATE event_counters SET current_cursor=current_cursor+1 WHERE workspace_id=? RETURNING current_cursor`, p.WorkspaceID).Scan(&cursor)
		if e != nil {
			return contracts.CommandReceipt{}, e
		}
		event.Cursor = fmt.Sprint(cursor)
		if event.AggregateType == "" || event.AggregateID == "" {
			return contracts.CommandReceipt{}, apiError(500, "invalid_event", "command handler returned event without aggregate identity", nil)
		}
		if len(event.Payload) == 0 {
			event.Payload = json.RawMessage(`{}`)
		}
		payload, e := json.Marshal(event)
		if e != nil {
			return contracts.CommandReceipt{}, e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO events(event_id,workspace_id,cursor,schema_version,event_type,occurred_at,subject_id,aggregate_type,aggregate_id,aggregate_version,payload) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, event.EventID, p.WorkspaceID, cursor, event.SchemaVersion, event.Type, event.OccurredAt.Format(time.RFC3339Nano), p.SubjectID, event.AggregateType, event.AggregateID, event.AggregateVersion, payload)
		if e != nil {
			return contracts.CommandReceipt{}, e
		}
		if e = enqueueEventNotification(ctx, tx, *event); e != nil {
			return contracts.CommandReceipt{}, e
		}
	}
	if cursor == 0 {
		if err = tx.QueryRowContext(ctx, `SELECT current_cursor FROM event_counters WHERE workspace_id=?`, p.WorkspaceID).Scan(&cursor); err != nil {
			return contracts.CommandReceipt{}, err
		}
	}
	receipt := contracts.CommandReceipt{CommandID: commandID, IdempotencyKey: key, WorkspaceID: p.WorkspaceID, Kind: req.Kind, Status: "accepted", ResourceType: result.ResourceType, ResourceID: result.ResourceID, ResourceVersion: result.ResourceVersion, AcceptedAt: accepted, EventCursor: fmt.Sprint(cursor)}
	receiptJSON, err = json.Marshal(receipt)
	if err != nil {
		return receipt, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO command_receipts(command_id,workspace_id,subject_id,idempotency_key,request_hash,receipt_json,accepted_at) VALUES(?,?,?,?,?,?,?)`, commandID, p.WorkspaceID, p.SubjectID, key, requestHash, receiptJSON, accepted.Format(time.RFC3339Nano))
	if err != nil {
		return receipt, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO command_work(command_id,workspace_id,kind,status,created_at) VALUES(?,?,?,'accepted',?)`, commandID, p.WorkspaceID, req.Kind, accepted.Format(time.RFC3339Nano))
	if err != nil {
		return receipt, err
	}
	if err = tx.Commit(); err != nil {
		return contracts.CommandReceipt{}, err
	}
	return receipt, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var n byte
	for i := range a {
		n |= a[i] ^ b[i]
	}
	return n == 0
}

func (s *Server) handleGetCommand(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	var raw []byte
	err := s.store.DB().QueryRowContext(r.Context(), `SELECT receipt_json FROM command_receipts WHERE command_id=? AND workspace_id=? AND subject_id=?`, r.PathValue("command_id"), p.WorkspaceID, p.SubjectID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, apiError(404, "not_found", "command receipt not found", nil))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	var receipt contracts.CommandReceipt
	if err = json.Unmarshal(raw, &receipt); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, receipt)
}
func (s *Server) handleGetCommandByKey(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	key := r.URL.Query().Get("key")
	if key == "" {
		writeError(w, apiError(400, "invalid_request", "key query parameter is required", nil))
		return
	}
	var raw []byte
	err := s.store.DB().QueryRowContext(r.Context(), `SELECT receipt_json FROM command_receipts WHERE workspace_id=? AND subject_id=? AND idempotency_key=?`, p.WorkspaceID, p.SubjectID, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, apiError(404, "not_found", "command receipt not found", nil))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	var receipt contracts.CommandReceipt
	if err = json.Unmarshal(raw, &receipt); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, receipt)
}

var _ context.Context
var _ = http.StatusOK
