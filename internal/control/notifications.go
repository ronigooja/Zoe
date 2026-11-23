package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			writeError(w, apiError(http.StatusBadRequest, "invalid_request", "limit must be between 1 and 100", nil))
			return
		}
		limit = value
	}
	query := `SELECT id,kind,title,body,aggregate_type,aggregate_id,source_event_id,created_at,read_at FROM notifications WHERE subject_id=?`
	args := []any{p.SubjectID}
	if r.URL.Query().Get("unread") == "true" {
		query += ` AND read_at IS NULL`
	} else if value := r.URL.Query().Get("unread"); value != "" && value != "false" {
		writeError(w, apiError(http.StatusBadRequest, "invalid_request", "unread must be true or false", nil))
		return
	}
	query += ` ORDER BY created_at DESC,id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.store.DB().QueryContext(r.Context(), query, args...)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()
	items := make([]contracts.Notification, 0)
	for rows.Next() {
		var item contracts.Notification
		var created string
		var read sql.NullString
		if err = rows.Scan(&item.ID, &item.Kind, &item.Title, &item.Body, &item.AggregateType, &item.AggregateID, &item.SourceEventID, &created, &read); err != nil {
			writeError(w, err)
			return
		}
		item.CreatedAt = parseTime(created)
		if read.Valid {
			t := parseTime(read.String)
			item.ReadAt = &t
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": items})
}

func enqueueEventNotification(ctx context.Context, tx *sql.Tx, e contracts.Event) error {
	if e.Type != "run.runtime_terminated" && e.Type != "run.stop_confirmed" {
		return nil
	}
	var payload struct {
		TaskID     string `json:"task_id"`
		TaskStatus string `json:"task_status"`
	}
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		return err
	}
	kind, title, body := "", "", ""
	switch payload.TaskStatus {
	case "awaiting_user":
		kind, title, body = "task.result_ready", "Result ready for review", "A task result is ready for your decision."
	case "failed":
		kind, title, body = "task.failed", "Task failed", "Task execution ended without an accepted result."
	case "canceled":
		kind, title, body = "task.stopped", "Task stopped", "Task execution has stopped."
	default:
		return nil
	}
	if payload.TaskID == "" {
		return errors.New("notification event is missing task identity")
	}
	var subjectID string
	err := tx.QueryRowContext(ctx, `SELECT d.subject_id FROM tasks t JOIN actors d ON d.id=t.created_by_device_id WHERE t.id=?`, payload.TaskID).Scan(&subjectID)
	if err != nil {
		return err
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO notifications(id,subject_id,source_event_id,aggregate_type,aggregate_id,kind,title,body,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, subjectID, e.EventID, "task", payload.TaskID, kind, title, body, e.OccurredAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT id FROM notifications WHERE subject_id=? AND source_event_id=?`, subjectID, e.EventID).Scan(&id); err != nil {
		return err
	}
	return enqueuePushDeliveries(ctx, tx, id, subjectID, e.EventID)
}

type markNotificationReadPayload struct {
	NotificationID string `json:"notification_id"`
}

func (s *Server) commandNotificationMarkRead(ctx context.Context, tx *sql.Tx, c CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	var p markNotificationReadPayload
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	p.NotificationID = strings.TrimSpace(p.NotificationID)
	if p.NotificationID == "" {
		return contracts.CommandResult{}, nil, apiError(http.StatusBadRequest, "invalid_request", "notification_id is required", nil)
	}
	var readAt sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT read_at FROM notifications WHERE id=? AND subject_id=?`, p.NotificationID, c.Principal.SubjectID).Scan(&readAt)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "not_found", "notification not found", nil)
	}
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if readAt.Valid {
		return contracts.CommandResult{ResourceType: "notification", ResourceID: p.NotificationID}, nil, nil
	}
	at := c.AcceptedAt.Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `UPDATE notifications SET read_at=? WHERE id=? AND subject_id=? AND read_at IS NULL`, at, p.NotificationID, c.Principal.SubjectID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	return contracts.CommandResult{ResourceType: "notification", ResourceID: p.NotificationID}, []contracts.Event{event("notification.read", "notification", p.NotificationID, 1, map[string]string{"notification_id": p.NotificationID})}, nil
}
