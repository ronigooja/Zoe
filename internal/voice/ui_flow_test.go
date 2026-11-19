package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/modelclient"
)

const uiFlowCreate = `{"version":"v0.9.1","createSurface":{"surfaceId":"screen","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}}`
const uiFlowRoot = `{"version":"v0.9.1","updateComponents":{"surfaceId":"screen","components":[{"id":"root","component":"Text","text":"Title"}]}}`

// uiFlowRun supplies the authoritative run required by task-result UI events.
func uiFlowRun(t *testing.T, db *sql.DB, task, run string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,fence,runner_id,authorized_device_id,command_id,goal,profile_id,runtime_route_id,model_route_id,role,harness,started_at) SELECT ?,workspace_id,id,'ui-attempt','stopped',1,'runner',created_by_device_id,command_id,goal,profile_id,'runtime','model','worker','opencode',created_at FROM tasks WHERE id=?`, run, task)
	if err != nil {
		t.Fatal(err)
	}
}

type uiFlowReceiptObserver struct {
	ResultReceiptAdapter
	submits, lookups int
}

func (a *uiFlowReceiptObserver) Submit(ctx context.Context, gateway string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	a.submits++
	return a.ResultReceiptAdapter.Submit(ctx, gateway, p)
}

func (a *uiFlowReceiptObserver) Lookup(ctx context.Context, gateway string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	a.lookups++
	return a.ResultReceiptAdapter.Lookup(ctx, gateway, p)
}

func uiFlowOutput(messages string, await bool) string {
	feedback := "Shown."
	if await {
		feedback = ""
	}
	b, _ := json.Marshal(map[string]any{"container": "window", "feedback": feedback, "awaiting_input": await, "voice_input": false, "size": map[string]int{"width": 400, "height": 300}})
	return "<ui-meta>" + string(b) + "</ui-meta><a2ui-json>[" + messages + "]</a2ui-json>"
}
func uiFlowModel(t *testing.T, output *string, mu *sync.Mutex) *modelclient.Client {
	t.Helper()
	wantPrompt := testPrompts(t).UIController()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Instructions string `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Instructions != wantPrompt {
			t.Error("UI controller did not receive the loaded prompt")
		}
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"output_text": *output})
	}))
	t.Cleanup(server.Close)
	c, err := modelclient.FromEnv("UI", func(k string) string {
		switch k {
		case "UI_BASE_URL":
			return server.URL
		case "UI_MODEL":
			return "fixture"
		case "UI_API_KEY_FILE":
			return "fixture"
		}
		return ""
	}, func(string) ([]byte, error) { return []byte("fixture"), nil }, 1000, 1, 30000)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func uiFlowServer(t *testing.T, r *deliveryRig) {
	t.Helper()
	server, err := control.NewServer(r.store, control.Config{})
	if err != nil {
		t.Fatal(err)
	}
	r.svc.uiEventServer = server
}
func uiFlowRegister(t *testing.T, r *deliveryRig) {
	t.Helper()
	r.seedCall(t, "call", "owner", 1)
	r.exec(t, `INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,exits,request) VALUES('turn','default','call',1,'turn',1,'original sentence',X'01',?,'turn-command','ui','resolved request')`, r.now().Format(time.RFC3339Nano))
	r.svc.createRegisterUIJobs(context.Background())
	r.svc.dispatchUIJobs(context.Background())
}

func TestUIRegisterJobFlow(t *testing.T) {
	r := uiTargetRig(t)
	uiFlowServer(t, r)
	var mu sync.Mutex
	output := uiFlowOutput(uiFlowCreate+","+uiFlowRoot, false)
	r.svc.uiController = uiFlowModel(t, &output, &mu)
	uiFlowRegister(t, r)
	var id, status, state string
	var awaiting, handled, events int
	if err := r.store.DB().QueryRow(`SELECT id,status,awaiting_input FROM ui_requests`).Scan(&id, &status, &awaiting); err != nil {
		t.Fatal(err)
	}
	_ = r.store.DB().QueryRow(`SELECT state_json FROM ui_surfaces`).Scan(&state)
	_ = r.store.DB().QueryRow(`SELECT ui_handled FROM voice_turns WHERE id='turn'`).Scan(&handled)
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='ui.surface'`).Scan(&events)
	if status != "generated" || awaiting != 0 || handled != 1 || events != 1 || !strings.Contains(state, `"root"`) {
		t.Fatalf("status=%s awaiting=%d handled=%d events=%d state=%s", status, awaiting, handled, events, state)
	}
	var raw []byte
	_ = r.store.DB().QueryRow(`SELECT payload FROM events WHERE event_type='ui.surface'`).Scan(&raw)
	var e struct {
		Payload map[string]json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(raw, &e) != nil || len(e.Payload) != 9 || string(e.Payload["voice_input"]) != "false" || string(e.Payload["size"]) != `{"width":400,"height":300}` {
		t.Fatal(string(raw))
	}
	r.svc.createRegisterUIJobs(context.Background())
	var jobs int
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM ui_jobs`).Scan(&jobs)
	if jobs != 1 {
		t.Fatal(jobs)
	}
}

func TestUIJobServerOwnedMessageVersion(t *testing.T) {
	for _, version := range []string{`"version":"v0.9",`, ""} {
		t.Run(version, func(t *testing.T) {
			r := uiTargetRig(t)
			uiFlowServer(t, r)
			var mu sync.Mutex
			output := uiFlowOutput(strings.ReplaceAll(uiFlowCreate+","+uiFlowRoot, `"version":"v0.9.1",`, version), false)
			r.svc.uiController = uiFlowModel(t, &output, &mu)
			uiFlowRegister(t, r)
			var state, payload, status string
			if err := r.store.DB().QueryRow(`SELECT status FROM ui_jobs`).Scan(&status); err != nil || status != "done" {
				t.Fatalf("status=%s err=%v", status, err)
			}
			if err := r.store.DB().QueryRow(`SELECT state_json FROM ui_surfaces`).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if err := r.store.DB().QueryRow(`SELECT payload FROM events WHERE event_type='ui.surface'`).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			for _, document := range []string{state, payload} {
				if strings.Count(document, `"version":"v0.9.1"`) != 2 || strings.Contains(document, `"version":"v0.9"`) {
					t.Fatalf("noncanonical document: %s", document)
				}
			}
		})
	}
}

func TestUIJobSafeValidationFailure(t *testing.T) {
	for _, tc := range []struct{ messages, code string }{
		{strings.Replace(uiFlowCreate, `"version":"v0.9.1"`, `"version":123`, 1), "validation_failed:string_required:version"},
		{uiFlowCreate + "," + strings.Replace(uiFlowRoot, `"text":"Title"`, `"text":{"private user text":"model content"}`, 1), "validation_failed:component_property:Text.text"},
		{`{"private user text":"model content"}`, "validation_failed:unknown_field"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			r := uiTargetRig(t)
			uiFlowServer(t, r)
			var mu sync.Mutex
			output := uiFlowOutput(tc.messages, false)
			r.svc.uiController = uiFlowModel(t, &output, &mu)
			uiFlowRegister(t, r)
			var status, code string
			if err := r.store.DB().QueryRow(`SELECT status,last_error FROM ui_jobs`).Scan(&status, &code); err != nil {
				t.Fatal(err)
			}
			if status != "failed" || code != tc.code || strings.Contains(code, "private") || strings.Contains(code, "model content") {
				t.Fatalf("status=%s code=%s", status, code)
			}
		})
	}
}

func TestUITaskJobsAccumulate(t *testing.T) {
	r := uiTargetRig(t)
	uiFlowServer(t, r)
	var mu sync.Mutex
	output := uiFlowOutput(uiFlowCreate+","+uiFlowRoot, false)
	r.svc.uiController = uiFlowModel(t, &output, &mu)
	r.seedTask(t, "task", "owner")
	uiFlowRun(t, r.store.DB(), "task", "run")
	r.exec(t, `UPDATE tasks SET ui_display=1,result='first result' WHERE id='task'`)
	r.seedEntry(t, "task", "entry-one", "first result")
	r.seedEvent(t, 1, "task", "run", "entry-one")
	r.exec(t, `UPDATE event_counters SET current_cursor=1 WHERE workspace_id='default'`)
	r.svc.consumeUITaskEvents(context.Background())
	r.svc.dispatchUIJobs(context.Background())
	mu.Lock()
	output = uiFlowOutput(`{"version":"v0.9.1","updateDataModel":{"surfaceId":"screen","value":{"text":"second"}}}`, false)
	mu.Unlock()
	r.seedEntry(t, "task", "entry-two", "second result")
	r.exec(t, `UPDATE tasks SET result='second result' WHERE id='task'`)
	r.seedEvent(t, 3, "task", "run", "entry-two")
	r.exec(t, `UPDATE event_counters SET current_cursor=3 WHERE workspace_id='default'`)
	r.svc.consumeUITaskEvents(context.Background())
	r.svc.dispatchUIJobs(context.Background())
	r.svc.consumeUITaskEvents(context.Background())
	var events, feedback, jobs int
	var raw []byte
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='ui.surface'`).Scan(&events)
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM ui_feedback_deliveries`).Scan(&feedback)
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM ui_jobs`).Scan(&jobs)
	_ = r.store.DB().QueryRow(`SELECT state_json FROM ui_surfaces`).Scan(&raw)
	var messages []json.RawMessage
	_ = json.Unmarshal(raw, &messages)
	if events != 2 || feedback != 0 || jobs != 2 || len(messages) != 3 {
		t.Fatalf("events=%d feedback=%d jobs=%d messages=%d", events, feedback, jobs, len(messages))
	}
	var pins int
	if err := r.store.DB().QueryRow(`SELECT count(*) FROM ui_result_sources WHERE run_id='run' AND ((entry_id='entry-one' AND event_cursor=1) OR (entry_id='entry-two' AND event_cursor=3))`).Scan(&pins); err != nil || pins != 2 {
		t.Fatalf("authoritative per-job pins=%d err=%v", pins, err)
	}
}

func TestUIInputRedisplayKey(t *testing.T) {
	r := uiTargetRig(t)
	r.seedTask(t, "task", "owner")
	r.seedEntry(t, "task", "entry", "existing result")
	r.exec(t, `UPDATE tasks SET result='existing result' WHERE id='task'`)
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,task_id,request,created_at,updated_at) VALUES('request','default','owner','task','task','Show the existing task result.',?,?)`, r.now().Format(time.RFC3339Nano), r.now().Format(time.RFC3339Nano))
	uiFlowRun(t, r.store.DB(), "task", "run")
	r.seedEvent(t, 1, "task", "run", "entry")
	tx, err := r.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := pinUIResultTx(context.Background(), tx, "default", "owner", "task", "request", "entry", 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r.exec(t, `UPDATE tasks SET result='mutable decoy' WHERE id='task'`)
	for _, key := range []string{"task_entry:entry", "task_entry:entry#turn-one", "task_entry:entry#turn-two"} {
		input, err := r.svc.uiInput(context.Background(), uiJob{w: "default", o: "owner", kind: "task_entry", key: key, request: "request"})
		if err != nil || !strings.Contains(string(input), "existing result") || strings.Contains(string(input), "mutable decoy") {
			t.Fatalf("key=%s input=%s err=%v", key, input, err)
		}
	}
}

func TestUIJobFailures(t *testing.T) {
	for _, name := range []string{"invalid", "disabled", "unconfigured", "state"} {
		t.Run(name, func(t *testing.T) {
			r := uiTargetRig(t)
			uiFlowServer(t, r)
			var mu sync.Mutex
			output := "{}"
			if name == "state" {
				output = uiFlowOutput(uiFlowCreate, false)
			}
			if name == "invalid" || name == "state" {
				r.svc.uiController = uiFlowModel(t, &output, &mu)
			} else if name == "unconfigured" {
				r.svc.uiController = &modelclient.Client{}
			}
			uiFlowRegister(t, r)
			var status, text, code string
			var count, handled int
			if err := r.store.DB().QueryRow(`SELECT status,last_error FROM ui_jobs`).Scan(&status, &code); err != nil {
				t.Fatal(err)
			}
			_ = r.store.DB().QueryRow(`SELECT text FROM ui_feedback_deliveries`).Scan(&text)
			_ = r.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='ui.surface'`).Scan(&count)
			_ = r.store.DB().QueryRow(`SELECT ui_handled FROM voice_turns WHERE id='turn'`).Scan(&handled)
			if status != "failed" || text != stewardErrorSpeech || count != 0 || handled != 1 {
				t.Fatalf("status=%s code=%s text=%s events=%d handled=%d", status, code, text, count, handled)
			}
		})
	}
}

func TestUICommands(t *testing.T) {
	r := uiTargetRig(t)
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,surface_id,awaiting_input,input_note,created_at,updated_at) VALUES('request','default','owner','register','screen',1,'Requested input',?,?)`, at, at)
	cmd := control.CommandContext{Principal: contracts.Principal{WorkspaceID: "default", SubjectID: "owner", Scopes: []string{"client:command"}}}
	ctx := context.Background()
	tx, err := r.store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := contracts.CommandRequest{Payload: json.RawMessage(`{"request_id":"request"}`)}
	if _, _, err = r.svc.commandUIRendered(ctx, tx, cmd, request); err == nil {
		t.Fatal("incomplete acknowledgement accepted")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	content := "  <raw>\n中文  "
	payload, _ := json.Marshal(map[string]any{"request_id": "request", "surface_id": "screen", "surface_version": 1, "name": "submit", "source_component_id": "submit", "context": map[string]string{"value": content}})
	request.Payload = payload
	tx, _ = r.store.DB().BeginTx(ctx, nil)
	wrong := cmd
	wrong.Principal.SubjectID = "other"
	if _, _, err = r.svc.commandUIAction(ctx, tx, wrong, request); err == nil {
		t.Fatal("wrong owner accepted")
	}
	_ = tx.Rollback()
	tx, _ = r.store.DB().BeginTx(ctx, nil)
	if _, _, err = r.svc.commandUIAction(ctx, tx, cmd, request); err == nil {
		t.Fatal("unbound input accepted")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var text string
	_ = r.store.DB().QueryRow(`SELECT text FROM ui_feedback_deliveries`).Scan(&text)
	if text != "" {
		t.Fatalf("text=%q", text)
	}
	tx, _ = r.store.DB().BeginTx(ctx, nil)
	_, _, err = r.svc.commandUIAction(ctx, tx, cmd, request)
	_ = tx.Rollback()
	if api, ok := err.(*control.APIError); !ok || api.Status != 409 {
		t.Fatalf("double submit=%v", err)
	}
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,surface_id,awaiting_input,input_note,created_at,updated_at) VALUES('compact','default','owner','register','screen',1,'Fields',?,?)`, at, at)
	request.Payload = json.RawMessage(`{"request_id":"compact","surface_id":"screen","surface_version":1,"name":"submit","source_component_id":"submit","context":{"a":"<raw>","b":2}}`)
	tx, _ = r.store.DB().BeginTx(ctx, nil)
	if _, _, err = r.svc.commandUIAction(ctx, tx, cmd, request); err == nil {
		t.Fatal("unbound structured input accepted")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_ = r.store.DB().QueryRow(`SELECT text FROM ui_feedback_deliveries WHERE request_id='compact'`).Scan(&text)
	if text != "" {
		t.Fatalf("compact=%q", text)
	}
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,surface_id,awaiting_input,created_at,updated_at) VALUES('not-awaiting','default','owner','register','screen',0,?,?)`, at, at)
	request.Payload = json.RawMessage(`{"request_id":"not-awaiting","surface_id":"screen","surface_version":1,"name":"submit","source_component_id":"submit","context":{}}`)
	tx, _ = r.store.DB().BeginTx(ctx, nil)
	_, _, err = r.svc.commandUIAction(ctx, tx, cmd, request)
	_ = tx.Rollback()
	if api, ok := err.(*control.APIError); !ok || api.Status != 409 {
		t.Fatalf("not-awaiting=%v", err)
	}
}

func TestUIFeedbackContinuity(t *testing.T) {
	r := uiTargetRig(t)
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,created_at,updated_at) VALUES('request','default','owner','register',?,?)`, at, at)
	r.exec(t, `UPDATE ui_requests SET status='failed' WHERE id='request'`)
	r.exec(t, `INSERT INTO ui_feedback_deliveries(request_id,workspace_id,owner_subject_id,text,created_at,updated_at) VALUES('request','default','owner','Shown text',?,?)`, at, at)
	r.svc.dispatchUIFeedback(context.Background())
	var status string
	var attempts int
	_ = r.store.DB().QueryRow(`SELECT status,attempts FROM ui_feedback_deliveries`).Scan(&status, &attempts)
	if status != "pending" || attempts != 0 {
		t.Fatalf("%s/%d", status, attempts)
	}
	r.seedCall(t, "call", "owner", 1)
	r.enableReceiptFixture(t)
	r.advance(31 * time.Second)
	r.svc.dispatchUIFeedback(context.Background())
	_ = r.store.DB().QueryRow(`SELECT status,attempts FROM ui_feedback_deliveries`).Scan(&status, &attempts)
	if status != "delivered" || attempts != 1 || r.posts.Load() != 1 {
		t.Fatalf("%s/%d posts=%d", status, attempts, r.posts.Load())
	}
	opening := r.svc.buildAVASOpeningContext(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "owner"})
	if len(opening.Items) != 1 || opening.Items[0].Text != "Shown text" || opening.Items[0].Role != "user" {
		t.Fatalf("opening=%+v", opening)
	}
}

func TestUIFeedbackGatewayRetries(t *testing.T) {
	r := uiTargetRig(t)
	at := r.now().Format(time.RFC3339Nano)
	r.seedCall(t, "call", "owner", 1)
	r.enableReceiptFixture(t)
	adapter := &uiFlowReceiptObserver{ResultReceiptAdapter: r.svc.resultReceiptAdapter}
	r.svc.resultReceiptAdapter = adapter
	r.code.Store(503)
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,created_at,updated_at) VALUES('request','default','owner','register',?,?)`, at, at)
	r.exec(t, `UPDATE ui_requests SET status='failed' WHERE id='request'`)
	r.exec(t, `INSERT INTO ui_feedback_deliveries(request_id,workspace_id,owner_subject_id,text,created_at,updated_at) VALUES('request','default','owner','Shown text',?,?)`, at, at)
	for i := 0; i < 3; i++ {
		r.svc.dispatchUIFeedback(context.Background())
		r.advance(31 * time.Second)
	}
	var status string
	var attempts int
	_ = r.store.DB().QueryRow(`SELECT status,attempts FROM ui_feedback_deliveries`).Scan(&status, &attempts)
	// Ambiguous submission stays pending and subsequent attempts reconcile only.
	var chunkState, code string
	if err := r.store.DB().QueryRow(`SELECT state FROM voice_text_chunks WHERE source_kind='ui_feedback' AND source_id='request'`).Scan(&chunkState); err != nil {
		t.Fatal(err)
	}
	if err := r.store.DB().QueryRow(`SELECT last_error FROM ui_feedback_deliveries`).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != 0 || adapter.submits != 1 || adapter.lookups != 2 || chunkState != "unknown" || code != "receipt_lookup_unavailable" {
		t.Fatalf("%s/%d submits=%d lookups=%d chunk=%s code=%s", status, attempts, adapter.submits, adapter.lookups, chunkState, code)
	}
}

func TestUIModelRetries(t *testing.T) {
	r := uiTargetRig(t)
	uiFlowServer(t, r)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	c, err := modelclient.FromEnv("UI", func(k string) string {
		switch k {
		case "UI_BASE_URL":
			return server.URL
		case "UI_MODEL":
			return "fixture"
		case "UI_API_KEY_FILE":
			return "fixture"
		}
		return ""
	}, func(string) ([]byte, error) { return []byte("fixture"), nil }, 1000, 1, 30000)
	if err != nil {
		t.Fatal(err)
	}
	r.svc.uiController = c
	uiFlowRegister(t, r)
	var status string
	var attempts, feedback int
	_ = r.store.DB().QueryRow(`SELECT status,attempts FROM ui_jobs`).Scan(&status, &attempts)
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM ui_feedback_deliveries`).Scan(&feedback)
	if status != "pending" || attempts != 1 || feedback != 0 {
		t.Fatalf("%s/%d feedback=%d", status, attempts, feedback)
	}
	for i := 0; i < 2; i++ {
		r.advance(5 * time.Second)
		r.svc.dispatchUIJobs(context.Background())
	}
	_ = r.store.DB().QueryRow(`SELECT status,attempts FROM ui_jobs`).Scan(&status, &attempts)
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM ui_feedback_deliveries`).Scan(&feedback)
	if status != "failed" || attempts != 3 || feedback != 1 {
		t.Fatalf("%s/%d feedback=%d", status, attempts, feedback)
	}
}

func TestUIOpeningFeedbackOrderAndBudget(t *testing.T) {
	r := newDeliveryRig(t)
	for i, id := range []string{"old", "new"} {
		at := r.now().Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,created_at,updated_at) VALUES(?,'default','owner','register',?,?)`, id, at, at)
		r.exec(t, `INSERT INTO ui_feedback_deliveries(request_id,workspace_id,owner_subject_id,text,status,created_at,updated_at) VALUES(?,'default','owner',?,'delivered',?,?)`, id, id, at, at)
	}
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "owner"}
	opening := r.svc.buildAVASOpeningContext(context.Background(), p)
	if len(opening.Items) != 2 || opening.Items[0].Text != "old" || opening.Items[1].Text != "new" {
		t.Fatalf("items=%+v", opening.Items)
	}
	r.svc.openingBudget = 3
	opening = r.svc.buildAVASOpeningContext(context.Background(), p)
	if len(opening.Items) != 1 || opening.Items[0].Text != "new" {
		t.Fatalf("budget items=%+v", opening.Items)
	}
}

func TestUIDeleteEventAndLease(t *testing.T) {
	r := uiTargetRig(t)
	uiFlowServer(t, r)
	var mu sync.Mutex
	output := uiFlowOutput(uiFlowCreate+","+uiFlowRoot, false)
	r.svc.uiController = uiFlowModel(t, &output, &mu)
	uiFlowRegister(t, r)
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,created_at,updated_at) VALUES('delete','default','owner','register',?,?)`, at, at)
	r.exec(t, `INSERT INTO ui_jobs(id,workspace_id,owner_subject_id,kind,dedupe_key,ui_request_id,attempts,next_attempt_at,created_at,updated_at) VALUES('delete-job','default','owner','register','register:delete','delete',1,?,?,?)`, r.now().Add(30*time.Second).Format(time.RFC3339Nano), at, at)
	mu.Lock()
	output = uiFlowOutput(`{"version":"v0.9.1","deleteSurface":{"surfaceId":"screen"}}`, false)
	mu.Unlock()
	r.svc.dispatchUIJobs(context.Background())
	var count int
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM ui_surfaces`).Scan(&count)
	if count != 1 {
		t.Fatal("lease was ignored")
	}
	r.advance(31 * time.Second)
	r.svc.dispatchUIJobs(context.Background())
	_ = r.store.DB().QueryRow(`SELECT count(*) FROM ui_surfaces WHERE deleted=0`).Scan(&count)
	if count != 0 {
		t.Fatal("surface was not tombstoned")
	}
	var raw []byte
	_ = r.store.DB().QueryRow(`SELECT payload FROM events WHERE event_type='ui.surface' ORDER BY cursor DESC LIMIT 1`).Scan(&raw)
	var e struct {
		Payload struct {
			Messages []json.RawMessage `json:"messages"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &e) != nil || len(e.Payload.Messages) != 1 || !strings.Contains(string(e.Payload.Messages[0]), "deleteSurface") {
		t.Fatalf("event=%s", raw)
	}
}

func TestUIValidationRetries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retries   int
		transient bool
		success   bool
		calls     int
		status    string
		code      string
	}{
		{"success", 2, false, true, 3, "done", ""},
		{"disabled", 0, false, false, 1, "failed", "validation_failed:string_required:version"},
		{"exhausted", 1, false, false, 2, "failed", "validation_failed:string_required:version"},
		{"transient", 2, true, false, 2, "pending", "model_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := uiTargetRig(t)
			uiFlowServer(t, r)
			r.svc.uiControllerMaxRetries = tc.retries
			var inputs []string
			invalid := uiFlowOutput(strings.Replace(uiFlowCreate, `"version":"v0.9.1"`, `"version":123`, 1), false)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body struct {
					Input string `json:"input"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				inputs = append(inputs, body.Input)
				if tc.transient && len(inputs) == 2 {
					w.WriteHeader(503)
					return
				}
				output := invalid
				if tc.success && len(inputs) == 3 {
					output = uiFlowOutput(uiFlowCreate+","+uiFlowRoot, false)
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"output_text": output})
			}))
			defer server.Close()
			c, err := modelclient.FromEnv("UI", func(k string) string {
				switch k {
				case "UI_BASE_URL":
					return server.URL
				case "UI_MODEL", "UI_API_KEY_FILE":
					return "fixture"
				}
				return ""
			}, func(string) ([]byte, error) { return []byte("fixture"), nil }, 1000, 1, 30000)
			if err != nil {
				t.Fatal(err)
			}
			r.svc.uiController = c
			uiFlowRegister(t, r)
			var status, code string
			var attempts int
			if err := r.store.DB().QueryRow(`SELECT status,coalesce(last_error,''),attempts FROM ui_jobs`).Scan(&status, &code, &attempts); err != nil {
				t.Fatal(err)
			}
			if len(inputs) != tc.calls || status != tc.status || code != tc.code || attempts != 1 {
				t.Fatalf("calls=%d status=%s code=%s attempts=%d", len(inputs), status, code, attempts)
			}
			var base map[string]json.RawMessage
			if err := json.Unmarshal([]byte(inputs[0]), &base); err != nil {
				t.Fatal(err)
			}
			if _, ok := base["previous_error"]; ok {
				t.Fatal("first call has previous_error")
			}
			for _, input := range inputs[1:] {
				var got map[string]json.RawMessage
				if err := json.Unmarshal([]byte(input), &got); err != nil {
					t.Fatal(err)
				}
				if string(got["previous_error"]) != `{"category":"string_required","field":"version"}` {
					t.Fatalf("feedback=%s", got["previous_error"])
				}
				delete(got, "previous_error")
				a, _ := json.Marshal(base)
				b, _ := json.Marshal(got)
				if string(a) != string(b) || strings.Contains(input, invalid) {
					t.Fatal("retry input changed or leaked output")
				}
			}
			if tc.status == "failed" {
				var feedback string
				if err := r.store.DB().QueryRow(`SELECT text FROM ui_feedback_deliveries`).Scan(&feedback); err != nil || feedback != stewardErrorSpeech {
					t.Fatalf("feedback=%q err=%v", feedback, err)
				}
			}
		})
	}
}
