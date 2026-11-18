package voice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/storage"
)

const writeSentence = "  remember my preferred airport is LHR\n"
const writeRestatement = "The user prefers LHR."

func memoryWriteJobFixture(t *testing.T) (*storage.Store, *Service, func()) {
	t.Helper()
	// These tests drive dispatch explicitly. Do not start an HTTP fixture's
	// registered service, which would compete for the same durable jobs.
	store, svc := newVoiceTestService(t, Config{Now: func() time.Time {
		return time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
	}})
	t.Cleanup(svc.Stop)
	at := svc.now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('handoff-session','default','owner','instance','request',X'01',1,'active',?,?)`, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`CREATE TABLE memory_write_outcomes(op_id TEXT PRIMARY KEY,status TEXT NOT NULL,error_kind TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(writeSentence))
	_, err := store.DB().Exec(`INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,exits,request) VALUES('write-turn','default','handoff-session',1,'write-turn',1,?,?,?,'write-command','memory_write',?)`, writeSentence, hash[:], svc.now().Format(time.RFC3339Nano), writeRestatement)
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
	for _, mode := range []string{"success", "disabled", "blocked"} {
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
					if req.URL.Path != "/v1/memory/write" || len(body) != 3 || body["op_id"] == "" || body["request"] != writeRestatement || body["user"] != memoryUser("default", "owner") {
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
						time.Sleep(150 * time.Millisecond)
						return
					}
					if mode == "failure" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if mode == "uncertain" {
						w.WriteHeader(http.StatusConflict)
						_, _ = w.Write([]byte(`{"error_kind":"write_uncertain"}`))
						return
					}
					if mode == "blocked" {
						w.WriteHeader(http.StatusConflict)
						_, _ = w.Write([]byte(`{"error_kind":"write_quarantined"}`))
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
			workerCalls := calls.Load()
			pendingJobs := writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status='pending' AND question=?`, writeRestatement)
			holdingReplies := writeJobCount(t, store, `SELECT count(*) FROM voice_fast_replies WHERE intent_kind=? AND status='delivered' AND text=''`, memoryWriteHolding)
			if workerCalls != 0 || pendingJobs != 1 || holdingReplies != 1 {
				t.Fatalf("handoff: worker calls=%d want=0, pending jobs=%d want=1, delivered holding replies=%d want=1", workerCalls, pendingJobs, holdingReplies)
			}
			if handled := writeJobCount(t, store, `SELECT handoff_handled FROM voice_turns WHERE id='write-turn'`); handled != 1 {
				t.Fatalf("turn not handled atomically with job: handoff_handled=%d want=1", handled)
			}
			// Later handled transcript revisions must not replace the original
			// sentence paired with the first durable job's restatement.
			if _, err := store.DB().Exec(`INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,exits,request,handoff_handled) SELECT 'later-revision',workspace_id,session_id,generation,turn_id,2,'different sentence',content_hash,confirmed_at,command_id,exits,'different restatement',1 FROM voice_turns WHERE id='write-turn'`); err != nil {
				t.Fatal(err)
			}
			svc.dispatchRecall(context.Background())
			dispatched := make(chan struct{})
			go func() {
				svc.workers.Wait()
				close(dispatched)
			}()
			select {
			case <-dispatched:
			case <-time.After(10 * time.Second):
				t.Fatalf("dispatch did not finish: worker calls=%d", calls.Load())
			}
			if terminal := writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status IN ('done','blocked')`); terminal != 1 {
				t.Fatalf("terminal jobs=%d want=1", terminal)
			}
			var outcome string
			if err := store.DB().QueryRow(`SELECT status FROM memory_write_outcomes`).Scan(&outcome); err != nil {
				t.Fatal(err)
			}
			wantOutcome := "write_error"
			if mode == "success" {
				wantOutcome = "write_ok"
			}
			if mode == "blocked" {
				wantOutcome = "write_blocked"
				if blocked := writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status='blocked'`); blocked != 1 {
					t.Fatalf("quarantine not persisted as blocked: blocked jobs=%d want=1, outcome=%s", blocked, outcome)
				}
			}
			if mode == "uncertain" || mode == "timeout" || mode == "failure" {
				wantOutcome = "write_uncertain"
			}
			if outcome != wantOutcome {
				t.Fatalf("outcome=%s want=%s", outcome, wantOutcome)
			}
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
			workerCalls = calls.Load()
			jobs := writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs`)
			replies := writeJobCount(t, store, `SELECT count(*) FROM voice_fast_replies WHERE text<>''`)
			errors := writeJobCount(t, store, `SELECT count(*) FROM voice_fast_replies WHERE text=?`, stewardErrorSpeech)
			tasks := writeJobCount(t, store, `SELECT count(*) FROM tasks`)
			if workerCalls != wantCalls || jobs != 1 || replies != wantErrors || errors != wantErrors || tasks != 0 {
				t.Fatalf("mode=%s calls=%d want=%d, jobs=%d want=1, replies=%d errors=%d want=%d each, tasks=%d want=0", mode, workerCalls, wantCalls, jobs, replies, errors, wantErrors, tasks)
			}
		})
	}
}

func TestMemoryWriteRunningRecoveryReconcilesStableOperation(t *testing.T) {
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
			svc.workers.Wait()
			if calls.Load() != 1 || writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status='done'`) != 1 || writeJobCount(t, store, `SELECT count(*) FROM voice_fast_replies WHERE text=?`, stewardErrorSpeech) != 0 || writeJobCount(t, store, `SELECT count(*) FROM voice_fast_replies WHERE text<>''`) != 0 || writeJobCount(t, store, `SELECT count(*) FROM tasks`) != 0 {
				t.Fatal("recovery resent write or failed to persist exactly one error")
			}
		})
	}
}

func TestMemoryWriteTransientRetriesSameOperation(t *testing.T) {
	store, svc, handoff := memoryWriteJobFixture(t)
	var calls atomic.Int32
	var first string
	svc.recall.MemoryAPI = workerClient(t, func(w http.ResponseWriter, r *http.Request) {
		var input map[string]string
		_ = json.NewDecoder(r.Body).Decode(&input)
		if calls.Add(1) == 1 {
			first = input["op_id"]
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error_kind":"write_uncertain"}`))
			return
		}
		if input["op_id"] != first || input["request"] != writeRestatement {
			t.Errorf("changed identity/request: %v", input)
		}
		_, _ = w.Write([]byte(`{"operations":[]}`))
	})
	handoff()
	svc.dispatchRecall(context.Background())
	svc.workers.Wait()
	if writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status='pending'`) != 1 || writeJobCount(t, store, `SELECT count(*) FROM voice_fast_replies WHERE text<>''`) != 0 {
		t.Fatal("transient finalized or announced failure")
	}
	svc.dispatchRecall(context.Background())
	svc.workers.Wait()
	if calls.Load() != 1 {
		t.Fatal("retried before backoff elapsed")
	}
	if _, err := store.DB().Exec(`UPDATE model_retry_state SET next_attempt_at='' WHERE consumer='memory_write'`); err != nil {
		t.Fatal(err)
	}
	svc.dispatchRecall(context.Background())
	svc.workers.Wait()
	if calls.Load() != 2 || writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status='done'`) != 1 || writeJobCount(t, store, `SELECT count(*) FROM voice_fast_replies WHERE text<>''`) != 0 {
		t.Fatal("retry did not reconcile silently")
	}
}

func TestMemoryWriteAuthStopRetainsOperation(t *testing.T) {
	store, svc, handoff := memoryWriteJobFixture(t)
	retry := backoff.Store{DB: store.DB()}
	if _, err := retry.ObserveConfig(context.Background(), "memory_write", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var first string
	svc.recall.MemoryAPI = workerClient(t, func(w http.ResponseWriter, r *http.Request) {
		var input map[string]string
		_ = json.NewDecoder(r.Body).Decode(&input)
		if calls.Add(1) == 1 {
			first = input["op_id"]
			w.WriteHeader(502)
			_, _ = w.Write([]byte(`{"error_kind":"upstream_unauthorized"}`))
			return
		}
		if input["op_id"] != first {
			t.Error("operation identity changed")
		}
		_, _ = w.Write([]byte(`{"operations":[]}`))
	})
	handoff()
	svc.dispatchRecall(context.Background())
	svc.workers.Wait()
	svc.dispatchRecall(context.Background())
	svc.workers.Wait()
	if calls.Load() != 1 || writeJobCount(t, store, `SELECT count(*) FROM memory_write_outcomes WHERE status='write_auth_stopped'`) != 1 {
		t.Fatal("write not stopped and retained")
	}
	if changed, err := retry.ObserveConfig(context.Background(), "memory_write", strings.Repeat("a", 64)); err != nil || changed {
		t.Fatalf("unchanged config: %v %v", changed, err)
	}
	svc.dispatchRecall(context.Background())
	svc.workers.Wait()
	if calls.Load() != 1 {
		t.Fatal("unchanged config cleared stop")
	}
	if changed, err := retry.ObserveConfig(context.Background(), "memory_write", strings.Repeat("b", 64)); err != nil || !changed {
		t.Fatal(err)
	}
	svc.dispatchRecall(context.Background())
	svc.workers.Wait()
	if calls.Load() != 2 || writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status='done'`) != 1 {
		t.Fatal("write did not resume")
	}
}

func TestMemoryWriteScopeIdentity(t *testing.T) {
	for _, pair := range [][2]string{{"default", "owner"}, {"a/\"b", "c\\d"}, {"", ""}} {
		if memoryUser(pair[0], pair[1]) != memoryapi.User(pair[0], pair[1]) {
			t.Fatal("scope differs")
		}
	}
}

func TestMemoryWriteRecordFailureRecoversForReconciliation(t *testing.T) {
	for _, startup := range []bool{false, true} {
		t.Run(fmt.Sprintf("startup=%v", startup), func(t *testing.T) {
			store, svc, handoff := memoryWriteJobFixture(t)
			var calls atomic.Int32
			var first string
			svc.recall.MemoryAPI = workerClient(t, func(w http.ResponseWriter, r *http.Request) {
				var input map[string]string
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				if calls.Add(1) == 1 {
					first = input["op_id"]
					if _, err := store.DB().Exec(`CREATE TRIGGER retry_record_failure BEFORE INSERT ON model_retry_state BEGIN SELECT RAISE(FAIL, 'injected'); END`); err != nil {
						t.Error(err)
					}
				} else if first == "" || input["op_id"] != first || input["request"] != writeRestatement {
					t.Error("reconciliation changed operation identity or request")
				}
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error_kind":"write_uncertain"}`))
			})
			handoff()
			svc.dispatchRecall(context.Background())
			svc.workers.Wait()
			if calls.Load() != 1 || writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status='running'`) != 1 || writeJobCount(t, store, `SELECT count(*) FROM memory_write_outcomes WHERE status='write_uncertain'`) != 1 {
				t.Fatal("record failure did not retain running uncertain job")
			}
			if _, err := store.DB().Exec(`DROP TRIGGER retry_record_failure`); err != nil {
				t.Fatal(err)
			}
			if !startup {
				svc.recoverRecall(context.Background(), false)
				if writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status='running'`) != 1 {
					t.Fatal("recovered before lease expired")
				}
				if _, err := store.DB().Exec(`UPDATE voice_fast_replies SET updated_at='2000-01-01T00:00:00Z'`); err != nil {
					t.Fatal(err)
				}
			}
			svc.recoverRecall(context.Background(), startup)
			if writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status='pending'`) != 1 {
				t.Fatal("recovery did not retain job")
			}
			svc.dispatchRecall(context.Background())
			svc.workers.Wait()
			if calls.Load() != 2 || writeJobCount(t, store, `SELECT count(*) FROM memory_write_outcomes WHERE status='write_uncertain'`) != 1 || writeJobCount(t, store, `SELECT count(*) FROM voice_recall_jobs WHERE status='pending'`) != 1 {
				t.Fatal("uncertain reconciliation was not retained")
			}
		})
	}
}
