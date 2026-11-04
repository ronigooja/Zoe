package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

const resultDeliveryMaxAttempts = 3

func boundedResultText(text string) string {
	var b strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) {
			r = ' '
		}
		b.WriteRune(r)
	}
	text = strings.Join(strings.Fields(b.String()), " ")
	r := []rune(text)
	if len(r) > 600 {
		r = r[:600]
	}
	return strings.TrimSpace(string(r))
}

func (s *Service) resultDispatcher() {
	t := time.NewTicker(s.resultDeliveryInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.dispatchResults(context.Background())
		case <-s.reaperStop:
			return
		}
	}
}

func (s *Service) dispatchResults(ctx context.Context) {
	now := s.now().UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `SELECT entry_id,workspace_id,task_id,run_id,subject_id,attempts FROM voice_result_deliveries WHERE status='pending' AND next_attempt_at<=? ORDER BY next_attempt_at LIMIT 25`, now)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, ws, task, run, subject string
		var attempts int
		if rows.Scan(&id, &ws, &task, &run, &subject, &attempts) == nil {
			s.deliverResult(ctx, id, ws, task, run, subject, attempts)
		}
	}
}

func (s *Service) deliverResult(ctx context.Context, id, workspace, task, run, subject string, attempts int) {
	res, err := s.db.ExecContext(ctx, `UPDATE voice_result_deliveries SET attempts=attempts+1,updated_at=? WHERE entry_id=? AND status='pending' AND attempts=?`, s.now().UTC().Format(time.RFC3339Nano), id, attempts)
	if err != nil {
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return
	}
	attempt := attempts + 1
	var text string
	if err := s.db.QueryRowContext(ctx, `SELECT content FROM task_entries WHERE id=? AND workspace_id=? AND task_id=? AND kind='assistant_message'`, id, workspace, task).Scan(&text); err != nil {
		s.retryResult(id, attempt, "entry_missing")
		return
	}
	text = boundedResultText(text)
	if text == "" {
		s.finishResult(id, "failed", "empty_result")
		return
	}
	var session, gateway string
	var generation, current int64
	err = s.db.QueryRowContext(ctx, `SELECT id,gateway_session_id,generation FROM voice_sessions WHERE workspace_id=? AND owner_id=? AND state='active' LIMIT 1`, workspace, subject).Scan(&session, &gateway, &generation)
	if err == sql.ErrNoRows {
		s.finishResult(id, "skipped_no_call", "")
		return
	}
	if err != nil {
		s.retryResult(id, attempt, "session_lookup")
		return
	}
	if err = s.db.QueryRowContext(ctx, `SELECT generation FROM voice_sessions WHERE id=? AND state='active'`, session).Scan(&current); err != nil || current != generation {
		s.finishResult(id, "skipped_stale", "stale_generation")
		return
	}
	body, _ := json.Marshal(map[string]string{"text": "Zoe result for the user: " + text + " Please tell them briefly."})
	resp, _, callErr := s.callGateway(ctx, http.MethodPost, "/v1/voice/sessions/"+url.PathEscape(gateway)+"/context", body)
	if resp != nil {
		resp.Body.Close()
	}
	if callErr == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = s.db.ExecContext(ctx, `UPDATE voice_result_deliveries SET status='delivered',voice_session_id=?,generation=?,updated_at=? WHERE entry_id=? AND status='pending'`, session, generation, s.now().UTC().Format(time.RFC3339Nano), id)
		return
	}
	code := "gateway_failed"
	if resp != nil && resp.StatusCode == http.StatusNotImplemented {
		code = "route_unsupported"
		s.finishResult(id, "failed", code)
		return
	}
	s.retryResult(id, attempt, code)
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
