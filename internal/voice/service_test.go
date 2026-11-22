package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const testCreationEvidenceID = "ev_0123456789abcdef0123456789abcdef"

func newVoiceTestService(t *testing.T, cfg Config) (*storage.Store, *Service) {
	t.Helper()
	if cfg.UIControllerInstruction == "" {
		cfg.UIControllerInstruction = testPrompts(t).UIController()
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := RegisterMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store, newService(store.DB(), cfg)
}

func TestResultConsumerEnqueuesFromEventAndAdvancesCursor(t *testing.T) {
	store, svc := newVoiceTestService(t, Config{Now: func() time.Time { return time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC) }})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES('device-1','default','subject-1','test','approved',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO command_receipts(command_id,workspace_id,subject_id,idempotency_key,request_hash,receipt_json,accepted_at) VALUES('cmd-1','default','subject-1','key',X'01','{}',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,result,error,created_by_device_id,command_id,owner_subject_id) VALUES('task-1','default','title','goal','user','default','queued',1,?,?, '', '', 'device-1','cmd-1','subject-1')`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO events(event_id,workspace_id,cursor,schema_version,event_type,occurred_at,aggregate_type,aggregate_id,aggregate_version,payload) VALUES('event-1','default',1,1,'task.assistant_message',?,'task','task-1',1,?)`, now, `{"payload":{"task_id":"task-1","run_id":"run-1","entry_id":"entry-1"}}`); err != nil {
		t.Fatal(err)
	}
	svc.consumeResultEvents(context.Background())
	var status string
	if err := store.DB().QueryRow(`SELECT status FROM voice_result_deliveries WHERE entry_id='entry-1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("status=%q", status)
	}
	var cursor int64
	if err := store.DB().QueryRow(`SELECT last_cursor FROM voice_result_cursor WHERE id=1`).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if cursor != 1 {
		t.Fatalf("cursor=%d", cursor)
	}
}

func TestVoiceTaskAdmissionBindsStableTurnAcrossRevisions(t *testing.T) {
	ctx := context.Background()
	store, svc := newVoiceTestService(t, Config{})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('session-atomic','default','user-1','device-1','request-atomic',X'01',1,'active',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id) VALUES('turn-1','default','session-atomic',1,'stable-turn',1,'authoritative transcript',X'01',?,'turn-command'),('turn-2','default','session-atomic',1,'stable-turn',2,'edited transcript',X'02',?,'turn-command-2')`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES('device-1','default','user-1','test','approved',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id) VALUES('task-1','default','client title','client goal','voice','default','queued',1,?,?,'device-1','create-1','user-1')`, now, now); err != nil {
		t.Fatal(err)
	}
	principal := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}
	ref, _ := json.Marshal(voiceTaskSourceReference{Kind: "voice_turn", SessionID: "session-atomic", Generation: 1, TurnID: "stable-turn", Revision: 1})
	for _, unavailable := range []any{nil, "", "none", " NONE "} {
		if _, err := store.DB().ExecContext(ctx, `UPDATE voice_turns SET request=? WHERE id='turn-1'`, unavailable); err != nil {
			t.Fatal(err)
		}
		tx, err := store.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		request := &control.TaskSourceRequest{Title: "client title", Goal: "client goal", InitialMessage: "client message"}
		events, admissionErr := svc.admitVoiceTask(ctx, tx, principal, "task-1", request, ref, time.Now().UTC())
		_ = tx.Rollback()
		var apiErr *control.APIError
		if !errors.As(admissionErr, &apiErr) || apiErr.Code != "request_unavailable" || len(events) != 0 || request.Goal != "client goal" {
			t.Fatalf("unavailable request %v used raw/client fallback: request=%+v events=%+v err=%v", unavailable, request, events, admissionErr)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE voice_turns SET exits='task',request='Complete resolved request, not the raw transcript'`); err != nil {
		t.Fatal(err)
	}
	request := &control.TaskSourceRequest{Title: "client title", Goal: "client goal", InitialMessage: "client message"}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := svc.admitVoiceTask(ctx, tx, principal, "task-1", request, ref, time.Now().UTC())
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if request.Title != "Complete resolved request, not the raw transcript" || request.Goal != request.Title || request.InitialMessage != request.Goal || len(events) != 1 || events[0].Type != "voice.task.created" {
		t.Fatalf("task not derived from accepted register request: request=%#v events=%#v", request, events)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ref, _ = json.Marshal(voiceTaskSourceReference{Kind: "voice_turn", SessionID: "session-atomic", Generation: 1, TurnID: "stable-turn", Revision: 2})
	otherTask := &control.TaskSourceRequest{Title: "other", Goal: "other", InitialMessage: "other"}
	secondTx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.admitVoiceTask(ctx, secondTx, principal, "task-2", otherTask, ref, time.Now().UTC())
	_ = secondTx.Rollback()
	var apiErr *control.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "voice_turn_already_dispatched" {
		t.Fatalf("later revision created another task for stable turn: err=%v", err)
	}
}

func handoffHTTPServer(t *testing.T) (*httptest.Server, *storage.Store, string) {
	return handoffHTTPServerWithConfig(t, Config{IngestEnabled: true})
}

func handoffHTTPServerWithConfig(t *testing.T, voiceCfg Config) (*httptest.Server, *storage.Store, string) {
	voiceCfg.IngestEnabled = true
	return handoffHTTPServerWithScopes(t, voiceCfg, nil)
}

func handoffHTTPServerWithScopes(t *testing.T, voiceCfg Config, scopes []string) (*httptest.Server, *storage.Store, string) {
	t.Helper()
	if voiceCfg.UIControllerInstruction == "" {
		voiceCfg.UIControllerInstruction = testPrompts(t).UIController()
	}
	if voiceCfg.RegisterConfig == nil && voiceCfg.ContextRegister == nil {
		model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct{ Input string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, request, _ := strings.Cut(body.Input, "Current sentence:\n")
			out, _ := json.Marshal(map[string]any{"route": "task", "request": request})
			_ = json.NewEncoder(w).Encode(map[string]string{"output_text": string(out)})
		}))
		t.Cleanup(model.Close)
		voiceCfg.RegisterConfig = &RegisterConfig{RegisterInstruction: testPrompts(t).Register(), CompactInstruction: testPrompts(t).Compact(), Model: RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"}, CompactModel: RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"}, CompactCharacters: 1000}
		if voiceCfg.Steward == nil {
			voiceCfg.Steward = &fakeSteward{}
		}
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	// Voice migrations are registered by Register below; the schema must exist
	// before the authenticated server starts serving the production route.
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	server, err := control.NewServer(store, control.Config{Profiles: map[string]control.RuntimeProfile{
		"zoe-steward": {ID: "zoe-steward", Harness: "opencode", RuntimeRouteID: "opencode", ModelRouteID: "default", Role: "steward", IsolationPolicy: "opencode-workspace-write-v1", PermissionTier: "read_only_research"},
	}, DefaultProfileID: "zoe-steward", InstanceScopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	if err = Register(server, store, voiceCfg); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err = control.InitializeInstanceAuth(ctx, store, key); err != nil {
		t.Fatal(err)
	}
	if err = server.RegisterRunnerCredential(ctx, "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA", "steward-test-runner", []string{"default"}, []string{"runner:claim"}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(func() { ts.Close(); server.Shutdown(); _ = store.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('handoff-session','default','owner','instance','request',X'01',1,'active',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	return ts, store, key
}

func handoffCommand(t *testing.T, client *http.Client, base, key, idempotency, kind string, payload any) (int, contracts.CommandReceipt, string) {
	t.Helper()
	body, _ := json.Marshal(contracts.CommandRequest{Kind: kind, Payload: mustJSON(payload)})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/commands", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Idempotency-Key", idempotency)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var receipt contracts.CommandReceipt
	_ = json.Unmarshal(raw, &receipt)
	return resp.StatusCode, receipt, string(raw)
}

func mustJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func TestVoiceHandoffHTTPLowImpactUsesStewardTaskAdmission(t *testing.T) {
	ts, store, _ := handoffHTTPServer(t)
	receipt := delegateTurn(t, ts, store, "turn-low", "research the current release notes")
	var source, profile, taskStatus string
	if err := store.DB().QueryRow(`SELECT source,profile_id,status FROM tasks WHERE id=?`, receipt.ResourceID).Scan(&source, &profile, &taskStatus); err != nil {
		t.Fatal(err)
	}
	if source != "voice" || profile != "zoe-steward" || taskStatus != "queued" {
		t.Fatalf("task admission source=%q profile=%q status=%q", source, profile, taskStatus)
	}
}

func TestHandoffDefaultFixtureAdmitsTask(t *testing.T) {
	ts, store, _ := handoffHTTPServer(t)
	receipt := delegateTurn(t, ts, store, "no-classifier", "is it done")
	var count int
	_ = store.DB().QueryRow(`SELECT count(*) FROM tasks WHERE id=?`, receipt.ResourceID).Scan(&count)
	if count != 1 {
		t.Fatalf("tasks=%d", count)
	}
}

func TestHandoffFixturesIsolateRetainedConnection(t *testing.T) {
	// database/sql.Close does not close a connection still held by a worker.
	// Keep one across fixture cleanup to deterministically model that overlap.
	parent := t
	t.Run("previous fixture", func(t *testing.T) {
		_, store, _ := handoffHTTPServer(t)
		conn, err := store.DB().Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// The parent cleanup runs after the next fixture, unlike this subtest's
		// server/store cleanup, so the previous SQLite database remains alive.
		parent.Cleanup(func() { _ = conn.Close() })
	})
	ts, store, _ := handoffHTTPServer(t)
	receipt := delegateTurn(t, ts, store, "isolated-fixture", "research release notes")
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM tasks WHERE id=?`, receipt.ResourceID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("tasks=%d, want 1", count)
	}
}

func TestVoiceTaskLinkRequiresSameDeviceVoiceTaskAndExposesTaskEvents(t *testing.T) {
	ctx := context.Background()
	store, svc := newVoiceTestService(t, Config{})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO actors(id,workspace_id,subject_id,name,status,version,created_at) VALUES('device-1','default','user-1','test','approved',1,?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('session-1','default','user-1','device-1','request-1',X'01',1,'ended',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id) VALUES('turn-row','default','session-1',1,'turn-a',1,'confirmed text',X'01',?,'voice-turn-cmd')`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,accepted_at,created_by_device_id,command_id) VALUES('task-1','default','title','goal','voice','default','queued',1,?,?,?,'device-1','task-create-cmd')`, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO events(event_id,workspace_id,cursor,schema_version,event_type,occurred_at,subject_id,aggregate_type,aggregate_id,aggregate_version,payload) VALUES('task-event','default',1,1,'task.created',?,'user-1','task','task-1',1,'{}')`, now); err != nil {
		t.Fatal(err)
	}
	principal := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1", Scopes: []string{"voice:turn:commit"}}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	version := int64(1)
	payload, _ := json.Marshal(linkTaskPayload{SessionID: "session-1", Generation: 1, TurnID: "turn-a", TurnRevision: 1, TaskID: "task-1"})
	result, events, err := svc.commandLinkTask(ctx, tx, control.CommandContext{CommandID: "link-command", Principal: principal, AcceptedAt: time.Now().UTC()}, contracts.CommandRequest{ExpectedVersion: &version, Payload: payload})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if result.ResourceID != "task-1" || len(events) != 1 || events[0].Type != "voice.task.linked" {
		t.Fatalf("voice task link result=%#v events=%#v", result, events)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var linkedEvents int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM voice_task_links l JOIN events e ON e.workspace_id=l.workspace_id AND e.aggregate_type='task' AND e.aggregate_id=l.task_id WHERE l.session_id='session-1' AND e.event_id='task-event'`).Scan(&linkedEvents); err != nil {
		t.Fatal(err)
	}
	if linkedEvents != 1 {
		t.Fatalf("linked authoritative task events=%d, want 1", linkedEvents)
	}
	other := principal
	other.DeviceID = "other-device"
	otherTx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer otherTx.Rollback()
	if _, _, err := svc.commandLinkTask(ctx, otherTx, control.CommandContext{CommandID: "bad-link", Principal: other, AcceptedAt: time.Now().UTC()}, contracts.CommandRequest{ExpectedVersion: &version, Payload: payload}); err == nil {
		t.Fatal("another device linked a voice turn to the task")
	}
}

func TestQueryGatewayOutcomeLeavesNonNeverEstablishedUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
		want   string
	}{
		{name: "open", status: http.StatusOK, body: `{"request_id":"req","outcome":"open"}`, want: "open"},
		{name: "released", status: http.StatusOK, body: `{"request_id":"req","outcome":"released"}`, want: "released"},
		{name: "unknown", status: http.StatusOK, body: `{"request_id":"req","outcome":"unknown"}`, want: "unknown"},
		{name: "not found", status: http.StatusNotFound, want: ""},
		{name: "malformed", status: http.StatusOK, body: `{`, want: ""},
		{name: "connection error", err: errors.New("connection failed"), want: ""},
		{name: "query timeout", err: context.DeadlineExceeded, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, svc := newVoiceTestService(t, Config{GatewayURL: "https://gateway.test", GatewayAPIKey: "key"})
			svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}
			got := svc.queryGatewayOutcome(context.Background(), "req")
			if got.Outcome != tc.want {
				t.Fatalf("outcome=%q, want %q", got.Outcome, tc.want)
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,last_error_code,gateway_request_id,created_at,updated_at) VALUES('query','default','owner','device','query',X'01',1,'quarantined','gateway_outcome_unknown','req',?,?)`, now, now); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().Exec(`UPDATE voice_pool_state SET blocked=1,reason_code='gateway_outcome_unknown',source_session_id='query'`); err != nil {
				t.Fatal(err)
			}
			if err := svc.resolveGatewayCreateOutcome(context.Background(), "query", 1, "quarantined", "req", got); err != nil {
				t.Fatal(err)
			}
			var state string
			var blocked, count int
			if err := store.DB().QueryRow(`SELECT state,(SELECT blocked FROM voice_pool_state),(SELECT COUNT(*) FROM voice_pool_reconciliations) FROM voice_sessions WHERE id='query'`).Scan(&state, &blocked, &count); err != nil {
				t.Fatal(err)
			}
			if state != "quarantined" || blocked != 1 || count != 0 {
				t.Fatalf("state=%s blocked=%d audit=%d", state, blocked, count)
			}
		})
	}
}

func TestGatewayAttestationSweepResolvesOldAllocatingOnlyOnce(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	store, svc := newVoiceTestService(t, Config{Now: func() time.Time { return now }, GatewayURL: "https://gateway.test", GatewayAPIKey: "key"})
	svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"request_id":"req-old","outcome":"never_established"}`)), Header: make(http.Header)}, nil
	})}
	old := now.Add(-3 * time.Minute).Format(time.RFC3339Nano)
	young := now.Add(-time.Minute).Format(time.RFC3339Nano)
	for _, row := range []struct{ id, owner, at string }{{"old", "old-owner", old}, {"young", "young-owner", young}} {
		_, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at,gateway_request_id) VALUES(?,?,?,'device',?,X'01',1,'allocating',?,?,?)`, row.id, "default", row.owner, row.id, row.at, row.at, "req-old")
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`UPDATE voice_pool_state SET blocked=1,reason_code='negotiation_in_progress',source_session_id='old' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	svc.attestStaleCreates(context.Background())
	svc.attestStaleCreates(context.Background())
	var oldState, youngState string
	var blocked, rows int
	if err := store.DB().QueryRow(`SELECT state FROM voice_sessions WHERE id='old'`).Scan(&oldState); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT state FROM voice_sessions WHERE id='young'`).Scan(&youngState); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT blocked FROM voice_pool_state`).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM voice_pool_reconciliations WHERE method='gateway_attestation'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if oldState != "failed" || youngState != "allocating" || blocked != 0 || rows != 1 {
		t.Fatalf("old=%s young=%s blocked=%d reconciliations=%d", oldState, youngState, blocked, rows)
	}
}

func TestGatewayAttestationDoesNotOverwriteConcurrentStateChange(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	store, svc := newVoiceTestService(t, Config{Now: func() time.Time { return now }, GatewayURL: "https://gateway.test", GatewayAPIKey: "key"})
	at := now.Add(-3 * time.Minute).Format(time.RFC3339Nano)
	_, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at,gateway_request_id) VALUES('race','default','race-owner','device','race-request',X'01',1,'allocating',?,?,?)`, at, at, "req-race")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE voice_pool_state SET blocked=1,reason_code='negotiation_in_progress',source_session_id='race' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if _, err := store.DB().Exec(`UPDATE voice_sessions SET state='active' WHERE id='race'`); err != nil {
			t.Fatalf("concurrent state change: %v", err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"request_id":"req-race","outcome":"never_established"}`)), Header: make(http.Header)}, nil
	})}
	svc.attestStaleCreates(context.Background())
	var state string
	var rows int
	if err := store.DB().QueryRow(`SELECT state FROM voice_sessions WHERE id='race'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM voice_pool_reconciliations`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if state != "active" || rows != 0 {
		t.Fatalf("state=%s reconciliations=%d", state, rows)
	}
}

func TestGatewayAttestationRequiresMatchingPoolSource(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	store, svc := newVoiceTestService(t, Config{})
	_, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('attested','default','owner','device','request',X'01',1,'quarantined',?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET last_error_code='gateway_outcome_unknown',gateway_request_id='attested-request' WHERE id='attested'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE voice_pool_state SET blocked=1,reason_code='gateway_outcome_unknown',source_session_id='other' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := svc.resolveGatewayCreateOutcome(context.Background(), "attested", 1, "quarantined", "attested-request", gatewayCreationOutcome{Outcome: "never_established"}); err != nil {
		t.Fatal(err)
	}
	var state string
	var blocked, rows int
	if err := store.DB().QueryRow(`SELECT state FROM voice_sessions WHERE id='attested'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT blocked FROM voice_pool_state`).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM voice_pool_reconciliations`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if state != "quarantined" || blocked != 1 || rows != 0 {
		t.Fatalf("state=%s blocked=%d reconciliations=%d", state, blocked, rows)
	}

}

func TestGatewayAttestationSweepLeavesNonNeverEstablishedOutcomesUntouched(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{name: "open", status: http.StatusOK, body: `{"request_id":"req-sweep","outcome":"open"}`},
		{name: "released", status: http.StatusOK, body: `{"request_id":"req-sweep","outcome":"released"}`},
		{name: "unknown", status: http.StatusOK, body: `{"request_id":"req-sweep","outcome":"unknown"}`},
		{name: "not found", status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
			store, svc := newVoiceTestService(t, Config{Now: func() time.Time { return now }, GatewayURL: "https://gateway.test", GatewayAPIKey: "key"})
			at := now.Add(-3 * time.Minute).Format(time.RFC3339Nano)
			if _, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at,gateway_request_id) VALUES('sweep','default','owner','device','sweep-request',X'01',1,'allocating',?,?,?)`, at, at, "req-sweep"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().Exec(`UPDATE voice_pool_state SET blocked=1,reason_code='negotiation_in_progress',source_session_id='sweep' WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}
			svc.attestStaleCreates(context.Background())
			var state, reason string
			var blocked, rows int
			if err := store.DB().QueryRow(`SELECT state,last_error_code FROM voice_sessions WHERE id='sweep'`).Scan(&state, &reason); err != nil {
				t.Fatal(err)
			}
			if err := store.DB().QueryRow(`SELECT blocked FROM voice_pool_state`).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if err := store.DB().QueryRow(`SELECT COUNT(*) FROM voice_pool_reconciliations`).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if state != "allocating" || reason != "" || blocked != 1 || rows != 0 {
				t.Fatalf("state=%s reason=%s blocked=%d reconciliations=%d", state, reason, blocked, rows)
			}
		})
	}
}
