package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodedMessage(t *testing.T, raw string) Message {
	t.Helper()
	var message Message
	if err := json.Unmarshal([]byte(raw), &message); err != nil {
		t.Fatal(err)
	}
	return message
}

func TestCompletionNewestFirstIdleSucceeded(t *testing.T) {
	messages := []Message{
		decodedMessage(t, `{"id":"msg_003","time":{"created":817682},"type":"idle","outcome":"succeeded"}`),
		decodedMessage(t, `{"id":"msg_002","time":{"created":815424},"type":"assistant"}`),
		decodedMessage(t, `{"id":"msg_001","time":{"created":800421},"type":"user"}`),
	}
	if _, done, err := Completion(messages); !done || err != nil {
		t.Fatalf("newest-first completion = done %v, err %v", done, err)
	}
}

func TestCompletionAscendingIdleSucceeded(t *testing.T) {
	messages := []Message{
		decodedMessage(t, `{"id":"msg_001","time":{"created":800421},"type":"user"}`),
		decodedMessage(t, `{"id":"msg_002","time":{"created":815424},"type":"assistant"}`),
		decodedMessage(t, `{"id":"msg_003","time":{"created":817682},"type":"idle","outcome":"succeeded"}`),
	}
	if _, done, err := Completion(messages); !done || err != nil {
		t.Fatalf("ascending completion = done %v, err %v", done, err)
	}
}

func TestCompletionNewestAssistantIsNotDone(t *testing.T) {
	messages := []Message{
		decodedMessage(t, `{"id":"msg_003","time":{"created":817682},"type":"assistant"}`),
		decodedMessage(t, `{"id":"msg_002","time":{"created":815424},"type":"idle","outcome":"succeeded"}`),
	}
	if _, done, err := Completion(messages); done || err != nil {
		t.Fatalf("newest assistant completion = done %v, err %v", done, err)
	}
}

func TestCompletionNewestIdleFailed(t *testing.T) {
	messages := []Message{
		decodedMessage(t, `{"id":"msg_003","time":{"created":817682},"type":"idle","outcome":"failed","error":"boom"}`),
		decodedMessage(t, `{"id":"msg_002","time":{"created":815424},"type":"assistant"}`),
	}
	if _, done, err := Completion(messages); !done || err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("failed completion = done %v, err %v", done, err)
	}
}

func TestCompletionNewestIdleInterrupted(t *testing.T) {
	messages := []Message{
		decodedMessage(t, `{"id":"msg_003","time":{"created":817682},"type":"idle","outcome":"interrupted"}`),
		decodedMessage(t, `{"id":"msg_002","time":{"created":815424},"type":"assistant"}`),
	}
	if _, done, err := Completion(messages); !done || !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted completion = done %v, err %v", done, err)
	}
}

func TestCompletionNewestIdleWithoutOutcomeIsNotDone(t *testing.T) {
	messages := []Message{
		decodedMessage(t, `{"id":"msg_003","time":{"created":817682},"type":"idle"}`),
		decodedMessage(t, `{"id":"msg_002","time":{"created":815424},"type":"assistant"}`),
	}
	if _, done, err := Completion(messages); done || err != nil {
		t.Fatalf("empty outcome completion = done %v, err %v", done, err)
	}
}

func TestCompletionUnorderedUsesLatestTimeThenID(t *testing.T) {
	messages := []Message{
		decodedMessage(t, `{"id":"msg_004","time":{"created":817682},"type":"idle","outcome":"succeeded"}`),
		decodedMessage(t, `{"id":"msg_003","time":{"created":817682},"type":"assistant"}`),
		decodedMessage(t, `{"id":"msg_001","time":{"created":800421},"type":"user"}`),
		decodedMessage(t, `{"id":"msg_002","time":{"created":815424},"type":"assistant"}`),
	}
	if _, done, err := Completion(messages); !done || err != nil {
		t.Fatalf("unordered completion = done %v, err %v", done, err)
	}
}

func TestClientBasicAuthAndSessionTitleIsNotIdempotency(t *testing.T) {
	var titles []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "u" || pass != "p" {
			t.Fatalf("auth = %q %q %v", user, pass, ok)
		}
		if r.URL.Path != "/api/session" {
			t.Fatalf("path %s", r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		titles = append(titles, body["title"])
		_ = json.NewEncoder(w).Encode(map[string]any{"data": Session{ID: "ses-" + string(rune('0'+len(titles))), Title: body["title"]}})
	}))
	defer s.Close()
	c, err := New(Config{BaseURL: s.URL, Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.CreateSession(context.Background(), "same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.CreateSession(context.Background(), "same")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID || len(titles) != 2 {
		t.Fatalf("sessions = %#v %#v titles=%v", a, b, titles)
	}
}

func TestCreateSessionOptionsSendAgentModelDirectoryAndPermissions(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["agent"] != "zoe-worker" || body["location"].(map[string]any)["directory"] != "/workspace" {
			t.Fatalf("body=%v", body)
		}
		if body["model"].(map[string]any)["providerID"] != "provider" {
			t.Fatalf("model=%v", body["model"])
		}
		if len(body["permissions"].([]any)) != 1 {
			t.Fatalf("permissions=%v", body["permissions"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": Session{ID: "session"}})
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, Password: "p"})
	if _, err := c.CreateSession(context.Background(), "title", SessionOptions{Agent: "zoe-worker", Model: map[string]string{"id": "model", "providerID": "provider"}, Directory: "/workspace", Permissions: []map[string]string{{"action": "read", "resource": "*", "effect": "allow"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestModelContextWindowUsesRealAPIShape(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/model" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6.1-sol","modelID":"gpt-6.1-sol","providerID":"cliproxyapi","limit":{"context":1050000}},{"id":"no-limit","modelID":"no-limit","providerID":"cliproxyapi"}]}`))
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, Password: "p"})
	if got, err := c.ModelContextWindow(context.Background(), "cliproxyapi", "gpt-6.1-sol"); err != nil || got != 1050000 {
		t.Fatalf("window=%d err=%v", got, err)
	}
	if _, err := c.ModelContextWindow(context.Background(), "cliproxyapi", "no-limit"); err == nil {
		t.Fatal("model without limit was accepted")
	}
}

func TestPrompt204IsNotCompletionAndCompletionNeedsMessage(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/s/prompt":
			w.WriteHeader(http.StatusNoContent)
		case "/api/session/s/message":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []Message{{Type: "idle"}}})
		}
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, Password: "p"})
	if err := c.Prompt(context.Background(), "s", "x"); err != nil {
		t.Fatal(err)
	}
	messages, err := c.Messages(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, done, _ := Completion(messages); done {
		t.Fatal("missing completion marker was terminal")
	}
}

func TestCompletionUsesLatestAssistantFinishOrError(t *testing.T) {
	m := Message{}
	m.Info.Role = "assistant"
	m.Info.Time.Completed = 42
	m.Info.Finish = "stop"
	_, ok, err := Completion(Status{Type: "idle"}, []Message{m})
	if !ok || err != nil {
		t.Fatalf("completion = %v %v", ok, err)
	}
	m.Info.Error = "failed"
	_, ok, err = Completion(Status{Type: "idle"}, []Message{m})
	if !ok || err == nil {
		t.Fatalf("error completion = %v %v", ok, err)
	}
}

func TestCompletionReturnsCompletedAssistantBeforeIdle(t *testing.T) {
	var messages []Message
	if err := json.Unmarshal([]byte(`[{"id":"assistant","type":"assistant","finish":"stop","content":[{"type":"text","text":"clarify?"}]},{"id":"idle","type":"idle","outcome":"succeeded"}]`), &messages); err != nil {
		t.Fatal(err)
	}
	got, done, err := Completion(messages)
	if err != nil || !done || got.ID != "assistant" || got.Parts[0]["text"] != "clarify?" {
		t.Fatalf("completion=%+v done=%v err=%v", got, done, err)
	}
}

func TestEvents(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: {\"type\":\"session.status\"}\n\n"))
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, Password: "p"})
	events, errs := c.Events(context.Background())
	e := <-events
	if e.Type != "session.status" {
		t.Fatal(e)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(e.Type) == "" {
		t.Fatal("empty event")
	}
}

func TestPermissionAndFormListReplyShapes(t *testing.T) {
	var replied bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/s/permission":
			json.NewEncoder(w).Encode(map[string]any{"data": []PermissionRequest{{ID: "p1", SessionID: "s", Action: "write", Resources: []string{"x"}, Metadata: map[string]any{"k": "v"}}}})
		case "/api/session/s/form":
			json.NewEncoder(w).Encode(map[string]any{"data": []FormRequest{{ID: "f1", SessionID: "s", Title: "Questions", Fields: []map[string]any{{"key": "q0"}}}}})
		case "/api/session/s/permission/p1/reply":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			replied = b["decision"] == "once"
		}
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, Password: "p"})
	permissions, err := c.PendingPermissions(context.Background(), "s")
	if err != nil || len(permissions) != 1 || permissions[0].Action != "write" {
		t.Fatalf("permissions=%+v err=%v", permissions, err)
	}
	forms, err := c.PendingForms(context.Background(), "s")
	if err != nil || len(forms) != 1 || forms[0].Title != "Questions" {
		t.Fatalf("forms=%+v err=%v", forms, err)
	}
	if err := c.ReplyPermission(context.Background(), "s", "p1", PermissionOnce, ""); err != nil || !replied {
		t.Fatalf("reply err=%v replied=%v", err, replied)
	}
}

func TestFormReplyUsesV2SessionFormEndpoint(t *testing.T) {
	var got map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session/s/form/f1/reply" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, Password: "p"})
	if err := c.ReplyForm(context.Background(), "s", "f1", map[string]any{"q0": "answer"}); err != nil {
		t.Fatal(err)
	}
	if got["answer"].(map[string]any)["q0"] != "answer" {
		t.Fatalf("body=%v", got)
	}
}
