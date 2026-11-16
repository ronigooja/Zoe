package voice

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/memoryapi"
)

// memoryWriteHolding identifies receiver-reconciled writes in the durable queue.
const memoryWriteHolding = "memory_write_holding"

// recallWorkerLimit bounds model work per service, including persistence retries.
const recallWorkerLimit = 4
const recallPersistAttempts = 3

func (s *Service) recallTimeout() time.Duration {
	if s.recall.RecallTimeout > 0 {
		return s.recall.RecallTimeout
	}
	return 60 * time.Second
}

// A running job's lease starts at its delivered holding receipt's updated_at.
// Updating that timestamp and claiming the job are atomic; no schema change is needed.
func (s *Service) recoverRecall(ctx context.Context, startup bool) {
	lease := s.recall.RecallLease
	if lease <= 0 {
		lease = 2 * s.recallTimeout()
	}
	query := `SELECT j.id FROM voice_recall_jobs j JOIN voice_fast_replies h ON h.id=j.holding_id WHERE j.status='running'`
	var args []any
	if !startup {
		query += ` AND h.updated_at<=?`
		args = append(args, s.now().Add(-lease).UTC().Format(time.RFC3339Nano))
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		// Reconcile writes using the same receiver operation identity. Completed
		// operations are returned, ready plans resume, uncertain effects fence.
		res, err := s.db.ExecContext(ctx, `UPDATE voice_recall_jobs SET status='pending' WHERE id=? AND status='running' AND holding_id IN (SELECT id FROM voice_fast_replies WHERE intent_kind=?)`, id, memoryWriteHolding)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n == 1 {
			continue
		}
		s.persistRecallRetry(ctx, id, s.recallFallback(ctx, id))
	}
}

func (s *Service) recallFallback(ctx context.Context, id string) string {
	return stewardErrorSpeech
}

func (s *Service) persistRecall(ctx context.Context, id, text string) error {
	return s.persistRecallStatus(ctx, id, text, "done")
}

func (s *Service) persistRecallStatus(ctx context.Context, id, text, terminal string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := s.now().UTC().Format(time.RFC3339Nano)
	if text == "" {
		_, err = tx.ExecContext(ctx, `UPDATE voice_recall_jobs SET status=? WHERE id=? AND status='running'`, terminal, id)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) SELECT id,workspace_id,session_id,generation,'recall-answer:'||turn_id,?,'recall_answer','pending',?,?,? FROM voice_recall_jobs WHERE id=? AND status='running'`, text, at, at, at, id)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE voice_recall_jobs SET status=? WHERE id=? AND status='running'`, terminal, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) persistRecallRetry(ctx context.Context, id, text string) bool {
	return s.persistRecallRetryStatus(ctx, id, text, "done")
}

func (s *Service) persistRecallRetryStatus(ctx context.Context, id, text, terminal string) bool {
	for attempt := 0; attempt < recallPersistAttempts; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		call, cancel := context.WithTimeout(ctx, time.Second)
		err := s.persistRecallStatus(call, id, text, terminal)
		cancel()
		if err == nil {
			return true
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
}

// dispatchRecall uses a silent delivered receipt for new jobs. The durable
// compare-and-set prevents retried handoffs/dispatchers starting twice.
func (s *Service) dispatchRecall(ctx context.Context) {
	s.recoverRecall(ctx, false)
	rows, err := s.db.QueryContext(ctx, `SELECT j.id FROM voice_recall_jobs j JOIN voice_fast_replies h ON h.id=j.holding_id WHERE j.status='pending' AND h.status='delivered' LIMIT 25`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.lifecycleMu.Lock()
		if s.stopped {
			s.lifecycleMu.Unlock()
			return
		}
		select {
		case s.recallSlots <- struct{}{}:
		default:
			s.lifecycleMu.Unlock()
			return
		}
		tx, err := s.db.BeginTx(ctx, nil)
		claimed := false
		if err == nil {
			res, e := tx.ExecContext(ctx, `UPDATE voice_recall_jobs SET status='running' WHERE id=? AND status='pending'`, id)
			if e == nil {
				n, _ := res.RowsAffected()
				if n == 1 {
					_, e = tx.ExecContext(ctx, `UPDATE voice_fast_replies SET updated_at=? WHERE id=(SELECT holding_id FROM voice_recall_jobs WHERE id=?)`, s.now().UTC().Format(time.RFC3339Nano), id)
					if e == nil {
						claimed = tx.Commit() == nil
					}
				}
			}
			tx.Rollback()
		}
		if !claimed {
			<-s.recallSlots
			s.lifecycleMu.Unlock()
			continue
		}
		s.workers.Add(1)
		go func() { defer s.workers.Done(); defer func() { <-s.recallSlots }(); s.runRecall(id) }()
		s.lifecycleMu.Unlock()
	}
}

func (s *Service) runRecall(id string) {
	ctx, cancel := context.WithTimeout(s.recallCtx, s.recallTimeout())
	defer cancel()
	var workspace, owner, session, turn, question, kind string
	var generation int64
	if err := s.db.QueryRowContext(ctx, `SELECT j.workspace_id,j.owner_id,j.session_id,j.generation,j.turn_id,j.question,h.intent_kind FROM voice_recall_jobs j JOIN voice_fast_replies h ON h.id=j.holding_id WHERE j.id=? AND j.status='running'`, id).Scan(&workspace, &owner, &session, &generation, &turn, &question, &kind); err != nil {
		return
	}
	started := time.Now()
	var text, status string
	var count int
	if kind == memoryWriteHolding {
		// A crash after saving a terminal receiver outcome must not dispatch again.
		var prior string
		err := s.db.QueryRowContext(ctx, `SELECT status FROM memory_write_outcomes WHERE op_id=?`, id).Scan(&prior)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return
		}
		if err == nil && (prior == "write_blocked" || prior == "write_error" || prior == "write_ok") {
			text = ""
			if prior != "write_ok" {
				text = stewardErrorSpeech
			}
			terminal := "done"
			if prior == "write_blocked" {
				terminal = "blocked"
			}
			s.persistRecallRetryStatus(ctx, id, text, terminal)
			s.dispatchFastReplies(ctx)
			return
		}
		// The CAS claim is already committed and this read is closed before
		// calling the worker, so slow writes hold no SQLite transaction.
		// The retained register request is the sole execution input. The turn
		// linkage remains provenance; do not reread its original utterance.
		if _, err := s.db.ExecContext(ctx, `INSERT INTO memory_write_outcomes(op_id,status) VALUES(?,'running') ON CONFLICT(op_id) DO UPDATE SET status='running'`, id); err != nil {
			return
		}
		err = s.recall.MemoryAPI.Write(ctx, memoryUser(workspace, owner), id, question)
		status = "write_ok"
		if err != nil {
			text, status = stewardErrorSpeech, "write_error"
			var failure *memoryapi.Error
			if errors.As(err, &failure) {
				switch failure.Kind {
				case "write_quarantined", "ingest_quarantined":
					status = "write_blocked"
				case "write_uncertain", "transport", "deadline_exceeded", "response_read", "response_invalid", "http_status", "backend_unavailable":
					status = "write_uncertain"
				}
			}
		}
		errorKind := ""
		var failure *memoryapi.Error
		if errors.As(err, &failure) {
			errorKind = failure.Kind
		}
		outcomeCtx, outcomeCancel := context.WithTimeout(s.recallCtx, time.Second)
		defer outcomeCancel()
		if _, e := s.db.ExecContext(outcomeCtx, `INSERT INTO memory_write_outcomes(op_id,status,error_kind) VALUES(?,?,?) ON CONFLICT(op_id) DO UPDATE SET status=excluded.status,error_kind=excluded.error_kind`, id, status, errorKind); e != nil {
			return
		}
		if status == "write_uncertain" {
			// No failure feedback for a recoverable unknown. Preserve the job ID
			// and request so the next dispatcher round reconciles the same plan.
			_, _ = s.db.ExecContext(outcomeCtx, `UPDATE voice_recall_jobs SET status='pending' WHERE id=? AND status='running'`, id)
			return
		}
	} else {
		text, count, status = s.recallAnswer(ctx, workspace, owner, question)
	}
	elapsedMS := time.Since(started).Milliseconds()
	// Model deadlines get a fallback, but shutdown leaves the lease for recovery.
	if s.recallCtx.Err() != nil {
		return
	}
	persist, done := context.WithTimeout(s.recallCtx, 8*time.Second)
	defer done()
	terminal := "done"
	if status == "write_blocked" {
		terminal = "blocked"
	}
	if !s.persistRecallRetryStatus(persist, id, text, terminal) {
		if !s.persistRecallRetryStatus(persist, id, s.recallFallback(persist, id), terminal) {
			return
		}
		status = "persist_fallback"
	}
	logging.Infof("voice recall id=%s candidates=%d status=%s elapsed_ms=%d", id, count, status, elapsedMS)
	s.dispatchFastReplies(persist)
}

func (s *Service) recallAnswer(ctx context.Context, workspace, owner, question string) (string, int, string) {
	items, err := s.recall.MemoryAPI.Search(ctx, memoryUser(workspace, owner), question)
	if err != nil {
		return stewardErrorSpeech, 0, "worker_error"
	}
	texts := make([]string, 0, len(items))
	for _, item := range items {
		texts = append(texts, item.Text)
	}
	return strings.Join(texts, "\n"), len(items), "ok"
}

// memoryUser uses a JSON tuple so workspace/subject separators cannot collide.
func memoryUser(workspace, owner string) string {
	return memoryapi.User(workspace, owner)
}
