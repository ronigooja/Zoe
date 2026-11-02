package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/screen"
)

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, apiError(401, "unauthenticated", "device authentication required", nil))
		return
	}
	tx, err := s.store.DB().BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback()
	snapshot, err := loadSnapshot(r.Context(), tx, p.WorkspaceID, p.SubjectID, s.cfg.MaxRuns)
	if err != nil {
		writeError(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, snapshot)
}

func (s *Server) handleScreen(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, apiError(401, "unauthenticated", "device authentication required", nil))
		return
	}
	tx, err := s.store.DB().BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback()
	snapshot, err := loadSnapshot(r.Context(), tx, p.WorkspaceID, p.SubjectID, s.cfg.MaxRuns)
	if err != nil {
		writeError(w, err)
		return
	}
	notifications := []contracts.Notification{}
	rows, err := tx.QueryContext(r.Context(), `SELECT id,kind,title,body,aggregate_type,aggregate_id,source_event_id,created_at,read_at FROM notifications WHERE workspace_id=? AND subject_id=? ORDER BY created_at,id`, p.WorkspaceID, p.SubjectID)
	if err != nil {
		writeError(w, err)
		return
	}
	for rows.Next() {
		var n contracts.Notification
		var created string
		var read sql.NullString
		if err := rows.Scan(&n.ID, &n.Kind, &n.Title, &n.Body, &n.AggregateType, &n.AggregateID, &n.SourceEventID, &created, &read); err != nil {
			rows.Close()
			writeError(w, err)
			return
		}
		n.CreatedAt = parseTime(created)
		if read.Valid {
			v := parseTime(read.String)
			n.ReadAt = &v
		}
		notifications = append(notifications, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeError(w, err)
		return
	}
	rows.Close()
	if err = tx.Commit(); err != nil {
		writeError(w, err)
		return
	}
	out, err := screen.Build(snapshot.Tasks, notifications, parseInt64(snapshot.EventCursor), time.Now().UTC())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, out)
}

func parseInt64(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }

func loadSnapshot(ctx context.Context, tx *sql.Tx, workspace, subject string, maxRuns int) (contracts.Snapshot, error) {
	out := contracts.Snapshot{WorkspaceID: workspace, SubjectID: subject, ServerVersion: "1", Projects: []contracts.Project{}, Tasks: []contracts.Task{}, Approvals: []contracts.Approval{}, Runs: []contracts.Run{}}
	var cursor int64
	if err := tx.QueryRowContext(ctx, `SELECT current_cursor FROM event_counters WHERE workspace_id=?`, workspace).Scan(&cursor); err != nil {
		return out, err
	}
	out.EventCursor = strconv.FormatInt(cursor, 10)
	capacity, err := loadExecutionCapacityWithLimit(ctx, tx, maxRuns)
	if err != nil {
		return out, err
	}
	out.ExecutionCapacity = capacity
	if err := tx.QueryRowContext(ctx, `SELECT lineage_id,generation,deletion_sequence FROM zoe_instance_state WHERE singleton=1`).Scan(&out.Authority.InstanceLineageID, &out.Authority.Generation, &out.Authority.DeletionSequence); err != nil {
		return out, err
	}
	projects, err := tx.QueryContext(ctx, `SELECT id,name,description,status,version,created_at,updated_at FROM projects p WHERE workspace_id=? AND (owner_subject_id=?) ORDER BY created_at,id`, workspace, subject)
	if err != nil {
		return out, err
	}
	for projects.Next() {
		var p contracts.Project
		var created, updated string
		if err = projects.Scan(&p.ID, &p.Name, &p.Description, &p.Status, &p.Version, &created, &updated); err != nil {
			projects.Close()
			return out, err
		}
		p.CreatedAt = parseTime(created)
		p.UpdatedAt = parseTime(updated)
		out.Projects = append(out.Projects, p)
	}
	if err = projects.Err(); err != nil {
		projects.Close()
		return out, err
	}
	projects.Close()
	tasks, err := tx.QueryContext(ctx, `SELECT id,coalesce(project_id,''),title,goal,source,profile_id,status,version,created_at,updated_at,accepted_at,started_at,finished_at,result,error,interaction_state,interaction_scope,interaction_run_id,interaction_action_digest,interaction_expires_at,interaction_expected_version FROM tasks t WHERE workspace_id=? AND (owner_subject_id=?) ORDER BY created_at,id`, workspace, subject)
	if err != nil {
		return out, err
	}
	for tasks.Next() {
		var t contracts.Task
		var created, updated string
		var accepted, started, finished, interactionExpires sql.NullString
		if err = tasks.Scan(&t.ID, &t.ProjectID, &t.Title, &t.Goal, &t.Source, &t.ProfileID, &t.Status, &t.Version, &created, &updated, &accepted, &started, &finished, &t.Result, &t.Error, &t.InteractionState, &t.InteractionScope, &t.InteractionRunID, &t.InteractionActionDigest, &interactionExpires, &t.InteractionExpectedVersion); err != nil {
			tasks.Close()
			return out, err
		}
		t.CreatedAt = parseTime(created)
		t.UpdatedAt = parseTime(updated)
		if accepted.Valid {
			v := parseTime(accepted.String)
			t.AcceptedAt = &v
		}
		if started.Valid {
			v := parseTime(started.String)
			t.StartedAt = &v
		}
		if finished.Valid {
			v := parseTime(finished.String)
			t.FinishedAt = &v
		}
		if interactionExpires.Valid {
			v := parseTime(interactionExpires.String)
			t.InteractionExpiresAt = &v
		}
		t.Messages, err = loadTaskMessages(ctx, tx, workspace, t.ID)
		if err != nil {
			tasks.Close()
			return out, err
		}
		if t.Status == "awaiting_user" {
			var canAccept int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM runs r JOIN evidence e ON e.run_id=r.id WHERE r.workspace_id=? AND r.task_id=? AND r.status='verification_pending' AND e.kind='agent_result' AND e.source='runner' AND e.verified=0`, workspace, t.ID).Scan(&canAccept); err != nil {
				tasks.Close()
				return out, err
			}
			t.CanAcceptResult = canAccept > 0
		}
		t.Evidence, err = loadEvidence(ctx, tx, workspace, t.ID)
		if err != nil {
			tasks.Close()
			return out, err
		}
		if t.Status == "queued" {
			var dependencies int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM task_dependencies WHERE task_id=? AND status<>'satisfied'`, t.ID).Scan(&dependencies); err != nil {
				tasks.Close()
				return out, err
			}
			switch {
			case dependencies > 0:
				t.QueueReason = "dependencies_pending"
			case capacity.DispatchBlocked:
				t.QueueReason = capacity.ReasonCode
			default:
				t.QueueReason = "pending_dispatch"
			}
		}
		out.Tasks = append(out.Tasks, t)
	}
	if err = tasks.Err(); err != nil {
		tasks.Close()
		return out, err
	}
	tasks.Close()
	approvals, err := tx.QueryContext(ctx, `SELECT a.id,a.task_id,a.task_version,a.status,a.prompt,a.version,a.created_at,a.expires_at,a.requested_action,a.requested_action_digest FROM approvals a JOIN tasks t ON t.id=a.task_id AND t.workspace_id=a.workspace_id WHERE a.workspace_id=? AND (t.owner_subject_id=?) ORDER BY a.created_at,a.id`, workspace, subject)
	if err != nil {
		return out, err
	}
	for approvals.Next() {
		var a contracts.Approval
		var created string
		var expires sql.NullString
		var action []byte
		if err = approvals.Scan(&a.ID, &a.TaskID, &a.TaskVersion, &a.Status, &a.Prompt, &a.Version, &created, &expires, &action, &a.RequestedActionDigest); err != nil {
			approvals.Close()
			return out, err
		}
		a.CreatedAt = parseTime(created)
		if expires.Valid {
			v := parseTime(expires.String)
			a.ExpiresAt = &v
		}
		if len(action) > 0 {
			a.RequestedAction = json.RawMessage(action)
		}
		out.Approvals = append(out.Approvals, a)
	}
	if err = approvals.Err(); err != nil {
		approvals.Close()
		return out, err
	}
	approvals.Close()
	runs, err := tx.QueryContext(ctx, `SELECT r.id,r.task_id,r.attempt_id,r.status,r.desired_control,r.control_version,r.started_at,r.stopped_at FROM runs r JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id WHERE r.workspace_id=? AND (t.owner_subject_id=?) ORDER BY r.started_at,r.id`, workspace, subject)
	if err != nil {
		return out, err
	}
	for runs.Next() {
		var v contracts.Run
		var started string
		var stopped sql.NullString
		if err = runs.Scan(&v.ID, &v.TaskID, &v.AttemptID, &v.Status, &v.DesiredControl, &v.ControlVersion, &started, &stopped); err != nil {
			runs.Close()
			return out, err
		}
		v.StartedAt = parseTime(started)
		v.TerminationConfirmed = v.Status == "stopped" || v.Status == "verification_pending" || v.Status == "verified" || v.Status == "failed"
		if stopped.Valid {
			t := parseTime(stopped.String)
			v.StoppedAt = &t
		}
		v.Evidence, err = loadRunEvidence(ctx, tx, workspace, v.ID)
		if err != nil {
			runs.Close()
			return out, err
		}
		out.Runs = append(out.Runs, v)
	}
	if err = runs.Err(); err != nil {
		runs.Close()
		return out, err
	}
	runs.Close()
	return out, nil
}

func loadTaskMessages(ctx context.Context, tx *sql.Tx, workspace, taskID string) ([]contracts.TaskMessage, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,task_id,kind,content,created_at FROM task_entries WHERE workspace_id=? AND task_id=? ORDER BY created_at,id`, workspace, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := []contracts.TaskMessage{}
	for rows.Next() {
		var m contracts.TaskMessage
		var created string
		if err := rows.Scan(&m.ID, &m.TaskID, &m.Kind, &m.Content, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(created)
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

func loadEvidence(ctx context.Context, tx *sql.Tx, workspace, task string) ([]contracts.Evidence, error) {
	return queryEvidence(ctx, tx, `SELECT id,kind,summary,source,created_at,verified FROM evidence WHERE workspace_id=? AND task_id=? ORDER BY created_at,id`, workspace, task)
}
func loadRunEvidence(ctx context.Context, tx *sql.Tx, workspace, run string) ([]contracts.Evidence, error) {
	return queryEvidence(ctx, tx, `SELECT id,kind,summary,source,created_at,verified FROM evidence WHERE workspace_id=? AND run_id=? ORDER BY created_at,id`, workspace, run)
}
func queryEvidence(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]contracts.Evidence, error) {
	out := []contracts.Evidence{}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var e contracts.Evidence
		var created string
		var verified int
		if err = rows.Scan(&e.ID, &e.Kind, &e.Summary, &e.Source, &created, &verified); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(created)
		e.Verified = verified == 1
		out = append(out, e)
	}
	return out, rows.Err()
}
func parseTime(s string) time.Time { t, _ := time.Parse(time.RFC3339Nano, s); return t }

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, apiError(401, "unauthenticated", "device authentication required", nil))
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, apiError(500, "stream_unavailable", "streaming is unavailable", nil))
		return
	}
	cursorText := r.Header.Get("Last-Event-ID")
	cursor := int64(0)
	if cursorText != "" {
		n, err := strconv.ParseInt(cursorText, 10, 64)
		if err != nil || n < 0 {
			s.writeSnapshotRequired(w, r.Context(), "invalid_cursor")
			return
		}
		cursor = n
	}
	var high int64
	if err := s.store.DB().QueryRowContext(r.Context(), `SELECT current_cursor FROM event_counters WHERE workspace_id=?`, p.WorkspaceID).Scan(&high); err != nil {
		writeError(w, err)
		return
	}
	if cursor > high {
		s.writeSnapshotRequired(w, r.Context(), "cursor_ahead")
		return
	}
	oldest := high + 1
	var earliest sql.NullInt64
	_ = s.store.DB().QueryRowContext(r.Context(), `SELECT min(cursor) FROM events WHERE workspace_id=?`, p.WorkspaceID).Scan(&earliest)
	if earliest.Valid {
		oldest = earliest.Int64
	}
	if cursor > 0 && oldest > 1 && cursor < oldest-1 {
		s.writeSnapshotRequired(w, r.Context(), "cursor_expired")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	flusher := w.(http.Flusher)
	flusher.Flush()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		tx, err := s.store.DB().BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return
		}
		if err = requireOwnerSessionTx(r.Context(), tx, p); err != nil {
			_ = tx.Rollback()
			return
		}

		rows, err := tx.QueryContext(r.Context(), `SELECT e.cursor,e.event_type,e.payload,(e.subject_id=? OR ?=1 OR e.aggregate_type='workspace' OR (e.aggregate_type='project' AND EXISTS(SELECT 1 FROM projects p WHERE p.workspace_id=e.workspace_id AND p.id=e.aggregate_id AND (p.owner_subject_id=?))) OR (e.aggregate_type='task' AND EXISTS(SELECT 1 FROM tasks t WHERE t.workspace_id=e.workspace_id AND t.id=e.aggregate_id AND (t.owner_subject_id=?))) OR (e.aggregate_type='run' AND EXISTS(SELECT 1 FROM runs r JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id WHERE r.workspace_id=e.workspace_id AND r.id=e.aggregate_id AND (t.owner_subject_id=?)))) AS visible FROM events e WHERE e.workspace_id=? AND e.cursor>? ORDER BY e.cursor LIMIT 100`, p.SubjectID, true, p.SubjectID, p.SubjectID, p.SubjectID, p.WorkspaceID, cursor)
		if err != nil {
			_ = tx.Rollback()
			return
		}
		type record struct {
			cursor  int64
			kind    string
			payload []byte
			visible bool
		}
		records := make([]record, 0, 100)
		for rows.Next() {
			var v record
			if err = rows.Scan(&v.cursor, &v.kind, &v.payload, &v.visible); err != nil {
				rows.Close()
				_ = tx.Rollback()
				return
			}
			records = append(records, v)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			_ = tx.Rollback()
			return
		}
		rows.Close()
		if tx.Commit() != nil {
			return
		}
		for _, v := range records {
			if v.visible {
				fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", v.cursor, v.kind, v.payload)
				flusher.Flush()
			}
			cursor = v.cursor
		}
		if len(records) > 0 {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) writeSnapshotRequired(w http.ResponseWriter, ctx context.Context, reason string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	var lineage, deletionSequence string
	var generation int64
	_ = s.store.DB().QueryRowContext(ctx, `SELECT lineage_id,generation,deletion_sequence FROM zoe_instance_state WHERE singleton=1`).Scan(&lineage, &generation, &deletionSequence)
	authority := map[string]any{"instance_lineage_id": lineage, "generation": generation, "deletion_sequence": deletionSequence}
	data, _ := json.Marshal(map[string]any{"reason": reason, "authority": authority})
	_, _ = fmt.Fprintf(w, "event: snapshot_required\ndata: %s\n\n", data)
}

var _ context.Context
var _ = http.StatusOK
