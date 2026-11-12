package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// These regressions require migration 106 and the staging APIs: before this
// change they do not compile; a schema-only implementation cannot dispatch a
// staged-only batch or atomically process its input with the claims and M.
func TestStageWriteIdempotentVerbatimBoundedAndIsolated(t *testing.T) {
	store, repo := newMemoryTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()
	text := "  Remember my exact words.\n"
	for _, scope := range [][2]string{{"default", "owner"}, {"default", "other"}} {
		if err := repo.StageWrite(ctx, scope[0], scope[1], "turn", text, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`INSERT INTO workspaces(id,name,created_at) SELECT 'second',name,created_at FROM workspaces WHERE id='default'`); err != nil {
		t.Fatal(err)
	}
	if err := repo.StageWrite(ctx, "second", "owner", "turn", text, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.StageWrite(ctx, "default", "owner", "turn", "replacement", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	items, err := repo.pendingStagedWrites(ctx, "default", "owner", now)
	if err != nil || len(items) != 1 || items[0].Text != text || !items[0].StagedAt.Equal(now) || items[0].Priority != 100 {
		t.Fatalf("first admission not retained: %#v %v", items, err)
	}
	for _, text := range []string{"", " \n", strings.Repeat("界", 4001)} {
		if err := repo.StageWrite(ctx, "default", "owner", "invalid", text, now); err == nil {
			t.Fatal("accepted empty or oversized text")
		}
	}
	if err := repo.StageWrite(ctx, "default", "owner", "max", strings.Repeat("界", 4000), now); err != nil {
		t.Fatal(err)
	}
}

func stagedOnlyFixture(t *testing.T) (*sql.DB, *Repository, time.Time, time.Time) {
	t.Helper()
	db, repo, from, now := batchFixture(t)
	if _, err := db.Exec(`DELETE FROM voice_transcript_turns; DELETE FROM tasks`); err != nil {
		t.Fatal(err)
	}
	return db, repo, from, now
}

func TestStagedOnlyBatchAtomicSuccessAndIsolation(t *testing.T) {
	db, repo, from, now := stagedOnlyFixture(t)
	ctx := context.Background()
	for _, item := range []struct {
		owner, key, text string
		at               time.Time
	}{{"owner", "turn", "  Keep my own words.\n", from.Add(-time.Hour)}, {"owner", "future", "future request", now.Add(time.Nanosecond)}, {"other", "turn", "private request", now}} {
		if err := repo.StageWrite(ctx, "default", item.owner, item.key, item.text, item.at); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	model := batchFakeModel(t, 200, `[{"subject":"user","key":"request","content":"Keep my own words."}]`, func(_ string, input string) {
		calls++
		var in batchInput
		if err := json.Unmarshal([]byte(input), &in); err != nil {
			t.Error(err)
		}
		if len(in.Transcript) != 0 || len(in.Tasks) != 0 || len(in.ExplicitRequests) != 1 || in.ExplicitRequests[0].Text != "  Keep my own words.\n" {
			t.Errorf("staged input: %#v", in)
		}
		// A backdated write admitted during extraction is not in this snapshot.
		if err := repo.StageWrite(ctx, "default", "owner", "during", "concurrent request", from); err != nil {
			t.Error(err)
		}
	})
	if ok, err := repo.RunBatch(ctx, "default", "owner", now, 24*time.Hour, model); err != nil || !ok || calls != 1 {
		t.Fatalf("staged-only batch: %v %v calls=%d", ok, err, calls)
	}
	var status string
	var processed int64
	if err := db.QueryRow(`SELECT status,processed_at FROM memory_staged_writes WHERE principal_id='owner' AND idempotency_key='turn'`).Scan(&status, &processed); err != nil || status != "processed" || processed != now.UnixNano() {
		t.Fatalf("processed=%s at=%d err=%v", status, processed, err)
	}
	boundary, err := repo.BoundaryAt(ctx, "default", "owner")
	if err != nil || !boundary.Equal(now) {
		t.Fatalf("M=%v err=%v", boundary, err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM memory_claims`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("claims=%d err=%v", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM memory_staged_writes WHERE status='pending'`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("unconsumed snapshot/owner/future=%d err=%v", count, err)
	}
	if err := repo.StageWrite(ctx, "default", "owner", "turn", "retry after processing", now); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM memory_staged_writes WHERE status='processed'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("processed replay=%d err=%v", count, err)
	}
}

func TestStagedBatchFailuresLeavePendingAndBoundaryUnchanged(t *testing.T) {
	for _, failure := range []string{"model", "claims", "boundary", "staging"} {
		t.Run(failure, func(t *testing.T) {
			db, repo, from, now := stagedOnlyFixture(t)
			ctx := context.Background()
			if err := repo.StageWrite(ctx, "default", "owner", "turn", "Remember this", now); err != nil {
				t.Fatal(err)
			}
			triggers := map[string]string{
				"claims":   `CREATE TRIGGER fail_claim BEFORE INSERT ON memory_claims BEGIN SELECT RAISE(ABORT,'fixture'); END`,
				"boundary": `CREATE TRIGGER fail_boundary BEFORE UPDATE OF boundary_ns ON memory_batch_boundaries BEGIN SELECT RAISE(ABORT,'fixture'); END`,
				"staging":  `CREATE TRIGGER fail_staging BEFORE UPDATE OF status ON memory_staged_writes BEGIN SELECT RAISE(ABORT,'fixture'); END`,
			}
			if trigger := triggers[failure]; trigger != "" {
				if _, err := db.Exec(trigger); err != nil {
					t.Fatal(err)
				}
			}
			status := 200
			if failure == "model" {
				status = 500
			}
			model := batchFakeModel(t, status, `[{"subject":"user","key":"k","content":"Remember this"}]`, nil)
			if ok, err := repo.RunBatch(ctx, "default", "owner", now, 24*time.Hour, model); err == nil || ok {
				t.Fatalf("expected failure: %v %v", ok, err)
			}
			boundary, err := repo.BoundaryAt(ctx, "default", "owner")
			if err != nil || !boundary.Equal(from) {
				t.Fatalf("M moved: %v %v", boundary, err)
			}
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM memory_staged_writes WHERE status='pending' AND processed_at IS NULL`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("pending=%d err=%v", count, err)
			}
			for _, table := range []string{"memory_claims", "memory_sources", "memory_batch_inputs"} {
				if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("partial %s=%d err=%v", table, count, err)
				}
			}
		})
	}
}

func TestBoundaryAtAndEmptyBatch(t *testing.T) {
	_, repo, from, now := stagedOnlyFixture(t)
	ctx := context.Background()
	for _, principal := range []string{"missing", "other"} {
		boundary, err := repo.BoundaryAt(ctx, "default", principal)
		if err != nil || !boundary.IsZero() {
			t.Fatalf("missing M=%v err=%v", boundary, err)
		}
	}
	model := batchFakeModel(t, 200, `[]`, func(string, string) { t.Error("empty batch dispatched") })
	if ok, err := repo.RunBatch(ctx, "default", "owner", now, 24*time.Hour, model); err != nil || ok {
		t.Fatalf("empty batch=%v err=%v", ok, err)
	}
	boundary, err := repo.BoundaryAt(ctx, "default", "owner")
	if err != nil || !boundary.Equal(from) {
		t.Fatalf("empty batch M=%v err=%v", boundary, err)
	}
}

func TestStagedOwnerDiscoveryPreservesSuccessCadence(t *testing.T) {
	_, repo, _, now := stagedOnlyFixture(t)
	ctx := context.Background()
	// This owner has neither a voice session nor a task.
	if err := repo.StageWrite(ctx, "default", "staged-only", "first", "Remember this", now); err != nil {
		t.Fatal(err)
	}
	calls := 0
	model := batchFakeModel(t, 200, `[]`, func(string, string) { calls++ })
	if err := repo.batchStep(ctx, model, 24*time.Hour, now); err != nil || calls != 1 {
		t.Fatalf("discovery calls=%d err=%v", calls, err)
	}
	if err := repo.StageWrite(ctx, "default", "staged-only", "second", "Remember another fact", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.RunBatch(ctx, "default", "staged-only", now.Add(time.Hour), 24*time.Hour, model); err != nil || ok || calls != 1 {
		t.Fatalf("staging bypassed cadence: %v %v calls=%d", ok, err, calls)
	}
	// RunBatch's live lease check uses wall time, so keep the due check in the
	// present rather than advancing the fixture clock by a day.
	var next int64
	if err := repo.db.QueryRow(`SELECT next_attempt_ms FROM memory_batch_boundaries WHERE principal_id='staged-only'`).Scan(&next); err != nil || next != now.Add(24*time.Hour).UnixMilli() {
		t.Fatalf("next attempt=%d err=%v", next, err)
	}
}

func TestMigration106UpgradePreservesRows(t *testing.T) {
	store, repo := newMemoryTestRepo(t)
	ctx := context.Background()
	db := store.DB()
	// Reconstruct the exact pre-106 schema; migration 106 adds only this table
	// and index. Migrate then exercises its registered production Up function.
	if _, err := db.Exec(`DROP TABLE memory_staged_writes; DELETE FROM schema_migrations WHERE version=106;
INSERT INTO memory_batch_boundaries(workspace_id,principal_id,boundary_ns) VALUES('default','owner',123);`); err != nil {
		t.Fatal(err)
	}
	claim := writeMemory(t, ctx, db, repo, Scope{Kind: ScopePersonal, PrincipalID: "owner"}, "existing fact", "existing-source", 0)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM memory_claims WHERE id=?`, claim.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("claim lost: %d %v", count, err)
	}
	boundary, err := repo.BoundaryAt(ctx, "default", "owner")
	if err != nil || boundary.UnixNano() != 123 {
		t.Fatalf("boundary lost: %v %v", boundary, err)
	}
	if err := repo.StageWrite(ctx, "default", "owner", "new", "new request", time.Now()); err != nil {
		t.Fatal(err)
	}
}
