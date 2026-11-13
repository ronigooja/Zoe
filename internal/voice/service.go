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
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/answer"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/steward"
	"github.com/ronigooja/Zoe/internal/storage"
)

type Config struct {
	MemoryAPI               *memoryapi.Client
	InstructionText         string
	IngestEnabled           bool
	GatewayURL              string
	GatewayAPIKey           string
	HTTPClient              *http.Client
	MaxOfferBytes           int64
	AccountMediaLifecycle   AccountMediaLifecycle
	HeartbeatTimeout        time.Duration
	Now                     func() time.Time
	ContextAssembler        control.ContextAssembler
	OpeningContextBudget    int
	ContextCharBudget       int
	OutcomeWriter           func(context.Context, contracts.Principal, string, string, string) error
	UpstreamRoute           string
	ResultDeliveryEnabled   bool
	ResultDeliveryInterval  time.Duration
	ContextRegister         *ContextRegister
	RegisterConfig          *RegisterConfig
	ClassificationTimeout   time.Duration
	Steward                 TaskSteward
	Answerer                Answerer
	RecallTimeout           time.Duration
	RecallLease             time.Duration
	RecallContextCharBudget int
	RecallHoldingText       string
}

type Answerer interface {
	Answer(context.Context, string, []contracts.MemoryContextItem, string) (answer.Result, error)
}

type TaskSteward interface {
	Choose(context.Context, string, string, string, []steward.Candidate) (steward.Choice, error)
}

type Service struct {
	db                     *sql.DB
	base                   *url.URL
	key                    string
	client                 *http.Client
	maxSDP                 int64
	accountMedia           AccountMediaLifecycle
	heartbeatTimeout       time.Duration
	now                    func() time.Time
	contextAssembler       control.ContextAssembler
	openingBudget          int
	contextCharBudget      int
	outcomeWriter          func(context.Context, contracts.Principal, string, string, string) error
	upstreamRoute          string
	resultDeliveryEnabled  bool
	resultDeliveryInterval time.Duration
	steward                TaskSteward
	register               *ContextRegister
	classifying            sync.Map // Voice-turn row IDs currently being classified.
	recall                 Config
	recallCtx              context.Context
	recallCancel           context.CancelFunc
	recallSlots            chan struct{}
	lifecycleMu            sync.Mutex
	started                bool
	stopped                bool
	workers                sync.WaitGroup
	reaperStop             chan struct{}
	reaperDone             chan struct{}
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
	OpeningContextDelivery  string `json:"opening_context_delivery,omitempty"`
	OpeningContextVersion   int    `json:"opening_context_version,omitempty"`
	OpeningContextChars     int    `json:"opening_context_chars,omitempty"`
	GatewayContextChars     int    `json:"gateway_context_chars,omitempty"`
}

type gatewaySessionRequest struct {
	OfferSDP       string          `json:"offer_sdp"`
	VoiceSessionID string          `json:"voice_session_id,omitempty"`
	Voice          string          `json:"voice,omitempty"`
	VoiceMode      string          `json:"voice_mode,omitempty"`
	LanguageCode   string          `json:"language_code,omitempty"`
	OpeningContext *openingContext `json:"opening_context,omitempty"`
	Instructions   string          `json:"instructions,omitempty"`
}

type gatewaySessionResponse struct {
	AnswerSDP              string                `json:"answer_sdp"`
	VoiceSessionID         string                `json:"voice_session_id"`
	VoiceSessionGeneration int64                 `json:"voice_session_generation,omitempty"`
	AccountLeaseID         string                `json:"account_lease_id,omitempty"`
	AccountLeaseGeneration int64                 `json:"account_lease_generation,omitempty"`
	AccountID              string                `json:"account_id,omitempty"`
	Conversation           string                `json:"conversation,omitempty"`
	ConversationConfirmed  *bool                 `json:"conversation_confirmed,omitempty"`
	ContextChars           int                   `json:"context_chars,omitempty"`
	OpeningContext         *openingContextResult `json:"opening_context,omitempty"`
}

type gatewayOutcomeResponse struct {
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome"`
}

type openingContextItem struct {
	Role string `json:"role"`
	Text string `json:"text"`
}
type openingContext struct {
	Kind    string               `json:"kind"`
	Version int                  `json:"version"`
	Items   []openingContextItem `json:"items"`
}
type openingContextResult struct {
	Applied   bool  `json:"applied"`
	Version   int   `json:"version,omitempty"`
	Silent    any   `json:"silent,omitempty"`
	Supported *bool `json:"supported,omitempty"`
}

func openingCharsFor(c *openingContext) int {
	if c == nil {
		return 0
	}
	n := 0
	for _, item := range c.Items {
		n += len([]rune(item.Text))
	}
	return n
}

func (s *Service) buildOpeningContext(ctx context.Context, p contracts.Principal) *openingContext {
	if s.upstreamRoute == "avas" {
		return s.buildAVASOpeningContext(ctx, p)
	}
	if s.contextAssembler == nil || s.openingBudget <= 0 {
		return nil
	}
	assembled, err := s.contextAssembler(ctx, p, "", p.SubjectID, "", contracts.MemoryContextDefaultBudgetTokens)
	if err != nil || len(assembled.Items) == 0 {
		return nil
	}
	version := 1
	_ = s.db.QueryRowContext(ctx, `SELECT coalesce(max(opening_context_version),0)+1 FROM voice_sessions WHERE workspace_id=? AND owner_id=?`, p.WorkspaceID, p.SubjectID).Scan(&version)
	c := &openingContext{Kind: "full", Version: version}
	used := 0
	for _, item := range assembled.Items {
		text := strings.TrimSpace(item.Subject + ": " + item.Key + " = " + item.Content)
		cost := len([]rune(text))
		if cost == 0 || used+cost > s.openingBudget {
			break
		}
		c.Items = append(c.Items, openingContextItem{Role: "user", Text: text})
		used += cost
	}
	rows, err := s.db.QueryContext(ctx, `SELECT title,goal FROM tasks WHERE workspace_id=? AND owner_subject_id=? AND status IN ('queued','running') ORDER BY updated_at DESC LIMIT 8`, p.WorkspaceID, p.SubjectID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var title, goal string
			if rows.Scan(&title, &goal) != nil {
				continue
			}
			text := "active task: " + strings.TrimSpace(title+" — "+goal)
			cost := len([]rune(text))
			if cost == 0 || used+cost > s.openingBudget {
				break
			}
			c.Items = append(c.Items, openingContextItem{Role: "user", Text: text})
			used += cost
		}
	}
	if len(c.Items) == 0 {
		return nil
	}
	return c
}

func (s *Service) buildAVASOpeningContext(ctx context.Context, p contracts.Principal) *openingContext {
	version := 1
	_ = s.db.QueryRowContext(ctx, `SELECT coalesce(max(opening_context_version),0)+1 FROM voice_sessions WHERE workspace_id=? AND owner_id=?`, p.WorkspaceID, p.SubjectID).Scan(&version)
	c := &openingContext{Kind: "full", Version: version, Items: []openingContextItem{}}
	used := 0
	appendItem := func(text string) bool {
		cost := len([]rune(text))
		if len(c.Items) >= 128 || used+cost > s.openingBudget {
			return false
		}
		c.Items = append(c.Items, openingContextItem{Role: "user", Text: text})
		used += cost
		return true
	}
	if s.contextAssembler != nil {
		assembled, err := s.contextAssembler(ctx, p, "", p.SubjectID, "", contracts.MemoryContextDefaultBudgetTokens)
		if err == nil {
			facts := 0
			for _, item := range assembled.Items {
				key, content := strings.TrimSpace(item.Key), strings.TrimSpace(item.Content)
				if key == "" || content == "" {
					continue
				}
				// AVAS facts use key:content, omitting internal subject IDs and
				// the assembler's machine-style equals sign.
				if facts == 5 || !appendItem(key+":"+content) {
					break
				}
				facts++
			}
		}
	}
	if s.register != nil {
		text, err := s.register.AssembleContext(ctx, RegisterOwner{p.WorkspaceID, p.SubjectID}, s.now(), max(0, s.openingBudget-used))
		if err == nil && text != "" {
			appendItem(text)
		}
	}
	// Running or queued tasks are always listed; waiting tasks only when updated
	// within the last 30 minutes, so abandoned tasks do not read as current work.
	recent := s.now().Add(-30 * time.Minute).UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `SELECT title FROM tasks WHERE workspace_id=? AND owner_subject_id=? AND (status IN ('active','queued') OR (status IN ('waiting','awaiting_user') AND updated_at>=?)) ORDER BY updated_at DESC,id LIMIT 32`, p.WorkspaceID, p.SubjectID, recent)
	if err == nil {
		defer rows.Close()
		seen := map[string]bool{}
		listed := 0
		for rows.Next() && listed < 8 {
			var title string
			if rows.Scan(&title) != nil {
				continue
			}
			title = strings.TrimSpace(title)
			if title == "" || seen[title] {
				continue
			}
			seen[title] = true
			if !appendItem("进行中的任务:" + title) {
				break
			}
			listed++
		}
	}
	return c
}

func (s *Service) deltaOpeningContext(ctx context.Context, p contracts.Principal, full *openingContext) *openingContext {
	if full == nil {
		return nil
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT opening_context_version FROM voice_sessions WHERE workspace_id=? AND owner_id=? AND state='active'`, p.WorkspaceID, p.SubjectID).Scan(&version); err != nil || version < 1 {
		return full
	}
	if version >= full.Version {
		return nil
	}
	return full
}

func (s *Service) applyOpeningContext(ctx context.Context, sessionID string, c *openingContext) (string, error) {
	if c == nil || len(c.Items) == 0 {
		return "unsupported", nil
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "unsupported", err
	}
	response, _, err := s.callGateway(ctx, http.MethodPost, "/v1/voice/sessions/"+url.PathEscape(sessionID)+"/opening-context", body)
	if err != nil {
		return "unverified", err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotImplemented {
		return "unsupported", nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "unverified", fmt.Errorf("opening context gateway status %d", response.StatusCode)
	}
	var result openingContextResult
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return "unverified", err
	}
	if result.Silent == "unverified" {
		return "unverified", nil
	}
	if result.Applied {
		return "delivered", nil
	}
	return "unverified", nil
}

func Register(server *control.Server, store *storage.Store, cfg Config) error {
	if _, err := ResolveInstructionText(cfg.InstructionText); err != nil {
		return err
	}
	if server == nil || store == nil || store.DB() == nil {
		return errors.New("voice registration requires control server and storage")
	}
	if err := RegisterMigrations(store); err != nil {
		return err
	}
	svc := newService(store.DB(), cfg)
	if cfg.RegisterConfig != nil {
		var err error
		svc.register, err = NewRegister(store.DB(), *cfg.RegisterConfig)
		if err != nil {
			return err
		}
	}
	server.RegisterShutdownHook(svc.Stop)
	svc.Start()
	if err := server.RegisterCommand("voice.turn.commit", svc.commandCommitTurn); err != nil {
		return err
	}
	if err := server.RegisterCommand("voice.task.link", svc.commandLinkTask); err != nil {
		return err
	}
	if err := server.RegisterCommand("voice.turn.handoff", func(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
		return svc.commandHandoff(ctx, tx, cmd, req, server)
	}); err != nil {
		return err
	}
	if err := server.RegisterCommandPreflight("voice.turn.handoff", svc.prepareHandoff); err != nil {
		return err
	}
	if err := server.RegisterTaskSourceHandler("voice", svc.admitVoiceTask); err != nil {
		return err
	}
	if err := server.RegisterCommand("voice.pool.reconcile", svc.commandReconcilePool); err != nil {
		return err
	}
	if cfg.IngestEnabled {
		if err := server.RegisterRunnerRoutes(func(mux *http.ServeMux) {
			mux.HandleFunc("POST /runner/v1/voice/turns", svc.handleTranscript)
		}); err != nil {
			return err
		}
	}
	return server.RegisterAuthenticatedRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /v1/voice/pool/status", svc.handlePoolStatus)
		mux.HandleFunc("GET /v1/voice/config", svc.handleVoiceConfig)
		mux.HandleFunc("POST /v1/voice/sessions", svc.handleCreate)
		mux.HandleFunc("GET /v1/voice/sessions/{session_id}", svc.handleReadSession)
		mux.HandleFunc("POST /v1/voice/sessions/{session_id}/connected", svc.handleConnected)
		mux.HandleFunc("POST /v1/voice/sessions/{session_id}/heartbeat", svc.handleHeartbeat)
		mux.HandleFunc("POST /v1/voice/sessions/{session_id}/outcome", svc.handleOutcome)
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
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	budget := cfg.OpeningContextBudget
	if budget <= 0 {
		budget = 8000
	}
	contextBudget := cfg.ContextCharBudget
	if contextBudget <= 0 {
		contextBudget = 12000
	}
	route := strings.ToLower(strings.TrimSpace(cfg.UpstreamRoute))
	if route != "avas" {
		route = "wm"
	}
	interval := cfg.ResultDeliveryInterval
	if interval <= 0 {
		interval = time.Second
	}
	workerCtx, workerCancel := context.WithCancel(context.Background())
	return &Service{db: db, base: base, key: cfg.GatewayAPIKey, client: client, maxSDP: cfg.MaxOfferBytes, accountMedia: cfg.AccountMediaLifecycle, heartbeatTimeout: timeout, now: now, contextAssembler: cfg.ContextAssembler, openingBudget: budget, contextCharBudget: contextBudget, outcomeWriter: cfg.OutcomeWriter, upstreamRoute: route, resultDeliveryEnabled: cfg.ResultDeliveryEnabled, resultDeliveryInterval: interval, register: cfg.ContextRegister, steward: cfg.Steward, recall: cfg, recallCtx: workerCtx, recallCancel: workerCancel, recallSlots: make(chan struct{}, recallWorkerLimit), reaperStop: make(chan struct{}), reaperDone: make(chan struct{})}
}

func (s *Service) writeOutcome(ctx context.Context, p contracts.Principal, sessionID, summary, outcome string) error {
	if s.outcomeWriter == nil {
		return errors.New("voice outcome memory writer is unavailable")
	}
	return s.outcomeWriter(ctx, p, sessionID, summary, outcome)
}

func (s *Service) rotateIfOverBudget(ctx context.Context, p contracts.Principal) error {
	if s.contextCharBudget <= 0 {
		return nil
	}
	var id, gatewayID, state string
	var generation int64
	var chars int
	err := s.db.QueryRowContext(ctx, `SELECT id,gateway_session_id,state,generation,gateway_context_chars FROM voice_sessions WHERE workspace_id=? AND owner_id=? AND state='active'`, p.WorkspaceID, p.SubjectID).Scan(&id, &gatewayID, &state, &generation, &chars)
	if errors.Is(err, sql.ErrNoRows) || chars <= s.contextCharBudget {
		return nil
	}
	if err != nil {
		return err
	}
	if gatewayID == "" {
		return apiError(http.StatusConflict, "voice_outcome_unknown", "over-budget voice session has no gateway binding", nil)
	}
	response, _, err := s.callGateway(ctx, http.MethodDelete, "/v1/voice/sessions/"+url.PathEscape(gatewayID), nil)
	if err != nil {
		return apiError(http.StatusServiceUnavailable, "voice_outcome_unknown", "voice rotation outcome is unknown", nil)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return apiError(http.StatusServiceUnavailable, "voice_outcome_unknown", "voice rotation was not confirmed", nil)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE voice_sessions SET state='ended',gateway_unbound=1,offer_answer='',ended_at=?,updated_at=? WHERE id=? AND generation=? AND state='active'`, s.now().UTC().Format(time.RFC3339Nano), s.now().UTC().Format(time.RFC3339Nano), id, generation)
	if err == nil {
		_, err = s.db.ExecContext(ctx, `UPDATE voice_pool_state SET blocked=0,reason_code='',source_session_id='',updated_at=?,updated_by='context_rotation' WHERE id=1 AND source_session_id=?`, s.now().UTC().Format(time.RFC3339Nano), id)
	}
	return err
}

func (s *Service) Start() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true
	if s.register != nil {
		s.register.Start(s.recallCtx)
	}
	s.recoverRecall(s.recallCtx, true)
	s.workers.Add(1)
	go func() { defer s.workers.Done(); s.reaper() }()
	if s.resultDeliveryEnabled || s.register != nil {
		s.workers.Add(1)
		go func() { defer s.workers.Done(); s.resultDispatcher() }()
	}
}

func (s *Service) Stop() {
	s.lifecycleMu.Lock()
	if !s.stopped {
		s.stopped = true
		s.recallCancel()
		close(s.reaperStop)
	}
	s.lifecycleMu.Unlock()
	if s.register != nil {
		s.register.Stop()
	}
	s.workers.Wait()
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
			s.attestStaleCreates(context.Background())
		}
	}
}

func (s *Service) queryGatewayOutcome(ctx context.Context, requestID string) string {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return ""
	}
	endpoint := "/v1/voice/requests/" + url.PathEscape(requestID) + "/outcome"
	ctx = context.WithoutCancel(ctx)
	for attempt := 0; attempt < 3; attempt++ {
		queryCtx, cancel := context.WithTimeout(withGatewayRequestID(ctx, requestID), 800*time.Millisecond)
		response, _, err := s.callGateway(queryCtx, http.MethodGet, endpoint, nil)
		if err == nil {
			if response.StatusCode == http.StatusNotFound {
				response.Body.Close()
				cancel()
				return ""
			}
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				var outcome gatewayOutcomeResponse
				decodeErr := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&outcome)
				response.Body.Close()
				if decodeErr == nil && outcome.RequestID == requestID && (outcome.Outcome == "never_established" || outcome.Outcome == "open" || outcome.Outcome == "released" || outcome.Outcome == "unknown") {
					cancel()
					return outcome.Outcome
				}
			} else {
				response.Body.Close()
			}
		}
		cancel()
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
		}
	}
	return ""
}

func (s *Service) attestStaleCreates(ctx context.Context) {
	cutoff := s.now().UTC().Add(-2 * time.Minute).Format("2006-01-02T15:04:05.000000000Z")
	rows, err := s.db.QueryContext(ctx, `SELECT id,generation,state,gateway_request_id FROM voice_sessions WHERE gateway_request_id<>'' AND ((state='allocating' AND julianday(COALESCE(created_at,updated_at)) < julianday(?)) OR (state='quarantined' AND last_error_code='gateway_outcome_unknown'))`, cutoff)
	if err != nil {
		logging.Errorf("voice gateway attestation query failed: %T", err)
		return
	}
	type candidate struct {
		id               string
		generation       int64
		state, requestID string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if rows.Scan(&c.id, &c.generation, &c.state, &c.requestID) == nil {
			candidates = append(candidates, c)
		}
	}
	if err := rows.Close(); err != nil {
		logging.Errorf("voice gateway attestation rows close failed: %T", err)
		return
	}
	for _, c := range candidates {
		if s.queryGatewayOutcome(ctx, c.requestID) == "never_established" {
			if err := s.resolveGatewayNeverEstablished(ctx, c.id, c.generation, c.state); err != nil {
				logging.Errorf("voice gateway attestation resolution failed for session %s: %T", c.id, err)
			}
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
		logging.Errorf("voice reaper query failed: %T", err)
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
		logging.Errorf("voice reaper rows close failed: %T", err)
		return
	}
	for _, id := range ids {
		claim, err := s.db.ExecContext(ctx, `UPDATE voice_sessions SET state='ending',updated_at=? WHERE id=? AND state='active' AND julianday(COALESCE(heartbeat_at,created_at,updated_at)) < julianday(?)`, s.now().UTC().Format(time.RFC3339Nano), id, cutoffValue)
		if err != nil {
			logging.Errorf("voice reaper claim failed: %T", err)
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
			logging.Errorf("voice reaper load failed: %T", err)
			continue
		}
		if gatewayID == "" {
			if err := s.markSession(ctx, id, "quarantined", "gateway_session_unbound", nil, ""); err != nil {
				logging.Errorf("voice reaper quarantine failed for session %s: %T", id, err)
			}
			continue
		}
		// Missing heartbeat implies the client stopped its local media, so media_closed is trusted for this system reclaim.
		_, _, _, err = s.releaseSession(ctx, contracts.Principal{WorkspaceID: workspaceID, SubjectID: ownerID}, id, generation, gatewayID, accountLeaseID, accountLeaseGeneration, gatewayMediaGeneration, gatewayUnbound, gatewayUnbindUnknown, true)
		if err != nil {
			logging.Errorf("voice reaper release failed for session %s: %T", id, err)
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
	if err := s.rotateIfOverBudget(ctx, p); err != nil {
		return sessionResponse{}, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return sessionResponse{}, err
	}
	hash := sha256.Sum256(body)
	var id, state, gatewayID, accountLeaseID string
	var generation int64
	var accountLeaseGeneration int64
	var gatewayMediaGeneration, gatewayContextChars int64
	var openingDelivery, openingManifest []byte
	var openingVersion, openingChars int
	var accountLeaseAccountHash []byte
	var answer []byte
	var existingHash []byte
	err = s.db.QueryRowContext(ctx, `SELECT id,generation,state,gateway_session_id,offer_answer,request_hash,account_lease_id,account_lease_generation,account_id_hash,gateway_media_generation,opening_context_delivery,opening_context_version,opening_context_chars,opening_context_manifest,gateway_context_chars FROM voice_sessions WHERE workspace_id=? AND owner_id=? AND request_id=?`, p.WorkspaceID, p.SubjectID, req.RequestID).
		Scan(&id, &generation, &state, &gatewayID, &answer, &existingHash, &accountLeaseID, &accountLeaseGeneration, &accountLeaseAccountHash, &gatewayMediaGeneration, &openingDelivery, &openingVersion, &openingChars, &openingManifest, &gatewayContextChars)
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
		return sessionResponse{SessionID: id, Generation: generation, State: state, AnswerSDP: saved.AnswerSDP, AccountLeaseID: accountLeaseID, AccountLeaseGeneration: accountLeaseGeneration, AccountLeaseAccountHash: accountLeaseAccountHash, OpeningContextDelivery: string(openingDelivery), OpeningContextVersion: openingVersion, OpeningContextChars: openingChars, GatewayContextChars: int(gatewayContextChars)}, nil
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
	var opening *openingContext
	if s.upstreamRoute == "avas" {
		opening = s.buildOpeningContext(ctx, p)
	}
	// The gateway owns account selection and returns only its opaque sticky
	// session handle. Zoe never receives or records an account credential/id.
	// Startup rejects invalid configuration. Keep media creation safe if an
	// internally constructed service nevertheless contains invalid text.
	instructions, _ := ResolveInstructionText(s.recall.InstructionText)
	gatewayBody, _ := json.Marshal(gatewaySessionRequest{OfferSDP: req.OfferSDP, Voice: req.Voice, VoiceMode: req.VoiceMode, LanguageCode: req.LanguageCode, OpeningContext: opening, Instructions: instructions})
	gatewayRequestID := uuid.NewString()
	if err := s.persistGatewayRequestID(ctx, id, gatewayRequestID); err != nil {
		_ = s.markSession(context.WithoutCancel(ctx), id, "failed", "gateway_request_id_persist_failed", nil, "")
		return sessionResponse{}, apiError(http.StatusInternalServerError, "voice_unavailable", "voice gateway request could not be durably prepared", nil)
	}
	response, _, callErr := s.callGateway(withGatewayRequestID(ctx, gatewayRequestID), http.MethodPost, "/v1/voice/sessions", gatewayBody)
	if callErr != nil {
		_ = s.markSession(ctx, id, "quarantined", "gateway_outcome_unknown", nil, "")
		if s.queryGatewayOutcome(ctx, gatewayRequestID) == "never_established" {
			_ = s.resolveGatewayNeverEstablished(ctx, id, 1, "quarantined")
		}
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
			if s.queryGatewayOutcome(ctx, gatewayRequestID) == "never_established" {
				_ = s.resolveGatewayNeverEstablished(ctx, id, 1, "quarantined")
			}
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
	if s.upstreamRoute == "wm" {
		opening = s.buildOpeningContext(ctx, p)
		if upstream.Conversation == "resumed" {
			opening = s.deltaOpeningContext(ctx, p, opening)
			if opening != nil {
				opening.Kind = "delta"
			}
		}
	}
	if s.upstreamRoute == "avas" {
		openingDelivery = []byte("unsupported")
		if opening != nil {
			openingDelivery = []byte("unverified")
			openingVersion = opening.Version
			openingChars = openingCharsFor(opening)
		}
	} else if upstream.OpeningContext != nil && upstream.OpeningContext.Applied {
		openingDelivery = []byte("delivered")
		openingVersion = opening.Version
		openingChars = openingCharsFor(opening)
	} else if opening != nil {
		openingDelivery = []byte("unverified")
		openingVersion = opening.Version
		openingChars = openingCharsFor(opening)
	}
	if upstream.OpeningContext != nil && upstream.OpeningContext.Silent == "unverified" {
		openingDelivery = []byte("unverified")
	}
	if upstream.OpeningContext != nil && (upstream.OpeningContext.Silent == nil || upstream.OpeningContext.Silent == true) && upstream.OpeningContext.Applied {
		openingDelivery = []byte("delivered")
	}
	if s.upstreamRoute == "avas" && upstream.OpeningContext != nil && upstream.OpeningContext.Supported != nil && !*upstream.OpeningContext.Supported {
		openingDelivery = []byte("unsupported")
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
	if opening != nil {
		manifest, _ := json.Marshal(opening)
		_, _ = s.db.ExecContext(ctx, `UPDATE voice_sessions SET opening_context_delivery=?,opening_context_version=?,opening_context_chars=?,opening_context_manifest=?,gateway_context_chars=? WHERE id=?`, string(openingDelivery), openingVersion, openingChars, manifest, upstream.ContextChars, id)
	}
	if opening == nil {
		openingDelivery = []byte("unsupported")
	}
	if opening != nil && s.upstreamRoute == "wm" {
		// The gateway owns route selection. WM applies context through this
		// endpoint; AVAS returns 501 because its creation-time initial_items
		// already carried the context. Both outcomes are non-fatal to media.
		delivery, _ := s.applyOpeningContext(ctx, upstream.VoiceSessionID, opening)
		openingDelivery = []byte(delivery)
		_, _ = s.db.ExecContext(ctx, `UPDATE voice_sessions SET opening_context_delivery=? WHERE id=?`, delivery, id)
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
	return sessionResponse{SessionID: id, Generation: 1, State: "active", AnswerSDP: upstream.AnswerSDP, AccountLeaseID: accountLeaseID, AccountLeaseGeneration: accountLeaseGeneration, AccountLeaseAccountHash: accountLeaseAccountHash, OpeningContextDelivery: string(openingDelivery), OpeningContextVersion: openingVersion, OpeningContextChars: openingChars, GatewayContextChars: upstream.ContextChars}, nil
}

func (s *Service) persistGatewayRequestID(ctx context.Context, sessionID, requestID string) error {
	result, err := s.db.ExecContext(context.WithoutCancel(ctx), `UPDATE voice_sessions SET gateway_request_id=?,updated_at=? WHERE id=? AND state='allocating'`, requestID, s.now().UTC().Format(time.RFC3339Nano), sessionID)
	if err == nil {
		if changed, rowsErr := result.RowsAffected(); rowsErr != nil {
			err = rowsErr
		} else if changed != 1 {
			err = fmt.Errorf("voice gateway request id was not persisted for allocating session %s", sessionID)
		}
	}
	return err
}

func (s *Service) resolveGatewayNeverEstablished(ctx context.Context, sessionID string, generation int64, expectedState string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE voice_sessions SET state='failed',last_error_code='gateway_never_established',updated_at=? WHERE id=? AND generation=? AND state=? AND last_error_code IN ('','gateway_outcome_unknown')`, now, sessionID, generation, expectedState)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	var workspace, owner string
	if err := tx.QueryRowContext(ctx, `SELECT workspace_id,owner_id FROM voice_sessions WHERE id=?`, sessionID).Scan(&workspace, &owner); err != nil {
		return err
	}
	var source string
	if err := tx.QueryRowContext(ctx, `SELECT source_session_id FROM voice_pool_state WHERE id=1`).Scan(&source); err != nil {
		return err
	}
	if source == sessionID {
		id := uuid.NewString()
		if _, err := tx.ExecContext(ctx, `INSERT INTO voice_pool_reconciliations(id,actor_id,source_session_id,generation,reason,method,created_at) VALUES(?,?,?,?,?,'gateway_attestation',?)`, id, "system", sessionID, generation, "gateway reported never_established", now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE voice_pool_state SET blocked=0,reason_code='',source_session_id='',updated_at=?,updated_by='system' WHERE id=1 AND source_session_id=?`, now, sessionID); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"reconciliation_id": id, "source_session_id": sessionID, "generation": generation, "method": "gateway_attestation", "reason": "gateway reported never_established", "account_identity_available": false})
		var cursor int64
		if err := tx.QueryRowContext(ctx, `UPDATE event_counters SET current_cursor=current_cursor+1 WHERE workspace_id=? RETURNING current_cursor`, workspace).Scan(&cursor); err != nil {
			return err
		}
		eventID := uuid.NewString()
		event := contracts.Event{EventID: eventID, WorkspaceID: workspace, SubjectID: owner, Cursor: fmt.Sprint(cursor), SchemaVersion: 1, Type: "voice.pool.reconciled", AggregateType: "voice_pool", AggregateID: "default", AggregateVersion: 1, OccurredAt: s.now().UTC(), Payload: payload}
		eventPayload, _ := json.Marshal(event)
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(event_id,workspace_id,cursor,schema_version,event_type,occurred_at,subject_id,aggregate_type,aggregate_id,aggregate_version,payload) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, eventID, workspace, cursor, 1, event.Type, now, owner, event.AggregateType, event.AggregateID, event.AggregateVersion, eventPayload); err != nil {
			return err
		}
	}
	return tx.Commit()
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
