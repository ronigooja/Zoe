package voice

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/logging"
)

// memoryWriteHolding identifies at-most-once writes in the shared durable queue.
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
		// A running write may already have reached Mem0. Finalize with one
		// idempotent error receipt, never reset to pending or call the worker.
		// Recall recovery retains its existing fallback behavior as well.
		s.persistRecallRetry(ctx, id, s.recallFallback(ctx, id))
	}
}

func (s *Service) recallFallback(ctx context.Context, id string) string {
	return stewardErrorSpeech
}

func (s *Service) persistRecall(ctx context.Context, id, text string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := s.now().UTC().Format(time.RFC3339Nano)
	if text == "" {
		_, err = tx.ExecContext(ctx, `UPDATE voice_recall_jobs SET status='done' WHERE id=? AND status='running'`, id)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) SELECT id,workspace_id,session_id,generation,'recall-answer:'||turn_id,?,'recall_answer','pending',?,?,? FROM voice_recall_jobs WHERE id=? AND status='running'`, text, at, at, at, id)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE voice_recall_jobs SET status='done' WHERE id=? AND status='running'`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) persistRecallRetry(ctx context.Context, id, text string) bool {
	for attempt := 0; attempt < recallPersistAttempts; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		call, cancel := context.WithTimeout(ctx, time.Second)
		err := s.persistRecall(call, id, text)
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
		// The CAS claim is already committed and this read is closed before
		// calling the worker, so slow writes hold no SQLite transaction.
		var sentence string
		err := s.db.QueryRowContext(ctx, `SELECT v.content FROM voice_turns v JOIN voice_recall_jobs j ON j.holding_id=v.id WHERE j.id=? AND v.workspace_id=j.workspace_id AND v.session_id=j.session_id AND v.generation=j.generation AND v.turn_id=j.turn_id AND v.handoff_handled=1`, id).Scan(&sentence)
		if err == nil {
			err = s.recall.MemoryAPI.Write(ctx, memoryUser(workspace, owner), sentence, question)
		}
		status = "write_ok"
		if err != nil {
			text, status = stewardErrorSpeech, "write_error"
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
	if !s.persistRecallRetry(persist, id, text) {
		if !s.persistRecallRetry(persist, id, s.recallFallback(persist, id)) {
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
	data, _ := json.Marshal([]string{workspace, owner})
	return string(data)
}
