package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

type testAccountMedia struct {
	verified bool
	mu       sync.Mutex
	calls    int
}

type roundTripFunc func(*http.Request) (*http.Response, error)

type fakeClassifier struct {
	category string
	err      error
	delay    time.Duration
	calls    atomic.Int32
}

func (f *fakeClassifier) Classify(ctx context.Context, text string) (string, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(f.delay):
		}
	}
	return f.category, f.err
}

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (m *testAccountMedia) Capabilities() AccountMediaCapabilities {
	return AccountMediaCapabilities{AccountLeaseIdentity: true, AccountScopedTermination: true, TrustedTerminationEvidence: true}
}

func (m *testAccountMedia) ReleaseAccountLease(context.Context, AccountLeaseReference, string) (AccountTerminationObservation, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	if !m.verified {
		return AccountTerminationObservation{}, nil
	}
	return AccountTerminationObservation{AccountID: "account", LeaseID: "lease", LeaseGeneration: 2, SessionID: "gateway", SessionGeneration: 3, Terminated: true, EvidenceID: "evidence", ObservedAt: time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)}, nil
}

func insertReaperSession(t *testing.T, store *storage.Store, id, heartbeat, created string) {
	t.Helper()
	var heartbeatValue any = heartbeat
	if heartbeat == "" {
		heartbeatValue = nil
	}
	_, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,gateway_session_id,generation,state,offer_answer,created_at,updated_at,heartbeat_at,account_lease_id,account_lease_generation,gateway_media_generation) VALUES(?,?,?,?,?,X'01',?,1,'active','',?,?,?, 'lease',2,3)`, id, "default", "user-"+id, "device", id, "gateway", created, created, heartbeatValue)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().Exec(`UPDATE voice_pool_state SET blocked=1,reason_code='active_session',source_session_id=? WHERE id=1`, id)
	if err != nil {
		t.Fatal(err)
	}
}

func reapWithTimeout(t *testing.T, svc *Service) {
	t.Helper()
	done := make(chan struct{})
	go func() { svc.reapStale(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reapStale timed out; likely held the sole SQLite connection")
	}
}

func TestVoiceReaperReclaimsVerifiedStaleSession(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 1, 0, 0, time.UTC)
	media := &testAccountMedia{verified: true}
	store, svc := newVoiceTestService(t, Config{HeartbeatTimeout: 30 * time.Second, Now: func() time.Time { return now }, GatewayURL: "https://gateway.test", GatewayAPIKey: "key", AccountMediaLifecycle: media})
	var deletes int
	svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deletes++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	insertReaperSession(t, store, "stale", now.Add(-time.Minute).Format(time.RFC3339Nano), now.Add(-time.Minute).Format(time.RFC3339Nano))
	reapWithTimeout(t, svc)
	var state string
	var blocked int
	if err := store.DB().QueryRow(`SELECT state FROM voice_sessions WHERE id='stale'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT blocked FROM voice_pool_state`).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	if state != "ended" || blocked != 0 || deletes != 1 {
		t.Fatalf("state=%s blocked=%d deletes=%d", state, blocked, deletes)
	}
}

func TestVoiceReaperQuarantinesUnverifiedAndDoesNotDeleteAgain(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 1, 0, 0, time.UTC)
	store, svc := newVoiceTestService(t, Config{HeartbeatTimeout: 30 * time.Second, Now: func() time.Time { return now }, GatewayURL: "https://gateway.test", GatewayAPIKey: "key"})
	deletes := 0
	svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deletes++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	insertReaperSession(t, store, "unverified", now.Add(-time.Minute).Format(time.RFC3339Nano), now.Add(-time.Minute).Format(time.RFC3339Nano))
	reapWithTimeout(t, svc)
	reapWithTimeout(t, svc)
	var state string
	var blocked int
	_ = store.DB().QueryRow(`SELECT state FROM voice_sessions WHERE id='unverified'`).Scan(&state)
	_ = store.DB().QueryRow(`SELECT blocked FROM voice_pool_state`).Scan(&blocked)
	if state != "quarantined" || blocked != 1 || deletes != 1 {
		t.Fatalf("state=%s blocked=%d deletes=%d", state, blocked, deletes)
	}
}

func TestVoiceReaperHeartbeatFallbackAndTimestampOrdering(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 30, 450000000, time.UTC)
	store, svc := newVoiceTestService(t, Config{HeartbeatTimeout: 30 * time.Second, Now: func() time.Time { return now }, GatewayURL: "https://gateway.test", GatewayAPIKey: "key"})
	svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	// .5Z is newer than the .45Z cutoff despite differing fractional widths.
	insertReaperSession(t, store, "fraction", "2026-10-31T10:00:00.5Z", now.Add(-time.Minute).Format(time.RFC3339Nano))
	insertReaperSession(t, store, "null-heartbeat", "", now.Add(-time.Minute).Format(time.RFC3339Nano))
	insertReaperSession(t, store, "fresh", now.Add(-time.Second).Format(time.RFC3339Nano), now.Add(-time.Minute).Format(time.RFC3339Nano))
	reapWithTimeout(t, svc)
	var fractionState, nullState, freshState string
	_ = store.DB().QueryRow(`SELECT state FROM voice_sessions WHERE id='fraction'`).Scan(&fractionState)
	_ = store.DB().QueryRow(`SELECT state FROM voice_sessions WHERE id='null-heartbeat'`).Scan(&nullState)
	_ = store.DB().QueryRow(`SELECT state FROM voice_sessions WHERE id='fresh'`).Scan(&freshState)
	if fractionState != "active" || nullState != "quarantined" || freshState != "active" {
		t.Fatalf("fraction=%s null=%s fresh=%s", fractionState, nullState, freshState)
	}
}

func TestVoiceReaperRaceReleasesOnceAndLateHeartbeatConflicts(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 1, 0, 0, time.UTC)
	store, svc := newVoiceTestService(t, Config{HeartbeatTimeout: 30 * time.Second, Now: func() time.Time { return now }, GatewayURL: "https://gateway.test", GatewayAPIKey: "key"})
	deletes := 0
	var mu sync.Mutex
	svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		deletes++
		mu.Unlock()
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	insertReaperSession(t, store, "race", now.Add(-time.Minute).Format(time.RFC3339Nano), now.Add(-time.Minute).Format(time.RFC3339Nano))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); reapWithTimeout(t, svc) }()
	go func() {
		defer wg.Done()
		_, _, _, _ = svc.releaseSession(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user"}, "race", 1, "gateway", "lease", 2, 3, 0, 0, false)
	}()
	wg.Wait()
	mu.Lock()
	count := deletes
	mu.Unlock()
	if count != 1 {
		t.Fatalf("gateway deletes=%d, want 1", count)
	}
	result, err := store.DB().Exec(`UPDATE voice_sessions SET heartbeat_at=? WHERE id='race' AND state='active'`, now.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	changed, _ := result.RowsAffected()
	if changed != 0 {
		t.Fatalf("late heartbeat changed %d rows", changed)
	}
}

func newVoiceTestService(t *testing.T, cfg Config) (*storage.Store, *Service) {
	t.Helper()
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

func TestVoiceHeartbeatTimeoutIsClamped(t *testing.T) {
	store, svc := newVoiceTestService(t, Config{HeartbeatTimeout: time.Second})
	if svc.heartbeatTimeout != 30*time.Second {
		t.Fatalf("short heartbeat timeout=%s, want 30s", svc.heartbeatTimeout)
	}
	_ = store
	_, svc = newVoiceTestService(t, Config{HeartbeatTimeout: 601 * time.Second})
	if svc.heartbeatTimeout != 600*time.Second {
		t.Fatalf("long heartbeat timeout=%s, want 600s", svc.heartbeatTimeout)
	}
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

func TestVoiceTurnCommitRequiresConfirmationAndDeduplicatesStableRevision(t *testing.T) {
	ctx := context.Background()
	store, svc := newVoiceTestService(t, Config{})
	_, err := store.DB().ExecContext(ctx, `INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('session-1','default','user-1','device-1','request-1',X'01',1,'active',?,?)`, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	principal := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1", Scopes: []string{"voice:turn:commit"}}
	command := func(confirmed bool, text string) (contracts.CommandResult, []contracts.Event, error) {
		tx, err := store.DB().BeginTx(ctx, nil)
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
		payload, _ := json.Marshal(commitTurnPayload{SessionID: "session-1", Generation: 1, TurnID: "turn-a", Revision: 2, ProviderMessage: "provider-message-a", Content: text, UserConfirmed: confirmed})
		result, events, err := svc.commandCommitTurn(ctx, tx, control.CommandContext{CommandID: "command-a", Principal: principal, AcceptedAt: time.Now().UTC()}, contracts.CommandRequest{Payload: payload})
		if err != nil {
			_ = tx.Rollback()
			return result, events, err
		}
		if err := tx.Commit(); err != nil {
			return result, events, err
		}
		return result, events, nil
	}
	if _, _, err := command(false, "请创建一个任务"); err == nil {
		t.Fatal("unconfirmed voice text was accepted as a committed turn")
	}
	first, events, err := command(true, "请创建一个任务")
	if err != nil || first.ResourceID == "" || len(events) != 1 || events[0].Type != "voice.turn.committed" {
		t.Fatalf("confirmed turn not durably proposed: result=%#v events=%#v err=%v", first, events, err)
	}
	second, events, err := command(true, "请创建一个任务")
	if err != nil || second.ResourceID != first.ResourceID || len(events) != 0 {
		t.Fatalf("duplicate stable revision was not deduplicated: result=%#v events=%#v err=%v", second, events, err)
	}
	if _, _, err := command(true, "请创建另一个任务"); err == nil {
		t.Fatal("same turn revision with different content did not conflict")
	}
	var tasks int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if tasks != 0 {
		t.Fatalf("voice turn directly created %d task(s); task.create must remain a separate command", tasks)
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
	if request.Title != "authoritative transcript" || request.Goal != "authoritative transcript" || request.InitialMessage != "authoritative transcript" || len(events) != 1 || events[0].Type != "voice.task.created" {
		t.Fatalf("task not derived from confirmed transcript: request=%#v events=%#v", request, events)
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
	return handoffHTTPServerWithConfig(t, Config{})
}

func handoffHTTPServerWithConfig(t *testing.T, voiceCfg Config) (*httptest.Server, *storage.Store, string) {
	t.Helper()
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
	}, DefaultProfileID: "zoe-steward"})
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

func TestVoiceHandoffHTTPRejectsReplayConflictAndStaleGeneration(t *testing.T) {
	ts, store, key := handoffHTTPServer(t)
	content := "research the current release notes"
	hash := sha256.Sum256([]byte(content))
	_, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "commit-1", "voice.turn.commit", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "turn-1", "revision": 1, "content": content, "user_confirmed": true})
	if strings.Contains(raw, `"status":"accepted"`) == false {
		t.Fatalf("commit failed: %s", raw)
	}
	payload := map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "turn-1", "revision": 1, "content_hash": fmt.Sprintf("%x", hash[:])}
	status, first, raw := handoffCommand(t, ts.Client(), ts.URL, key, "handoff-1", "voice.turn.handoff", payload)
	if status != http.StatusOK || first.ResourceID == "" {
		t.Fatalf("handoff failed: status=%d body=%s", status, raw)
	}
	status, second, raw := handoffCommand(t, ts.Client(), ts.URL, key, "handoff-1", "voice.turn.handoff", payload)
	if status != http.StatusOK || second.ResourceID != first.ResourceID {
		t.Fatalf("idempotent replay changed receipt: status=%d body=%s", status, raw)
	}
	payload["content_hash"] = strings.Repeat("0", 64)
	if status, _, _ = handoffCommand(t, ts.Client(), ts.URL, key, "handoff-conflict", "voice.turn.handoff", payload); status != http.StatusConflict {
		t.Fatalf("conflicting hash status=%d, want 409", status)
	}
	_, _ = store.DB().Exec(`UPDATE voice_sessions SET state='ended',generation=2 WHERE id='handoff-session'`)
	payload["generation"] = 1
	payload["content_hash"] = fmt.Sprintf("%x", hash[:])
	if status, _, _ = handoffCommand(t, ts.Client(), ts.URL, key, "handoff-stale", "voice.turn.handoff", payload); status != http.StatusConflict {
		t.Fatalf("stale generation status=%d, want 409", status)
	}
}

func TestVoiceHandoffHTTPLowImpactUsesStewardTaskAdmission(t *testing.T) {
	ts, store, key := handoffHTTPServer(t)
	content := "research the current release notes"
	hash := sha256.Sum256([]byte(content))
	if status, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "commit-low", "voice.turn.commit", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "turn-low", "revision": 1, "content": content, "user_confirmed": true}); status != http.StatusOK {
		t.Fatalf("commit status=%d body=%s", status, raw)
	}
	status, receipt, raw := handoffCommand(t, ts.Client(), ts.URL, key, "handoff-low", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "turn-low", "revision": 1, "content_hash": fmt.Sprintf("%x", hash[:])})
	if status != http.StatusOK {
		t.Fatalf("handoff status=%d body=%s", status, raw)
	}
	var source, profile, taskStatus string
	if err := store.DB().QueryRow(`SELECT source,profile_id,status FROM tasks WHERE id=?`, receipt.ResourceID).Scan(&source, &profile, &taskStatus); err != nil {
		t.Fatal(err)
	}
	if source != "voice" || profile != "zoe-steward" || taskStatus != "queued" {
		t.Fatalf("task admission source=%q profile=%q status=%q", source, profile, taskStatus)
	}
}

func TestVoiceHandoffHTTPFailClosedForHighImpactAndAmbiguousRequests(t *testing.T) {
	for _, tc := range []struct{ name, text string }{{"high", "delete the old project"}, {"ambiguous", "please handle this"}} {
		t.Run(tc.name, func(t *testing.T) {
			ts, store, key := handoffHTTPServer(t)
			hash := sha256.Sum256([]byte(tc.text))
			if status, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "commit-"+tc.name, "voice.turn.commit", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "turn-" + tc.name, "revision": 1, "content": tc.text, "user_confirmed": true}); status != http.StatusOK {
				t.Fatalf("commit status=%d body=%s", status, raw)
			}
			status, receipt, raw := handoffCommand(t, ts.Client(), ts.URL, key, "handoff-"+tc.name, "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "turn-" + tc.name, "revision": 1, "content_hash": fmt.Sprintf("%x", hash[:])})
			if status != http.StatusOK {
				t.Fatalf("handoff status=%d body=%s", status, raw)
			}
			var taskStatus, interaction string
			if err := store.DB().QueryRow(`SELECT status,interaction_state FROM tasks WHERE id=?`, receipt.ResourceID).Scan(&taskStatus, &interaction); err != nil {
				t.Fatal(err)
			}
			if taskStatus != "waiting" || interaction != "explicit_confirmation" {
				t.Fatalf("request bypassed confirmation: status=%q interaction=%q", taskStatus, interaction)
			}
			appendStatus, _, appendRaw := handoffCommand(t, ts.Client(), ts.URL, key, "append-"+tc.name, "task.append", map[string]any{"task_id": receipt.ResourceID, "content": "yes, proceed"})
			if appendStatus != http.StatusConflict || !strings.Contains(appendRaw, `"code":"confirmation_invalid"`) {
				t.Fatalf("ordinary task.append bypassed scoped confirmation: status=%d body=%s", appendStatus, appendRaw)
			}
			var entryCount int
			if err := store.DB().QueryRow(`SELECT count(*) FROM task_entries WHERE task_id=? AND content='yes, proceed'`, receipt.ResourceID).Scan(&entryCount); err != nil {
				t.Fatal(err)
			}
			if entryCount != 0 {
				t.Fatalf("rejected confirmation append persisted %d entry/entries", entryCount)
			}
		})
	}
}

func commitAndHandoff(t *testing.T, ts *httptest.Server, key, id, content string) contracts.CommandReceipt {
	return commitAndHandoffSession(t, ts, key, "handoff-session", id, content)
}

func commitAndHandoffSession(t *testing.T, ts *httptest.Server, key, session, id, content string) contracts.CommandReceipt {
	t.Helper()
	hash := sha256.Sum256([]byte(content))
	if status, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "commit-"+id, "voice.turn.commit", map[string]any{"session_id": session, "generation": 1, "turn_id": id, "revision": 1, "content": content, "user_confirmed": true}); status != http.StatusOK {
		t.Fatalf("commit status=%d body=%s", status, raw)
	}
	status, receipt, raw := handoffCommand(t, ts.Client(), ts.URL, key, "handoff-"+id, "voice.turn.handoff", map[string]any{"session_id": session, "generation": 1, "turn_id": id, "revision": 1, "content_hash": fmt.Sprintf("%x", hash[:])})
	if status != http.StatusOK {
		t.Fatalf("handoff status=%d body=%s", status, raw)
	}
	return receipt
}

func TestHandoffProgressCategoryCreatesNoTaskAndDeliversPlainReply(t *testing.T) {
	classifier := &fakeClassifier{category: "progress"}
	posts := atomic.Int32{}
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(body["text"], "Zoe result") {
			t.Errorf("wrapper leaked: %q", body["text"])
		}
		if body["text"] != "现在没有在处理的任务。" {
			t.Errorf("text=%q", body["text"])
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: classifier, GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client()})
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id='gateway-handoff' WHERE id='handoff-session'`); err != nil {
		t.Fatal(err)
	}
	commitAndHandoff(t, ts, key, "progress-order", "is it done yet")
	var tasks, replies int
	_ = store.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks)
	_ = store.DB().QueryRow(`SELECT count(*) FROM voice_fast_replies`).Scan(&replies)
	if tasks != 0 || replies != 1 {
		t.Fatalf("tasks=%d replies=%d", tasks, replies)
	}
	newService(store.DB(), Config{GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client(), Now: time.Now}).dispatchFastReplies(context.Background())
	if posts.Load() != 1 {
		t.Fatalf("gateway posts=%d", posts.Load())
	}
}

func TestHandoffProgressReportsRunningTaskSecondsFromTaskTable(t *testing.T) {
	now := time.Date(2026, 11, 6, 1, 0, 10, 0, time.UTC)
	classifier := &fakeClassifier{category: "progress"}
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: classifier, Now: func() time.Time { return now }})
	at := now.Add(-10 * time.Second).Format(time.RFC3339Nano)
	_, _ = store.DB().Exec(`INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES('task-device','default','owner','task','approved',?)`, at)
	_, _ = store.DB().Exec(`INSERT INTO command_receipts(command_id,workspace_id,subject_id,idempotency_key,request_hash,receipt_json,accepted_at) VALUES('task-cmd','default','owner','task-key',X'01','{}',?)`, at)
	_, _ = store.DB().Exec(`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,started_at,created_by_device_id,command_id,owner_subject_id) VALUES('running','default','running','running','voice','zoe-steward','active',1,?,?,?,?,?,?)`, at, at, at, "task-device", "task-cmd", "owner")
	_, _ = store.DB().Exec(`INSERT INTO voice_task_links(workspace_id,session_id,generation,turn_id,turn_revision,task_id,linked_by_device_id,command_id,linked_at) VALUES('default','handoff-session',1,'old',1,'running','instance','task-cmd',?)`, at)
	commitAndHandoff(t, ts, key, "progress-running", "is it done")
	var text string
	if err := store.DB().QueryRow(`SELECT text FROM voice_fast_replies`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "还在处理,已经10秒了。" {
		t.Fatalf("text=%q", text)
	}
}

func TestHandoffProgressWithoutRecentTaskSaysNothingRunning(t *testing.T) {
	classifier := &fakeClassifier{category: "progress"}
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: classifier})
	commitAndHandoff(t, ts, key, "progress-none", "is it done")
	var text string
	_ = store.DB().QueryRow(`SELECT text FROM voice_fast_replies`).Scan(&text)
	if text != "现在没有在处理的任务。" {
		t.Fatalf("text=%q", text)
	}
}

func TestHandoffTaskRecallMemoryAndUnknownCategoriesStillCreateStewardTask(t *testing.T) {
	for _, category := range []string{"task", "recall", "memory", "unknown"} {
		t.Run(category, func(t *testing.T) {
			classifier := &fakeClassifier{category: category}
			ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: classifier})
			receipt := commitAndHandoff(t, ts, key, "category-"+category, "please handle this")
			var count int
			_ = store.DB().QueryRow(`SELECT count(*) FROM tasks WHERE id=?`, receipt.ResourceID).Scan(&count)
			if count != 1 {
				t.Fatalf("tasks=%d", count)
			}
		})
	}
}

func TestHandoffClassifierErrorOrTimeoutFallsBackToSlowPath(t *testing.T) {
	for i, classifier := range []*fakeClassifier{{err: errors.New("classifier failed")}, {delay: 2 * time.Second, category: "progress"}} {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: classifier})
			receipt := commitAndHandoff(t, ts, key, fmt.Sprintf("classifier-failure-%d", i), "please handle this")
			var count int
			_ = store.DB().QueryRow(`SELECT count(*) FROM tasks WHERE id=?`, receipt.ResourceID).Scan(&count)
			if count != 1 {
				t.Fatalf("tasks=%d", count)
			}
		})
	}
}

func TestHandoffWithoutClassifierBehavesAsBefore(t *testing.T) {
	ts, store, key := handoffHTTPServer(t)
	receipt := commitAndHandoff(t, ts, key, "no-classifier", "is it done")
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
	ts, store, key := handoffHTTPServer(t)
	receipt := commitAndHandoff(t, ts, key, "isolated-fixture", "research release notes")
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM tasks WHERE id=?`, receipt.ResourceID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("tasks=%d, want 1", count)
	}
}

func TestFailedClassificationIsNotRetriedEveryTick(t *testing.T) {
	classifier := &fakeClassifier{err: errors.New("no")}
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: classifier})
	commitAndHandoff(t, ts, key, "failed-once", "please handle this")
	var category string
	_ = store.DB().QueryRow(`SELECT triage_category FROM voice_turns WHERE turn_id='failed-once'`).Scan(&category)
	if category != "unclassified" || classifier.calls.Load() != 1 {
		t.Fatalf("category=%q calls=%d", category, classifier.calls.Load())
	}
	newService(store.DB(), Config{Classifier: classifier}).classifyCommittedTurns(context.Background())
	if classifier.calls.Load() != 1 {
		t.Fatalf("failed classification retried: calls=%d", classifier.calls.Load())
	}
}

func TestTriageCacheEntryIsRemovedAfterHandoffConsumesIt(t *testing.T) {
	_, svc := newVoiceTestService(t, Config{Now: func() time.Time { return time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC) }})
	key := triageKey("session", 1, "turn", 1)
	svc.cacheTriage(key, "progress")
	if got := svc.waitForTriageCache(context.Background(), "session", 1, "turn", 1); got != "progress" {
		t.Fatalf("category=%q", got)
	}
	svc.triageMu.RLock()
	_, present := svc.triage[key]
	svc.triageMu.RUnlock()
	if present {
		t.Fatal("consumed triage cache entry remains")
	}
}

func TestTriageCacheIsBoundedWhenHandoffNeverArrives(t *testing.T) {
	_, svc := newVoiceTestService(t, Config{})
	for i := 0; i < triageCacheCap+17; i++ {
		svc.cacheTriage(fmt.Sprintf("turn-%d", i), "task")
	}
	svc.triageMu.RLock()
	size := len(svc.triage)
	svc.triageMu.RUnlock()
	if size != triageCacheCap {
		t.Fatalf("cache size=%d cap=%d", size, triageCacheCap)
	}
}

func TestHandoffUsesPersistedCategoryWhenCacheEntryIsMissing(t *testing.T) {
	classifier := &fakeClassifier{category: "task"}
	store, svc := newVoiceTestService(t, Config{Classifier: classifier})
	at := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('persisted-session','default','owner','device','persisted-request',X'01',1,'active',?,?)`, at, at)
	_, _ = store.DB().Exec(`INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,triage_category) VALUES('persisted-turn','default','persisted-session',1,'persisted-category',1,'is it done',X'01',?,'cmd','task')`, at)
	if got := svc.ensureClassified(context.Background(), "default", "persisted-session", 1, "persisted-category", 1); got != "task" {
		t.Fatalf("persisted category=%q", got)
	}
	if classifier.calls.Load() != 0 {
		t.Fatal("persisted category lookup called classifier")
	}
}

func TestHandoffHighImpactIgnoresClassifier(t *testing.T) {
	classifier := &fakeClassifier{category: "progress"}
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: classifier})
	receipt := commitAndHandoff(t, ts, key, "high-impact", "delete all files")
	var status, interaction string
	_ = store.DB().QueryRow(`SELECT status,interaction_state FROM tasks WHERE id=?`, receipt.ResourceID).Scan(&status, &interaction)
	if status != "waiting" || interaction != "explicit_confirmation" {
		t.Fatalf("status=%q interaction=%q", status, interaction)
	}
	var replies int
	_ = store.DB().QueryRow(`SELECT count(*) FROM voice_fast_replies`).Scan(&replies)
	if replies != 0 {
		t.Fatalf("fast replies=%d", replies)
	}
}

func TestHandoffReplayIsIdempotentAcrossRoutes(t *testing.T) {
	t.Run("progress", func(t *testing.T) {
		classifier := &fakeClassifier{category: "progress"}
		posts := atomic.Int32{}
		gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1); w.WriteHeader(http.StatusOK) }))
		defer gateway.Close()
		ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: classifier, GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client()})
		_, _ = store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id='gateway-replay' WHERE id='handoff-session'`)
		receipt := commitAndHandoff(t, ts, key, "replay-progress", "is it done")
		status, second, raw := handoffCommand(t, ts.Client(), ts.URL, key, "handoff-replay-progress", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "replay-progress", "revision": 1, "content_hash": fmt.Sprintf("%x", sha256.Sum256([]byte("is it done")))})
		if status != http.StatusOK || second.ResourceID != receipt.ResourceID {
			t.Fatalf("progress replay status=%d raw=%s", status, raw)
		}
		newService(store.DB(), Config{GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client(), Now: time.Now}).dispatchFastReplies(context.Background())
		var replies int
		_ = store.DB().QueryRow(`SELECT count(*) FROM voice_fast_replies`).Scan(&replies)
		if replies != 1 || posts.Load() != 1 {
			t.Fatalf("replies=%d posts=%d", replies, posts.Load())
		}
	})
	t.Run("task", func(t *testing.T) {
		ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: &fakeClassifier{category: "task"}})
		content := "research this"
		receipt := commitAndHandoff(t, ts, key, "replay-task", content)
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
		status, second, raw := handoffCommand(t, ts.Client(), ts.URL, key, "handoff-replay-task", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "replay-task", "revision": 1, "content_hash": hash})
		if status != http.StatusOK || second.ResourceID != receipt.ResourceID {
			t.Fatalf("task replay status=%d raw=%s", status, raw)
		}
		var tasks int
		_ = store.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks)
		if tasks != 1 {
			t.Fatalf("tasks=%d", tasks)
		}
	})
}

func TestVoiceSessionRequiresConfiguredSpaceGateway(t *testing.T) {
	store, svc := newVoiceTestService(t, Config{})
	_, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}, createRequest{RequestID: "request-1", OfferSDP: "v=0"})
	var api *control.APIError
	if !errors.As(err, &api) || api.Status != http.StatusServiceUnavailable || api.Code != "voice_unavailable" {
		t.Fatalf("unconfigured Space gateway did not fail closed: err=%v", err)
	}
	var sessions int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM voice_sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("unconfigured gateway created %d voice session records", sessions)
	}
}

func TestVoiceCreatePersistsOnlyAccountHashAndOpaqueLease(t *testing.T) {
	const accountID = "sensitive-account-correlation-id"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/voice/sessions" {
			t.Errorf("gateway request=%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answer_sdp": "v=0\\r\\n", "voice_session_id": "gateway-session",
			"voice_session_generation": 3,
			"account_lease_id":         "opaque-lease", "account_lease_generation": 9,
			"account_id": accountID,
		})
	}))
	defer server.Close()
	store, svc := newVoiceTestService(t, Config{GatewayURL: server.URL, GatewayAPIKey: "test-key", HTTPClient: server.Client()})
	principal := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}
	created, err := svc.create(context.Background(), principal, createRequest{RequestID: "request-sensitive", OfferSDP: "v=0\\r\\n"})
	if err != nil {
		t.Fatal(err)
	}
	if created.AccountLeaseID != "opaque-lease" || created.AccountLeaseGeneration != 9 {
		t.Fatalf("lease identity was not returned: %#v", created)
	}
	var answer []byte
	var accountHash []byte
	var mediaGeneration int64
	if err := store.DB().QueryRow(`SELECT offer_answer,account_id_hash,gateway_media_generation FROM voice_sessions WHERE id=?`, created.SessionID).Scan(&answer, &accountHash, &mediaGeneration); err != nil {
		t.Fatal(err)
	}
	if string(answer) == "" || bytes.Contains(answer, []byte(accountID)) || bytes.Contains(answer, []byte("account_id")) {
		t.Fatalf("persisted answer contains account identity material: %s", answer)
	}
	wantHash := sha256.Sum256([]byte(accountID))
	if !bytes.Equal(accountHash, wantHash[:]) {
		t.Fatalf("persisted account hash=%x want=%x", accountHash, wantHash)
	}
	if mediaGeneration != 3 {
		t.Fatalf("gateway media generation=%d want 3", mediaGeneration)
	}
}

func TestVoiceOpeningContextWMIsPostedOnceAndUnverifiedIsRecorded(t *testing.T) {
	var createCalls, openingCalls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/voice/sessions" && r.Method == http.MethodPost:
			createCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"answer_sdp": "v=0\r\n", "voice_session_id": "gw-wm", "opening_context": map[string]any{"applied": true}})
		case r.URL.Path == "/v1/voice/sessions/gw-wm/opening-context" && r.Method == http.MethodPost:
			openingCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"applied": true, "silent": "unverified", "version": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store, svc := newVoiceTestService(t, Config{GatewayURL: server.URL, GatewayAPIKey: "test-key", HTTPClient: server.Client(), ContextAssembler: func(context.Context, contracts.Principal, string, string, string, int) (contracts.MemoryContext, error) {
		return contracts.MemoryContext{Items: []contracts.MemoryContextItem{{Subject: "owner", Key: "rule", Content: "wm context"}}}, nil
	}})
	created, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}, createRequest{RequestID: "wm-opening", OfferSDP: "v=0\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	if createCalls != 1 || openingCalls != 1 {
		t.Fatalf("create=%d opening=%d; WM must make exactly one opening call", createCalls, openingCalls)
	}
	var delivery string
	if err := store.DB().QueryRow(`SELECT opening_context_delivery FROM voice_sessions WHERE id=?`, created.SessionID).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	if delivery != "unverified" {
		t.Fatalf("delivery=%q; silent unverified must not claim delivery", delivery)
	}
}

func TestVoiceOpeningContextAVASIsCreationContextAnd501IsNonFatal(t *testing.T) {
	var openingCalls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/voice/sessions" {
			var body struct {
				OpeningContext *openingContext `json:"opening_context"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OpeningContext == nil || len(body.OpeningContext.Items) != 1 {
				t.Errorf("AVAS create missing opening_context: err=%v body=%+v", err, body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"answer_sdp": "v=0\r\n", "voice_session_id": "gw-avas"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/opening-context") {
			openingCalls++
			http.Error(w, "unsupported", http.StatusNotImplemented)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	store, svc := newVoiceTestService(t, Config{GatewayURL: server.URL, GatewayAPIKey: "test-key", HTTPClient: server.Client(), UpstreamRoute: "avas", ContextAssembler: func(context.Context, contracts.Principal, string, string, string, int) (contracts.MemoryContext, error) {
		return contracts.MemoryContext{Items: []contracts.MemoryContextItem{{Subject: "owner", Key: "rule", Content: "avas context"}}}, nil
	}})
	created, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}, createRequest{RequestID: "avas-opening", OfferSDP: "v=0\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	if openingCalls != 0 {
		t.Fatalf("AVAS made %d live opening calls", openingCalls)
	}
	var delivery string
	if err := store.DB().QueryRow(`SELECT opening_context_delivery FROM voice_sessions WHERE id=?`, created.SessionID).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	if delivery != "unsupported" {
		t.Fatalf("delivery=%q; AVAS creation context must not claim live delivery", delivery)
	}
}

func TestVoiceOpeningContextCharacterBudgetIsDeterministicAndOwnerScoped(t *testing.T) {
	var payload openingContext
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/voice/sessions" {
			_ = json.NewEncoder(w).Encode(map[string]any{"answer_sdp": "v=0\r\n", "voice_session_id": "gw-budget"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/opening-context") {
			_ = json.NewDecoder(r.Body).Decode(&payload)
			_ = json.NewEncoder(w).Encode(map[string]any{"applied": true, "silent": "unverified"})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	store, svc := newVoiceTestService(t, Config{GatewayURL: server.URL, GatewayAPIKey: "test-key", HTTPClient: server.Client(), OpeningContextBudget: 30, ContextAssembler: func(_ context.Context, p contracts.Principal, _ string, _ string, _ string, _ int) (contracts.MemoryContext, error) {
		return contracts.MemoryContext{Items: []contracts.MemoryContextItem{{Subject: p.SubjectID, Key: "k", Content: "1234567890"}, {Subject: "other-owner", Key: "secret", Content: "must-not-appear"}}}, nil
	}})
	_, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}, createRequest{RequestID: "budget-opening", OfferSDP: "v=0\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 1 || len([]rune(payload.Items[0].Text)) > 30 || strings.Contains(payload.Items[0].Text, "other-owner") {
		t.Fatalf("payload=%+v; budget/owner filtering failed", payload)
	}
	_ = store
}

func TestVoiceOpeningContextIncludesOnlyQueuedAndRunningTasksForOwner(t *testing.T) {
	store, svc := newVoiceTestService(t, Config{
		OpeningContextBudget: 1000,
		ContextAssembler: func(context.Context, contracts.Principal, string, string, string, int) (contracts.MemoryContext, error) {
			return contracts.MemoryContext{Items: []contracts.MemoryContextItem{{Subject: "owner", Key: "rule", Content: "context"}}}, nil
		},
	})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`PRAGMA ignore_check_constraints = ON`); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"user-1", "user-2"} {
		if _, err := store.DB().Exec(`INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES(?,?,?,?,?,?)`, "device-"+owner, "default", owner, owner, "approved", now); err != nil {
			t.Fatal(err)
		}
	}
	for _, task := range []struct {
		id, title, goal, status, owner string
	}{
		{"running-task", "running title", "running goal", "running", "user-1"},
		{"awaiting-task", "awaiting title", "awaiting goal", "awaiting_user", "user-1"},
		{"other-task", "other title", "other goal", "running", "user-2"},
	} {
		if _, err := store.DB().Exec(`INSERT INTO command_receipts(command_id,workspace_id,subject_id,idempotency_key,request_hash,receipt_json,accepted_at) VALUES(?,?,?,?,X'01','{}',?)`, "cmd-"+task.id, "default", task.owner, "key-"+task.id, now); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, task.id, "default", task.title, task.goal, "voice", "default", task.status, 1, now, now, "device-"+task.owner, "cmd-"+task.id, task.owner); err != nil {
			t.Fatal(err)
		}
	}

	opening := svc.buildOpeningContext(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user-1"})
	if opening == nil {
		t.Fatal("opening context is nil")
	}
	texts := make([]string, 0, len(opening.Items))
	for _, item := range opening.Items {
		texts = append(texts, item.Text)
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "active task: running title — running goal") {
		t.Fatalf("running task missing from opening context: %q", joined)
	}
	if strings.Contains(joined, "awaiting title") {
		t.Fatalf("awaiting_user task appeared in opening context: %q", joined)
	}
	if strings.Contains(joined, "other title") {
		t.Fatalf("another owner's task appeared in opening context: %q", joined)
	}
}

func TestVoiceOpeningContextResumedUsesDeltaAndNewUsesFull(t *testing.T) {
	var requests []openingContext
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/voice/sessions" {
			var body struct {
				OpeningContext *openingContext `json:"opening_context"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			conversation := "new"
			if len(requests) == 1 {
				conversation = "resumed"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"answer_sdp": "v=0\r\n", "voice_session_id": fmt.Sprintf("gw-%d", len(requests)+1), "conversation": conversation, "conversation_confirmed": conversation == "resumed"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/opening-context") {
			var c openingContext
			_ = json.NewDecoder(r.Body).Decode(&c)
			requests = append(requests, c)
			_ = json.NewEncoder(w).Encode(map[string]any{"applied": true, "silent": "unverified"})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	store, svc := newVoiceTestService(t, Config{GatewayURL: server.URL, GatewayAPIKey: "test-key", HTTPClient: server.Client(), ContextAssembler: func(context.Context, contracts.Principal, string, string, string, int) (contracts.MemoryContext, error) {
		return contracts.MemoryContext{Items: []contracts.MemoryContextItem{{Subject: "owner", Key: "rule", Content: "stable context"}}}, nil
	}})
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}
	first, err := svc.create(context.Background(), p, createRequest{RequestID: "new-context", OfferSDP: "v=0\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET state='ended',opening_context_version=1 WHERE id=?`, first.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE voice_pool_state SET blocked=0,source_session_id='' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.create(context.Background(), p, createRequest{RequestID: "resumed-context", OfferSDP: "v=0\r\n"}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0].Kind != "full" || requests[1].Kind != "delta" || requests[0].Version >= requests[1].Version {
		t.Fatalf("opening requests=%+v; new must be full and resumed must be newer delta", requests)
	}
	var version int
	if err := store.DB().QueryRow(`SELECT opening_context_version FROM voice_sessions WHERE request_id='resumed-context'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != requests[1].Version {
		t.Fatalf("stored version=%d request version=%d", version, requests[1].Version)
	}
}

func TestVoiceRotationDeletesOverBudgetSessionBeforeNextCreate(t *testing.T) {
	var deletes, creates int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/v1/voice/sessions" {
			creates++
			_ = json.NewEncoder(w).Encode(map[string]any{"answer_sdp": "v=0\r\n", "voice_session_id": "gw-new"})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	store, svc := newVoiceTestService(t, Config{GatewayURL: server.URL, GatewayAPIKey: "test-key", HTTPClient: server.Client(), ContextCharBudget: 10})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,gateway_session_id,generation,state,created_at,updated_at,gateway_context_chars) VALUES('old','default','user-1','device-1','old',X'01','gw-old',1,'active',?,?,99)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE voice_pool_state SET blocked=1,source_session_id='old' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}, createRequest{RequestID: "new", OfferSDP: "v=0\r\n"}); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 || creates != 1 {
		t.Fatalf("deletes=%d creates=%d; rotation did not precede next create", deletes, creates)
	}
}

func TestVoiceCreateAbsentGatewayMediaGenerationDefaultsToZero(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"answer_sdp": "v=0\\r\\n", "voice_session_id": "gateway-session"})
	}))
	defer server.Close()
	store, svc := newVoiceTestService(t, Config{GatewayURL: server.URL, GatewayAPIKey: "test-key", HTTPClient: server.Client()})
	created, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}, createRequest{RequestID: "request-no-media-generation", OfferSDP: "v=0\\r\\n"})
	if err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := store.DB().QueryRow(`SELECT gateway_media_generation FROM voice_sessions WHERE id=?`, created.SessionID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 0 {
		t.Fatalf("absent gateway media generation=%d want 0", generation)
	}
}

func TestVoiceCreateNegativeGatewayMediaGenerationQuarantines(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"answer_sdp": "v=0\\r\\n", "voice_session_id": "gateway-session", "voice_session_generation": -1})
	}))
	defer server.Close()
	store, svc := newVoiceTestService(t, Config{GatewayURL: server.URL, GatewayAPIKey: "test-key", HTTPClient: server.Client()})
	_, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}, createRequest{RequestID: "request-negative-media-generation", OfferSDP: "v=0\\r\\n"})
	if err == nil {
		t.Fatal("negative gateway media generation was accepted")
	}
	var state, code string
	if err := store.DB().QueryRow(`SELECT state,last_error_code FROM voice_sessions`).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "quarantined" || code != "gateway_media_generation_invalid" {
		t.Fatalf("negative generation state=%q code=%q", state, code)
	}
}

func TestVoiceCreateAcceptsGatewayOpaqueLeaseWithoutAccountID(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answer_sdp": "v=0\\r\\n", "voice_session_id": "gateway-session",
			"account_lease_id": "opaque-lease", "account_lease_generation": 1,
		})
	}))
	defer server.Close()
	store, svc := newVoiceTestService(t, Config{GatewayURL: server.URL, GatewayAPIKey: "test-key", HTTPClient: server.Client()})
	created, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}, createRequest{RequestID: "request-opaque", OfferSDP: "v=0\\r\\n"})
	if err != nil {
		t.Fatal(err)
	}
	if created.AccountLeaseID != "opaque-lease" || created.AccountLeaseGeneration != 1 {
		t.Fatalf("opaque lease identity was not accepted: %#v", created)
	}
	var accountHash []byte
	if err := store.DB().QueryRow(`SELECT account_id_hash FROM voice_sessions WHERE id=?`, created.SessionID).Scan(&accountHash); err != nil {
		t.Fatal(err)
	}
	if len(accountHash) != 0 {
		t.Fatalf("account identity should remain hidden, got hash length %d", len(accountHash))
	}
}

func TestSpaceDownstreamSDPDTOUsesObservedFieldNames(t *testing.T) {
	body, err := json.Marshal(gatewaySessionRequest{OfferSDP: "v=0\r\n", Voice: "", VoiceMode: "", LanguageCode: ""})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["offer_sdp"] != "v=0\r\n" || len(fields) != 1 {
		t.Fatalf("new Space session request DTO=%s; expected only offer_sdp for a fresh session", body)
	}
	var answer gatewaySessionResponse
	if err := json.Unmarshal([]byte(`{"answer_sdp":"v=0\\r\\n","voice_session_id":"opaque-session-id"}`), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.AnswerSDP == "" || answer.VoiceSessionID != "opaque-session-id" {
		t.Fatalf("Space answer DTO did not decode: %#v", answer)
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

func TestUnknownMediaOutcomeBlocksWholePoolUntilOperatorReconciliation(t *testing.T) {
	ctx := context.Background()
	store, svc := newVoiceTestService(t, Config{})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('session-1','default','user-1','device-1','request-1',X'01',1,'allocating',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := svc.markSession(ctx, "session-1", "active", "", []byte(`{"answer_sdp":"transient"}`), "gateway-session-1"); err != nil {
		t.Fatal(err)
	}
	var blocked int
	var reason string
	if err := store.DB().QueryRowContext(ctx, `SELECT blocked,reason_code FROM voice_pool_state WHERE id=1`).Scan(&blocked, &reason); err != nil || blocked != 1 || reason != "active_session" {
		t.Fatalf("active voice generation did not reserve the hidden pool: blocked=%d reason=%q err=%v", blocked, reason, err)
	}
	if err := svc.markSession(ctx, "session-1", "quarantined", "gateway_outcome_unknown", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT blocked FROM voice_pool_state WHERE id=1`).Scan(&blocked); err != nil || blocked != 1 {
		t.Fatalf("unknown media outcome did not block account pool: blocked=%d err=%v", blocked, err)
	}
	principal := contracts.Principal{WorkspaceID: "default", SubjectID: "operator", DeviceID: "operator-device", Scopes: []string{"voice:pool:reconcile"}}
	payload, _ := json.Marshal(reconcilePoolPayload{SessionID: "session-1", Generation: 1, Reason: "Manually verified no live media remains", ConfirmedOutOfBandTermination: true})
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, events, err := svc.commandReconcilePool(ctx, tx, control.CommandContext{CommandID: "reconcile-command", Principal: principal, AcceptedAt: time.Now().UTC()}, contracts.CommandRequest{Payload: payload})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "voice.pool.reconciled" {
		t.Fatalf("operator reconciliation event=%#v", events)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var reconciliations int
	if err := store.DB().QueryRowContext(ctx, `SELECT blocked FROM voice_pool_state WHERE id=1`).Scan(&blocked); err != nil || blocked != 0 {
		t.Fatalf("operator reconciliation did not clear pool block: blocked=%d err=%v", blocked, err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM voice_pool_reconciliations WHERE method='operator_attestation' AND source_session_id='session-1' AND generation=1`).Scan(&reconciliations); err != nil || reconciliations != 1 {
		t.Fatalf("operator reconciliation audit rows=%d err=%v", reconciliations, err)
	}
}

func TestVoiceCreateGatewayAttestationNeverEstablishedReleasesPool(t *testing.T) {
	var requestID string
	var store *storage.Store
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/voice/sessions" {
			requestID = r.Header.Get("X-Request-ID")
			var persisted string
			if err := store.DB().QueryRow(`SELECT gateway_request_id FROM voice_sessions WHERE request_id='request-attest'`).Scan(&persisted); err != nil {
				t.Fatalf("read persisted gateway request id: %v", err)
			}
			if persisted != requestID || requestID == "" {
				t.Fatalf("persisted request id=%q, header=%q", persisted, requestID)
			}
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/voice/requests/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, fmt.Sprintf(`{"request_id":%q,"outcome":"never_established"}`, requestID))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	var svc *Service
	store, svc = newVoiceTestService(t, Config{GatewayURL: server.URL, GatewayAPIKey: "key", HTTPClient: server.Client()})
	if _, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}, createRequest{RequestID: "request-attest", OfferSDP: "v=0"}); err == nil {
		t.Fatal("create unexpectedly succeeded")
	}
	var state, code string
	var blocked, reconciliations, events int
	if err := store.DB().QueryRow(`SELECT state,last_error_code FROM voice_sessions`).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT blocked FROM voice_pool_state`).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM voice_pool_reconciliations WHERE method='gateway_attestation'`).Scan(&reconciliations); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE event_type='voice.pool.reconciled'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || code != "gateway_never_established" || blocked != 0 || reconciliations != 1 || events != 1 {
		t.Fatalf("state=%s code=%s blocked=%d reconciliations=%d events=%d", state, code, blocked, reconciliations, events)
	}
}

func TestVoiceCreate422AlreadyUnblocksNegotiationFence(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":"upstream_rejected","established":false,"upstream_status":502}`)
	}))
	defer server.Close()
	store, svc := newVoiceTestService(t, Config{GatewayURL: server.URL, GatewayAPIKey: "key", HTTPClient: server.Client()})
	if _, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}, createRequest{RequestID: "request-422", OfferSDP: "v=0"}); err == nil {
		t.Fatal("create unexpectedly succeeded")
	}
	var state, code string
	var blocked int
	if err := store.DB().QueryRow(`SELECT state,last_error_code FROM voice_sessions`).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT blocked FROM voice_pool_state`).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || code != "gateway_rejected" || blocked != 0 {
		t.Fatalf("state=%s code=%s blocked=%d", state, code, blocked)
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
			if got := svc.queryGatewayOutcome(context.Background(), "req"); got != tc.want {
				t.Fatalf("outcome=%q, want %q", got, tc.want)
			}
			var count int
			if err := store.DB().QueryRow(`SELECT COUNT(*) FROM voice_pool_reconciliations`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("query created %d reconciliation rows", count)
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
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET last_error_code='gateway_outcome_unknown' WHERE id='attested'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE voice_pool_state SET blocked=1,reason_code='gateway_outcome_unknown',source_session_id='other' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := svc.resolveGatewayNeverEstablished(context.Background(), "attested", 1, "quarantined"); err != nil {
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
	if state != "failed" || blocked != 1 || rows != 0 {
		t.Fatalf("state=%s blocked=%d reconciliations=%d", state, blocked, rows)
	}
	if _, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('other','default','other-owner','device','other-request',X'01',1,'quarantined',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(reconcilePoolPayload{SessionID: "other", Generation: 1, Reason: "Verified no upstream media remains", ConfirmedOutOfBandTermination: true})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.commandReconcilePool(context.Background(), tx, control.CommandContext{Principal: contracts.Principal{SubjectID: "operator", Scopes: []string{"voice:pool:reconcile"}}, AcceptedAt: time.Now().UTC()}, contracts.CommandRequest{Payload: payload})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("source session remained unreconcilable: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
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

func TestEndingMediaCannotBeReconciledBeforeAtomicHangupFinalization(t *testing.T) {
	ctx := context.Background()
	store, svc := newVoiceTestService(t, Config{})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,gateway_session_id,created_at,updated_at) VALUES('session-ending','default','user-1','device-1','request-ending',X'01',4,'ending','gateway-session',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE voice_pool_state SET blocked=1,reason_code='active_session',source_session_id='session-ending' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	principal := contracts.Principal{WorkspaceID: "default", SubjectID: "operator", DeviceID: "operator-device", Scopes: []string{"voice:pool:reconcile"}}
	payload, _ := json.Marshal(reconcilePoolPayload{SessionID: "session-ending", Generation: 4, Reason: "Verified gateway completion evidence", ConfirmedOutOfBandTermination: true})
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.commandReconcilePool(ctx, tx, control.CommandContext{CommandID: "reconcile-too-early", Principal: principal, AcceptedAt: time.Now().UTC()}, contracts.CommandRequest{Payload: payload})
	_ = tx.Rollback()
	var apiErr *control.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "voice_sessions_active" {
		t.Fatalf("in-flight ending session did not fence reconciliation: err=%v", err)
	}

	observation := AccountTerminationObservation{AccountID: "opaque-account", LeaseID: "opaque-lease", SessionID: "gateway-session", Terminated: true, EvidenceID: "evidence-1", ObservedAt: time.Now().UTC()}
	blocked, err := svc.completeHangup(ctx, contracts.Principal{WorkspaceID: "default", SubjectID: "user-1"}, "session-ending", 4, observation, true)
	if err != nil || blocked {
		t.Fatalf("trusted completion did not atomically release the matching pool fence: blocked=%t err=%v", blocked, err)
	}
	var state string
	if err := store.DB().QueryRowContext(ctx, `SELECT state FROM voice_sessions WHERE id='session-ending'`).Scan(&state); err != nil || state != "ended" {
		t.Fatalf("final state=%q err=%v", state, err)
	}
	var blockedDB, evidenceRows int
	if err := store.DB().QueryRowContext(ctx, `SELECT blocked FROM voice_pool_state WHERE id=1`).Scan(&blockedDB); err != nil || blockedDB != 0 {
		t.Fatalf("pool fence was not cleared with termination evidence: blocked=%d err=%v", blockedDB, err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM voice_account_terminations WHERE session_id='session-ending' AND generation=4 AND evidence_id='evidence-1'`).Scan(&evidenceRows); err != nil || evidenceRows != 1 {
		t.Fatalf("trusted termination evidence rows=%d err=%v", evidenceRows, err)
	}
}
