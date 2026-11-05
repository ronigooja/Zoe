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
)

func testClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	key := filepathTemp(t, " secret-key \n")
	client, err := FromEnv(func(name string) string {
		return map[string]string{"ZOE_TRIAGE_BASE_URL": server.URL + "/v1", "ZOE_TRIAGE_MODEL": "triage-model", "ZOE_TRIAGE_API_KEY_FILE": key}[name]
	}, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	return client, server
}

func filepathTemp(t *testing.T, value string) string {
	t.Helper()
	path := t.TempDir() + "/key"
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func response(word string) string { return `{"output_text":"` + word + `"}` }

func TestClassifySendsOnlySystemInstructionAndSingleSentence(t *testing.T) {
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
		if _, ok := body["instructions"]; !ok {
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
