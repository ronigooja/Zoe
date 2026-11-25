package voice

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/limits"
	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/modelclient"
	"github.com/ronigooja/Zoe/internal/steward"
	"github.com/ronigooja/Zoe/internal/storage"
)

type Config struct {
	Ready                   *ReadyHandle
	RetryModels             map[string]RetryModelConfig
	RetryStatusPath         string
	ResultDelivery          ResultDeliveryConfig
	DiscoverResultReceipts  bool
	MemoryAPI               *memoryapi.Client
	InstructionText         string
	UIControllerInstruction string
	IngestEnabled           bool
	GatewayURL              string
	GatewayDialOrigin       string
	GatewayAPIKey           string
	HTTPClient              *http.Client
	Limits                  limits.Set
	Now                     func() time.Time
	// UpstreamRoute accepts only empty or avas; retained to reject legacy configuration.
	UpstreamRoute          string
	ResultDeliveryEnabled  bool
	ResultDeliveryInterval time.Duration
	ContextRegister        *ContextRegister
	RegisterConfig         *RegisterConfig
	ClassificationTimeout  time.Duration
	Steward                TaskSteward
	UIController           *modelclient.Client
	RecallTimeout          time.Duration
	RecallLease            time.Duration
}

type TaskSteward interface {
	ChooseLatest(context.Context, func(context.Context) (steward.Input, error)) (steward.Choice, error)
}

type Service struct {
	retry                  backoff.Store
	retryModels            map[string]RetryModelConfig
	retryReadyMu           sync.Mutex
	retryReady             bool
	retryStatusPath        string
	retryPublish           chan struct{}
	resultReceiptAdapter   ResultReceiptAdapter
	beforeResultClaim      func() // Test seam after source/destination reads, before the claim transaction.
	resultCapability       *gatewayResultCapability
	db                     *sql.DB
	base                   *url.URL
	dialOrigin             string
	key                    string
	client                 *http.Client
	limits                 limits.Set
	now                    func() time.Time
	resultDeliveryEnabled  bool
	resultDeliveryInterval time.Duration
	steward                TaskSteward
	uiController           *modelclient.Client
	uiEventServer          *control.Server
	register               *ContextRegister
	classifying            sync.Map // Voice-turn row IDs currently being classified.
	recall                 Config
	recallCtx              context.Context
	recallCancel           context.CancelFunc
	recallSlots            chan struct{}
	lifecycleMu            sync.Mutex
	poolRecoveryMu         sync.Mutex
	poolRecoveryNext       time.Time
	poolRecoveryRunning    bool
	unresolvedCallLogMu    sync.Mutex
	unresolvedCallLogNext  map[string]time.Time
	started                bool
	stopped                bool
	workers                sync.WaitGroup
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
	SessionID  string `json:"voice_session_id"`
	Generation int64  `json:"generation"`
	State      string `json:"state"`
}

type gatewayCreationOutcome struct {
	Outcome, EvidenceID string
	lookupMissing       bool
}

const (
	staleCreateAttestationAge = 2 * time.Minute
	unresolvedCreateTimeout   = 5 * time.Minute
	requestFenceTimeout       = 2 * time.Second
	fencedNoLiveMedia         = "fenced_no_live_media"
)

var gatewayEvidenceIDPattern = regexp.MustCompile(`^ev_[0-9a-f]{32}$`)

// recoverable applies only to unbound creates; the resolver fences their identity.
func (o gatewayCreationOutcome) recoverable() bool {
	return o.Outcome == "never_established" || (o.Outcome == "released" && gatewayEvidenceIDPattern.MatchString(o.EvidenceID))
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
	ContentHash string `json:"content_hash,omitempty"`
	Applied     bool   `json:"applied"`
	Version     int    `json:"version,omitempty"`
	Silent      any    `json:"silent,omitempty"`
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

// openingContextHash binds the gateway creation receipt to exactly the sent items.
func openingContextHash(c *openingContext) string {
	data, _ := json.Marshal(c)
	return resultHash(string(data))
}

func Register(server *control.Server, store *storage.Store, cfg Config) error {
	if route := strings.ToLower(strings.TrimSpace(cfg.UpstreamRoute)); route != "" && route != "avas" {
		return errors.New("Zoe voice supports AVAS only; remove legacy ZOE_VOICE_UPSTREAM_ROUTE after confirming the gateway uses AVAS")
	}
	if server == nil || store == nil || store.DB() == nil {
		return errors.New("voice registration requires control server and storage")
	}
	if err := RegisterMigrations(store); err != nil {
		return err
	}
	svc := newService(store.DB(), cfg)
	svc.retry = backoff.Store{DB: store.DB()}
	svc.retryModels = cfg.RetryModels
	svc.retryStatusPath = cfg.RetryStatusPath
	svc.retryPublish = make(chan struct{}, 1)
	if cfg.Ready != nil {
		cfg.Ready.service = svc
	}
	svc.uiEventServer = server
	if cfg.RegisterConfig != nil {
		var err error
		svc.register, err = NewRegister(store.DB(), *cfg.RegisterConfig)
		if err != nil {
			return err
		}
	}
	server.RegisterShutdownHook(svc.Stop)
	if err := server.RegisterCommand("voice.text_input", svc.commandVoiceTextInput); err != nil {
		return err
	}
	if err := server.RegisterCommand("ui.screen", svc.commandUIScreen); err != nil {
		return err
	}
	if err := server.RegisterCommand("ui.rendered", svc.commandUIRendered); err != nil {
		return err
	}
	if err := server.RegisterCommand("ui.action", svc.commandUIAction); err != nil {
		return err
	}
	if err := server.RegisterCommand("voice.pool.reconcile", svc.commandReconcilePool); err != nil {
		return err
	}
	if err := server.RegisterCommand("voice.task.link", svc.commandLinkTask); err != nil {
		return err
	}
	if err := server.RegisterTaskSourceHandler("voice", svc.admitVoiceTask); err != nil {
		return err
	}
	svc.Start()
	if cfg.IngestEnabled {
		if err := server.RegisterRunnerRoutes(func(mux *http.ServeMux) {
			mux.HandleFunc("POST /runner/v1/voice/turns", svc.handleTranscript)
			svc.registerCallRoutes(mux)
		}); err != nil {
			return err
		}
	}
	return server.RegisterAuthenticatedRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /v1/recovery", svc.handleRecovery)
		mux.HandleFunc("GET /v1/ui/snapshot", svc.handleUISnapshot)
		mux.HandleFunc("GET /v1/voice/pool/status", svc.handlePoolStatus)
		mux.HandleFunc("GET /v1/voice/config", svc.handleVoiceConfig)
		mux.HandleFunc("GET /v1/voice/sessions/{session_id}", svc.handleReadSession)
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
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client = &clientCopy
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	interval := cfg.ResultDeliveryInterval
	if interval <= 0 {
		interval = time.Second
	}
	workerCtx, workerCancel := context.WithCancel(context.Background())
	s := &Service{resultReceiptAdapter: cfg.ResultDelivery.ReceiptAdapter, db: db, base: base, key: cfg.GatewayAPIKey, client: client, limits: cfg.Limits, now: now, resultDeliveryEnabled: cfg.ResultDeliveryEnabled, resultDeliveryInterval: interval, register: cfg.ContextRegister, steward: cfg.Steward, uiController: cfg.UIController, recall: cfg, recallCtx: workerCtx, recallCancel: workerCancel, recallSlots: make(chan struct{}, cfg.Limits.Int("voice_recall_workers"))}
	s.dialOrigin = validDialOrigin(cfg.GatewayDialOrigin)
	if cfg.DiscoverResultReceipts && s.gatewayReady() && s.resultReceiptAdapter == nil {
		s.resultCapability = &gatewayResultCapability{adapter: NewGatewayResultReceiptAdapter(s)}
	}
	if cfg.DiscoverResultReceipts && s.gatewayReady() {
		s.resultDeliveryEnabled = true
	}
	return s
}

func (s *Service) Start() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true
	if s.register != nil {
		if s.retry.DB == nil {
			s.retry = backoff.Store{DB: s.db}
		}
		s.register.retry = &s.retry
		s.register.requestPublish = s.PublishRetryState
		s.register.Start(s.recallCtx)
	}
	s.recoverRecall(s.recallCtx, true)
	if s.uiEventServer != nil {
		s.workers.Add(1)
		go func() { defer s.workers.Done(); s.delegationWorker(s.recallCtx) }()
	}
	s.workers.Add(1)
	go func() { defer s.workers.Done(); s.reconcileCallsLoop(s.recallCtx) }()
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
	}
	s.lifecycleMu.Unlock()
	if s.register != nil {
		s.register.Stop()
	}
	s.workers.Wait()
}

func (s *Service) queryGatewayOutcome(ctx context.Context, requestID string) gatewayCreationOutcome {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return gatewayCreationOutcome{}
	}
	endpoint := "/v1/voice/requests/" + url.PathEscape(requestID) + "/outcome"
	ctx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return gatewayCreationOutcome{}
		}
		queryCtx, cancel := context.WithTimeout(withGatewayRequestID(ctx, requestID), 800*time.Millisecond)
		response, _, err := s.callGateway(queryCtx, http.MethodGet, endpoint, nil)
		if err == nil {
			if response.StatusCode == http.StatusNotFound {
				response.Body.Close()
				cancel()
				return gatewayCreationOutcome{lookupMissing: true}
			}
			if response.StatusCode == http.StatusOK {
				const maxOutcomeBytes = 64 << 10
				body, decodeErr := io.ReadAll(io.LimitReader(response.Body, maxOutcomeBytes+1))
				response.Body.Close()
				if decodeErr == nil && len(body) <= maxOutcomeBytes && queryCtx.Err() == nil {
					if outcome, ok := decodeGatewayCreateReply(body, requestID, "outcome"); ok {
						cancel()
						return outcome
					}
				}
				// Do not let a second reply in this sweep turn a malformed
				// success into authorization to issue a fence.
				cancel()
				return gatewayCreationOutcome{}
			} else {
				response.Body.Close()
			}
		}
		cancel()
		if attempt < 2 {
			timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return gatewayCreationOutcome{}
			case <-timer.C:
			}
		}
	}
	return gatewayCreationOutcome{}
}

func (s *Service) fenceGatewayCreate(ctx context.Context, requestID string) (gatewayCreationOutcome, string) {
	ctx, cancel := context.WithTimeout(ctx, requestFenceTimeout)
	defer cancel()
	response, _, err := s.callGateway(ctx, http.MethodPost, "/v1/voice/requests/"+url.PathEscape(requestID)+"/fence", nil)
	if err != nil {
		return gatewayCreationOutcome{}, "fence_transport"
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return gatewayCreationOutcome{}, "fence_http"
	}
	const maxFenceBytes = 4 << 10
	data, err := io.ReadAll(io.LimitReader(response.Body, maxFenceBytes+1))
	if err != nil || len(data) > maxFenceBytes || ctx.Err() != nil {
		return gatewayCreationOutcome{}, "fence_malformed"
	}
	if outcome, ok := decodeGatewayCreateReply(data, requestID, "result"); ok {
		return outcome, ""
	}
	return gatewayCreationOutcome{}, "fence_malformed"
}

func decodeGatewayCreateReply(data []byte, requestID, resultField string) (gatewayCreationOutcome, bool) {
	fields, ok := uniqueJSONObject(data)
	if !ok || !utf8.Valid(data) {
		return gatewayCreationOutcome{}, false
	}
	values := make(map[string]string, len(fields))
	for key, raw := range fields {
		var value *string
		if (key != "request_id" && key != resultField && key != "evidence_id") || json.Unmarshal(raw, &value) != nil || value == nil {
			return gatewayCreationOutcome{}, false
		}
		values[key] = *value
	}
	if requestID == "" || values["request_id"] != requestID {
		return gatewayCreationOutcome{}, false
	}
	outcome := gatewayCreationOutcome{Outcome: values[resultField], EvidenceID: values["evidence_id"]}
	_, evidencePresent := fields["evidence_id"]
	switch outcome.Outcome {
	case "released":
		return outcome, outcome.recoverable()
	case "never_established", "open", "unknown":
		return outcome, !evidencePresent
	case fencedNoLiveMedia, "busy":
		return outcome, resultField == "result" && !evidencePresent
	}
	return gatewayCreationOutcome{}, false
}

func (s *Service) attestStaleCreates(ctx context.Context) {
	now := s.now().UTC()
	rows, err := s.db.QueryContext(ctx, `SELECT s.id,s.generation,s.state,s.gateway_request_id,
		CASE WHEN s.state='quarantined' THEN s.updated_at ELSE s.created_at END
		FROM voice_pool_state p JOIN voice_sessions s ON s.id=p.source_session_id
		WHERE p.id=1 AND p.blocked=1 AND s.gateway_session_id='' AND s.gateway_request_id<>''
		AND ((s.state='allocating' AND p.reason_code='negotiation_in_progress') OR (s.state='quarantined' AND s.last_error_code='gateway_outcome_unknown' AND p.reason_code='gateway_outcome_unknown'))`)
	if err != nil {
		logging.Errorf("voice gateway attestation query failed: %T", err)
		return
	}
	type candidate struct {
		id               string
		generation       int64
		state, requestID string
		since            string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if rows.Scan(&c.id, &c.generation, &c.state, &c.requestID, &c.since) == nil {
			candidates = append(candidates, c)
		}
	}
	if err := rows.Close(); err != nil {
		logging.Errorf("voice gateway attestation rows close failed: %T", err)
		return
	}
	for _, c := range candidates {
		if ctx.Err() != nil {
			return
		}
		since, timeErr := time.Parse(time.RFC3339Nano, c.since)
		age := now.Sub(since)
		if c.state == "allocating" && (timeErr != nil || age < staleCreateAttestationAge) {
			if timeErr != nil {
				s.logUnresolvedCall(c.id, "voice create unresolved: error_class=invalid_creation_time")
			}
			continue
		}
		outcome := gatewayCreationOutcome{}
		if s.gatewayReady() {
			outcome = s.queryGatewayOutcome(ctx, c.requestID)
		}
		kind := outcome.Outcome
		if kind == "" {
			kind = "outcome_unavailable"
		}
		if timeErr == nil && age >= unresolvedCreateTimeout && (outcome.Outcome == "unknown" || outcome.lookupMissing) {
			var failure string
			outcome, failure = s.fenceGatewayCreate(ctx, c.requestID)
			kind = outcome.Outcome
			if failure != "" {
				kind = failure
			}
		}
		if outcome.recoverable() || outcome.Outcome == fencedNoLiveMedia {
			if err := s.resolveGatewayCreateOutcome(ctx, c.id, c.generation, c.state, c.requestID, outcome, c.since); err != nil {
				s.logUnresolvedCall(c.id, "voice create unresolved: error_class=resolution_failed")
			}
		} else if ctx.Err() == nil {
			s.logUnresolvedCall(c.id, "voice create unresolved: error_class=%s", kind)
		}
	}
}

func (s *Service) gatewayReady() bool {
	return s.base != nil && s.base.Hostname() != "" && s.base.User == nil && s.base.RawQuery == "" && !s.base.ForceQuery && s.base.Fragment == "" && s.base.Scheme == "https" && strings.TrimSpace(s.key) != ""
}

func (s *Service) callGateway(ctx context.Context, method, endpoint string, body []byte) (*http.Response, string, error) {
	if !s.gatewayReady() {
		return nil, "", errors.New("Space voice gateway is not configured")
	}
	u := *s.base
	path, query, _ := strings.Cut(endpoint, "?")
	u.RawPath = strings.TrimRight(s.base.EscapedPath(), "/") + path
	var err error
	u.Path, err = url.PathUnescape(u.RawPath)
	if err != nil {
		return nil, "", err
	}
	u.RawQuery = query
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

// recoverQuarantinedCreate applies only to the pool's unresolved, unbound create
// source. The transaction rechecks session state and fences pool release by source.
func (s *Service) recoverQuarantinedCreate(ctx context.Context) error {
	var id, requestID string
	var generation int64
	err := s.db.QueryRowContext(ctx, `SELECT s.id,s.generation,s.gateway_request_id FROM voice_pool_state p JOIN voice_sessions s ON s.id=p.source_session_id WHERE p.id=1 AND p.blocked=1 AND p.reason_code='gateway_outcome_unknown' AND s.state='quarantined' AND s.last_error_code='gateway_outcome_unknown' AND s.gateway_session_id='' AND s.gateway_request_id<>''`).Scan(&id, &generation, &requestID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if outcome := s.queryGatewayOutcome(ctx, requestID); outcome.recoverable() {
		return s.resolveGatewayCreateOutcome(ctx, id, generation, "quarantined", requestID, outcome, "")
	}
	return nil
}

// recoverPoolStatus applies only to authenticated pool-status polls for unknown
// creates. At most one poll recovery runs at once, with a ten-second cooldown.
func (s *Service) recoverPoolStatus(ctx context.Context) {
	s.poolRecoveryMu.Lock()
	now := s.now()
	if s.poolRecoveryRunning || now.Before(s.poolRecoveryNext) {
		s.poolRecoveryMu.Unlock()
		return
	}
	s.poolRecoveryRunning = true
	s.poolRecoveryNext = now.Add(10 * time.Second)
	s.poolRecoveryMu.Unlock()
	defer func() {
		s.poolRecoveryMu.Lock()
		s.poolRecoveryRunning = false
		s.poolRecoveryMu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.recoverQuarantinedCreate(ctx); err != nil && ctx.Err() == nil {
		logging.Errorf("voice pool status recovery failed: %T", err)
	}
}

// resolveGatewayCreateOutcome applies only to attested failed, unbound creates.
// Released evidence attests gateway-local closure, not upstream reclamation.
func (s *Service) resolveGatewayCreateOutcome(ctx context.Context, sessionID string, generation int64, expectedState, requestID string, outcome gatewayCreationOutcome, timeoutSince string) error {
	timedOut := outcome.Outcome == fencedNoLiveMedia && timeoutSince != "" && (expectedState == "allocating" || expectedState == "quarantined")
	if requestID == "" || (!outcome.recoverable() && !timedOut) {
		return nil
	}
	code, reason := "gateway_never_established", "gateway reported never_established"
	if outcome.Outcome == "released" {
		code, reason = "gateway_negotiation_released", "gateway outcome released evidence_id="+outcome.EvidenceID
	}
	if timedOut {
		code, reason = "unresolved_timeout", "unresolved_timeout"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC().Format(time.RFC3339Nano)
	timeoutGuard := ""
	args := []any{code, now, sessionID, generation, expectedState, requestID, sessionID}
	if timedOut {
		if expectedState == "quarantined" {
			timeoutGuard = ` AND last_error_code='gateway_outcome_unknown' AND updated_at=? AND (SELECT reason_code FROM voice_pool_state WHERE id=1)='gateway_outcome_unknown'`
		} else {
			timeoutGuard = ` AND last_error_code='' AND created_at=? AND (SELECT reason_code FROM voice_pool_state WHERE id=1)='negotiation_in_progress'`
		}
		args = append(args, timeoutSince)
	}
	result, err := tx.ExecContext(ctx, `UPDATE voice_sessions SET state='failed',last_error_code=?,updated_at=? WHERE id=? AND generation=? AND state=? AND gateway_session_id='' AND gateway_request_id=? AND last_error_code IN ('','gateway_outcome_unknown') AND EXISTS(SELECT 1 FROM voice_pool_state WHERE id=1 AND blocked=1 AND source_session_id=?)`+timeoutGuard, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM voice_sessions WHERE id=?`, sessionID).Scan(&owner); err != nil {
		return err
	}
	var source string
	if err := tx.QueryRowContext(ctx, `SELECT source_session_id FROM voice_pool_state WHERE id=1`).Scan(&source); err != nil {
		return err
	}
	if source == sessionID {
		id := uuid.NewString()
		if _, err := tx.ExecContext(ctx, `INSERT INTO voice_pool_reconciliations(id,actor_id,source_session_id,generation,reason,method,created_at) VALUES(?,?,?,?,?,'gateway_attestation',?)`, id, "system", sessionID, generation, reason, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE voice_pool_state SET blocked=0,reason_code='',source_session_id='',updated_at=?,updated_by='system' WHERE id=1 AND source_session_id=?`, now, sessionID); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"reconciliation_id": id, "source_session_id": sessionID, "generation": generation, "method": "gateway_attestation", "reason": reason, "account_identity_available": false})
		var cursor int64
		if err := tx.QueryRowContext(ctx, `UPDATE event_counters SET current_cursor=current_cursor+1 WHERE singleton=1 RETURNING current_cursor`).Scan(&cursor); err != nil {
			return err
		}
		eventID := uuid.NewString()
		event := contracts.Event{EventID: eventID, SubjectID: owner, Cursor: fmt.Sprint(cursor), SchemaVersion: 1, Type: "voice.pool.reconciled", AggregateType: "voice_pool", AggregateID: "default", AggregateVersion: 1, OccurredAt: s.now().UTC(), Payload: payload}
		eventPayload, _ := json.Marshal(event)
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(event_id,cursor,schema_version,event_type,occurred_at,subject_id,aggregate_type,aggregate_id,aggregate_version,payload) VALUES(?,?,?,?,?,?,?,?,?,?)`, eventID, cursor, 1, event.Type, now, owner, event.AggregateType, event.AggregateID, event.AggregateVersion, eventPayload); err != nil {
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
