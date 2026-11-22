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
	rows, err := s.db.QueryContext(ctx, `SELECT d.request_id,d.workspace_id,d.owner_subject_id,d.text,d.attempts FROM ui_feedback_deliveries d JOIN ui_requests r ON r.id=d.request_id WHERE d.status='pending' AND coalesce(d.next_attempt_at,'')<=? AND r.status IN ('failed','rendered') ORDER BY d.created_at,d.request_id LIMIT ?`, s.now().UTC().Format(time.RFC3339Nano), s.limits.Int("voice_feedback_sweep_rows"))
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
	unlock := s.lockUIOwner(d.workspace, d.owner)
	defer unlock()
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
	// Revalidate after selection and session lookup, immediately before HTTP.
	var eligible bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ui_requests r JOIN ui_feedback_deliveries d ON d.request_id=r.id WHERE r.id=? AND r.workspace_id=? AND r.owner_subject_id=? AND d.status='pending' AND d.attempts=? AND r.status IN ('failed','rendered'))`, d.id, d.workspace, d.owner, attempt).Scan(&eligible)
	if err != nil {
		s.retryUIFeedback(ctx, d, attempt, "eligibility_lookup")
		return
	}
	if !eligible {
		s.finishUIFeedback(ctx, d, attempt, "presentation_superseded")
		return
	}
	ok, code := s.deliverDurableText(ctx, d.workspace, d.owner, "ui_feedback", d.id, session, generation, d.text)
	if !ok {
		s.deferUIFeedback(ctx, d, attempt, code)
		return
	}
	completed := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	r, err = tx.ExecContext(ctx, `UPDATE ui_feedback_deliveries SET status='delivered',last_error=NULL,updated_at=? WHERE request_id=? AND status='pending' AND attempts=?`, completed.Format(time.RFC3339Nano), d.id, attempt)
	if err != nil {
		return
	}
	n, _ = r.RowsAffected()
	if n != 1 {
		return
	}
	if recordContextEvent(ctx, tx, contextEvent{workspace: d.workspace, owner: d.owner, session: session, kind: "ui_feedback", id: d.id, role: "background", text: d.text, at: completed}) != nil {
		return
	}
	// Transport delivery state never changes render/failure evidence on the request.
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
