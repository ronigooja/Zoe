package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ronigooja/Zoe/internal/limits"
	"github.com/ronigooja/Zoe/internal/storage"
)

func clientSocketFixture(t *testing.T, scopes []string) (*Server, string, string) {
	t.Helper()
	ctx := t.Context()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, err := GenerateInstanceKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := InitializeInstanceAuth(ctx, store, key); err != nil {
		t.Fatal(err)
	}
	loaded := limits.Defaults()
	for key, value := range map[string]int64{"client_ws_heartbeat_seconds": 5, "client_ws_frame_max_bytes": 4096, "client_mcp_call_timeout_seconds": 1} {
		loaded, err = loaded.With(key, value)
		if err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewServer(store, Config{InstanceScopes: scopes, Limits: loaded})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewUnstartedServer(s.Handler())
	h.Config.ReadTimeout = 3 * time.Second
	h.Config.WriteTimeout = 3 * time.Second
	h.Start()
	t.Cleanup(h.Close)
	t.Cleanup(s.Shutdown)
	return s, h.URL, key
}

func dialClientSocket(t *testing.T, base, key, installation string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(t.Context(), base+"/v1/ws", &websocket.DialOptions{HTTPHeader: http.Header{
		"Authorization": {"Bearer " + key}, "X-Zoe-Installation-ID": {installation},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func writeClientSocket(t *testing.T, c *websocket.Conn, frame string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatal(err)
	}
}

func readClientSocket(t *testing.T, c *websocket.Conn) map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestClientSocketAuth(t *testing.T) {
	_, base, key := clientSocketFixture(t, []string{"client:read"})
	for _, tc := range []struct {
		key    string
		status int
	}{{"", 401}, {key, 403}} {
		_, resp, err := websocket.Dial(t.Context(), base+"/v1/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + tc.key}}})
		if err == nil || resp == nil || resp.StatusCode != tc.status {
			t.Fatalf("upgrade accepted or rejected with unexpected status: %v", err)
		}
	}
}

func TestClientSocketReplacementAndUnknownChannel(t *testing.T) {
	_, base, key := clientSocketFixture(t, nil)
	first := dialClientSocket(t, base, key, "first")
	writeClientSocket(t, first, `{"channel":"unregistered"}`)
	frame := readClientSocket(t, first)
	if string(frame["channel"]) != `"transport"` || string(frame["type"]) != `"error"` || string(frame["error"]) != `{"channel":"unregistered","code":"unknown_channel"}` {
		t.Fatalf("unexpected frame: %s", frame)
	}
	second := dialClientSocket(t, base, key, "second")
	writeClientSocket(t, second, `{"channel":"unregistered"}`)
	_ = readClientSocket(t, second)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, _, err := first.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("previous connection was not closed: %v", err)
	}
}

func TestClientSocketOversize(t *testing.T) {
	_, base, key := clientSocketFixture(t, nil)
	c := dialClientSocket(t, base, key, "")
	writeClientSocket(t, c, strings.Repeat("a", 4097))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("wanted close 1009: %v", err)
	}
}

func TestClientSocketHeartbeat(t *testing.T) {
	_, base, key := clientSocketFixture(t, nil)
	c := dialClientSocket(t, base, key, "")
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil || ctx.Err() == nil {
		t.Fatalf("active client should respond to pings and survive HTTP deadlines: %v", err)
	}
	if time.Since(start) < 5*time.Second {
		t.Fatal("active connection did not survive the heartbeat limit")
	}
	c = dialClientSocket(t, base, key, "")
	ctx, stop := context.WithTimeout(t.Context(), 7*time.Second)
	defer stop()
	// With no reader, the client does not respond to pings.
	<-time.After(5500 * time.Millisecond)
	if _, _, err := c.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("silent client did not close: %v", err)
	}
}
