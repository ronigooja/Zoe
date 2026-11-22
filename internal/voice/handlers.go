package voice

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/logging"
)

var ErrModelConsumerStopped = errors.New("model consumer stopped")

func (s *Service) interactiveModel(ctx context.Context, consumer string, call func() error) error {
	store := s.retry
	if store.DB == nil {
		store.DB = s.db
	}
	due, err := store.Due(ctx, consumer, "", s.now())
	if err != nil {
		logging.Warnf("model_retry_state_unreadable consumer=%s", consumer)
		due = true
	}
	if !due {
		return ErrModelConsumerStopped
	}
	err = call()
	if err != nil && backoff.Classify(err) == backoff.Auth {
		if recordErr := store.Record(ctx, consumer, "", backoff.Auth, s.now(), backoff.Interactive, nil); recordErr != nil {
			logging.Warnf("model_retry_record_failed consumer=%s", consumer)
		} else {
			s.PublishRetryState()
		}
	} else if err == nil {
		_ = store.Clear(ctx, consumer, "")
	}
	return err
}

func (s *Service) handleReadSession(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	var result sessionResponse
	err := s.db.QueryRowContext(r.Context(), `SELECT id,generation,state FROM voice_sessions WHERE workspace_id=? AND owner_id=? AND id=?`, p.WorkspaceID, p.SubjectID, r.PathValue("session_id")).Scan(&result.SessionID, &result.Generation, &result.State)
	if errors.Is(err, sql.ErrNoRows) {
		control.WriteError(w, apiError(http.StatusNotFound, "not_found", "voice session not found", nil))
		return
	}
	if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Service) handleTurns(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	var owner string
	if err := s.db.QueryRowContext(r.Context(), `SELECT owner_id FROM voice_sessions WHERE workspace_id=? AND id=?`, p.WorkspaceID, r.PathValue("session_id")).Scan(&owner); errors.Is(err, sql.ErrNoRows) {
		control.WriteError(w, apiError(http.StatusNotFound, "not_found", "voice session not found", nil))
		return
	} else if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	if owner != p.SubjectID {
		control.WriteError(w, apiError(http.StatusForbidden, "forbidden", "voice session belongs to another subject", nil))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT turn_id,generation,revision,provider_message_id,content,confirmed_at FROM voice_turns WHERE session_id=? ORDER BY confirmed_at,turn_id,revision`, r.PathValue("session_id"))
	if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	defer rows.Close()
	type turnView struct {
		TurnID            string `json:"turn_id"`
		Generation        int64  `json:"generation"`
		Revision          int64  `json:"revision"`
		ProviderMessageID string `json:"provider_message_id,omitempty"`
		Content           string `json:"content"`
		ConfirmedAt       string `json:"confirmed_at"`
	}
	items := []turnView{}
	for rows.Next() {
		var item turnView
		if err := rows.Scan(&item.TurnID, &item.Generation, &item.Revision, &item.ProviderMessageID, &item.Content, &item.ConfirmedAt); err != nil {
			control.WriteError(w, safeSessionError(err))
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"turns": items})
}

func (s *Service) handleReceipts(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	var owner string
	if err := s.db.QueryRowContext(r.Context(), `SELECT owner_id FROM voice_sessions WHERE workspace_id=? AND id=?`, p.WorkspaceID, r.PathValue("session_id")).Scan(&owner); errors.Is(err, sql.ErrNoRows) {
		control.WriteError(w, apiError(http.StatusNotFound, "not_found", "voice session not found", nil))
		return
	} else if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	if owner != p.SubjectID {
		control.WriteError(w, apiError(http.StatusForbidden, "forbidden", "voice session belongs to another subject", nil))
		return
	}
	type receiptView struct {
		EventID     string          `json:"event_id"`
		Cursor      string          `json:"cursor"`
		Type        string          `json:"type"`
		TaskID      string          `json:"task_id"`
		TaskVersion int64           `json:"task_version"`
		OccurredAt  string          `json:"occurred_at"`
		Payload     json.RawMessage `json:"payload"`
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT e.event_id,e.cursor,e.event_type,e.aggregate_id,e.aggregate_version,e.occurred_at,e.payload FROM voice_task_links l JOIN events e ON e.workspace_id=l.workspace_id AND e.aggregate_type='task' AND e.aggregate_id=l.task_id WHERE l.workspace_id=? AND l.session_id=? AND l.generation=(SELECT generation FROM voice_sessions WHERE id=l.session_id) ORDER BY e.cursor`, p.WorkspaceID, r.PathValue("session_id"))
	if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	defer rows.Close()
	items := []receiptView{}
	for rows.Next() {
		var item receiptView
		if err := rows.Scan(&item.EventID, &item.Cursor, &item.Type, &item.TaskID, &item.TaskVersion, &item.OccurredAt, &item.Payload); err != nil {
			control.WriteError(w, safeSessionError(err))
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"receipts": items})
}

type linkTaskPayload struct {
	SessionID    string `json:"session_id"`
	Generation   int64  `json:"generation"`
	TurnID       string `json:"turn_id"`
	TurnRevision int64  `json:"turn_revision"`
	TaskID       string `json:"task_id"`
}

type voiceTaskSourceReference struct {
	Kind        string `json:"kind"`
	SessionID   string `json:"voice_session_id"`
	Generation  int64  `json:"generation"`
	TurnID      string `json:"turn_id"`
	Revision    int64  `json:"revision"`
	ContentHash string `json:"content_hash,omitempty"`
}

// admitVoiceTask binds a confirmed stable turn to the task inside the same
// transaction as task.create. Revision is provenance; it is deliberately not
// part of the uniqueness key, so a later transcript revision cannot dispatch
// a second task.
func (s *Service) admitVoiceTask(ctx context.Context, tx *sql.Tx, principal contracts.Principal, taskID string, request *control.TaskSourceRequest, raw json.RawMessage, acceptedAt time.Time) ([]contracts.Event, error) {
	var ref voiceTaskSourceReference
	if err := decodePayload(raw, &ref); err != nil {
		return nil, apiError(http.StatusBadRequest, "invalid_source", "voice source reference is invalid", nil)
	}
	if ref.Kind != "voice_turn" || ref.SessionID == "" || ref.Generation < 1 || ref.TurnID == "" || ref.Revision < 1 {
		return nil, apiError(http.StatusBadRequest, "invalid_source", "voice source reference requires a confirmed stable turn identity", nil)
	}
	var owner, deviceID, state string
	var generation int64
	err := tx.QueryRowContext(ctx, `SELECT owner_id,device_id,state,generation FROM voice_sessions WHERE workspace_id=? AND id=?`, principal.WorkspaceID, ref.SessionID).Scan(&owner, &deviceID, &state, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apiError(http.StatusNotFound, "voice_turn_not_found", "voice session was not found", nil)
	}
	if err != nil {
		return nil, err
	}
	if owner != principal.SubjectID || deviceID != principal.DeviceID {
		return nil, apiError(http.StatusForbidden, "invalid_source", "voice turn belongs to another subject or device", nil)
	}
	if generation != ref.Generation {
		return nil, apiError(http.StatusConflict, "voice_generation_stale", "voice turn is not from the bound media generation", nil)
	}
	var content string
	var resolved sql.NullString
	var storedExits sql.NullString
	var transcriptSeq sql.NullInt64
	var contentHash []byte
	err = tx.QueryRowContext(ctx, `SELECT content,content_hash,exits,request,transcript_seq FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, principal.WorkspaceID, ref.SessionID, ref.Generation, ref.TurnID, ref.Revision).Scan(&content, &contentHash, &storedExits, &resolved, &transcriptSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apiError(http.StatusNotFound, "voice_turn_not_found", "confirmed voice turn revision was not found", nil)
	}
	if err != nil {
		return nil, err
	}
	if transcriptSeq.Valid && !request.TrustedSource {
		return nil, apiError(http.StatusForbidden, "invalid_source", "delegated turns are admitted by the voice worker", nil)
	}
	if request.TrustedSource && (!transcriptSeq.Valid || ref.ContentHash != hex.EncodeToString(contentHash)) {
		return nil, apiError(http.StatusConflict, "invalid_source", "delegation does not match the accepted turn", nil)
	}
	if !transcriptSeq.Valid && state != "active" {
		return nil, apiError(http.StatusConflict, "voice_generation_stale", "voice turn is not from the active media generation", nil)
	}
	var existingTask string
	err = tx.QueryRowContext(ctx, `SELECT task_id FROM voice_task_links WHERE session_id=? AND generation=? AND turn_id=?`, ref.SessionID, ref.Generation, ref.TurnID).Scan(&existingTask)
	if err == nil {
		return nil, apiError(http.StatusConflict, "voice_turn_already_dispatched", "this stable voice turn is already bound to a task", map[string]string{"task_id": existingTask})
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if !resolved.Valid || strings.TrimSpace(resolved.String) == "" || strings.EqualFold(strings.TrimSpace(resolved.String), "none") {
		return nil, apiError(http.StatusConflict, "request_unavailable", "a complete register request is required", nil)
	}
	content = resolved.String
	request.Title = voiceTaskTitle(content)
	request.Goal = content
	request.InitialMessage = content
	if (RouteResult{Route: storedExits.String}).Has("ui") {
		if err := s.recordTaskUITx(ctx, tx, principal, taskID, ref.SessionID, ref.Generation, ref.TurnID, ref.Revision, acceptedAt); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET ui_display=1 WHERE workspace_id=? AND id=?`, principal.WorkspaceID, taskID); err != nil {
			return nil, err
		}
	}
	eventPayload, _ := json.Marshal(map[string]any{"task_id": taskID, "voice_session_id": ref.SessionID, "generation": ref.Generation, "turn_id": ref.TurnID, "turn_revision": ref.Revision, "content_hash": hex.EncodeToString(contentHash), "status": "task_created"})
	event := contracts.Event{Type: "voice.task.created", AggregateType: "voice_session", AggregateID: ref.SessionID, AggregateVersion: ref.Generation, Payload: eventPayload, OccurredAt: acceptedAt}
	_, err = tx.ExecContext(ctx, `INSERT INTO voice_task_links(workspace_id,session_id,generation,turn_id,turn_revision,task_id,linked_by_device_id,command_id,linked_at) VALUES(?,?,?,?,?,?,?,?,?)`, principal.WorkspaceID, ref.SessionID, ref.Generation, ref.TurnID, ref.Revision, taskID, principal.DeviceID, taskID, acceptedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, apiError(http.StatusConflict, "voice_turn_already_dispatched", "this stable voice turn is already bound to a task", nil)
		}
		return nil, err
	}
	return []contracts.Event{event}, nil
}

func (s *Service) commandLinkTask(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if !control.HasScope(cmd.Principal, "client:command") && !control.HasScope(cmd.Principal, "voice:turn:commit") {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "forbidden", "voice task linking requires command scope", nil)
	}
	var payload linkTaskPayload
	if err := decodePayload(req.Payload, &payload); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if payload.SessionID == "" || payload.Generation < 1 || payload.TurnID == "" || payload.TurnRevision < 1 || payload.TaskID == "" {
		return contracts.CommandResult{}, nil, apiError(http.StatusBadRequest, "invalid_request", "session, generation, turn revision, and task are required", nil)
	}
	var owner, sessionState string
	var currentGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT owner_id,state,generation FROM voice_sessions WHERE workspace_id=? AND id=?`, cmd.Principal.WorkspaceID, payload.SessionID).Scan(&owner, &sessionState, &currentGeneration); errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "not_found", "voice session not found", nil)
	} else if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if owner != cmd.Principal.SubjectID || currentGeneration != payload.Generation {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "forbidden", "voice turn belongs to another owner or media generation", nil)
	}
	if sessionState == "quarantined" || sessionState == "allocating" || sessionState == "ending" {
		return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "voice_generation_unresolved", "task cannot be linked while media generation is unresolved", nil)
	}
	var transcriptSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT transcript_seq FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, cmd.Principal.WorkspaceID, payload.SessionID, payload.Generation, payload.TurnID, payload.TurnRevision).Scan(&transcriptSeq); errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "voice_turn_not_found", "confirmed voice turn revision not found", nil)
	} else if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if transcriptSeq.Valid {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "invalid_source", "delegated turns are linked by the voice worker", nil)
	}
	var taskSource, createdBy string
	var taskVersion int64
	if err := tx.QueryRowContext(ctx, `SELECT source,created_by_device_id,version FROM tasks WHERE workspace_id=? AND id=?`, cmd.Principal.WorkspaceID, payload.TaskID).Scan(&taskSource, &createdBy, &taskVersion); errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "not_found", "task not found", nil)
	} else if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if taskSource != "voice" || createdBy != cmd.Principal.DeviceID {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "forbidden", "only a voice-sourced task created by this device can be linked", nil)
	}
	if req.ExpectedVersion == nil || *req.ExpectedVersion != taskVersion {
		return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "version_conflict", "task version changed before voice linkage", map[string]any{"current_version": taskVersion})
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO voice_task_links(workspace_id,session_id,generation,turn_id,turn_revision,task_id,linked_by_device_id,command_id,linked_at) VALUES(?,?,?,?,?,?,?,?,?)`, cmd.Principal.WorkspaceID, payload.SessionID, payload.Generation, payload.TurnID, payload.TurnRevision, payload.TaskID, cmd.Principal.DeviceID, cmd.CommandID, cmd.AcceptedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "voice_task_link_conflict", "task or turn is already linked to a different voice record", nil)
		}
		return contracts.CommandResult{}, nil, err
	}
	payloadJSON, _ := json.Marshal(map[string]any{"task_id": payload.TaskID, "voice_session_id": payload.SessionID, "generation": payload.Generation, "turn_id": payload.TurnID, "turn_revision": payload.TurnRevision})
	event := contracts.Event{Type: "voice.task.linked", AggregateType: "voice_session", AggregateID: payload.SessionID, AggregateVersion: payload.Generation, Payload: payloadJSON}
	return contracts.CommandResult{ResourceType: "voice_task_link", ResourceID: payload.TaskID, ResourceVersion: taskVersion}, []contracts.Event{event}, nil
}

func (s *Service) handlePoolStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	if !control.HasScope(p, "voice:read") && !control.HasScope(p, "voice:pool:reconcile") && !control.HasScope(p, "client:admin") {
		control.WriteError(w, apiError(http.StatusForbidden, "forbidden", "voice:read scope is required", nil))
		return
	}
	var blocked int
	var reason, updatedAt string
	if err := s.db.QueryRowContext(r.Context(), `SELECT blocked,reason_code,updated_at FROM voice_pool_state WHERE id=1`).Scan(&blocked, &reason, &updatedAt); err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	// Only session creators may trigger automatic stale-create recovery here;
	// read-only status access does not authorize persistent recovery writes.
	if blocked != 0 && reason == "gateway_outcome_unknown" && control.HasScope(p, "voice:open") {
		s.recoverPoolStatus(r.Context())
		if err := s.db.QueryRowContext(r.Context(), `SELECT blocked,reason_code,updated_at FROM voice_pool_state WHERE id=1`).Scan(&blocked, &reason, &updatedAt); err != nil {
			control.WriteError(w, safeSessionError(err))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"blocked": blocked != 0, "reason_code": reason, "updated_at": updatedAt,
		"account_lease_identity":       s.gatewayReady(),
		"account_scoped_termination":   false,
		"trusted_termination_evidence": s.gatewayReady(),
	})
}

// handleVoiceConfig proxies only Space's documented non-secret voice choices,
// WebRTC DataChannel settings, and ICE server configuration. Gateway auth and
// account data are never returned to the client.
func (s *Service) handleVoiceConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := control.PrincipalFromContext(r.Context()); !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	if s.dialOrigin == "" {
		control.WriteError(w, apiError(http.StatusServiceUnavailable, "voice_unavailable", "gateway dial origin is unavailable", nil))
		return
	}
	config, err := s.readVoiceConfig(withGatewayRequestID(r.Context(), r.Header.Get("X-Zoe-Client-Request-ID")))
	if err != nil {
		control.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		spacePublicConfig
		GatewayDialOrigin string `json:"gateway_dial_origin"`
	}{config, s.dialOrigin})
}

func validDialOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.Opaque != "" || strings.ContainsAny(raw, "?#") || strings.HasSuffix(u.Host, ":") {
		return ""
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return ""
		}
	}
	return u.String()
}

func (s *Service) readVoiceConfig(ctx context.Context) (spacePublicConfig, error) {
	response, gatewayRequestID, err := s.callGateway(ctx, http.MethodGet, "/v1/voice/config", nil)
	if err != nil {
		return spacePublicConfig{}, apiError(http.StatusServiceUnavailable, "voice_unavailable", "Space voice configuration is unavailable", map[string]any{"request_id": gatewayRequestID})
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return spacePublicConfig{}, apiError(http.StatusServiceUnavailable, "voice_unavailable", "Space voice configuration is unavailable", map[string]any{"request_id": gatewayRequestID, "status": response.StatusCode})
	}
	var config spacePublicConfig
	dec := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&config); err != nil || config.Version == "" || len(config.WebRTC) == 0 {
		return spacePublicConfig{}, apiError(http.StatusBadGateway, "voice_config_invalid", "Space voice configuration response is invalid", map[string]any{"request_id": gatewayRequestID})
	}
	return config, nil
}

type spacePublicConfig struct {
	Version    string          `json:"version"`
	Defaults   json.RawMessage `json:"defaults"`
	Voices     json.RawMessage `json:"voices"`
	VoiceModes json.RawMessage `json:"voice_modes"`
	Languages  json.RawMessage `json:"languages"`
	WebRTC     json.RawMessage `json:"webrtc"`
}

func decodeRequest(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apiError(http.StatusBadRequest, "invalid_request", "invalid JSON request", nil)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return apiError(http.StatusBadRequest, "invalid_request", "request must contain one JSON value", nil)
	}
	return nil
}

func decodePayload(raw json.RawMessage, dst any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apiError(http.StatusBadRequest, "invalid_request", "voice command payload is invalid", nil)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
