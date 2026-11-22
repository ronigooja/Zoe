package voice

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

func checkpointRig(t *testing.T) *deliveryRig {
	t.Helper()
	r := newReceiptDeliveryRig(t)
	// Checkpoint tables come from the production fresh baseline.
	return r
}

// Exercise the shared transactional binding fences independently of report eligibility.
func (s *Service) selectPendingResultForTest(ctx context.Context, principal contracts.Principal, entry, session string, generation int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.selectPendingResultTx(ctx, tx, principal, entry, session, generation); err != nil {
		return err
	}
	return tx.Commit()
}

func TestResultCheckpointExplicitSelectionDelivers(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "original result")
	r.run(t)
	r.seedCall(t, "next", "user-1", 2)
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1"}
	for i := 0; i < 2; i++ {
		if err := r.svc.selectPendingResultForTest(context.Background(), p, "entry-1", "next", 2); err != nil {
			t.Fatal(err)
		}
	}
	r.run(t)
	status, _, attempts := r.row(t, "entry-1")
	if status != "delivered" || attempts != 1 || r.posts.Load() != 1 {
		t.Fatalf("status=%s attempts=%d posts=%d", status, attempts, r.posts.Load())
	}
	pending, err := pendingResultsForContext(context.Background(), r.store.DB(), "default", "user-1")
	if err != nil || len(pending) != 0 {
		t.Fatalf("sent history=%v err=%v", pending, err)
	}
}

func TestResultCheckpointSelectionUnknownCannotRebind(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "original result")
	r.seedCall(t, "old", "user-1", 1)
	r.code.Store(503)
	r.run(t)
	r.exec(t, `UPDATE voice_sessions SET state='ended' WHERE id='old'`)
	r.seedCall(t, "next", "user-1", 2)
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1"}
	if err := r.svc.selectPendingResultForTest(context.Background(), p, "entry-1", "next", 2); err == nil {
		t.Fatal("unknown destination rebound")
	}
	if err := r.svc.selectPendingResultForTest(context.Background(), p, "entry-1", "next", 99); err == nil {
		t.Fatal("wrong generation accepted")
	}
	var session string
	_ = r.store.DB().QueryRow(`SELECT session_id FROM voice_result_checkpoints`).Scan(&session)
	if session != "old" {
		t.Fatal(session)
	}
	pending, err := pendingResultsForContext(context.Background(), r.store.DB(), "default", "user-1")
	if err != nil || len(pending) != 0 {
		t.Fatalf("ambiguous history=%v err=%v", pending, err)
	}
}

func TestResultCheckpointSelectionUnsentCanRebind(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "original result")
	r.svc.consumeResultEvents(context.Background())
	r.seedCall(t, "old", "user-1", 1)
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1"}
	if err := r.svc.selectPendingResultForTest(context.Background(), p, "entry-1", "old", 1); err != nil {
		t.Fatal(err)
	}
	r.exec(t, `UPDATE voice_sessions SET state='ended' WHERE id='old'`)
	r.seedCall(t, "next", "user-1", 2)
	if err := r.svc.selectPendingResultForTest(context.Background(), p, "entry-1", "next", 2); err != nil {
		t.Fatal(err)
	}
	r.run(t)
	if len(r.paths) != 1 || r.paths[0] != "/v1/voice/sessions/gw-next/result-deliveries" {
		t.Fatal(r.paths)
	}
}

func TestResultCheckpointForgedDeliveryRejectedByEvent(t *testing.T) {
	for _, field := range []string{"run", "entry", "task"} {
		t.Run(field, func(t *testing.T) {
			r := checkpointRig(t)
			r.setup(t, "real")
			r.seedCall(t, "s1", "user-1", 1)
			r.svc.consumeResultEvents(context.Background())
			id, task, run := "entry-1", "task-1", "run-1"
			switch field {
			case "run":
				run = "forged"
				r.exec(t, `UPDATE voice_result_deliveries SET run_id='forged'`)
			case "entry":
				id = "forged"
				r.seedEntry(t, "task-1", id, "forged")
				r.exec(t, `UPDATE voice_result_deliveries SET entry_id='forged'`)
			case "task":
				task = "forged"
				r.seedTask(t, task, "user-1")
				r.exec(t, `UPDATE task_entries SET task_id='forged'`)
				r.exec(t, `UPDATE voice_result_deliveries SET task_id='forged'`)
			}
			r.svc.deliverCheckpointedResult(context.Background(), id, "default", task, run, "user-1", 0, nil)
			if r.posts.Load() != 0 {
				t.Fatal("forged source sent")
			}
			p := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1"}
			if err := r.svc.selectPendingResultForTest(context.Background(), p, id, "s1", 1); err == nil {
				t.Fatal("forged source selected")
			}
			pending, err := pendingResultsForContext(context.Background(), r.store.DB(), "default", "user-1")
			if err != nil || len(pending) != 0 {
				t.Fatalf("forged context=%v err=%v", pending, err)
			}
		})
	}
}

type checkpointAdapter struct {
	submits, lookups int
	failAt           int
	lookupApplied    bool
	requests         []ResultChunkRequest
}

func (a *checkpointAdapter) Submit(_ context.Context, _ string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	a.submits++
	a.requests = append(a.requests, p)
	if a.submits == a.failAt {
		return ResultChunkReceipt{}, errors.New("lost response")
	}
	return ResultChunkReceipt{p.MediaGeneration, p.DeliveryID, p.ContentHash, "applied"}, nil
}
func (a *checkpointAdapter) Lookup(_ context.Context, _ string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	a.lookups++
	if a.lookupApplied {
		return ResultChunkReceipt{p.MediaGeneration, p.DeliveryID, p.ContentHash, "applied"}, nil
	}
	return ResultChunkReceipt{p.MediaGeneration, p.DeliveryID, p.ContentHash, "unknown"}, nil
}
func (r *deliveryRig) checkpointSend(a ResultReceiptAdapter) {
	_, _, attempts := r.rowForCheckpoint()
	r.svc.deliverCheckpointedResult(context.Background(), "entry-1", "default", "task-1", "run-1", "user-1", attempts, a)
}
func (r *deliveryRig) rowForCheckpoint() (string, string, int) {
	var status, code string
	var attempts int
	_ = r.store.DB().QueryRow(`SELECT status,last_error_code,attempts FROM voice_result_deliveries WHERE entry_id='entry-1'`).Scan(&status, &code, &attempts)
	return status, code, attempts
}

func TestResultCheckpointNoCallPreservesOpeningCandidate(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, " original\nresult ")
	for i := 0; i < 8; i++ {
		r.run(t)
		r.advance(time.Minute)
	}
	status, code, attempts := r.row(t, "entry-1")
	if status != "pending" || code != "awaiting_call" || attempts != 0 || r.posts.Load() != 0 {
		t.Fatalf("status=%s code=%s attempts=%d posts=%d", status, code, attempts, r.posts.Load())
	}
	pending, err := pendingResultsForContext(context.Background(), r.store.DB(), "default", "user-1")
	if err != nil || len(pending) != 1 || pending[0].EntryID != "entry-1" || !strings.HasSuffix(pending[0].Text, "Result:  original\nresult ") {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	other, err := pendingResultsForContext(context.Background(), r.store.DB(), "default", "other")
	if err != nil || len(other) != 0 {
		t.Fatalf("other=%v err=%v", other, err)
	}
}

func TestResultCheckpointRestartSkipsConfirmedFencesUnknown(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, strings.Repeat("界", 6000))
	r.seedCall(t, "s1", "user-1", 1)
	r.exec(t, `UPDATE voice_sessions SET gateway_media_generation=42 WHERE id='s1'`)
	r.svc.consumeResultEvents(context.Background())
	a := &checkpointAdapter{failAt: 2}
	r.checkpointSend(a)
	if a.submits != 2 || a.requests[0].MediaGeneration != 42 {
		t.Fatalf("adapter=%+v", a)
	}
	var state string
	_ = r.store.DB().QueryRow(`SELECT state FROM voice_result_chunks WHERE ordinal=0`).Scan(&state)
	if state != "confirmed" {
		t.Fatal(state)
	}
	_ = r.store.DB().QueryRow(`SELECT state FROM voice_result_chunks WHERE ordinal=1`).Scan(&state)
	if state != "unknown" {
		t.Fatal(state)
	}
	// Simulate process death after persisting attempt, before confirmation checkpoint.
	r.exec(t, `UPDATE voice_result_chunks SET state='attempted' WHERE ordinal=1`)
	r.svc = newService(r.store.DB(), Config{GatewayURL: r.svc.base.String(), GatewayAPIKey: "gw-key", HTTPClient: r.svc.client, Now: r.now})
	r.advance(time.Minute)
	r.checkpointSend(a)
	if a.submits != 2 || a.lookups != 1 {
		t.Fatalf("submits=%d lookups=%d", a.submits, a.lookups)
	}
	status, _, _ := r.row(t, "entry-1")
	if status != "pending" {
		t.Fatal(status)
	}
	// An unknown lookup must keep the suffix blocked, even on repeated retries.
	r.advance(time.Minute)
	r.checkpointSend(a)
	if a.submits != 2 || a.lookups != 2 {
		t.Fatalf("unknown receipt replayed prefix: submits=%d lookups=%d", a.submits, a.lookups)
	}
	a.lookupApplied = true
	r.advance(time.Minute)
	r.checkpointSend(a)
	var chunks []string
	for _, request := range a.requests {
		chunks = append(chunks, request.Text)
	}
	if status, _, _ := r.row(t, "entry-1"); status != "delivered" || a.submits != 3 || a.lookups != 3 || strings.Join(chunks, "") != strings.Repeat("界", 6000) {
		t.Fatalf("reconciliation status=%s submits=%d lookups=%d", status, a.submits, a.lookups)
	}
}

func TestResultCheckpointAmbiguousFailureNeverReplaysPrefix(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, strings.Repeat("x", 16001))
	r.seedCall(t, "s1", "user-1", 1)
	r.code.Store(503)
	r.run(t)
	r.code.Store(200)
	r.advance(time.Minute)
	r.run(t)
	if r.posts.Load() != 3 || r.lookups.Load() != 1 || strings.Join(r.texts, "") != strings.Repeat("x", 16001) {
		t.Fatalf("prefix replay or incomplete continuation: posts=%d lookups=%d", r.posts.Load(), r.lookups.Load())
	}
	if status, _, _ := r.row(t, "entry-1"); status != "delivered" {
		t.Fatal(status)
	}
}

func TestResultCheckpointDestinationIdentityFence(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE voice_sessions SET generation=2`,
		`UPDATE voice_sessions SET gateway_media_generation=43`,
		`UPDATE voice_sessions SET owner_id='other'`,
		`UPDATE voice_sessions SET gateway_session_id='replacement'`,
		`UPDATE tasks SET owner_subject_id='other'`,
		`UPDATE voice_result_deliveries SET run_id='different'`,
		`UPDATE task_entries SET content='changed'`,
	} {
		t.Run(mutation, func(t *testing.T) {
			r := checkpointRig(t)
			r.setup(t, "hello")
			r.seedCall(t, "s1", "user-1", 1)
			r.exec(t, `UPDATE voice_sessions SET gateway_media_generation=42`)
			r.svc.consumeResultEvents(context.Background())
			a := &checkpointAdapter{failAt: 1}
			r.checkpointSend(a)
			r.exec(t, mutation)
			r.advance(time.Minute)
			r.checkpointSend(a)
			if a.submits != 1 || a.lookups != 0 {
				t.Fatalf("destination mismatch reached adapter: %+v", a)
			}
		})
	}
}

func TestResultCheckpointConcurrentClaims(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "hello")
	r.seedCall(t, "s1", "user-1", 1)
	r.svc.consumeResultEvents(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.svc.deliverCheckpointedResult(context.Background(), "entry-1", "default", "task-1", "run-1", "user-1", 0, r.svc.resultReceiptAdapter)
		}()
	}
	wg.Wait()
	if r.posts.Load() != 1 {
		t.Fatalf("posts=%d", r.posts.Load())
	}
}

type expiringCheckpointAdapter struct {
	r     *deliveryRig
	calls int
}

func (a *expiringCheckpointAdapter) Submit(_ context.Context, _ string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	a.calls++
	a.r.advance(time.Minute)
	return ResultChunkReceipt{p.MediaGeneration, p.DeliveryID, p.ContentHash, "applied"}, nil
}
func (a *expiringCheckpointAdapter) Lookup(_ context.Context, _ string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	return ResultChunkReceipt{}, errors.New("unavailable")
}
func TestResultCheckpointExpiredClaimCannotAdvance(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, strings.Repeat("x", 9000))
	r.seedCall(t, "s1", "user-1", 1)
	r.exec(t, `UPDATE voice_sessions SET gateway_media_generation=42`)
	r.svc.consumeResultEvents(context.Background())
	a := &expiringCheckpointAdapter{r: r}
	r.checkpointSend(a)
	if a.calls != 1 {
		t.Fatal(a.calls)
	}
	var state string
	_ = r.store.DB().QueryRow(`SELECT state FROM voice_result_chunks WHERE ordinal=0`).Scan(&state)
	if state != "attempted" {
		t.Fatal(state)
	}
	r.checkpointSend(a)
	if a.calls != 1 {
		t.Fatal("expired attempted chunk resent")
	}
}

func TestResultCheckpointReceiptBinding(t *testing.T) {
	p := ResultChunkRequest{42, "delivery", "hash", "text"}
	for _, r := range []ResultChunkReceipt{{43, "delivery", "hash", "applied"}, {42, "other", "hash", "applied"}, {42, "delivery", "other", "applied"}, {42, "delivery", "hash", "pending"}, {42, "delivery", "hash", "unknown"}} {
		if receiptApplied(p, r) {
			t.Fatalf("invalid receipt accepted: %+v", r)
		}
	}
	if !receiptApplied(p, ResultChunkReceipt{42, "delivery", "hash", "applied"}) {
		t.Fatal("matching receipt rejected")
	}
}

func TestResultCheckpointMissingAuthoritativeEventCannotDeliver(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "original result")
	r.seedCall(t, "s1", "user-1", 1)
	r.svc.consumeResultEvents(context.Background())
	r.exec(t, `DELETE FROM events WHERE event_type='task.assistant_message'`)
	r.run(t)
	if status, _, attempts := r.row(t, "entry-1"); status != "pending" || attempts != 0 || r.posts.Load() != 0 {
		t.Fatalf("unauthoritative source status=%s attempts=%d posts=%d", status, attempts, r.posts.Load())
	}
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1"}
	if err := r.svc.selectPendingResultForTest(context.Background(), p, "entry-1", "s1", 1); err == nil {
		t.Fatal("source without authoritative event selected")
	}
	pending, err := pendingResultsForContext(context.Background(), r.store.DB(), "default", "user-1")
	if err != nil || len(pending) != 0 {
		t.Fatalf("unauthoritative context=%v err=%v", pending, err)
	}
}

func TestResultCheckpointMissingMediaGenerationDoesNotSubmit(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "hello")
	r.seedCall(t, "s1", "user-1", 99)
	r.exec(t, `UPDATE voice_sessions SET gateway_media_generation=0`)
	r.svc.consumeResultEvents(context.Background())
	a := &checkpointAdapter{}
	r.checkpointSend(a)
	status, _, attempts := r.row(t, "entry-1")
	if a.submits != 0 || status != "pending" || attempts != 0 {
		t.Fatalf("missing gateway media generation: submits=%d status=%s attempts=%d", a.submits, status, attempts)
	}
}

func TestResultCheckpointOpeningRejectsNonAuthoritativeEntries(t *testing.T) {
	for _, mutation := range []string{
		`DELETE FROM task_entries WHERE id='entry-1'`,
		`UPDATE task_entries SET kind='user_message'`,
		`UPDATE task_entries SET task_id='other-task'`,
		`UPDATE voice_result_deliveries SET run_id='forged'`,
	} {
		t.Run(mutation, func(t *testing.T) {
			r := checkpointRig(t)
			r.setup(t, "original")
			r.svc.consumeResultEvents(context.Background())
			r.seedCall(t, "s1", "user-1", 1)
			if strings.Contains(mutation, "other-task") {
				r.seedTask(t, "other-task", "user-1")
			}
			r.exec(t, mutation)
			if opening := reserveTestOpening(t, r, "s1"); len(opening.Items) != 0 {
				t.Fatalf("invalid source selected: %+v", opening.Items)
			}
		})
	}
}

func TestResultCheckpointSelectionLiveLeaseRebindRejected(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "original")
	r.svc.consumeResultEvents(context.Background())
	r.seedCall(t, "old", "user-1", 1)
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1"}
	if err := r.svc.selectPendingResultForTest(context.Background(), p, "entry-1", "old", 1); err != nil {
		t.Fatal(err)
	}
	r.exec(t, `UPDATE voice_result_checkpoints SET lease_until=?`, r.now().Add(time.Minute).Format(time.RFC3339Nano))
	r.exec(t, `UPDATE voice_sessions SET state='ended' WHERE id='old'`)
	r.seedCall(t, "next", "user-1", 2)
	if err := r.svc.selectPendingResultForTest(context.Background(), p, "entry-1", "next", 2); err == nil {
		t.Fatal("live lease rebound")
	}
}

func TestResultCheckpointSelectionConfirmedRebindRejected(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "original")
	r.seedCall(t, "old", "user-1", 1)
	r.run(t)
	r.advance(time.Minute)
	r.exec(t, `UPDATE voice_sessions SET state='ended' WHERE id='old'`)
	r.seedCall(t, "next", "user-1", 2)
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1"}
	if err := r.svc.selectPendingResultForTest(context.Background(), p, "entry-1", "next", 2); err == nil {
		t.Fatal("confirmed destination rebound")
	}
	var session string
	if err := r.store.DB().QueryRow(`SELECT session_id FROM voice_result_checkpoints`).Scan(&session); err != nil || session != "old" {
		t.Fatalf("session=%s err=%v", session, err)
	}
}

func TestResultCheckpointPartialProgressSelectionPreserved(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, strings.Repeat("x", 17000))
	r.seedCall(t, "s1", "user-1", 1)
	r.exec(t, `UPDATE voice_sessions SET gateway_media_generation=42`)
	r.svc.consumeResultEvents(context.Background())
	a := &checkpointAdapter{failAt: 2}
	r.checkpointSend(a)
	var before, after string
	query := `SELECT json_group_array(json_object('ordinal',ordinal,'delivery_id',delivery_id,'hash',content_hash,'text',text,'state',state)) FROM (SELECT * FROM voice_result_chunks ORDER BY ordinal)`
	if err := r.store.DB().QueryRow(query).Scan(&before); err != nil {
		t.Fatal(err)
	}
	var claim int64
	var lease string
	if err := r.store.DB().QueryRow(`SELECT claim,lease_until FROM voice_result_checkpoints`).Scan(&claim, &lease); err != nil {
		t.Fatal(err)
	}
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1"}
	for i := 0; i < 2; i++ {
		if err := r.svc.selectPendingResultForTest(context.Background(), p, "entry-1", "s1", 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.store.DB().QueryRow(query).Scan(&after); err != nil {
		t.Fatal(err)
	}
	var afterClaim int64
	var afterLease string
	if err := r.store.DB().QueryRow(`SELECT claim,lease_until FROM voice_result_checkpoints`).Scan(&afterClaim, &afterLease); err != nil {
		t.Fatal(err)
	}
	if before != after || claim != afterClaim || lease != afterLease {
		t.Fatal("same-destination selection mutated partial checkpoint")
	}
}
