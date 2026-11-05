package triage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func triageThinkingRequest(t *testing.T, model, style, thinking, effort string) map[string]any {
	t.Helper()
	var body map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/responses"
		if style == "chat" {
			wantPath = "/chat/completions"
		}
		if r.URL.Path != wantPath {
			t.Errorf("path=%s want=%s", r.URL.Path, wantPath)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if style == "chat" {
			_, _ = w.Write([]byte(chatResponse("task")))
		} else {
			_, _ = w.Write([]byte(response("task")))
		}
	}))
	defer s.Close()
	c := clientFromTestEnv(t, s, map[string]string{
		"ZOE_TRIAGE_MODEL": model, "ZOE_TRIAGE_API_STYLE": style,
		"ZOE_TRIAGE_THINKING": thinking, "ZOE_TRIAGE_REASONING_EFFORT": effort,
	})
	if got, err := c.Classify(context.Background(), "classify this sentence"); err != nil || got != Task {
		t.Fatalf("category=%s err=%v", got, err)
	}
	return body
}

func requireTriageEffort(t *testing.T, body map[string]any, effort string) {
	t.Helper()
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != effort {
		t.Fatalf("reasoning=%v want effort=%s", body["reasoning"], effort)
	}
	if body["thinking"] != nil || body["reasoning_effort"] != nil || body["temperature"] != nil {
		t.Fatalf("unexpected chat fields or temperature: %v", body)
	}
}

func TestTriageResponsesThinkingDisabledSendsEffortNone(t *testing.T) {
	requireTriageEffort(t, triageThinkingRequest(t, "deepseek-flash", "responses", "disabled", ""), "none")
}

func TestTriageResponsesThinkingDisabledOverridesConfiguredEffort(t *testing.T) {
	for _, model := range []string{"deepseek-flash", "gpt-6.1-sol"} {
		for _, effort := range []string{"low", "medium", "high", "max", "none"} {
			t.Run(model+"/"+effort, func(t *testing.T) {
				requireTriageEffort(t, triageThinkingRequest(t, model, "responses", "disabled", effort), "none")
			})
		}
	}
}

func TestTriageResponsesThinkingEmptyKeepsExistingBehaviorForGptModels(t *testing.T) {
	for _, model := range []string{"gpt-6.1-sol", "gpt-5.6-x", "o1-mini", "o3-mini"} {
		t.Run(model, func(t *testing.T) {
			requireTriageEffort(t, triageThinkingRequest(t, model, "responses", "", ""), "low")
			requireTriageEffort(t, triageThinkingRequest(t, model, "responses", "", "high"), "high")
		})
	}
	for _, model := range []string{"gpt-4o", "deepseek-flash"} {
		body := triageThinkingRequest(t, model, "responses", "", "high")
		if body["reasoning"] != nil || body["temperature"] != float64(0) {
			t.Fatalf("existing non-reasoning behavior changed: %v", body)
		}
	}
}

func TestTriageResponsesThinkingEnabledSendsConfiguredEffort(t *testing.T) {
	for _, model := range []string{"deepseek-flash", "gpt-6.1-sol"} {
		for _, effort := range []string{"", "none", "low", "medium", "high", "max"} {
			t.Run(model+"/"+effort, func(t *testing.T) {
				body := triageThinkingRequest(t, model, "responses", "enabled", effort)
				if effort != "" {
					requireTriageEffort(t, body, effort)
				} else if body["reasoning"] != nil || body["temperature"] != nil || body["thinking"] != nil {
					t.Fatalf("empty enabled effort must use provider default: %v", body)
				}
			})
		}
	}
}

func TestTriageAcceptsEffortNoneAndMaxAndStillRejectsMinimal(t *testing.T) {
	for _, effort := range []string{"", "none", "low", "medium", "high", "max", "minimal", "xhigh", "invalid"} {
		t.Run(effort, func(t *testing.T) {
			values := map[string]string{
				"ZOE_TRIAGE_BASE_URL": "https://gateway.example", "ZOE_TRIAGE_MODEL": "deepseek-flash",
				"ZOE_TRIAGE_API_KEY_FILE": filepathTemp(t, "key"), "ZOE_TRIAGE_REASONING_EFFORT": effort,
			}
			_, err := FromEnv(func(name string) string { return values[name] }, os.ReadFile)
			invalid := effort == "minimal" || effort == "xhigh" || effort == "invalid"
			if (err != nil) != invalid {
				t.Fatalf("effort=%q err=%v", effort, err)
			}
		})
	}
}

func TestTriageChatStyleThinkingBehaviorUnchanged(t *testing.T) {
	for _, thinking := range []string{"", "enabled", "disabled"} {
		for _, effort := range []string{"", "low", "high"} {
			t.Run(thinking+"/"+effort, func(t *testing.T) {
				body := triageThinkingRequest(t, "deepseek-flash", "chat", thinking, effort)
				if thinking == "" {
					if body["thinking"] != nil {
						t.Fatalf("unexpected thinking: %v", body)
					}
				} else if got, ok := body["thinking"].(map[string]any); !ok || got["type"] != thinking {
					t.Fatalf("thinking=%v want=%s", body["thinking"], thinking)
				}
				if effort == "" && body["reasoning_effort"] != nil || effort != "" && body["reasoning_effort"] != effort {
					t.Fatalf("reasoning_effort=%v want=%q", body["reasoning_effort"], effort)
				}
				if body["reasoning"] != nil || body["temperature"] != float64(0) {
					t.Fatalf("chat behavior changed: %v", body)
				}
			})
		}
	}
}
