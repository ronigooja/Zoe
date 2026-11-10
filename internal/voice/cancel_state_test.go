package voice

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
)

func assertPlainChineseControlText(t *testing.T, text, want string) {
	t.Helper()
	if text != want {
		t.Fatalf("reply=%q want=%q", text, want)
	}
	for _, r := range text {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			t.Fatalf("ASCII English in reply %q", text)
		}
	}
}

func TestCancelPersistedStateTruth(t *testing.T) {
	for _, scenario := range []string{"none", "queued", "live", "complete", "failed", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			f := &pendingClassifier{category: "task"}
			ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: f})
			id := ""
			initialControlVersion := 0
			want := "现在没有在运行的任务。"
			if scenario != "none" {
				id = commitAndHandoff(t, ts, key, "work", "look this up").ResourceID
				want = "任务已经停止。"
			}
			if scenario == "live" {
				req, _ := http.NewRequest(http.MethodPost, ts.URL+"/runner/v1/claims", nil)
				req.Header.Set("Authorization", "Bearer BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA")
				resp, err := ts.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				var claim contracts.JobClaim
				err = json.NewDecoder(resp.Body).Decode(&claim)
				resp.Body.Close()
				if err != nil || resp.StatusCode != http.StatusOK || claim.TaskID != id {
					t.Fatalf("claim status=%d err=%v task=%q", resp.StatusCode, err, claim.TaskID)
				}
				want = "已经请求停止，正在停下。"
				if err := store.DB().QueryRow(`SELECT control_version FROM runs WHERE task_id=?`, id).Scan(&initialControlVersion); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "complete" || scenario == "failed" || scenario == "canceled" {
				if _, err := store.DB().Exec(`UPDATE tasks SET status=? WHERE id=?`, scenario, id); err != nil {
					t.Fatal(err)
				}
				if scenario == "complete" {
					want = "任务已经完成。"
				} else if scenario == "failed" {
					want = "任务已经失败。"
				}
			}
			f.category = "cancel"
			commitAndHandoff(t, ts, key, "cancel", "取消")
			// A distinct command receipt retrying the same turn must not stop twice.
			hash := sha256.Sum256([]byte("取消"))
			code, _, _ := handoffCommand(t, ts.Client(), ts.URL, key, "retry-cancel", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "cancel", "revision": 1, "content_hash": fmt.Sprintf("%x", hash[:])})
			if code != http.StatusOK {
				t.Fatalf("retry status=%d", code)
			}
			var text string
			if err := store.DB().QueryRow(`SELECT text FROM voice_fast_replies WHERE turn_id='cancel'`).Scan(&text); err != nil {
				t.Fatal(err)
			}
			assertPlainChineseControlText(t, text, want)
			var outgoing []string
			gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body struct {
					Text string `json:"text"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Errorf("gateway body: %v", err)
				}
				outgoing = append(outgoing, body.Text)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(gateway.Close)
			if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id='gw-cancel' WHERE id='handoff-session'`); err != nil {
				t.Fatal(err)
			}
			svc := newService(store.DB(), Config{GatewayURL: gateway.URL, GatewayAPIKey: "test-key", HTTPClient: gateway.Client()})
			svc.dispatchFastReplies(context.Background())
			svc.dispatchFastReplies(context.Background())
			if len(outgoing) != 1 {
				t.Fatalf("outgoing replies=%d", len(outgoing))
			}
			assertPlainChineseControlText(t, outgoing[0], want)
			var replies, stops int
			if err := store.DB().QueryRow(`SELECT count(*) FROM voice_fast_replies WHERE turn_id='cancel'`).Scan(&replies); err != nil {
				t.Fatal(err)
			}
			if err := store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='task.stop_requested' AND aggregate_id=?`, id).Scan(&stops); err != nil {
				t.Fatal(err)
			}
			wantStops := 0
			if scenario == "queued" || scenario == "live" {
				wantStops = 1
			}
			if replies != 1 || stops != wantStops {
				t.Fatalf("replies=%d stops=%d wantStops=%d", replies, stops, wantStops)
			}
			if id != "" {
				var status string
				if err := store.DB().QueryRow(`SELECT status FROM tasks WHERE id=?`, id).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if scenario == "queued" && status != "canceled" {
					t.Fatalf("queued stop status=%q", status)
				}
				if scenario == "live" {
					var desired string
					var version int
					if err := store.DB().QueryRow(`SELECT desired_control,control_version FROM runs WHERE task_id=?`, id).Scan(&desired, &version); err != nil {
						t.Fatal(err)
					}
					if status != "active" || desired != "stop" || version != initialControlVersion+1 || strings.Contains(text, "已经停止") {
						t.Fatalf("live status=%q desired=%q control_version=%d reply=%q", status, desired, version, text)
					}
				}
			}
		})
	}
}
