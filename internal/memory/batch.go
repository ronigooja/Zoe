package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/internal/memoryapi"
)

const ingestChunkBytes = 200 * 1024

// Reserve the exact fixed-length SHA-256 identities when sizing HTTP bodies.
func batchBodyFits(user, text string) bool {
	data, _ := json.Marshal(map[string]string{"user": user, "text": text, "batch_id": fmt.Sprintf("%064d", 0), "chunk_id": fmt.Sprintf("%064d", 0)})
	return len(data) <= 256*1024
}

type ingestClient interface {
	IngestChunk(context.Context, string, string, string, string) (int, int, error)
}

type batchEntry struct {
	at     time.Time
	id     string
	seq    int64
	text   string
	source string
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

// batchInput reads transcript turns and confirmed conversation events.
type batchQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func batchSources(ctx context.Context, q batchQuerier, workspace, principal string, from, until time.Time, unseen bool) ([]batchEntry, error) {
	var turns, events []batchEntry
	seen := map[string]bool{}
	if unseen {
		rows, err := q.QueryContext(ctx, `SELECT source_id FROM memory_ingest_members WHERE workspace_id=? AND principal_id=?`, workspace, principal)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				return nil, err
			}
			seen[id] = true
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()
	}
	rows, err := q.QueryContext(ctx, `SELECT t.at,t.session_id,t.seq,t.role,t.text FROM voice_transcript_turns t JOIN voice_sessions s ON s.id=t.session_id AND s.workspace_id=t.workspace_id WHERE s.workspace_id=? AND s.owner_id=?`, workspace, principal)
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
		e.source = fmt.Sprintf("turn:%q:%d", e.id, e.seq)
		if !seen[e.source] && (unseen || e.at.After(from)) && !e.at.After(until) {
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
	rows, err = q.QueryContext(ctx, `SELECT source_kind,source_id,at,role,text FROM voice_context_events WHERE workspace_id=? AND owner_id=?`, workspace, principal)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e batchEntry
		var at, kind, role, text string
		if err = rows.Scan(&kind, &e.id, &at, &role, &text); err != nil {
			break
		}
		e.at, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			break
		}
		e.source = fmt.Sprintf("event:%q:%q", kind, e.id)
		if !seen[e.source] && (unseen || e.at.After(from)) && !e.at.After(until) {
			e.text = fmt.Sprintf("[%s] %s: %s\n", e.at.UTC().Format(time.RFC3339Nano), role, text)
			events = append(events, e)
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
			return a.source < b.source
		})
	}
	entries := append(turns, events...)
	less(entries)
	return entries, nil
}

func (r *Repository) batchInput(ctx context.Context, workspace, principal string, from, until time.Time) ([]string, error) {
	entries, err := batchSources(ctx, r.db, workspace, principal, from, until, false)
	var blocks []string
	for _, e := range entries {
		blocks = append(blocks, e.text)
	}
	return blocks, err
}

// ingestChunks preserves whole turns/events unless one block exceeds the limit.
// JSON size is also bounded: escaping can make 200 KiB of text exceed 256 KiB.
func ingestChunks(blocks []string, user string) []string {
	var chunks []string
	current := ""
	for _, block := range blocks {
		for len(block) > 0 {
			if len(block) <= ingestChunkBytes && batchBodyFits(user, block) {
				if len(current)+len(block) > ingestChunkBytes || !batchBodyFits(user, current+block) {
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
				if batchBodyFits(user, block[:mid]) {
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
	// A new owner is due immediately, from the beginning. Only a successful,
	// nonempty batch advances the boundary and schedules now + interval.
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
	user := memoryapi.User(workspace, principal)
	if !batchBodyFits(user, "\u0000\u0000\u0000\u0000") {
		return 0, errors.New("memory ingest owner exceeds request limit")
	}
	var batchID string
	var until int64
	var batchStatus, blockedReason string
	// The source snapshot and membership assignment are one SQLite transaction.
	// Inserts after this snapshot remain unassigned even if their event time < M.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT id,until_ns,status,error_kind FROM memory_ingest_batches WHERE workspace_id=? AND principal_id=? AND from_ns=?`, workspace, principal, boundary).Scan(&batchID, &until, &batchStatus, &blockedReason)
	if err == nil && batchStatus == "blocked" {
		return 0, &memoryapi.Error{Kind: blockedReason}
	}
	if errors.Is(err, sql.ErrNoRows) {
		entries, e := batchSources(ctx, tx, workspace, principal, time.Time{}, now, true)
		if e != nil {
			return 0, e
		}
		if len(entries) == 0 {
			return 0, nil
		}
		var blocks []string
		for _, entry := range entries {
			blocks = append(blocks, entry.text)
		}
		chunks := ingestChunks(blocks, user)
		until = now.UnixNano()
		batchID = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", user, boundary, until))))
		if _, e = tx.ExecContext(ctx, `INSERT INTO memory_ingest_batches(id,workspace_id,principal_id,from_ns,until_ns) VALUES(?,?,?,?,?)`, batchID, workspace, principal, boundary, until); e != nil {
			return 0, e
		}
		for _, entry := range entries {
			if _, e = tx.ExecContext(ctx, `INSERT INTO memory_ingest_members VALUES(?,?,?,?)`, workspace, principal, entry.source, batchID); e != nil {
				return 0, e
			}
		}
		for i, text := range chunks {
			id := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", batchID, i, text))))
			if _, e = tx.ExecContext(ctx, `INSERT INTO memory_ingest_chunks(batch_id,position,id,text) VALUES(?,?,?,?)`, batchID, i, id, text); e != nil {
				return 0, e
			}
		}
	} else if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	// Read and close before HTTP: never hold the single SQLite connection
	// during external work. The persisted snapshot fixes the retry interval.
	rows, err := r.db.QueryContext(ctx, `SELECT id,text FROM memory_ingest_chunks WHERE batch_id=? AND done=0 ORDER BY position`, batchID)
	if err != nil {
		return 0, err
	}
	type pendingChunk struct{ id, text string }
	var pending []pendingChunk
	for rows.Next() {
		var c pendingChunk
		if err = rows.Scan(&c.id, &c.text); err != nil {
			break
		}
		pending = append(pending, c)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return 0, err
	}
	for i, c := range pending {
		if _, _, err = client.IngestChunk(ctx, user, batchID, c.id, c.text); err != nil {
			if memoryapi.Quarantined(err) {
				var failure *memoryapi.Error
				errors.As(err, &failure)
				if _, e := r.db.ExecContext(ctx, `UPDATE memory_ingest_batches SET status='blocked',error_kind=? WHERE id=?`, failure.Kind, batchID); e != nil {
					return i, e
				}
			}
			return i, err
		}
		if _, err = r.db.ExecContext(ctx, `UPDATE memory_ingest_chunks SET done=1 WHERE id=?`, c.id); err != nil {
			return i, err
		}
	}
	res, err := r.db.ExecContext(ctx, `UPDATE memory_ingest_state SET boundary_ns=?,next_attempt_ms=? WHERE workspace_id=? AND principal_id=? AND boundary_ns=? AND next_attempt_ms=?`, until, now.Add(interval).UnixMilli(), workspace, principal, boundary, next)
	if err != nil {
		return len(pending), err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		err = ErrVersionConflict
	}
	return len(pending), err
}
