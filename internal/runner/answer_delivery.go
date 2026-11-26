package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ronigooja/Zoe/contracts"
)

type answerDeliveryJournal interface {
	AnswerState(context.Context, string, string, string, contracts.RunQuestionBinding) (string, error)
	SetAnswerState(context.Context, string, string, string, contracts.RunQuestionBinding, string) error
}

func (e *Engine) deliverBoundAnswer(ctx context.Context, c contracts.JobClaim, b RuntimeBinding, entry contracts.RunEntry, key string) error {
	reply, ok := e.Runtime.(FormReply)
	if !ok {
		return errors.New("runtime lacks form reply")
	}
	pending, ok := e.Runtime.(PendingRequests)
	if !ok {
		return errors.New("runtime lacks pending-question inspection")
	}
	control, ok := e.Control.(interface {
		AcknowledgeEntry(context.Context, string, contracts.EntryDelivery) error
	})
	if !ok {
		return errors.New("control lacks answer acknowledgement")
	}
	err := deliverAnswer(ctx, e.Journal, c, b, entry, func() error {
		if key == "" {
			return errors.New("runtime question field missing")
		}
		observeModel(ctx, b.SessionID, "request", "answer:"+entry.ID, entry.Content)
		return reply.ReplyForm(ctx, b, entry.Question.RequestID, map[string]any{key: entry.Content})
	}, func() (bool, error) {
		forms, err := pending.PendingForms(ctx, b)
		if err != nil {
			return false, err
		}
		for _, form := range forms {
			if form.ID == entry.Question.RequestID {
				return true, nil
			}
		}
		return false, nil
	}, func() error {
		return control.AcknowledgeEntry(ctx, c.RunID, contracts.EntryDelivery{AttemptID: c.AttemptID, Fence: c.Fence, EntryID: entry.ID, QuestionID: entry.Question.QuestionID, ActionDigest: entry.Question.ActionDigest})
	})
	if errors.Is(err, ErrOutcomePending) {
		if reportErr := e.emit(ctx, c, "effect_unknown", "runtime", "accepted answer delivery is uncertain; automatic replay paused", map[string]any{"reason": "answer_delivery_uncertain", "entry_id": entry.ID, "question_id": entry.Question.QuestionID}, nil); reportErr != nil {
			return reportErr
		}
	}
	return err
}

func (j *Journal) PendingAnswerDelivery(ctx context.Context, run, session string) (*contracts.RunEntry, error) {
	var entry contracts.RunEntry
	var body []byte
	err := j.db.QueryRowContext(ctx, `SELECT entry_id,binding_json FROM journal_answer_deliveries WHERE run_id=? AND session_id=? AND state<>'acknowledged' ORDER BY rowid LIMIT 1`, run, session).Scan(&entry.ID, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entry.Question = &contracts.RunQuestionBinding{}
	if err := json.Unmarshal(body, entry.Question); err != nil {
		return nil, err
	}
	return &entry, nil
}

func (j *Journal) AnswerState(ctx context.Context, run, entry, session string, binding contracts.RunQuestionBinding) (string, error) {
	var state, storedSession string
	var stored []byte
	err := j.db.QueryRowContext(ctx, `SELECT state,session_id,binding_json FROM journal_answer_deliveries WHERE run_id=? AND entry_id=?`, run, entry).Scan(&state, &storedSession, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	canonical, _ := json.Marshal(binding)
	if storedSession != session || string(stored) != string(canonical) {
		return "", errors.New("answer delivery binding changed")
	}
	return state, nil
}

func (j *Journal) SetAnswerState(ctx context.Context, run, entry, session string, binding contracts.RunQuestionBinding, state string) error {
	body, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	_, err = j.db.ExecContext(ctx, `INSERT INTO journal_answer_deliveries(run_id,entry_id,session_id,binding_json,state) VALUES(?,?,?,?,?) ON CONFLICT(run_id,entry_id) DO UPDATE SET state=excluded.state WHERE session_id=excluded.session_id AND binding_json=excluded.binding_json`, run, entry, session, body, state)
	return err
}

// deliverAnswer never replays an attempted reply: current pending forms do not
// establish a receipt for a particular answer or fence a still-running POST.
func deliverAnswer(ctx context.Context, journal answerDeliveryJournal, c contracts.JobClaim, b RuntimeBinding, entry contracts.RunEntry, send func() error, inspect func() (bool, error), ack func() error) error {
	if entry.Question == nil || entry.Question.RunID != c.RunID || entry.ID == "" {
		return errors.New("answer requires exact run and entry binding")
	}
	q := *entry.Question
	state, err := journal.AnswerState(ctx, c.RunID, entry.ID, b.SessionID, q)
	if err != nil {
		return err
	}
	if state == "acknowledged" {
		return nil
	}
	if state == "attempted" {
		pending, err := inspect()
		if err != nil {
			return fmt.Errorf("%w: question inspection failed: %v", ErrOutcomePending, err)
		}
		return fmt.Errorf("%w: question pending=%t; OpenCode has no answer receipt or replay fence", ErrOutcomePending, pending)
	}
	if state == "" {
		if err := journal.SetAnswerState(ctx, c.RunID, entry.ID, b.SessionID, q, "attempted"); err != nil {
			return err
		}
		if err := send(); err != nil {
			return fmt.Errorf("%w: runtime answer reply uncertain: %v", ErrOutcomePending, err)
		}
		if err := journal.SetAnswerState(ctx, c.RunID, entry.ID, b.SessionID, q, "confirmed"); err != nil {
			return fmt.Errorf("%w: runtime answered but confirmation checkpoint failed: %v", ErrOutcomePending, err)
		}
	}
	if err := ack(); err != nil {
		return err
	}
	return journal.SetAnswerState(ctx, c.RunID, entry.ID, b.SessionID, q, "acknowledged")
}
