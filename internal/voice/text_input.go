package voice

import (
	"context"
	"database/sql"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

func (s *Service) validTextInput(text string) bool {
	return utf8.ValidString(text) && strings.TrimSpace(text) != "" && len(text) <= s.limits.Int("voice_text_max_bytes")
}

func (s *Service) insertTextInput(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, text string) error {
	if !s.validTextInput(text) {
		return apiError(400, "invalid_request", "Text input is invalid", nil)
	}
	at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_text_inputs(command_id,owner_subject_id,text,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?)`, cmd.CommandID, cmd.Principal.SubjectID, text, at, at, at)
	return err
}
func (s *Service) commandVoiceTextInput(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if err := uiScope(cmd); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if !utf8.Valid(req.Payload) {
		return contracts.CommandResult{}, nil, apiError(400, "invalid_request", "Text input is invalid", nil)
	}
	var p struct {
		Text string `json:"text"`
	}
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	err := s.insertTextInput(ctx, tx, cmd, p.Text)
	return contracts.CommandResult{ResourceType: "voice_text_input", ResourceID: cmd.CommandID}, nil, err
}

type voiceInputDelivery struct {
	kind, id, owner, text string
	attempts              int
}

func (s *Service) dispatchVoiceInputs(ctx context.Context) {
	now := s.now().UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `SELECT 'user_text',command_id,owner_subject_id,text,attempts FROM voice_text_inputs WHERE status='pending' AND coalesce(next_attempt_at,'')<=? UNION ALL SELECT kind,source_id,owner_subject_id,text,attempts FROM voice_input_notices WHERE status='pending' AND coalesce(next_attempt_at,'')<=? LIMIT ?`, now, now, s.limits.Int("voice_text_sweep_rows"))
	if err != nil {
		return
	}
	var batch []voiceInputDelivery
	for rows.Next() {
		var d voiceInputDelivery
		if rows.Scan(&d.kind, &d.id, &d.owner, &d.text, &d.attempts) == nil {
			batch = append(batch, d)
		}
	}
	rows.Close()
	for _, d := range batch {
		s.deliverVoiceInput(ctx, d)
	}
}
func (s *Service) deliverVoiceInput(ctx context.Context, d voiceInputDelivery) {
	unlock := s.lockUIOwner(d.owner)
	defer unlock()
	table, where, args := "voice_input_notices", "kind=? AND source_id=?", []any{d.kind, d.id}
	if d.kind == "user_text" {
		table, where, args = "voice_text_inputs", "command_id=?", []any{d.id}
	}
	now := s.now().UTC()
	at := now.Format(time.RFC3339Nano)
	claimArgs := append([]any{now.Add(30 * time.Second).Format(time.RFC3339Nano), at}, args...)
	claimArgs = append(claimArgs, d.attempts, at)
	r, err := s.db.ExecContext(ctx, `UPDATE `+table+` SET attempts=attempts+1,next_attempt_at=?,updated_at=? WHERE `+where+` AND status='pending' AND attempts=? AND coalesce(next_attempt_at,'')<=?`, claimArgs...)
	if err != nil {
		return
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return
	}
	attempt := d.attempts + 1
	update := func(status, code string, consume bool, delay time.Duration) {
		delta := 0
		if !consume {
			delta = -1
		}
		a := []any{status, delta, s.now().Add(delay).UTC().Format(time.RFC3339Nano), code, s.now().UTC().Format(time.RFC3339Nano)}
		a = append(a, args...)
		a = append(a, attempt)
		_, _ = s.db.ExecContext(ctx, `UPDATE `+table+` SET status=?,attempts=attempts+?,next_attempt_at=?,last_error=?,updated_at=? WHERE `+where+` AND status='pending' AND attempts=?`, a...)
	}
	var session, gateway string
	var generation int64
	err = s.db.QueryRowContext(ctx, `SELECT id,gateway_session_id,generation FROM voice_sessions WHERE owner_id=? AND state='active' LIMIT 1`, d.owner).Scan(&session, &gateway, &generation)
	if err == sql.ErrNoRows || (err == nil && gateway == "") {
		update("pending", "no_active_call", false, 30*time.Second)
		return
	}
	if err != nil {
		if attempt >= 3 {
			update("failed", "session_lookup", true, 0)
		} else {
			update("pending", "session_lookup", true, time.Duration(1<<uint(attempt-1))*time.Second)
		}
		return
	}
	prefix := map[string]string{"user_text": "[User text input] ", "ui_input_request": "The background tool has asked the user for input. Request: ", "ui_input_received": "The background tool received the user's input. Content: "}[d.kind]
	ok, code := s.deliverDurableText(ctx, d.owner, d.kind, d.id, session, generation, prefix+d.text)
	if !ok {
		update("pending", code, false, 30*time.Second)
		return
	}
	completed := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	a := append([]any{completed.Format(time.RFC3339Nano)}, args...)
	a = append(a, attempt)
	r, err = tx.ExecContext(ctx, `UPDATE `+table+` SET status='delivered',last_error='',updated_at=? WHERE `+where+` AND status='pending' AND attempts=?`, a...)
	if err != nil {
		return
	}
	n, err = r.RowsAffected()
	if err != nil || n != 1 {
		return
	}
	if recordContextEvent(ctx, tx, contextEvent{owner: d.owner, session: session, kind: d.kind, id: d.id, role: contextEventRole(d.kind), text: d.text, at: completed}) != nil {
		return
	}
	_ = tx.Commit()
}
