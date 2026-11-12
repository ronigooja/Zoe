package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/answer"
)

type recallModelFunc func(context.Context, string, []contracts.MemoryContextItem, string) (answer.Result, error)

func (f recallModelFunc) Answer(ctx context.Context, q string, items []contracts.MemoryContextItem, conversation string) (answer.Result, error) {
	return f(ctx, q, items, conversation)
}

func TestRecallMemoryAndRegisterAnswer(t *testing.T) {
	for _, mode := range []string{"answer", "not_found", "memory_only", "bounded", "empty", "context_error", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
			_, store, _ := handoffHTTPServerWithConfig(t, Config{})
			// Real owner-scoped register and transcript rows exercise the isolation boundary.
			if _, err := store.DB().Exec(`INSERT INTO voice_context_registers(workspace_id,owner_subject_id,compact,compact_through,source_memory_boundary) VALUES('default','owner','owner compact','2026-11-12T10:00:00Z','0001-01-01T00:00:00Z'),('default','other','private compact','2026-11-12T10:00:00Z','0001-01-01T00:00:00Z'); INSERT INTO voice_transcript_turns(workspace_id,session_id,seq,role,text,at,received_at) VALUES('default','handoff-session',1,'user','owner recent','2026-11-12T11:00:00Z','2026-11-12T11:00:00Z');`); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) SELECT 'private-session',workspace_id,'other',device_id,'private-request',request_hash,1,state,created_at,updated_at FROM voice_sessions WHERE id='handoff-session'; INSERT INTO voice_transcript_turns(workspace_id,session_id,seq,role,text,at,received_at) VALUES('default','private-session',1,'user','private recent','2026-11-12T11:00:00Z','2026-11-12T11:00:00Z');`); err != nil {
				t.Fatal(err)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body struct {
					Input string `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				var input struct {
					Items        []contracts.MemoryContextItem `json:"memory_items"`
					Conversation string                        `json:"conversation"`
				}
				if err := json.Unmarshal([]byte(body.Input), &input); err != nil {
					t.Error(err)
				}
				if len(input.Items) != 1 || input.Items[0].Content != "owner fact" {
					t.Errorf("memory=%+v", input.Items)
				}
				if mode == "memory_only" {
					if input.Conversation != "" {
						t.Error("unexpected conversation")
					}
				} else if mode == "bounded" {
					if len([]rune(input.Conversation)) > 16 || !strings.Contains(input.Conversation, "owner recent") {
						t.Errorf("unbounded or missing recent context=%q", input.Conversation)
					}
				} else if !strings.Contains(input.Conversation, "owner compact") || !strings.Contains(input.Conversation, "owner recent") {
					t.Errorf("conversation=%q", input.Conversation)
				}
				if strings.Contains(body.Input, "private") {
					t.Error("owner isolation lost")
				}
				output := `{"status":"answer","text":"你说过这件事。"}`
				if mode == "not_found" {
					output = `{"status":"not_found"}`
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"output_text": output})
			}))
			defer server.Close()
			client, err := answer.FromEnv(func(key string) string {
				return map[string]string{"ZOE_ANSWER_BASE_URL": server.URL, "ZOE_ANSWER_MODEL": "fixture", "ZOE_ANSWER_API_KEY_FILE": "/fixture"}[key]
			}, func(string) ([]byte, error) { return []byte("fixture"), nil })
			if err != nil {
				t.Fatal(err)
			}
			cfg := Config{Now: func() time.Time { return now }, Answerer: client}
			if mode == "bounded" {
				cfg.RecallContextCharBudget = 16
			}
			cfg.ContextAssembler = func(ctx context.Context, p contracts.Principal, task, owner, query string, budget int) (contracts.MemoryContext, error) {
				if p.WorkspaceID != "default" || p.SubjectID != "owner" || owner != "owner" || task != "" || budget != 2048 || query != "question" {
					t.Fatal("memory scope/budget changed")
				}
				if mode == "context_error" {
					return contracts.MemoryContext{}, context.Canceled
				}
				if mode == "empty" {
					return contracts.MemoryContext{}, nil
				}
				return contracts.MemoryContext{Items: []contracts.MemoryContextItem{{Content: "owner fact"}}}, nil
			}
			svc := newService(store.DB(), cfg)
			defer svc.Stop()
			if mode != "memory_only" && mode != "empty" {
				svc.register = &ContextRegister{db: store.DB(), rawWindow: 2 * time.Hour}
			}
			ctx := context.Background()
			if mode == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			text, _, status := svc.recallAnswer(ctx, "default", "owner", "question")
			wantCalls := 1
			if mode == "empty" || mode == "context_error" || mode == "timeout" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want=%d", calls, wantCalls)
			}
			if mode == "not_found" || mode == "empty" {
				if text != recallNotFound || status != "not_found" {
					t.Fatalf("text=%q status=%s", text, status)
				}
			} else if mode == "context_error" || mode == "timeout" {
				if text != recallUnavailable {
					t.Fatal(text)
				}
			} else if status != "answer" {
				t.Fatalf("status=%s text=%q", status, text)
			}
		})
	}
}

func TestRecallSpeechRejectsMarkupAndIDs(t *testing.T) {
	for _, text := range []string{"**garden**", "[garden](https://example.com)", "12345678-1234-1234-1234-123456789abc"} {
		if got := recallSpeech(text); got != recallUnavailable {
			t.Errorf("unsafe speech=%q", got)
		}
	}
}
