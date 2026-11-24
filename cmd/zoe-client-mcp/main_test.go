package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStdioRelay(t *testing.T) {
	var changed atomic.Bool
	var calls atomic.Int32
	token := t.Name()
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /runner/v1/client-mcp/tools":
			name := "client_echo"
			if changed.Load() {
				name = "client_echo_new"
			}
			fmt.Fprintf(w, `{"tools":[{"name":%q,"inputSchema":{"type":"object"}}]}`, name)
		case "POST /runner/v1/client-mcp/call":
			calls.Add(1)
			var req struct {
				SessionID string   `json:"session_id"`
				Call      toolCall `json:"call"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID != "context-session" || req.Call.Name != "client_echo" || !strings.Contains(string(req.Call.Arguments), "untrusted-session") {
				t.Error("call did not forward trusted metadata separately from arguments")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{"content":[{"type":"text","text":"client result"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(h.Close)
	runner, err := newRunnerClient(h.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	in, send := io.Pipe()
	output, out := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serve(t.Context(), in, out, runner, 20*time.Millisecond)
		_ = out.Close()
	}()
	t.Cleanup(func() { _ = send.Close(); _ = output.Close(); _ = in.Close() })
	lines := make(chan string, 8)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	request := func(line string) {
		t.Helper()
		if _, err := fmt.Fprintln(send, line); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(fields ...string) {
		t.Helper()
		select {
		case line := <-lines:
			for _, field := range fields {
				if !strings.Contains(line, field) {
					t.Fatalf("response %s lacks %s", line, field)
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatal("stdio response timed out")
		}
	}
	request(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	expect(`"id":1`, `"listChanged":true`, `"protocolVersion":"2025-11-25"`)
	request(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	request(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	expect(`"id":2`, `"name":"client_echo"`)
	request(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"client_echo","arguments":{"_meta":{"ai.opencode/sessionID":"untrusted-session"}}}}`)
	expect(`"id":3`, `"isError":true`, `mcp_unavailable`)
	if calls.Load() != 0 {
		t.Fatal("missing session reached runner route")
	}
	request(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"client_echo","arguments":{"session_id":"untrusted-session"},"_meta":{"ai.opencode/sessionID":"context-session"}}}`)
	expect(`"id":4`, `client result`)
	changed.Store(true)
	expect(`"method":"notifications/tools/list_changed"`)
	request(`{"jsonrpc":"2.0","id":5,"method":"tools/list"}`)
	expect(`"id":5`, `"name":"client_echo_new"`)
	_ = send.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
