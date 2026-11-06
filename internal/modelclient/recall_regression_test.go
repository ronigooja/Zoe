package modelclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/answer"
	"github.com/ronigooja/Zoe/internal/summarize"
)

func TestRecallResponsesThinkingControlsReasoningForBothModels(t *testing.T) {
	for _, kind := range []string{"summary", "answer"} {
		for _, tc := range []struct {
			name, model, thinking, effort, want string
			reasoning                           bool
		}{
			{"disabled_non_gpt", "deepseek", "disabled", "high", "none", true},
			{"enabled_configured", "deepseek", "enabled", "max", "max", true},
			{"enabled_none_verbatim", "deepseek", "enabled", "none", "none", true},
			{"enabled_empty_no_fallback", "gpt-6-test", "enabled", "", "", false},
			{"empty_legacy_default", "gpt-6-test", "", "", "low", true},
			{"empty_non_reasoning", "deepseek", "", "high", "", false},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					reasoning, ok := body["reasoning"].(map[string]any)
					if ok != tc.reasoning || (ok && reasoning["effort"] != tc.want) {
						t.Errorf("reasoning=%v want present=%t effort=%s", body["reasoning"], tc.reasoning, tc.want)
					}
					_, temperature := body["temperature"]
					wantTemperature := tc.thinking == "" && tc.model == "deepseek"
					if temperature != wantTemperature {
						t.Errorf("temperature present=%t want=%t", temperature, wantTemperature)
					}
					if _, ok := body["thinking"]; ok {
						t.Error("Responses must not send chat thinking")
					}
					output := `{"status":"not_found"}`
					if kind == "summary" {
						output = `[{"topic":"Plan","summary":"Agreed.","first_seq":1,"last_seq":1}]`
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"output_text": output})
				}))
				defer s.Close()
				prefix := "ZOE_" + strings.ToUpper(kind)
				values := map[string]string{prefix + "_BASE_URL": s.URL, prefix + "_MODEL": tc.model, prefix + "_THINKING": tc.thinking, prefix + "_REASONING_EFFORT": tc.effort, prefix + "_API_KEY_FILE": "/key"}
				get := func(n string) string { return values[n] }
				read := func(string) ([]byte, error) { return []byte("fixture"), nil }
				if kind == "summary" {
					c, err := summarize.FromEnv(get, read)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = c.Summarize(context.Background(), []summarize.Turn{{Seq: 1, Role: "user", Text: "Agreed."}}); err != nil {
						t.Fatal(err)
					}
				} else {
					c, err := answer.FromEnv(get, read)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = c.Answer(context.Background(), "What?", nil, nil); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestRecallEffortValidationAcceptsGatewaySetAndRejectsMinimal(t *testing.T) {
	for _, kind := range []string{"summary", "answer"} {
		for _, effort := range []string{"", "none", "low", "medium", "high", "max", "minimal"} {
			t.Run(kind+"/"+effort, func(t *testing.T) {
				prefix := "ZOE_" + strings.ToUpper(kind)
				values := map[string]string{prefix + "_BASE_URL": "https://gateway.example/v1", prefix + "_MODEL": "deepseek", prefix + "_REASONING_EFFORT": effort, prefix + "_API_KEY_FILE": "/key"}
				get := func(n string) string { return values[n] }
				read := func(string) ([]byte, error) { return []byte("fixture"), nil }
				var err error
				if kind == "summary" {
					_, err = summarize.FromEnv(get, read)
				} else {
					_, err = answer.FromEnv(get, read)
				}
				if (err != nil) != (effort == "minimal") {
					t.Fatalf("effort=%s err=%v", effort, err)
				}
			})
		}
	}
}

func TestRecallChatKeepsThinkingAndVerbatimEffort(t *testing.T) {
	for _, kind := range []string{"summary", "answer"} {
		t.Run(kind, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/responses" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				thinking, _ := body["thinking"].(map[string]any)
				if thinking["type"] != "disabled" || body["reasoning_effort"] != "max" || body["temperature"] != float64(0) {
					t.Errorf("chat fields=%v", body)
				}
				output := `{"status":"not_found"}`
				if kind == "summary" {
					output = `[{"topic":"Plan","summary":"Agreed.","first_seq":1,"last_seq":1}]`
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": output}}}})
			}))
			defer s.Close()
			prefix := "ZOE_" + strings.ToUpper(kind)
			values := map[string]string{prefix + "_BASE_URL": s.URL, prefix + "_MODEL": "deepseek", prefix + "_THINKING": "disabled", prefix + "_REASONING_EFFORT": "max", prefix + "_API_KEY_FILE": "/key"}
			get := func(n string) string { return values[n] }
			read := func(string) ([]byte, error) { return []byte("fixture"), nil }
			if kind == "summary" {
				c, err := summarize.FromEnv(get, read)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = c.Summarize(context.Background(), []summarize.Turn{{Seq: 1, Role: "user", Text: "Agreed."}}); err != nil {
					t.Fatal(err)
				}
			} else {
				c, err := answer.FromEnv(get, read)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = c.Answer(context.Background(), "What?", nil, nil); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
