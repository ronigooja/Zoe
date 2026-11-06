package voice

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/internal/answer"
	"github.com/ronigooja/Zoe/internal/recall/dates"
)

const recallExcerptBytes = 16 * 1024
const recallNotFound = "I could not find that in our conversation records."
const recallPrefix = "Read this to the user exactly as written: "

// recallWorkerLimit bounds model work per service, including persistence retries.
const recallWorkerLimit = 4
const recallPersistAttempts = 3

func (s *Service) recallTimeout() time.Duration {
	if s.recall.RecallTimeout > 0 {
		return s.recall.RecallTimeout
	}
	return 8 * time.Second
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
		s.persistRecallRetry(ctx, id, recallSpeech(recallNotFound, nil))
	}
}

func (s *Service) persistRecall(ctx context.Context, id, text string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := s.now().UTC().Format(time.RFC3339Nano)
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

var recallIDs = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F-]{27,}\b`)

func recallSpeech(text string, candidates []answer.Candidate) string {
	for _, c := range candidates {
		text = strings.ReplaceAll(text, c.ID, "")
	}
	text = recallIDs.ReplaceAllString(text, "")
	// Reject markup instead of risking reading formatting, links or evidence IDs aloud.
	if strings.ContainsAny(text, "*#`[]<>_~|") {
		text = recallNotFound
	}
	text = boundedResultText(text)
	if text == "" {
		text = recallNotFound
	}
	return recallPrefix + text
}

type recallCandidate struct {
	answer.Candidate
	session     string
	first, last int64
}

// dispatchRecall starts work only after the holding receipt is delivered. The
// durable compare-and-set prevents retried handoffs/dispatchers starting twice.
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
	var workspace, owner, session, turn, question string
	var generation int64
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_id,owner_id,session_id,generation,turn_id,question FROM voice_recall_jobs WHERE id=?`, id).Scan(&workspace, &owner, &session, &generation, &turn, &question); err != nil {
		return
	}
	text, count, status := s.recallAnswer(ctx, workspace, owner, question)
	// Model deadlines get a fallback, but shutdown leaves the lease for recovery.
	if s.recallCtx.Err() != nil {
		return
	}
	persist, done := context.WithTimeout(s.recallCtx, 8*time.Second)
	defer done()
	if !s.persistRecallRetry(persist, id, text) {
		if !s.persistRecallRetry(persist, id, recallSpeech(recallNotFound, nil)) {
			return
		}
		status = "persist_fallback"
	}
	log.Printf("voice recall id=%s candidates=%d status=%s", id, count, status)
	s.dispatchFastReplies(persist)
}

func (s *Service) recallAnswer(ctx context.Context, workspace, owner, question string) (string, int, string) {
	fail := func(n int, status string) (string, int, string) { return recallSpeech(recallNotFound, nil), n, status }
	if s.recall.Answerer == nil {
		return fail(0, "unconfigured")
	}
	limit := s.recall.RecallSummaryLimit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return fail(0, "timezone")
	}
	query := `SELECT v.id,v.topic,v.started_at,v.ended_at,v.summary,v.session_id,v.first_seq,v.last_seq FROM voice_call_summaries v JOIN voice_sessions s ON s.id=v.session_id AND s.workspace_id=v.workspace_id WHERE v.workspace_id=? AND s.owner_id=?`
	args := []any{workspace, owner}
	if r, ok := dates.Resolve(question, s.now(), loc); ok {
		query += ` AND julianday(v.started_at)>=julianday(?) AND julianday(v.started_at)<julianday(?)`
		args = append(args, r.Start.Format(time.RFC3339Nano), r.End.Format(time.RFC3339Nano))
	}
	query += ` ORDER BY julianday(v.started_at) DESC,v.id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fail(0, "summary_error")
	}
	var candidates []answer.Candidate
	known := map[string]recallCandidate{}
	for rows.Next() {
		var c recallCandidate
		if err = rows.Scan(&c.ID, &c.Topic, &c.StartTime, &c.EndTime, &c.Summary, &c.session, &c.first, &c.last); err != nil {
			rows.Close()
			return fail(0, "summary_error")
		}
		candidates = append(candidates, c.Candidate)
		known[c.ID] = c
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fail(0, "summary_error")
	}
	n := len(candidates)
	if n == 0 {
		return fail(0, "no_summaries")
	}
	result, err := s.recall.Answerer.Answer(ctx, question, candidates, nil)
	if err != nil || ctx.Err() != nil {
		return fail(n, "model_error")
	}
	if result.Status == "need_transcript" {
		excerpts := []answer.Excerpt{}
		remaining := recallExcerptBytes
		seen := map[string]bool{}
		if len(result.SummaryIDs) == 0 {
			return fail(n, "invalid_request")
		}
		for _, id := range result.SummaryIDs {
			c, ok := known[id]
			if !ok || seen[id] {
				return fail(n, "invalid_request")
			}
			seen[id] = true
			if remaining == 0 {
				continue
			}
			turns, err := ReadTranscriptTurns(ctx, s.db, workspace, c.session, c.first, c.last)
			if err != nil {
				return fail(n, "transcript_error")
			}
			var b strings.Builder
			for _, t := range turns {
				line := fmt.Sprintf("%s: %s\n", t.Role, t.Text)
				if len(line) > remaining {
					line = line[:remaining]
					for !utf8.ValidString(line) {
						line = line[:len(line)-1]
					}
				}
				b.WriteString(line)
				remaining -= len(line)
				if remaining == 0 {
					break
				}
			}
			excerpts = append(excerpts, answer.Excerpt{SummaryID: id, Text: b.String()})
		}
		result, err = s.recall.Answerer.Answer(ctx, question, candidates, excerpts)
	}
	if err != nil || ctx.Err() != nil {
		return fail(n, "model_error")
	}
	if result.Status != "answer" || strings.TrimSpace(result.Text) == "" {
		return fail(n, "not_found")
	}
	return recallSpeech(result.Text, candidates), n, "answer"
}
