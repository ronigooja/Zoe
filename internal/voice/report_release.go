package voice

import (
	"context"
	"database/sql"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

// releaseReportResultsTx runs only with the reply's fenced delivered transition.
// The exact turn reference excludes redisplay and later revisions of the same turn.
func (s *Service) releaseReportResultsTx(ctx context.Context, tx *sql.Tx, reply string, dest fastReplyDestination, at time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT d.entry_id,e.content,coalesce(c.content_hash,'') FROM voice_fast_replies f
JOIN voice_turns t ON t.id=f.report_turn_id AND t.session_id=f.session_id AND t.generation=f.generation AND t.turn_id=f.turn_id AND t.steward_action='report'
JOIN voice_sessions v ON v.id=f.session_id AND v.generation=f.generation AND v.owner_id=? AND v.state='active' AND v.id=? AND v.generation=? AND v.gateway_session_id=? AND v.gateway_media_generation=?
JOIN voice_result_deliveries d ON d.subject_id=v.owner_id AND d.task_id=t.steward_task_id
JOIN tasks task ON task.id=d.task_id AND task.owner_subject_id=d.subject_id
JOIN task_entries e ON e.id=d.entry_id AND e.task_id=d.task_id AND e.kind='assistant_message'
LEFT JOIN voice_result_checkpoints c ON c.entry_id=d.entry_id
WHERE f.id=? AND f.status='delivered' AND f.intent_kind='task_report'
AND d.status='pending' AND d.last_error_code='awaiting_context' AND `+resultNotInOpening+` AND `+authoritativeResultPredicate+`
AND NOT EXISTS(SELECT 1 FROM voice_result_chunks ch WHERE ch.entry_id=d.entry_id AND ch.state!='pending')
AND (c.entry_id IS NULL OR (c.session_id=v.id AND c.generation=v.generation AND c.gateway_session_id=v.gateway_session_id AND c.media_generation=v.gateway_media_generation) OR c.lease_until<=?)`, dest.owner, dest.session, dest.generation, dest.gateway, dest.media, reply, at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	var entries []string
	for rows.Next() {
		var entry, text, hash string
		if err := rows.Scan(&entry, &text, &hash); err != nil {
			rows.Close()
			return err
		}
		if hash == "" || hash == resultHash(text) {
			entries = append(entries, entry)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := s.selectPendingResultTx(ctx, tx, contracts.Principal{SubjectID: dest.owner}, entry, dest.session, dest.generation); err != nil {
			return err
		}
	}
	return nil
}
