package voice

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestContextEventRecorder(t *testing.T) {
	f := newRegisterFixture(t)
	ctx := context.Background()
	e := contextEvent{workspace: "default", owner: "owner", session: "handoff-session", kind: "task_result", id: "entry", role: "background", text: "full text", at: f.now}
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = recordContextEvent(ctx, tx, e); err != nil {
		t.Fatal(err)
	}
	if err = recordContextEvent(ctx, tx, e); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"session", "text", "role", "kind"} {
		bad := e
		switch field {
		case "session":
			bad.session = "other"
		case "text":
			bad.text = "changed"
		case "role":
			bad.role = "user"
		case "kind":
			bad.kind = "unknown"
		}
		if recordContextEvent(ctx, tx, bad) == nil {
			t.Fatalf("accepted conflicting %s", field)
		}
	}
	for _, kind := range []string{"task_result", "ui_feedback", "fast_reply", "user_text", "ui_input_request", "ui_input_received"} {
		good := e
		good.kind = kind
		good.id = kind
		good.role = contextEventRole(kind)
		if err = recordContextEvent(ctx, tx, good); err != nil {
			t.Fatal(err)
		}
		good.role = map[string]string{"user": "background", "background": "user"}[good.role]
		if recordContextEvent(ctx, tx, good) == nil {
			t.Fatal("accepted invalid pairing")
		}
	}
	var count int
	if err = tx.QueryRow(`SELECT count(*) FROM voice_context_events WHERE source_id='entry'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO voice_context_events VALUES('default','owner','s','bad','x','user','text','at','at')`,
		`INSERT INTO voice_context_events VALUES('default','owner','s','user_text','x','assistant','text','at','at')`,
	} {
		if _, err = f.db.Exec(query); err == nil {
			t.Fatal("schema accepted invalid event")
		}
	}
}

func (f *registerFixture) event(kind, id, text string, at time.Time) {
	f.t.Helper()
	f.exec(`INSERT INTO voice_context_events VALUES('default','owner','handoff-session',?,?,?,?,?,?)`, kind, id, contextEventRole(kind), text, at.UTC().Format(time.RFC3339Nano), f.now.UTC().Format(time.RFC3339Nano))
}

func TestRegisterEventMergeCoverageWatermarkFingerprintAndFence(t *testing.T) {
	f := newRegisterFixture(t)
	f.exec(`CREATE TABLE IF NOT EXISTS memory_ingest_members(workspace_id TEXT,principal_id TEXT,source_id TEXT,batch_id TEXT); CREATE TABLE IF NOT EXISTS memory_ingest_batches(id TEXT,workspace_id TEXT,principal_id TEXT,until_ns INTEGER)`)
	at := f.now.Add(-2 * time.Hour)
	f.turn("handoff-session", 1, "user", "turn", at)
	f.event("task_result", "a", "same text", at)
	raw := f.layers(f.now).Raw
	if strings.Index(raw, "background: same text") > strings.Index(raw, "user: turn") {
		t.Fatal("noncanonical equal-time order")
	}
	if !f.rebuild(f.now) {
		t.Fatal("no initial build")
	}
	if strings.Contains(f.layers(f.now).Raw, "same text") {
		t.Fatal("compacted event stayed raw")
	}
	// Same timestamp and same completion clock as the published compact.
	f.event("task_result", "b", "same text", at)
	if !strings.Contains(f.layers(f.now).Raw, "background: same text") {
		t.Fatal("late event lost at equal build time")
	}
	if !f.rebuild(f.now) {
		t.Fatal("new identity did not change fingerprint")
	}
	f.event("task_result", "c", "fence input", at)
	f.onCall = func() { f.event("task_result", "d", "arrived during build", at) }
	if f.rebuild(f.now) {
		t.Fatal("event arrival passed publication fence")
	}
	f.onCall = nil
	if !strings.Contains(f.layers(f.now).Raw, "arrived during build") {
		t.Fatal("fenced event lost")
	}
	f.boundary = f.now.Add(-time.Hour)
	s, err := f.r.snapshot(context.Background(), f.owner, f.now)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, turn := range s.turns {
		if turn.key == `event:"task_result":"d"` {
			found = turn.eligible
		}
	}
	if !found {
		t.Fatal("unconsumed event before M excluded")
	}
}

func TestVoiceInputEventsAtomicUnwrappedAndIdempotent(t *testing.T) {
	for _, kind := range []string{"user_text", "ui_input_request", "ui_input_received"} {
		t.Run(kind, func(t *testing.T) {
			r := newReceiptDeliveryRig(t)
			r.seedCall(t, "call", "owner", 1)
			at := r.now().Format(time.RFC3339Nano)
			if kind == "user_text" {
				r.exec(t, `INSERT INTO voice_text_inputs(command_id,workspace_id,owner_subject_id,text,next_attempt_at,created_at,updated_at) VALUES('source','default','owner','original',?,?,?)`, at, at, at)
			} else {
				r.exec(t, `INSERT INTO voice_input_notices(kind,source_id,workspace_id,owner_subject_id,text,next_attempt_at,created_at,updated_at) VALUES(?,'source','default','owner','original',?,?,?)`, kind, at, at, at)
			}
			r.exec(t, `CREATE TRIGGER reject_event BEFORE INSERT ON voice_context_events BEGIN SELECT RAISE(ABORT,'fixture'); END`)
			r.svc.dispatchVoiceInputs(context.Background())
			var count int
			if err := r.store.DB().QueryRow(`SELECT count(*) FROM voice_context_events`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("count=%d err=%v", count, err)
			}
			table := "voice_input_notices"
			if kind == "user_text" {
				table = "voice_text_inputs"
			}
			var status string
			if err := r.store.DB().QueryRow(`SELECT status FROM ` + table).Scan(&status); err != nil || status != "pending" {
				t.Fatalf("rollback status=%s err=%v", status, err)
			}
			r.exec(t, `DROP TRIGGER reject_event`)
			r.advance(31 * time.Second)
			r.svc.dispatchVoiceInputs(context.Background())
			r.svc.dispatchVoiceInputs(context.Background())
			var text, role, received, eventAt string
			if err := r.store.DB().QueryRow(`SELECT count(*),text,role,at,received_at FROM voice_context_events`).Scan(&count, &text, &role, &eventAt, &received); err != nil || count != 1 || text != "original" || role != contextEventRole(kind) || eventAt != received || eventAt != r.now().Format(time.RFC3339Nano) {
				t.Fatalf("event=%d/%s/%s/%s/%s err=%v", count, text, role, eventAt, received, err)
			}
		})
	}
}

func assertRecordedEvent(t *testing.T, r *deliveryRig, kind, text string, count int) {
	t.Helper()
	var got int
	if err := r.store.DB().QueryRow(`SELECT count(*) FROM voice_context_events WHERE source_kind=? AND text=?`, kind, text).Scan(&got); err != nil || got != count {
		t.Fatalf("%s events=%d want=%d err=%v", kind, got, count, err)
	}
}

func TestTaskResultEventRollbackAndPartialConfirmation(t *testing.T) {
	r := checkpointRig(t)
	text := strings.Repeat("x", 17000)
	r.setup(t, text)
	r.seedCall(t, "s1", "user-1", 1)
	r.svc.consumeResultEvents(context.Background())
	a := &checkpointAdapter{failAt: 2}
	r.checkpointSend(a)
	assertRecordedEvent(t, r, "task_result", text, 0)
	r.advance(31 * time.Second)
	a.lookupApplied = true
	r.exec(t, `CREATE TRIGGER reject_event BEFORE INSERT ON voice_context_events BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	r.checkpointSend(a)
	status, _, _ := r.rowForCheckpoint()
	if status != "pending" {
		t.Fatal("event failure did not roll back delivered result")
	}
	assertRecordedEvent(t, r, "task_result", text, 0)
	r.exec(t, `DROP TRIGGER reject_event`)
	r.advance(31 * time.Second)
	r.checkpointSend(a)
	r.checkpointSend(a)
	assertRecordedEvent(t, r, "task_result", text, 1)
}

func TestFastReplyEventsBothProducersRollbackAndRetry(t *testing.T) {
	for _, intent := range []string{"task_report", "recall_answer"} {
		t.Run(intent, func(t *testing.T) {
			r := newReceiptDeliveryRig(t)
			r.seedCall(t, "s1", "user-1", 1)
			at := r.now().Format(time.RFC3339Nano)
			text := strings.Repeat("answer", 3000)
			r.exec(t, `INSERT INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,status,next_attempt_at,created_at,updated_at) VALUES('reply','default','s1',1,'turn',?,'pending',?,?,?)`, text, at, at, at)
			r.exec(t, `CREATE TRIGGER reject_event BEFORE INSERT ON voice_context_events BEGIN SELECT RAISE(ABORT,'fixture'); END`)
			r.svc.deliverFastReply(context.Background(), "reply", "default", "s1", 1, text, intent, 0)
			var status string
			if err := r.store.DB().QueryRow(`SELECT status FROM voice_fast_replies`).Scan(&status); err != nil || status != "pending" {
				t.Fatalf("rollback status=%s err=%v", status, err)
			}
			r.exec(t, `DROP TRIGGER reject_event`)
			r.advance(31 * time.Second)
			r.svc.deliverFastReply(context.Background(), "reply", "default", "s1", 1, text, intent, 0)
			r.svc.deliverFastReply(context.Background(), "reply", "default", "s1", 1, text, intent, 0)
			assertRecordedEvent(t, r, "fast_reply", text, 1)
		})
	}
}

func TestUIFeedbackEventRollbackAndRetry(t *testing.T) {
	r := uiTargetRig(t)
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,status,created_at,updated_at) VALUES('request','default','owner','register','failed',?,?)`, at, at)
	text := strings.Repeat("feedback", 3000)
	r.exec(t, `INSERT INTO ui_feedback_deliveries(request_id,workspace_id,owner_subject_id,text,created_at,updated_at) VALUES('request','default','owner',?,?,?)`, text, at, at)
	r.seedCall(t, "call", "owner", 1)
	r.enableReceiptFixture(t)
	r.exec(t, `CREATE TRIGGER reject_event BEFORE INSERT ON voice_context_events BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	r.svc.dispatchUIFeedback(context.Background())
	var status string
	if err := r.store.DB().QueryRow(`SELECT status FROM ui_feedback_deliveries`).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("rollback status=%s err=%v", status, err)
	}
	assertRecordedEvent(t, r, "ui_feedback", text, 0)
	r.exec(t, `DROP TRIGGER reject_event`)
	r.advance(31 * time.Second)
	r.svc.dispatchUIFeedback(context.Background())
	r.svc.dispatchUIFeedback(context.Background())
	assertRecordedEvent(t, r, "ui_feedback", text, 1)
}

func TestGenericContextEventsWaitForAllChunks(t *testing.T) {
	for _, kind := range []string{"fast_reply", "ui_feedback", "user_text", "ui_input_request", "ui_input_received"} {
		t.Run(kind, func(t *testing.T) {
			r := uiTargetRig(t)
			r.seedCall(t, "call", "owner", 1)
			a := &checkpointAdapter{failAt: 2}
			r.svc.resultReceiptAdapter = a
			at := r.now().Format(time.RFC3339Nano)
			text := strings.Repeat("x", 17000)
			var dispatch func()
			switch kind {
			case "fast_reply":
				r.exec(t, `INSERT INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,status,next_attempt_at,created_at,updated_at) VALUES('source','default','call',1,'turn',?,'pending',?,?,?)`, text, at, at, at)
				dispatch = func() { r.svc.dispatchFastReplies(context.Background()) }
			case "ui_feedback":
				r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,status,created_at,updated_at) VALUES('source','default','owner','register','failed',?,?)`, at, at)
				r.exec(t, `INSERT INTO ui_feedback_deliveries(request_id,workspace_id,owner_subject_id,text,created_at,updated_at) VALUES('source','default','owner',?,?,?)`, text, at, at)
				dispatch = func() { r.svc.dispatchUIFeedback(context.Background()) }
			case "user_text":
				r.exec(t, `INSERT INTO voice_text_inputs(command_id,workspace_id,owner_subject_id,text,next_attempt_at,created_at,updated_at) VALUES('source','default','owner',?,?,?,?)`, text, at, at, at)
				dispatch = func() { r.svc.dispatchVoiceInputs(context.Background()) }
			default:
				r.exec(t, `INSERT INTO voice_input_notices(kind,source_id,workspace_id,owner_subject_id,text,next_attempt_at,created_at,updated_at) VALUES(?,'source','default','owner',?,?,?,?)`, kind, text, at, at, at)
				dispatch = func() { r.svc.dispatchVoiceInputs(context.Background()) }
			}
			dispatch()
			if a.submits != 2 {
				t.Fatalf("submits=%d", a.submits)
			}
			assertRecordedEvent(t, r, kind, text, 0)
			r.advance(31 * time.Second)
			dispatch()
			assertRecordedEvent(t, r, kind, text, 0)
			a.lookupApplied = true
			r.advance(31 * time.Second)
			dispatch()
			dispatch()
			assertRecordedEvent(t, r, kind, text, 1)
		})
	}
}
