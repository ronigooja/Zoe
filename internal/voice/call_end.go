package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/logging"
)

type terminationEvidence struct {
	GatewaySessionID       string `json:"voice_session_id"`
	MediaGeneration        int64  `json:"voice_session_generation"`
	AccountLeaseID         string `json:"account_lease_id"`
	AccountLeaseGeneration int64  `json:"account_lease_generation"`
	AccountID              string `json:"account_id"`
	Terminated             bool   `json:"terminated"`
	EvidenceID             string `json:"evidence_id"`
	ObservedAt             string `json:"observed_at"`
	Reason                 string `json:"reason"`
	PeerAClosed            bool   `json:"peer_a_closed"`
	PeerBClosed            bool   `json:"peer_b_closed"`
	DataChannelsClosed     bool   `json:"data_channels_closed"`
	TunnelsClosed          bool   `json:"tunnels_closed"`
	RetiredLegs            *int   `json:"retired_legs"`
	RetiredLegsUnclean     *int   `json:"retired_legs_unclean"`
}

func wireUTCTime(raw string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	_, offset := t.Zone()
	return t.UTC(), err == nil && offset == 0 && !t.IsZero()
}

func (e *terminationEvidence) matches(c lifecycleCall, now time.Time) bool {
	if e == nil || e.GatewaySessionID != c.GatewaySessionID || e.MediaGeneration != c.MediaGeneration || e.AccountLeaseID != c.AccountLeaseID || e.AccountLeaseGeneration != c.AccountLeaseGeneration ||
		!e.Terminated || !e.PeerAClosed || !e.PeerBClosed || !e.DataChannelsClosed || !e.TunnelsClosed || e.RetiredLegs == nil || *e.RetiredLegs < 0 || e.RetiredLegsUnclean == nil || *e.RetiredLegsUnclean != 0 ||
		strings.TrimSpace(e.AccountID) == "" || strings.TrimSpace(e.Reason) == "" || !gatewayEvidenceIDPattern.MatchString(e.EvidenceID) {
		return false
	}
	at, ok := wireUTCTime(e.ObservedAt)
	accountHash := sha256.Sum256([]byte(e.AccountID))
	return ok && !at.After(now) && (len(c.accountHash) == 0 || bytes.Equal(c.accountHash, accountHash[:]))
}

func (e terminationEvidence) sanitizedRecord() ([]byte, error) {
	// The account alias is represented exclusively by account_id_hash.
	e.AccountID = ""
	at, _ := wireUTCTime(e.ObservedAt)
	e.ObservedAt = at.Format(time.RFC3339Nano)
	return json.Marshal(e)
}

type callEndedRequest struct {
	callBinding
	Reason              string               `json:"reason"`
	ClosedAt            string               `json:"closed_at"`
	TerminationEvidence *terminationEvidence `json:"termination_evidence,omitempty"`
}

const maxCallClosedAtSkew = 5 * time.Minute

func (r callEndedRequest) valid(receivedAt time.Time) bool {
	if at, ok := wireUTCTime(r.ClosedAt); !ok || at.After(receivedAt.Add(maxCallClosedAtSkew)) || !r.callBinding.valid() {
		return false
	}
	switch r.Reason {
	case "user_hangup", "upstream_voice_end", "media_lost", "gateway_shutdown", "failure":
		return true
	}
	return false
}

type callEndedResponse struct {
	OK                  bool   `json:"ok"`
	State               string `json:"state"`
	UpstreamTermination string `json:"upstream_termination"`
	PoolBlocked         bool   `json:"pool_blocked"`
}

func (s *Service) handleCallEnded(w http.ResponseWriter, r *http.Request) {
	receivedAt := s.now().UTC()
	p, ok := callRunner(w, r)
	if !ok {
		return
	}
	var req callEndedRequest
	if err := decodeRequest(w, r, s.limits.Int64("voice_lifecycle_max_bytes"), &req); err != nil || !req.valid(receivedAt) {
		control.WriteError(w, apiError(http.StatusBadRequest, "invalid_request", "invalid end report", nil))
		return
	}
	ctx, id := r.Context(), r.PathValue("session_id")
	result, err := s.endCall(ctx, p, id, req, receivedAt)
	if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	if result.State == "quarantined" {
		if evidence, outcome := s.queryTerminationEvidence(ctx, req.callBinding); outcome == "released" {
			result, err = s.endFromLookup(ctx, id, req.callBinding, evidence)
			if err != nil {
				control.WriteError(w, safeSessionError(err))
				return
			}
		} else if outcome == "unknown" && ctx.Err() == nil {
			s.logUnresolvedCall(id, "voice session %s termination unresolved: state=quarantined outcome=unknown", id)
		}
	}
	status := http.StatusOK
	if result.State == "quarantined" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, result)
}

func (s *Service) endCall(ctx context.Context, p contracts.RunnerPrincipal, id string, req callEndedRequest, receivedAt time.Time) (callEndedResponse, error) {
	var zero callEndedResponse
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	c, err := loadLifecycleCall(ctx, tx, id)
	if err != nil {
		return zero, err
	}
	if err := c.authorize(p); err != nil {
		return zero, err
	}
	if req.callBinding != c.callBinding || (c.state != "ended" && c.poolSource != id) {
		return zero, callStale()
	}
	closedAt, _ := wireUTCTime(req.ClosedAt)
	closed := closedAt.Format(time.RFC3339Nano)
	if c.endReason != "" && (c.endReason != req.Reason || c.closedAt != closed) {
		return zero, callReplayConflict()
	}
	if c.endReason == "" {
		// Keep the wire timestamp for replay checks and the capped value for
		// closure, even if this report must wait for verified evidence.
		c.effectiveClosedAt = closed
		if closedAt.After(receivedAt) {
			c.effectiveClosedAt = receivedAt.Format(time.RFC3339Nano)
		}
	}
	now := s.now().UTC()
	verified := req.TerminationEvidence.matches(c, now)
	var evidenceHash string
	if req.TerminationEvidence != nil && (verified || c.endEvidenceHash != "") {
		serialized, err := json.Marshal(req.TerminationEvidence)
		if err != nil {
			return zero, err
		}
		evidenceHash = resultHash(string(serialized))
		if c.endReason == "" || strings.HasPrefix(c.endEvidenceHash, "only:") {
			// If the first report carried evidence, dropping it is not an
			// exact replay. Otherwise accept retries of the original report.
			evidenceHash = "only:" + evidenceHash
		}
	}
	if c.endEvidenceHash != "" && (req.TerminationEvidence != nil || strings.HasPrefix(c.endEvidenceHash, "only:")) && evidenceHash != c.endEvidenceHash {
		return zero, callReplayConflict()
	}
	if c.state == "ended" && req.TerminationEvidence != nil && c.endEvidenceHash == "" {
		// A late supplement must match an already-verified closure as well.
		if !verified {
			return zero, callReplayConflict()
		}
		var stored []byte
		if err := tx.QueryRowContext(ctx, `SELECT gateway_evidence FROM voice_account_terminations WHERE session_id=? AND generation=?`, id, c.Generation).Scan(&stored); err != nil {
			return zero, err
		}
		record, err := req.TerminationEvidence.sanitizedRecord()
		if err != nil {
			return zero, err
		}
		if !bytes.Equal(stored, record) {
			return zero, callReplayConflict()
		}
	}
	if c.endReason == "" || (c.endEvidenceHash == "" && evidenceHash != "") {
		if _, err := tx.ExecContext(ctx, `UPDATE voice_sessions SET call_ended_reason=?,call_closed_at=?,call_effective_closed_at=?,call_end_evidence_hash=? WHERE id=?`, req.Reason, closed, c.effectiveClosedAt, evidenceHash, id); err != nil {
			return zero, err
		}
	}
	if c.state != "ended" && (c.state != "quarantined" || verified) {
		c.state, err = s.applyTermination(ctx, tx, id, c, req.TerminationEvidence, now)
		if err != nil {
			return zero, err
		}
	}
	return commitEndResult(ctx, tx, c.state)
}

func (s *Service) applyTermination(ctx context.Context, tx *sql.Tx, id string, c lifecycleCall, e *terminationEvidence, now time.Time) (string, error) {
	at := now.Format(time.RFC3339Nano)
	claim, err := tx.ExecContext(ctx, `UPDATE voice_sessions SET state='ending',updated_at=? WHERE id=? AND generation=? AND state IN ('active','ending','quarantined') AND EXISTS(SELECT 1 FROM voice_pool_state WHERE id=1 AND source_session_id=?)`, at, id, c.Generation, id)
	if err != nil {
		return "", err
	}
	if n, err := claim.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return "", err
		}
		return "", callStale()
	}
	if !e.matches(c, now) {
		const reason = "upstream_termination_unverified"
		if _, err := tx.ExecContext(ctx, `UPDATE voice_sessions SET state='quarantined',last_error_code=?,gateway_unbind_unknown=1 WHERE id=?`, reason, id); err != nil {
			return "", err
		}
		_, err := tx.ExecContext(ctx, `UPDATE voice_pool_state SET blocked=1,reason_code=?,updated_at=?,updated_by='system' WHERE id=1 AND source_session_id=?`, reason, at, id)
		return "quarantined", err
	}
	observedAt, _ := wireUTCTime(e.ObservedAt)
	observed := observedAt.Format(time.RFC3339Nano)
	endedAt := c.effectiveClosedAt
	if endedAt == "" {
		endedAt = observed
	}
	accountHash := sha256.Sum256([]byte(e.AccountID))
	leaseHash := sha256.Sum256([]byte(e.AccountLeaseID))
	record, err := e.sanitizedRecord()
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO voice_account_terminations(id,session_id,generation,account_id_hash,lease_id_hash,evidence_id,observed_at,adapter_kind,gateway_evidence) VALUES(?,?,?,?,?,?,?,'gateway_termination_evidence',?)`, uuid.NewString(), id, c.Generation, accountHash[:], leaseHash[:], e.EvidenceID, observed, record); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE voice_sessions SET state='ended',last_error_code='',ended_at=?,account_id_hash=?,gateway_unbound=1,gateway_unbind_unknown=0 WHERE id=?`, endedAt, accountHash[:], id); err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE voice_pool_state SET blocked=0,reason_code='',source_session_id='',updated_at=?,updated_by='gateway_termination_evidence' WHERE id=1 AND source_session_id=?`, at, id)
	return "ended", err
}

func commitEndResult(ctx context.Context, tx *sql.Tx, state string) (callEndedResponse, error) {
	result := callEndedResponse{OK: true, State: state, UpstreamTermination: "unverified"}
	if state == "ended" {
		result.UpstreamTermination = "verified"
	}
	if err := tx.QueryRowContext(ctx, `SELECT blocked FROM voice_pool_state WHERE id=1`).Scan(&result.PoolBlocked); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func (s *Service) queryTerminationEvidence(ctx context.Context, b callBinding) (*terminationEvidence, string) {
	if !s.gatewayReady() {
		return nil, "unknown"
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	query := url.Values{
		"media_generation":         {strconv.FormatInt(b.MediaGeneration, 10)},
		"account_lease_id":         {b.AccountLeaseID},
		"account_lease_generation": {strconv.FormatInt(b.AccountLeaseGeneration, 10)},
	}
	endpoint := "/v1/voice/sessions/" + url.PathEscape(b.GatewaySessionID) + "/termination-evidence?" + query.Encode()
	r, _, err := s.callGateway(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "unknown"
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return nil, "unknown"
	}
	var result struct {
		Outcome  string               `json:"outcome"`
		Evidence *terminationEvidence `json:"termination_evidence"`
	}
	const max = 64 << 10
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil || len(body) > max || json.Unmarshal(body, &result) != nil {
		return nil, "unknown"
	}
	switch result.Outcome {
	case "released":
		if result.Evidence.matches(lifecycleCall{callBinding: b}, s.now()) {
			return result.Evidence, "released"
		}
	case "not_yet":
		if result.Evidence == nil {
			return nil, "not_yet"
		}
	}
	return nil, "unknown"
}

func (s *Service) endFromLookup(ctx context.Context, id string, b callBinding, e *terminationEvidence) (callEndedResponse, error) {
	var zero callEndedResponse
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	c, err := loadLifecycleCall(ctx, tx, id)
	if err != nil {
		return zero, err
	}
	if b != c.callBinding || (c.state != "ended" && c.poolSource != id) {
		return zero, callStale()
	}
	if c.state != "ended" {
		now := s.now().UTC()
		if !e.matches(c, now) {
			return zero, errors.New("termination evidence conflicts with persisted call binding")
		}
		c.state, err = s.applyTermination(ctx, tx, id, c, e, now)
		if err != nil {
			return zero, err
		}
	}
	return commitEndResult(ctx, tx, c.state)
}

func (s *Service) reconcileCallsLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.expireUnresolvedCallLogs()
			s.reconcileBoundCalls(ctx)
			s.attestStaleCreates(ctx)
		}
	}
}

const unresolvedCallLogInterval = time.Minute

func (s *Service) logUnresolvedCall(id, format string, args ...any) {
	now := s.now()
	s.unresolvedCallLogMu.Lock()
	if now.Before(s.unresolvedCallLogNext[id]) {
		s.unresolvedCallLogMu.Unlock()
		return
	}
	if s.unresolvedCallLogNext == nil {
		s.unresolvedCallLogNext = make(map[string]time.Time)
	}
	s.unresolvedCallLogNext[id] = now.Add(unresolvedCallLogInterval)
	s.unresolvedCallLogMu.Unlock()
	logging.Errorf(format, args...)
}

func (s *Service) expireUnresolvedCallLogs() {
	now := s.now()
	s.unresolvedCallLogMu.Lock()
	defer s.unresolvedCallLogMu.Unlock()
	for id, next := range s.unresolvedCallLogNext {
		if !now.Before(next) {
			delete(s.unresolvedCallLogNext, id)
		}
	}
}

func (s *Service) reconcileBoundCalls(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,state,generation,gateway_session_id,gateway_media_generation,account_lease_id,account_lease_generation FROM voice_sessions WHERE lifecycle_runner_id<>'' AND state IN ('active','ending','quarantined')`)
	if err != nil {
		if ctx.Err() == nil {
			logging.Errorf("voice evidence reconciliation query failed: %T", err)
		}
		return
	}
	type candidate struct {
		id, state string
		callBinding
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.state, &c.Generation, &c.GatewaySessionID, &c.MediaGeneration, &c.AccountLeaseID, &c.AccountLeaseGeneration); err != nil {
			logging.Errorf("voice evidence reconciliation row failed: %T", err)
			continue
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil && ctx.Err() == nil {
		logging.Errorf("voice evidence reconciliation rows failed: %T", err)
	}
	for _, c := range candidates {
		if ctx.Err() != nil {
			return
		}
		if !c.valid() {
			s.logUnresolvedCall(c.id, "voice termination binding unknown for session %s", c.id)
			continue
		}
		e, outcome := s.queryTerminationEvidence(ctx, c.callBinding)
		if outcome == "released" {
			if _, err := s.endFromLookup(ctx, c.id, c.callBinding, e); err != nil && ctx.Err() == nil {
				s.logUnresolvedCall(c.id, "voice evidence reconciliation failed for session %s: %T", c.id, err)
			}
		} else if (outcome == "unknown" || c.state == "ending") && ctx.Err() == nil {
			s.logUnresolvedCall(c.id, "voice session %s termination unresolved: state=%s outcome=%s", c.id, c.state, outcome)
		}
	}
}
