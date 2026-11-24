package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testClientMCPState = `{"channel":"mcp","type":"state","enabled_mcp_ids":["test_mcp"],"tools":{"test_mcp":{"tools":[{"name":"echo","inputSchema":{"type":"object"}}]},"disabled":{"tools":[{"name":"omit"}]}}}`

func TestClientMCPCatalogUnion(t *testing.T) {
	s := &clientSockets{devices: make(map[string]*clientSocket)}
	a, stopA := context.WithCancel(t.Context())
	b, stopB := context.WithCancel(t.Context())
	defer stopA()
	defer stopB()
	first := &clientSocket{ctx: a, cancel: stopA}
	second := &clientSocket{ctx: b, cancel: stopB}
	s.replace("first", first)
	s.replace("second", second)
	s.handleFrame(first, json.RawMessage(testClientMCPState))
	s.handleFrame(second, json.RawMessage(`{"channel":"mcp","type":"state","enabled_mcp_ids":["another"],"tools":{"another":{"tools":[{"name":"echo"}]}}}`))
	tools := s.mcpTools()
	if len(tools) != 2 || string(tools[0]["name"]) != `"another_echo"` || string(tools[1]["name"]) != `"test_mcp_echo"` {
		t.Fatalf("unexpected union: %s", tools)
	}
	s.remove("first", first)
	if tools := s.mcpTools(); len(tools) != 1 || string(tools[0]["name"]) != `"another_echo"` {
		t.Fatalf("unexpected tools after disconnect: %s", tools)
	}
}

func clientMCPFixture(t *testing.T) (*Server, string, string, string, string, *websocket.Conn) {
	t.Helper()
	s, base, instanceKey := clientSocketFixture(t, nil)
	newRunner := func(id string, scopes []string) string {
		key, err := GenerateInstanceKey()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.RegisterRunnerCredential(t.Context(), key, id, scopes); err != nil {
			t.Fatal(err)
		}
		return key
	}
	key := newRunner("runner", []string{"runner:control:read"})
	other := newRunner("other", []string{"runner:control:read"})
	readless := newRunner("readless", []string{"runner:observe"})
	at := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tasks(id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id) VALUES('task','test','test','user','profile','active',1,?,?,'instance','command','owner')`, []any{at, at}},
		{`INSERT INTO runs(id,task_id,attempt_id,status,fence,runner_id,authorized_device_id,command_id,goal,profile_id,runtime_route_id,model_route_id,role,harness,started_at,runtime_session_id) VALUES('run','task','attempt','active',1,'runner','instance','command','goal','profile','runtime','model','worker','opencode',?,'open-session')`, []any{at}},
	} {
		if _, err := s.store.DB().ExecContext(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	c := dialClientSocket(t, base, instanceKey, "installation")
	return s, base, key, other, readless, c
}

func readyClientMCPCatalog(t *testing.T, c *websocket.Conn, state string) {
	t.Helper()
	writeClientSocket(t, c, state)
	writeClientSocket(t, c, `{"channel":"sync"}`)
	_ = readClientSocket(t, c)
}

type clientMCPHTTPResult struct {
	status int
	body   string
	err    error
}

func callClientMCPHTTP(ctx context.Context, base, key, path, body string) clientMCPHTTPResult {
	method := http.MethodGet
	if body != "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, base+"/runner/v1/client-mcp/"+path, strings.NewReader(body))
	if err != nil {
		return clientMCPHTTPResult{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return clientMCPHTTPResult{err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return clientMCPHTTPResult{resp.StatusCode, strings.TrimSpace(string(data)), err}
}

func (r clientMCPHTTPResult) require(t *testing.T, status int, text string) {
	t.Helper()
	if r.err != nil || r.status != status || !strings.Contains(r.body, text) {
		t.Fatalf("HTTP response: status=%d, body=%s, err=%v; expected status %d and %q", r.status, r.body, r.err, status, text)
	}
}

func startClientMCPHTTP(t *testing.T, base, key, body string) <-chan clientMCPHTTPResult {
	t.Helper()
	done := make(chan clientMCPHTTPResult, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	t.Cleanup(cancel)
	go func() { done <- callClientMCPHTTP(ctx, base, key, "call", body) }()
	return done
}

func TestClientMCPCatalogAndRouting(t *testing.T) {
	s, base, key, other, readless, c := clientMCPFixture(t)
	readyClientMCPCatalog(t, c, testClientMCPState)
	callClientMCPHTTP(t.Context(), base, "", "tools", "").require(t, 401, "unauthenticated")
	callClientMCPHTTP(t.Context(), base, readless, "tools", "").require(t, 403, "forbidden")
	callClientMCPHTTP(t.Context(), base, key, "tools", "").require(t, 200, `{"tools":[{"inputSchema":{"type":"object"},"name":"test_mcp_echo"}]}`)
	for _, tc := range []struct{ key, session string }{{key, ""}, {key, "invalid"}, {other, "open-session"}} {
		callClientMCPHTTP(t.Context(), base, tc.key, "call", `{"session_id":"`+tc.session+`","call":{"name":"test_mcp_echo"}}`).require(t, 503, "mcp_unavailable")
	}
	done := startClientMCPHTTP(t, base, key, `{"session_id":"open-session","device_id":"elsewhere","owner_subject_id":"elsewhere","call":{"name":"test_mcp_echo","arguments":{"x":123,"session_id":"wrong"}}}`)
	frame := readClientSocket(t, c)
	if string(frame["channel"]) != `"mcp"` || string(frame["type"]) != `"call"` || string(frame["mcp_id"]) != `"test_mcp"` || !bytes.Contains(frame["jsonrpc"], []byte(`"params":{"arguments":{"x":123,"session_id":"wrong"},"name":"echo"}`)) {
		t.Fatalf("bad call frame: %s", frame)
	}
	writeClientSocket(t, c, `{"channel":"mcp","type":"result","request_id":"unknown","ok":true,"result":{"content":[]}}`)
	id := string(frame["request_id"])
	writeClientSocket(t, c, `{"channel":"mcp","type":"result","request_id":`+id+`,"ok":true,"result":{"content":[{"type":"text","text":"outcome"}]}}`)
	writeClientSocket(t, c, `{"channel":"mcp","type":"result","request_id":`+id+`,"ok":false,"error":{"code":"mcp_exited"}}`)
	(<-done).require(t, 200, `"text":"outcome"`)
	if _, err := s.store.DB().ExecContext(t.Context(), `UPDATE tasks SET owner_subject_id='other' WHERE id='task'`); err != nil {
		t.Fatal(err)
	}
	callClientMCPHTTP(t.Context(), base, key, "call", `{"session_id":"open-session","owner_subject_id":"owner","call":{"name":"test_mcp_echo"}}`).require(t, 503, "mcp_unavailable")
	if _, err := s.store.DB().ExecContext(t.Context(), `UPDATE tasks SET owner_subject_id='owner' WHERE id='task'; UPDATE runs SET status='verification_pending' WHERE id='run';`); err != nil {
		t.Fatal(err)
	}
	callClientMCPHTTP(t.Context(), base, key, "call", `{"session_id":"open-session","call":{"name":"test_mcp_echo"}}`).require(t, 503, "mcp_unavailable")
	if _, err := s.store.DB().ExecContext(t.Context(), `UPDATE runs SET status='active' WHERE id='run';
		INSERT INTO runner_observations(observation_id,idempotency_key,run_id,attempt_id,fence,runner_id,kind,occurred_at,body_hash,body)
		VALUES('end','end','run','attempt',1,'runner','runtime_terminated','',x'01','{"runtime_session_id":"open-session"}');`); err != nil {
		t.Fatal(err)
	}
	callClientMCPHTTP(t.Context(), base, key, "call", `{"session_id":"open-session","call":{"name":"test_mcp_echo"}}`).require(t, 503, "mcp_unavailable")
	readyClientMCPCatalog(t, c, `{"channel":"mcp","type":"state","enabled_mcp_ids":[],"tools":{}}`)
	callClientMCPHTTP(t.Context(), base, key, "tools", "").require(t, 200, `{"tools":[]}`)
}

func TestClientMCPReplacementDoesNotReplay(t *testing.T) {
	s, base, key, _, _, c := clientMCPFixture(t)
	readyClientMCPCatalog(t, c, testClientMCPState)
	request := `{"session_id":"open-session","call":{"name":"test_mcp_echo"}}`
	done := startClientMCPHTTP(t, base, key, request)
	_ = readClientSocket(t, c)
	instanceKey, err := GenerateInstanceKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := RotateInstanceKey(t.Context(), s.store, instanceKey); err != nil {
		t.Fatal(err)
	}
	replacement := dialClientSocket(t, base, instanceKey, "replacement")
	writeClientSocket(t, replacement, `{"channel":"sync"}`)
	if frame := readClientSocket(t, replacement); string(frame["channel"]) != `"transport"` {
		t.Fatalf("new connection received replayed frame: %s", frame)
	}
	(<-done).require(t, 503, "client_disconnected")
	callClientMCPHTTP(t.Context(), base, key, "tools", "").require(t, 200, `{"tools":[]}`)
	callClientMCPHTTP(t.Context(), base, key, "call", request).require(t, 503, "mcp_unavailable")
}

func TestClientMCPFailures(t *testing.T) {
	_, base, key, _, _, c := clientMCPFixture(t)
	readyClientMCPCatalog(t, c, testClientMCPState)
	call := `{"session_id":"open-session","call":{"name":"test_mcp_echo"}}`
	callClientMCPHTTP(t.Context(), base, key, "call", `{"session_id":"open-session","call":{"name":"disabled_omit"}}`).require(t, 503, "mcp_unavailable")
	for _, reason := range []string{"mcp_disabled", "mcp_restarted", "mcp_exited", "frame_too_large"} {
		done := startClientMCPHTTP(t, base, key, call)
		frame := readClientSocket(t, c)
		writeClientSocket(t, c, `{"channel":"mcp","type":"result","request_id":`+string(frame["request_id"])+`,"ok":false,"error":{"code":"`+reason+`"}}`)
		status := 503
		if reason == "frame_too_large" {
			status = 413
		}
		(<-done).require(t, status, reason)
	}
	args, err := json.Marshal(map[string]string{"value": strings.Repeat("a", 5000)})
	if err != nil {
		t.Fatal(err)
	}
	callClientMCPHTTP(t.Context(), base, key, "call", `{"session_id":"open-session","call":{"name":"test_mcp_echo","arguments":`+string(args)+`}}`).require(t, 413, "frame_too_large")
	done := startClientMCPHTTP(t, base, key, call)
	frame := readClientSocket(t, c)
	cancel := readClientSocket(t, c)
	if string(cancel["type"]) != `"cancel"` || !bytes.Equal(cancel["request_id"], frame["request_id"]) {
		t.Fatalf("unexpected cancel frame: %s", cancel)
	}
	(<-done).require(t, 504, "mcp_timeout")
	done = startClientMCPHTTP(t, base, key, call)
	_ = readClientSocket(t, c)
	if err := c.CloseNow(); err != nil {
		t.Fatal(err)
	}
	(<-done).require(t, 503, "client_disconnected")
	callClientMCPHTTP(t.Context(), base, key, "tools", "").require(t, 200, `{"tools":[]}`)
}
