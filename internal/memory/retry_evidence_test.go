package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/memoryapi"
)

func evidenceClient(t *testing.T, handler http.HandlerFunc) *memoryapi.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := memoryapi.New(server.URL, token, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func reopenEvidenceDB(t *testing.T, db *sql.DB) (*sql.DB, *Repository) {
	t.Helper()
	var path string
	if err := db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	reopened.SetMaxOpenConns(1)
	t.Cleanup(func() { reopened.Close() })
	return reopened, NewRepository(reopened)
}

func TestIngestRetryGrowthAndRestart(t *testing.T) {
	db, repo, now := ingestFixture(t)
	if _, err := db.Exec(`INSERT INTO voice_transcript_turns VALUES('workspace','session',1,'user','text',?)`, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := evidenceClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/memory/config" {
			fmt.Fprint(w, `{"config_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
			return
		}
		calls++
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error_kind":"backend_unavailable"}`)
	})
	ctx := context.Background()
	// Midpoint gives exact doubling; the last sample tests downward cap jitter.
	delays := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, 48 * time.Minute}
	for i, delay := range delays {
		jitter := func() float64 {
			if i == len(delays)-1 {
				return 0
			}
			if i == len(delays)-2 {
				return 1
			}
			return 0.5
		}
		if err := repo.batchStepWithJitter(ctx, client, time.Hour, now, jitter); err == nil {
			t.Fatal("expected transient failure")
		}
		var failures int
		var nextText string
		if err := db.QueryRow(`SELECT failures,next_attempt_at FROM model_retry_state WHERE consumer='memory_ingest' AND scope=?`, memoryapi.User("workspace", "owner")).Scan(&failures, &nextText); err != nil {
			t.Fatal(err)
		}
		next, err := time.Parse(time.RFC3339Nano, nextText)
		if err != nil {
			t.Fatal(err)
		}
		if failures != i+1 || !next.Equal(now.Add(delay)) {
			t.Fatalf("failure %d: count=%d next=%v expected=%v", i+1, failures, next, now.Add(delay))
		}
		if i == 0 {
			db, repo = reopenEvidenceDB(t, db)
		}
		for tick := now.Add(time.Minute); tick.Before(next); tick = tick.Add(time.Minute) {
			if err := repo.batchStepWithJitter(ctx, client, time.Hour, tick, jitter); err != nil {
				t.Fatal(err)
			}
			if calls != i+1 {
				t.Fatalf("call at premature scan %v", tick)
			}
		}
		if err := repo.batchStepWithJitter(ctx, client, time.Hour, next.Add(-time.Nanosecond), jitter); err != nil || calls != i+1 {
			t.Fatalf("called before persisted due time: %d %v", calls, err)
		}
		now = next
	}
	if calls != len(delays) {
		t.Fatalf("calls=%d", calls)
	}
}

func TestIngestAuthRestartAndCredentialAbsence(t *testing.T) {
	db, repo, now := ingestFixture(t)
	if _, err := db.Exec(`INSERT INTO voice_transcript_turns VALUES('workspace','session',1,'user','text',?)`, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	key := "synthetic-api-key-absence-check"
	rawHash := sha256.Sum256([]byte(key))
	plainHash := hex.EncodeToString(rawHash[:])
	digest := backoff.Fingerprint("https://example.test", "model", "high", "", []byte(key))
	if digest == plainHash || strings.Contains(digest, key) {
		t.Fatal("credential exposed in digest")
	}
	var responses []string
	calls := 0
	client := evidenceClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body string
		if r.URL.Path == "/v1/memory/config" {
			data, _ := json.Marshal(map[string]string{"config_digest": digest})
			body = string(data)
		} else {
			calls++
			if calls == 1 {
				w.WriteHeader(502)
				body = `{"error_kind":"upstream_unauthorized"}`
			} else {
				body = `{"added":1}`
			}
		}
		responses = append(responses, body)
		fmt.Fprint(w, body)
	})
	ctx := context.Background()
	if err := repo.batchStep(ctx, client, time.Hour, now); err == nil {
		t.Fatal("expected unauthorized")
	}
	db, repo = reopenEvidenceDB(t, db)
	if err := repo.batchStep(ctx, client, time.Hour, now.Add(time.Hour)); err != nil || calls != 1 {
		t.Fatalf("auth stop lost after restart: %d %v", calls, err)
	}
	var observed string
	if err := db.QueryRow(`SELECT fingerprint FROM model_config_fingerprints WHERE consumer='memory_ingest'`).Scan(&observed); err != nil || observed != digest {
		t.Fatal("digest was not observed")
	}
	// Capture every table and every column, not merely the retry tables.
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			for _, value := range values {
				text := fmt.Sprint(value)
				if b, ok := value.([]byte); ok {
					text = string(b)
				}
				if strings.Contains(text, key) || strings.Contains(text, plainHash) {
					t.Fatalf("credential material in table %s", table)
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	digest = backoff.Fingerprint("https://example.test", "new-model", "high", "", []byte(key))
	if err := repo.batchStep(ctx, client, time.Hour, now.Add(2*time.Hour)); err != nil || calls != 2 {
		t.Fatalf("changed config did not resume: %d %v", calls, err)
	}
	for _, body := range responses {
		if strings.Contains(body, key) || strings.Contains(body, plainHash) {
			t.Fatal("credential material in worker response")
		}
	}
}
