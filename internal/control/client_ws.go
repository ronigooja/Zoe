package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

var errClientFrameTooLarge = errors.New("client WebSocket frame too large")

type clientSockets struct {
	mu      sync.Mutex
	devices map[string]*clientSocket
	closed  bool
}

type clientSocket struct {
	conn     *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	owner    string
	device   string
	maxBytes int64
	catalog  clientMCPCatalog
	pending  map[string]chan clientMCPResult
	monitor  *monitorSubscription
}

func (s *Server) registerClientSocketRoutes() {
	s.clientSockets.devices = make(map[string]*clientSocket)
	s.device.HandleFunc("GET /v1/ws", s.handleClientSocket)
	s.RegisterShutdownHook(s.clientSockets.shutdown)
}

func (s *Server) handleClientSocket(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok || p.DeviceID == "" {
		writeError(w, apiError(401, "unauthenticated", "valid client principal required", nil))
		return
	}
	activity := make(chan struct{}, 1)
	start := time.Now()
	var lastActivity atomic.Int64
	touch := func() {
		lastActivity.Store(int64(time.Since(start)))
		select {
		case activity <- struct{}{}:
		default:
		}
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OnPingReceived: func(context.Context, []byte) bool { touch(); return true },
		OnPongReceived: func(context.Context, []byte) { touch() },
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer conn.CloseNow()
	socket := &clientSocket{conn: conn, ctx: ctx, cancel: cancel, owner: p.SubjectID, device: p.DeviceID, maxBytes: s.cfg.Limits.Int64("client_ws_frame_max_bytes"), pending: make(map[string]chan clientMCPResult)}
	defer socket.stopMonitor()
	conn.SetReadLimit(socket.maxBytes)
	if !s.clientSockets.replace(p.DeviceID, socket) {
		return
	}
	defer s.clientSockets.remove(p.DeviceID, socket)
	heartbeat := time.Duration(s.cfg.Limits.Int64("client_ws_heartbeat_seconds")) * time.Second
	go socket.heartbeat(heartbeat, activity, func() time.Duration {
		return time.Since(start) - time.Duration(lastActivity.Load())
	})
	for {
		kind, frame, err := conn.Read(ctx)
		if err != nil {
			return
		}
		touch()
		if kind == websocket.MessageText {
			s.clientSockets.handleFrame(socket, frame)
		}
	}
}

func (s *clientSockets) replace(device string, socket *clientSocket) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if old := s.devices[device]; old != nil {
		old.cancel()
	}
	s.devices[device] = socket
	return true
}

func (s *clientSockets) remove(device string, socket *clientSocket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	socket.cancel()
	if s.devices[device] == socket {
		delete(s.devices, device)
	}
}

func (s *clientSockets) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, socket := range s.devices {
		socket.cancel()
	}
	clear(s.devices)
}

func (c *clientSocket) heartbeat(heartbeat time.Duration, activity <-chan struct{}, silence func() time.Duration) {
	idle := time.NewTimer(heartbeat)
	defer idle.Stop()
	tick := time.NewTicker(heartbeat / 2)
	defer tick.Stop()
	var pingDone <-chan struct{}
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-activity:
			idle.Reset(heartbeat - silence())
		case <-idle.C:
			if remaining := heartbeat - silence(); remaining > 0 {
				idle.Reset(remaining)
				continue
			}
			c.cancel()
			return
		case <-pingDone:
			pingDone = nil
		case <-tick.C:
			if pingDone == nil {
				done := make(chan struct{})
				pingDone = done
				go func() {
					defer close(done)
					_ = c.conn.Ping(c.ctx)
				}()
			}
		}
	}
}

func (s *clientSockets) handleFrame(c *clientSocket, frame json.RawMessage) {
	var envelope struct {
		Channel *string `json:"channel"`
	}
	if json.Unmarshal(frame, &envelope) != nil || envelope.Channel == nil {
		return
	}
	if *envelope.Channel == "mcp" {
		s.handleMCPFrame(c, frame)
		return
	}
	if *envelope.Channel == "model_monitor" {
		c.handleMonitorFrame(frame)
		return
	}
	_ = c.write(c.ctx, map[string]any{
		"channel": "transport",
		"type":    "error",
		"error":   map[string]string{"code": "unknown_channel", "channel": *envelope.Channel},
	})
}

func (c *clientSocket) write(ctx context.Context, value any) error {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	frame := buf.Bytes()[:buf.Len()-1]
	if int64(len(frame)) > c.maxBytes {
		return errClientFrameTooLarge
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	defer cancel()
	return c.conn.Write(ctx, websocket.MessageText, frame)
}
