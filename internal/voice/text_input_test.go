package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/uictl"
	"strings"
	"testing"
	"time"
)

func TestTextInputValidationAndDedupe(t *testing.T) {
	r := uiTargetRig(t)
	ctx := context.Background()
	cmd := control.CommandContext{CommandID: "text-1", AcceptedAt: r.now(), Principal: contracts.Principal{WorkspaceID: "default", SubjectID: "owner", Scopes: []string{"client:command"}}}
	run := func(c control.CommandContext, payload []byte) error {
		tx, err := r.store.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = r.svc.commandVoiceTextInput(ctx, tx, c, contracts.CommandRequest{Payload: payload})
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	for _, p := range [][]byte{[]byte(`{"text":" "}`), []byte(`{"text":"` + strings.Repeat("a", 20001) + `"}`), []byte{'{', '"', 't', 'e', 'x', 't', '"', ':', '"', 0xff, '"', '}'}, []byte(`{"text":"ok","extra":1}`)} {
		if run(cmd, p) == nil {
			t.Fatal("invalid input accepted")
		}
	}
	wrong := cmd
	wrong.Principal.Scopes = nil
	if run(wrong, []byte(`{"text":"ok"}`)) == nil {
		t.Fatal("scope accepted")
	}
	if err := run(cmd, []byte(`{"text":"first"}`)); err != nil {
		t.Fatal(err)
	}
	if err := run(cmd, []byte(`{"text":"second"}`)); err != nil {
		t.Fatal(err)
	}
	var text string
	var count int
	if err := r.store.DB().QueryRow(`SELECT count(*),text FROM voice_text_inputs`).Scan(&count, &text); err != nil || count != 1 || text != "first" {
		t.Fatalf("count=%d text=%q err=%v", count, text, err)
	}
	r.svc.dispatchVoiceInputs(ctx)
	var status string
	var attempts int
	if err := r.store.DB().QueryRow(`SELECT status,attempts FROM voice_text_inputs`).Scan(&status, &attempts); err != nil || status != "pending" || attempts != 0 {
		t.Fatalf("status=%s attempts=%d err=%v", status, attempts, err)
	}
}
func TestVoiceInputActionDoesNotCreateTaskEntry(t *testing.T) {
	r := uiTargetRig(t)
	ctx := context.Background()
	at := r.now().Format("2006-01-02T15:04:05.999999999Z07:00")
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,surface_id,surface_version,voice_input,input_note,created_at,updated_at) VALUES('vr','default','owner','register','screen',1,1,'Type',?,?)`, at, at)
	r.exec(t, `INSERT INTO ui_surfaces(workspace_id,owner_subject_id,surface_id,container,state_json,version,request_id,created_at,updated_at) VALUES('default','owner','screen','window','[]',1,'vr',?,?)`, at, at)
	cmd := control.CommandContext{CommandID: "answer", AcceptedAt: r.now(), Principal: contracts.Principal{WorkspaceID: "default", SubjectID: "owner", Scopes: []string{"client:command"}}}
	payload, _ := json.Marshal(map[string]any{"request_id": "vr", "surface_id": "screen", "surface_version": 1, "name": "submit", "source_component_id": "button", "context": map[string]string{"value": "hello"}})
	tx, _ := r.store.DB().BeginTx(ctx, nil)
	_, _, err := r.svc.commandUIAction(ctx, tx, cmd, contracts.CommandRequest{Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var text string
	var count int
	_ = r.store.DB().QueryRow(`SELECT text FROM voice_text_inputs WHERE command_id='answer'`).Scan(&text)
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM task_entries`).Scan(&count)
	if text != "hello" || count != 0 {
		t.Fatalf("text=%q entries=%d", text, count)
	}
	var tasks, notices int
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks)
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM voice_input_notices`).Scan(&notices)
	if tasks != 0 || notices != 0 {
		t.Fatalf("tasks=%d notices=%d", tasks, notices)
	}
	tx, _ = r.store.DB().BeginTx(ctx, nil)
	_, _, err = r.svc.commandUIAction(ctx, tx, cmd, contracts.CommandRequest{Payload: payload})
	_ = tx.Rollback()
	if err == nil {
		t.Fatal("duplicate action accepted")
	}
}

func TestVoiceInputDeliveryFormatsAndChunks(t *testing.T) {
	r := newReceiptDeliveryRig(t)
	ctx := context.Background()
	at := r.now().Format(time.RFC3339Nano)
	long := strings.Repeat("界", 6000)
	r.exec(t, `INSERT INTO voice_text_inputs(command_id,workspace_id,owner_subject_id,text,next_attempt_at,created_at,updated_at) VALUES('text','default','owner',?,?,?,?)`, long, at, at, at)
	for _, d := range []struct{ kind, id, text string }{{"ui_input_request", "q", "Question?"}, {"ui_input_received", "answer", "Answer!"}} {
		r.exec(t, `INSERT INTO voice_input_notices(kind,source_id,workspace_id,owner_subject_id,text,next_attempt_at,created_at,updated_at) VALUES(?,?,'default','owner',?,?,?,?)`, d.kind, d.id, d.text, at, at, at)
	}
	r.svc.dispatchVoiceInputs(ctx)
	var attempts int
	_ = r.store.DB().QueryRow(`SELECT attempts FROM voice_text_inputs`).Scan(&attempts)
	if attempts != 0 {
		t.Fatalf("no-call attempts=%d", attempts)
	}
	r.seedCall(t, "call", "owner", 1)
	r.advance(31 * time.Second)
	r.svc.dispatchVoiceInputs(ctx)
	rows, err := r.store.DB().Query(`SELECT source_kind,ordinal,text FROM voice_text_chunks ORDER BY source_kind,ordinal`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	texts := map[string]string{}
	chunks := 0
	for rows.Next() {
		var kind, text string
		var ordinal int
		if err := rows.Scan(&kind, &ordinal, &text); err != nil {
			t.Fatal(err)
		}
		if len(text) > 8000 {
			t.Fatalf("chunk length=%d", len(text))
		}
		texts[kind] += text
		if kind == "user_text" {
			chunks++
		}
	}
	if texts["user_text"] != "[User text input] "+long || texts["ui_input_request"] != "The background tool has asked the user for input. Request: Question?" || texts["ui_input_received"] != "The background tool received the user's input. Content: Answer!" || chunks < 2 {
		t.Fatalf("incorrect formats or chunks: %d", chunks)
	}
}

func TestQuestionNoticesFollowRenderAndAnswer(t *testing.T) {
	r := uiTargetRig(t)
	uiFlowServer(t, r)
	uiTargetQuestion(t, r)
	uiTargetPresentation(t, r, true)
	surfaceID := uiStoredID(t, r, "request")
	ctx := context.Background()
	cmd := control.CommandContext{CommandID: "answer-command", AcceptedAt: r.now(), Principal: contracts.Principal{WorkspaceID: "default", SubjectID: "owner", DeviceID: "device-1", Scopes: []string{"client:command"}}}
	var count int
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM voice_input_notices`).Scan(&count)
	if count != 0 {
		t.Fatal("notice before ACK")
	}
	tx, _ := r.store.DB().BeginTx(ctx, nil)
	_, _, err := r.svc.commandUIRendered(ctx, tx, cmd, contracts.CommandRequest{Payload: json.RawMessage(strings.ReplaceAll(`{"request_id":"request","surface_id":"screen","surface_version":1,"success":true}`, `"screen"`, `"`+surfaceID+`"`))})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var text string
	if err = r.store.DB().QueryRow(`SELECT text FROM voice_input_notices WHERE kind='ui_input_request' AND source_id='question'`).Scan(&text); err != nil || text != "Which?" {
		t.Fatalf("question=%q err=%v", text, err)
	}
	tx, _ = r.store.DB().BeginTx(ctx, nil)
	_, _, err = r.svc.commandUIAction(ctx, tx, cmd, contracts.CommandRequest{Payload: json.RawMessage(strings.ReplaceAll(`{"request_id":"request","surface_id":"screen","surface_version":1,"name":"submit","source_component_id":"submit","context":{"value":"answer"}}`, `"screen"`, `"`+surfaceID+`"`))})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = r.store.DB().QueryRow(`SELECT text FROM voice_input_notices WHERE kind='ui_input_received' AND source_id='answer-command'`).Scan(&text); err != nil || text != "answer" {
		t.Fatalf("answer=%q err=%v", text, err)
	}
}

func TestVoiceInputCommitUIJob(t *testing.T) {
	r := uiTargetRig(t)
	uiFlowServer(t, r)
	uiTargetQuestion(t, r)
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,task_id,created_at,updated_at) VALUES('request','default','owner','task','task',?,?)`, at, at)
	r.exec(t, `INSERT INTO ui_jobs(id,workspace_id,owner_subject_id,kind,dedupe_key,ui_request_id,attempts,next_attempt_at,created_at,updated_at) VALUES('job','default','owner','task_entry','voice-job','request',1,?,?,?)`, at, at, at)
	output := strings.Replace(uiFlowOutput(uiFlowCreate+","+uiFlowRoot, false), `"voice_input":false`, `"voice_input":true`, 1)
	output = strings.Replace(output, `"feedback":"Shown."`, `"feedback":""`, 1)
	o, err := uictl.Parse(output, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.svc.commitUIJob(context.Background(), uiJob{id: "job", w: "default", o: "owner", request: "request", kind: "task_entry", attempts: 1}, o, &uictl.Task{TaskID: "task", QuestionID: "question", Question: "Which?"}); err != nil {
		t.Fatal(err)
	}
	var voice, await bool
	var task, question sql.NullString
	if err = r.store.DB().QueryRow(`SELECT voice_input,awaiting_input,task_id,question_id FROM ui_requests WHERE id='request'`).Scan(&voice, &await, &task, &question); err != nil {
		t.Fatal(err)
	}
	if !voice || await || task.Valid || question.Valid {
		t.Fatalf("voice=%v awaiting=%v task=%v question=%v", voice, await, task, question)
	}
	var payload []byte
	if err = r.store.DB().QueryRow(`SELECT payload FROM events WHERE event_type='ui.surface'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event struct {
		Payload map[string]json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(payload, &event) != nil || string(event.Payload["voice_input"]) != "true" {
		t.Fatalf("event=%s", payload)
	}
	uiACK(t, r, "request", true)
	snapshot, err := r.svc.readUISnapshot(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "owner"})
	if err != nil || len(snapshot.Surfaces) != 1 || !snapshot.Surfaces[0].VoiceInput {
		t.Fatalf("voice snapshot=%+v err=%v", snapshot, err)
	}
	b, _ := json.Marshal(map[string]any{"request_id": "request", "surface_id": uiStoredID(t, r, "request"), "surface_version": 1, "name": "submit", "source_component_id": "submit", "context": map[string]string{"value": "typed text"}})
	tx, err := r.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	cmd := control.CommandContext{CommandID: "voice-answer", AcceptedAt: r.now(), Principal: contracts.Principal{WorkspaceID: "default", SubjectID: "owner", Scopes: []string{"client:command"}}}
	if _, _, err = r.svc.commandUIAction(context.Background(), tx, cmd, contracts.CommandRequest{Payload: b}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	snapshot, err = r.svc.readUISnapshot(context.Background(), cmd.Principal)
	if err != nil || len(snapshot.Surfaces) != 0 {
		t.Fatalf("answered voice snapshot=%+v err=%v", snapshot, err)
	}
}

func TestVoiceInputStaleActions(t *testing.T) {
	for _, tc := range []struct {
		name, surface string
		version       int
		status, code  string
	}{{"version", "screen", 2, "generated", "ui_input_stale"}, {"surface", "other", 1, "generated", "ui_request_not_awaiting_input"}, {"received", "screen", 1, "input_received", "ui_request_not_awaiting_input"}} {
		t.Run(tc.name, func(t *testing.T) {
			r := uiTargetRig(t)
			at := r.now().Format(time.RFC3339Nano)
			r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,surface_id,surface_version,voice_input,input_note,status,created_at,updated_at) VALUES('vr','default','owner','register','screen',1,1,'Type',?,?,?)`, tc.status, at, at)
			r.exec(t, `INSERT INTO ui_surfaces(workspace_id,owner_subject_id,surface_id,container,state_json,version,request_id,created_at,updated_at) VALUES('default','owner','screen','window','[]',1,'vr',?,?)`, at, at)
			cmd := control.CommandContext{CommandID: "answer", AcceptedAt: r.now(), Principal: contracts.Principal{WorkspaceID: "default", SubjectID: "owner", Scopes: []string{"client:command"}}}
			payload, _ := json.Marshal(map[string]any{"request_id": "vr", "surface_id": tc.surface, "surface_version": tc.version, "name": "submit", "source_component_id": "button", "context": map[string]string{"value": "hello"}})
			tx, _ := r.store.DB().BeginTx(context.Background(), nil)
			_, _, err := r.svc.commandUIAction(context.Background(), tx, cmd, contracts.CommandRequest{Payload: payload})
			_ = tx.Rollback()
			api, ok := err.(*control.APIError)
			if !ok || api.Code != tc.code {
				t.Fatalf("error=%v", err)
			}
			var n int
			_ = r.store.DB().QueryRow(`SELECT count(*) FROM voice_text_inputs`).Scan(&n)
			if n != 0 {
				t.Fatalf("inputs=%d", n)
			}
		})
	}
}
