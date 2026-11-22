package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/ronigooja/Zoe/internal/limits"
	"time"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/contracts"
)

const openingResultSchema = `
CREATE TABLE voice_result_opening (
 entry_id TEXT PRIMARY KEY REFERENCES voice_result_deliveries(entry_id),
 session_id TEXT NOT NULL REFERENCES voice_sessions(id),
 text TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('reserved','consumed')),
 created_at TEXT NOT NULL, consumed_at TEXT
);
CREATE INDEX voice_result_opening_session ON voice_result_opening(session_id,state);
CREATE TRIGGER voice_result_opening_release_terminal AFTER UPDATE OF state ON voice_sessions
WHEN NEW.state IN ('ended','failed') AND OLD.state!=NEW.state
BEGIN
 DELETE FROM voice_result_opening WHERE session_id=NEW.id AND state='reserved';
END;`

// Shared by pending voice paths, not UI rendering or authoritative task facts.
const resultNotInOpening = ` NOT EXISTS(SELECT 1 FROM voice_result_opening o WHERE o.entry_id=d.entry_id) `

type pendingOpeningResult struct{ EntryID, Text string }

func pendingResultsForContext(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, workspace, owner string, sets ...limits.Set) ([]pendingOpeningResult, error) {
	rows, err := db.QueryContext(ctx, `SELECT d.entry_id,t.goal,t.status,coalesce(t.summary_result,e.content) FROM voice_result_deliveries d JOIN tasks t ON t.id=d.task_id AND t.workspace_id=d.workspace_id AND t.owner_subject_id=d.subject_id JOIN task_entries e ON e.id=d.entry_id AND e.task_id=d.task_id AND e.workspace_id=d.workspace_id WHERE d.workspace_id=? AND d.subject_id=? AND d.status='pending' AND e.kind='assistant_message' AND `+authoritativeResultPredicate+` AND `+resultNotInOpening+` AND NOT EXISTS(SELECT 1 FROM voice_result_chunks x WHERE x.entry_id=d.entry_id AND x.state!='pending') ORDER BY d.created_at,d.entry_id LIMIT ?`, workspace, owner, limits.Resolve(sets).Int("opening_pending_results"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingOpeningResult
	for rows.Next() {
		var id, goal, status, result string
		if err := rows.Scan(&id, &goal, &status, &result); err != nil {
			return nil, err
		}
		out = append(out, pendingOpeningResult{id, "Task result:\nGoal: " + goal + "\nStatus: " + status + "\nResult: " + result})
	}
	return out, rows.Err()
}

func (s *Service) openingRegisterText(ctx context.Context, p contracts.Principal) string {
	if s.register == nil {
		return ""
	}
	text, err := s.register.AssembleContext(ctx, RegisterOwner{p.WorkspaceID, p.SubjectID}, s.now(), s.limits.Int("opening_context_runes"))
	if err != nil {
		return ""
	}
	return text
}

// buildAVASOpeningContext reserves exactly the sent result items in the same
// transaction as the allocating session and durable gateway request identity.
func (s *Service) buildAVASOpeningContext(ctx context.Context, tx *sql.Tx, p contracts.Principal, session, registerText, now string) (*openingContext, error) {
	c := &openingContext{Kind: "full", Items: []openingContextItem{}}
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(opening_context_version),0)+1 FROM voice_sessions WHERE workspace_id=? AND owner_id=?`, p.WorkspaceID, p.SubjectID).Scan(&c.Version); err != nil {
		return nil, err
	}
	remaining := s.limits.Int("opening_context_runes")
	appendItem := func(text string) bool {
		cost := utf8.RuneCountInString(text)
		if !utf8.ValidString(text) || len(c.Items) >= limits.GatewayOpeningItems || cost > remaining {
			return false
		}
		c.Items = append(c.Items, openingContextItem{Role: "user", Text: text})
		remaining -= cost
		return true
	}
	if registerText != "" {
		appendItem(registerText)
	}
	pending, err := pendingResultsForContext(ctx, tx, p.WorkspaceID, p.SubjectID, s.limits)
	if err != nil {
		return nil, err
	}
	for _, item := range pending {
		if !appendItem(item.Text) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO voice_result_opening(entry_id,session_id,text,state,created_at) VALUES(?,?,?,'reserved',?)`, item.EntryID, session, item.Text, now); err != nil {
			return nil, err
		}
	}
	manifest, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE voice_sessions SET opening_context_delivery='unverified',opening_context_manifest=?,opening_context_version=?,opening_context_chars=? WHERE id=?`, manifest, c.Version, openingCharsFor(c), session)
	return c, err
}

func strictOpeningReceipt(opening *openingContext, receipt *openingContextResult) bool {
	return opening != nil && receipt != nil && receipt.Applied && receipt.Silent == true && receipt.Version == opening.Version && receipt.ContentHash == openingContextHash(opening)
}

// acknowledgeOpeningResults runs inside the active transition. A missing
// receipt is uncertain; a present non-matching receipt releases the manifest.
func (s *Service) acknowledgeOpeningResults(ctx context.Context, tx *sql.Tx, p contracts.Principal, session string, opening *openingContext, receipt *openingContextResult) error {
	if receipt == nil {
		return nil
	}
	if !strictOpeningReceipt(opening, receipt) {
		_, err := tx.ExecContext(ctx, `DELETE FROM voice_result_opening WHERE session_id=? AND state='reserved'`, session)
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT entry_id,text FROM voice_result_opening WHERE session_id=? AND state='reserved' ORDER BY entry_id`, session)
	if err != nil {
		return err
	}
	var reserved []pendingOpeningResult
	for rows.Next() {
		var item pendingOpeningResult
		if err := rows.Scan(&item.EntryID, &item.Text); err != nil {
			rows.Close()
			return err
		}
		reserved = append(reserved, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := s.now().UTC()
	for _, item := range reserved {
		if err := recordContextEvent(ctx, tx, contextEvent{workspace: p.WorkspaceID, owner: p.SubjectID, session: session, kind: "task_result", id: item.EntryID, role: "background", text: item.Text, at: now}); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE voice_result_opening SET state='consumed',consumed_at=? WHERE session_id=? AND state='reserved'`, now.Format(time.RFC3339Nano), session)
	return err
}
