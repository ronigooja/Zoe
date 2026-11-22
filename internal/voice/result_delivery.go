package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

const fastReplyLease = 30 * time.Second

// resultDeliveryLease bounds how long a claimed delivery is hidden from other dispatchers.
const resultDeliveryLease = 30 * time.Second

func (s *Service) resultDispatcher() {
	t := time.NewTicker(s.resultDeliveryInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.consumeUITaskEvents(s.recallCtx)
			s.dispatchUIJobs(s.recallCtx)
			s.dispatchUIFeedback(s.recallCtx)
			s.dispatchVoiceInputs(s.recallCtx)
			s.consumeResultEvents(s.recallCtx)
			s.dispatchResults(s.recallCtx)
			s.dispatchFastReplies(s.recallCtx)
		case <-s.recallCtx.Done():
			return
		}
	}
}

// consumeResultEvents is the voice-owned boundary between control events and delivery rows.
// Unknown tasks are durably recorded as failed so the cursor can advance without blocking later events.
func (s *Service) consumeResultEvents(ctx context.Context) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	var last int64
	if err = tx.QueryRowContext(ctx, `SELECT last_cursor FROM voice_result_cursor WHERE id=1`).Scan(&last); err != nil {
		return
	}
	rows, err := tx.QueryContext(ctx, `SELECT cursor,event_id,workspace_id,aggregate_id,payload FROM events WHERE event_type='task.assistant_message' AND cursor>? ORDER BY cursor LIMIT ?`, last, s.limits.Int("voice_result_sweep_rows"))
	if err != nil {
		return
	}
	type event struct {
		cursor                               int64
		eventID, workspace, task, entry, run string
	}
	var events []event
	for rows.Next() {
		var e event
		var payload []byte
		if rows.Scan(&e.cursor, &e.eventID, &e.workspace, &e.task, &payload) != nil {
			rows.Close()
			return
		}
		var p struct {
			Payload struct {
				EntryID string `json:"entry_id"`
				RunID   string `json:"run_id"`
				TaskID  string `json:"task_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(payload, &p) != nil {
			rows.Close()
			return
		}
		e.entry, e.run = p.Payload.EntryID, p.Payload.RunID
		if p.Payload.TaskID != "" {
			e.task = p.Payload.TaskID
		}
		events = append(events, e)
	}
	rows.Close()
	for _, e := range events {
		var subject string
		err := tx.QueryRowContext(ctx, `SELECT owner_subject_id FROM tasks WHERE id=? AND workspace_id=?`, e.task, e.workspace).Scan(&subject)
		at := s.now().UTC().Format(time.RFC3339Nano)
		if err == sql.ErrNoRows {
			_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_result_deliveries(entry_id,workspace_id,task_id,run_id,subject_id,status,last_error_code,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,'','failed','task_unknown',?,?,?)`, e.entry, e.workspace, e.task, e.run, at, at, at)
		} else if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_result_deliveries(entry_id,workspace_id,task_id,run_id,subject_id,status,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,'pending',?,?,?)`, e.entry, e.workspace, e.task, e.run, subject, at, at, at)
		}
		if err != nil {
			return
		}
		if _, err = tx.ExecContext(ctx, `UPDATE voice_result_cursor SET last_cursor=? WHERE id=1 AND last_cursor<?`, e.cursor, e.cursor); err != nil {
			return
		}
	}
	if err = tx.Commit(); err != nil {
		return
	}
}

func (s *Service) dispatchResults(ctx context.Context) {
	now := s.now().UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `SELECT entry_id,workspace_id,task_id,run_id,subject_id,attempts FROM voice_result_deliveries d WHERE status='pending' AND last_error_code!='awaiting_context' AND next_attempt_at<=? AND `+resultNotInOpening+` ORDER BY next_attempt_at LIMIT ?`, now, s.limits.Int("voice_result_sweep_rows"))
	if err != nil {
		return
	}
	// Collect due rows and release the cursor before delivering: deliverResult
	// writes through the same pool, and an open result set would hold the only
	// SQLite connection and deadlock the dispatcher.
	type due struct {
		id, workspace, task, run, subject string
		attempts                          int
	}
	var batch []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.id, &d.workspace, &d.task, &d.run, &d.subject, &d.attempts) == nil {
			batch = append(batch, d)
		}
	}
	rows.Close()
	for _, d := range batch {
		s.deliverResult(ctx, d.id, d.workspace, d.task, d.run, d.subject, d.attempts)
	}
}

// A stranded lease from a closed conversation must not delay the new call.
// This exception stops applying as soon as its checkpoint moves.
const fastReplyOldBinding = ` EXISTS(SELECT 1 FROM voice_text_checkpoints c WHERE c.workspace_id=f.workspace_id AND c.owner_id=o.owner_id AND c.source_kind='fast_reply' AND c.source_id=f.id AND (c.session_id!=v.id OR c.generation!=v.generation)) `

func (s *Service) dispatchFastReplies(ctx context.Context) {
	now := s.now().UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `SELECT f.id,f.workspace_id,f.session_id,f.generation,f.attempts FROM voice_fast_replies f JOIN voice_sessions o ON o.id=f.session_id AND o.workspace_id=f.workspace_id AND o.generation=f.generation JOIN voice_sessions v ON v.workspace_id=o.workspace_id AND v.owner_id=o.owner_id AND v.state='active' AND v.gateway_session_id!='' AND v.gateway_media_generation>0 WHERE f.status='pending' AND (f.next_attempt_at<=? OR `+fastReplyOldBinding+`) AND f.intent_kind NOT IN ('recall_holding','memory_write_holding') AND trim(f.text,char(9,10,11,12,13,32))!='' ORDER BY f.created_at,f.id LIMIT ?`, now, s.limits.Int("voice_fast_reply_sweep_rows"))
	if err != nil {
		return
	}
	type due struct {
		id, workspace, session string
		generation             int64
		attempts               int
	}
	var batch []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.id, &d.workspace, &d.session, &d.generation, &d.attempts) == nil {
			batch = append(batch, d)
		}
	}
	rows.Close()
	for _, d := range batch {
		s.deliverFastReply(ctx, d.id, d.workspace, d.session, d.generation, d.attempts)
	}
	s.dispatchRecall(ctx)
}

func (s *Service) deliverFastReply(ctx context.Context, id, workspace, session string, generation int64, attempts int) {
	now := s.now().UTC()
	// Reserve dispatch without spending a retry before a validated external attempt.
	// The checkpoint claim fences network work; this token fences scheduler updates.
	token := now.Add(fastReplyLease).Format(time.RFC3339Nano)
	destination, text, code, err := s.claimFastReply(ctx, id, workspace, session, generation, attempts, now, token)
	if err != nil {
		return
	}
	ok := false
	if destination.session != "" {
		ok, code = s.deliverDurableText(ctx, workspace, destination.owner, "fast_reply", id, destination.session, destination.generation, text)
	}
	status := "pending"
	if ok {
		status = "delivered"
	}
	if code == "receipt_rejected" {
		status = "failed"
	}
	completed := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE voice_fast_replies AS f SET status=?,voice_session_id=CASE WHEN ? THEN ? ELSE voice_session_id END,voice_generation=CASE WHEN ? THEN ? ELSE voice_generation END,last_error_code=?,updated_at=? WHERE id=? AND workspace_id=? AND session_id=? AND generation=? AND status='pending' AND attempts=? AND next_attempt_at=? AND NOT EXISTS(SELECT 1 FROM voice_text_checkpoints c WHERE c.workspace_id=f.workspace_id AND c.owner_id=? AND c.source_kind='fast_reply' AND c.source_id=f.id AND (c.session_id!=? OR c.generation!=?))`, status, ok, destination.session, ok, destination.generation, code, completed.Format(time.RFC3339Nano), id, workspace, session, generation, attempts, token, destination.owner, destination.session, destination.generation)
	if err != nil {
		return
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return
	}
	if ok && recordContextEvent(ctx, tx, contextEvent{workspace: workspace, owner: destination.owner, session: destination.session, kind: "fast_reply", id: id, role: "background", text: text, at: completed}) != nil {
		return
	}
	if ok && s.releaseReportResultsTx(ctx, tx, id, workspace, destination, completed) != nil {
		return
	}
	_ = tx.Commit()
}

type fastReplyDestination struct {
	owner, session, gateway string
	generation, media       int64
}

// The text checkpoint is the destination binding; the reply row keeps its
// immutable origin. Reserve dispatch and move an old checkpoint atomically.
func (s *Service) claimFastReply(ctx context.Context, id, workspace, session string, generation int64, attempts int, now time.Time, token string) (fastReplyDestination, string, string, error) {
	var dest fastReplyDestination
	var text string
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dest, "", "", err
	}
	defer tx.Rollback()
	stamp := now.Format(time.RFC3339Nano)
	err = tx.QueryRowContext(ctx, `UPDATE voice_fast_replies AS f SET next_attempt_at=?,updated_at=? WHERE id=? AND workspace_id=? AND session_id=? AND generation=? AND status='pending' AND attempts=? AND intent_kind NOT IN ('recall_holding','memory_write_holding') AND (next_attempt_at<=? OR EXISTS(SELECT 1 FROM voice_sessions o JOIN voice_sessions v ON v.workspace_id=o.workspace_id AND v.owner_id=o.owner_id AND v.state='active' AND v.gateway_session_id!='' AND v.gateway_media_generation>0 WHERE o.id=f.session_id AND o.workspace_id=f.workspace_id AND o.generation=f.generation AND `+fastReplyOldBinding+`)) RETURNING text`, token, stamp, id, workspace, session, generation, attempts, stamp).Scan(&text)
	if err != nil {
		return dest, "", "", err
	}
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
		return dest, "", "", sql.ErrNoRows
	}
	err = tx.QueryRowContext(ctx, `SELECT v.owner_id,v.id,v.generation,v.gateway_session_id,v.gateway_media_generation FROM voice_sessions o JOIN voice_sessions v ON v.workspace_id=o.workspace_id AND v.owner_id=o.owner_id WHERE o.id=? AND o.workspace_id=? AND o.generation=? AND v.state='active' AND v.gateway_session_id!='' AND v.gateway_media_generation>0 AND NOT EXISTS(SELECT 1 FROM voice_text_checkpoints c WHERE c.workspace_id=o.workspace_id AND c.source_kind='fast_reply' AND c.source_id=? AND c.owner_id!=o.owner_id) ORDER BY v.created_at,v.id LIMIT 1`, session, workspace, generation, id).Scan(&dest.owner, &dest.session, &dest.generation, &dest.gateway, &dest.media)
	if err == sql.ErrNoRows {
		return dest, text, "awaiting_context", tx.Commit()
	}
	if err != nil {
		return dest, "", "", err
	}
	if err := rebindFastReplyCheckpoint(ctx, tx, workspace, id, resultHash(text), dest); err != nil {
		return dest, "", "", err
	}
	return dest, text, "", tx.Commit()
}

func (s *Service) finishFastReply(id, status, code string) {
	_, _ = s.db.Exec(`UPDATE voice_fast_replies SET status=?,last_error_code=?,updated_at=? WHERE id=? AND status='pending'`, status, code, s.now().UTC().Format(time.RFC3339Nano), id)
}
func (s *Service) retryFastReply(id string, attempt int, code string) {
	if attempt >= s.limits.Int("voice_delivery_attempts") {
		s.finishFastReply(id, "failed", code)
		return
	}
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	_, _ = s.db.Exec(`UPDATE voice_fast_replies SET next_attempt_at=?,last_error_code=?,updated_at=? WHERE id=? AND status='pending'`, s.now().Add(d).UTC().Format(time.RFC3339Nano), code, s.now().UTC().Format(time.RFC3339Nano), id)
}

func (s *Service) deliverResult(ctx context.Context, id, workspace, task, run, subject string, attempts int) {
	adapter := s.receiptAdapter(ctx)
	if adapter == nil {
		_, _ = s.db.ExecContext(ctx, `UPDATE voice_result_deliveries AS d SET last_error_code=CASE WHEN last_error_code='awaiting_context' THEN last_error_code ELSE 'receipt_capability_disabled' END,next_attempt_at=?,updated_at=? WHERE entry_id=? AND status='pending' AND `+resultNotInOpening, s.now().Add(resultDeliveryLease).UTC().Format(time.RFC3339Nano), s.now().UTC().Format(time.RFC3339Nano), id)
		return
	}
	s.deliverCheckpointedResult(ctx, id, workspace, task, run, subject, attempts, adapter)
}

func (s *Service) finishResult(id, status, code string) {
	_, _ = s.db.Exec(`UPDATE voice_result_deliveries AS d SET status=?,last_error_code=?,updated_at=? WHERE entry_id=? AND status='pending' AND `+resultNotInOpening, status, code, s.now().UTC().Format(time.RFC3339Nano), id)
}
func (s *Service) retryResult(id string, attempt int, code string) {
	if attempt >= s.limits.Int("voice_delivery_attempts") {
		s.finishResult(id, "failed", code)
		return
	}
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	_, _ = s.db.Exec(`UPDATE voice_result_deliveries AS d SET next_attempt_at=?,last_error_code=?,updated_at=? WHERE entry_id=? AND status='pending' AND `+resultNotInOpening, s.now().Add(d).UTC().Format(time.RFC3339Nano), code, s.now().UTC().Format(time.RFC3339Nano), id)
}
