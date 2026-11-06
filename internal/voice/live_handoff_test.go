package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/answer"
	"github.com/ronigooja/Zoe/internal/steward"
)

func TestDefaultClassificationBudget(t *testing.T) {
	svc := &Service{}
	if got := svc.classificationTimeout(); got != 6*time.Second {
		t.Fatalf("default classification budget=%v, want 6s", got)
	}
	svc.recall.ClassificationTimeout = 7 * time.Second
	if got := svc.classificationTimeout(); got != 7*time.Second {
		t.Fatalf("configured classification budget=%v, want 7s", got)
	}
}

func TestThreeSecondStewardAskHonouredByHandoff(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3000 * time.Millisecond)
		_, _ = fmt.Fprint(w, `{"output_text":"ask"}`)
	}))
	defer model.Close()
	env := map[string]string{"ZOE_STEWARD_BASE_URL": model.URL, "ZOE_STEWARD_MODEL": "fake-steward", "ZOE_STEWARD_API_KEY_FILE": "/fake/key"}
	client, err := steward.FromEnv(func(k string) string { return env[k] }, func(string) ([]byte, error) { return []byte("fake-key"), nil })
	if err != nil {
		t.Fatal(err)
	}
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: &fakeClassifier{category: "task"}, Steward: client})
	receipt := commitAndHandoff(t, ts, key, "three-second-ask", "please handle this")
	var status string
	if err := store.DB().QueryRow(`SELECT status FROM tasks WHERE id=?`, receipt.ResourceID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "waiting" {
		t.Fatalf("three-second ask silently dispatched: task status=%s", status)
	}
}

func TestStewardFailureReasonLogs(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"steward_timeout", context.DeadlineExceeded}, {"steward_error", errors.New("private transcript secret")}} {
		t.Run(tc.name, func(t *testing.T) {
			_, store, _ := handoffHTTPServerWithConfig(t, Config{})
			const content = "private question for the steward"
			hash := sha256.Sum256([]byte(content))
			_, err := store.DB().Exec(`INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id) VALUES('steward-log','default','handoff-session',1,'steward-log',1,?,?,?,'steward-log')`, content, hash[:], time.Now().UTC().Format(time.RFC3339Nano))
			if err != nil {
				t.Fatal(err)
			}
			svc := newService(store.DB(), Config{Classifier: &fakeClassifier{category: "task"}, Steward: &fakeSteward{err: tc.err}})
			defer svc.Stop()
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(previous)
			svc.classifyCommittedTurn("steward-log")
			if !strings.Contains(logs.String(), "steward_ms=") || !strings.Contains(logs.String(), "reason="+tc.name) {
				t.Fatalf("missing Steward reason: %s", logs.String())
			}
			if strings.Contains(logs.String(), content) || strings.Contains(logs.String(), "private transcript secret") {
				t.Fatal("private text logged")
			}
		})
	}
}

// delayedLiveClassifier reproduces a model completing after handoff starts.
type delayedLiveClassifier struct{ delay time.Duration }

func (f delayedLiveClassifier) Classify(ctx context.Context, _ string) (string, error) {
	select {
	case <-time.After(f.delay):
		return "recall", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type delayedLiveSteward struct{ delay time.Duration }

func (f delayedLiveSteward) Decide(ctx context.Context, _ string) (steward.Decision, error) {
	select {
	case <-time.After(f.delay):
		return steward.Dispatch, nil
	case <-ctx.Done():
		return steward.Dispatch, ctx.Err()
	}
}

func TestLiveRecallHTTPProductionAnswerWiring(t *testing.T) {
	const question = "上一次通话咱们聊了什么"
	var logs bytes.Buffer
	previousLog := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLog)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "今日天气查询") || !strings.Contains(string(body), question) {
			t.Errorf("model did not receive scoped previous-call evidence: %s", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"output_text": `{"status":"answer","text":"上次通话我们聊了今天的天气。"}`})
	}))
	defer model.Close()
	env := map[string]string{"ZOE_ANSWER_BASE_URL": model.URL, "ZOE_ANSWER_MODEL": "fake-answer", "ZOE_ANSWER_API_KEY_FILE": "/fake/answer-key", "ZOE_ANSWER_TIMEOUT_MS": "8000"}
	client, err := answer.FromEnv(func(k string) string { return env[k] }, func(string) ([]byte, error) { return []byte("fake-key"), nil })
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var pushes []string
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		pushes = append(pushes, body["text"])
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()
	cfg := Config{Classifier: delayedLiveClassifier{1200 * time.Millisecond}, Steward: delayedLiveSteward{1500 * time.Millisecond}, Answerer: client, GatewayURL: gateway.URL, GatewayAPIKey: "fake-key", HTTPClient: gateway.Client()}
	ts, store, key := handoffHTTPServerWithConfig(t, cfg)
	_, err = store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id='gateway' WHERE id='handoff-session';
INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) SELECT 'previous-call',workspace_id,owner_id,device_id,'previous-request',X'01',1,'ended',created_at,updated_at FROM voice_sessions WHERE id='handoff-session';
INSERT INTO voice_call_summaries(id,workspace_id,session_id,topic,started_at,ended_at,first_seq,last_seq,summary,created_at) VALUES('previous-summary','default','previous-call','今日天气查询','2026-11-05T12:00:00Z','2026-11-05T12:01:00Z',2,5,'我们聊了今天的天气。','2026-11-05T12:02:00Z');`)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "commit-live-recall", "voice.turn.commit", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "live-recall", "revision": 1, "content": question, "user_confirmed": true}); code != http.StatusOK {
		t.Fatalf("commit: %d %s", code, raw)
	}
	time.Sleep(300 * time.Millisecond)
	hash := sha256.Sum256([]byte(question))
	if code, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "handoff-live-recall", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "live-recall", "revision": 1, "content_hash": fmt.Sprintf("%x", hash[:])}); code != http.StatusOK {
		t.Fatalf("handoff: %d %s", code, raw)
	}
	var holdings, jobs, tasks int
	if err := store.DB().QueryRow(`SELECT (SELECT count(*) FROM voice_fast_replies WHERE intent_kind='recall_holding'),(SELECT count(*) FROM voice_recall_jobs),(SELECT count(*) FROM tasks)`).Scan(&holdings, &jobs, &tasks); err != nil {
		t.Fatal(err)
	}
	if holdings != 1 || jobs != 1 || tasks != 0 {
		t.Fatalf("recall routing: holding=%d jobs=%d tasks=%d; want 1,1,0", holdings, jobs, tasks)
	}
	svc := newService(store.DB(), cfg)
	defer svc.Stop()
	svc.dispatchFastReplies(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(pushes) == 2
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pushes) != 2 || pushes[1] != recallPrefix+"上次通话我们聊了今天的天气。" {
		t.Fatalf("holding then grounded answer: %q", pushes)
	}
	if strings.Contains(logs.String(), question) || !strings.Contains(logs.String(), "triage_ms=") || !strings.Contains(logs.String(), "steward_ms=0") || !strings.Contains(logs.String(), "wait_ms=") {
		t.Fatalf("classification log missing metrics or leaked sentence: %s", logs.String())
	}
}

func TestLiveHandoffBudgetTimeoutIsRetryableWithoutEffects(t *testing.T) {
	ts, store, key := handoffHTTPServerWithConfig(t, Config{ClassificationTimeout: 30 * time.Millisecond})
	const question = "上一次通话咱们聊了什么"
	hash := sha256.Sum256([]byte(question))
	_, err := store.DB().Exec(`INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id) VALUES('late-id','default','handoff-session',1,'late',1,?,?,?,'late-commit')`, question, hash[:], time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "late", "revision": 1, "content_hash": fmt.Sprintf("%x", hash[:])}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	code, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "late-handoff", "voice.turn.handoff", payload)
	if code != http.StatusServiceUnavailable || !strings.Contains(raw, "triage_timeout") {
		t.Fatalf("timeout: %d %s", code, raw)
	}
	var effects int
	if err := store.DB().QueryRow(`SELECT (SELECT count(*) FROM tasks)+(SELECT count(*) FROM voice_recall_jobs)+(SELECT count(*) FROM voice_fast_replies)`).Scan(&effects); err != nil {
		t.Fatal(err)
	}
	if effects != 0 {
		t.Fatalf("timeout created %d effects", effects)
	}
	if !strings.Contains(logs.String(), "budget_hit=true reason=triage_timeout") || strings.Contains(logs.String(), question) {
		t.Fatalf("timeout log missing or leaked: %s", logs.String())
	}
	// A late classification only persists routing; retry admits the effect once.
	if _, err := store.DB().Exec(`UPDATE voice_turns SET triage_category='recall',steward_decision='dispatch' WHERE id='late-id'`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"late-handoff", "late-new-command"} {
		code, _, raw = handoffCommand(t, ts.Client(), ts.URL, key, id, "voice.turn.handoff", payload)
		if code != http.StatusOK {
			t.Fatalf("retry: %d %s", code, raw)
		}
	}
	var jobs, holdings, tasks int
	if err := store.DB().QueryRow(`SELECT (SELECT count(*) FROM voice_recall_jobs),(SELECT count(*) FROM voice_fast_replies WHERE intent_kind='recall_holding'),(SELECT count(*) FROM tasks)`).Scan(&jobs, &holdings, &tasks); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || holdings != 1 || tasks != 0 {
		t.Fatalf("late effects jobs=%d holding=%d tasks=%d", jobs, holdings, tasks)
	}
}

func TestLiveNonTaskRoutingSkipsStewardAndMemoryStaysSlow(t *testing.T) {
	for _, category := range []string{"progress", "recall", "memory", "task"} {
		t.Run(category, func(t *testing.T) {
			decision := &fakeSteward{decision: steward.Ask}
			ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: &fakeClassifier{category: category}, Steward: decision})
			receipt := commitAndHandoff(t, ts, key, "routing-"+category, "please handle this")
			if category == "task" {
				if decision.calls.Load() != 1 {
					t.Fatalf("task Steward calls=%d", decision.calls.Load())
				}
				var status string
				if err := store.DB().QueryRow(`SELECT status FROM tasks WHERE id=?`, receipt.ResourceID).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if status != "waiting" {
					t.Fatalf("task decision not awaited: %s", status)
				}
			} else {
				if decision.calls.Load() != 0 {
					t.Fatalf("non-task Steward calls=%d", decision.calls.Load())
				}
				want := "voice_turn"
				if category == "memory" {
					want = "task"
				}
				if receipt.ResourceType != want {
					t.Fatalf("route=%s want=%s", receipt.ResourceType, want)
				}
			}
		})
	}
}

func TestLiveHoldQuestionDeliveredOnce(t *testing.T) {
	for _, tc := range []struct {
		name, text, intent string
		cfg                Config
	}{
		{"steward", "please handle this", "steward_ask", Config{Steward: &fakeSteward{decision: steward.Ask}}},
		{"hard-impact-no-steward", "帮我删除下载文件夹里的所有文件", "explicit_confirmation", Config{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts int
			var spoken string
			gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				posts++
				spoken = body["text"]
				w.WriteHeader(http.StatusOK)
			}))
			defer gateway.Close()
			cfg := tc.cfg
			cfg.GatewayURL, cfg.GatewayAPIKey, cfg.HTTPClient = gateway.URL, "fake-key", gateway.Client()
			ts, store, key := handoffHTTPServerWithConfig(t, cfg)
			if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id='gateway' WHERE id='handoff-session'`); err != nil {
				t.Fatal(err)
			}
			first := commitAndHandoff(t, ts, key, tc.name, tc.text)
			hash := sha256.Sum256([]byte(tc.text))
			code, second, raw := handoffCommand(t, ts.Client(), ts.URL, key, "retry-"+tc.name, "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": tc.name, "revision": 1, "content_hash": fmt.Sprintf("%x", hash[:])})
			if code != 200 || second.ResourceID != first.ResourceID {
				t.Fatalf("retry: %d %s", code, raw)
			}
			var count int
			if err := store.DB().QueryRow(`SELECT count(*) FROM voice_fast_replies WHERE turn_id=? AND intent_kind=?`, tc.name, tc.intent).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("question queue count=%d, want 1", count)
			}
			if tc.intent == "explicit_confirmation" {
				code, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "unsafe-confirm-"+tc.name, "task.append", map[string]any{"task_id": first.ResourceID, "content": "yes, proceed"})
				if code != http.StatusConflict || !strings.Contains(raw, "confirmation_invalid") {
					t.Fatalf("unscoped assent bypassed hold: %d %s", code, raw)
				}
				var state string
				if err := store.DB().QueryRow(`SELECT status FROM tasks WHERE id=?`, first.ResourceID).Scan(&state); err != nil {
					t.Fatal(err)
				}
				if state != "waiting" {
					t.Fatalf("hard-impact hold resumed without scope: %s", state)
				}
			}
			svc := newService(store.DB(), cfg)
			svc.dispatchFastReplies(context.Background())
			svc.dispatchFastReplies(context.Background())
			if posts != 1 || !strings.HasPrefix(spoken, recallPrefix) || !strings.Contains(spoken, steward.Question(tc.text)) {
				t.Fatalf("question delivery posts=%d text=%q", posts, spoken)
			}
		})
	}
}
