package voice

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const contextEventSchema = `CREATE TABLE voice_context_events (
 owner_id TEXT NOT NULL, session_id TEXT NOT NULL,
 source_kind TEXT NOT NULL CHECK(source_kind IN ('task_result','ui_feedback','fast_reply','user_text','ui_input_request','ui_input_received')),
 source_id TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('user','background')),
 text TEXT NOT NULL, at TEXT NOT NULL, received_at TEXT NOT NULL,
 PRIMARY KEY(owner_id,source_kind,source_id)
);
CREATE INDEX voice_context_events_owner ON voice_context_events(owner_id);
ALTER TABLE voice_context_registers ADD COLUMN event_watermark INTEGER NOT NULL DEFAULT 0;`

type contextEvent struct {
	owner, session, kind, id, role, text string
	at                                   time.Time
}

func contextEventRole(kind string) string {
	switch kind {
	case "task_result", "ui_feedback", "fast_reply", "ui_input_request":
		return "background"
	case "user_text", "ui_input_received":
		return "user"
	}
	return ""
}

// recordContextEvent participates in the caller's delivery transaction only.
func recordContextEvent(ctx context.Context, tx *sql.Tx, e contextEvent) error {
	if e.owner == "" || e.session == "" || e.id == "" || e.role == "" || contextEventRole(e.kind) != e.role || e.at.IsZero() {
		return errors.New("invalid context event")
	}
	at := e.at.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_context_events(owner_id,session_id,source_kind,source_id,role,text,at,received_at) VALUES(?,?,?,?,?,?,?,?)`, e.owner, e.session, e.kind, e.id, e.role, e.text, at, at); err != nil {
		return err
	}
	var owner, session, role, text string
	if err := tx.QueryRowContext(ctx, `SELECT owner_id,session_id,role,text FROM voice_context_events WHERE owner_id=? AND source_kind=? AND source_id=?`, e.owner, e.kind, e.id).Scan(&owner, &session, &role, &text); err != nil {
		return err
	}
	if owner != e.owner || session != e.session || role != e.role || text != e.text {
		return errors.New("context event identity conflict")
	}
	return nil
}
