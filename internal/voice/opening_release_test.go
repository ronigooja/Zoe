package voice

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/limits"
)

func TestOversizedOpeningResultDispatchesAfterCallEstablishes(t *testing.T) {
	r := checkpointRig(t)
	original := strings.Repeat("界", r.svc.limits.Int("opening_context_runes")+1)
	r.setup(t, original)
	r.exec(t, `UPDATE tasks SET owner_subject_id='owner'`)
	r.run(t)
	if status, code, attempts := r.row(t, "entry-1"); status != "pending" || code != "awaiting_call" || attempts != 0 {
		t.Fatalf("before opening: %s %s %d", status, code, attempts)
	}
	cfg := callConfig()
	cfg.Now = r.now
	h := newCallHarness(t, r.store, cfg)
	a, _ := h.open(201)
	if len(a.OpeningContext.Items) != 0 {
		t.Fatal("oversized original entered the opening")
	}
	r.run(t)
	if r.posts.Load() != 0 {
		t.Fatal("result dispatched before establishment")
	}
	h.post(callRoute+"/"+a.SessionID+"/established", callEstablishedRequest{testBinding(a), nil}, 200, "")
	r.run(t)
	if status, code, attempts := r.row(t, "entry-1"); status != "delivered" || code != "gateway_accepted" || attempts != 1 {
		t.Fatalf("after establishment: %s %s %d", status, code, attempts)
	}
	if len(r.texts) < 2 || strings.Join(r.texts, "") != original {
		t.Fatal("chunks did not reconstruct the original")
	}
	for _, chunk := range r.texts {
		if len(chunk) > limits.GatewayTextChunkBytes {
			t.Fatalf("chunk is %d bytes", len(chunk))
		}
	}
}

func TestOpeningDeferredResultReceiptRecovery(t *testing.T) {
	for _, mode := range []string{"nonmatching", "matching", "missing"} {
		t.Run(mode, func(t *testing.T) {
			r := checkpointRig(t)
			r.setup(t, "original result")
			r.exec(t, `UPDATE tasks SET owner_subject_id='owner'`)
			r.run(t)
			if status, code, attempts := r.row(t, "entry-1"); status != "pending" || code != "awaiting_call" || attempts != 0 {
				t.Fatalf("before opening: %s %s %d", status, code, attempts)
			}
			cfg := callConfig()
			cfg.Now = r.now
			h := newCallHarness(t, r.store, cfg)
			a, _ := h.open(201)
			if len(a.OpeningContext.Items) != 1 {
				t.Fatalf("deferred result missing from next opening: %+v", a.OpeningContext)
			}
			var reserved int
			if err := r.store.DB().QueryRow(`SELECT count(*) FROM voice_result_opening WHERE entry_id='entry-1' AND session_id=? AND state='reserved'`, a.SessionID).Scan(&reserved); err != nil || reserved != 1 {
				t.Fatalf("reservation=%d err=%v", reserved, err)
			}
			receipt := appliedTestOpening(a.OpeningContext)
			switch mode {
			case "nonmatching":
				receipt.ContentHash = "wrong"
			case "missing":
				receipt = nil
			}
			h.post(callRoute+"/"+a.SessionID+"/established", callEstablishedRequest{testBinding(a), receipt}, 200, "")
			var state, due string
			if err := r.store.DB().QueryRow(`SELECT coalesce((SELECT state FROM voice_result_opening WHERE entry_id='entry-1'),''),next_attempt_at FROM voice_result_deliveries WHERE entry_id='entry-1'`).Scan(&state, &due); err != nil {
				t.Fatal(err)
			}
			_, code, _ := r.row(t, "entry-1")
			want := map[string]string{"nonmatching": "", "matching": "consumed", "missing": "reserved"}[mode]
			if state != want || (mode == "nonmatching" && (code != "" || due != r.now().Format(time.RFC3339Nano))) || (mode != "nonmatching" && code != "awaiting_call") {
				t.Fatalf("state=%q code=%q due=%q", state, code, due)
			}
			for i := 0; i < 2; i++ {
				r.run(t)
				r.advance(time.Minute)
			}
			status, _, attempts := r.row(t, "entry-1")
			if mode == "nonmatching" {
				if status != "delivered" || attempts != 1 || r.posts.Load() != 1 || len(r.paths) != 1 || r.paths[0] != "/v1/voice/sessions/media/+opaque/result-deliveries" {
					t.Fatalf("status=%s attempts=%d posts=%d paths=%v", status, attempts, r.posts.Load(), r.paths)
				}
			} else if status != "pending" || attempts != 0 || r.posts.Load() != 0 {
				t.Fatalf("reserved/consumed result dispatched: %s %d posts=%d", status, attempts, r.posts.Load())
			}
		})
	}
}

func TestOpeningReservationWinsDispatcherReadRace(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "original result")
	r.svc.consumeResultEvents(context.Background())
	r.seedCall(t, "opening", "user-1", 1)
	var opening *openingContext
	hooks := 0
	r.svc.beforeResultClaim = func() {
		hooks++
		opening = reserveTestOpening(t, r, "opening")
	}
	r.run(t)
	r.svc.beforeResultClaim = nil
	if hooks != 1 || opening == nil || len(opening.Items) != 1 {
		t.Fatalf("hooks=%d opening=%+v", hooks, opening)
	}
	var checkpoints, chunks int
	if err := r.store.DB().QueryRow(`SELECT (SELECT count(*) FROM voice_result_checkpoints),(SELECT count(*) FROM voice_result_chunks)`).Scan(&checkpoints, &chunks); err != nil || checkpoints != 0 || chunks != 0 {
		t.Fatalf("claim did not roll back: checkpoints=%d chunks=%d err=%v", checkpoints, chunks, err)
	}
	if _, _, attempts := r.row(t, "entry-1"); attempts != 0 || r.posts.Load() != 0 {
		t.Fatalf("dispatcher won reserved result: attempts=%d posts=%d", attempts, r.posts.Load())
	}
	if err := acknowledgeTestOpening(r, "opening", opening, appliedTestOpening(opening)); err != nil {
		t.Fatal(err)
	}
	r.run(t)
	var consumed, events int
	if err := r.store.DB().QueryRow(`SELECT (SELECT count(*) FROM voice_result_opening WHERE state='consumed'),(SELECT count(*) FROM voice_context_events WHERE source_kind='task_result')`).Scan(&consumed, &events); err != nil || consumed != 1 || events != 1 || r.posts.Load() != 0 {
		t.Fatalf("consumed=%d events=%d posts=%d err=%v", consumed, events, r.posts.Load(), err)
	}
}

func TestOpeningReleaseDeferralFences(t *testing.T) {
	for _, mode := range []string{"unreserved", "consumed", "other-owner", "not-pending", "attempted", "inactive", "delete-failure"} {
		t.Run(mode, func(t *testing.T) {
			r := checkpointRig(t)
			r.setup(t, "original result")
			r.run(t)
			r.seedCall(t, "opening", "user-1", 1)
			opening := reserveTestOpening(t, r, "opening")
			switch mode {
			case "unreserved":
				r.exec(t, `DELETE FROM voice_result_opening`)
			case "consumed":
				r.exec(t, `UPDATE voice_result_opening SET state='consumed'`)
			case "other-owner":
				r.exec(t, `UPDATE voice_result_deliveries SET subject_id='other'`)
			case "not-pending":
				r.exec(t, `UPDATE voice_result_deliveries SET status='delivered'`)
			case "attempted":
				r.exec(t, `INSERT INTO voice_result_checkpoints(entry_id,task_id,run_id,subject_id,content_hash,session_id,generation,gateway_session_id,media_generation) VALUES('entry-1','task-1','run-1','user-1',?,'opening',1,'gw-opening',1)`, resultHash("original result"))
				r.exec(t, `INSERT INTO voice_result_chunks(entry_id,ordinal,delivery_id,content_hash,text,state) VALUES('entry-1',0,'chunk',?,'original result','attempted')`, resultHash("original result"))
			case "inactive":
				r.exec(t, `UPDATE voice_sessions SET state='allocating' WHERE id='opening'`)
			case "delete-failure":
				r.exec(t, `CREATE TRIGGER reject_release BEFORE DELETE ON voice_result_opening BEGIN SELECT RAISE(ABORT,'fixture'); END;`)
			}
			err := acknowledgeTestOpening(r, "opening", opening, &openingContextResult{})
			if (err != nil) != (mode == "delete-failure") {
				t.Fatalf("release error=%v", err)
			}
			if _, code, _ := r.row(t, "entry-1"); code != "awaiting_call" {
				t.Fatalf("deferral crossed %s fence: %q", mode, code)
			}
		})
	}
}

type openingDuringSubmitAdapter struct {
	checkpointAdapter
	beforeSubmit func()
}

func (a *openingDuringSubmitAdapter) Submit(ctx context.Context, gateway string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	a.beforeSubmit()
	return a.checkpointAdapter.Submit(ctx, gateway, p)
}

func TestDispatcherAttemptWinsOpeningSelectionRace(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "original result")
	r.svc.consumeResultEvents(context.Background())
	r.seedCall(t, "active", "user-1", 1)
	a := &openingDuringSubmitAdapter{beforeSubmit: func() {
		var attempts int
		var state string
		if err := r.store.DB().QueryRow(`SELECT d.attempts,c.state FROM voice_result_deliveries d JOIN voice_result_chunks c ON c.entry_id=d.entry_id WHERE d.entry_id='entry-1'`).Scan(&attempts, &state); err != nil || attempts != 1 || state != "attempted" {
			t.Fatalf("uncommitted attempt: attempts=%d state=%s err=%v", attempts, state, err)
		}
		r.exec(t, `UPDATE voice_sessions SET state='ended' WHERE id='active'`)
		r.seedCall(t, "opening", "user-1", 2)
		opening := reserveTestOpening(t, r, "opening")
		if len(opening.Items) != 0 {
			t.Fatalf("attempted result selected: %+v", opening)
		}
	}}
	r.checkpointSend(a)
	if status, _, attempts := r.row(t, "entry-1"); status != "delivered" || attempts != 1 || a.submits != 1 {
		t.Fatalf("status=%s attempts=%d submits=%d", status, attempts, a.submits)
	}
}
