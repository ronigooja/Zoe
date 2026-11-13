package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/internal/memoryapi"
)

const ingestChunkBytes = 200 * 1024

type ingestClient interface {
	Ingest(context.Context, string, string) (int, int, error)
}

type batchEntry struct {
	at   time.Time
	id   string
	seq  int64
	text string
}

// BoundaryAt returns the last successful worker ingestion boundary, without
// creating state. A missing or never-successful owner returns zero time.
func (r *Repository) BoundaryAt(ctx context.Context, workspace, principal string) (time.Time, error) {
	var ns int64
	err := r.db.QueryRowContext(ctx, `SELECT boundary_ns FROM memory_ingest_state WHERE workspace_id=? AND principal_id=?`, workspace, principal).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && ns == 0) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, ns).UTC(), nil
}

// batchInput reads only transcript turns and completed task requests/results.
func (r *Repository) batchInput(ctx context.Context, workspace, principal string, from, until time.Time) ([]string, error) {
	var turns, tasks []batchEntry
	rows, err := r.db.QueryContext(ctx, `SELECT t.at,t.session_id,t.seq,t.role,t.text FROM voice_transcript_turns t JOIN voice_sessions s ON s.id=t.session_id AND s.workspace_id=t.workspace_id WHERE s.workspace_id=? AND s.owner_id=?`, workspace, principal)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e batchEntry
		var at, role, text string
		if err = rows.Scan(&at, &e.id, &e.seq, &role, &text); err != nil {
			break
		}
		e.at, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			break
		}
		if e.at.After(from) && !e.at.After(until) {
			e.text = fmt.Sprintf("[%s] %s: %s\n", e.at.UTC().Format(time.RFC3339Nano), role, text)
			turns = append(turns, e)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = r.db.QueryContext(ctx, `SELECT id,finished_at,title,result FROM tasks WHERE workspace_id=? AND owner_subject_id=? AND status='complete' AND finished_at IS NOT NULL`, workspace, principal)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e batchEntry
		var at, request, result string
		if err = rows.Scan(&e.id, &at, &request, &result); err != nil {
			break
		}
		e.at, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			break
		}
		if e.at.After(from) && !e.at.After(until) {
			e.text = fmt.Sprintf("[%s] completed task:\nuser: %s\nresult: %s\n", e.at.UTC().Format(time.RFC3339Nano), request, result)
			tasks = append(tasks, e)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	less := func(entries []batchEntry) {
		sort.Slice(entries, func(i, j int) bool {
			a, b := entries[i], entries[j]
			if !a.at.Equal(b.at) {
				return a.at.Before(b.at)
			}
			if a.id != b.id {
				return a.id < b.id
			}
			return a.seq < b.seq
		})
	}
	less(turns)
	less(tasks)
	var blocks []string
	for _, e := range append(turns, tasks...) {
		blocks = append(blocks, e.text)
	}
	return blocks, nil
}

// ingestChunks preserves whole turns/tasks unless one block exceeds the limit.
// JSON size is also bounded: escaping can make 200 KiB of text exceed 256 KiB.
func ingestChunks(blocks []string, user string) []string {
	var chunks []string
	current := ""
	for _, block := range blocks {
		for len(block) > 0 {
			if len(block) <= ingestChunkBytes && ingestBodyFits(user, block) {
				if len(current)+len(block) > ingestChunkBytes || !ingestBodyFits(user, current+block) {
					if current != "" {
						chunks = append(chunks, current)
						current = ""
					}
				}
				current += block
				break
			}
			if current != "" {
				chunks = append(chunks, current)
				current = ""
			}
			low, high := 0, min(len(block), ingestChunkBytes)
			for low < high {
				mid := low + (high-low+1)/2
				if ingestBodyFits(user, block[:mid]) {
					low = mid
				} else {
					high = mid - 1
				}
			}
			n := low
			for n < len(block) && n > 0 && !utf8.RuneStart(block[n]) {
				n--
			}
			chunks = append(chunks, block[:n])
			block = block[n:]
		}
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks
}

func (r *Repository) runBatch(ctx context.Context, client ingestClient, workspace, principal string, now time.Time, interval time.Duration) (int, error) {
	if _, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO memory_ingest_state(workspace_id,principal_id,boundary_ns) VALUES(?,?,0)`, workspace, principal); err != nil {
		return 0, err
	}
	var boundary, next int64
	if err := r.db.QueryRowContext(ctx, `SELECT boundary_ns,next_attempt_ms FROM memory_ingest_state WHERE workspace_id=? AND principal_id=?`, workspace, principal).Scan(&boundary, &next); err != nil {
		return 0, err
	}
	if now.UnixMilli() < next || now.UnixNano() <= boundary {
		return 0, nil
	}
	blocks, err := r.batchInput(ctx, workspace, principal, time.Unix(0, boundary), now)
	if err != nil {
		return 0, err
	}
	user := memoryapi.User(workspace, principal)
	if !ingestBodyFits(user, "\u0000\u0000\u0000\u0000") {
		return 0, errors.New("memory ingest owner exceeds request limit")
	}
	chunks := ingestChunks(blocks, user)
	for i, text := range chunks {
		if _, _, err = client.Ingest(ctx, user, text); err != nil {
			return i, err
		}
	}
	if len(chunks) == 0 {
		return 0, nil
	}
	res, err := r.db.ExecContext(ctx, `UPDATE memory_ingest_state SET boundary_ns=?,next_attempt_ms=? WHERE workspace_id=? AND principal_id=? AND boundary_ns=? AND next_attempt_ms=?`, now.UnixNano(), now.Add(interval).UnixMilli(), workspace, principal, boundary, next)
	if err != nil {
		return len(chunks), err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		err = ErrVersionConflict
	}
	return len(chunks), err
}
