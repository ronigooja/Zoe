package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/storage"
)

// PushSender sends a silent wake-up to one registered device. Implementations
// must not include task titles, notification bodies, or other private content.
type PushSender interface {
	Send(context.Context, PushRequest) PushResponse
}

type PushRequest struct {
	Token      string
	APNSID     string
	CollapseID string
	Cursor     string
}

type PushResponse struct {
	StatusCode int
	Reason     string
	APNSID     string
}

type APNSConfig struct {
	Environment string // exactly "sandbox" or "production"
	TeamID      string
	KeyID       string
	Topic       string
	PrivateKey  []byte // contents of the deployment-provided .p8 key
	HTTPClient  *http.Client
}

type APNSSender struct {
	host        string
	teamID      string
	keyID       string
	topic       string
	key         *ecdsa.PrivateKey
	client      *http.Client
	mu          sync.Mutex
	providerJWT string
	jwtExpires  time.Time
}

// NewAPNSSender validates a fixed APNs environment and static topic. Device
// requests can never influence the APNs host or topic.
func NewAPNSSender(cfg APNSConfig) (*APNSSender, error) {
	var host string
	switch cfg.Environment {
	case "sandbox":
		host = "https://api.sandbox.push.apple.com"
	case "production":
		host = "https://api.push.apple.com"
	default:
		return nil, errors.New("APNs environment must be sandbox or production")
	}
	if strings.TrimSpace(cfg.TeamID) == "" || strings.TrimSpace(cfg.KeyID) == "" || !validTopic(cfg.Topic) || len(cfg.PrivateKey) == 0 {
		return nil, errors.New("APNs team ID, key ID, topic, and private key are required")
	}
	block, _ := pem.Decode(cfg.PrivateKey)
	if block == nil {
		return nil, errors.New("APNs private key is not valid PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse APNs private key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve.Params().BitSize != 256 {
		return nil, errors.New("APNs private key must be P-256 ECDSA")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &APNSSender{host: host, teamID: cfg.TeamID, keyID: cfg.KeyID, topic: cfg.Topic, key: key, client: client}, nil
}

func validTopic(topic string) bool {
	if topic == "" || len(topic) > 255 {
		return false
	}
	for _, r := range topic {
		if !(r == '.' || r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func (s *APNSSender) jwt(now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.providerJWT != "" && now.Before(s.jwtExpires) {
		return s.providerJWT, nil
	}
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": s.keyID})
	claims, _ := json.Marshal(map[string]any{"iss": s.teamID, "iat": now.Unix()})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	r, sigS, err := ecdsa.Sign(rand.Reader, s.key, digest[:])
	if err != nil {
		return "", err
	}
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	sigS.FillBytes(raw[32:])
	s.providerJWT = unsigned + "." + base64.RawURLEncoding.EncodeToString(raw)
	// APNs provider JWTs are valid for up to one hour. Rotate early and reuse
	// the same token across sends to stay within Apple's update guidance.
	s.jwtExpires = now.Add(50 * time.Minute)
	return s.providerJWT, nil
}

func (s *APNSSender) Send(ctx context.Context, p PushRequest) PushResponse {
	jwt, err := s.jwt(time.Now())
	if err != nil {
		return PushResponse{Reason: "jwt_signing_failed"}
	}
	payload, _ := json.Marshal(map[string]any{"aps": map[string]int{"content-available": 1}, "zoe": map[string]string{"inbox_cursor": p.Cursor}})
	url := s.host + "/3/device/" + p.Token
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		return PushResponse{Reason: "request_build_failed"}
	}
	req.Header.Set("authorization", "bearer "+jwt)
	req.Header.Set("apns-topic", s.topic)
	req.Header.Set("apns-push-type", "background")
	req.Header.Set("apns-priority", "5")
	req.Header.Set("apns-id", p.APNSID)
	req.Header.Set("apns-collapse-id", p.CollapseID)
	req.Header.Set("content-type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return PushResponse{Reason: "transport_error"}
	}
	defer resp.Body.Close()
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return PushResponse{StatusCode: resp.StatusCode, Reason: body.Reason, APNSID: resp.Header.Get("apns-id")}
}

type pushTokenPayload struct {
	Platform       string `json:"platform"`
	Token          string `json:"token"`
	TokenID        string `json:"token_id,omitempty"`
	InstallationID string `json:"installation_id,omitempty"`
}

func (s *Server) commandPushTokenRegister(ctx context.Context, tx *sql.Tx, c CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if !HasScope(c.Principal, "device:push") {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "forbidden", "device:push scope is required", nil)
	}
	var p pushTokenPayload
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if p.InstallationID == "" {
		p.InstallationID = c.Principal.InstallationID
	}
	if !validInstallationID(p.InstallationID) {
		return contracts.CommandResult{}, nil, apiError(400, "invalid_request", "installation_id UUID is required", nil)
	}
	p.Platform = strings.ToLower(strings.TrimSpace(p.Platform))
	p.Token = strings.TrimSpace(p.Token)
	if p.Platform != "ios" && p.Platform != "macos" {
		return contracts.CommandResult{}, nil, apiError(http.StatusBadRequest, "invalid_request", "platform must be ios or macos", nil)
	}
	decoded, err := hex.DecodeString(p.Token)
	if err != nil || len(decoded) != 32 || strings.ToLower(p.Token) != p.Token {
		return contracts.CommandResult{}, nil, apiError(http.StatusBadRequest, "invalid_request", "token must be 64 lowercase hexadecimal characters", nil)
	}
	hash := sha256.Sum256(decoded)
	at := c.AcceptedAt.Format(time.RFC3339Nano)
	var tokenID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM device_push_tokens WHERE token_hash=?`, hash[:]).Scan(&tokenID)
	if err == nil {
		var registeredDevice, registeredPlatform string
		if err = tx.QueryRowContext(ctx, `SELECT installation_id,platform FROM device_push_tokens WHERE id=?`, tokenID).Scan(&registeredDevice, &registeredPlatform); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if registeredDevice != p.InstallationID || registeredPlatform != p.Platform {
			return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "token_conflict", "push token is already registered to another device", nil)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE device_push_tokens SET status='disabled',disabled_at=?,updated_at=? WHERE installation_id=? AND platform=? AND status='active' AND id<>?`, at, at, p.InstallationID, p.Platform, tokenID); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE push_deliveries SET status='disabled',updated_at=? WHERE status='pending' AND push_token_id IN (SELECT id FROM device_push_tokens WHERE installation_id=? AND platform=? AND status='disabled')`, at, p.InstallationID, p.Platform); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE device_push_tokens SET subject_id=?,device_id=?,installation_id=?,platform=?,token_hex=?,status='active',created_at=?,updated_at=?,disabled_at=NULL WHERE id=?`, c.Principal.SubjectID, c.Principal.DeviceID, p.InstallationID, p.Platform, p.Token, at, at, tokenID); err != nil {
			return contracts.CommandResult{}, nil, err
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `UPDATE device_push_tokens SET status='disabled',disabled_at=?,updated_at=? WHERE installation_id=? AND platform=? AND status='active'`, at, at, p.InstallationID, p.Platform); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE push_deliveries SET status='disabled',updated_at=? WHERE status='pending' AND push_token_id IN (SELECT id FROM device_push_tokens WHERE installation_id=? AND platform=? AND status='disabled')`, at, p.InstallationID, p.Platform); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		tokenID, err = randomID()
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO device_push_tokens(id,subject_id,device_id,installation_id,platform,token_hex,token_hash,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'active',?,?)`, tokenID, c.Principal.SubjectID, c.Principal.DeviceID, p.InstallationID, p.Platform, p.Token, hash[:], at, at); err != nil {
			return contracts.CommandResult{}, nil, err
		}
	} else {
		return contracts.CommandResult{}, nil, err
	}
	return contracts.CommandResult{ResourceType: "device_push_token", ResourceID: tokenID}, []contracts.Event{event("device.push_token.registered", "device_push_token", tokenID, 1, map[string]string{"platform": p.Platform})}, nil
}

func (s *Server) commandPushTokenRevoke(ctx context.Context, tx *sql.Tx, c CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if !HasScope(c.Principal, "device:push") {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "forbidden", "device:push scope is required", nil)
	}
	var p pushTokenPayload
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	p.TokenID = strings.TrimSpace(p.TokenID)
	if p.TokenID == "" {
		return contracts.CommandResult{}, nil, apiError(http.StatusBadRequest, "invalid_request", "token_id is required", nil)
	}
	if p.InstallationID == "" {
		p.InstallationID = c.Principal.InstallationID
	}
	if !validInstallationID(p.InstallationID) {
		return contracts.CommandResult{}, nil, apiError(400, "invalid_request", "installation_id UUID is required", nil)
	}
	at := c.AcceptedAt.Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE device_push_tokens SET status='disabled',disabled_at=?,updated_at=? WHERE id=? AND subject_id=? AND installation_id=? AND status='active'`, at, at, p.TokenID, c.Principal.SubjectID, p.InstallationID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if n == 0 {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "not_found", "push token not found", nil)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE push_deliveries SET status='disabled',updated_at=? WHERE push_token_id=? AND status='pending'`, at, p.TokenID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	return contracts.CommandResult{ResourceType: "device_push_token", ResourceID: p.TokenID}, []contracts.Event{event("device.push_token.revoked", "device_push_token", p.TokenID, 2, map[string]string{"platform": p.Platform})}, nil
}

// DispatchPushDeliveries sends due outbox rows. Delivery remains best-effort and
// durable; it never gates the authoritative notification transaction.
func (s *Server) DispatchPushDeliveries(ctx context.Context, limit int) error {
	return DispatchPushDeliveries(ctx, s.store, s.pushSender, limit)
}

// DispatchPushDeliveries sends due durable delivery rows through sender.
func DispatchPushDeliveries(ctx context.Context, store *storage.Store, sender PushSender, limit int) error {
	if store == nil || sender == nil || limit <= 0 {
		return nil
	}
	rows, err := store.DB().QueryContext(ctx, `SELECT d.id,d.push_token_id,t.token_hex,d.apns_id,d.collapse_id,d.inbox_cursor,d.attempts FROM push_deliveries d JOIN device_push_tokens t ON t.id=d.push_token_id WHERE d.status='pending' AND d.next_attempt_at<=? AND t.status='active' ORDER BY d.next_attempt_at,d.created_at LIMIT ?`, time.Now().UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return err
	}
	type item struct {
		id, tokenID, token, apnsID, collapseID, cursor string
		attempts                                       int
	}
	var due []item
	for rows.Next() {
		var d item
		if err = rows.Scan(&d.id, &d.tokenID, &d.token, &d.apnsID, &d.collapseID, &d.cursor, &d.attempts); err != nil {
			rows.Close()
			return err
		}
		due = append(due, d)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	disabledToken := make(map[string]bool)
	for _, d := range due {
		if disabledToken[d.tokenID] {
			continue
		}
		var stillAuthorized int
		if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM push_deliveries pd JOIN device_push_tokens pt ON pt.id=pd.push_token_id JOIN actors dev ON dev.id=pt.device_id WHERE pd.id=? AND pd.status='pending' AND pt.status='active' AND dev.status='approved'`, d.id).Scan(&stillAuthorized); err != nil {
			return err
		}
		if stillAuthorized == 0 {
			continue
		}
		res := sender.Send(ctx, PushRequest{Token: d.token, APNSID: d.apnsID, CollapseID: d.collapseID, Cursor: d.cursor})
		now := time.Now().UTC()
		switch {
		case res.StatusCode == http.StatusOK:
			_, err = store.DB().ExecContext(ctx, `UPDATE push_deliveries SET status='delivered',attempts=attempts+1,last_status_code=?,last_error_code='',updated_at=? WHERE id=? AND status='pending'`, res.StatusCode, now.Format(time.RFC3339Nano), d.id)
		case res.StatusCode == http.StatusGone:
			disabledToken[d.tokenID] = true
			_, err = store.DB().ExecContext(ctx, `UPDATE device_push_tokens SET status='disabled',disabled_at=?,updated_at=? WHERE id=?`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), d.tokenID)
			if err == nil {
				_, err = store.DB().ExecContext(ctx, `UPDATE push_deliveries SET status='disabled',attempts=attempts+1,last_status_code=?,last_error_code=?,updated_at=? WHERE push_token_id=? AND status='pending'`, res.StatusCode, safeAPNSReason(res.Reason), now.Format(time.RFC3339Nano), d.tokenID)
			}
		case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 || res.StatusCode == 0:
			attempt := d.attempts + 1
			delay := time.Second * time.Duration(1<<min(attempt, 10))
			if delay > time.Hour {
				delay = time.Hour
			}
			_, err = store.DB().ExecContext(ctx, `UPDATE push_deliveries SET attempts=?,next_attempt_at=?,last_status_code=?,last_error_code=?,updated_at=? WHERE id=? AND status='pending'`, attempt, now.Add(delay).Format(time.RFC3339Nano), res.StatusCode, safeAPNSReason(res.Reason), now.Format(time.RFC3339Nano), d.id)
		default:
			_, err = store.DB().ExecContext(ctx, `UPDATE push_deliveries SET status='failed',attempts=attempts+1,last_status_code=?,last_error_code=?,updated_at=? WHERE id=? AND status='pending'`, res.StatusCode, safeAPNSReason(res.Reason), now.Format(time.RFC3339Nano), d.id)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func safeAPNSReason(reason string) string {
	if len(reason) > 128 {
		return reason[:128]
	}
	for _, r := range reason {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return "apns_error"
		}
	}
	return reason
}

func enqueuePushDeliveries(ctx context.Context, tx *sql.Tx, notificationID, subject, eventID string) error {
	var cursor int64
	if err := tx.QueryRowContext(ctx, `SELECT cursor FROM events WHERE event_id=?`, eventID).Scan(&cursor); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM device_push_tokens WHERE subject_id=? AND status='active'`, subject)
	if err != nil {
		return err
	}
	var tokenIDs []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		tokenIDs = append(tokenIDs, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, tokenID := range tokenIDs {
		id, err := randomID()
		if err != nil {
			return err
		}
		apnsID, err := randomUUID()
		if err != nil {
			return err
		}
		collapseID := "zoe-" + notificationID
		if len(collapseID) > 64 {
			collapseID = collapseID[:64]
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO push_deliveries(id,notification_id,push_token_id,status,next_attempt_at,apns_id,collapse_id,inbox_cursor,created_at,updated_at) VALUES(?,?,?,'pending',?,?,?,?,?,?)`, id, notificationID, tokenID, time.Now().UTC().Format(time.RFC3339Nano), apnsID, collapseID, fmt.Sprint(cursor), time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	return nil
}

func randomUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Installation metadata separates APNs targets; it never grants API authority.
func validInstallationID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
