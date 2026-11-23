package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

// orderedRunEntries uses the same instant + id ordering as task entry pagination.
func orderedRunEntries(ctx context.Context, tx *sql.Tx, runID string, undelivered bool) ([]contracts.RunEntry, error) {
	query := `SELECT e.id,e.content,e.created_at,a.id,a.run_id,a.request_id,a.action_digest,a.question FROM task_entries e JOIN runs r ON r.task_id=e.task_id LEFT JOIN task_question_answers b ON b.entry_id=e.id AND b.task_id=e.task_id LEFT JOIN pending_runtime_actions a ON a.id=b.question_id AND a.task_id=b.task_id WHERE r.id=? AND e.kind='user_message' AND NOT EXISTS (SELECT 1 FROM events v WHERE v.aggregate_id=e.task_id AND v.event_type='task.entry_appended' AND v.aggregate_version=1 AND json_extract(v.payload,'$.payload.entry.id')=e.id)`
	if undelivered {
		query += ` AND NOT EXISTS (SELECT 1 FROM run_entry_deliveries d WHERE d.run_id=r.id AND d.entry_id=e.id)`
	}
	rows, err := tx.QueryContext(ctx, query, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []contracts.RunEntry
	for rows.Next() {
		var e contracts.RunEntry
		var stamp string
		var questionID, questionRun, requestID, digest, question sql.NullString
		if err := rows.Scan(&e.ID, &e.Content, &stamp, &questionID, &questionRun, &requestID, &digest, &question); err != nil {
			return nil, err
		}
		if questionID.Valid {
			e.Question = &contracts.RunQuestionBinding{QuestionID: questionID.String, RunID: questionRun.String, RequestID: requestID.String, ActionDigest: digest.String, Text: question.String}
		}
		e.CreatedAt, err = time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})
	return entries, nil
}

func loadPriorContexts(ctx context.Context, tx *sql.Tx, taskID string) ([]contracts.TaskPriorContext, error) {
	var prior contracts.TaskPriorContext
	var result sql.NullString
	var additions []byte
	err := tx.QueryRowContext(ctx, `SELECT prior_task_id,goal,status,result,additions_json FROM task_prior_contexts WHERE task_id=?`, taskID).Scan(&prior.TaskID, &prior.Goal, &prior.Status, &result, &additions)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if result.Valid {
		prior.Result = &result.String
	}
	if err := json.Unmarshal(additions, &prior.Additions); err != nil {
		return nil, err
	}
	return []contracts.TaskPriorContext{prior}, nil
}

func markRunEntries(ctx context.Context, tx *sql.Tx, runID, kind string, entries []contracts.RunEntry) error {
	for _, e := range entries {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO run_entry_deliveries(run_id,entry_id,delivered_at,prompt_kind) VALUES(?,?,?,?)`, runID, e.ID, time.Now().UTC().Format(time.RFC3339Nano), kind); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleEntryDelivery(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok || !HasRunnerScope(p, "runner:control:read") {
		writeError(w, apiError(403, "forbidden", "runner lacks control scope", nil))
		return
	}
	var in contracts.EntryDelivery
	// Identity-only receipts are capped at 4096 bytes independently of question text.
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.cfg.Limits.Int64("entry_receipt_max_bytes")))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		writeError(w, apiError(400, "invalid_request", "invalid delivery", nil))
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF || !validEntryReceipt(in) {
		writeError(w, apiError(400, "invalid_request", "bounded identity-only delivery receipt required", nil))
		return
	}
	tx, err := s.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback()
	var authorized bool
	err = tx.QueryRowContext(r.Context(), `SELECT 1 FROM runs WHERE id=? AND runner_id=? AND attempt_id=? AND fence=? AND status='active' AND desired_control='run'`, r.PathValue("run_id"), p.RunnerID, in.AttemptID, in.Fence).Scan(&authorized)
	if err != nil || !HasRunnerScope(p, "runner:control:read") {
		writeError(w, apiError(409, "fence_conflict", "delivery does not match active run", nil))
		return
	}
	entries, err := orderedRunEntries(r.Context(), tx, r.PathValue("run_id"), false)
	if err != nil {
		writeError(w, err)
		return
	}
	for _, e := range entries {
		if e.ID == in.EntryID {
			if e.Question != nil {
				contextReceipt := in.Context && e.Question.RunID != r.PathValue("run_id")
				if in.QuestionID != e.Question.QuestionID || in.ActionDigest != e.Question.ActionDigest || (!contextReceipt && e.Question.RunID != r.PathValue("run_id")) || (in.Context && !contextReceipt) {
					writeError(w, apiError(409, "question_binding_conflict", "answer acknowledgement requires its exact runtime binding", nil))
					return
				}
				var bound bool
				if contextReceipt {
					err = tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM pending_runtime_actions a JOIN task_question_answers b ON b.question_id=a.id JOIN task_entries e ON e.id=b.entry_id JOIN runs old ON old.id=a.run_id AND old.task_id=a.task_id JOIN runs current ON current.id=? AND current.task_id=a.task_id JOIN run_entry_deliveries d ON d.run_id=current.id AND d.entry_id=e.id AND d.prompt_kind='initial' WHERE b.entry_id=? AND b.question_id=? AND b.task_id=a.task_id AND a.run_id=? AND old.status='stopped' AND old.fence=a.fence AND a.action_digest=? AND a.kind='worker_question' AND a.decision='answer' AND a.message=e.content)`, r.PathValue("run_id"), in.EntryID, in.QuestionID, e.Question.RunID, in.ActionDigest).Scan(&bound)
				} else {
					err = tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM pending_runtime_actions a JOIN task_question_answers b ON b.question_id=a.id JOIN task_entries e ON e.id=b.entry_id WHERE b.entry_id=? AND b.question_id=? AND b.task_id=a.task_id AND a.run_id=? AND a.fence=? AND a.action_digest=? AND a.kind='worker_question' AND a.decision='answer' AND a.message=e.content)`, in.EntryID, in.QuestionID, r.PathValue("run_id"), in.Fence, in.ActionDigest).Scan(&bound)
				}
				if err != nil {
					writeError(w, err)
					return
				}
				if !bound {
					writeError(w, apiError(409, "question_binding_conflict", "answer receipt does not match stored authorization", nil))
					return
				}
				if _, err = tx.ExecContext(r.Context(), `UPDATE task_question_answers SET delivery_state='acknowledged',acknowledged_at=coalesce(acknowledged_at,?) WHERE entry_id=? AND question_id=?`, time.Now().UTC().Format(time.RFC3339Nano), e.ID, e.Question.QuestionID); err != nil {
					writeError(w, err)
					return
				}
				// Context delivery closes the original stopped question, never authorizes
				// its old runtime form in the new session.
				if _, err = tx.ExecContext(r.Context(), `UPDATE pending_runtime_actions SET authorization_used=1 WHERE id=? AND run_id=? AND request_id=? AND action_digest=? AND kind='worker_question' AND decision='answer'`, e.Question.QuestionID, e.Question.RunID, e.Question.RequestID, e.Question.ActionDigest); err != nil {
					writeError(w, err)
					return
				}
			} else if in.QuestionID != "" || in.ActionDigest != "" || in.Context {
				writeError(w, apiError(409, "question_binding_conflict", "addition has no question binding", nil))
				return
			}
			if err = markRunEntries(r.Context(), tx, r.PathValue("run_id"), "append", []contracts.RunEntry{e}); err == nil {
				err = tx.Commit()
			}
			if err != nil {
				writeError(w, err)
				return
			}
			w.WriteHeader(204)
			return
		}
	}
	writeError(w, apiError(404, "not_found", "entry not found", nil))
}

func validEntryReceipt(in contracts.EntryDelivery) bool {
	return len(in.AttemptID) > 0 && len(in.AttemptID) <= 128 && len(in.EntryID) > 0 && len(in.EntryID) <= 128 && len(in.QuestionID) <= 128 && len(in.ActionDigest) <= 64 && in.Fence > 0 && ((in.QuestionID == "") == (in.ActionDigest == ""))
}
