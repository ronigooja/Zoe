package voice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

const writeSentence = "  remember my preferred airport is LHR\n"
const writeRestatement = "The user prefers LHR."

func memoryWriteJobFixture(t *testing.T) (*storage.Store, *Service, func()) {
	t.Helper()
	store, svc := recallRecoveryFixture(t, Config{}, 0)
	hash := sha256.Sum256([]byte(writeSentence))
	_, err := store.DB().Exec(`INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,triage_category,report_text) VALUES('write-turn','default','handoff-session',1,'write-turn',1,?,?,?,'write-command','memory_write',?)`, writeSentence, hash[:], svc.now().Format(time.RFC3339Nano), writeRestatement)
	if err != nil {
		t.Fatal(err)
	}
	handoff := func() {
		t.Helper()
		tx, err := store.DB().BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		cmd := control.CommandContext{CommandID: "write-handoff", AcceptedAt: svc.now(), Principal: contracts.Principal{WorkspaceID: "default", SubjectID: "owner", DeviceID: "instance", Scopes: []string{"client:command"}}}
		result, _, err := svc.commandHandoff(context.Background(), tx, cmd, contracts.CommandRequest{Payload: mustJSON(handoffPayload{SessionID: "handoff-session", Generation: 1, TurnID: "write-turn", Revision: 1, ContentHash: hex.EncodeToString(hash[:])})}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.ResourceType != "voice_turn" || result.ResourceID != "write-turn" {
			t.Fatalf("result=%+v", result)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	return store, svc, handoff
}

func writeJobCount(t *testing.T, store *storage.Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestMemoryWriteDurableHandoffAndDispatch(t *testing.T) {
	for _, mode := range []string{"success", "failure", "disabled", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			store, svc, handoff := memoryWriteJobFixture(t)
			var calls atomic.Int32
			if mode != "disabled" {
				svc.recall.MemoryAPI = workerClient(t, func(w http.ResponseWriter, req *http.Request) {
					calls.Add(1)
					var body map[string]string
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if req.URL.Path != "/v1/memory/write" || body["sentence"] != writeSentence || body["restatement"] != writeRestatement || body["user"] != memoryUser("default", "owner") {
						t.Errorf("path=%s body=%v", req.URL.Path, body)
					}
					// The store has one connection. This independent write would
					// block if the worker call retained any open transaction.
					ctx, cancel := context.WithTimeout(req.Context(), 500*time.Millisecond)
					defer cancel()
					if _, err := store.DB().ExecContext(ctx, `UPDATE voice_sessions SET updated_at=updated_at WHERE id='handoff-session'`); err != nil {
						t.Errorf("worker called inside transaction: %v", err)
					}
					if mode == "timeout" {
						<-req.Context().Done()
						return
					}
					if mode == "failure" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					_, _ = w.Write([]byte(`{"operations":[]}`))
				})
			}
			if mode == "timeout" {
				svc.recall.RecallTimeout = 100 * time.Millisecond
			}
			handoff()
			handoff()
			if calls.Load() != 0 || writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status='pending' AND question=?`, writeRestatement) != 1 || writeJobCount(t, store, `SELECT count(*) FROM voice_fast_replies WHERE intent_kind=? AND status='delivered' AND text=''`, memoryWriteHolding) != 1 {
				t.Fatal("handoff called worker or failed to enqueue exactly once")
			}
			if writeJobCount(t, store, `SELECT handoff_handled FROM voice_turns WHERE id='write-turn'`) != 1 {
				t.Fatal("turn not handled atomically with job")
			}
			// Later handled transcript revisions must not replace the original
			// sentence paired with the first durable job's restatement.
			if _, err := store.DB().Exec(`INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,triage_category,report_text,handoff_handled) SELECT 'later-revision',workspace_id,session_id,generation,turn_id,2,'different sentence',content_hash,confirmed_at,command_id,triage_category,'different restatement',1 FROM voice_turns WHERE id='write-turn'`); err != nil {
				t.Fatal(err)
			}
			svc.dispatchRecall(context.Background())
			waitRecall(t, func() bool {
				return writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status='done'`) == 1
			})
			handoff()
			svc.dispatchRecall(context.Background())
			svc.recoverRecall(context.Background(), true)
			svc.workers.Wait()
			wantCalls := int32(1)
			if mode == "disabled" {
				wantCalls = 0
			}
			wantErrors := 1
			if mode == "success" {
				wantErrors = 0
			}
			if calls.Load() != wantCalls || writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs`) != 1 || writeJobCount(t, store, `SELECT count(*) FROM voice_fast_replies WHERE text<>''`) != wantErrors || writeJobCount(t, store, `SELECT count(*) FROM voice_fast_replies WHERE text=?`, stewardErrorSpeech) != wantErrors || writeJobCount(t, store, `SELECT count(*) FROM tasks`) != 0 {
				t.Fatalf("mode=%s calls=%d; expected one job, %d errors, no tasks", mode, calls.Load(), wantErrors)
			}
		})
	}
}

func TestMemoryWriteRunningRecoveryNeverResends(t *testing.T) {
	for _, startup := range []bool{true, false} {
		t.Run(map[bool]string{true: "restart", false: "lease"}[startup], func(t *testing.T) {
			store, svc, handoff := memoryWriteJobFixture(t)
			var calls atomic.Int32
			svc.recall.MemoryAPI = workerClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				_, _ = w.Write([]byte(`{"operations":[]}`))
			})
			handoff()
			if _, err := store.DB().Exec(`UPDATE voice_recall_jobs SET status='running'`); err != nil {
				t.Fatal(err)
			}
			if !startup {
				svc.recoverRecall(context.Background(), false)
				if writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status='running'`) != 1 {
					t.Fatal("recovered before lease expiry")
				}
				if _, err := store.DB().Exec(`UPDATE voice_fast_replies SET updated_at='2000-01-01T00:00:00Z'`); err != nil {
					t.Fatal(err)
				}
			}
			svc.recoverRecall(context.Background(), startup)
			svc.recoverRecall(context.Background(), true)
			handoff()
			svc.dispatchRecall(context.Background())
			if calls.Load() != 0 || writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status='done'`) != 1 || writeJobCount(t, store, `SELECT count(*) FROM voice_fast_replies WHERE text=?`, stewardErrorSpeech) != 1 || writeJobCount(t, store, `SELECT count(*) FROM voice_fast_replies WHERE text<>''`) != 1 || writeJobCount(t, store, `SELECT count(*) FROM tasks`) != 0 {
				t.Fatal("recovery resent write or failed to persist exactly one error")
			}
		})
	}
}
