package voice

import (
	"context"
	"strings"
	"testing"
	"time"
)

func seedReportReply(t *testing.T, r *deliveryRig, action string) {
	t.Helper()
	r.exec(t, `INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,steward_action,steward_task_id,handoff_handled) VALUES('report-turn','default','s1',1,'report',1,'status',X'01','now','cmd-task-1',?,'task-1',1)`, action)
	r.exec(t, `INSERT INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at,report_turn_id) VALUES('reply','default','s1',1,'report',?,'task_report','pending',?,'now','now','report-turn')`, strings.Repeat("report ", 1500), r.now().Format(time.RFC3339Nano))
}

func deferredReportRig(t *testing.T, action string) *deliveryRig {
	t.Helper()
	r := checkpointRig(t)
	r.setup(t, "original result")
	r.svc.consumeResultEvents(context.Background())
	r.seedCall(t, "s1", "user-1", 1)
	r.exec(t, `UPDATE voice_result_deliveries SET last_error_code='awaiting_context' WHERE entry_id='entry-1'`)
	seedReportReply(t, r, action)
	return r
}

func TestReportReplyReleaseAfterAllReceipts(t *testing.T) {
	r := deferredReportRig(t, "report")
	r.run(t)
	if r.posts.Load() != 0 {
		t.Fatal("result escaped before reply")
	}
	r.svc.dispatchFastReplies(context.Background())
	if status, code, _ := r.row(t, "entry-1"); status != "pending" || code != "" || r.posts.Load() != 2 {
		t.Fatalf("release status=%s code=%s posts=%d", status, code, r.posts.Load())
	}
	var confirmed int
	if err := r.store.DB().QueryRow(`SELECT count(*) FROM voice_text_chunks WHERE state='confirmed'`).Scan(&confirmed); err != nil || confirmed != 2 {
		t.Fatalf("reply receipts=%d err=%v", confirmed, err)
	}
	r.run(t)
	if status, _, _ := r.row(t, "entry-1"); status != "delivered" || len(r.texts) != 3 || r.texts[2] != "original result" {
		t.Fatalf("status=%s texts=%v", status, r.texts)
	}
}

func TestReportReplyFailureAndCallEndKeepDeferred(t *testing.T) {
	for _, mode := range []string{"failure", "exhausted", "ended", "resumed"} {
		t.Run(mode, func(t *testing.T) {
			r := deferredReportRig(t, "report")
			switch mode {
			case "failure":
				r.code.Store(503)
			case "exhausted":
				r.svc.finishFastReply("reply", "failed", "receipt_rejected")
			case "ended", "resumed":
				r.exec(t, `UPDATE voice_sessions SET state='ended' WHERE id='s1'`)
				if mode == "resumed" {
					r.seedCall(t, "s2", "user-1", 2)
				}
			}
			r.svc.dispatchFastReplies(context.Background())
			if status, code, _ := r.row(t, "entry-1"); status != "pending" || code != "awaiting_context" {
				t.Fatalf("status=%s code=%s", status, code)
			}
			pending, err := pendingResultsForContext(context.Background(), r.store.DB(), "default", "user-1")
			if err != nil || len(pending) != 1 {
				t.Fatalf("next opening candidates=%v err=%v", pending, err)
			}
		})
	}
}

func TestReportReplyReleaseEligibility(t *testing.T) {
	for _, mode := range []string{"redisplay", "reserved", "consumed", "other_task", "other_owner", "source_changed", "live_lease", "unknown", "confirmed", "wrong_revision"} {
		t.Run(mode, func(t *testing.T) {
			r := deferredReportRig(t, "report")
			switch mode {
			case "redisplay":
				r.exec(t, `UPDATE voice_turns SET steward_action='redisplay'`)
			case "reserved", "consumed":
				r.exec(t, `INSERT INTO voice_result_opening(entry_id,session_id,text,state,created_at) VALUES('entry-1','s1','original',?,'now')`, mode)
			case "other_task":
				r.seedTask(t, "other", "user-1")
				r.exec(t, `UPDATE voice_turns SET steward_task_id='other'`)
			case "other_owner":
				r.exec(t, `UPDATE tasks SET owner_subject_id='other'; UPDATE voice_result_deliveries SET subject_id='other'`)
			case "wrong_revision":
				r.exec(t, `UPDATE voice_turns SET steward_action='redisplay'; INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,steward_action,steward_task_id) VALUES('later','default','s1',1,'report',2,'status',X'01','now','cmd-task-1','report','task-1')`)
			default:
				r.exec(t, `INSERT INTO voice_result_checkpoints(entry_id,workspace_id,task_id,run_id,subject_id,content_hash,session_id,generation,gateway_session_id,media_generation,lease_until) VALUES('entry-1','default','task-1','run-1','user-1',?,'old',1,'gw-old',1,?)`, resultHash("original result"), r.now().Add(time.Minute).Format(time.RFC3339Nano))
				if mode == "source_changed" {
					r.exec(t, `UPDATE task_entries SET content='changed'; UPDATE voice_result_checkpoints SET lease_until=''`)
				} else if mode != "live_lease" {
					r.exec(t, `INSERT INTO voice_result_chunks(entry_id,ordinal,delivery_id,content_hash,text,state) VALUES('entry-1',0,'immutable','hash','original',?)`, mode)
				}
			}
			r.svc.dispatchFastReplies(context.Background())
			if status, code, _ := r.row(t, "entry-1"); status != "pending" || code != "awaiting_context" {
				t.Fatalf("status=%s code=%s", status, code)
			}
			var replyStatus string
			if err := r.store.DB().QueryRow(`SELECT status FROM voice_fast_replies WHERE id='reply'`).Scan(&replyStatus); err != nil || replyStatus != "delivered" {
				t.Fatalf("reply=%s err=%v", replyStatus, err)
			}
		})
	}
}

func TestReportReplyWithoutResultLaterDispatchesNormally(t *testing.T) {
	r := checkpointRig(t)
	r.seedTask(t, "task-1", "user-1")
	r.seedCall(t, "s1", "user-1", 1)
	seedReportReply(t, r, "report")
	r.svc.dispatchFastReplies(context.Background())
	r.seedEntry(t, "task-1", "entry-1", "later result")
	r.seedEvent(t, 1, "task-1", "run-1", "entry-1")
	r.run(t)
	if status, _, _ := r.row(t, "entry-1"); status != "delivered" || len(r.texts) != 3 || r.texts[2] != "later result" {
		t.Fatalf("status=%s texts=%v", status, r.texts)
	}
}

func TestReportReplyReleaseScopedAndIdempotent(t *testing.T) {
	r := deferredReportRig(t, "report")
	for i, owner := range []string{"user-1", "other"} {
		task, entry := "another-task", "another-entry"
		if i == 1 {
			task, entry = "other-task", "other-entry"
		}
		r.seedTask(t, task, owner)
		r.seedEntry(t, task, entry, "unrelated result")
		r.seedEvent(t, int64(i+2), task, "another-run", entry)
	}
	r.svc.consumeResultEvents(context.Background())
	r.exec(t, `UPDATE voice_result_deliveries SET last_error_code='awaiting_context'`)
	r.svc.dispatchFastReplies(context.Background())
	for _, entry := range []string{"another-entry", "other-entry"} {
		if status, code, _ := r.row(t, entry); status != "pending" || code != "awaiting_context" {
			t.Fatalf("unrelated %s status=%s code=%s", entry, status, code)
		}
	}
	var before, after string
	query := `SELECT json_object('session',session_id,'claim',claim,'lease',lease_until,'hash',content_hash) FROM voice_result_checkpoints WHERE entry_id='entry-1'`
	if err := r.store.DB().QueryRow(query).Scan(&before); err != nil {
		t.Fatal(err)
	}
	tx, err := r.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	err = r.svc.releaseReportResultsTx(context.Background(), tx, "reply", "default", fastReplyDestination{owner: "user-1", session: "s1", gateway: "gw-s1", generation: 1, media: 1}, r.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := r.store.DB().QueryRow(query).Scan(&after); err != nil || before != after {
		t.Fatalf("checkpoint changed: before=%s after=%s err=%v", before, after, err)
	}
}
