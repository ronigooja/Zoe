package voice

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/ronigooja/Zoe/internal/limits"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/contracts"
)

// ResultCheckpointSchema must be installed by the voice schema migration before dispatch.
// Chunk state confirmed means gateway acceptance, never upstream consumption or speech.
const ResultCheckpointSchema = `
CREATE TABLE voice_result_checkpoints (
 entry_id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, task_id TEXT NOT NULL,
 run_id TEXT NOT NULL, subject_id TEXT NOT NULL, content_hash TEXT NOT NULL,
 session_id TEXT NOT NULL, generation INTEGER NOT NULL, gateway_session_id TEXT NOT NULL,
 media_generation INTEGER NOT NULL, claim INTEGER NOT NULL DEFAULT 0, lease_until TEXT NOT NULL DEFAULT ''
);
CREATE TABLE voice_result_chunks (
 entry_id TEXT NOT NULL REFERENCES voice_result_checkpoints(entry_id), ordinal INTEGER NOT NULL,
 delivery_id TEXT NOT NULL UNIQUE, content_hash TEXT NOT NULL, text TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','attempted','confirmed','unknown')),
 PRIMARY KEY(entry_id,ordinal)
);
CREATE TABLE voice_result_availability (
 entry_id TEXT NOT NULL, session_id TEXT NOT NULL, generation INTEGER NOT NULL,
 available_at TEXT NOT NULL, PRIMARY KEY(entry_id,session_id,generation)
);`

// ResultChunkRequest binds an immutable payload to the actual gateway media generation.
type ResultChunkRequest struct {
	MediaGeneration int64  `json:"media_generation"`
	DeliveryID      string `json:"delivery_id"`
	ContentHash     string `json:"content_hash"`
	Text            string `json:"text"`
}
type ResultChunkReceipt struct {
	MediaGeneration int64  `json:"media_generation"`
	DeliveryID      string `json:"delivery_id"`
	ContentHash     string `json:"content_hash"`
	State           string `json:"state"`
}

// ResultReceiptAdapter is optional: the current /context route has no receipt support.
// Lookup must not resend. An attempted/unknown chunk may only advance on a matching applied receipt.
type ResultReceiptAdapter interface {
	Submit(context.Context, string, ResultChunkRequest) (ResultChunkReceipt, error)
	Lookup(context.Context, string, ResultChunkRequest) (ResultChunkReceipt, error)
}

// GatewayResultReceiptAdapter implements the proposed API, not a capability assertion.
// Main must opt in only after gateway capability verification; no fallback after Submit.
type GatewayResultReceiptAdapter struct{ Service *Service }

// ResultDeliveryConfig applies to result dispatch. Nil disables sending, retaining pending work.
type ResultDeliveryConfig struct{ ReceiptAdapter ResultReceiptAdapter }

// ResultDeliveryBaselineColumns documents existing integration dependencies (no ALTER required).
// Main adds Config.ResultDelivery ResultDeliveryConfig and stores ReceiptAdapter on Service;
// dispatch passes that adapter instead of nil to deliverCheckpointedResult.
const ResultDeliveryBaselineColumns = `CREATE TABLE voice_result_deliveries (
 entry_id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, task_id TEXT NOT NULL,
 run_id TEXT NOT NULL, subject_id TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('pending','delivered','failed','skipped_no_call','skipped_stale')),
 attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TEXT NOT NULL,
 last_error_code TEXT NOT NULL DEFAULT '', voice_session_id TEXT NOT NULL DEFAULT '',
 generation INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX voice_result_deliveries_due ON voice_result_deliveries(status,next_attempt_at,created_at);
CREATE TABLE voice_result_cursor (id INTEGER PRIMARY KEY CHECK(id=1), last_cursor INTEGER NOT NULL);
INSERT INTO voice_result_cursor(id,last_cursor) VALUES(1,0);
-- Existing voice_sessions dependencies: id,workspace_id,owner_id,state,gateway_session_id TEXT;
-- generation,gateway_media_generation INTEGER; created_at TEXT.
-- Additional tables: ResultCheckpointSchema.`

// NewGatewayResultReceiptAdapter constructs an opt-in proposed-API adapter; it does not probe support.
func NewGatewayResultReceiptAdapter(s *Service) ResultReceiptAdapter {
	return GatewayResultReceiptAdapter{Service: s}
}

// authoritativeResultPredicate applies whenever a delivery source is read or selected.
// Control commits these event identities with the assistant entry in the observation transaction.
const authoritativeResultPredicate = ` EXISTS(SELECT 1 FROM events a WHERE a.event_type='task.assistant_message' AND a.workspace_id=d.workspace_id AND a.aggregate_type='task' AND a.aggregate_id=d.task_id AND json_extract(a.payload,'$.payload.entry_id')=d.entry_id AND json_extract(a.payload,'$.payload.task_id')=d.task_id AND json_extract(a.payload,'$.payload.run_id')=d.run_id) `

func (a GatewayResultReceiptAdapter) request(ctx context.Context, gateway string, p ResultChunkRequest, lookup bool) (ResultChunkReceipt, error) {
	path := "/v1/voice/sessions/" + url.PathEscape(gateway) + "/result-deliveries"
	method := http.MethodPost
	body, _ := json.Marshal(p)
	if lookup {
		method = http.MethodGet
		body = nil
		path += "/" + url.PathEscape(p.DeliveryID)
	}
	var resp *http.Response
	var err error
	if lookup {
		u := *a.Service.base
		u.Path = strings.TrimRight(u.Path, "/") + path
		query := u.Query()
		query.Set("media_generation", strconv.FormatInt(p.MediaGeneration, 10))
		u.RawQuery = query.Encode()
		req, requestErr := http.NewRequestWithContext(ctx, method, u.String(), nil)
		if requestErr != nil {
			return ResultChunkReceipt{}, requestErr
		}
		req.Header.Set("Authorization", "Bearer "+a.Service.key)
		resp, err = a.Service.client.Do(req)
	} else {
		resp, _, err = a.Service.callGateway(ctx, method, path, body)
	}
	if resp != nil {
		defer resp.Body.Close()
	}
	var receipt ResultChunkReceipt
	if err != nil {
		return receipt, err
	}
	if resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return receipt, errors.New("result_receipt_unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(data) > 4096 {
		return receipt, errors.New("invalid_result_receipt")
	}
	err = json.Unmarshal(data, &receipt)
	return receipt, err
}
func (a GatewayResultReceiptAdapter) Submit(ctx context.Context, gateway string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	return a.request(ctx, gateway, p, false)
}
func (a GatewayResultReceiptAdapter) Lookup(ctx context.Context, gateway string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	return a.request(ctx, gateway, p, true)
}
func resultHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}
func receiptApplied(p ResultChunkRequest, r ResultChunkReceipt) bool {
	return r.State == "applied" && r.DeliveryID == p.DeliveryID && r.ContentHash == p.ContentHash && r.MediaGeneration == p.MediaGeneration
}

func (s *Service) deliverCheckpointedResult(ctx context.Context, id, workspace, task, run, owner string, attempts int, adapter ResultReceiptAdapter) {
	now := s.now().UTC()
	// Owner/task/run identity is checked against the durable source row, not caller trust.
	var text, session, gateway string
	var generation, media int64
	err := s.db.QueryRowContext(ctx, `SELECT e.content FROM voice_result_deliveries d JOIN task_entries e ON e.id=d.entry_id AND e.workspace_id=d.workspace_id AND e.task_id=d.task_id JOIN tasks t ON t.id=d.task_id AND t.workspace_id=d.workspace_id AND t.owner_subject_id=d.subject_id WHERE d.entry_id=? AND d.workspace_id=? AND d.task_id=? AND d.run_id=? AND d.subject_id=? AND d.status='pending' AND e.kind='assistant_message' AND `+authoritativeResultPredicate+` AND `+resultNotInOpening, id, workspace, task, run, owner).Scan(&text)
	if err != nil {
		return
	}
	if !utf8.ValidString(text) {
		s.finishResult(id, "failed", "invalid_utf8")
		return
	}
	if strings.TrimSpace(text) == "" {
		s.finishResult(id, "failed", "empty_result")
		return
	}
	err = s.db.QueryRowContext(ctx, `SELECT id,gateway_session_id,generation,gateway_media_generation FROM voice_sessions WHERE workspace_id=? AND owner_id=? AND state='active' AND gateway_session_id!='' AND (NOT EXISTS(SELECT 1 FROM voice_result_checkpoints WHERE entry_id=?) OR id=(SELECT session_id FROM voice_result_checkpoints WHERE entry_id=?)) ORDER BY created_at,id LIMIT 1`, workspace, owner, id, id).Scan(&session, &gateway, &generation, &media)
	if err == sql.ErrNoRows {
		_, _ = s.db.ExecContext(ctx, `UPDATE voice_result_deliveries AS d SET next_attempt_at=?,last_error_code='awaiting_context',updated_at=? WHERE entry_id=? AND status='pending' AND attempts=? AND `+resultNotInOpening, now.Add(resultDeliveryLease).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), id, attempts)
		return
	}
	if err != nil || (adapter != nil && media <= 0) {
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_result_checkpoints(entry_id,workspace_id,task_id,run_id,subject_id,content_hash,session_id,generation,gateway_session_id,media_generation) VALUES(?,?,?,?,?,?,?,?,?,?)`, id, workspace, task, run, owner, resultHash(text), session, generation, gateway, media)
	if err != nil {
		return
	}
	res, err := tx.ExecContext(ctx, `UPDATE voice_result_checkpoints SET claim=claim+1,lease_until=? WHERE entry_id=? AND workspace_id=? AND task_id=? AND run_id=? AND subject_id=? AND content_hash=? AND session_id=? AND generation=? AND gateway_session_id=? AND media_generation=? AND lease_until<=?`, now.Add(resultDeliveryLease).Format(time.RFC3339Nano), id, workspace, task, run, owner, resultHash(text), session, generation, gateway, media, now.Format(time.RFC3339Nano))
	if err != nil {
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return
	}
	var claim int64
	if tx.QueryRowContext(ctx, `SELECT claim FROM voice_result_checkpoints WHERE entry_id=?`, id).Scan(&claim) != nil {
		return
	}
	remaining := text
	for ordinal := 0; len(remaining) > 0; ordinal++ {
		n := min(limits.GatewayTextChunkBytes, len(remaining))
		for n < len(remaining) && !utf8.RuneStart(remaining[n]) {
			n--
		}
		chunk := remaining[:n]
		remaining = remaining[n:]
		identity, _ := json.Marshal([]any{workspace, owner, task, run, id, session, generation, gateway, media, ordinal, resultHash(chunk)})
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_result_chunks(entry_id,ordinal,delivery_id,content_hash,text,state) VALUES(?,?,?,?,?,'pending')`, id, ordinal, resultHash(string(identity)), resultHash(chunk), chunk); err != nil {
			return
		}
	}
	res, err = tx.ExecContext(ctx, `UPDATE voice_result_deliveries AS d SET attempts=attempts+1,next_attempt_at=?,updated_at=? WHERE entry_id=? AND status='pending' AND attempts=? AND next_attempt_at<=? AND last_error_code!='awaiting_context' AND `+resultNotInOpening, now.Add(resultDeliveryLease).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), id, attempts, now.Format(time.RFC3339Nano))
	if err != nil {
		return
	}
	n, _ = res.RowsAffected()
	if n != 1 {
		return
	}
	if tx.Commit() != nil {
		return
	}
	for ordinal := 0; ; ordinal++ {
		var p ResultChunkRequest
		p.MediaGeneration = media
		var state string
		err = s.db.QueryRowContext(ctx, `SELECT delivery_id,content_hash,text,state FROM voice_result_chunks WHERE entry_id=? AND ordinal=?`, id, ordinal).Scan(&p.DeliveryID, &p.ContentHash, &p.Text, &state)
		if err == sql.ErrNoRows {
			completed := s.now().UTC()
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return
			}
			defer tx.Rollback()
			res, err := tx.ExecContext(ctx, `UPDATE voice_result_deliveries AS d SET status='delivered',voice_session_id=?,generation=?,last_error_code='gateway_accepted',updated_at=? WHERE entry_id=? AND status='pending' AND EXISTS(SELECT 1 FROM voice_result_checkpoints WHERE entry_id=? AND claim=? AND lease_until>?) AND NOT EXISTS(SELECT 1 FROM voice_result_chunks WHERE entry_id=? AND state!='confirmed') AND `+resultNotInOpening, session, generation, completed.Format(time.RFC3339Nano), id, id, claim, completed.Format(time.RFC3339Nano), id)
			if err != nil {
				return
			}
			n, err := res.RowsAffected()
			if err != nil || n != 1 {
				return
			}
			if recordContextEvent(ctx, tx, contextEvent{workspace: workspace, owner: owner, session: session, kind: "task_result", id: id, role: "background", text: text, at: completed}) != nil {
				return
			}
			_ = tx.Commit()
			return
		}
		if err != nil {
			return
		}
		if state == "confirmed" {
			continue
		}
		// Fenced persisted attempt precedes every external send. Expiry never authorizes replay.
		res, err = s.db.ExecContext(ctx, `UPDATE voice_result_chunks AS d SET state='attempted' WHERE entry_id=? AND ordinal=? AND state=? AND `+resultNotInOpening+` AND EXISTS(SELECT 1 FROM voice_result_checkpoints c JOIN voice_sessions v ON v.id=c.session_id AND v.workspace_id=c.workspace_id AND v.owner_id=c.subject_id AND v.generation=c.generation AND v.gateway_session_id=c.gateway_session_id AND v.gateway_media_generation=c.media_generation AND v.state='active' WHERE c.entry_id=? AND c.claim=? AND c.lease_until>?)`, id, ordinal, state, id, claim, s.now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return
		}
		n, _ = res.RowsAffected()
		if n != 1 {
			return
		}
		applied := false
		if adapter != nil {
			var receipt ResultChunkReceipt
			if state == "pending" {
				receipt, err = adapter.Submit(ctx, gateway, p)
			} else {
				receipt, err = adapter.Lookup(ctx, gateway, p)
			}
			applied = err == nil && receiptApplied(p, receipt)
		} else if state == "pending" {
			// Legacy HTTP success proves only gateway acceptance. Any failure is ambiguous.
			// Legacy /context cannot prove acceptance or reconcile an uncertain
			// prefix. A missing capability never authorizes an unreceipted send.
			applied = false
		}
		next := "unknown"
		if applied {
			next = "confirmed"
		}
		res, err = s.db.ExecContext(ctx, `UPDATE voice_result_chunks SET state=? WHERE entry_id=? AND ordinal=? AND state='attempted' AND EXISTS(SELECT 1 FROM voice_result_checkpoints WHERE entry_id=? AND claim=? AND lease_until>?)`, next, id, ordinal, id, claim, s.now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return
		}
		n, _ = res.RowsAffected()
		if n != 1 || !applied {
			return
		}
	}
}

// selectPendingResultTx binds an existing authoritative source and clears its deferral.
// A same-destination selection is idempotent and retains all checkpoints/leases.
// Rebinding requires every chunk pending and no unexpired claim; sent/ambiguous history is immutable.
func (s *Service) selectPendingResultTx(ctx context.Context, tx *sql.Tx, principal contracts.Principal, entry, voiceSessionID string, generation int64) error {
	var task, run, text, status string
	err := tx.QueryRowContext(ctx, `SELECT d.task_id,d.run_id,e.content,d.status FROM voice_result_deliveries d JOIN tasks t ON t.id=d.task_id AND t.workspace_id=d.workspace_id AND t.owner_subject_id=d.subject_id JOIN task_entries e ON e.id=d.entry_id AND e.workspace_id=d.workspace_id AND e.task_id=d.task_id AND e.kind='assistant_message' WHERE d.entry_id=? AND d.workspace_id=? AND d.subject_id=? AND `+authoritativeResultPredicate+` AND `+resultNotInOpening, entry, principal.WorkspaceID, principal.SubjectID).Scan(&task, &run, &text, &status)
	if err != nil {
		return err
	}
	var gateway string
	var media int64
	err = tx.QueryRowContext(ctx, `SELECT gateway_session_id,gateway_media_generation FROM voice_sessions WHERE id=? AND workspace_id=? AND owner_id=? AND generation=? AND state='active' AND gateway_session_id!=''`, voiceSessionID, principal.WorkspaceID, principal.SubjectID, generation).Scan(&gateway, &media)
	if err != nil {
		return err
	}
	var oldSession, oldGateway, lease, hash string
	var oldGeneration, oldMedia int64
	err = tx.QueryRowContext(ctx, `SELECT session_id,generation,gateway_session_id,media_generation,lease_until,content_hash FROM voice_result_checkpoints WHERE entry_id=?`, entry).Scan(&oldSession, &oldGeneration, &oldGateway, &oldMedia, &lease, &hash)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if err == nil {
		if hash != resultHash(text) {
			return errors.New("result_source_changed")
		}
		same := oldSession == voiceSessionID && oldGeneration == generation && oldGateway == gateway && oldMedia == media
		if !same {
			var sent int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM voice_result_chunks WHERE entry_id=? AND state!='pending'`, entry).Scan(&sent); err != nil {
				return err
			}
			if sent != 0 || lease > now || status != "pending" {
				return errors.New("result_destination_fenced")
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM voice_result_chunks WHERE entry_id=?`, entry); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE voice_result_checkpoints SET session_id=?,generation=?,gateway_session_id=?,media_generation=?,claim=claim+1,lease_until='' WHERE entry_id=?`, voiceSessionID, generation, gateway, media, entry); err != nil {
				return err
			}
		}
	} else {
		if status != "pending" {
			return errors.New("result_not_pending")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO voice_result_checkpoints(entry_id,workspace_id,task_id,run_id,subject_id,content_hash,session_id,generation,gateway_session_id,media_generation) VALUES(?,?,?,?,?,?,?,?,?,?)`, entry, principal.WorkspaceID, task, run, principal.SubjectID, resultHash(text), voiceSessionID, generation, gateway, media); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE voice_result_deliveries SET last_error_code='',next_attempt_at=?,updated_at=? WHERE entry_id=? AND status='pending' AND last_error_code='awaiting_context'`, now, now, entry); err != nil {
		return err
	}
	return nil
}
