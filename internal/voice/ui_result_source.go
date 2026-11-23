package voice

import (
	"context"
	"database/sql"
	"errors"
)

// pinUIResultTx binds one authoritative source, never a mutable tasks.result.
// An empty entry deliberately selects the latest authoritative result once.
func pinUIResultTx(ctx context.Context, tx *sql.Tx, owner, task, request, entry string, cursor int64) error {
	var run, text string
	var selected int64
	err := tx.QueryRowContext(ctx, `SELECT e.id,json_extract(v.payload,'$.payload.run_id'),e.content,v.cursor FROM events v JOIN task_entries e ON e.id=json_extract(v.payload,'$.payload.entry_id') AND e.task_id=v.aggregate_id JOIN tasks t ON t.id=e.task_id JOIN runs r ON r.id=json_extract(v.payload,'$.payload.run_id') AND r.task_id=t.id WHERE v.event_type='task.assistant_message' AND v.aggregate_type='task' AND t.owner_subject_id=? AND t.id=? AND json_extract(v.payload,'$.payload.task_id')=t.id AND e.kind='assistant_message' AND (?='' OR e.id=?) AND (?=0 OR v.cursor=?) ORDER BY v.cursor DESC LIMIT 1`, owner, task, entry, entry, cursor, cursor).Scan(&entry, &run, &text, &selected)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ui_result_sources(request_id,task_id,run_id,entry_id,event_cursor,content_hash) VALUES(?,?,?,?,?,?)`, request, task, run, entry, selected, resultHash(text))
	return err
}

func (s *Service) uiPinnedResult(ctx context.Context, owner, task, request string) (*string, error) {
	var text, hash string
	err := s.db.QueryRowContext(ctx, `SELECT e.content,b.content_hash FROM ui_result_sources b JOIN ui_requests q ON q.id=b.request_id AND q.task_id=b.task_id JOIN tasks t ON t.id=b.task_id JOIN task_entries e ON e.id=b.entry_id AND e.task_id=t.id JOIN runs r ON r.id=b.run_id AND r.task_id=t.id JOIN events v ON v.cursor=b.event_cursor AND v.aggregate_type='task' AND v.aggregate_id=t.id AND v.event_type='task.assistant_message' AND json_extract(v.payload,'$.payload.task_id')=t.id AND json_extract(v.payload,'$.payload.entry_id')=e.id AND json_extract(v.payload,'$.payload.run_id')=r.id WHERE b.request_id=? AND t.owner_subject_id=? AND q.owner_subject_id=? AND t.id=? AND e.kind='assistant_message'`, request, owner, owner, task).Scan(&text, &hash)
	if err != nil {
		return nil, err
	}
	if hash != resultHash(text) {
		return nil, errors.New("ui_result_source_changed")
	}
	return &text, nil
}
