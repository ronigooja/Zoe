package voice

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type textReceiptFixture struct {
	submits, lookups int
	lost             bool
}

func (a *textReceiptFixture) Submit(_ context.Context, _ string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	a.submits++
	if a.lost {
		a.lost = false
		return ResultChunkReceipt{}, errors.New("response lost")
	}
	return ResultChunkReceipt{MediaGeneration: p.MediaGeneration, DeliveryID: p.DeliveryID, ContentHash: p.ContentHash, State: "applied"}, nil
}
func (a *textReceiptFixture) Lookup(_ context.Context, _ string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	a.lookups++
	return ResultChunkReceipt{MediaGeneration: p.MediaGeneration, DeliveryID: p.DeliveryID, ContentHash: p.ContentHash, State: "applied"}, nil
}

func TestDurableTextLookupNamespaceAndMissingCapability(t *testing.T) {
	r := newDeliveryRig(t)
	r.seedCall(t, "text-call", "user-1", 1)
	r.exec(t, `UPDATE voice_sessions SET gateway_media_generation=7 WHERE id='text-call'`)
	ctx := context.Background()
	send := func(kind, id, text string) (bool, string) {
		return r.svc.deliverDurableText(ctx, "user-1", kind, id, "text-call", 1, text)
	}
	if ok, code := send("fast_reply", "same", "full text"); ok || code != "receipt_capability_disabled" {
		t.Fatalf("nil=%v %s", ok, code)
	}
	adapter := &textReceiptFixture{lost: true}
	r.svc.resultReceiptAdapter = adapter
	if ok, code := send("fast_reply", "same", "full text"); ok || code != "receipt_transport_unknown" {
		t.Fatalf("lost=%v %s", ok, code)
	}
	if ok, code := send("fast_reply", "same", "full text"); !ok {
		t.Fatalf("reconcile=%s", code)
	}
	if ok, _ := send("fast_reply", "same", "full text"); !ok {
		t.Fatal("repeat")
	}
	if adapter.submits != 1 || adapter.lookups != 1 {
		t.Fatalf("duplicate submits=%d lookup=%d", adapter.submits, adapter.lookups)
	}
	if ok, code := send("ui_feedback", "same", "different namespace"); !ok {
		t.Fatalf("namespace=%s", code)
	}
	if adapter.submits != 2 {
		t.Fatal("source kind collision")
	}
	if ok, _ := send("fast_reply", "same", "mutated content"); ok {
		t.Fatal("source changed")
	}
	r.exec(t, `UPDATE voice_sessions SET state='ended' WHERE id='text-call'`)
	if ok, code := send("fast_reply", "no-call", "pending text"); ok || code != "awaiting_context" {
		t.Fatalf("no call=%v %s", ok, code)
	}
}

type textAdapterFuncs struct {
	submit func(ResultChunkRequest) (ResultChunkReceipt, error)
	lookup func(ResultChunkRequest) (ResultChunkReceipt, error)
}

func (a textAdapterFuncs) Submit(_ context.Context, _ string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	return a.submit(p)
}
func (a textAdapterFuncs) Lookup(_ context.Context, _ string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	return a.lookup(p)
}
func textApplied(p ResultChunkRequest) (ResultChunkReceipt, error) {
	return ResultChunkReceipt{MediaGeneration: p.MediaGeneration, DeliveryID: p.DeliveryID, ContentHash: p.ContentHash, State: "applied"}, nil
}
func textTestRig(t *testing.T) (*deliveryRig, func(string) (bool, string)) {
	t.Helper()
	r := newDeliveryRig(t)
	r.seedCall(t, "text-call", "user-1", 1)
	r.exec(t, `UPDATE voice_sessions SET gateway_media_generation=7 WHERE id='text-call'`)
	return r, func(text string) (bool, string) {
		return r.svc.deliverDurableText(context.Background(), "user-1", "fast_reply", "source", "text-call", 1, text)
	}
}

func TestDurableTextConcurrentClaimExpiryBlockingSubmit(t *testing.T) {
	r, send := textTestRig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var submits, lookups atomic.Int32
	r.svc.resultReceiptAdapter = textAdapterFuncs{
		submit: func(p ResultChunkRequest) (ResultChunkReceipt, error) {
			submits.Add(1)
			close(entered)
			<-release
			return textApplied(p)
		},
		lookup: func(p ResultChunkRequest) (ResultChunkReceipt, error) {
			lookups.Add(1)
			return ResultChunkReceipt{}, errors.New("temporarily unavailable")
		},
	}
	done := make(chan string, 1)
	go func() { _, code := send("hello"); done <- code }()
	<-entered
	if ok, code := send("hello"); ok || code != "checkpoint_identity_or_busy" {
		t.Fatalf("concurrent=%v %s", ok, code)
	}
	r.advance(2 * time.Minute)
	if ok, code := send("hello"); ok || code != "receipt_lookup_unavailable" {
		t.Fatalf("takeover=%v %s", ok, code)
	}
	close(release)
	if code := <-done; code != "checkpoint_lease_lost" {
		t.Fatal(code)
	}
	var state string
	if err := r.store.DB().QueryRow(`SELECT state FROM voice_text_chunks`).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("stale confirmation state=%s err=%v", state, err)
	}
	if submits.Load() != 1 || lookups.Load() != 1 {
		t.Fatalf("submit=%d lookup=%d", submits.Load(), lookups.Load())
	}
}

func TestDurableTextDestinationChangesStopChunks(t *testing.T) {
	for _, change := range []string{"state='ended'", "generation=2", "owner_id='other'", "gateway_session_id='replacement'", "gateway_media_generation=8"} {
		t.Run(change, func(t *testing.T) {
			r, send := textTestRig(t)
			calls := 0
			r.svc.resultReceiptAdapter = textAdapterFuncs{submit: func(p ResultChunkRequest) (ResultChunkReceipt, error) {
				calls++
				r.exec(t, `UPDATE voice_sessions SET `+change+` WHERE id='text-call'`)
				return textApplied(p)
			}}
			if ok, code := send(strings.Repeat("x", 16001)); ok || code != "checkpoint_lease_lost" || calls != 1 {
				t.Fatalf("ok=%v code=%s submits=%d", ok, code, calls)
			}
		})
	}
}

func TestDurableTextExpiredWorkerCannotConfirmOrSendNext(t *testing.T) {
	r, send := textTestRig(t)
	submits := 0
	r.svc.resultReceiptAdapter = textAdapterFuncs{submit: func(p ResultChunkRequest) (ResultChunkReceipt, error) {
		submits++
		r.advance(2 * time.Minute)
		return textApplied(p)
	}, lookup: textApplied}
	text := strings.Repeat("x", 8001)
	if ok, code := send(text); ok || code != "checkpoint_lease_lost" {
		t.Fatalf("%v %s", ok, code)
	}
	var state string
	if err := r.store.DB().QueryRow(`SELECT state FROM voice_text_chunks WHERE ordinal=0`).Scan(&state); err != nil || state != "attempted" {
		t.Fatalf("expired confirmation state=%s err=%v", state, err)
	}
	if submits != 1 {
		t.Fatalf("submits=%d", submits)
	}
}

func TestFastReplyOwnerMismatchDoesNotSpendRetries(t *testing.T) {
	r, send := textTestRig(t)
	r.svc.resultReceiptAdapter = &textReceiptFixture{}
	if ok, code := send("hello"); !ok {
		t.Fatal(code)
	}
	r.exec(t, `UPDATE voice_sessions SET owner_id='replacement' WHERE id='text-call'`)
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `INSERT INTO voice_fast_replies(id,session_id,generation,turn_id,text,status,next_attempt_at,created_at,updated_at) VALUES('source','text-call',1,'turn','hello','pending',?,?,?)`, at, at, at)
	r.svc.deliverFastReply(context.Background(), "source", "text-call", 1, 0)
	var attempts int
	var status, code string
	if err := r.store.DB().QueryRow(`SELECT attempts,status,last_error_code FROM voice_fast_replies WHERE id='source'`).Scan(&attempts, &status, &code); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || status != "pending" || code != "awaiting_context" {
		t.Fatalf("%d %s %s", attempts, status, code)
	}
}

func TestDurableTextConfirmedPrefixLookupOnly(t *testing.T) {
	r, send := textTestRig(t)
	submits, lookups := 0, 0
	r.svc.resultReceiptAdapter = textAdapterFuncs{submit: func(p ResultChunkRequest) (ResultChunkReceipt, error) {
		submits++
		if submits == 2 {
			return ResultChunkReceipt{}, errors.New("lost")
		}
		return textApplied(p)
	}, lookup: func(p ResultChunkRequest) (ResultChunkReceipt, error) { lookups++; return textApplied(p) }}
	text := strings.Repeat("x", 16001)
	if ok, code := send(text); ok || code != "receipt_transport_unknown" {
		t.Fatalf("%v %s", ok, code)
	}
	if ok, code := send(text); !ok {
		t.Fatal(code)
	}
	if submits != 3 || lookups != 1 {
		t.Fatalf("submit=%d lookup=%d", submits, lookups)
	}
}

type textRejected struct{}

func (textRejected) Error() string             { return "known rejection before write" }
func (textRejected) RejectedBeforeWrite() bool { return true }

func TestDurableTextReceiptStages(t *testing.T) {
	for _, stage := range []string{"rejected", "invalid", "transport"} {
		t.Run(stage, func(t *testing.T) {
			r, send := textTestRig(t)
			// Prerequisite proposal: production voice_text_chunks CHECK must allow
			// rejected in addition to pending/attempted/confirmed/unknown.
			if stage == "rejected" {
				r.exec(t, `DROP TABLE voice_text_chunks`)
				_, chunks, _ := strings.Cut(TextCheckpointSchema, "CREATE TABLE voice_text_chunks")
				r.exec(t, "CREATE TABLE voice_text_chunks"+strings.Replace(chunks, "'confirmed','unknown'", "'confirmed','unknown','rejected'", 1))
			}
			submits := 0
			r.svc.resultReceiptAdapter = textAdapterFuncs{submit: func(p ResultChunkRequest) (ResultChunkReceipt, error) {
				submits++
				if stage == "rejected" {
					return ResultChunkReceipt{}, textRejected{}
				}
				if stage == "transport" {
					return ResultChunkReceipt{}, errors.New("lost")
				}
				return ResultChunkReceipt{State: "applied", DeliveryID: "wrong"}, nil
			}, lookup: func(p ResultChunkRequest) (ResultChunkReceipt, error) {
				return ResultChunkReceipt{}, errors.New("unavailable")
			}}
			want := map[string]string{"rejected": "receipt_rejected", "invalid": "receipt_invalid", "transport": "receipt_transport_unknown"}[stage]
			if ok, code := send("hello"); ok || code != want {
				t.Fatalf("%v %s", ok, code)
			}
			var state string
			if err := r.store.DB().QueryRow(`SELECT state FROM voice_text_chunks`).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if stage == "rejected" && state != "rejected" || stage != "rejected" && state != "unknown" {
				t.Fatal(state)
			}
			want = "receipt_lookup_unavailable"
			if stage == "rejected" {
				want = "receipt_rejected"
			}
			if ok, code := send("hello"); ok || code != want {
				t.Fatalf("retry=%v %s", ok, code)
			}
			if submits != 1 {
				t.Fatalf("resubmitted %d", submits)
			}
		})
	}
}

func TestFastReplyGenerationMismatchDoesNotSpendRetries(t *testing.T) {
	r, _ := textTestRig(t)
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `INSERT INTO voice_fast_replies(id,session_id,generation,turn_id,text,status,next_attempt_at,created_at,updated_at) VALUES('reply','text-call',99,'turn','hello','pending',?,?,?)`, at, at, at)
	r.svc.deliverFastReply(context.Background(), "reply", "text-call", 99, 0)
	var attempts int
	var status, code string
	if err := r.store.DB().QueryRow(`SELECT attempts,status,last_error_code FROM voice_fast_replies WHERE id='reply'`).Scan(&attempts, &status, &code); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || status != "pending" || code != "awaiting_context" {
		t.Fatalf("%d %s %s", attempts, status, code)
	}
}

func TestFastReplyRebindReplaysWholeAnswerOnce(t *testing.T) {
	r := checkpointRig(t)
	ctx := context.Background()
	adapter := &checkpointAdapter{failAt: 2}
	r.svc.resultReceiptAdapter = adapter
	r.seedCall(t, "origin", "user-1", 1)
	text := strings.Repeat("界", 3000)
	at := r.now().Format(time.RFC3339Nano)
	for _, item := range []struct{ id, kind, text string }{
		{"answer", "recall_answer", text},
		{"holding", "recall_holding", "not an answer"},
		{"empty", "recall_answer", " \t\n"},
	} {
		r.exec(t, `INSERT INTO voice_fast_replies(id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) VALUES(?,'origin',1,?,?,?,'pending',?,?,?)`, item.id, item.id, item.text, item.kind, at, at, at)
	}
	r.svc.dispatchFastReplies(ctx)
	if adapter.submits != 2 || adapter.lookups != 0 {
		t.Fatalf("origin submits=%d lookups=%d", adapter.submits, adapter.lookups)
	}
	oldFirstID := adapter.requests[0].DeliveryID
	r.exec(t, `UPDATE voice_sessions SET state='ended' WHERE id='origin'`)
	r.seedCall(t, "other", "other-owner", 2)
	r.svc.dispatchFastReplies(ctx)
	if adapter.submits != 2 {
		t.Fatal("answer crossed owners")
	}
	r.seedCall(t, "next", "user-1", 3)
	r.exec(t, `UPDATE voice_sessions SET gateway_media_generation=42 WHERE id='next'`)
	r.svc.dispatchFastReplies(ctx)
	r.advance(31 * time.Second)
	r.svc.dispatchFastReplies(ctx)
	if adapter.submits != 4 || adapter.lookups != 0 || adapter.requests[2].Text+adapter.requests[3].Text != text || adapter.requests[2].DeliveryID == oldFirstID || adapter.requests[2].MediaGeneration != 42 {
		t.Fatalf("expected full send under new identity; submits=%d lookups=%d", adapter.submits, adapter.lookups)
	}
	var n int
	for _, query := range []string{
		`SELECT count(*) FROM voice_fast_replies WHERE id='answer' AND session_id='origin' AND generation=1 AND voice_session_id='next' AND voice_generation=3 AND status='delivered'`,
		`SELECT count(*) FROM voice_context_events WHERE source_kind='fast_reply' AND source_id='answer' AND session_id='next' AND owner_id='user-1'`,
		`SELECT count(*) FROM voice_text_checkpoints WHERE source_kind='fast_reply' AND source_id='answer' AND session_id='next' AND generation=3 AND gateway_session_id='gw-next' AND media_generation=42`,
		`SELECT count(*) FROM voice_text_checkpoints WHERE source_kind='fast_reply'`,
		`SELECT count(*) FROM voice_context_events WHERE source_kind='fast_reply'`,
	} {
		if err := r.store.DB().QueryRow(query).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s: %d, %v", query, n, err)
		}
	}
}
