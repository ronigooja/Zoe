package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/memoryapi"
)

func workerClient(t *testing.T, handler http.HandlerFunc) *memoryapi.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := memoryapi.New(server.URL, path, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestRecallWorkerUnmodifiedContext(t *testing.T) {
	for _, mode := range []string{"found", "empty", "failure", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			r := newDeliveryRig(t)
			text := " \n**unmodified** 🙂\t" + strings.Repeat("memory 🙂\n", 2000) + "\x00 "
			calls := 0
			if mode != "disabled" {
				r.svc.recall.MemoryAPI = workerClient(t, func(w http.ResponseWriter, req *http.Request) {
					calls++
					var body map[string]string
					_ = json.NewDecoder(req.Body).Decode(&body)
					if req.URL.Path != "/v1/memory/search" || body["query"] != "register restatement" || body["user"] != memoryUser("default", "owner") {
						t.Errorf("request=%v path=%s", body, req.URL.Path)
					}
					if mode == "failure" {
						w.WriteHeader(503)
						return
					}
					items := []memoryapi.Item{}
					if mode == "found" {
						items = append(items, memoryapi.Item{Text: text})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
				})
			}
			got, _, _ := r.svc.recallAnswer(context.Background(), "default", "owner", "register restatement")
			switch mode {
			case "found":
				if got != text {
					t.Fatal("memory text changed")
				}
				ok, code := r.svc.appendContext(context.Background(), "gateway", got)
				if !ok {
					t.Fatal(code)
				}
				if strings.Join(r.texts, "") != text || len(r.texts) < 2 {
					t.Fatal("chunking changed content")
				}
				for _, chunk := range r.texts {
					if len(chunk) > 8000 {
						t.Fatal("oversized chunk")
					}
				}
			case "empty":
				if got != "" {
					t.Fatal(got)
				}
			default:
				if got != stewardErrorSpeech {
					t.Fatal(got)
				}
			}
			if mode != "disabled" && calls != 1 {
				t.Fatal(calls)
			}
		})
	}
}

func TestRecallWorkerDurableFailureAndEmpty(t *testing.T) {
	for _, mode := range []string{"disabled", "failure", "empty"} {
		t.Run(mode, func(t *testing.T) {
			cfg := Config{}
			if mode != "disabled" {
				cfg.MemoryAPI = workerClient(t, func(w http.ResponseWriter, req *http.Request) {
					if mode == "failure" {
						w.WriteHeader(503)
						return
					}
					_, _ = w.Write([]byte(`{"items":[]}`))
				})
			}
			store, svc := recallRecoveryFixture(t, cfg, 1)
			svc.dispatchRecall(context.Background())
			waitRecall(t, func() bool {
				var state string
				_ = store.DB().QueryRow(`SELECT status FROM voice_recall_jobs WHERE id='job-0'`).Scan(&state)
				return state == "done"
			})
			svc.dispatchRecall(context.Background())
			var tasks, replies int
			_ = store.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks)
			_ = store.DB().QueryRow(`SELECT count(*) FROM voice_fast_replies WHERE intent_kind='recall_answer'`).Scan(&replies)
			want := 1
			if mode == "empty" {
				want = 0
			}
			if tasks != 0 || replies != want {
				t.Fatalf("tasks=%d replies=%d want=%d", tasks, replies, want)
			}
			if want == 1 {
				var speech string
				_ = store.DB().QueryRow(`SELECT text FROM voice_fast_replies WHERE intent_kind='recall_answer'`).Scan(&speech)
				if speech != "Processing error." {
					t.Fatal(speech)
				}
			}
		})
	}
}
