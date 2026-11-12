package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

func batchFixture(t *testing.T) (*sql.DB, *Repository, time.Time, time.Time) {
	t.Helper()
	store, repo := newMemoryTestRepo(t)
	db := store.DB()
	// Minimal real SQLite transcript fixture reproduces the production columns;
	// importing voice here would introduce a memory/voice import cycle.
	_, err := db.Exec(`CREATE TABLE voice_sessions(id TEXT PRIMARY KEY,workspace_id TEXT,owner_id TEXT);
CREATE TABLE voice_transcript_turns(workspace_id TEXT,session_id TEXT,seq INTEGER,role TEXT,text TEXT,at TEXT,received_at TEXT,PRIMARY KEY(session_id,seq));
INSERT INTO voice_sessions VALUES('a','default','owner'),('b','default','owner'),('other','default','other');
INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES('device','default','owner','fixture','approved','2026-11-12T00:00:00Z');`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	from := now.Add(-24 * time.Hour)
	_, err = db.Exec(`INSERT INTO memory_batch_boundaries(workspace_id,principal_id,boundary_ns) VALUES('default','owner',?)`, from.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	turn := func(session string, seq int, role, text string, at time.Time) {
		t.Helper()
		_, e := db.Exec(`INSERT INTO voice_transcript_turns VALUES('default',?,?,?,?,?,?)`, session, seq, role, text, at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
		if e != nil {
			t.Fatal(e)
		}
	}
	turn("a", 0, "user", "at boundary excluded", from)
	turn("b", 0, "assistant", "assistant is data", from.Add(time.Hour))
	turn("a", 1, "user", "I prefer tea", from.Add(time.Hour))
	turn("a", 2, "assistant", "at now included", now)
	turn("a", 3, "user", "future excluded", now.Add(time.Nanosecond))
	turn("other", 0, "user", "other owner secret", from.Add(time.Hour))
	for _, task := range []struct {
		id, owner, status string
		at                time.Time
	}{{"done", "owner", "complete", now}, {"old", "owner", "complete", from}, {"future", "owner", "complete", now.Add(time.Second)}, {"foreign", "other", "complete", now}, {"failed", "owner", "failed", now}} {
		_, err = db.Exec(`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,finished_at,result,created_by_device_id,command_id,owner_subject_id) VALUES(?,'default',?,'rewritten goal','voice','fixture',?,1,?,?,?,'full durable result','device',?,?)`, task.id, "original request "+task.id, task.status, from.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), task.at.Format(time.RFC3339Nano), task.id, task.owner)
		if err != nil {
			t.Fatal(err)
		}
	}
	return db, repo, from, now
}

func batchFakeModel(t *testing.T, status int, output string, inspect func(string, string)) BatchModel {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Instructions string `json:"instructions"`
			Input        string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if inspect != nil {
			inspect(req.Instructions, req.Input)
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"output_text": output})
	}))
	t.Cleanup(server.Close)
	env := map[string]string{"ZOE_MEM0_LLM_BASE_URL": server.URL, "ZOE_MEM0_LLM_MODEL": "fixture", "ZOE_MEM0_LLM_API_KEY_FILE": "fixture"}
	client, err := BatchModelFromEnv(func(k string) string { return env[k] }, func(string) ([]byte, error) { return []byte("test-only"), nil })
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestMemoryBatchInterval(t *testing.T) {
	for _, s := range []string{"0", "169", "bad"} {
		if _, err := BatchInterval(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
	for _, s := range []string{"", "1", "24", "168"} {
		if _, err := BatchInterval(s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMemoryBatchWindowKeysIsolationAtomicSuccessAndRead(t *testing.T) {
	db, repo, _, now := batchFixture(t)
	ctx := context.Background()
	writeMemory(t, ctx, db, repo, Scope{Kind: ScopePersonal, PrincipalID: "owner"}, "existing fact", "existing-source", 0)
	calls := 0
	model := batchFakeModel(t, 200, `[{"subject":"user","key":"drink","content":"I prefer tea"}]`, func(instruction, input string) {
		calls++
		if instruction != MemoryExtractionInstruction {
			t.Error("instruction changed")
		}
		var in batchInput
		if err := json.Unmarshal([]byte(input), &in); err != nil {
			t.Fatal(err)
		}
		if len(in.Transcript) != 3 || in.Transcript[0].Session != "a" || in.Transcript[1].Session != "b" || in.Transcript[2].Text != "at now included" {
			t.Errorf("window/order: %#v", in.Transcript)
		}
		if len(in.Tasks) != 1 || in.Tasks[0].Request != "original request done" || in.Tasks[0].Result != "full durable result" {
			t.Errorf("tasks: %#v", in.Tasks)
		}
		if len(in.Keys) != 1 {
			t.Errorf("keys: %#v", in.Keys)
		}
		for _, s := range []string{"other owner secret", "future excluded", "at boundary excluded", "rewritten goal"} {
			if strings.Contains(input, s) {
				t.Errorf("leaked %s", s)
			}
		}
	})
	ok, err := repo.RunBatch(ctx, "default", "owner", now, 24*time.Hour, model)
	if err != nil || !ok {
		t.Fatalf("run=%v err=%v", ok, err)
	}
	boundary, err := repo.ReadBoundary(ctx, "default", "owner")
	if err != nil || !boundary.Equal(now) {
		t.Fatalf("boundary=%v err=%v", boundary, err)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM memory_proposals`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("proposals=%d err=%v", count, err)
	}
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "owner", Scopes: []string{"memory:read:personal"}}
	_, claims, err := repo.AssembleContext(ctx, p, "", "owner", "drink", contracts.MemoryContextDefaultBudgetTokens)
	if err != nil || len(claims) != 1 || claims[0].Status != ClaimActive || claims[0].Content != "I prefer tea" || claims[0].Authority != AuthorityModelInferred || len(claims[0].Sources) != 1 || claims[0].Sources[0].Kind != "memory_batch" {
		t.Fatalf("batch not readable: %#v err=%v", claims, err)
	}
	p.SubjectID = "other"
	_, claims, err = repo.AssembleContext(ctx, p, "", "other", "drink", contracts.MemoryContextDefaultBudgetTokens)
	if err != nil || len(claims) != 0 {
		t.Fatalf("owner read leak: %#v err=%v", claims, err)
	}
	// Idempotent repeat and a later empty period never dispatch a model.
	if ok, err = repo.RunBatch(ctx, "default", "owner", now, 24*time.Hour, model); err != nil || ok {
		t.Fatalf("rerun %v %v", ok, err)
	}
	_, err = db.Exec(`DELETE FROM voice_transcript_turns WHERE session_id='a' AND seq=3;DELETE FROM tasks WHERE id='future'`)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err = repo.RunBatch(ctx, "default", "owner", now.Add(25*time.Hour), 24*time.Hour, model); err != nil || ok {
		t.Fatalf("empty %v %v", ok, err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	other, err := repo.ReadBoundary(ctx, "default", "other")
	if err != nil || !other.Equal(time.Unix(0, 0)) {
		t.Fatalf("owner leaked: %v %v", other, err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM memory_claims`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("idempotence claims=%d err=%v", count, err)
	}
}

func TestMemoryBatchUpdatesExistingKeyWithoutDuplicates(t *testing.T) {
	db, repo, _, now := batchFixture(t)
	ctx := context.Background()
	first := writeMemory(t, ctx, db, repo, Scope{Kind: ScopePersonal, PrincipalID: "owner"}, "old fact", "original", 0)
	output, _ := json.Marshal([]extractedMemory{{Subject: first.Subject, Key: first.Key, Content: "updated durable fact"}})
	model := batchFakeModel(t, 200, string(output), nil)
	if ok, err := repo.RunBatch(ctx, "default", "owner", now, time.Hour, model); err != nil || !ok {
		t.Fatalf("update=%v err=%v", ok, err)
	}
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "owner", Scopes: []string{"memory:read:personal"}}
	_, claims, err := repo.AssembleContext(ctx, p, "", "owner", "", contracts.MemoryContextDefaultBudgetTokens)
	if err != nil || len(claims) != 1 || claims[0].ID != first.ID || claims[0].Revision != first.Revision+1 || claims[0].Content != "updated durable fact" || claims[0].Status != ClaimActive {
		t.Fatalf("update %#v err=%v", claims, err)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM memory_claims`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicates=%d err=%v", count, err)
	}
}

func TestMemoryBatchTrustDoesNotEscapeOtherWritePaths(t *testing.T) {
	db, repo, _, now := batchFixture(t)
	ctx := context.Background()
	source := Source{ID: "forged-batch", Version: 1, Scope: Scope{Kind: ScopePersonal, PrincipalID: "owner"}, Kind: "memory_batch", Reference: "forged", Producer: "memory-batch", RecordedAt: now}
	claim := Claim{ID: "fixture", WorkspaceID: "default", Scope: source.Scope, Subject: "user", Key: "drink", Content: "untrusted", Authority: AuthorityModelInferred, Status: ClaimActive, Revision: 1, Sources: []Source{source}}
	if err := claim.ValidateForWrite(); err == nil {
		t.Fatal("public validation accepted active model output")
	}
	claim.ID = ""
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = repo.PutTx(ctx, tx, WriteInput{WorkspaceID: "default", ActorID: "owner", Claim: claim, Source: source}); err == nil {
		t.Fatal("manual write forged batch trust")
	}
	if _, err = repo.putBatchClaimTx(ctx, tx, "default", "other", source, extractedMemory{Subject: "u", Key: "k", Content: "v"}); err == nil {
		t.Fatal("batch owner mismatch accepted")
	}
	if _, err = repo.putBatchClaimTx(ctx, tx, "default", "owner", source, extractedMemory{Subject: "u", Key: "k", Content: "v"}); err == nil {
		t.Fatal("batch without evidence accepted")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	source.Kind = "user_note"
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = insertSourceTx(ctx, tx, "default", source); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	proposal, err := repo.CreateProposal(ctx, contracts.RunnerPrincipal{WorkspaceIDs: []string{"default"}}, runnerProposalRequest{WorkspaceID: "default", SourceID: source.ID, SourceVersion: 1, ExtractorVersion: "other", Scope: source.Scope, Subject: "u", Key: "k", Content: "v"})
	if err != nil || proposal.Status != "pending" {
		t.Fatalf("other proposal=%#v err=%v", proposal, err)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM memory_claims`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("other path created claims=%d err=%v", count, err)
	}
}

func TestMemoryBatchEmptyExtractionAdvancesWithoutClaims(t *testing.T) {
	db, repo, _, now := batchFixture(t)
	model := batchFakeModel(t, 200, `[]`, nil)
	ok, err := repo.RunBatch(context.Background(), "default", "owner", now, time.Hour, model)
	if err != nil || !ok {
		t.Fatalf("run=%v err=%v", ok, err)
	}
	boundary, err := repo.ReadBoundary(context.Background(), "default", "owner")
	if err != nil || !boundary.Equal(now) {
		t.Fatalf("boundary=%v err=%v", boundary, err)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM memory_claims`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("claims=%d err=%v", count, err)
	}
}

func TestMemoryBatchLeaseFencesModelDispatch(t *testing.T) {
	db, repo, from, now := batchFixture(t)
	_, err := db.Exec(`UPDATE memory_batch_boundaries SET lease_token='other',lease_until_ms=? WHERE principal_id='owner'`, now.Add(time.Minute).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	model := batchFakeModel(t, 200, `[]`, func(string, string) { t.Error("leased owner called model") })
	if ok, err := repo.RunBatch(context.Background(), "default", "owner", now, time.Hour, model); err != nil || ok {
		t.Fatalf("leased run=%v err=%v", ok, err)
	}
	boundary, err := repo.ReadBoundary(context.Background(), "default", "owner")
	if err != nil || !boundary.Equal(from) {
		t.Fatalf("boundary=%v err=%v", boundary, err)
	}
}

func TestMemoryBatchFailuresRollbackAndRetry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		output  string
		trigger string
	}{{"model", 500, `[]`, ""}, {"invalid_json", 200, `{}`, ""}, {"null", 200, `null`, ""}, {"invalid_fields", 200, `[{"subject":"u","key":"k","content":""}]`, ""}, {"write", 200, `[{"subject":"u","key":"k","content":"v"}]`, `CREATE TRIGGER fail_boundary BEFORE UPDATE OF boundary_ns ON memory_batch_boundaries BEGIN SELECT RAISE(ABORT,'fixture write failure'); END;`}} {
		t.Run(tc.name, func(t *testing.T) {
			db, repo, from, now := batchFixture(t)
			ctx := context.Background()
			if tc.trigger != "" {
				if _, err := db.Exec(tc.trigger); err != nil {
					t.Fatal(err)
				}
			}
			model := batchFakeModel(t, tc.status, tc.output, nil)
			if ok, err := repo.RunBatch(ctx, "default", "owner", now, 24*time.Hour, model); err == nil || ok {
				t.Fatalf("expected failure: %v %v", ok, err)
			}
			boundary, err := repo.ReadBoundary(ctx, "default", "owner")
			if err != nil || !boundary.Equal(from) {
				t.Fatalf("boundary moved: %v %v", boundary, err)
			}
			for _, table := range []string{"memory_claims", "memory_proposals", "memory_sources", "memory_batch_inputs"} {
				var count int
				if err = db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("partial %s=%d err=%v", table, count, err)
				}
			}
			if tc.trigger != "" {
				if _, err = db.Exec(`DROP TRIGGER fail_boundary`); err != nil {
					t.Fatal(err)
				}
			}
			retry := batchFakeModel(t, 200, `[{"subject":"u","key":"k","content":"v"}]`, nil)
			if ok, err := repo.RunBatch(ctx, "default", "owner", now.Add(2*time.Minute), 24*time.Hour, retry); err != nil || !ok {
				t.Fatalf("retry: %v %v", ok, err)
			}
		})
	}
}
