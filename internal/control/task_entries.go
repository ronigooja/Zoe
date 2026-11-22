package control

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

type taskEntriesCursor struct {
	Version   int    `json:"v"`
	TaskID    string `json:"task_id"`
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

type taskEntriesPage struct {
	Messages   []contracts.TaskMessage `json:"messages"`
	NextCursor *string                 `json:"next_cursor"`
}

func (s *Server) handleTaskEntries(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	if !HasScope(p, "client:read") {
		writeError(w, apiError(http.StatusForbidden, "forbidden", "client:read scope is required", nil))
		return
	}

	taskID := strings.TrimSpace(r.PathValue("task_id"))
	if taskID == "" {
		writeError(w, apiError(http.StatusBadRequest, "invalid_request", "task_id is required", nil))
		return
	}
	limit, err := s.parseTaskEntriesLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, apiError(http.StatusBadRequest, "invalid_request", "limit must be an integer from 1 to 100", nil))
		return
	}
	cursor, err := decodeTaskEntriesCursor(r.URL.Query().Get("cursor"))
	if err != nil || (cursor != nil && cursor.TaskID != taskID) {
		writeError(w, apiError(http.StatusBadRequest, "invalid_cursor", "cursor is invalid", nil))
		return
	}
	page, err := loadTaskEntriesPage(r.Context(), s.store.DB(), p, taskID, cursor, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) parseTaskEntriesLimit(raw string) (int, error) {
	if raw == "" {
		return s.cfg.Limits.Int("task_entries_page_default"), nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > s.cfg.Limits.Int("task_entries_page_max") {
		return 0, errors.New("limit out of range")
	}
	return limit, nil
}

func encodeTaskEntriesCursor(cursor taskEntriesCursor) (string, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeTaskEntriesCursor(raw string) (*taskEntriesCursor, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 2048 {
		return nil, errors.New("cursor too large")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	var cursor taskEntriesCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return nil, err
	}
	if cursor.Version != 1 || cursor.TaskID == "" || cursor.CreatedAt == "" || cursor.ID == "" || len(cursor.ID) > 256 {
		return nil, errors.New("cursor fields are invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt); err != nil {
		return nil, errors.New("cursor timestamp is invalid")
	}
	return &cursor, nil
}

func loadTaskEntriesPage(ctx context.Context, db *sql.DB, p contracts.Principal, taskID string, cursor *taskEntriesCursor, limit int) (taskEntriesPage, error) {
	out := taskEntriesPage{Messages: []contracts.TaskMessage{}}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	allowed, err := PrincipalCanAccessTask(ctx, tx, p, taskID)
	if err != nil {
		return out, err
	}
	if !allowed {
		return out, apiError(http.StatusNotFound, "not_found", "task not found", nil)
	}
	// Compare whole seconds and nanoseconds independently. RFC3339Nano omits
	// trailing fractional zeroes, so ordering its raw strings is incorrect.
	// Strip the fraction before unixepoch to avoid SQLite millisecond rounding;
	// retain the original offset so equal instants also share the same key.
	query := `WITH entries AS (
		SELECT e.*,
		unixepoch(substr(e.created_at,1,19) || CASE WHEN substr(e.created_at,-1)='Z' THEN 'Z' ELSE substr(e.created_at,-6) END) AS created_second,
		CASE WHEN substr(e.created_at,20,1)='.' THEN
			CAST(substr(substr(e.created_at,21,length(e.created_at)-20-CASE WHEN substr(e.created_at,-1)='Z' THEN 1 ELSE 6 END) || '000000000',1,9) AS INTEGER)
			ELSE 0 END AS created_nanos
		FROM task_entries e WHERE e.workspace_id=? AND e.task_id=?
	) SELECT id,task_id,kind,content,created_at FROM entries`
	args := []any{p.WorkspaceID, taskID}
	if cursor != nil {
		at, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt)
		if err != nil {
			return out, err
		}
		query += ` WHERE (created_second,created_nanos,id) > (?,?,?)`
		args = append(args, at.Unix(), at.Nanosecond(), cursor.ID)
	}
	query += ` ORDER BY created_second,created_nanos,id LIMIT ?`
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return out, err
	}
	createdTimes := make([]string, 0, limit+1)
	for rows.Next() {
		var message contracts.TaskMessage
		var createdAt string
		if err = rows.Scan(&message.ID, &message.TaskID, &message.Kind, &message.Content, &createdAt); err != nil {
			rows.Close()
			return out, err
		}
		message.CreatedAt = parseTime(createdAt)
		out.Messages = append(out.Messages, message)
		createdTimes = append(createdTimes, createdAt)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	if err = rows.Close(); err != nil {
		return out, err
	}
	if len(out.Messages) > limit {
		out.Messages = out.Messages[:limit]
		last := out.Messages[len(out.Messages)-1]
		next, encodeErr := encodeTaskEntriesCursor(taskEntriesCursor{Version: 1, TaskID: taskID, CreatedAt: createdTimes[limit-1], ID: last.ID})
		if encodeErr != nil {
			return out, fmt.Errorf("encode task entries cursor: %w", encodeErr)
		}
		out.NextCursor = &next
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}
