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

// resultDeliveryLease bounds how long a claimed delivery is hidden from other dispatchers.
const resultDeliveryLease = 30 * time.Second

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
			s.consumeResultEvents(context.Background())
			s.dispatchResults(context.Background())
		case <-s.reaperStop:
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
	rows, err := s.db.QueryContext(ctx, `SELECT entry_id,workspace_id,task_id,run_id,subject_id,attempts FROM voice_result_deliveries WHERE status='pending' AND next_attempt_at<=? ORDER BY next_attempt_at LIMIT 25`, now)
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

func (s *Service) deliverResult(ctx context.Context, id, workspace, task, run, subject string, attempts int) {
	// Claim the row with a short lease: attempts is the compare-and-set token and
	// next_attempt_at moves into the future, so a concurrent dispatcher that
	// already read this row can no longer win it and, if this process dies mid
	// send, the row becomes due again after the lease.
	now := s.now().UTC()
	res, err := s.db.ExecContext(ctx, `UPDATE voice_result_deliveries SET attempts=attempts+1,next_attempt_at=?,updated_at=? WHERE entry_id=? AND status='pending' AND attempts=? AND next_attempt_at<=?`, now.Add(resultDeliveryLease).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), id, attempts, now.Format(time.RFC3339Nano))
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
