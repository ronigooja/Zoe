package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

const resultDeliveryMaxAttempts = 3
const fastReplyLease = 30 * time.Second

// resultDeliveryLease bounds how long a claimed delivery is hidden from other dispatchers.
const resultDeliveryLease = 30 * time.Second

// appendContext is an explicit unsupported legacy boundary. Persisted callers
// must use deliverDurableText with a stable source identity.
func (s *Service) appendContext(ctx context.Context, gateway, text string) (bool, string) {
	return false, "receipt_identity_required"
}

func (s *Service) resultDispatcher() {
	t := time.NewTicker(s.resultDeliveryInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.classifyCommittedTurns(s.recallCtx)
			s.createRegisterUIJobs(s.recallCtx)
			s.consumeUITaskEvents(s.recallCtx)
			s.dispatchUIJobs(s.recallCtx)
			s.dispatchUIFeedback(s.recallCtx)
			s.consumeResultEvents(s.recallCtx)
			s.dispatchResults(s.recallCtx)
			s.dispatchFastReplies(s.recallCtx)
		case <-s.reaperStop:
			return
		}
	}
}

func (s *Service) classifyCommittedTurns(ctx context.Context) {
	if s.register == nil {
		return
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,content FROM voice_turns WHERE exits IS NULL ORDER BY confirmed_at LIMIT 25`)
	if err != nil {
		return
	}
	type turn struct{ id, content string }
	var turns []turn
	for rows.Next() {
		var t turn
		if rows.Scan(&t.id, &t.content) == nil {
			turns = append(turns, t)
		}
	}
	rows.Close()
	for _, t := range turns {
		s.classifyCommittedTurn(t.id)
	}
}

func (s *Service) ensureClassified(ctx context.Context, workspace, session string, generation int64, turn string, revision int64) string {
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		var category string
		if err := s.db.QueryRowContext(ctx, `SELECT coalesce(exits,'') FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, workspace, session, generation, turn, revision).Scan(&category); err == nil && category != "" {
			return category
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(5 * time.Millisecond):
		}
	}
	return ""
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
	rows, err := tx.QueryContext(ctx, `SELECT cursor,event_id,workspace_id,aggregate_id,payload FROM events WHERE event_type='task.assistant_message' AND cursor>? ORDER BY cursor LIMIT 25`, last)
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
	rows, err := s.db.QueryContext(ctx, `SELECT entry_id,workspace_id,task_id,run_id,subject_id,attempts FROM voice_result_deliveries WHERE status='pending' AND last_error_code!='awaiting_context' AND next_attempt_at<=? ORDER BY next_attempt_at LIMIT 25`, now)
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

func (s *Service) dispatchFastReplies(ctx context.Context) {
	now := s.now().UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `SELECT id,workspace_id,session_id,generation,text,intent_kind,attempts FROM voice_fast_replies WHERE status='pending' AND next_attempt_at<=? ORDER BY created_at LIMIT 25`, now)
	if err != nil {
		return
	}
	type due struct {
		id, workspace, session, text, intent string
		generation                           int64
		attempts                             int
	}
	var batch []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.id, &d.workspace, &d.session, &d.generation, &d.text, &d.intent, &d.attempts) == nil {
			batch = append(batch, d)
		}
	}
	rows.Close()
	for _, d := range batch {
		s.deliverFastReply(ctx, d.id, d.workspace, d.session, d.generation, d.text, d.intent, d.attempts)
	}
	s.dispatchRecall(ctx)
}

func (s *Service) deliverFastReply(ctx context.Context, id, workspace, session string, generation int64, text, intent string, attempts int) {
	now := s.now().UTC()
	// Reserve dispatch without spending a retry before a validated external attempt.
	// The checkpoint claim fences network work; this token fences scheduler updates.
	token := now.Add(fastReplyLease).Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `UPDATE voice_fast_replies SET next_attempt_at=?,updated_at=? WHERE id=? AND workspace_id=? AND session_id=? AND generation=? AND status='pending' AND attempts=? AND next_attempt_at<=?`, token, now.Format(time.RFC3339Nano), id, workspace, session, generation, attempts, now.Format(time.RFC3339Nano))
	if err != nil {
		return
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return
	}
	var owner string
	err = s.db.QueryRowContext(ctx, `SELECT owner_id FROM voice_sessions v WHERE v.id=? AND v.workspace_id=? AND v.generation=? AND v.state='active' AND v.gateway_session_id!='' AND v.gateway_media_generation>0 AND NOT EXISTS(SELECT 1 FROM voice_text_checkpoints c WHERE c.workspace_id=v.workspace_id AND c.source_kind='fast_reply' AND c.source_id=? AND c.owner_id!=v.owner_id)`, session, workspace, generation, id).Scan(&owner)
	ok, code := false, "awaiting_context"
	if err == nil {
		ok, code = s.deliverDurableText(ctx, workspace, owner, "fast_reply", id, session, generation, text)
	} else if err != sql.ErrNoRows {
		code = "session_lookup"
	}
	status := "pending"
	if ok {
		status = "delivered"
	}
	if code == "receipt_rejected" {
		status = "failed"
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE voice_fast_replies SET status=?,voice_session_id=CASE WHEN ? THEN ? ELSE voice_session_id END,voice_generation=CASE WHEN ? THEN ? ELSE voice_generation END,last_error_code=?,updated_at=? WHERE id=? AND workspace_id=? AND session_id=? AND generation=? AND status='pending' AND attempts=? AND next_attempt_at=?`, status, ok, session, ok, generation, code, s.now().UTC().Format(time.RFC3339Nano), id, workspace, session, generation, attempts, token)

}

func (s *Service) finishFastReply(id, status, code string) {
	_, _ = s.db.Exec(`UPDATE voice_fast_replies SET status=?,last_error_code=?,updated_at=? WHERE id=? AND status='pending'`, status, code, s.now().UTC().Format(time.RFC3339Nano), id)
}
func (s *Service) retryFastReply(id string, attempt int, code string) {
	if attempt >= resultDeliveryMaxAttempts {
		s.finishFastReply(id, "failed", code)
		return
	}
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	_, _ = s.db.Exec(`UPDATE voice_fast_replies SET next_attempt_at=?,last_error_code=?,updated_at=? WHERE id=? AND status='pending'`, s.now().Add(d).UTC().Format(time.RFC3339Nano), code, s.now().UTC().Format(time.RFC3339Nano), id)
}

func (s *Service) deliverResult(ctx context.Context, id, workspace, task, run, subject string, attempts int) {
	if s.resultReceiptAdapter == nil {
		_, _ = s.db.ExecContext(ctx, `UPDATE voice_result_deliveries SET last_error_code=CASE WHEN last_error_code='awaiting_context' THEN last_error_code ELSE 'receipt_capability_disabled' END,next_attempt_at=?,updated_at=? WHERE entry_id=? AND status='pending'`, s.now().Add(resultDeliveryLease).UTC().Format(time.RFC3339Nano), s.now().UTC().Format(time.RFC3339Nano), id)
		return
	}
	s.deliverCheckpointedResult(ctx, id, workspace, task, run, subject, attempts, s.resultReceiptAdapter)
}

func (s *Service) finishResult(id, status, code string) {
	_, _ = s.db.Exec(`UPDATE voice_result_deliveries SET status=?,last_error_code=?,updated_at=? WHERE entry_id=? AND status='pending'`, status, code, s.now().UTC().Format(time.RFC3339Nano), id)
}
func (s *Service) retryResult(id string, attempt int, code string) {
	if attempt >= resultDeliveryMaxAttempts {
		s.finishResult(id, "failed", code)
		return
	}
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	_, _ = s.db.Exec(`UPDATE voice_result_deliveries SET next_attempt_at=?,last_error_code=?,updated_at=? WHERE entry_id=? AND status='pending'`, s.now().Add(d).UTC().Format(time.RFC3339Nano), code, s.now().UTC().Format(time.RFC3339Nano), id)
}
