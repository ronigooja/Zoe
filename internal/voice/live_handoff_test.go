package voice

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/answer"
	"github.com/ronigooja/Zoe/internal/steward"
)

// delayedLiveClassifier reproduces a model completing after handoff starts.
type delayedLiveClassifier struct{}

func (delayedLiveClassifier) Classify(ctx context.Context, _ string) (string, error) {
	select {
	case <-time.After(100 * time.Millisecond):
		return "recall", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestLiveRecallHTTPProductionAnswerWiring(t *testing.T) {
	const question = "上一次通话咱们聊了什么"
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
	cfg := Config{Classifier: delayedLiveClassifier{}, Steward: &fakeSteward{decision: steward.Dispatch}, Answerer: client, GatewayURL: gateway.URL, GatewayAPIKey: "fake-key", HTTPClient: gateway.Client()}
	ts, store, key := handoffHTTPServerWithConfig(t, cfg)
	_, err = store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id='gateway' WHERE id='handoff-session';
INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) SELECT 'previous-call',workspace_id,owner_id,device_id,'previous-request',X'01',1,'ended',created_at,updated_at FROM voice_sessions WHERE id='handoff-session';
INSERT INTO voice_call_summaries(id,workspace_id,session_id,topic,started_at,ended_at,first_seq,last_seq,summary,created_at) VALUES('previous-summary','default','previous-call','今日天气查询','2026-11-05T12:00:00Z','2026-11-05T12:01:00Z',2,5,'我们聊了今天的天气。','2026-11-05T12:02:00Z');`)
	if err != nil {
		t.Fatal(err)
	}
	commitAndHandoff(t, ts, key, "live-recall", question)
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
