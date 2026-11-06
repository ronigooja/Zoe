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
		res, err := s.db.ExecContext(ctx, `UPDATE voice_recall_jobs SET status='running' WHERE id=? AND status='pending'`, id)
		if err != nil {
			continue
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			continue
		}
		go s.runRecall(id)
	}
}

func (s *Service) runRecall(id string) {
	timeout := s.recall.RecallTimeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var workspace, owner, session, turn, question string
	var generation int64
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_id,owner_id,session_id,generation,turn_id,question FROM voice_recall_jobs WHERE id=?`, id).Scan(&workspace, &owner, &session, &generation, &turn, &question); err != nil {
		return
	}
	text, count, status := s.recallAnswer(ctx, workspace, owner, question)
	// Persist with a fresh bounded context even when model work used its deadline.
	persist, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	tx, err := s.db.BeginTx(persist, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	at := s.now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(persist, `INSERT OR IGNORE INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,'recall_answer','pending',?,?,?)`, id, workspace, session, generation, "recall-answer:"+turn, text, at, at, at)
	if err != nil {
		return
	}
	if _, err = tx.ExecContext(persist, `UPDATE voice_recall_jobs SET status='done' WHERE id=?`, id); err != nil {
		return
	}
	if tx.Commit() != nil {
		return
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
