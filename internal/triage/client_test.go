package triage

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
)

func testClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	key := filepathTemp(t, " secret-key \n")
	client, err := FromEnv(func(name string) string {
		return map[string]string{"ZOE_TRIAGE_BASE_URL": server.URL + "/v1", "ZOE_TRIAGE_MODEL": "gpt-6-triage-model", "ZOE_TRIAGE_API_KEY_FILE": key}[name]
	}, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	return client, server
}

func TestTriageClientSendsConfiguredEffortForReasoningModels(t *testing.T) {
	for _, effort := range []string{"minimal", "low", "medium", "high"} {
		t.Run(effort, func(t *testing.T) {
			var got map[string]any
			client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				_, _ = w.Write([]byte(response("task")))
			}))
			defer server.Close()
			client.reasoningEffort = effort
			if _, err := client.Classify(context.Background(), "x"); err != nil {
				t.Fatal(err)
			}
			reasoning, ok := got["reasoning"].(map[string]any)
			if !ok || reasoning["effort"] != effort {
				t.Fatalf("reasoning=%v", got["reasoning"])
			}
		})
	}
}

func TestTriageClientEmptyEffortKeepsLowDefault(t *testing.T) {
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["reasoning"].(map[string]any)["effort"] != "low" {
			t.Fatalf("body=%v", got)
		}
		_, _ = w.Write([]byte(response("task")))
	}))
	defer server.Close()
	if _, err := client.Classify(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
}

func TestTriageClientDoesNotSendEffortForNonReasoningModel(t *testing.T) {
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		if _, ok := got["reasoning"]; ok {
			t.Fatalf("body=%v", got)
		}
		_, _ = w.Write([]byte(response("task")))
	}))
	defer server.Close()
	client.model = "gpt-4o"
	client.reasoningEffort = "high"
	if _, err := client.Classify(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
}

func filepathTemp(t *testing.T, value string) string {
	t.Helper()
	path := t.TempDir() + "/key"
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func response(word string) string {
	return `{"output":[{"type":"message","content":[{"type":"output_text","text":"` + word + `"}]}]}`
}

func TestClassifySendsTheNewInstruction(t *testing.T) {
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret-key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "triage-model" || body["input"] != "one sentence" {
			t.Errorf("body = %#v", body)
		}
		if _, ok := body["history"]; ok || strings.Contains(string(mustJSON(body)), "first sentence") {
			t.Errorf("request contained history: %#v", body)
		}
		if body["instructions"] != instructions {
			t.Error("missing system instruction")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response("task")))
	}))
	defer server.Close()
	if got, err := client.Classify(context.Background(), "one sentence"); err != nil || got != Task {
		t.Fatalf("classify = %q, %v", got, err)
	}
}

func TestInstructionDefinesAllFiveCategoriesWithoutExamples(t *testing.T) {
	for _, category := range []string{"progress", "recall", "memory", "task", "chat"} {
		if !strings.Contains(instructions, category+": ") {
			t.Errorf("instruction has no definition for %s", category)
		}
	}
	for _, phrase := range []string{"state, duration, or result", "past conversations or calls", "stable personal fact or preference", "requiring tools, research, files, devices, or the web", "needs no lookup"} {
		if !strings.Contains(instructions, phrase) {
			t.Errorf("instruction lacks definition phrase %q", phrase)
		}
	}
	if !strings.Contains(instructions, "unsure, choose task") || !strings.Contains(instructions, "exactly one lowercase word") {
		t.Fatalf("instruction lacks fallback or output rule: %s", instructions)
	}
	for _, r := range instructions {
		if unicode.Is(unicode.Han, r) {
			t.Fatalf("instruction contains CJK text: %s", instructions)
		}
	}
	if strings.ContainsAny(instructions, "\"'“”‘’") {
		t.Fatalf("instruction contains an example or CJK text: %s", instructions)
	}
}

func TestInstructionStaysCompact(t *testing.T) {
	if len([]byte(instructions)) > 2500 {
		t.Fatalf("instruction is %d bytes", len([]byte(instructions)))
	}
}

func mustJSON(value any) []byte { b, _ := json.Marshal(value); return b }

func TestClassifyReturnsEachCategory(t *testing.T) {
	words := []struct {
		text string
		want Category
	}{{"PROGRESS", Progress}, {"recall.", Recall}, {"memory!", Memory}, {"task,", Task}, {"chat?", Chat}}
	for _, tc := range words {
		t.Run(string(tc.want), func(t *testing.T) {
			client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(response(tc.text))) }))
			defer server.Close()
			if got, err := client.Classify(context.Background(), "sentence"); err != nil || got != tc.want {
				t.Fatalf("classify = %q, %v", got, err)
			}
		})
	}
}

func TestClassifyRejectsUnknownEmptyAndMultiWordOutput(t *testing.T) {
	for _, output := range []string{"unknown", "", "progress and task"} {
		t.Run(output, func(t *testing.T) {
			client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(response(output))) }))
			defer server.Close()
			if got, err := client.Classify(context.Background(), "sentence"); err == nil || got != "" {
				t.Fatalf("classify = %q, %v", got, err)
			}
		})
	}
}

func TestClassifyTimeoutAndHTTPErrorsReturnError(t *testing.T) {
	for _, status := range []int{500, 401, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", status) }))
			defer server.Close()
			if _, err := client.Classify(context.Background(), "sentence"); err == nil {
				t.Fatal("expected HTTP error")
			}
		})
	}
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(250 * time.Millisecond) }))
	client.http.Timeout = 20 * time.Millisecond
	defer server.Close()
	if _, err := client.Classify(context.Background(), "sentence"); err == nil {
		t.Fatal("expected timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Classify(ctx, "sentence"); err == nil {
		t.Fatal("expected cancellation")
	}
}

func TestClassifyIsStatelessAcrossCalls(t *testing.T) {
	var bodies []string
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, string(mustJSON(body)))
		_, _ = w.Write([]byte(response("chat")))
	}))
	defer server.Close()
	if _, err := client.Classify(context.Background(), "first sentence"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Classify(context.Background(), "second sentence"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || strings.Contains(bodies[1], "first sentence") {
		t.Fatalf("bodies = %v", bodies)
	}
}

func TestFromEnvDisabledWhenBaseURLEmptyAndRejectsBadValues(t *testing.T) {
	get := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	if client, err := FromEnv(get(map[string]string{}), os.ReadFile); client != nil || err != nil {
		t.Fatalf("disabled = %#v, %v", client, err)
	}
	for _, values := range []map[string]string{{"ZOE_TRIAGE_BASE_URL": "http://x"}, {"ZOE_TRIAGE_BASE_URL": "http://x", "ZOE_TRIAGE_MODEL": "m"}, {"ZOE_TRIAGE_BASE_URL": "http://x", "ZOE_TRIAGE_MODEL": "m", "ZOE_TRIAGE_API_KEY_FILE": "/missing"}, {"ZOE_TRIAGE_BASE_URL": "http://x", "ZOE_TRIAGE_MODEL": "m", "ZOE_TRIAGE_API_KEY_FILE": filepathTemp(t, "key"), "ZOE_TRIAGE_TIMEOUT_MS": "199"}} {
		if _, err := FromEnv(get(values), os.ReadFile); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("values %#v: %v", values, err)
		}
	}
}

func TestClassifyReusesConnection(t *testing.T) {
	var connections atomic.Int32
	listener := &countingListener{Listener: mustListen(t), count: &connections}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(response("chat"))) }))
	server.Listener = listener
	server.Start()
	defer server.Close()
	key := filepathTemp(t, "key")
	client, err := FromEnv(func(name string) string {
		return map[string]string{"ZOE_TRIAGE_BASE_URL": server.URL + "/v1", "ZOE_TRIAGE_MODEL": "m", "ZOE_TRIAGE_API_KEY_FILE": key}[name]
	}, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err = client.Classify(context.Background(), "sentence"); err != nil {
			t.Fatal(err)
		}
	}
	if connections.Load() >= 5 {
		t.Fatalf("new connections = %d", connections.Load())
	}
}

func TestClassifyOmitsTemperatureAndSendsReasoningForReasoningModels(t *testing.T) {
	models := []struct {
		model     string
		reasoning bool
	}{{"gpt-6.1-sol", true}, {"gpt-5.6-x", true}, {"o3-mini", true}, {"gpt-4o-mini", false}}
	for _, tc := range models {
		t.Run(tc.model, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				_, _ = w.Write([]byte(response("chat")))
			}))
			defer server.Close()
			key := filepathTemp(t, "key")
			client, err := FromEnv(func(name string) string {
				return map[string]string{"ZOE_TRIAGE_BASE_URL": server.URL + "/v1", "ZOE_TRIAGE_MODEL": tc.model, "ZOE_TRIAGE_API_KEY_FILE": key}[name]
			}, os.ReadFile)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Classify(context.Background(), "sentence"); err != nil {
				t.Fatal(err)
			}
			_, hasTemperature := body["temperature"]
			_, hasReasoning := body["reasoning"]
			if hasTemperature == tc.reasoning || hasReasoning != tc.reasoning {
				t.Fatalf("request = %#v", body)
			}
			if tc.reasoning && body["max_output_tokens"].(float64) < 128 {
				t.Fatalf("max_output_tokens = %v", body["max_output_tokens"])
			}
		})
	}
}

func TestClassifyParsesMessageItemAfterReasoningItem(t *testing.T) {
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"output":[{"type":"reasoning","content":[{"type":"summary_text","text":"not a category"}]},{"type":"message","content":[{"type":"output_text","text":"task"}]}]}`))
	}))
	defer server.Close()
	if got, err := client.Classify(context.Background(), "sentence"); err != nil || got != Task {
		t.Fatalf("classify = %q, %v", got, err)
	}
}

func TestClassifyRejectsIncompleteResponse(t *testing.T) {
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"task"}]}]}`))
	}))
	defer server.Close()
	if got, err := client.Classify(context.Background(), "sentence"); err == nil || got != "" {
		t.Fatalf("classify = %q, %v", got, err)
	}
}

func TestClassifyAcceptsTopLevelOutputTextOnlyResponse(t *testing.T) {
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"output_text":"recall"}`))
	}))
	defer server.Close()
	if got, err := client.Classify(context.Background(), "sentence"); err != nil || got != Recall {
		t.Fatalf("classify = %q, %v", got, err)
	}
}

func TestClassifyPrefersMessageItemOverTopLevelOutputText(t *testing.T) {
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"output_text":"memory","output":[{"type":"message","content":[{"type":"output_text","text":"task"}]}]}`))
	}))
	defer server.Close()
	if got, err := client.Classify(context.Background(), "sentence"); err != nil || got != Task {
		t.Fatalf("classify = %q, %v", got, err)
	}
}

func TestClassifyOutputTokenBudgetLeavesRoomForReasoning(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(response("chat")))
	}))
	defer server.Close()
	key := filepathTemp(t, "key")
	client, err := FromEnv(func(name string) string {
		return map[string]string{"ZOE_TRIAGE_BASE_URL": server.URL + "/v1", "ZOE_TRIAGE_MODEL": "gpt-6.1-sol", "ZOE_TRIAGE_API_KEY_FILE": key}[name]
	}, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Classify(context.Background(), "sentence"); err != nil {
		t.Fatal(err)
	}
	if body["max_output_tokens"].(float64) < 128 {
		t.Fatalf("max_output_tokens = %v", body["max_output_tokens"])
	}
}

type countingListener struct {
	net.Listener
	count *atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.count.Add(1)
	}
	return c, err
}
func mustListen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l
}
