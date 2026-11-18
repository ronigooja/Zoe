package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/internal/memoryapi"
)

func TestMemoryBatchInterval(t *testing.T) {
	for _, value := range []string{"0", "169", "bad"} {
		if _, err := BatchInterval(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	for value, want := range map[string]time.Duration{"": 24 * time.Hour, "1": time.Hour, "168": 168 * time.Hour} {
		if got, err := BatchInterval(value); err != nil || got != want {
			t.Fatalf("%q: %v %v", value, got, err)
		}
	}
}

func TestMemoryIngestChunks(t *testing.T) {
	user := memoryapi.User("workspace", "owner")
	for _, blocks := range [][]string{
		{strings.Repeat("a", ingestChunkBytes), "next turn\n"},
		{strings.Repeat("界", ingestChunkBytes), "next turn\n"},
		{strings.Repeat("\x00\"\\<\n", ingestChunkBytes)},
		{"small turn\n", strings.Repeat("b", ingestChunkBytes-1)},
	} {
		chunks := ingestChunks(blocks, user)
		if strings.Join(chunks, "") != strings.Join(blocks, "") {
			t.Fatal("text changed")
		}
		for _, chunk := range chunks {
			if len(chunk) == 0 || len(chunk) > ingestChunkBytes || !utf8.ValidString(chunk) || !ingestBodyFits(user, chunk) {
				t.Fatalf("invalid chunk: bytes=%d", len(chunk))
			}
		}
	}
	blocks := []string{strings.Repeat("a", ingestChunkBytes-10), "whole turn!\n"}
	chunks := ingestChunks(blocks, user)
	if len(chunks) != 2 || chunks[1] != blocks[1] {
		t.Fatal("split an avoidable turn")
	}
}

func TestMemoryBatchDisabled(t *testing.T) {
	if (*Repository)(nil).StartBatches(context.Background(), nil, time.Hour) {
		t.Fatal("started disabled scheduler")
	}
}

func TestMemoryBatchQuarantineRetainsSourcesAndAllowsOtherOwners(t *testing.T) {
	db, repo, now := ingestFixture(t)
	for _, session := range []string{"session", "other"} {
		if _, err := db.Exec(`INSERT INTO voice_transcript_turns VALUES('workspace',?,1,'user','fact',?)`, session, now.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	client := ingestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input map[string]string
		_ = json.NewDecoder(r.Body).Decode(&input)
		if input["user"] == memoryapi.User("workspace", "owner") {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error_kind":"ingest_quarantined"}`))
			return
		}
		_, _ = w.Write([]byte(`{"added":1,"updated":0}`))
	})
	for i := 0; i < 3; i++ {
		if _, err := repo.runBatch(context.Background(), client, "workspace", "owner", now.Add(time.Duration(i)*time.Hour), time.Hour); !memoryapi.Quarantined(err) {
			t.Fatalf("error=%v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("blocked calls=%d", calls)
	}
	var boundary, members, chunks int
	var status, reason string
	if err := db.QueryRow(`SELECT boundary_ns FROM memory_ingest_state WHERE principal_id='owner'`).Scan(&boundary); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status,error_kind FROM memory_ingest_batches WHERE principal_id='owner'`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	_ = db.QueryRow(`SELECT count(*) FROM memory_ingest_members WHERE principal_id='owner'`).Scan(&members)
	_ = db.QueryRow(`SELECT count(*) FROM memory_ingest_chunks WHERE done=0`).Scan(&chunks)
	if boundary != 0 || status != "blocked" || reason != "ingest_quarantined" || members != 1 || chunks != 1 {
		t.Fatalf("state=%d %s %s members=%d chunks=%d", boundary, status, reason, members, chunks)
	}
	if _, err := repo.runBatch(context.Background(), client, "workspace", "other", now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("other owner calls=%d", calls)
	}
}

func TestMemoryBatchTransientReconcilesSameIdentity(t *testing.T) {
	db, repo, now := ingestFixture(t)
	if _, err := db.Exec(`INSERT INTO voice_transcript_turns VALUES('workspace','session',1,'user','fact',?)`, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	var first map[string]string
	calls := 0
	client := ingestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input map[string]string
		_ = json.NewDecoder(r.Body).Decode(&input)
		if calls == 1 {
			first = input
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error_kind":"backend_unavailable"}`))
			return
		}
		for _, key := range []string{"batch_id", "chunk_id", "text", "user"} {
			if input[key] != first[key] {
				t.Errorf("changed %s", key)
			}
		}
		_, _ = w.Write([]byte(`{"added":1,"updated":0}`))
	})
	if _, err := repo.runBatch(context.Background(), client, "workspace", "owner", now, time.Hour); err == nil {
		t.Fatal("missing transient error")
	}
	if _, err := repo.runBatch(context.Background(), client, "workspace", "owner", now.Add(time.Minute), time.Hour); err != nil {
		t.Fatal(err)
	}
	boundary, err := repo.BoundaryAt(context.Background(), "workspace", "owner")
	if err != nil || !boundary.Equal(now) || calls != 2 {
		t.Fatalf("boundary=%v calls=%d err=%v", boundary, calls, err)
	}
}

func ingestFixture(t *testing.T) (*sql.DB, *Repository, time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ingest.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE voice_sessions(id TEXT PRIMARY KEY,workspace_id TEXT,owner_id TEXT);
CREATE TABLE voice_transcript_turns(workspace_id TEXT,session_id TEXT,seq INTEGER,role TEXT,text TEXT,at TEXT);
CREATE TABLE tasks(id TEXT,workspace_id TEXT,owner_subject_id TEXT,status TEXT,finished_at TEXT,goal TEXT,result TEXT);
CREATE TABLE memory_ingest_batches(id TEXT PRIMARY KEY,workspace_id TEXT,principal_id TEXT,from_ns INTEGER,until_ns INTEGER,status TEXT NOT NULL DEFAULT 'pending',error_kind TEXT NOT NULL DEFAULT '',UNIQUE(workspace_id,principal_id,from_ns));
CREATE TABLE memory_ingest_chunks(batch_id TEXT,position INTEGER,id TEXT UNIQUE,text TEXT,done INTEGER DEFAULT 0,PRIMARY KEY(batch_id,position));
CREATE TABLE memory_ingest_members(workspace_id TEXT,principal_id TEXT,source_id TEXT,batch_id TEXT,PRIMARY KEY(workspace_id,principal_id,source_id));
CREATE TABLE memory_ingest_state(workspace_id TEXT,principal_id TEXT,boundary_ns INTEGER NOT NULL,next_attempt_ms INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(workspace_id,principal_id));
CREATE TABLE model_retry_state(consumer TEXT,scope TEXT,failures INTEGER,next_attempt_at TEXT,auth_failed INTEGER,last_error TEXT,updated_at TEXT,PRIMARY KEY(consumer,scope));
CREATE TABLE model_config_fingerprints(consumer TEXT PRIMARY KEY,fingerprint TEXT);
INSERT INTO voice_sessions VALUES('session','workspace','owner'),('other','workspace','other');`)
	if err != nil {
		t.Fatal(err)
	}
	return db, NewRepository(db), time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
}

func ingestWorker(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *memoryapi.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/memory/config" {
			_, _ = w.Write([]byte(`{"config_digest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`))
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := memoryapi.New(server.URL, token, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestMemoryIngestTranscriptTasksWindowAndCadence(t *testing.T) {
	db, repo, now := ingestFixture(t)
	for _, turn := range []struct {
		seq        int
		role, text string
		at         time.Time
	}{
		{2, "assistant", "answer", now}, {1, "user", "original words", now.Add(-time.Hour)}, {3, "user", "future", now.Add(time.Second)},
	} {
		if _, err := db.Exec(`INSERT INTO voice_transcript_turns VALUES('workspace','session',?,?,?,?)`, turn.seq, turn.role, turn.text, turn.at.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the other owner's first batch outside this owner's cadence window:
	// no state means due immediately, independently of the first owner's timer.
	if _, err := db.Exec(`INSERT INTO voice_transcript_turns VALUES('workspace','other',1,'user','private',?)`, now.Add(48*time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tasks VALUES('done','workspace','owner','complete',?,'original task sentence','result text'),('failed','workspace','owner','failed',?,'do not send','failure')`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	want := "[2026-11-13T11:00:00Z] user: original words\n[2026-11-13T12:00:00Z] completed task done:\nrequest: original task sentence\nresult: result text\n[2026-11-13T12:00:00Z] assistant: answer\n"
	client := ingestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input["text"] != want || input["user"] != memoryapi.User("workspace", "owner") || r.URL.Path != "/v1/memory/ingest" {
			t.Errorf("unexpected input: %#v", input)
		}
		w.Write([]byte(`{"added":2,"updated":1}`))
	})
	if err := repo.batchStep(context.Background(), client, 24*time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	var boundary, next int64
	if err := db.QueryRow(`SELECT boundary_ns,next_attempt_ms FROM memory_ingest_state WHERE principal_id='owner'`).Scan(&boundary, &next); err != nil || boundary != now.UnixNano() || next != now.Add(24*time.Hour).UnixMilli() {
		t.Fatalf("state=%d,%d err=%v", boundary, next, err)
	}
	if err := repo.batchStep(context.Background(), client, 24*time.Hour, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("ran before interval")
	}
	if _, err := repo.runBatch(context.Background(), client, "workspace", "nobody", now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("called worker without content")
	}
	want = "[2026-11-13T12:00:01Z] user: future\n"
	if err := repo.batchStep(context.Background(), client, 24*time.Hour, now.Add(24*time.Hour-time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("ran just before interval")
	}
	if err := repo.batchStep(context.Background(), client, 24*time.Hour, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("did not run when due: calls=%d", calls)
	}
	if err := db.QueryRow(`SELECT boundary_ns,next_attempt_ms FROM memory_ingest_state WHERE principal_id='owner'`).Scan(&boundary, &next); err != nil || boundary != now.Add(24*time.Hour).UnixNano() || next != now.Add(48*time.Hour).UnixMilli() {
		t.Fatalf("second state=%d,%d err=%v", boundary, next, err)
	}
}

func TestMemoryIngestFailureResumesPendingChunksAndSuccessOnlyCAS(t *testing.T) {
	db, repo, now := ingestFixture(t)
	for seq := 0; seq < 2; seq++ {
		if _, err := db.Exec(`INSERT INTO voice_transcript_turns VALUES('workspace','session',?,'user',?,?)`, seq, strings.Repeat("a", 150*1024), now.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	calls, fail := 0, true
	var batchID, secondChunkID string
	client := ingestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input map[string]string
		json.NewDecoder(r.Body).Decode(&input)
		if calls == 1 {
			batchID = input["batch_id"]
		}
		if calls == 2 {
			secondChunkID = input["chunk_id"]
		}
		if input["batch_id"] != batchID || len(batchID) != 64 || len(input["chunk_id"]) != 64 {
			t.Error("missing or changed deterministic identity")
		}
		if calls == 3 && input["chunk_id"] != secondChunkID {
			t.Error("retried a different chunk")
		}
		if len(input["text"]) > ingestChunkBytes {
			t.Error("oversized")
		}
		var boundary int64
		if err := db.QueryRow(`SELECT boundary_ns FROM memory_ingest_state WHERE principal_id='owner'`).Scan(&boundary); err != nil || boundary != 0 {
			t.Errorf("advanced before all success: %d %v", boundary, err)
		}
		if fail && calls == 2 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error_kind":"backend_unavailable"}`))
			return
		}
		w.Write([]byte(`{"added":1,"updated":0}`))
	})
	if err := repo.batchStep(context.Background(), client, time.Hour, now); err == nil {
		t.Fatal("failure ignored")
	}
	var boundary, next int64
	if err := db.QueryRow(`SELECT boundary_ns,next_attempt_ms FROM memory_ingest_state WHERE principal_id='owner'`).Scan(&boundary, &next); err != nil || boundary != 0 || next != 0 {
		t.Fatalf("failed state changed: %d %d %v", boundary, next, err)
	}
	fail = false
	// Close and reopen the disk database, not just the repository wrapper.
	var path string
	if err := db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, _ = sql.Open("sqlite", path)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	repo = NewRepository(db)
	if err := repo.batchStep(context.Background(), client, time.Hour, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("did not resume pending chunk: %d", calls)
	}
	if err := db.QueryRow(`SELECT boundary_ns FROM memory_ingest_state WHERE principal_id='owner'`).Scan(&boundary); err != nil || boundary != now.UnixNano() {
		t.Fatalf("boundary=%d err=%v", boundary, err)
	}
}

func TestIngestAuthStopClearsOnlyOnConfigChange(t *testing.T) {
	db, repo, now := ingestFixture(t)
	if _, err := db.Exec(`INSERT INTO voice_transcript_turns VALUES('workspace','session',1,'user','text',?)`, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/memory/config" {
			_ = json.NewEncoder(w).Encode(map[string]string{"config_digest": digest})
			return
		}
		calls++
		if calls == 1 {
			w.WriteHeader(502)
			_, _ = w.Write([]byte(`{"error_kind":"upstream_unauthorized"}`))
			return
		}
		_, _ = w.Write([]byte(`{"added":1}`))
	}))
	defer server.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := memoryapi.New(server.URL, token, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.batchStep(context.Background(), client, time.Hour, now); err == nil {
		t.Fatal("expected auth error")
	}
	if err := repo.batchStep(context.Background(), client, time.Hour, now.Add(time.Minute)); err != nil || calls != 1 {
		t.Fatalf("stop not retained: %d %v", calls, err)
	}
	digest = strings.Repeat("b", 64)
	if err := repo.batchStep(context.Background(), client, time.Hour, now.Add(2*time.Minute)); err != nil || calls != 2 {
		t.Fatalf("stop not cleared: %d %v", calls, err)
	}
}

func TestMemoryIngestCASRejectsConcurrentAdvance(t *testing.T) {
	db, repo, now := ingestFixture(t)
	if _, err := db.Exec(`INSERT INTO voice_transcript_turns VALUES('workspace','session',1,'user','text',?)`, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	client := ingestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		if _, err := db.Exec(`UPDATE memory_ingest_state SET boundary_ns=42 WHERE principal_id='owner'`); err != nil {
			t.Error(err)
		}
		w.Write([]byte(`{"added":1,"updated":0}`))
	})
	if _, err := repo.runBatch(context.Background(), client, "workspace", "owner", now, time.Hour); err != ErrVersionConflict {
		t.Fatalf("CAS error=%v", err)
	}
	var boundary int64
	if err := db.QueryRow(`SELECT boundary_ns FROM memory_ingest_state WHERE principal_id='owner'`).Scan(&boundary); err != nil || boundary != 42 {
		t.Fatalf("overwrote concurrent boundary: %d %v", boundary, err)
	}
}

func TestMemoryIngestLateBackdatedSourceAfterSnapshot(t *testing.T) {
	db, repo, now := ingestFixture(t)
	if _, err := db.Exec(`INSERT INTO voice_transcript_turns VALUES('workspace','session',1,'user','first',?)`, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := ingestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input map[string]string
		json.NewDecoder(r.Body).Decode(&input)
		if calls == 1 {
			// This independent insert proves HTTP happens outside snapshot tx.
			if _, err := db.Exec(`INSERT INTO voice_transcript_turns VALUES('workspace','session',2,'assistant','late backdated',?)`, now.Add(-time.Hour).Format(time.RFC3339Nano)); err != nil {
				t.Error(err)
			}
		} else if !strings.Contains(input["text"], "late backdated") || strings.Contains(input["text"], "user: first") {
			t.Error("unseen source dropped or completed source repeated")
		}
		w.Write([]byte(`{"added":1,"updated":0}`))
	})
	if _, err := repo.runBatch(context.Background(), client, "workspace", "owner", now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.runBatch(context.Background(), client, "workspace", "owner", now.Add(time.Hour), time.Hour); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestMemoryIngestBoundaryReadAndWindow(t *testing.T) {
	db, repo, now := ingestFixture(t)
	ctx := context.Background()
	if boundary, err := repo.BoundaryAt(ctx, "workspace", "owner"); err != nil || !boundary.IsZero() {
		t.Fatalf("missing boundary=%v err=%v", boundary, err)
	}
	from := now.Add(-time.Hour)
	if _, err := db.Exec(`INSERT INTO memory_ingest_state VALUES('workspace','owner',?,0)`, from.UnixNano()); err != nil {
		t.Fatal(err)
	}
	// Transcript at is RFC3339Nano TEXT; only the ingestion boundary is ns.
	if _, err := db.Exec(`INSERT INTO voice_transcript_turns VALUES('workspace','session',0,'user','exclude boundary',?),('workspace','session',1,'assistant','include now',?)`, from.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if boundary, err := repo.BoundaryAt(ctx, "workspace", "owner"); err != nil || !boundary.Equal(from) {
		t.Fatalf("boundary=%v err=%v", boundary, err)
	}
	blocks, err := repo.batchInput(ctx, "workspace", "owner", from, now)
	if err != nil || len(blocks) != 1 || blocks[0] != "[2026-11-13T12:00:00Z] assistant: include now\n" {
		t.Fatalf("blocks=%v err=%v", blocks, err)
	}
}
