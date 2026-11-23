package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/uictl"
)

// uiTargetRig uses the production fresh baseline for UI fixtures.
func uiTargetRig(t *testing.T) *deliveryRig {
	t.Helper()
	r := newDeliveryRig(t)
	uiTargetSchema(t, r.store.DB())
	return r
}

func uiTargetSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	var present int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('ui_surfaces') WHERE name='deleted'`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present != 0 {
		t.Fatal("production UI schema retains removed column")
	}
}

func TestUITargetResolvedRequestAndResult(t *testing.T) {
	r := uiTargetRig(t)
	r.seedTask(t, "task", "owner")
	r.seedEntry(t, "task", "decoy", "unrelated latest assistant message")
	r.exec(t, `UPDATE tasks SET result='authoritative result',status='complete' WHERE id='task'`)
	r.exec(t, `INSERT INTO ui_requests(id,owner_subject_id,source,task_id,request,created_at,updated_at) VALUES('request','owner','task','task','Show the complete resolved request.','now','now')`)
	r.seedEntry(t, "task", "authoritative", "authoritative result")
	r.exec(t, `INSERT INTO runs(id,task_id,attempt_id,status,fence,runner_id,authorized_device_id,command_id,goal,profile_id,runtime_route_id,model_route_id,role,harness,started_at) VALUES('result-run','task','attempt','stopped',1,'runner','device-1','cmd','goal','profile','runtime','model','worker','opencode','now')`)
	r.seedEvent(t, 1, "task", "result-run", "authoritative")
	tx, err := r.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pinUIResultTx(context.Background(), tx, "owner", "task", "request", "authoritative", 1); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	b, err := r.svc.uiInput(context.Background(), uiJob{o: "owner", request: "request", kind: "task_entry", key: "task_entry:decoy"})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"ORIGINAL SECRET", "LEGACY SECRET", "unrelated latest", "utterances", "restatements", "task_message"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("leaked %s: %s", forbidden, b)
		}
	}
	if !strings.Contains(string(b), "authoritative result") || !strings.Contains(string(b), "Show the complete resolved request.") {
		t.Fatal(string(b))
	}
	r.exec(t, `UPDATE task_entries SET content='forged replacement' WHERE id='authoritative'`)
	if _, err = r.svc.uiInput(context.Background(), uiJob{o: "owner", request: "request"}); err == nil {
		t.Fatal("missing result accepted")
	}
}

func uiTargetPresentation(t *testing.T, r *deliveryRig, awaiting bool) {
	t.Helper()
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `INSERT INTO ui_requests(id,owner_subject_id,source,task_id,created_at,updated_at) VALUES('request','owner','task','task',?,?)`, at, at)
	r.exec(t, `INSERT INTO ui_jobs(id,owner_subject_id,kind,dedupe_key,ui_request_id,attempts,next_attempt_at,created_at,updated_at) VALUES('job','owner','task_entry','task_entry:result','request',1,?,?,?)`, at, at, at)
	o, err := uictl.Parse(uiFlowOutput(uiFlowCreate+","+uiFlowRoot, awaiting), nil, uiTestMode(awaiting))
	if err != nil {
		t.Fatal(err)
	}
	facts := &uictl.Task{TaskID: "task", QuestionID: "question", Question: "Which?"}
	if !awaiting {
		facts.QuestionID, facts.Question = "", ""
	}
	if err = r.svc.commitUIJob(context.Background(), uiJob{id: "job", o: "owner", request: "request", kind: "task_entry", attempts: 1}, o, facts); err != nil {
		t.Fatal(err)
	}
}

func TestUITargetRenderACKBarrier(t *testing.T) {
	r := uiTargetRig(t)
	uiFlowServer(t, r)
	r.seedTask(t, "task", "owner")
	r.seedCall(t, "call", "owner", 1)
	r.enableReceiptFixture(t)
	uiTargetPresentation(t, r, false)
	surfaceID := uiStoredID(t, r, "request")
	r.svc.dispatchUIFeedback(context.Background())
	if r.posts.Load() != 0 {
		t.Fatal("feedback before ACK")
	}
	cmd := control.CommandContext{Principal: contracts.Principal{SubjectID: "owner", Scopes: []string{"client:command"}}, AcceptedAt: r.now()}
	for _, payload := range []string{
		`{"request_id":"request","surface_id":"screen","surface_version":2,"success":true}`,
	} {
		tx, _ := r.store.DB().BeginTx(context.Background(), nil)
		_, _, err := r.svc.commandUIRendered(context.Background(), tx, cmd, contracts.CommandRequest{Payload: json.RawMessage(payload)})
		_ = tx.Rollback()
		if err == nil {
			t.Fatal("stale or failed ACK accepted")
		}
	}
	tx, _ := r.store.DB().BeginTx(context.Background(), nil)
	_, _, err := r.svc.commandUIRendered(context.Background(), tx, cmd, contracts.CommandRequest{Payload: json.RawMessage(strings.ReplaceAll(`{"request_id":"request","surface_id":"screen","surface_version":1,"success":true}`, `"screen"`, `"`+surfaceID+`"`))})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// A fresh command cannot acknowledge a consumed document again.
	tx, _ = r.store.DB().BeginTx(context.Background(), nil)
	_, _, err = r.svc.commandUIRendered(context.Background(), tx, cmd, contracts.CommandRequest{Payload: json.RawMessage(strings.ReplaceAll(`{"request_id":"request","surface_id":"screen","surface_version":1,"success":true}`, `"screen"`, `"`+surfaceID+`"`))})
	_ = tx.Rollback()
	if err == nil {
		t.Fatal("duplicate ACK accepted")
	}
	r.svc.dispatchUIFeedback(context.Background())
	if r.posts.Load() != 1 {
		t.Fatal("ACK feedback missing or duplicated")
	}
}

func uiTargetQuestion(t *testing.T, r *deliveryRig) {
	t.Helper()
	r.seedTask(t, "task", "owner")
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `UPDATE tasks SET status='waiting',interaction_state='awaiting_message',interaction_run_id='question-run',interaction_action_digest='digest',interaction_question='Which?' WHERE id='task'`)
	r.exec(t, `INSERT INTO runs(id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at) VALUES('question-run','task','question-attempt','stopped','run',1,1,'runner','device-1','cmd-task','goal','zoe-steward','opencode','runtime','model','worker',?)`, at)
	r.exec(t, `INSERT INTO pending_runtime_actions(id,task_id,run_id,fence,request_id,kind,question,action_digest,expires_at,created_at) VALUES('question','task','question-run',1,'question-request','worker_question','Which?','digest',?,?)`, r.now().Add(time.Hour).Format(time.RFC3339Nano), at)
}

func TestUITargetDirectQuestionInput(t *testing.T) {
	for _, mode := range []string{"answer", "duplicate", "owner", "stale-question", "stale-surface", "stale-client-version"} {
		t.Run(mode, func(t *testing.T) {
			r := uiTargetRig(t)
			uiFlowServer(t, r)
			uiTargetQuestion(t, r)
			uiTargetPresentation(t, r, true)
			cmd := control.CommandContext{Principal: contracts.Principal{SubjectID: "owner", DeviceID: "device-1", Scopes: []string{"client:command"}}, AcceptedAt: r.now()}
			if mode == "owner" {
				cmd.Principal.SubjectID = "other"
			}
			if mode == "stale-question" {
				r.exec(t, `UPDATE tasks SET interaction_action_digest='new-question' WHERE id='task'`)
			}
			if mode == "stale-surface" {
				r.exec(t, `UPDATE ui_surfaces SET version=version+1 WHERE surface_id=?`, uiStoredID(t, r, "request"))
			}
			content := "  full answer\n中文  "
			version := 1
			if mode == "stale-client-version" {
				version = 2
			}
			b, _ := json.Marshal(map[string]any{"request_id": "request", "surface_id": uiStoredID(t, r, "request"), "surface_version": version, "name": "submit", "source_component_id": "submit", "context": map[string]string{"value": content}})
			submit := func() error {
				tx, _ := r.store.DB().BeginTx(context.Background(), nil)
				defer tx.Rollback()
				_, _, err := r.svc.commandUIAction(context.Background(), tx, cmd, contracts.CommandRequest{Payload: b})
				if err != nil {
					return err
				}
				return tx.Commit()
			}
			err := submit()
			wantSuccess := mode == "answer" || mode == "duplicate"
			if (err == nil) != wantSuccess {
				t.Fatalf("err=%v", err)
			}
			if mode == "duplicate" && submit() == nil {
				t.Fatal("duplicate accepted")
			}
			var answers, feedback int
			_ = r.store.DB().QueryRow(`SELECT count(*) FROM task_question_answers`).Scan(&answers)
			_ = r.store.DB().QueryRow(`SELECT count(*) FROM ui_feedback_deliveries`).Scan(&feedback)
			want := 0
			if wantSuccess {
				want = 1
				var retained int
				if err := r.store.DB().QueryRow(`SELECT count(*) FROM ui_surfaces`).Scan(&retained); err != nil || retained != 0 {
					t.Fatalf("answered surface retained=%d err=%v", retained, err)
				}
				var got string
				_ = r.store.DB().QueryRow(`SELECT e.content FROM task_entries e JOIN task_question_answers a ON a.entry_id=e.id WHERE a.question_id='question'`).Scan(&got)
				if got != content {
					t.Fatalf("content=%q", got)
				}
			}
			if answers != want || feedback != 0 {
				t.Fatalf("answers=%d feedback=%d", answers, feedback)
			}
		})
	}
}

func TestUITargetFailureEvidenceSurvivesFeedback(t *testing.T) {
	r := uiTargetRig(t)
	r.seedCall(t, "call", "owner", 1)
	r.enableReceiptFixture(t)
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `INSERT INTO ui_requests(id,owner_subject_id,source,status,created_at,updated_at) VALUES('failure','owner','register','failed',?,?)`, at, at)
	r.exec(t, `INSERT INTO ui_feedback_deliveries(request_id,owner_subject_id,text,created_at,updated_at) VALUES('failure','owner','Failed.',?,?)`, at, at)
	r.svc.dispatchUIFeedback(context.Background())
	var request, delivery string
	_ = r.store.DB().QueryRow(`SELECT r.status,d.status FROM ui_requests r JOIN ui_feedback_deliveries d ON d.request_id=r.id`).Scan(&request, &delivery)
	if request != "failed" || delivery != "delivered" {
		t.Fatalf("%s %s", request, delivery)
	}
}

func TestUITargetFailedACK(t *testing.T) {
	r := uiTargetRig(t)
	uiFlowServer(t, r)
	r.seedTask(t, "task", "owner")
	uiTargetPresentation(t, r, false)
	surfaceID := uiStoredID(t, r, "request")
	cmd := control.CommandContext{Principal: contracts.Principal{SubjectID: "owner", Scopes: []string{"client:command"}}, AcceptedAt: r.now()}
	tx, _ := r.store.DB().BeginTx(context.Background(), nil)
	_, _, err := r.svc.commandUIRendered(context.Background(), tx, cmd, contracts.CommandRequest{Payload: json.RawMessage(strings.ReplaceAll(`{"request_id":"request","surface_id":"screen","surface_version":1,"success":false}`, `"screen"`, `"`+surfaceID+`"`))})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var status string
	var feedback int
	_ = r.store.DB().QueryRow(`SELECT status FROM ui_requests WHERE id='request'`).Scan(&status)
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM ui_feedback_deliveries`).Scan(&feedback)
	if status != "failed" || feedback != 0 {
		t.Fatalf("%s %d", status, feedback)
	}
}

func TestUITargetCommandReceiptReplay(t *testing.T) {
	r := uiTargetRig(t)
	uiFlowServer(t, r)
	uiTargetQuestion(t, r)
	uiTargetPresentation(t, r, true)
	r.exec(t, `UPDATE pending_runtime_actions SET expires_at=? WHERE id='question'`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, _, err := control.InitializeInstanceAuth(context.Background(), r.store, key); err != nil {
		t.Fatal(err)
	}
	// The actual authenticated command endpoint owns the durable receipt boundary.
	if err := r.svc.uiEventServer.RegisterCommand("ui.action", r.svc.commandUIAction); err != nil {
		t.Fatal(err)
	}
	r.exec(t, `UPDATE actors SET subject_id='owner' WHERE id='instance'`)
	server := httptest.NewServer(r.svc.uiEventServer.Handler())
	defer server.Close()
	payload := json.RawMessage(strings.ReplaceAll(`{"request_id":"request","surface_id":"screen","surface_version":1,"name":"submit","source_component_id":"submit","context":{"value":"Bound answer"}}`, `"screen"`, `"`+uiStoredID(t, r, "request")+`"`))
	status, receipt, raw := handoffCommand(t, server.Client(), server.URL, key, "same-command", "ui.action", payload)
	if status != 200 && status != 202 {
		t.Fatalf("%d %s", status, raw)
	}
	status, replay, raw := handoffCommand(t, server.Client(), server.URL, key, "same-command", "ui.action", payload)
	if (status != 200 && status != 202) || replay.CommandID != receipt.CommandID {
		t.Fatalf("replay %d %s", status, raw)
	}
	status, _, raw = handoffCommand(t, server.Client(), server.URL, key, "different-command", "ui.action", payload)
	if status != 409 {
		t.Fatalf("new duplicate %d %s", status, raw)
	}
	var n int
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM task_question_answers WHERE question_id='question'`).Scan(&n)
	if n != 1 {
		t.Fatal(n)
	}
}
