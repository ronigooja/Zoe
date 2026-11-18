package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sync"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/uictl"
)

var uiRetryJitter = rand.Float64

func (s *Service) createRegisterUIJobs(ctx context.Context) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT v.id,v.workspace_id,s.owner_id,v.request FROM voice_turns v JOIN voice_sessions s ON s.id=v.session_id WHERE v.exits='ui' AND v.ui_handled=0 AND trim(coalesce(v.request,''))<>'' AND NOT EXISTS(SELECT 1 FROM ui_requests r WHERE r.voice_turn_id=v.id) ORDER BY v.confirmed_at LIMIT 25`)
	if err != nil {
		return
	}
	type turn struct{ id, w, o, request string }
	var turns []turn
	for rows.Next() {
		var t turn
		if rows.Scan(&t.id, &t.w, &t.o, &t.request) != nil {
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
		if _, err = tx.ExecContext(ctx, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,voice_turn_id,request,created_at,updated_at) VALUES(?,?,?,'register',?,?,?,?)`, id, t.w, t.o, t.id, t.request, at, at); err != nil {
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
	rows, err := tx.QueryContext(ctx, `SELECT e.workspace_id,e.cursor,e.aggregate_id,e.payload,e.event_type FROM events e JOIN ui_task_cursor c ON c.workspace_id=e.workspace_id WHERE e.event_type IN ('task.assistant_message','run.worker_question') AND e.cursor>c.last_cursor ORDER BY e.workspace_id,e.cursor LIMIT 100`)
	if err != nil {
		return
	}
	type event struct {
		w, task string
		kind    string
		cursor  int64
		payload []byte
	}
	var events []event
	for rows.Next() {
		var e event
		if rows.Scan(&e.w, &e.cursor, &e.task, &e.payload, &e.kind) != nil {
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
				Run   string `json:"run_id"`
				Entry string `json:"entry_id"`
				Task  string `json:"task_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(e.payload, &p) != nil {
			return
		}
		if p.Payload.Task != "" && p.Payload.Task != e.task {
			continue
		}
		var owner string
		err = tx.QueryRowContext(ctx, `SELECT owner_subject_id FROM tasks WHERE workspace_id=? AND id=? AND ui_display=1`, e.w, e.task).Scan(&owner)
		if err != nil && err != sql.ErrNoRows {
			return
		}
		var ready bool
		if err == nil {
			if err = tx.QueryRowContext(ctx, `SELECT trim(coalesce(result,''))<>'' OR interaction_state='awaiting_message' FROM tasks WHERE workspace_id=? AND id=?`, e.w, e.task).Scan(&ready); err != nil {
				return
			}
		}
		if p.Payload.Entry == "" {
			p.Payload.Entry = e.task + ":" + fmt.Sprint(e.cursor)
		}
		if err == nil && ready {
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
			if e.kind == "task.assistant_message" {
				if p.Payload.Run == "" || p.Payload.Entry == "" {
					return
				}
				if err := pinUIResultTx(ctx, tx, e.w, owner, e.task, id, p.Payload.Entry, e.cursor); err != nil {
					return
				}
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
	rows, err := s.db.QueryContext(ctx, `SELECT id,workspace_id,owner_subject_id,kind,dedupe_key,ui_request_id,attempts FROM ui_jobs WHERE status='pending' AND julianday(next_attempt_at)<=julianday(?) ORDER BY created_at,rowid LIMIT 25`, s.now().UTC().Format(time.RFC3339Nano))
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
	if err := s.db.QueryRowContext(ctx, `SELECT coalesce(task_id,'') FROM ui_requests WHERE id=? AND workspace_id=? AND owner_subject_id=?`, j.request, j.w, j.o).Scan(&task); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT request FROM ui_requests WHERE workspace_id=? AND owner_subject_id=? AND request IS NOT NULL AND ((?<>'' AND task_id=?) OR (task_id IS NULL AND id=?)) ORDER BY created_at,rowid`, j.w, j.o, task, task, j.request)
	if err != nil {
		return nil, err
	}
	var requests []string
	for rows.Next() {
		var r string
		if err = rows.Scan(&r); err != nil {
			rows.Close()
			return nil, err
		}
		requests = append(requests, r)
	}
	rows.Close()
	var facts *uictl.Task
	if task != "" {
		facts = &uictl.Task{TaskID: task}
		if err = s.db.QueryRowContext(ctx, `SELECT status,interaction_question FROM tasks WHERE workspace_id=? AND owner_subject_id=? AND id=?`, j.w, j.o, task).Scan(&facts.Status, &facts.Question); err != nil {
			return nil, err
		}
		facts.Result, err = s.uiPinnedResult(ctx, j.w, j.o, task, j.request)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if err == sql.ErrNoRows {
			var pinned bool
			if queryErr := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ui_result_sources WHERE request_id=?)`, j.request).Scan(&pinned); queryErr != nil {
				return nil, queryErr
			}
			if pinned {
				return nil, errors.New("ui_result_binding_invalid")
			}
		}
		if facts.Result != nil && *facts.Result == "" {
			facts.Result = nil
		}
		if facts.Question != "" {
			err = s.db.QueryRowContext(ctx, `SELECT a.id FROM pending_runtime_actions a JOIN tasks t ON t.id=a.task_id AND t.workspace_id=a.workspace_id WHERE t.id=? AND t.workspace_id=? AND t.owner_subject_id=? AND t.interaction_state='awaiting_message' AND t.interaction_run_id=a.run_id AND t.interaction_action_digest=a.action_digest AND a.decision='' AND a.authorization_used=0 AND julianday(a.expires_at)>julianday(?) AND NOT EXISTS(SELECT 1 FROM task_question_answers b WHERE b.question_id=a.id)`, task, j.w, j.o, s.now().UTC().Format(time.RFC3339Nano)).Scan(&facts.QuestionID)
			if err != nil {
				return nil, err
			}
		}
		if facts.Result == nil && facts.QuestionID == "" {
			return nil, errors.New("task_result_pending")
		}
	}
	rows, err = s.db.QueryContext(ctx, `SELECT surface_id,coalesce(request_id,''),version,container,state_json,width,height,deleted FROM ui_surfaces WHERE workspace_id=? AND owner_subject_id=? ORDER BY updated_at,surface_id`, j.w, j.o)
	if err != nil {
		return nil, err
	}
	var surfaces []uictl.Surface
	for rows.Next() {
		var sf uictl.Surface
		var raw []byte
		var width, height sql.NullInt64
		if err = rows.Scan(&sf.SurfaceID, &sf.RequestID, &sf.Version, &sf.Container, &raw, &width, &height, &sf.Deleted); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal(raw, &sf.Messages); err != nil {
			rows.Close()
			return nil, err
		}
		if width.Valid && height.Valid {
			sf.Size = &uictl.Size{Width: int(width.Int64), Height: int(height.Int64)}
		}
		surfaces = append(surfaces, sf)
	}
	rows.Close()
	var screen uictl.Size
	err = s.db.QueryRowContext(ctx, `SELECT width,height FROM ui_screens WHERE workspace_id=? AND owner_subject_id=?`, j.w, j.o).Scan(&screen.Width, &screen.Height)
	if err == sql.ErrNoRows {
		return uictl.BuildInput(s.now(), requests, facts, surfaces, nil)
	}
	if err != nil {
		return nil, err
	}
	return uictl.BuildInput(s.now(), requests, facts, surfaces, &screen)
}
func (s *Service) runUIJob(ctx context.Context, j uiJob) {
	now := s.now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `UPDATE ui_jobs SET attempts=attempts+1,next_attempt_at=?,updated_at=? WHERE id=? AND status='pending' AND attempts=? AND julianday(next_attempt_at)<=julianday(?) AND NOT EXISTS(SELECT 1 FROM ui_jobs earlier WHERE earlier.workspace_id=ui_jobs.workspace_id AND earlier.owner_subject_id=ui_jobs.owner_subject_id AND earlier.status='pending' AND earlier.rowid<ui_jobs.rowid)`, now.Add(30*time.Second).Format(time.RFC3339Nano), stamp, j.id, j.attempts, stamp)
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
	store := s.retry
	if store.DB == nil {
		store = backoff.Store{DB: s.db}
	}
	due, err := store.Due(ctx, "ui_controller", "", now)
	if err != nil || !due {
		s.failUIJob(ctx, j, "model_error", false)
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
		kind := backoff.Classify(err)
		if kind == backoff.Auth {
			if store.Record(ctx, "ui_controller", "", kind, s.now(), backoff.Interactive, uiRetryJitter) == nil {
				s.PublishRetryState()
			}
		}
		s.failUIJob(ctx, j, "model_error", kind == backoff.Transient)
		return
	}
	if store.Clear(ctx, "ui_controller", "") == nil {
		s.PublishRetryState()
	}
	var controllerInput uictl.Input
	if err = json.Unmarshal(input, &controllerInput); err != nil {
		s.failUIJob(ctx, j, "input_invalid", false)
		return
	}
	parsed, err := uictl.Parse(output, controllerInput.Screen)
	if err != nil {
		code := "validation_failed:" + uictl.FailureCode(err)
		call.Result(nil, code)
		s.failUIJob(ctx, j, code, false)
		return
	}
	call.Result(parsed, "")
	if err = s.commitUIJob(ctx, j, parsed, controllerInput.Task, controllerInput.Surfaces); err != nil {
		code := "storage_error"
		if errors.Is(err, errUIState) {
			code = "state_invalid"
		}
		if errors.Is(err, errUISnapshot) {
			code = "snapshot_changed"
		}
		s.failUIJob(ctx, j, code, code == "storage_error" || code == "snapshot_changed")
	}
}

var errUIState = errors.New("ui_state_invalid")
var errUISnapshot = errors.New("ui_snapshot_changed")

// UI presentation commits and sends for one owner serialize within this process.
// No database transaction spans gateway HTTP; other processes are not fenced.
var uiOwnerLocks [64]sync.Mutex

func (s *Service) lockUIOwner(workspace, owner string) func() {
	h := fnv.New64a()
	_, _ = h.Write([]byte(workspace + "\x00" + owner))
	mu := &uiOwnerLocks[h.Sum64()%uint64(len(uiOwnerLocks))]
	mu.Lock()
	return mu.Unlock
}

func (s *Service) commitUIJob(ctx context.Context, j uiJob, o uictl.Output, facts *uictl.Task, snapshot []uictl.Surface) error {
	unlock := s.lockUIOwner(j.w, j.o)
	defer unlock()
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
	rows, err := tx.QueryContext(ctx, `SELECT surface_id,coalesce(request_id,''),version,deleted FROM ui_surfaces WHERE workspace_id=? AND owner_subject_id=?`, j.w, j.o)
	if err != nil {
		return err
	}
	expected := map[string]uictl.Surface{}
	for _, sf := range snapshot {
		expected[sf.SurfaceID] = sf
	}
	matched := 0
	for rows.Next() {
		var id, request string
		var version int64
		var deleted bool
		if err = rows.Scan(&id, &request, &version, &deleted); err != nil {
			rows.Close()
			return err
		}
		sf, ok := expected[id]
		if !ok || sf.Version != version || sf.RequestID != request || sf.Deleted != deleted {
			rows.Close()
			return errUISnapshot
		}
		matched++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if matched != len(expected) {
		return errUISnapshot
	}
	if facts != nil {
		var currentResult string
		if err = tx.QueryRowContext(ctx, `SELECT result FROM tasks WHERE id=? AND workspace_id=? AND owner_subject_id=?`, facts.TaskID, j.w, j.o).Scan(&currentResult); err != nil {
			return err
		}
		expected := ""
		if facts.Result != nil {
			expected = *facts.Result
		}
		if currentResult != expected {
			return errUIState
		}
	}
	var question any
	if o.AwaitingInput {
		if facts == nil || facts.QuestionID == "" {
			return errUIState
		}
		var current bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_runtime_actions a JOIN tasks t ON t.id=a.task_id AND t.workspace_id=a.workspace_id WHERE a.id=? AND t.id=? AND t.workspace_id=? AND t.owner_subject_id=? AND t.interaction_state='awaiting_message' AND t.interaction_run_id=a.run_id AND t.interaction_action_digest=a.action_digest AND a.decision='' AND a.authorization_used=0 AND julianday(a.expires_at)>julianday(?) AND NOT EXISTS(SELECT 1 FROM task_question_answers b WHERE b.question_id=a.id))`, facts.QuestionID, facts.TaskID, j.w, j.o, s.now().UTC().Format(time.RFC3339Nano)).Scan(&current); err != nil {
			return err
		}
		if !current {
			return errUIState
		}
		question = facts.QuestionID
	}
	var raw []byte
	var version int64
	var deleted bool
	var width, height sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT state_json,version,width,height,deleted FROM ui_surfaces WHERE workspace_id=? AND owner_subject_id=? AND surface_id=?`, j.w, j.o, o.SurfaceID).Scan(&raw, &version, &width, &height, &deleted)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	var stored []json.RawMessage
	if len(raw) > 0 && json.Unmarshal(raw, &stored) != nil {
		return errUIState
	}
	// Tombstones retain identity/version, not an existing renderable document.
	if deleted {
		stored = nil
	}
	state, err := uictl.Apply(stored, o)
	if err != nil {
		return errUIState
	}
	if (len(raw) == 0 || deleted) && !state.Deleted {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM ui_surfaces WHERE workspace_id=? AND owner_subject_id=? AND deleted=0`, j.w, j.o).Scan(&count); err != nil {
			return err
		}
		if count >= 64 {
			return errUIState
		}
	}
	at := s.now().UTC().Format(time.RFC3339Nano)
	version++
	if state.Deleted {
		data, _ := json.Marshal(state.Messages)
		_, err = tx.ExecContext(ctx, `INSERT INTO ui_surfaces(workspace_id,owner_subject_id,surface_id,container,state_json,version,request_id,deleted,created_at,updated_at) VALUES(?,?,?,?,?,?,?,1,?,?) ON CONFLICT(workspace_id,owner_subject_id,surface_id) DO UPDATE SET state_json=excluded.state_json,deleted=1,version=excluded.version,request_id=excluded.request_id,updated_at=excluded.updated_at`, j.w, j.o, o.SurfaceID, o.Container, string(data), version, j.request, at, at)
	} else {
		data, _ := json.Marshal(state.Messages)
		var newWidth, newHeight any
		if o.Size != nil {
			newWidth, newHeight = o.Size.Width, o.Size.Height
			width, height = sql.NullInt64{Int64: int64(o.Size.Width), Valid: true}, sql.NullInt64{Int64: int64(o.Size.Height), Valid: true}
		}
		// Omitted size preserves the previously stored dimensions on updates.
		_, err = tx.ExecContext(ctx, `INSERT INTO ui_surfaces(workspace_id,owner_subject_id,surface_id,container,state_json,version,created_at,updated_at,width,height) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(workspace_id,owner_subject_id,surface_id) DO UPDATE SET container=excluded.container,state_json=excluded.state_json,version=excluded.version,updated_at=excluded.updated_at,width=COALESCE(excluded.width,ui_surfaces.width),height=COALESCE(excluded.height,ui_surfaces.height)`, j.w, j.o, o.SurfaceID, o.Container, string(data), version, at, at, newWidth, newHeight)
	}
	if err != nil {
		return err
	}
	if !state.Deleted {
		if _, err = tx.ExecContext(ctx, `UPDATE ui_surfaces SET request_id=?,deleted=0 WHERE workspace_id=? AND owner_subject_id=? AND surface_id=?`, j.request, j.w, j.o, o.SurfaceID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ui_requests SET task_id=CASE WHEN ? THEN NULL ELSE task_id END,surface_id=?,surface_version=?,question_id=?,feedback=?,awaiting_input=?,voice_input=?,input_note=?,status='generated',updated_at=? WHERE id=?`, o.VoiceInput, o.SurfaceID, version, question, o.Feedback, o.AwaitingInput, o.VoiceInput, o.InputNote, at, j.request); err != nil {
		return err
	}
	eventPayload := map[string]any{"request_id": j.request, "surface_id": o.SurfaceID, "container": o.Container, "awaiting_input": o.AwaitingInput, "voice_input": o.VoiceInput, "messages": state.Messages}
	eventPayload["surface_version"] = version
	eventPayload["deleted"] = state.Deleted
	if o.AwaitingInput {
		eventPayload["task_id"], eventPayload["question_id"] = facts.TaskID, facts.QuestionID
	}
	if !state.Deleted && width.Valid && height.Valid {
		eventPayload["size"] = uictl.Size{Width: int(width.Int64), Height: int(height.Int64)}
	}
	payload, _ := json.Marshal(eventPayload)
	if s.uiEventServer == nil {
		return errors.New("event_server_missing")
	}
	if err = s.uiEventServer.AppendEventsTx(ctx, tx, contracts.Principal{WorkspaceID: j.w, SubjectID: j.o}, []contracts.Event{{Type: "ui.surface", AggregateType: "ui_surface", AggregateID: o.SurfaceID, AggregateVersion: version, Payload: payload, OccurredAt: s.now()}}); err != nil {
		return err
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
		_, _ = s.db.ExecContext(ctx, `UPDATE ui_jobs SET last_error=?,next_attempt_at=?,updated_at=? WHERE id=? AND status='pending' AND attempts=?`, code, s.now().Add(backoff.Delay(j.attempts, backoff.Interactive.Base, backoff.Interactive.Cap, uiRetryJitter)).UTC().Format(time.RFC3339Nano), at, j.id, j.attempts)
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
