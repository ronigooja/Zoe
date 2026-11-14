package voice

import (
	"context"
	"database/sql"
	"time"
)

type uiFeedback struct {
	id, workspace, owner, text string
	attempts                   int
}

func (s *Service) dispatchUIFeedback(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT request_id,workspace_id,owner_subject_id,text,attempts FROM ui_feedback_deliveries WHERE status='pending' AND coalesce(next_attempt_at,'')<=? ORDER BY created_at,request_id LIMIT 25`, s.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return
	}
	var batch []uiFeedback
	for rows.Next() {
		var d uiFeedback
		if rows.Scan(&d.id, &d.workspace, &d.owner, &d.text, &d.attempts) == nil {
			batch = append(batch, d)
		}
	}
	rows.Close()
	for _, d := range batch {
		s.deliverUIFeedback(ctx, d)
	}
}

func (s *Service) deliverUIFeedback(ctx context.Context, d uiFeedback) {
	now := s.now().UTC()
	at := now.Format(time.RFC3339Nano)
	r, err := s.db.ExecContext(ctx, `UPDATE ui_feedback_deliveries SET attempts=attempts+1,next_attempt_at=?,updated_at=? WHERE request_id=? AND status='pending' AND attempts=? AND coalesce(next_attempt_at,'')<=?`, now.Add(30*time.Second).Format(time.RFC3339Nano), at, d.id, d.attempts, at)
	if err != nil {
		return
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return
	}
	attempt := d.attempts + 1
	var session, gateway string
	var generation, current int64
	err = s.db.QueryRowContext(ctx, `SELECT id,gateway_session_id,generation FROM voice_sessions WHERE workspace_id=? AND owner_id=? AND state='active' LIMIT 1`, d.workspace, d.owner).Scan(&session, &gateway, &generation)
	if err == sql.ErrNoRows || (err == nil && gateway == "") {
		s.deferUIFeedback(ctx, d, attempt, "no_active_call")
		return
	}
	if err != nil {
		s.retryUIFeedback(ctx, d, attempt, "session_lookup")
		return
	}
	if err = s.db.QueryRowContext(ctx, `SELECT generation FROM voice_sessions WHERE id=? AND workspace_id=? AND owner_id=? AND state='active'`, session, d.workspace, d.owner).Scan(&current); err != nil || current != generation {
		s.deferUIFeedback(ctx, d, attempt, "stale_generation")
		return
	}
	if attempt > 3 {
		s.finishUIFeedback(ctx, d, attempt, "gateway_failed")
		return
	}
	ok, code := s.appendContext(ctx, gateway, d.text)
	if !ok {
		if code == "gateway_failed" {
			s.retryUIFeedback(ctx, d, attempt, code)
		} else {
			s.finishUIFeedback(ctx, d, attempt, code)
		}
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	r, err = tx.ExecContext(ctx, `UPDATE ui_feedback_deliveries SET status='delivered',last_error=NULL,updated_at=? WHERE request_id=? AND status='pending' AND attempts=?`, s.now().UTC().Format(time.RFC3339Nano), d.id, attempt)
	if err != nil {
		return
	}
	n, _ = r.RowsAffected()
	if n != 1 {
		return
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ui_requests SET status='feedback_delivered',updated_at=? WHERE id=?`, s.now().UTC().Format(time.RFC3339Nano), d.id); err != nil {
		return
	}
	_ = tx.Commit()
}

func (s *Service) deferUIFeedback(ctx context.Context, d uiFeedback, attempt int, code string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE ui_feedback_deliveries SET attempts=attempts-1,next_attempt_at=?,last_error=?,updated_at=? WHERE request_id=? AND status='pending' AND attempts=?`, s.now().Add(30*time.Second).UTC().Format(time.RFC3339Nano), code, s.now().UTC().Format(time.RFC3339Nano), d.id, attempt)
}
func (s *Service) retryUIFeedback(ctx context.Context, d uiFeedback, attempt int, code string) {
	if attempt >= 3 {
		s.finishUIFeedback(ctx, d, attempt, code)
		return
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE ui_feedback_deliveries SET next_attempt_at=?,last_error=?,updated_at=? WHERE request_id=? AND status='pending' AND attempts=?`, s.now().Add(time.Duration(1<<uint(attempt-1))*time.Second).UTC().Format(time.RFC3339Nano), code, s.now().UTC().Format(time.RFC3339Nano), d.id, attempt)
}
func (s *Service) finishUIFeedback(ctx context.Context, d uiFeedback, attempt int, code string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE ui_feedback_deliveries SET status='failed',last_error=?,updated_at=? WHERE request_id=? AND status='pending' AND attempts=?`, code, s.now().UTC().Format(time.RFC3339Nano), d.id, attempt)
}
