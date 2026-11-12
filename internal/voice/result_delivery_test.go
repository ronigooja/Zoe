package voice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/storage"
)

type deliveryRig struct {
	store *storage.Store
	svc   *Service
	posts atomic.Int32
	mu    sync.Mutex
	paths []string
	auth  []string
	texts []string
	code  atomic.Int32
	clock atomic.Int64
}

func (r *deliveryRig) now() time.Time { return time.Unix(0, r.clock.Load()).UTC() }
func (r *deliveryRig) advance(d time.Duration) {
	r.clock.Add(int64(d))
}

func newDeliveryRig(t *testing.T) *deliveryRig {
	t.Helper()
	r := &deliveryRig{}
	r.clock.Store(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC).UnixNano())
	r.code.Store(200)
	gw := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(body, &p)
		r.posts.Add(1)
		r.mu.Lock()
		r.paths = append(r.paths, req.URL.Path)
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		r.texts = append(r.texts, p.Text)
		r.mu.Unlock()
		w.WriteHeader(int(r.code.Load()))
	}))
	t.Cleanup(gw.Close)
	store, svc := newVoiceTestService(t, Config{GatewayURL: gw.URL, GatewayAPIKey: "gw-key", HTTPClient: gw.Client(), Now: r.now})
	r.store, r.svc = store, svc
	return r
}

func (r *deliveryRig) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := r.store.DB().Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (r *deliveryRig) seedTask(t *testing.T, taskID, subject string) {
	t.Helper()
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `INSERT OR IGNORE INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES('device-1','default',?, 'test','approved',?)`, subject, at)
	r.exec(t, `INSERT INTO command_receipts(command_id,workspace_id,subject_id,idempotency_key,request_hash,receipt_json,accepted_at) VALUES(?, 'default',?,?,X'01','{}',?)`, "cmd-"+taskID, subject, "key-"+taskID, at)
	r.exec(t, `INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,result,error,created_by_device_id,command_id,owner_subject_id) VALUES(?, 'default','title','goal','voice','zoe-steward','awaiting_user',1,?,?, '', '', 'device-1',?,?)`, taskID, at, at, "cmd-"+taskID, subject)
}

func (r *deliveryRig) seedEntry(t *testing.T, taskID, entryID, text string) {
	t.Helper()
	r.exec(t, `INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES(?,?,'default','assistant_message',?,'device-1',?)`, entryID, taskID, text, r.now().Format(time.RFC3339Nano))
}

func (r *deliveryRig) seedEvent(t *testing.T, cursor int64, taskID, runID, entryID string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"payload": map[string]string{"task_id": taskID, "run_id": runID, "entry_id": entryID, "entry_kind": "assistant_message", "role": "steward"}})
	r.exec(t, `INSERT INTO events(event_id,workspace_id,cursor,schema_version,event_type,occurred_at,aggregate_type,aggregate_id,aggregate_version,payload) VALUES(?, 'default',?,1,'task.assistant_message',?,'task',?,1,?)`, "event-"+entryID, cursor, r.now().Format(time.RFC3339Nano), taskID, payload)
}

func (r *deliveryRig) seedCall(t *testing.T, sessionID, owner string, generation int64) {
	t.Helper()
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,gateway_session_id,generation,state,created_at,updated_at) VALUES(?, 'default',?, 'device-1',?,X'01','gw-'||?,?,'active',?,?)`, sessionID, owner, "req-"+sessionID, sessionID, generation, at, at)
}

func (r *deliveryRig) run(t *testing.T) {
	t.Helper()
	r.svc.consumeResultEvents(context.Background())
	r.svc.dispatchResults(context.Background())
}

func (r *deliveryRig) row(t *testing.T, entryID string) (status, code string, attempts int) {
	t.Helper()
	if err := r.store.DB().QueryRow(`SELECT status,last_error_code,attempts FROM voice_result_deliveries WHERE entry_id=?`, entryID).Scan(&status, &code, &attempts); err != nil {
		t.Fatalf("row %s: %v", entryID, err)
	}
	return
}

func (r *deliveryRig) setup(t *testing.T, text string) {
	t.Helper()
	r.seedTask(t, "task-1", "user-1")
	r.seedEntry(t, "task-1", "entry-1", text)
	r.seedEvent(t, 1, "task-1", "run-1", "entry-1")
}

func TestResultDeliveryLiveCallSendsOneBoundedMessage(t *testing.T) {
	r := newDeliveryRig(t)
	r.setup(t, "Tokyo will be rainy today.")
	r.seedCall(t, "s1", "user-1", 1)
	r.run(t)
	if got := r.posts.Load(); got != 1 {
		t.Fatalf("gateway posts=%d, want 1", got)
	}
	if r.paths[0] != "/v1/voice/sessions/gw-s1/context" || r.auth[0] != "Bearer gw-key" {
		t.Fatalf("path=%q auth=%q", r.paths[0], r.auth[0])
	}
	if want := "Tokyo will be rainy today."; r.texts[0] != want {
		t.Fatalf("text=%q want %q", r.texts[0], want)
	}
	if status, _, attempts := r.row(t, "entry-1"); status != "delivered" || attempts != 1 {
		t.Fatalf("status=%s attempts=%d", status, attempts)
	}
	r.run(t)
	if got := r.posts.Load(); got != 1 {
		t.Fatalf("delivered row must not be re-sent, posts=%d", got)
	}
}

func TestResultDeliveryNoLiveCallIsSkipped(t *testing.T) {
	r := newDeliveryRig(t)
	r.setup(t, "hello")
	r.run(t)
	if r.posts.Load() != 0 {
		t.Fatal("no call must not reach the gateway")
	}
	if status, _, _ := r.row(t, "entry-1"); status != "skipped_no_call" {
		t.Fatalf("status=%s", status)
	}
}

func TestResultDeliveryIgnoresAnotherOwnersCall(t *testing.T) {
	r := newDeliveryRig(t)
	r.setup(t, "hello")
	r.seedCall(t, "s-other", "someone-else", 1)
	r.run(t)
	if r.posts.Load() != 0 {
		t.Fatal("another owner's call must never receive the result")
	}
	if status, _, _ := r.row(t, "entry-1"); status != "skipped_no_call" {
		t.Fatalf("status=%s", status)
	}
}

func TestResultDeliveryGatewayFailureRetriesThenFails(t *testing.T) {
	r := newDeliveryRig(t)
	r.code.Store(503)
	r.setup(t, "hello")
	r.seedCall(t, "s1", "user-1", 1)
	r.run(t)
	if status, code, attempts := r.row(t, "entry-1"); status != "pending" || attempts != 1 || code != "gateway_failed" {
		t.Fatalf("after first failure status=%s attempts=%d code=%s", status, attempts, code)
	}
	r.run(t)
	if r.posts.Load() != 1 {
		t.Fatalf("backoff must delay the retry, posts=%d", r.posts.Load())
	}
	for i := 0; i < 4; i++ {
		r.advance(10 * time.Second)
		r.run(t)
	}
	status, _, attempts := r.row(t, "entry-1")
	if status != "failed" || attempts != resultDeliveryMaxAttempts {
		t.Fatalf("status=%s attempts=%d", status, attempts)
	}
	if got := r.posts.Load(); got != int32(resultDeliveryMaxAttempts) {
		t.Fatalf("posts=%d, want %d", got, resultDeliveryMaxAttempts)
	}
}

func TestResultDeliveryUnsupportedRouteFailsWithoutRetry(t *testing.T) {
	r := newDeliveryRig(t)
	r.code.Store(501)
	r.setup(t, "hello")
	r.seedCall(t, "s1", "user-1", 1)
	r.run(t)
	r.advance(time.Minute)
	r.run(t)
	if status, code, _ := r.row(t, "entry-1"); status != "failed" || code != "route_unsupported" {
		t.Fatalf("status=%s code=%s", status, code)
	}
	if r.posts.Load() != 1 {
		t.Fatalf("501 must not be retried, posts=%d", r.posts.Load())
	}
}

func TestResultDeliveryDuplicateEventSendsOnce(t *testing.T) {
	r := newDeliveryRig(t)
	r.setup(t, "hello")
	r.seedCall(t, "s1", "user-1", 1)
	r.run(t)
	r.exec(t, `UPDATE voice_result_cursor SET last_cursor=0 WHERE id=1`)
	r.run(t)
	var n int
	if err := r.store.DB().QueryRow(`SELECT count(*) FROM voice_result_deliveries`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
	if r.posts.Load() != 1 {
		t.Fatalf("replayed event posts=%d, want 1", r.posts.Load())
	}
}

func TestResultDeliveryMissingEntryRetriesThenFails(t *testing.T) {
	r := newDeliveryRig(t)
	r.seedTask(t, "task-1", "user-1")
	r.seedEvent(t, 1, "task-1", "run-1", "entry-missing")
	r.seedCall(t, "s1", "user-1", 1)
	r.run(t)
	if status, code, _ := r.row(t, "entry-missing"); status != "pending" || code != "entry_missing" {
		t.Fatalf("status=%s code=%s", status, code)
	}
	for i := 0; i < 4; i++ {
		r.advance(10 * time.Second)
		r.run(t)
	}
	if status, code, _ := r.row(t, "entry-missing"); status != "failed" || code != "entry_missing" {
		t.Fatalf("status=%s code=%s", status, code)
	}
	if r.posts.Load() != 0 {
		t.Fatal("a missing entry must never reach the gateway")
	}
}

func TestResultDeliveryUnknownTaskDoesNotBlockLaterEvents(t *testing.T) {
	r := newDeliveryRig(t)
	r.seedEvent(t, 1, "task-ghost", "run-0", "entry-ghost")
	r.seedTask(t, "task-1", "user-1")
	r.seedEntry(t, "task-1", "entry-1", "hello")
	r.seedEvent(t, 2, "task-1", "run-1", "entry-1")
	r.seedCall(t, "s1", "user-1", 1)
	r.run(t)
	if status, code, _ := r.row(t, "entry-ghost"); status != "failed" || code != "task_unknown" {
		t.Fatalf("ghost status=%s code=%s", status, code)
	}
	if status, _, _ := r.row(t, "entry-1"); status != "delivered" {
		t.Fatalf("later event status=%s", status)
	}
}

func TestResultDeliverySessionEndedBeforeSendIsNotSentToANewerCall(t *testing.T) {
	r := newDeliveryRig(t)
	r.setup(t, "hello")
	r.seedCall(t, "s-old", "user-1", 1)
	r.svc.consumeResultEvents(context.Background())
	r.exec(t, `UPDATE voice_sessions SET state='ended' WHERE id='s-old'`)
	r.run(t)
	if r.posts.Load() != 0 {
		t.Fatal("a result must not be sent once its call has ended")
	}
	if status, _, _ := r.row(t, "entry-1"); status != "skipped_no_call" {
		t.Fatalf("status=%s", status)
	}
}

func TestResultDeliveryConcurrentDispatchersSendOnce(t *testing.T) {
	r := newDeliveryRig(t)
	r.setup(t, "hello")
	r.seedCall(t, "s1", "user-1", 1)
	r.svc.consumeResultEvents(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.svc.dispatchResults(context.Background())
		}()
	}
	wg.Wait()
	if got := r.posts.Load(); got != 1 {
		t.Fatalf("concurrent dispatchers posted %d times, want 1", got)
	}
}

func TestResultDeliveryTextIsSanitizedAndBounded(t *testing.T) {
	r := newDeliveryRig(t)
	long := strings.Repeat("天", 700)
	r.setup(t, "line one\n\nline\ttwo\x00\x07 end "+long)
	r.seedCall(t, "s1", "user-1", 1)
	r.run(t)
	if r.posts.Load() != 1 {
		t.Fatalf("posts=%d", r.posts.Load())
	}
	sent := strings.TrimSuffix(strings.TrimPrefix(r.texts[0], "Zoe result for the user: "), " Please tell them briefly.")
	if strings.ContainsAny(sent, "\n\t\x00\x07") {
		t.Fatalf("control characters leaked: %q", sent)
	}
	if !strings.HasPrefix(sent, "line one line two end ") {
		t.Fatalf("whitespace not collapsed: %q", sent[:40])
	}
	if n := len([]rune(sent)); n != 600 {
		t.Fatalf("bounded text has %d runes, want 600", n)
	}
}

func TestResultDeliveryWhitespaceOnlyReplyIsNotSent(t *testing.T) {
	r := newDeliveryRig(t)
	r.setup(t, " \n\t ")
	r.seedCall(t, "s1", "user-1", 1)
	r.run(t)
	if r.posts.Load() != 0 {
		t.Fatal("blank reply must not be sent")
	}
	if status, code, _ := r.row(t, "entry-1"); status != "failed" || code != "empty_result" {
		t.Fatalf("status=%s code=%s", status, code)
	}
}

func TestResultCursorAdvancesMonotonicallyAndSurvivesRestart(t *testing.T) {
	r := newDeliveryRig(t)
	r.seedTask(t, "task-1", "user-1")
	for i, id := range []string{"entry-1", "entry-2", "entry-3"} {
		r.seedEntry(t, "task-1", id, "reply "+id)
		r.seedEvent(t, int64(i+1), "task-1", "run-"+id, id)
	}
	r.seedCall(t, "s1", "user-1", 1)
	r.run(t)
	if r.posts.Load() != 3 {
		t.Fatalf("posts=%d, want 3", r.posts.Load())
	}
	var cursor int64
	if err := r.store.DB().QueryRow(`SELECT last_cursor FROM voice_result_cursor WHERE id=1`).Scan(&cursor); err != nil || cursor != 3 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	restarted := newService(r.store.DB(), Config{GatewayURL: r.svc.base.String(), GatewayAPIKey: "gw-key", HTTPClient: r.svc.client, Now: r.now})
	restarted.consumeResultEvents(context.Background())
	restarted.dispatchResults(context.Background())
	if r.posts.Load() != 3 {
		t.Fatalf("restart re-sent results, posts=%d", r.posts.Load())
	}
	r.seedEntry(t, "task-1", "entry-4", "reply entry-4")
	r.seedEvent(t, 4, "task-1", "run-4", "entry-4")
	restarted.consumeResultEvents(context.Background())
	restarted.dispatchResults(context.Background())
	if r.posts.Load() != 4 {
		t.Fatalf("restarted service must pick up new events, posts=%d", r.posts.Load())
	}
}

func TestFastReplySkippedWhenNoActiveCall(t *testing.T) {
	r := newDeliveryRig(t)
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `INSERT INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) VALUES('fast-none','default','missing',1,'turn','现在没有在处理的任务。','progress','pending',?,?,?)`, at, at, at)
	r.svc.dispatchFastReplies(context.Background())
	var status string
	if err := r.store.DB().QueryRow(`SELECT status FROM voice_fast_replies WHERE id='fast-none'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "skipped" || r.posts.Load() != 0 {
		t.Fatalf("status=%q posts=%d", status, r.posts.Load())
	}
}

func TestFastReplyDeliveryRetriesThenFails(t *testing.T) {
	r := newDeliveryRig(t)
	r.code.Store(503)
	r.seedCall(t, "s-fast", "user-1", 1)
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `INSERT INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) VALUES('fast-fail','default','s-fast',1,'turn','hello','progress','pending',?,?,?)`, at, at, at)
	for i := 0; i < 4; i++ {
		r.svc.dispatchFastReplies(context.Background())
		r.advance(10 * time.Second)
	}
	var status string
	var attempts int
	_ = r.store.DB().QueryRow(`SELECT status,attempts FROM voice_fast_replies WHERE id='fast-fail'`).Scan(&status, &attempts)
	if status != "failed" || attempts != resultDeliveryMaxAttempts || r.posts.Load() != resultDeliveryMaxAttempts {
		t.Fatalf("status=%q attempts=%d posts=%d", status, attempts, r.posts.Load())
	}
}

func TestFastReplyRecomputesSecondsOnRetry(t *testing.T) {
	r := newDeliveryRig(t)
	r.seedCall(t, "s-fast", "user-1", 1)
	at := r.now().Add(-5 * time.Second).Format(time.RFC3339Nano)
	r.seedTask(t, "task-fast", "user-1")
	r.exec(t, `UPDATE tasks SET status='active',started_at=?,updated_at=? WHERE id='task-fast'`, at, at)
	r.exec(t, `INSERT INTO voice_task_links(workspace_id,session_id,generation,turn_id,turn_revision,task_id,linked_by_device_id,command_id,linked_at) VALUES('default','s-fast',1,'old',1,'task-fast','device-1','cmd-fast',?)`, at)
	r.exec(t, `INSERT INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) VALUES('fast-recompute','default','s-fast',1,'turn','stale','progress','pending',?,?,?)`, r.now().Format(time.RFC3339Nano), at, at)
	r.svc.dispatchFastReplies(context.Background())
	if !strings.Contains(r.texts[0], "已经5秒") {
		t.Fatalf("first text=%q", r.texts[0])
	}
}

func TestRegisterIsNeverCalledInsideATransaction(t *testing.T) {
	r := newDeliveryRig(t)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if _, err := r.store.DB().Exec(`UPDATE voice_turns SET content=content WHERE id='turn-lock'`); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"output_text": `{"route":"recall","restatement":""}`})
	}))
	defer model.Close()
	register, err := NewRegister(r.store.DB(), RegisterConfig{Model: RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"}, CompactCharacters: 1000})
	if err != nil {
		t.Fatal(err)
	}
	svc := newService(r.store.DB(), Config{ContextRegister: register, Now: r.now})
	at := r.now().Format(time.RFC3339Nano)
	r.seedCall(t, "s-lock", "user-1", 1)
	r.exec(t, `INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id) VALUES('turn-lock','default','s-lock',1,'turn',1,'is it done',X'01',?, 'cmd')`, at)
	done := make(chan struct{})
	go func() { svc.classifyCommittedTurn("turn-lock"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("register blocked")
	}
	if _, err := r.store.DB().Exec(`UPDATE voice_turns SET content=content WHERE id='turn-lock'`); err != nil {
		t.Fatal(err)
	}
}
