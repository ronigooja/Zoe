package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

func TestPrompt204IsNotCompletionAndCompletionNeedsMessage(t *testing.T) {
	busy := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/s/prompt_async":
			busy = true
			w.WriteHeader(http.StatusNoContent)
		case "/session/status":
			if busy {
				_ = json.NewEncoder(w).Encode(map[string]Status{"s": {Type: "busy"}})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]Status{"s": {Type: "idle"}})
			}
		case "/session/s/message":
			_ = json.NewEncoder(w).Encode([]Message{{Info: struct {
				ID   string `json:"id"`
				Role string `json:"role"`
				Time struct {
					Created   int64 `json:"created"`
					Completed int64 `json:"completed"`
				} `json:"time"`
				Finish string `json:"finish"`
				Error  any    `json:"error,omitempty"`
			}{Role: "assistant"}}})
		}
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, Password: "p"})
	if err := c.PromptAsync(context.Background(), "s", map[string]string{"parts": "x"}); err != nil {
		t.Fatal(err)
	}
	status, _ := c.Status(context.Background())
	if _, done, _ := Completion(status["s"], nil); done {
		t.Fatal("204 was treated as completion")
	}
	busy = false
	messages, err := c.Messages(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, done, _ := Completion(status["s"], messages); done {
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
