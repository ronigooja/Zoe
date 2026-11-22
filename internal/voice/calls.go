package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/ronigooja/Zoe/internal/limits"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/logging"
)

const voiceRunnerScope = "voice:turn:ingest"

func (s *Service) registerCallRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /runner/v1/voice/calls", s.handleCallStart)
	mux.HandleFunc("POST /runner/v1/voice/calls/{session_id}/established", s.handleCallEstablished)
	mux.HandleFunc("POST /runner/v1/voice/calls/{session_id}/ended", s.handleCallEnded)
}

type callStartResponse struct {
	SessionID          string          `json:"voice_session_id"`
	Generation         int64           `json:"generation"`
	GatewayRequestID   string          `json:"gateway_request_id"`
	Instructions       string          `json:"instructions"`
	OpeningContext     *openingContext `json:"opening_context"`
	OpeningContextHash string          `json:"opening_context_hash"`
}

func (s *Service) handleCallStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runner, ok := callRunner(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("X-Zoe-Instance-Key")
	r.Header.Del("X-Zoe-Instance-Key")
	p, err := s.uiEventServer.AuthenticateForwardedInstanceKey(ctx, key)
	if err != nil {
		control.WriteError(w, err)
		return
	}
	if !control.HasRunnerScope(runner, voiceRunnerScope) || (!control.HasScope(p, "voice:open") && !control.HasScope(p, "client:command")) {
		control.WriteError(w, callForbidden())
		return
	}
	var req createRequest
	// Go's JSON decoder replaces malformed UTF-8. Reject it before decoding
	// the payload whose decoded fields form the durable idempotency key.
	var raw json.RawMessage
	if err := decodeRequest(w, r, 6*s.limits.Int64("voice_sdp_max_bytes")+limits.CallStartJSONOverhead, &raw); err != nil {
		control.WriteError(w, err)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if !utf8.Valid(raw) || dec.Decode(&req) != nil || hasNullDialField(raw) || strings.TrimSpace(req.RequestID) == "" || strings.TrimSpace(req.OfferSDP) == "" || int64(len(req.OfferSDP)) > s.limits.Int64("voice_sdp_max_bytes") {
		control.WriteError(w, apiError(http.StatusBadRequest, "invalid_request", "request_id and a bounded offer_sdp are required", nil))
		return
	}
	// Do not allow a caller to smuggle the credential into an echoed or
	// persisted field of the dial payload.
	for _, field := range []string{req.RequestID, req.OfferSDP, req.Voice, req.VoiceMode, req.LanguageCode} {
		if strings.Contains(field, key) {
			control.WriteError(w, apiError(http.StatusBadRequest, "invalid_request", "invalid dial field", nil))
			return
		}
	}
	result, replay, err := s.startCall(ctx, p, runner, req)
	if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}

func hasNullDialField(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return true
	}
	for _, value := range fields {
		if bytes.Equal(value, []byte("null")) {
			return true
		}
	}
	return false
}

func (s *Service) startCall(ctx context.Context, p contracts.Principal, runner contracts.RunnerPrincipal, req createRequest) (callStartResponse, bool, error) {
	var result callStartResponse
	payload, err := json.Marshal(req)
	if err != nil {
		return result, false, err
	}
	hash := sha256.Sum256(payload)
	// Validate advertised settings outside the write transaction, but do not
	// depend on the gateway's availability or current roster to replay.
	var preflightDone bool
	var preflightErr error
	var registerText string
	for {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return result, false, err
		}
		if err := control.RequireForwardedInstanceKeyTx(ctx, tx, p); err != nil {
			tx.Rollback()
			return result, false, err
		}
		var existingHash, saved []byte
		var runnerID string
		err = tx.QueryRowContext(ctx, `SELECT request_hash,call_start_response,lifecycle_runner_id FROM voice_sessions WHERE workspace_id=? AND owner_id=? AND request_id=?`, p.WorkspaceID, p.SubjectID, req.RequestID).Scan(&existingHash, &saved, &runnerID)
		if !errors.Is(err, sql.ErrNoRows) {
			tx.Rollback()
			switch {
			case err != nil:
				return result, false, err
			case runnerID != runner.RunnerID:
				return result, false, callForbidden()
			case !bytes.Equal(existingHash, hash[:]):
				return result, false, callReplayConflict()
			}
			err := json.Unmarshal(saved, &result)
			return result, true, err
		}
		if err := callAdmission(ctx, tx, p); err != nil {
			tx.Rollback()
			return result, false, err
		}
		if !preflightDone {
			tx.Rollback()
			preflightDone = true
			preflightErr = s.validateDialSettings(ctx, req)
			if preflightErr == nil {
				registerText = s.openingRegisterText(ctx, p)
			}
			continue
		}
		if preflightErr != nil {
			tx.Rollback()
			return result, false, preflightErr
		}
		defer tx.Rollback()
		result, err = s.allocateCall(ctx, tx, p, runner.RunnerID, req.RequestID, hash[:], registerText)
		return result, false, err
	}
}

func callAdmission(ctx context.Context, tx *sql.Tx, p contracts.Principal) error {
	var busy, blocked bool
	var reason string
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM voice_sessions WHERE workspace_id=? AND owner_id=? AND state IN ('allocating','active','ending')),blocked,reason_code FROM voice_pool_state WHERE id=1`, p.WorkspaceID, p.SubjectID).Scan(&busy, &blocked, &reason); err != nil {
		return err
	}
	if blocked && reason != "active_session" && reason != "negotiation_in_progress" {
		return apiError(http.StatusServiceUnavailable, "voice_pool_quarantined", "voice pool is quarantined", nil)
	}
	if busy || blocked {
		return apiError(http.StatusConflict, "voice_lease_busy", "another voice call is active or unresolved", nil)
	}
	return nil
}

func (s *Service) allocateCall(ctx context.Context, tx *sql.Tx, p contracts.Principal, runner, request string, hash []byte, registerText string) (callStartResponse, error) {
	result := callStartResponse{SessionID: uuid.NewString(), Generation: 1, GatewayRequestID: uuid.NewString(), Instructions: s.recall.InstructionText}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,gateway_request_id,lifecycle_runner_id,generation,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,1,'allocating',?,?)`, result.SessionID, p.WorkspaceID, p.SubjectID, p.DeviceID, request, hash, result.GatewayRequestID, runner, now, now); err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE voice_pool_state SET blocked=1,reason_code='negotiation_in_progress',source_session_id=?,updated_at=?,updated_by='system' WHERE id=1`, result.SessionID, now); err != nil {
		return result, err
	}
	opening, err := s.buildAVASOpeningContext(ctx, tx, p, result.SessionID, registerText, now)
	if err != nil {
		return result, err
	}
	result.OpeningContext, result.OpeningContextHash = opening, openingContextHash(opening)
	saved, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE voice_sessions SET call_start_response=? WHERE id=?`, saved, result.SessionID); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func (s *Service) validateDialSettings(ctx context.Context, req createRequest) error {
	unavailable := apiError(http.StatusServiceUnavailable, "voice_unavailable", "voice service is unavailable", nil)
	if !s.gatewayReady() || s.dialOrigin == "" {
		return unavailable
	}
	if req.Voice == "" && req.VoiceMode == "" && req.LanguageCode == "" {
		return nil
	}
	config, err := s.readVoiceConfig(ctx)
	if err != nil {
		return unavailable
	}
	for _, choice := range []struct {
		id   string
		list json.RawMessage
	}{{req.Voice, config.Voices}, {req.VoiceMode, config.VoiceModes}, {req.LanguageCode, config.Languages}} {
		if choice.id == "" {
			continue
		}
		var options []struct{ ID string }
		if json.Unmarshal(choice.list, &options) != nil || options == nil {
			return unavailable
		}
		found := false
		for _, option := range options {
			found = found || option.ID == choice.id
		}
		if !found {
			return apiError(http.StatusBadRequest, "invalid_request", "unsupported voice settings", nil)
		}
	}
	return nil
}

type callBinding struct {
	Generation             int64  `json:"generation"`
	GatewaySessionID       string `json:"gateway_session_id"`
	MediaGeneration        int64  `json:"media_generation"`
	AccountLeaseID         string `json:"account_lease_id"`
	AccountLeaseGeneration int64  `json:"account_lease_generation"`
}

func (b callBinding) valid() bool {
	return b.Generation > 0 && b.MediaGeneration > 0 && b.AccountLeaseGeneration > 0 && validBindingID(b.GatewaySessionID) && validBindingID(b.AccountLeaseID)
}

func validBindingID(id string) bool {
	return id != "" && strings.TrimSpace(id) == id && id != "." && id != ".."
}

type callEstablishedRequest struct {
	callBinding
	OpeningContext *openingContextResult `json:"opening_context,omitempty"`
}

type lifecycleCall struct {
	callBinding
	principal                                               contracts.Principal
	runner, state, establishedHash, poolSource              string
	endReason, closedAt, effectiveClosedAt, endEvidenceHash string
	accountHash, opening                                    []byte
}

func loadLifecycleCall(ctx context.Context, tx *sql.Tx, id string) (lifecycleCall, error) {
	var c lifecycleCall
	err := tx.QueryRowContext(ctx, `SELECT s.generation,s.gateway_session_id,s.gateway_media_generation,s.account_lease_id,s.account_lease_generation,s.workspace_id,s.owner_id,s.device_id,s.lifecycle_runner_id,s.state,s.call_established_hash,p.source_session_id,s.call_ended_reason,s.call_closed_at,s.call_effective_closed_at,s.call_end_evidence_hash,s.account_id_hash,s.opening_context_manifest FROM voice_sessions s CROSS JOIN voice_pool_state p WHERE s.id=? AND p.id=1`, id).Scan(&c.Generation, &c.GatewaySessionID, &c.MediaGeneration, &c.AccountLeaseID, &c.AccountLeaseGeneration, &c.principal.WorkspaceID, &c.principal.SubjectID, &c.principal.DeviceID, &c.runner, &c.state, &c.establishedHash, &c.poolSource, &c.endReason, &c.closedAt, &c.effectiveClosedAt, &c.endEvidenceHash, &c.accountHash, &c.opening)
	if errors.Is(err, sql.ErrNoRows) {
		logging.Errorf("voice lifecycle report references an unknown session")
		return c, apiError(http.StatusNotFound, "not_found", "voice session not found", nil)
	}
	return c, err
}

func (c lifecycleCall) authorize(runner contracts.RunnerPrincipal) error {
	if runner.RunnerID != c.runner || !control.HasRunnerScope(runner, voiceRunnerScope) {
		return callForbidden()
	}
	return nil
}

func callRunner(w http.ResponseWriter, r *http.Request) (contracts.RunnerPrincipal, bool) {
	p, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "runner authentication required", nil))
		return p, false
	}
	for _, scope := range p.Scopes {
		if scope == voiceRunnerScope || scope == "*" {
			return p, true
		}
	}
	control.WriteError(w, callForbidden())
	return p, false
}

func callForbidden() error {
	return apiError(http.StatusForbidden, "forbidden", "credential cannot access this voice call", nil)
}

func callStale() error {
	return apiError(http.StatusConflict, "voice_generation_stale", "voice call binding is stale", nil)
}

func callReplayConflict() error {
	return apiError(http.StatusConflict, "idempotency_conflict", "voice call replay changed the committed input", nil)
}

func (s *Service) handleCallEstablished(w http.ResponseWriter, r *http.Request) {
	p, ok := callRunner(w, r)
	if !ok {
		return
	}
	var req callEstablishedRequest
	if err := decodeRequest(w, r, s.limits.Int64("voice_lifecycle_max_bytes"), &req); err != nil || !req.callBinding.valid() {
		control.WriteError(w, apiError(http.StatusBadRequest, "invalid_request", "invalid establishment report", nil))
		return
	}
	if err := s.establishCall(r.Context(), p, r.PathValue("session_id"), req); err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Service) establishCall(ctx context.Context, p contracts.RunnerPrincipal, id string, req callEstablishedRequest) error {
	serialized, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hash := resultHash(string(serialized))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := loadLifecycleCall(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := c.authorize(p); err != nil {
		return err
	}
	if req.Generation != c.Generation || (c.GatewaySessionID != "" && req.callBinding != c.callBinding) {
		return callStale()
	}
	if c.establishedHash != "" {
		if hash == c.establishedHash {
			return nil
		}
		return callReplayConflict()
	}
	if c.state != "allocating" || c.poolSource != id || c.GatewaySessionID != "" {
		return callStale()
	}
	var alreadyBound bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM voice_sessions WHERE id<>? AND gateway_session_id=? AND gateway_media_generation=?)`, id, req.GatewaySessionID, req.MediaGeneration).Scan(&alreadyBound); err != nil {
		return err
	}
	if alreadyBound {
		return callStale()
	}
	var opening openingContext
	if err := json.Unmarshal(c.opening, &opening); err != nil {
		return err
	}
	delivery := "unverified"
	if strictOpeningReceipt(&opening, req.OpeningContext) {
		delivery = "delivered"
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE voice_sessions SET state='active',gateway_session_id=?,gateway_media_generation=?,account_lease_id=?,account_lease_generation=?,opening_context_delivery=?,call_established_hash=?,updated_at=? WHERE id=?`, req.GatewaySessionID, req.MediaGeneration, req.AccountLeaseID, req.AccountLeaseGeneration, delivery, hash, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE voice_pool_state SET reason_code='active_session',updated_at=?,updated_by='system' WHERE id=1 AND source_session_id=?`, now, id); err != nil {
		return err
	}
	if err := s.acknowledgeOpeningResults(ctx, tx, c.principal, id, &opening, req.OpeningContext); err != nil {
		return err
	}
	return tx.Commit()
}
