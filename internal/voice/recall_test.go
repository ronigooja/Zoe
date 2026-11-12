package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/answer"
)

type recallModelFunc func(context.Context, string, []answer.Candidate, []answer.Excerpt) (answer.Result, error)

func (f recallModelFunc) Answer(ctx context.Context, q string, c []answer.Candidate, e []answer.Excerpt) (answer.Result, error) {
	return f(ctx, q, c, e)
}

func TestRecallRoute(t *testing.T) {
	for _, mode := range []string{"answer", "transcript", "round_limit", "unknown_id", "not_found", "no_summary", "unconfigured", "error", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
			var mu sync.Mutex
			var pushes []string
			gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/context") {
					t.Errorf("path=%s", r.URL.Path)
				}
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				pushes = append(pushes, body["text"])
				mu.Unlock()
				w.WriteHeader(200)
			}))
			defer gateway.Close()
			calls := 0
			model := recallModelFunc(func(ctx context.Context, q string, c []answer.Candidate, e []answer.Excerpt) (answer.Result, error) {
				calls++
				if len(c) != 2 || c[0].ID != "selected-summary" || c[1].ID != "unrequested-summary" {
					t.Errorf("wrong scoped/date candidates: %+v", c)
				}
				switch mode {
				case "unknown_id":
					return answer.Result{Status: "need_transcript", SummaryIDs: []string{"private-summary"}}, nil
				case "error":
					return answer.Result{}, errors.New("private transcript secret")
				case "timeout":
					<-ctx.Done()
					return answer.Result{}, ctx.Err()
				case "not_found":
					return answer.Result{Status: "not_found"}, nil
				case "transcript", "round_limit":
					if calls == 1 {
						return answer.Result{Status: "need_transcript", SummaryIDs: []string{"selected-summary"}}, nil
					}
					if len(e) != 1 || e[0].SummaryID != "selected-summary" || len(e[0].Text) != recallExcerptBytes || strings.Contains(e[0].Text, "outside-range-secret") {
						t.Errorf("wrong excerpts: count=%d bytes=%d", len(e), len(e[0].Text))
					}
					if mode == "round_limit" {
						return answer.Result{Status: "need_transcript", SummaryIDs: []string{"selected-summary"}}, nil
					}
				}
				return answer.Result{Status: "answer", Text: "我们聊过花园。"}, nil
			})
			router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"output_text": `{"route":"recall","restatement":""}`})
			}))
			defer router.Close()
			cfg := Config{RegisterConfig: &RegisterConfig{Model: RegisterModelConfig{BaseURL: router.URL, Model: "fixture", APIKey: "fixture"}, CompactCharacters: 1000}, GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client(), Now: func() time.Time { return now }, Answerer: model, RecallTimeout: 100 * time.Millisecond}
			if mode == "unconfigured" {
				cfg.Answerer = nil
			}
			ts, store, key := handoffHTTPServerWithConfig(t, cfg)
			at := now.Format(time.RFC3339Nano)
			if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id='gateway' WHERE id='handoff-session'`); err != nil {
				t.Fatal(err)
			}
			// Clone the session identity to make foreign-owner exclusion real.
			_, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,offer_answer,created_at,updated_at,heartbeat_at) SELECT 'other-session',workspace_id,'another-owner',device_id,'other-request',X'01',generation,'ended','',created_at,updated_at,heartbeat_at FROM voice_sessions WHERE id='handoff-session'`)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.DB().Exec(`INSERT INTO workspaces(id,name,created_at) VALUES('foreign-workspace','Foreign',?);`, at)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,offer_answer,created_at,updated_at) SELECT 'foreign-session','foreign-workspace',owner_id,device_id,'foreign-request',X'01',generation,'ended','',created_at,updated_at FROM voice_sessions WHERE id='handoff-session'`)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.DB().Exec(`INSERT INTO voice_call_summaries(id,workspace_id,session_id,topic,started_at,ended_at,first_seq,last_seq,summary,created_at) VALUES('foreign-summary','foreign-workspace','foreign-session','foreign','2026-11-05T12:00:00Z','2026-11-05T12:00:00Z',1,1,'foreign summary secret',?)`, at)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range []struct{ id, session, start string }{{"selected-summary", "handoff-session", "2026-11-05T12:00:00Z"}, {"old-summary", "handoff-session", "2026-11-01T12:00:00Z"}, {"private-summary", "other-session", "2026-11-05T12:00:00Z"}} {
				if mode == "no_summary" {
					continue
				}
				_, err := store.DB().Exec(`INSERT INTO voice_call_summaries(id,workspace_id,session_id,topic,started_at,ended_at,first_seq,last_seq,summary,created_at) VALUES(?,'default',?,'garden',?,?,2,2,'private summary secret',?)`, v.id, v.session, v.start, v.start, at)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode != "no_summary" {
				_, err = store.DB().Exec(`INSERT INTO voice_call_summaries(id,workspace_id,session_id,topic,started_at,ended_at,first_seq,last_seq,summary,created_at) VALUES('unrequested-summary','default','handoff-session','other','2026-11-05T10:00:00Z','2026-11-05T10:00:00Z',1,1,'unrequested summary',?)`, at)
				if err != nil {
					t.Fatal(err)
				}
			}
			for seq, text := range map[int]string{1: "outside-range-secret", 2: strings.Repeat("private transcript secret ", 1000), 3: "outside-range-secret"} {
				_, err := store.DB().Exec(`INSERT INTO voice_transcript_turns(workspace_id,session_id,seq,role,text,at,received_at) VALUES('default','handoff-session',?,'user',?,?,?)`, seq, text, at, at)
				if err != nil {
					t.Fatal(err)
				}
			}
			var logs bytes.Buffer
			old := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(old)
			q := "what did we discuss yesterday"
			receipt := commitAndHandoff(t, ts, key, "recall-turn", q)
			var tasks int
			_ = store.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks)
			if tasks != 0 || receipt.ResourceType != "voice_turn" {
				t.Fatalf("recall created task: tasks=%d resource=%s", tasks, receipt.ResourceType)
			}
			svc := newService(store.DB(), cfg)
			// Command acceptance uses the control clock, independently of voice's
			// fake clock. Make the holding row due explicitly before dispatch.
			if _, err := store.DB().Exec(`UPDATE voice_fast_replies SET next_attempt_at=? WHERE intent_kind='recall_holding'`, at); err != nil {
				t.Fatal(err)
			}
			svc.dispatchFastReplies(context.Background())
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				mu.Lock()
				n := len(pushes)
				mu.Unlock()
				if n == 2 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			// A different command ID must still share the stable turn receipts.
			hash := sha256.Sum256([]byte(q))
			status, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "retry-recall", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "recall-turn", "revision": 1, "content_hash": fmt.Sprintf("%x", hash[:])})
			if status != 200 {
				t.Fatalf("retry=%d %s", status, raw)
			}
			svc.dispatchFastReplies(context.Background())
			mu.Lock()
			defer mu.Unlock()
			if len(pushes) != 2 {
				t.Fatalf("holding then answer: got %d pushes", len(pushes))
			}
			if pushes[0] != "我查一下我们之前的对话。" {
				t.Fatalf("holding=%q", pushes[0])
			}
			expected := recallNotFound
			if mode == "unconfigured" || mode == "error" || mode == "timeout" || mode == "unknown_id" || mode == "round_limit" {
				expected = "我现在没法查看对话记录，请稍后再试。"
			}
			if mode == "answer" || mode == "transcript" {
				expected = "对话记录里记着：我们聊过花园。"
			}
			if pushes[1] != expected {
				t.Fatalf("answer=%q", pushes[1])
			}
			if calls > 2 {
				t.Fatalf("extra rounds=%d", calls)
			}
			if (mode == "unconfigured" || mode == "no_summary") && calls != 0 {
				t.Fatalf("unexpected model calls=%d", calls)
			}
			for _, text := range pushes {
				if strings.ContainsAny(text, "*#`[]<>_") || strings.Contains(text, "selected-summary") {
					t.Fatalf("not speakable: %q", text)
				}
			}
			if strings.Contains(logs.String(), q) || strings.Contains(logs.String(), "private transcript secret") || strings.Contains(logs.String(), "private summary secret") {
				t.Fatal("private text leaked to logs")
			}
			if !strings.Contains(logs.String(), "voice recall id=") || !strings.Contains(logs.String(), "elapsed_ms=") {
				t.Fatal("recall log missing answer elapsed_ms")
			}
			if mode == "timeout" && !strings.Contains(logs.String(), "status=timeout") {
				t.Fatal("recall deadline missing timeout status")
			}
		})
	}
}

func TestRecallSpeechRejectsMarkupAndEvidenceIDs(t *testing.T) {
	for _, text := range []string{"**garden**", "[garden](https://example.com)", "selected-summary"} {
		got := recallSpeech(text, []answer.Candidate{{ID: "selected-summary"}})
		if got != recallUnavailable {
			t.Fatalf("unsafe speech: %q", got)
		}
	}
}

func TestRecallLatestSummaryLimit(t *testing.T) {
	now := time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
	_, store, _ := handoffHTTPServerWithConfig(t, Config{})
	for i := 0; i < 3; i++ {
		at := now.Add(-time.Duration(i) * time.Hour).Format(time.RFC3339Nano)
		_, err := store.DB().Exec(`INSERT INTO voice_call_summaries(id,workspace_id,session_id,topic,started_at,ended_at,first_seq,last_seq,summary,created_at) VALUES(?,'default','handoff-session','topic',?,?,1,1,'summary',?)`, fmt.Sprintf("summary-%d", i), at, at, at)
		if err != nil {
			t.Fatal(err)
		}
	}
	model := recallModelFunc(func(_ context.Context, _ string, c []answer.Candidate, _ []answer.Excerpt) (answer.Result, error) {
		if len(c) != 2 || c[0].ID != "summary-0" || c[1].ID != "summary-1" {
			t.Fatalf("latest summary limit: %+v", c)
		}
		return answer.Result{Status: "answer", Text: "我们聊过花园。"}, nil
	})
	svc := newService(store.DB(), Config{Now: func() time.Time { return now }, Answerer: model, RecallSummaryLimit: 2})
	text, n, status := svc.recallAnswer(context.Background(), "default", "owner", "remember our conversations")
	if status != "answer" || n != 2 || text != "对话记录里记着：我们聊过花园。" {
		t.Fatalf("status=%s count=%d text=%q", status, n, text)
	}
}

func TestRecallFailureSentences(t *testing.T) {
	for _, status := range []string{"unconfigured", "timezone", "summary_error", "no_summaries", "model_error", "invalid_request", "timeout", "transcript_error", "not_found"} {
		t.Run(status, func(t *testing.T) {
			want := recallUnavailable
			if status == "no_summaries" || status == "not_found" {
				want = recallNotFound
			}
			if got := recallFailureSpeech(status); got != want {
				t.Fatalf("%s speech=%q, want %q", status, got, want)
			}
		})
	}
}

func TestRecallSummaryReadFailureIsNotMissingEvidence(t *testing.T) {
	_, store, _ := handoffHTTPServerWithConfig(t, Config{})
	model := recallModelFunc(func(context.Context, string, []answer.Candidate, []answer.Excerpt) (answer.Result, error) {
		t.Fatal("model called after summary read failure")
		return answer.Result{}, nil
	})
	svc := newService(store.DB(), Config{Answerer: model})
	defer svc.Stop()
	if _, err := store.DB().Exec(`DROP TABLE voice_call_summaries`); err != nil {
		t.Fatal(err)
	}
	text, count, status := svc.recallAnswer(context.Background(), "default", "owner", "private question")
	if status != "summary_error" || count != 0 || text != recallUnavailable {
		t.Fatalf("summary failure status=%s count=%d text=%q", status, count, text)
	}
}
