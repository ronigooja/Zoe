package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/steward"
)

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	if !control.HasScope(p, "client:command") && !control.HasScope(p, "voice:open") {
		control.WriteError(w, apiError(http.StatusForbidden, "forbidden", "voice:open scope is required", nil))
		return
	}
	var req createRequest
	if err := decodeRequest(w, r, s.maxSDP+16_384, &req); err != nil {
		control.WriteError(w, err)
		return
	}
	result, err := s.create(withGatewayRequestID(r.Context(), r.Header.Get("X-Zoe-Client-Request-ID")), p, req)
	if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	writeJSON(w, http.StatusCreated, result)
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

func (s *Service) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	result, err := s.db.ExecContext(r.Context(), `UPDATE voice_sessions SET heartbeat_at=?,updated_at=? WHERE workspace_id=? AND owner_id=? AND id=? AND state='active'`, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), p.WorkspaceID, p.SubjectID, r.PathValue("session_id"))
	if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		control.WriteError(w, apiError(http.StatusConflict, "voice_lease_unavailable", "voice session is not active; uncertain media remains isolated", nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleOutcome(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	var req struct {
		Summary string `json:"summary"`
		Outcome string `json:"outcome"`
	}
	if err := decodeRequest(w, r, 32_768, &req); err != nil {
		control.WriteError(w, err)
		return
	}
	if strings.TrimSpace(req.Summary) == "" || len(req.Summary) > 20_000 || (req.Outcome != "completed" && req.Outcome != "failed") {
		control.WriteError(w, apiError(http.StatusBadRequest, "invalid_request", "bounded summary and completed or failed outcome are required", nil))
		return
	}
	if err := s.writeOutcome(r.Context(), p, r.PathValue("session_id"), strings.TrimSpace(req.Summary), req.Outcome); err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "source": "voice_session:" + r.PathValue("session_id")})
}

func (s *Service) handleConnected(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	var req struct {
		Generation int64 `json:"generation"`
	}
	if err := decodeRequest(w, r, 1024, &req); err != nil {
		control.WriteError(w, err)
		return
	}
	if req.Generation < 1 {
		control.WriteError(w, apiError(http.StatusBadRequest, "invalid_request", "generation is required", nil))
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(r.Context(), `UPDATE voice_sessions SET offer_answer='',heartbeat_at=?,updated_at=? WHERE workspace_id=? AND owner_id=? AND id=? AND generation=? AND state='active'`, now, now, p.WorkspaceID, p.SubjectID, r.PathValue("session_id"), req.Generation)
	if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		control.WriteError(w, apiError(http.StatusConflict, "voice_generation_stale", "active voice generation was not found", nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": "active"})
}

func (s *Service) handleHangup(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	var req struct {
		MediaClosed            bool   `json:"media_closed"`
		Generation             int64  `json:"generation"`
		AccountLeaseID         string `json:"account_lease_id"`
		AccountLeaseGeneration int64  `json:"account_lease_generation"`
	}
	if err := decodeRequest(w, r, 1024, &req); err != nil {
		control.WriteError(w, err)
		return
	}
	if !req.MediaClosed {
		control.WriteError(w, apiError(http.StatusBadRequest, "media_close_required", "close local tracks, DataChannel, and PeerConnection before releasing this lease", nil))
		return
	}
	if req.Generation < 1 || req.AccountLeaseGeneration < 0 {
		control.WriteError(w, apiError(http.StatusBadRequest, "invalid_request", "session and account lease generations are required", nil))
		return
	}
	var state, gatewayID, accountLeaseID string
	var generation int64
	var accountLeaseGeneration int64
	var gatewayMediaGeneration int64
	var accountIDHash []byte
	var gatewayUnbound int
	var gatewayUnbindUnknown int
	if err := s.db.QueryRowContext(r.Context(), `SELECT state,gateway_session_id,generation,account_lease_id,account_lease_generation,account_id_hash,gateway_unbound,gateway_unbind_unknown,gateway_media_generation FROM voice_sessions WHERE workspace_id=? AND owner_id=? AND id=?`, p.WorkspaceID, p.SubjectID, r.PathValue("session_id")).Scan(&state, &gatewayID, &generation, &accountLeaseID, &accountLeaseGeneration, &accountIDHash, &gatewayUnbound, &gatewayUnbindUnknown, &gatewayMediaGeneration); errors.Is(err, sql.ErrNoRows) {
		control.WriteError(w, apiError(http.StatusNotFound, "not_found", "voice session not found", nil))
		return
	} else if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	if generation != req.Generation || accountLeaseID != req.AccountLeaseID || accountLeaseGeneration != req.AccountLeaseGeneration {
		control.WriteError(w, apiError(http.StatusConflict, "voice_generation_stale", "hangup identity does not match the bound media and account lease generation", nil))
		return
	}
	if state == "ended" {
		var blocked int
		if err := s.db.QueryRowContext(r.Context(), `SELECT blocked FROM voice_pool_state WHERE id=1`).Scan(&blocked); err != nil {
			control.WriteError(w, safeSessionError(err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": "ended", "upstream_termination": "verified", "pool_blocked": blocked != 0})
		return
	}
	if state == "ending" {
		control.WriteError(w, apiError(http.StatusConflict, "voice_outcome_unknown", "gateway release is already unresolved; media lease remains quarantined", nil))
		return
	}
	if gatewayID == "" {
		if err := s.markSession(r.Context(), r.PathValue("session_id"), "quarantined", "gateway_session_unbound", nil, ""); err != nil {
			control.WriteError(w, safeSessionError(err))
			return
		}
		control.WriteError(w, apiError(http.StatusConflict, "voice_outcome_unknown", "gateway session binding is unknown; lease quarantined", nil))
		return
	}
	status, result, poolBlocked, err := s.releaseSession(withGatewayRequestID(r.Context(), r.Header.Get("X-Zoe-Client-Request-ID")), p, r.PathValue("session_id"), generation, gatewayID, accountLeaseID, accountLeaseGeneration, gatewayMediaGeneration, gatewayUnbound, gatewayUnbindUnknown, false)
	if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	writeJSON(w, status, map[string]any{"ok": true, "state": result, "upstream_termination": map[bool]string{true: "verified", false: "unverified"}[result == "ended"], "pool_blocked": poolBlocked})
}

func (s *Service) releaseSession(ctx context.Context, p contracts.Principal, sessionID string, generation int64, gatewayID, accountLeaseID string, accountLeaseGeneration, gatewayMediaGeneration int64, gatewayUnbound, gatewayUnbindUnknown int, claimed bool) (int, string, bool, error) {
	if !claimed {
		claim, err := s.db.ExecContext(ctx, `UPDATE voice_sessions SET state='ending',updated_at=? WHERE id=? AND generation=? AND state IN ('active','quarantined')`, s.now().UTC().Format(time.RFC3339Nano), sessionID, generation)
		if err != nil {
			return 0, "", true, err
		}
		if n, _ := claim.RowsAffected(); n != 1 {
			return 0, "", true, apiError(http.StatusConflict, "voice_outcome_unknown", "voice generation is already ending or otherwise unresolved; media lease remains quarantined", nil)
		}
	}
	if gatewayUnbound == 0 && gatewayUnbindUnknown == 0 {
		response, requestID, callErr := s.callGateway(ctx, http.MethodDelete, "/v1/voice/sessions/"+url.PathEscape(gatewayID), nil)
		if callErr != nil {
			if persistErr := s.markSession(ctx, sessionID, "quarantined", "gateway_release_unknown", nil, ""); persistErr != nil {
				return 0, "", true, persistErr
			}
			return http.StatusServiceUnavailable, "", true, apiError(http.StatusServiceUnavailable, "voice_outcome_unknown", "gateway release result is unknown; media lease quarantined", map[string]any{"request_id": requestID})
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			if persistErr := s.markSession(ctx, sessionID, "quarantined", "gateway_release_failed", nil, ""); persistErr != nil {
				return 0, "", true, persistErr
			}
			return http.StatusServiceUnavailable, "", true, apiError(http.StatusServiceUnavailable, "voice_outcome_unknown", "gateway did not confirm session unbinding; media lease quarantined", map[string]any{"request_id": requestID})
		}
		result, err := s.db.ExecContext(ctx, `UPDATE voice_sessions SET gateway_unbound=1,gateway_unbind_unknown=0,updated_at=? WHERE id=? AND generation=? AND state='ending'`, s.now().UTC().Format(time.RFC3339Nano), sessionID, generation)
		if err != nil {
			return 0, "", true, err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return 0, "", true, apiError(http.StatusConflict, "voice_generation_unresolved", "gateway unbind receipt could not be durably tied to this generation", nil)
		}
	}
	terminationVerified := false
	var observation AccountTerminationObservation
	if s.accountMedia != nil {
		c := s.accountMedia.Capabilities()
		if c.AccountLeaseIdentity && c.AccountScopedTermination && c.TrustedTerminationEvidence && accountLeaseID != "" && accountLeaseGeneration > 0 && gatewayMediaGeneration > 0 {
			leaseHash := sha256.Sum256([]byte(accountLeaseID))
			key := fmt.Sprintf("voice-release:%s:%d:%x:%d", sessionID, generation, leaseHash[:], accountLeaseGeneration)
			var observeErr error
			observation, observeErr = s.accountMedia.ReleaseAccountLease(ctx, AccountLeaseReference{LeaseID: accountLeaseID, Generation: accountLeaseGeneration, SessionID: gatewayID, SessionGeneration: gatewayMediaGeneration}, key)
			observedAt := observation.ObservedAt.UTC()
			if observeErr == nil && observation.SessionID == gatewayID && observation.SessionGeneration == gatewayMediaGeneration && observation.Terminated && observation.AccountID != "" && observation.LeaseID == accountLeaseID && observation.LeaseGeneration == accountLeaseGeneration && observation.EvidenceID != "" && !observedAt.IsZero() && !observedAt.After(s.now().UTC()) {
				terminationVerified = true
			}
		}
	}
	poolBlocked, err := s.completeHangup(ctx, p, sessionID, generation, observation, terminationVerified)
	if err != nil {
		return 0, "", true, err
	}
	if !terminationVerified {
		log.Printf("voice reaper/client release unverified for session %s", sessionID)
	}
	if terminationVerified {
		return http.StatusOK, "ended", poolBlocked, nil
	}
	return http.StatusAccepted, "quarantined", poolBlocked, nil
}

// completeHangup commits local closure, optional trusted upstream termination
// evidence, and the pool fence together. Until this transaction commits the
// session remains ending, so operator reconciliation cannot race an in-flight
// gateway/termination check and release another generation's account slot.
func (s *Service) completeHangup(ctx context.Context, principal contracts.Principal, sessionID string, generation int64, observation AccountTerminationObservation, terminationVerified bool) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return true, err
	}
	defer tx.Rollback()
	var state string
	var gatewayUnbindUnknown int
	if err := tx.QueryRowContext(ctx, `SELECT state,gateway_unbind_unknown FROM voice_sessions WHERE workspace_id=? AND owner_id=? AND id=? AND generation=?`, principal.WorkspaceID, principal.SubjectID, sessionID, generation).Scan(&state, &gatewayUnbindUnknown); err != nil {
		return true, err
	}
	if state != "ending" {
		return true, apiError(http.StatusConflict, "voice_generation_unresolved", "voice session changed while hangup was being confirmed", nil)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if terminationVerified {
		accountHash := sha256.Sum256([]byte(observation.AccountID))
		leaseHash := sha256.Sum256([]byte(observation.LeaseID))
		if _, err := tx.ExecContext(ctx, `INSERT INTO voice_account_terminations(id,session_id,generation,account_id_hash,lease_id_hash,evidence_id,observed_at,adapter_kind) VALUES(?,?,?,?,?,?,?,'trusted_account_media')`, uuid.NewString(), sessionID, generation, accountHash[:], leaseHash[:], observation.EvidenceID, observation.ObservedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return true, err
		}
	}
	if terminationVerified {
		if _, err := tx.ExecContext(ctx, `UPDATE voice_sessions SET state='ended',last_error_code='',offer_answer='',ended_at=?,updated_at=? WHERE workspace_id=? AND owner_id=? AND id=? AND generation=? AND state='ending'`, now, now, principal.WorkspaceID, principal.SubjectID, sessionID, generation); err != nil {
			return true, err
		}
	} else {
		reason := "upstream_termination_unverified"
		if gatewayUnbindUnknown != 0 {
			reason = "gateway_unbind_unknown"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE voice_sessions SET state='quarantined',last_error_code=?,offer_answer='',ended_at=NULL,updated_at=? WHERE workspace_id=? AND owner_id=? AND id=? AND generation=? AND state='ending'`, reason, now, principal.WorkspaceID, principal.SubjectID, sessionID, generation); err != nil {
			return true, err
		}
	}
	if terminationVerified {
		if _, err := tx.ExecContext(ctx, `UPDATE voice_pool_state SET blocked=0,reason_code='',source_session_id='',updated_at=?,updated_by='trusted_account_media' WHERE id=1 AND source_session_id=?`, now, sessionID); err != nil {
			return true, err
		}
	} else {
		reason := "upstream_termination_unverified"
		if gatewayUnbindUnknown != 0 {
			reason = "gateway_unbind_unknown"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE voice_pool_state SET blocked=1,reason_code=?,updated_at=?,updated_by='system' WHERE id=1 AND source_session_id=?`, reason, now, sessionID); err != nil {
			return true, err
		}
	}
	var blocked int
	if err := tx.QueryRowContext(ctx, `SELECT blocked FROM voice_pool_state WHERE id=1`).Scan(&blocked); err != nil {
		return true, err
	}
	if err := tx.Commit(); err != nil {
		return true, err
	}
	return blocked != 0, nil
}

type commitTurnPayload struct {
	SessionID       string `json:"session_id"`
	Generation      int64  `json:"generation"`
	TurnID          string `json:"turn_id"`
	Revision        int64  `json:"revision"`
	ProviderMessage string `json:"provider_message_id,omitempty"`
	Content         string `json:"content"`
	UserConfirmed   bool   `json:"user_confirmed"`
}

type handoffPayload struct {
	SessionID   string `json:"session_id"`
	Generation  int64  `json:"generation"`
	TurnID      string `json:"turn_id"`
	Revision    int64  `json:"revision"`
	ContentHash string `json:"content_hash"`
}

// commandHandoff is the server-owned boundary between a stable voice turn and
// Worker. It deliberately does not accept an impact level or routing decision
// from the caller. Ordinary requests dispatch unless the stateless Steward asks
// an intent question; hard high-impact holds require scoped confirmation.
func (s *Service) commandHandoff(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest, server *control.Server) (contracts.CommandResult, []contracts.Event, error) {
	if !control.HasScope(cmd.Principal, "client:command") && !control.HasScope(cmd.Principal, "voice:turn:commit") {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "forbidden", "voice handoff scope is required", nil)
	}
	var p handoffPayload
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if p.SessionID == "" || p.Generation < 1 || p.TurnID == "" || p.Revision < 1 || len(p.ContentHash) != sha256.Size*2 {
		return contracts.CommandResult{}, nil, apiError(http.StatusBadRequest, "invalid_request", "session, generation, turn revision, and content hash are required", nil)
	}
	// Classification preparation runs before ExecuteCommand acquires its sole
	// database connection. Read the durable routing decision inside this tx.
	category := ""
	var owner, device, state string
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT owner_id,device_id,state,generation FROM voice_sessions WHERE workspace_id=? AND id=?`, cmd.Principal.WorkspaceID, p.SessionID).Scan(&owner, &device, &state, &generation); errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "voice_session_not_found", "voice session was not found", nil)
	} else if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if owner != cmd.Principal.SubjectID || device != cmd.Principal.DeviceID {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "invalid_source", "voice turn belongs to another subject or device", nil)
	}
	if state != "active" || generation != p.Generation {
		return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "voice_generation_stale", "voice handoff belongs to an inactive media generation", nil)
	}
	var taskID, content, storedCategory, stewardDecision string
	var storedHash []byte
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(l.task_id,nullif(v.resumed_task_id,''),''),v.content,v.content_hash,coalesce(v.triage_category,''),v.steward_decision FROM voice_turns v LEFT JOIN voice_task_links l ON l.session_id=v.session_id AND l.generation=v.generation AND l.turn_id=v.turn_id WHERE v.workspace_id=? AND v.session_id=? AND v.generation=? AND v.turn_id=? AND v.revision=?`, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, p.Revision).Scan(&taskID, &content, &storedHash, &storedCategory, &stewardDecision); errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "voice_turn_not_found", "confirmed voice turn revision was not found", nil)
	} else if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if storedCategory != "" {
		category = storedCategory
	}
	providedHash, err := hex.DecodeString(p.ContentHash)
	if err != nil || !bytes.Equal(storedHash, providedHash) {
		return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "turn_content_conflict", "handoff content hash does not match the committed turn", nil)
	}
	if taskID != "" {
		return contracts.CommandResult{ResourceType: "task", ResourceID: taskID}, nil, nil
	}
	var handled int
	if err := tx.QueryRowContext(ctx, `SELECT handoff_handled FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, p.Revision).Scan(&handled); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if handled != 0 {
		return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID, ResourceVersion: p.Revision}, nil, nil
	}
	if result, events, done, err := s.pendingControlTx(ctx, tx, cmd, p, category, server); done {
		return result, events, err
	}
	content = strings.TrimSpace(content)
	impact := classifyHandoff(content)
	if category == "memory_write" && impact != "high" {
		// Source-only extraction currently has no worker materialization contract.
		// Never acknowledge a save that the existing pipeline cannot perform.
		at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,'memory_write','pending',?,?,?)`, uuid.NewString(), cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, recallPrefix+memorySaveUnavailable, at, at, at)
		return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID, ResourceVersion: p.Revision}, nil, err
	}
	if (category == "recall" || category == "memory_read") && impact != "high" {
		id := uuid.NewString()
		at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
		holding := s.recall.RecallHoldingText
		if strings.TrimSpace(holding) == "" {
			holding = "I will check our earlier conversations."
		}
		intent := "recall_holding"
		if category == "memory_read" {
			holding, intent = "I will check my saved notes.", "memory_read_holding"
		}
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'pending',?,?,?)`, id, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, recallSpeech(holding, nil), intent, at, at, at)
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_recall_jobs(id,workspace_id,owner_id,session_id,generation,turn_id,question,holding_id) VALUES(?,?,?,?,?,?,?,(SELECT id FROM voice_fast_replies WHERE session_id=? AND generation=? AND turn_id=?))`, uuid.NewString(), cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, p.SessionID, p.Generation, p.TurnID, content, p.SessionID, p.Generation, p.TurnID)
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
		event, _ := json.Marshal(map[string]any{"route": "fast", "category": category, "turn_id": p.TurnID, "revision": p.Revision})
		return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID, ResourceVersion: p.Revision}, []contracts.Event{{Type: "voice.turn.handoff", AggregateType: "voice_session", AggregateID: p.SessionID, AggregateVersion: p.Generation, Payload: event}}, nil
	}
	if category == "progress" && impact != "high" {
		text, err := s.progressReplyTx(ctx, tx, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, p.SessionID, p.TurnID, s.now())
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?, ?,?,?,?)`, uuid.NewString(), cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, text, "progress", "pending", cmd.AcceptedAt.UTC().Format(time.RFC3339Nano), cmd.AcceptedAt.UTC().Format(time.RFC3339Nano), cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		handoffEvent, _ := json.Marshal(map[string]any{"route": "fast", "category": category, "turn_id": p.TurnID, "revision": p.Revision})
		return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID, ResourceVersion: p.Revision}, []contracts.Event{{Type: "voice.turn.handoff", AggregateType: "voice_session", AggregateID: p.SessionID, AggregateVersion: p.Generation, Payload: handoffEvent}}, nil
	}
	ref, _ := json.Marshal(voiceTaskSourceReference{Kind: "voice_turn", SessionID: p.SessionID, Generation: p.Generation, TurnID: p.TurnID, Revision: p.Revision, ContentHash: p.ContentHash})
	admission, err := server.AdmitTaskTx(ctx, tx, control.AdmissionContext{Principal: cmd.Principal, Source: "voice", SourceRef: ref, IdempotencyKey: cmd.CommandID, ProfileID: "zoe-steward", Title: content, Goal: content, InitialMessage: content, AcceptedAt: cmd.AcceptedAt})
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	events := admission.Events
	status := "queued"
	if impact == "high" || stewardDecision == "ask" {
		status = "waiting"
		// Admission consent is scoped to this request, never a runtime action.
		question := steward.Question(content)
		interaction := "awaiting_message"
		if impact == "high" {
			interaction = "explicit_confirmation"
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tasks SET status='waiting',interaction_state=?,interaction_question=?,version=version+1,updated_at=? WHERE id=? AND workspace_id=?`, interaction, question, cmd.AcceptedAt.UTC().Format(time.RFC3339Nano), admission.Task.ID, cmd.Principal.WorkspaceID); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if impact == "high" {
			expires := cmd.AcceptedAt.Add(s.recall.ConfirmationWindow).UTC().Format(time.RFC3339Nano)
			digest := control.AdmissionConfirmationDigest(admission.Task.ID, content, question, admission.Task.Version+1, expires)
			if _, err = tx.ExecContext(ctx, `UPDATE tasks SET interaction_scope='task:admit',interaction_run_id=?,interaction_action_digest=?,interaction_expected_version=version,interaction_expires_at=? WHERE id=? AND workspace_id=?`, "admission:"+admission.Task.ID, digest, expires, admission.Task.ID, cmd.Principal.WorkspaceID); err != nil {
				return contracts.CommandResult{}, nil, err
			}
		}
		// Both admission holds reach the live call before a scoped answer.
		intent, text := "steward_ask", question
		if impact == "high" {
			intent = "explicit_confirmation"
		}
		at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
		if _, err = tx.ExecContext(ctx, `INSERT INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'pending',?,?,?) ON CONFLICT(session_id,generation,turn_id) DO NOTHING`, uuid.NewString(), cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, recallPrefix+text, intent, at, at, at); err != nil {
			return contracts.CommandResult{}, nil, err
		}
	}
	handoffEvent, _ := json.Marshal(map[string]any{"task_id": admission.Task.ID, "turn_id": p.TurnID, "revision": p.Revision, "content_hash": p.ContentHash, "profile_id": "zoe-steward", "impact": impact, "status": status, "route": "slow", "category": category})
	events = append(events, contracts.Event{Type: "voice.turn.handoff", AggregateType: "voice_session", AggregateID: p.SessionID, AggregateVersion: p.Generation, Payload: handoffEvent})
	return contracts.CommandResult{ResourceType: "task", ResourceID: admission.Task.ID, ResourceVersion: admission.Task.Version}, events, nil
}

func (s *Service) progressReplyTx(ctx context.Context, tx *sql.Tx, workspace, owner, session, turn string, now time.Time) (string, error) {
	return s.progressReplyQuery(ctx, tx, workspace, owner, session, now)
}

type progressQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Service) progressReplyQuery(ctx context.Context, db progressQueryer, workspace, owner, session string, now time.Time) (string, error) {
	cutoff := now.Add(-30 * time.Minute).UTC().Format(time.RFC3339Nano)
	ageCutoff := now.Add(-s.recall.ProgressMaxTaskAge).UTC().Format(time.RFC3339Nano)
	var status, started, created string
	err := db.QueryRowContext(ctx, `SELECT t.status,coalesce(t.started_at,''),t.created_at FROM tasks t JOIN voice_task_links l ON l.task_id=t.id AND l.workspace_id=t.workspace_id WHERE t.workspace_id=? AND t.owner_subject_id=? AND l.session_id=? AND t.created_at>=? AND t.status IN ('active','queued','waiting') ORDER BY coalesce(t.started_at,t.created_at) DESC LIMIT 1`, workspace, owner, session, ageCutoff).Scan(&status, &started, &created)
	if err == nil && (status == "active" || status == "queued" || status == "waiting") {
		stamp := started
		if stamp == "" {
			stamp = created
		}
		at, parseErr := time.Parse(time.RFC3339Nano, stamp)
		if parseErr != nil {
			return "现在没有在处理的任务。", nil
		}
		seconds := int(now.Sub(at).Seconds())
		if seconds < 0 {
			seconds = 0
		}
		return fmt.Sprintf("还在处理,已经%d秒了。", seconds), nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var recentStatus string
	err = db.QueryRowContext(ctx, `SELECT t.status FROM tasks t JOIN voice_task_links l ON l.task_id=t.id AND l.workspace_id=t.workspace_id WHERE t.workspace_id=? AND t.owner_subject_id=? AND l.session_id=? AND t.created_at>=? AND t.status IN ('awaiting_user','complete') AND coalesce(t.updated_at,t.created_at)>=? ORDER BY coalesce(t.updated_at,t.created_at) DESC LIMIT 1`, workspace, owner, session, ageCutoff, cutoff).Scan(&recentStatus)
	if err == nil {
		return "结果已经给你了,或者已经准备好了。", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return "现在没有在处理的任务。", nil
}

func (s *Service) progressReply(ctx context.Context, workspace, owner, session string) string {
	text, err := s.progressReplyQuery(ctx, s.db, workspace, owner, session, s.now())
	if err == nil {
		return text
	}
	return "现在没有在处理的任务。"
}

func classifyHandoff(content string) string {
	lower := strings.ToLower(content)
	for _, marker := range []string{"delete", "send", "spend", "deploy", "permission", "删除", "发送", "付款", "部署", "权限"} {
		if strings.Contains(lower, marker) {
			return "high"
		}
	}
	for _, marker := range []string{"research", "summarize", "summary", "查找", "研究", "总结", "调研"} {
		if strings.Contains(lower, marker) {
			return "low"
		}
	}
	return "low"
}

func (s *Service) commandCommitTurn(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if !control.HasScope(cmd.Principal, "client:command") && !control.HasScope(cmd.Principal, "voice:turn:commit") {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "forbidden", "voice turn commit scope is required", nil)
	}
	var payload commitTurnPayload
	if err := decodePayload(req.Payload, &payload); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if !payload.UserConfirmed || payload.SessionID == "" || payload.Generation < 1 || strings.TrimSpace(payload.TurnID) == "" || payload.Revision < 1 || strings.TrimSpace(payload.Content) == "" || len(payload.Content) > 20_000 {
		return contracts.CommandResult{}, nil, apiError(http.StatusBadRequest, "invalid_request", "an explicitly user-confirmed bounded turn with stable identity and revision is required", nil)
	}
	var state string
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT state,generation FROM voice_sessions WHERE id=? AND workspace_id=? AND owner_id=?`, payload.SessionID, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID).Scan(&state, &generation); errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "not_found", "voice session not found", nil)
	} else if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if state != "active" || generation != payload.Generation {
		return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "voice_generation_stale", "turn belongs to an inactive media generation", nil)
	}
	content := strings.TrimSpace(payload.Content)
	hash := sha256.Sum256([]byte(content))
	var existingID string
	var existingHash []byte
	err := tx.QueryRowContext(ctx, `SELECT id,content_hash FROM voice_turns WHERE session_id=? AND generation=? AND turn_id=? AND revision=?`, payload.SessionID, payload.Generation, payload.TurnID, payload.Revision).Scan(&existingID, &existingHash)
	if err == nil {
		if !bytes.Equal(hash[:], existingHash) {
			return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "turn_revision_conflict", "turn revision was already committed with different text", nil)
		}
		return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: existingID, ResourceVersion: payload.Revision}, nil, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, err
	}
	id := uuid.NewString()
	_, err = tx.ExecContext(ctx, `INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,provider_message_id,content,content_hash,confirmed_at,command_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, cmd.Principal.WorkspaceID, payload.SessionID, payload.Generation, payload.TurnID, payload.Revision, payload.ProviderMessage, content, hash[:], cmd.AcceptedAt.UTC().Format(time.RFC3339Nano), cmd.CommandID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	go s.classifyCommittedTurn(id)
	turnEvent, _ := json.Marshal(map[string]any{"turn_id": payload.TurnID, "revision": payload.Revision, "voice_session_id": payload.SessionID, "generation": payload.Generation, "content_hash": hex.EncodeToString(hash[:]), "status": "confirmed_candidate"})
	event := contracts.Event{Type: "voice.turn.committed", AggregateType: "voice_turn", AggregateID: id, AggregateVersion: payload.Revision, Payload: turnEvent}
	return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: id, ResourceVersion: payload.Revision}, []contracts.Event{event}, nil
}

func (s *Service) classifyCommittedTurn(id string) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), s.classificationTimeout())
	defer cancel()
	var content, session, turn string
	var generation, revision int64
	if err := s.db.QueryRowContext(ctx, `SELECT content,session_id,generation,turn_id,revision FROM voice_turns WHERE id=? AND triage_category IS NULL`, id).Scan(&content, &session, &generation, &turn, &revision); err != nil {
		return
	}
	var workspace, owner string
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_id,owner_id FROM voice_sessions WHERE id=?`, session).Scan(&workspace, &owner); err != nil {
		return
	}
	pending, pendingErr := s.pendingHeld(ctx, s.db, workspace, owner, session, generation)
	if pendingErr != nil {
		return
	}
	// Persist the snapshot before model I/O so timeout cannot erase its binding.
	if pending.id == "" {
		pending.version = -1
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE voice_turns SET pending_task_id=?,pending_task_version=?,pending_question=? WHERE id=? AND triage_category IS NULL AND pending_task_version=0`, pending.id, pending.version, pending.question, id); err != nil {
		return
	}
	if err := s.db.QueryRowContext(ctx, `SELECT pending_task_id,pending_task_version,pending_question FROM voice_turns WHERE id=?`, id).Scan(&pending.id, &pending.version, &pending.question); err != nil {
		return
	}
	category := "task"
	var err error
	attempts := 0
	reason := "none"
	triageStarted := time.Now()
	if s.classifier != nil {
		// Both attempts share the remaining classification budget. Progress
		// delivery recomputes elapsed time at send time, even after slow triage.
		for attempts < 2 && ctx.Err() == nil {
			attempts++
			if classifier, ok := s.classifier.(interface {
				ClassifyPending(context.Context, string, string) (string, error)
			}); ok && pending.id != "" {
				category, err = classifier.ClassifyPending(ctx, content, pending.question)
			} else {
				category, err = s.classifier.Classify(ctx, content)
			}
			if pending.id == "" && (category == "confirm" || category == "decline") {
				category = "task"
			}
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			if err == nil && validCategory(category) {
				break
			}
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	}
	if err != nil || !validCategory(category) {
		category = "unclassified"
		reason = "triage_error"
		var timeout interface{ Timeout() bool }
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
			reason = "triage_timeout"
		}
	}
	triageMS := time.Since(triageStarted).Milliseconds()
	stewardMS := int64(0)
	decision := steward.Dispatch
	if classifyHandoff(content) == "high" {
		decision = steward.Ask
	} else if s.steward != nil && category == "task" {
		stewardStarted := time.Now()
		answer, callErr := s.steward.Decide(ctx, content)
		if ctx.Err() != nil {
			callErr = ctx.Err()
		}
		decision = steward.Resolve(answer, callErr, false)
		stewardMS = time.Since(stewardStarted).Milliseconds()
		if callErr != nil {
			reason = "steward_error"
			var timeout interface{ Timeout() bool }
			if errors.Is(callErr, context.DeadlineExceeded) || (errors.As(callErr, &timeout) && timeout.Timeout()) {
				reason = "steward_timeout"
			}
		}
	}
	// Persist using a fresh short context even if model I/O exhausted its deadline.
	writeCtx, writeCancel := context.WithTimeout(context.Background(), time.Second)
	defer writeCancel()
	res, writeErr := s.db.ExecContext(writeCtx, `UPDATE voice_turns SET triage_category=?,steward_decision=? WHERE id=? AND triage_category IS NULL`, category, string(decision), id)
	if writeErr == nil {
		if n, _ := res.RowsAffected(); n == 1 {
			s.cacheTriage(triageKey(session, generation, turn, revision), category)
		}
	}
	log.Printf("voice_classification id=%s triage_ms=%d steward_ms=%d total_ms=%d budget_hit=%t storage_error=%t reason=%s attempts=%d", id, triageMS, stewardMS, time.Since(started).Milliseconds(), ctx.Err() != nil, writeErr != nil, reason, attempts)
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
	if state != "active" || generation != ref.Generation {
		return nil, apiError(http.StatusConflict, "voice_generation_stale", "voice turn is not from the active media generation", nil)
	}
	var content string
	var contentHash []byte
	err = tx.QueryRowContext(ctx, `SELECT content,content_hash FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, principal.WorkspaceID, ref.SessionID, ref.Generation, ref.TurnID, ref.Revision).Scan(&content, &contentHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apiError(http.StatusNotFound, "voice_turn_not_found", "confirmed voice turn revision was not found", nil)
	}
	if err != nil {
		return nil, err
	}
	var existingTask string
	err = tx.QueryRowContext(ctx, `SELECT task_id FROM voice_task_links WHERE session_id=? AND generation=? AND turn_id=?`, ref.SessionID, ref.Generation, ref.TurnID).Scan(&existingTask)
	if err == nil {
		return nil, apiError(http.StatusConflict, "voice_turn_already_dispatched", "this stable voice turn is already bound to a task", map[string]string{"task_id": existingTask})
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	content = strings.TrimSpace(content)
	if content == "" || len(content) > 20_000 {
		return nil, apiError(http.StatusConflict, "invalid_source", "confirmed voice turn content is empty or exceeds task limits", nil)
	}
	title := strings.TrimSpace(strings.SplitN(content, "\n", 2)[0])
	if runes := []rune(title); len(runes) > 200 {
		title = string(runes[:200])
	}
	request.Title = title
	request.Goal = content
	request.InitialMessage = content
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
	var turnCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, cmd.Principal.WorkspaceID, payload.SessionID, payload.Generation, payload.TurnID, payload.TurnRevision).Scan(&turnCount); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if turnCount != 1 {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "voice_turn_not_found", "confirmed voice turn revision not found", nil)
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
	capabilities := AccountMediaCapabilities{}
	if s.accountMedia != nil {
		capabilities = s.accountMedia.Capabilities()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"blocked": blocked != 0, "reason_code": reason, "updated_at": updatedAt,
		"account_lease_identity":       capabilities.AccountLeaseIdentity,
		"account_scoped_termination":   capabilities.AccountScopedTermination,
		"trusted_termination_evidence": capabilities.TrustedTerminationEvidence,
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
	response, gatewayRequestID, err := s.callGateway(withGatewayRequestID(r.Context(), r.Header.Get("X-Zoe-Client-Request-ID")), http.MethodGet, "/v1/voice/config", nil)
	if err != nil {
		control.WriteError(w, apiError(http.StatusServiceUnavailable, "voice_unavailable", "Space voice configuration is unavailable", map[string]any{"request_id": gatewayRequestID}))
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		control.WriteError(w, apiError(http.StatusServiceUnavailable, "voice_unavailable", "Space voice configuration is unavailable", map[string]any{"request_id": gatewayRequestID, "status": response.StatusCode}))
		return
	}
	var config spacePublicConfig
	dec := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&config); err != nil || config.Version == "" || len(config.WebRTC) == 0 {
		control.WriteError(w, apiError(http.StatusBadGateway, "voice_config_invalid", "Space voice configuration response is invalid", map[string]any{"request_id": gatewayRequestID}))
		return
	}
	writeJSON(w, http.StatusOK, config)
}

type spacePublicConfig struct {
	Version    string          `json:"version"`
	Defaults   json.RawMessage `json:"defaults"`
	Voices     json.RawMessage `json:"voices"`
	VoiceModes json.RawMessage `json:"voice_modes"`
	Languages  json.RawMessage `json:"languages"`
	WebRTC     json.RawMessage `json:"webrtc"`
}

type reconcilePoolPayload struct {
	SessionID                     string `json:"session_id"`
	Generation                    int64  `json:"generation"`
	Reason                        string `json:"reason"`
	ConfirmedOutOfBandTermination bool   `json:"confirmed_out_of_band_termination"`
}

func (s *Service) commandReconcilePool(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if !control.HasScope(cmd.Principal, "voice:pool:reconcile") && !control.HasScope(cmd.Principal, "client:admin") {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "forbidden", "voice pool reconciliation requires an operator scope", nil)
	}
	var payload reconcilePoolPayload
	if err := decodePayload(req.Payload, &payload); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	payload.Reason = strings.TrimSpace(payload.Reason)
	if payload.SessionID == "" || payload.Generation < 1 || !payload.ConfirmedOutOfBandTermination || len(payload.Reason) < 12 || len(payload.Reason) > 500 {
		return contracts.CommandResult{}, nil, apiError(http.StatusBadRequest, "invalid_request", "source session, generation, explicit out-of-band termination confirmation, and a 12-500 character reason are required", nil)
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM voice_sessions WHERE state IN ('allocating','active','ending')`).Scan(&active); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if active != 0 {
		return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "voice_sessions_active", "all known media sessions must be locally closed before pool reconciliation", map[string]any{"active_sessions": active})
	}
	var blocked int
	var sourceSessionID string
	if err := tx.QueryRowContext(ctx, `SELECT blocked,source_session_id FROM voice_pool_state WHERE id=1`).Scan(&blocked, &sourceSessionID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if blocked == 0 {
		return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "voice_pool_not_blocked", "voice account pool is not awaiting reconciliation", nil)
	}
	if sourceSessionID != payload.SessionID {
		return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "voice_reconciliation_stale", "reconciliation source does not match the quarantined media session", nil)
	}
	var generation int64
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT generation,state FROM voice_sessions WHERE id=?`, payload.SessionID).Scan(&generation, &state); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if generation != payload.Generation || state != "quarantined" && state != "ended" {
		return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "voice_reconciliation_stale", "session generation is not eligible for out-of-band reconciliation", nil)
	}
	at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
	id := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `INSERT INTO voice_pool_reconciliations(id,actor_id,source_session_id,generation,reason,method,created_at) VALUES(?,?,?, ?,?,'operator_attestation',?)`, id, cmd.Principal.SubjectID, payload.SessionID, payload.Generation, payload.Reason, at); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE voice_pool_state SET blocked=0,reason_code='',source_session_id='',updated_at=?,updated_by=? WHERE id=1`, at, cmd.Principal.SubjectID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	eventPayload, _ := json.Marshal(map[string]any{"reconciliation_id": id, "source_session_id": payload.SessionID, "generation": payload.Generation, "method": "operator_attestation", "out_of_band_termination_attested": true, "reason": payload.Reason, "account_identity_available": false})
	event := contracts.Event{Type: "voice.pool.reconciled", AggregateType: "voice_pool", AggregateID: "default", AggregateVersion: 1, Payload: eventPayload}
	return contracts.CommandResult{ResourceType: "voice_pool_reconciliation", ResourceID: id, ResourceVersion: 1}, []contracts.Event{event}, nil
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
