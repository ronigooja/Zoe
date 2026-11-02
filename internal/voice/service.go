package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
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
	"github.com/ronigooja/Zoe/internal/storage"
)

type Config struct {
	GatewayURL            string
	GatewayAPIKey         string
	HTTPClient            *http.Client
	MaxOfferBytes         int64
	AccountMediaLifecycle AccountMediaLifecycle
	HeartbeatTimeout      time.Duration
	Now                   func() time.Time
	ContextMaxChars       int
}

type Service struct {
	db               *sql.DB
	base             *url.URL
	key              string
	client           *http.Client
	maxSDP           int64
	accountMedia     AccountMediaLifecycle
	heartbeatTimeout time.Duration
	now              func() time.Time
	contextMaxChars  int
	reaperStop       chan struct{}
	reaperDone       chan struct{}
}

type gatewayRequestIDContextKey struct{}

func withGatewayRequestID(ctx context.Context, requestID string) context.Context {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return ctx
	}
	return context.WithValue(ctx, gatewayRequestIDContextKey{}, requestID)
}

type createRequest struct {
	RequestID    string `json:"request_id"`
	OfferSDP     string `json:"offer_sdp"`
	Voice        string `json:"voice,omitempty"`
	VoiceMode    string `json:"voice_mode,omitempty"`
	LanguageCode string `json:"language_code,omitempty"`
}

type sessionResponse struct {
	SessionID               string `json:"voice_session_id"`
	Generation              int64  `json:"generation"`
	State                   string `json:"state"`
	AnswerSDP               string `json:"answer_sdp,omitempty"`
	AccountLeaseID          string `json:"account_lease_id,omitempty"`
	AccountLeaseGeneration  int64  `json:"account_lease_generation,omitempty"`
	AccountLeaseAccountHash []byte `json:"-"`
	Conversation            string `json:"conversation,omitempty"`
	ConversationReason      string `json:"conversation_reason,omitempty"`
	ConversationConfirmed   *bool  `json:"conversation_confirmed"`
	ContextChars            int    `json:"context_chars"`
	RotateRequired          bool   `json:"rotate_required"`
}

type gatewaySessionRequest struct {
	OfferSDP       string `json:"offer_sdp"`
	VoiceSessionID string `json:"voice_session_id,omitempty"`
	Voice          string `json:"voice,omitempty"`
	VoiceMode      string `json:"voice_mode,omitempty"`
	LanguageCode   string `json:"language_code,omitempty"`
}

type gatewaySessionResponse struct {
	AnswerSDP              string `json:"answer_sdp"`
	VoiceSessionID         string `json:"voice_session_id"`
	VoiceSessionGeneration int64  `json:"voice_session_generation,omitempty"`
	AccountLeaseID         string `json:"account_lease_id,omitempty"`
	AccountLeaseGeneration int64  `json:"account_lease_generation,omitempty"`
	AccountID              string `json:"account_id,omitempty"`
	Conversation           string `json:"conversation,omitempty"`
	ConversationReason     string `json:"conversation_reason,omitempty"`
	ConversationConfirmed  *bool  `json:"conversation_confirmed"`
	ContextChars           int    `json:"context_chars"`
}

func Register(server *control.Server, store *storage.Store, cfg Config) error {
	if server == nil || store == nil || store.DB() == nil {
		return errors.New("voice registration requires control server and storage")
	}
	if err := RegisterMigrations(store); err != nil {
		return err
	}
	svc := newService(store.DB(), cfg)
	server.RegisterShutdownHook(svc.Stop)
	svc.Start()
	if err := server.RegisterCommand("voice.turn.commit", svc.commandCommitTurn); err != nil {
		return err
	}
	if err := server.RegisterCommand("voice.task.link", svc.commandLinkTask); err != nil {
		return err
	}
	if err := server.RegisterTaskSourceHandler("voice", svc.admitVoiceTask); err != nil {
		return err
	}
	if err := server.RegisterCommand("voice.pool.reconcile", svc.commandReconcilePool); err != nil {
		return err
	}
	return server.RegisterAuthenticatedRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /v1/voice/pool/status", svc.handlePoolStatus)
		mux.HandleFunc("GET /v1/voice/config", svc.handleVoiceConfig)
		mux.HandleFunc("POST /v1/voice/sessions", svc.handleCreate)
		mux.HandleFunc("GET /v1/voice/sessions/{session_id}", svc.handleReadSession)
		mux.HandleFunc("POST /v1/voice/sessions/{session_id}/connected", svc.handleConnected)
		mux.HandleFunc("POST /v1/voice/sessions/{session_id}/heartbeat", svc.handleHeartbeat)
		mux.HandleFunc("POST /v1/voice/sessions/{session_id}/hangup", svc.handleHangup)
		mux.HandleFunc("GET /v1/voice/sessions/{session_id}/turns", svc.handleTurns)
		mux.HandleFunc("GET /v1/voice/sessions/{session_id}/receipts", svc.handleReceipts)
	})
}

func newService(db *sql.DB, cfg Config) *Service {
	var base *url.URL
	if raw := strings.TrimSpace(cfg.GatewayURL); raw != "" {
		base, _ = url.Parse(raw)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 25 * time.Second}
	}
	if cfg.MaxOfferBytes <= 0 {
		cfg.MaxOfferBytes = 1 << 20
	}
	timeout := cfg.HeartbeatTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	if timeout < 30*time.Second {
		timeout = 30 * time.Second
	}
	if timeout > 600*time.Second {
		timeout = 600 * time.Second
	}
	contextMax := cfg.ContextMaxChars
	if contextMax <= 0 {
		contextMax = 60000
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, base: base, key: cfg.GatewayAPIKey, client: client, maxSDP: cfg.MaxOfferBytes, accountMedia: cfg.AccountMediaLifecycle, heartbeatTimeout: timeout, now: now, contextMaxChars: contextMax, reaperStop: make(chan struct{}), reaperDone: make(chan struct{})}
}

func (s *Service) Start() { go s.reaper() }

func (s *Service) Stop() {
	select {
	case <-s.reaperStop:
	default:
		close(s.reaperStop)
	}
	<-s.reaperDone
}

func (s *Service) reaper() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	defer close(s.reaperDone)
	for {
		select {
		case <-s.reaperStop:
			return
		case <-ticker.C:
			s.reapStale(context.Background())
		}
	}
}

func (s *Service) reapStale(ctx context.Context) {
	cutoff := s.now().UTC().Add(-s.heartbeatTimeout)
	cutoffValue := cutoff.Format("2006-01-02T15:04:05.000000000Z")
	// julianday parses RFC3339 fractional seconds numerically; a text comparison
	// is incorrect because RFC3339Nano trims trailing fractional zeroes.
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM voice_sessions WHERE state='active' AND julianday(COALESCE(heartbeat_at,created_at,updated_at)) < julianday(?)`, cutoffValue)
	if err != nil {
		log.Printf("voice reaper query failed: %v", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		log.Printf("voice reaper rows close failed: %v", err)
		return
	}
	for _, id := range ids {
		claim, err := s.db.ExecContext(ctx, `UPDATE voice_sessions SET state='ending',updated_at=? WHERE id=? AND state='active' AND julianday(COALESCE(heartbeat_at,created_at,updated_at)) < julianday(?)`, s.now().UTC().Format(time.RFC3339Nano), id, cutoffValue)
		if err != nil {
			log.Printf("voice reaper claim failed: %v", err)
			continue
		}
		changed, _ := claim.RowsAffected()
		if changed != 1 {
			continue
		}
		var gatewayID, accountLeaseID, workspaceID, ownerID string
		var generation, accountLeaseGeneration, gatewayMediaGeneration int64
		var gatewayUnbound, gatewayUnbindUnknown int
		if err := s.db.QueryRowContext(ctx, `SELECT workspace_id,owner_id,gateway_session_id,generation,account_lease_id,account_lease_generation,gateway_media_generation,gateway_unbound,gateway_unbind_unknown FROM voice_sessions WHERE id=? AND state='ending'`, id).Scan(&workspaceID, &ownerID, &gatewayID, &generation, &accountLeaseID, &accountLeaseGeneration, &gatewayMediaGeneration, &gatewayUnbound, &gatewayUnbindUnknown); err != nil {
			log.Printf("voice reaper load failed: %v", err)
			continue
		}
		if gatewayID == "" {
			if err := s.markSession(ctx, id, "quarantined", "gateway_session_unbound", nil, ""); err != nil {
				log.Printf("voice reaper quarantine failed for session %s: %v", id, err)
			}
			continue
		}
		// Missing heartbeat implies the client stopped its local media, so media_closed is trusted for this system reclaim.
		_, _, _, err = s.releaseSession(ctx, contracts.Principal{WorkspaceID: workspaceID, SubjectID: ownerID}, id, generation, gatewayID, accountLeaseID, accountLeaseGeneration, gatewayMediaGeneration, gatewayUnbound, gatewayUnbindUnknown, true)
		if err != nil {
			log.Printf("voice reaper release failed for session %s: %v", id, err)
		}
	}
}

func (s *Service) gatewayReady() bool {
	return s.base != nil && s.base.Host != "" && s.base.User == nil && s.base.RawQuery == "" && s.base.Fragment == "" && s.base.Scheme == "https" && strings.TrimSpace(s.key) != ""
}

func (s *Service) callGateway(ctx context.Context, method, endpoint string, body []byte) (*http.Response, string, error) {
	if !s.gatewayReady() {
		return nil, "", errors.New("Space voice gateway is not configured")
	}
	u := *s.base
	u.Path = strings.TrimRight(s.base.Path, "/") + endpoint
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, "", err
	}
	requestID, _ := ctx.Value(gatewayRequestIDContextKey{}).(string)
	if strings.TrimSpace(requestID) == "" {
		requestID = uuid.NewString()
	}
	req.Header.Set("X-Request-ID", requestID)
	req.Header.Set("Authorization", "Bearer "+s.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := s.client.Do(req)
	return response, requestID, err
}

func (s *Service) create(ctx context.Context, p contracts.Principal, req createRequest) (sessionResponse, error) {
	if !s.gatewayReady() {
		return sessionResponse{}, apiError(http.StatusServiceUnavailable, "voice_unavailable", "Space voice gateway is not configured", nil)
	}
	if p.WorkspaceID == "" || p.SubjectID == "" || p.DeviceID == "" {
		return sessionResponse{}, apiError(http.StatusUnauthorized, "unauthenticated", "authenticated workspace, subject, and device are required", nil)
	}
	if strings.TrimSpace(req.RequestID) == "" || strings.TrimSpace(req.OfferSDP) == "" || int64(len(req.OfferSDP)) > s.maxSDP {
		return sessionResponse{}, apiError(http.StatusBadRequest, "invalid_request", "request_id and a bounded offer_sdp are required", nil)
	}
	if req.VoiceMode != "" && req.VoiceMode != "primary_assistant" && req.VoiceMode != "wingman" {
		return sessionResponse{}, apiError(http.StatusBadRequest, "invalid_request", "unsupported voice_mode", nil)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return sessionResponse{}, err
	}
	hash := sha256.Sum256(body)
	var id, state, gatewayID, accountLeaseID string
	var generation int64
	var accountLeaseGeneration int64
	var gatewayMediaGeneration int64
	var accountLeaseAccountHash []byte
	var answer []byte
	var existingHash []byte
	err = s.db.QueryRowContext(ctx, `SELECT id,generation,state,gateway_session_id,offer_answer,request_hash,account_lease_id,account_lease_generation,account_id_hash,gateway_media_generation FROM voice_sessions WHERE workspace_id=? AND owner_id=? AND request_id=?`, p.WorkspaceID, p.SubjectID, req.RequestID).
		Scan(&id, &generation, &state, &gatewayID, &answer, &existingHash, &accountLeaseID, &accountLeaseGeneration, &accountLeaseAccountHash, &gatewayMediaGeneration)
	if err == nil {
		if !bytes.Equal(existingHash, hash[:]) {
			return sessionResponse{}, apiError(http.StatusConflict, "idempotency_conflict", "request_id was already used with different session input", nil)
		}
		if state != "active" {
			return sessionResponse{}, apiError(http.StatusConflict, "voice_outcome_unknown", "the prior media negotiation is unresolved; start a new session only after quarantining this lease", nil)
		}
		if len(answer) == 0 {
			return sessionResponse{}, apiError(http.StatusConflict, "voice_request_already_bound", "this media generation was already acknowledged; read its session state", nil)
		}
		var saved gatewaySessionResponse
		if err := json.Unmarshal(answer, &saved); err != nil {
			return sessionResponse{}, err
		}
		return sessionResponse{SessionID: id, Generation: generation, State: state, AnswerSDP: saved.AnswerSDP, AccountLeaseID: accountLeaseID, AccountLeaseGeneration: accountLeaseGeneration, AccountLeaseAccountHash: accountLeaseAccountHash}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return sessionResponse{}, err
	}
	id = uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sessionResponse{}, err
	}
	defer tx.Rollback()
	var blocked int
	if err := tx.QueryRowContext(ctx, `SELECT blocked FROM voice_pool_state WHERE id=1`).Scan(&blocked); err != nil {
		return sessionResponse{}, err
	}
	if blocked != 0 {
		return sessionResponse{}, apiError(http.StatusServiceUnavailable, "voice_pool_quarantined", "Space account pool is blocked pending operator reconciliation", nil)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES(?,?,?,?,?,?,1,'allocating',?,?)`, id, p.WorkspaceID, p.SubjectID, p.DeviceID, req.RequestID, hash[:], now, now); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return sessionResponse{}, apiError(http.StatusConflict, "voice_lease_busy", "another voice media generation is active or awaiting negotiation for this user", nil)
		}
		return sessionResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE voice_pool_state SET blocked=1,reason_code='negotiation_in_progress',source_session_id=?,updated_at=?,updated_by='system' WHERE id=1`, id, now); err != nil {
		return sessionResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return sessionResponse{}, err
	}
	// The gateway owns account selection and returns only its opaque sticky
	// session handle. Zoe never receives or records an account credential/id.
	gatewayBody, _ := json.Marshal(gatewaySessionRequest{OfferSDP: req.OfferSDP, Voice: req.Voice, VoiceMode: req.VoiceMode, LanguageCode: req.LanguageCode})
	response, gatewayRequestID, callErr := s.callGateway(ctx, http.MethodPost, "/v1/voice/sessions", gatewayBody)
	if callErr != nil {
		_ = s.markSession(ctx, id, "quarantined", "gateway_outcome_unknown", nil, "")
		return sessionResponse{}, apiError(http.StatusServiceUnavailable, "voice_outcome_unknown", "Space gateway outcome is unknown; media lease quarantined", map[string]any{"request_id": gatewayRequestID})
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := "gateway_rejected"
		state := "failed"
		if response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests {
			code, state = "gateway_outcome_unknown", "quarantined"
		}
		_ = s.markSession(ctx, id, state, code, nil, "")
		if state == "quarantined" {
			return sessionResponse{}, apiError(http.StatusServiceUnavailable, "voice_outcome_unknown", "Space gateway did not establish a provably terminated media session; lease quarantined", nil)
		}
		return sessionResponse{}, apiError(http.StatusBadGateway, "voice_gateway_rejected", "Space gateway rejected media negotiation", map[string]any{"status": response.StatusCode, "request_id": gatewayRequestID})
	}
	var upstream gatewaySessionResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, s.maxSDP*2)).Decode(&upstream); err != nil || strings.TrimSpace(upstream.AnswerSDP) == "" || strings.TrimSpace(upstream.VoiceSessionID) == "" {
		_ = s.markSession(ctx, id, "quarantined", "gateway_response_invalid", nil, "")
		return sessionResponse{}, apiError(http.StatusBadGateway, "voice_outcome_unknown", "Space gateway response is incomplete; media lease quarantined", nil)
	}
	if upstream.VoiceSessionGeneration < 0 {
		_ = s.markSession(ctx, id, "quarantined", "gateway_media_generation_invalid", nil, "")
		return sessionResponse{}, apiError(http.StatusBadGateway, "voice_outcome_unknown", "Space gateway returned an invalid media generation; media lease quarantined", nil)
	}
	// The gateway's account ID is sensitive correlation data. Persist only the
	// answer and opaque lease identity; the account ID is hashed below.
	answerJSON, _ := json.Marshal(struct {
		AnswerSDP string `json:"answer_sdp"`
	}{AnswerSDP: upstream.AnswerSDP})
	if err := s.markSession(ctx, id, "active", "", answerJSON, upstream.VoiceSessionID); err != nil {
		// The gateway created a session, but Zoe could not durably bind it. Keep
		// the allocating lease; do not free a potentially live media resource.
		return sessionResponse{}, apiError(http.StatusInternalServerError, "voice_outcome_unknown", "media was negotiated but its durable binding failed; lease remains isolated", nil)
	}
	// The Space gateway intentionally keeps account_id server-side. Its
	// downstream contract exposes only the opaque lease identity and
	// generation, which are sufficient for an exact release request.
	leaseProvided := upstream.AccountLeaseID != "" || upstream.AccountLeaseGeneration != 0
	if leaseProvided {
		if strings.TrimSpace(upstream.AccountLeaseID) == "" || len(upstream.AccountLeaseID) > 512 || strings.TrimSpace(upstream.AccountLeaseID) != upstream.AccountLeaseID || upstream.AccountLeaseGeneration < 1 {
			_ = s.markSession(ctx, id, "quarantined", "gateway_account_lease_invalid", nil, "")
			return sessionResponse{}, apiError(http.StatusBadGateway, "voice_outcome_unknown", "Space gateway returned an incomplete account lease identity; media lease quarantined", nil)
		}
		accountHash := []byte{}
		if strings.TrimSpace(upstream.AccountID) != "" && len(upstream.AccountID) <= 256 && strings.TrimSpace(upstream.AccountID) == upstream.AccountID {
			hash := sha256.Sum256([]byte(upstream.AccountID))
			accountHash = hash[:]
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE voice_sessions SET account_lease_id=?,account_lease_generation=?,account_id_hash=?,gateway_media_generation=? WHERE id=? AND state='active'`, upstream.AccountLeaseID, upstream.AccountLeaseGeneration, accountHash, upstream.VoiceSessionGeneration, id); err != nil {
			// The upstream media exists but its account lease identity is not durably bound.
			// Keep the owner pool fenced; a future hangup cannot claim account-scoped release.
			return sessionResponse{}, apiError(http.StatusInternalServerError, "voice_outcome_unknown", "media lease identity could not be durably bound; account pool remains isolated", nil)
		}
		accountLeaseID, accountLeaseGeneration, accountLeaseAccountHash = upstream.AccountLeaseID, upstream.AccountLeaseGeneration, accountHash
	} else if _, err := s.db.ExecContext(ctx, `UPDATE voice_sessions SET gateway_media_generation=? WHERE id=? AND state='active'`, upstream.VoiceSessionGeneration, id); err != nil {
		return sessionResponse{}, apiError(http.StatusInternalServerError, "voice_outcome_unknown", "media generation could not be durably bound; account pool remains isolated", nil)
	}
	rotate := RotateRequired(upstream.ContextChars, s.contextMaxChars)
	if _, err := s.db.ExecContext(ctx, `UPDATE voice_sessions SET conversation_state=?,conversation_reason=?,conversation_confirmed=?,context_chars=?,rotate_required=? WHERE id=?`, defaultConversation(upstream.Conversation), upstream.ConversationReason, nullableBool(upstream.ConversationConfirmed), upstream.ContextChars, boolInt(rotate), id); err != nil {
		return sessionResponse{}, err
	}
	return sessionResponse{SessionID: id, Generation: 1, State: "active", AnswerSDP: upstream.AnswerSDP, AccountLeaseID: accountLeaseID, AccountLeaseGeneration: accountLeaseGeneration, AccountLeaseAccountHash: accountLeaseAccountHash, Conversation: defaultConversation(upstream.Conversation), ConversationReason: upstream.ConversationReason, ConversationConfirmed: upstream.ConversationConfirmed, ContextChars: upstream.ContextChars, RotateRequired: rotate}, nil
}

func defaultConversation(value string) string {
	if value == "" {
		return "new"
	}
	return value
}
func nullableBool(value *bool) any {
	if value == nil {
		return nil
	}
	return boolInt(*value)
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Service) markSession(ctx context.Context, id, state, code string, answer []byte, gatewayID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE voice_sessions SET state=?,last_error_code=?,offer_answer=coalesce(?,offer_answer),gateway_session_id=CASE WHEN ?='' THEN gateway_session_id ELSE ? END,gateway_unbind_unknown=CASE WHEN ? IN ('gateway_release_unknown','gateway_release_failed') THEN 1 ELSE gateway_unbind_unknown END,updated_at=? WHERE id=? AND state IN ('allocating','active','ending','quarantined')`, state, code, answer, gatewayID, gatewayID, code, now, id); err != nil {
		return err
	}
	if state == "quarantined" {
		if _, err := tx.ExecContext(ctx, `UPDATE voice_pool_state SET blocked=1,reason_code=?,source_session_id=?,updated_at=?,updated_by='system' WHERE id=1`, code, id, now); err != nil {
			return err
		}
	} else if state == "active" {
		if _, err := tx.ExecContext(ctx, `UPDATE voice_pool_state SET blocked=1,reason_code='active_session',source_session_id=?,updated_at=?,updated_by='system' WHERE id=1`, id, now); err != nil {
			return err
		}
	} else if state == "failed" {
		if _, err := tx.ExecContext(ctx, `UPDATE voice_pool_state SET blocked=0,reason_code='',source_session_id='',updated_at=?,updated_by='system' WHERE id=1 AND source_session_id=? AND reason_code='negotiation_in_progress'`, now, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func apiError(status int, code, message string, details any) error {
	return &control.APIError{Status: status, Code: code, Message: message, Details: details}
}

func safeSessionError(err error) error {
	var api *control.APIError
	if errors.As(err, &api) {
		return err
	}
	return apiError(http.StatusInternalServerError, "internal", "voice operation failed", nil)
}

func (s *Service) String() string {
	return fmt.Sprintf("voice service configured=%t", s.gatewayReady())
}
