package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/uictl"
)

func (s *Service) createRegisterUIJobs(ctx context.Context) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT v.id,v.workspace_id,s.owner_id,v.content,coalesce(v.restatement,'') FROM voice_turns v JOIN voice_sessions s ON s.id=v.session_id WHERE v.exits='ui' AND v.ui_handled=0 AND NOT EXISTS(SELECT 1 FROM ui_requests r WHERE r.voice_turn_id=v.id) ORDER BY v.confirmed_at LIMIT 25`)
	if err != nil {
		return
	}
	type turn struct{ id, w, o, text, re string }
	var turns []turn
	for rows.Next() {
		var t turn
		if rows.Scan(&t.id, &t.w, &t.o, &t.text, &t.re) != nil {
			rows.Close()
			return
		}
		turns = append(turns, t)
	}
	rows.Close()
	at := s.now().UTC().Format(time.RFC3339Nano)
	for _, t := range turns {
		id, e := randomID()
		if e != nil {
			return
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,voice_turn_id,restatement,utterance,created_at,updated_at) VALUES(?,?,?,'register',?,?,?,?,?)`, id, t.w, t.o, t.id, t.re, t.text, at, at); err != nil {
			return
		}
		if err = s.insertUIJobTx(ctx, tx, t.w, t.o, "register", t.id, id, at); err != nil {
			return
		}
	}
	_ = tx.Commit()
}
func (s *Service) insertUIJobTx(ctx context.Context, tx *sql.Tx, w, o, kind, key, request, at string) error {
	id, err := randomID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO ui_jobs(id,workspace_id,owner_subject_id,kind,dedupe_key,ui_request_id,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, w, o, kind, kind+":"+key, request, at, at, at)
	return err
}

// consumeUITaskEvents uses workspace-local cursors independently of result delivery.
func (s *Service) consumeUITaskEvents(ctx context.Context) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO ui_task_cursor(workspace_id) SELECT id FROM workspaces`); err != nil {
		return
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.workspace_id,e.cursor,e.aggregate_id,e.payload FROM events e JOIN ui_task_cursor c ON c.workspace_id=e.workspace_id WHERE e.event_type='task.assistant_message' AND e.cursor>c.last_cursor ORDER BY e.workspace_id,e.cursor LIMIT 100`)
	if err != nil {
		return
	}
	type event struct {
		w, task string
		cursor  int64
		payload []byte
	}
	var events []event
	for rows.Next() {
		var e event
		if rows.Scan(&e.w, &e.cursor, &e.task, &e.payload) != nil {
			rows.Close()
			return
		}
		events = append(events, e)
	}
	rows.Close()
	at := s.now().UTC().Format(time.RFC3339Nano)
	for _, e := range events {
		var p struct {
			Payload struct {
				Entry string `json:"entry_id"`
				Task  string `json:"task_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(e.payload, &p) != nil {
			return
		}
		if p.Payload.Task != "" {
			e.task = p.Payload.Task
		}
		var owner string
		err = tx.QueryRowContext(ctx, `SELECT owner_subject_id FROM tasks WHERE workspace_id=? AND id=? AND ui_display=1`, e.w, e.task).Scan(&owner)
		if err != nil && err != sql.ErrNoRows {
			return
		}
		if err == nil && p.Payload.Entry != "" {
			var already bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ui_jobs WHERE dedupe_key=?)`, "task_entry:"+p.Payload.Entry).Scan(&already); err != nil {
				return
			}
			if already {
				if _, err = tx.ExecContext(ctx, `UPDATE ui_task_cursor SET last_cursor=? WHERE workspace_id=? AND last_cursor<?`, e.cursor, e.w, e.cursor); err != nil {
					return
				}
				continue
			}
			// Each task-entry job has its own request receipt and feedback lifecycle.
			id, x := randomID()
			if x != nil {
				return
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,task_id,created_at,updated_at) VALUES(?,?,?,'task',?,?,?)`, id, e.w, owner, e.task, at, at); err != nil {
				return
			}
			if err = s.insertUIJobTx(ctx, tx, e.w, owner, "task_entry", p.Payload.Entry, id, at); err != nil {
				return
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE ui_task_cursor SET last_cursor=? WHERE workspace_id=? AND last_cursor<?`, e.cursor, e.w, e.cursor); err != nil {
			return
		}
	}
	_ = tx.Commit()
}

type uiJob struct {
	id, w, o, kind, key, request string
	attempts                     int
}

func (s *Service) dispatchUIJobs(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,workspace_id,owner_subject_id,kind,dedupe_key,ui_request_id,attempts FROM ui_jobs WHERE status='pending' AND next_attempt_at<=? ORDER BY created_at,rowid LIMIT 25`, s.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return
	}
	var jobs []uiJob
	for rows.Next() {
		var j uiJob
		if rows.Scan(&j.id, &j.w, &j.o, &j.kind, &j.key, &j.request, &j.attempts) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	for _, j := range jobs {
		s.runUIJob(ctx, j)
	}
}
func (s *Service) uiInput(ctx context.Context, j uiJob) ([]byte, error) {
	var task string
	if err := s.db.QueryRowContext(ctx, `SELECT coalesce(task_id,'') FROM ui_requests WHERE id=?`, j.request).Scan(&task); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT coalesce(utterance,''),coalesce(restatement,'') FROM ui_requests WHERE workspace_id=? AND owner_subject_id=? AND ((?<>'' AND task_id=? AND utterance IS NOT NULL) OR (task_id IS NULL AND id=?)) ORDER BY created_at,rowid`, j.w, j.o, task, task, j.request)
	if err != nil {
		return nil, err
	}
	var utterances, restatements []string
	for rows.Next() {
		var u, r string
		if err = rows.Scan(&u, &r); err != nil {
			rows.Close()
			return nil, err
		}
		utterances = append(utterances, u)
		restatements = append(restatements, r)
	}
	rows.Close()
	var message string
	if j.kind == "task_entry" {
		if err = s.db.QueryRowContext(ctx, `SELECT content FROM task_entries WHERE workspace_id=? AND task_id=? AND id=? AND kind='assistant_message'`, j.w, task, j.key[len("task_entry:"):]).Scan(&message); err != nil {
			return nil, err
		}
	}
	rows, err = s.db.QueryContext(ctx, `SELECT container,state_json FROM ui_surfaces WHERE workspace_id=? AND owner_subject_id=? ORDER BY updated_at,surface_id LIMIT 65`, j.w, j.o)
	if err != nil {
		return nil, err
	}
	var surfaces []uictl.Surface
	for rows.Next() {
		var sf uictl.Surface
		var raw []byte
		if err = rows.Scan(&sf.Container, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal(raw, &sf.Messages); err != nil {
			rows.Close()
			return nil, err
		}
		surfaces = append(surfaces, sf)
	}
	rows.Close()
	return uictl.BuildInput(s.now(), utterances, restatements, message, surfaces)
}
func (s *Service) runUIJob(ctx context.Context, j uiJob) {
	now := s.now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `UPDATE ui_jobs SET attempts=attempts+1,next_attempt_at=?,updated_at=? WHERE id=? AND status='pending' AND attempts=? AND next_attempt_at<=? AND NOT EXISTS(SELECT 1 FROM ui_jobs earlier WHERE earlier.workspace_id=ui_jobs.workspace_id AND earlier.owner_subject_id=ui_jobs.owner_subject_id AND earlier.status='pending' AND earlier.rowid<ui_jobs.rowid)`, now.Add(30*time.Second).Format(time.RFC3339Nano), stamp, j.id, j.attempts, stamp)
	if err != nil {
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return
	}
	j.attempts++
	if j.attempts > 3 {
		s.failUIJob(ctx, j, "model_error", false)
		return
	}
	if !s.uiController.Configured() {
		s.failUIJob(ctx, j, "ui_controller_disabled", false)
		return
	}
	input, err := s.uiInput(ctx, j)
	if err != nil {
		s.failUIJob(ctx, j, "input_invalid", false)
		return
	}
	callCtx, cancel := context.WithTimeout(logging.WithUserHash(ctx, logging.UserHash(j.o)), 25*time.Second)
	defer cancel()
	prompt := uictl.Prompt + "\n" + uictl.SubsetDefinitions
	call := logging.ModelCallFromContext(callCtx, "ui_controller")
	call.Input(prompt, string(input))
	output, err := s.uiController.Complete(callCtx, prompt, string(input))
	call.Output(output)
	if err != nil {
		call.Result(nil, "model_error")
		s.failUIJob(ctx, j, "model_error", true)
		return
	}
	parsed, err := uictl.Parse(output)
	if err != nil {
		call.Result(nil, "validation_failed")
		s.failUIJob(ctx, j, "validation_failed", false)
		return
	}
	call.Result(parsed, "")
	if err = s.commitUIJob(ctx, j, parsed); err != nil {
		code := "storage_error"
		if errors.Is(err, errUIState) {
			code = "state_invalid"
		}
		s.failUIJob(ctx, j, code, code == "storage_error")
	}
}

var errUIState = errors.New("ui_state_invalid")

func (s *Service) commitUIJob(ctx context.Context, j uiJob, o uictl.Output) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	var attempts int
	if err = tx.QueryRowContext(ctx, `SELECT status,attempts FROM ui_jobs WHERE id=?`, j.id).Scan(&status, &attempts); err != nil {
		return err
	}
	if status != "pending" || attempts != j.attempts {
		return nil
	}
	var raw []byte
	var version int64
	err = tx.QueryRowContext(ctx, `SELECT state_json,version FROM ui_surfaces WHERE workspace_id=? AND owner_subject_id=? AND surface_id=?`, j.w, j.o, o.SurfaceID).Scan(&raw, &version)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	var stored []json.RawMessage
	if len(raw) > 0 && json.Unmarshal(raw, &stored) != nil {
		return errUIState
	}
	state, err := uictl.Apply(stored, o)
	if err != nil {
		return errUIState
	}
	if len(raw) == 0 && !state.Deleted {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM ui_surfaces WHERE workspace_id=? AND owner_subject_id=?`, j.w, j.o).Scan(&count); err != nil {
			return err
		}
		if count >= 64 {
			return errUIState
		}
	}
	at := s.now().UTC().Format(time.RFC3339Nano)
	version++
	if state.Deleted {
		_, err = tx.ExecContext(ctx, `DELETE FROM ui_surfaces WHERE workspace_id=? AND owner_subject_id=? AND surface_id=?`, j.w, j.o, o.SurfaceID)
	} else {
		data, _ := json.Marshal(state.Messages)
		_, err = tx.ExecContext(ctx, `INSERT INTO ui_surfaces(workspace_id,owner_subject_id,surface_id,container,state_json,version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(workspace_id,owner_subject_id,surface_id) DO UPDATE SET container=excluded.container,state_json=excluded.state_json,version=excluded.version,updated_at=excluded.updated_at`, j.w, j.o, o.SurfaceID, o.Container, string(data), version, at, at)
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ui_requests SET surface_id=?,awaiting_input=?,input_note=?,status='generated',updated_at=? WHERE id=?`, o.SurfaceID, o.AwaitingInput, o.InputNote, at, j.request); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"request_id": j.request, "surface_id": o.SurfaceID, "container": o.Container, "awaiting_input": o.AwaitingInput, "messages": state.Messages})
	if s.uiEventServer == nil {
		return errors.New("event_server_missing")
	}
	if err = s.uiEventServer.AppendEventsTx(ctx, tx, contracts.Principal{WorkspaceID: j.w, SubjectID: j.o}, []contracts.Event{{Type: "ui.surface", AggregateType: "ui_surface", AggregateID: o.SurfaceID, AggregateVersion: version, Payload: payload, OccurredAt: s.now()}}); err != nil {
		return err
	}
	if !o.AwaitingInput {
		if err = s.queueUIFeedbackTx(ctx, tx, j.request, j.w, j.o, o.Feedback, at); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ui_jobs SET status='done',updated_at=? WHERE id=?`, at, j.id); err != nil {
		return err
	}
	if j.kind == "register" {
		if _, err = tx.ExecContext(ctx, `UPDATE voice_turns SET ui_handled=1 WHERE id=(SELECT voice_turn_id FROM ui_requests WHERE id=?)`, j.request); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Service) failUIJob(ctx context.Context, j uiJob, code string, retry bool) {
	logging.Warnf("ui_controller error_kind=%s", code)
	at := s.now().UTC().Format(time.RFC3339Nano)
	if retry && j.attempts < 3 {
		_, _ = s.db.ExecContext(ctx, `UPDATE ui_jobs SET last_error=?,next_attempt_at=?,updated_at=? WHERE id=? AND status='pending' AND attempts=?`, code, s.now().Add(time.Duration(1<<uint(j.attempts-1))*time.Second).UTC().Format(time.RFC3339Nano), at, j.id, j.attempts)
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE ui_jobs SET status='failed',last_error=?,updated_at=? WHERE id=? AND status='pending' AND attempts=?`, code, at, j.id, j.attempts)
	if err != nil {
		return
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ui_requests SET status='failed',updated_at=? WHERE id=?`, at, j.request); err != nil {
		return
	}
	if err = s.queueUIFeedbackTx(ctx, tx, j.request, j.w, j.o, stewardErrorSpeech, at); err != nil {
		return
	}
	if j.kind == "register" {
		if _, err = tx.ExecContext(ctx, `UPDATE voice_turns SET ui_handled=1 WHERE id=(SELECT voice_turn_id FROM ui_requests WHERE id=?)`, j.request); err != nil {
			return
		}
	}
	_ = tx.Commit()
}
