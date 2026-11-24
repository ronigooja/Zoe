package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"time"
)

type clientMCPTool struct {
	id, name   string
	definition map[string]json.RawMessage
}

type clientMCPCatalog map[string]clientMCPTool

type clientMCPResult struct {
	RequestID string          `json:"request_id"`
	OK        *bool           `json:"ok"`
	Result    json.RawMessage `json:"result"`
	Error     json.RawMessage `json:"error"`
}

func (s *Server) registerClientMCPRoutes() {
	s.runner.HandleFunc("GET /runner/v1/client-mcp/tools", s.handleClientMCPTools)
	s.runner.HandleFunc("POST /runner/v1/client-mcp/call", s.handleClientMCPCall)
}

func (s *clientSockets) handleMCPFrame(c *clientSocket, frame json.RawMessage) {
	var envelope struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(frame, &envelope) != nil {
		return
	}
	switch envelope.Type {
	case "state":
		s.receiveMCPState(c, frame)
	case "result":
		var result clientMCPResult
		if json.Unmarshal(frame, &result) != nil || result.OK == nil || (*result.OK && !bytes.HasPrefix(result.Result, []byte("{"))) || (!*result.OK && len(result.Error) == 0) {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if pending := c.pending[result.RequestID]; pending != nil && c.ctx.Err() == nil {
			delete(c.pending, result.RequestID)
			pending <- result
		}
	}
}

func (s *clientSockets) receiveMCPState(c *clientSocket, frame json.RawMessage) {
	var state struct {
		Enabled []string `json:"enabled_mcp_ids"`
		Tools   map[string]struct {
			Tools []map[string]json.RawMessage `json:"tools"`
		} `json:"tools"`
	}
	if json.Unmarshal(frame, &state) != nil || state.Enabled == nil || state.Tools == nil {
		return
	}
	catalog := make(clientMCPCatalog)
	enabled := make(map[string]bool, len(state.Enabled))
	for _, id := range state.Enabled {
		enabled[id] = true
	}
	for id, list := range state.Tools {
		if id == "" || !enabled[id] {
			continue
		}
		for _, definition := range list.Tools {
			var name string
			if json.Unmarshal(definition["name"], &name) != nil || name == "" {
				continue
			}
			key := id + "_" + name
			if _, duplicate := catalog[key]; duplicate {
				catalog[key] = clientMCPTool{}
				continue
			}
			definition["name"], _ = json.Marshal(key)
			catalog[key] = clientMCPTool{id: id, name: name, definition: definition}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c.catalog = catalog
}

func (s *clientSockets) mcpTools() []map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	devices := make([]string, 0, len(s.devices))
	for id := range s.devices {
		devices = append(devices, id)
	}
	sort.Strings(devices)
	union := make(clientMCPCatalog)
	for _, id := range devices {
		c := s.devices[id]
		if c.ctx.Err() != nil {
			continue
		}
		for name, tool := range c.catalog {
			old, exists := union[name]
			if !exists {
				union[name] = tool
			} else if old.id != tool.id || old.name != tool.name {
				union[name] = clientMCPTool{}
			}
		}
	}
	names := make([]string, 0, len(union))
	for name, tool := range union {
		if tool.id != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	tools := make([]map[string]json.RawMessage, 0, len(names))
	for _, name := range names {
		tools = append(tools, union[name].definition)
	}
	return tools
}

func (s *Server) handleClientMCPTools(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok || !HasRunnerScope(p, "runner:control:read") {
		writeError(w, apiError(403, "forbidden", "runner lacks required scope", nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": s.clientSockets.mcpTools()})
}

func (s *Server) handleClientMCPCall(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok || !HasRunnerScope(p, "runner:control:read") {
		writeError(w, apiError(403, "forbidden", "runner lacks required scope", nil))
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
		Call      struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"call"`
	}
	if err := decodeClientMCPCall(r.Body, &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, clientMCPError("frame_too_large"))
		} else {
			writeError(w, apiError(400, "invalid_request", "invalid JSON request", nil))
		}
		return
	}
	var owner, device string
	err := s.resolveClientMCPSession(r.Context(), p.RunnerID, req.SessionID, &owner, &device)
	if err != nil {
		writeError(w, err)
		return
	}
	timeout := time.Duration(s.cfg.Limits.Int64("client_mcp_call_timeout_seconds")) * time.Second
	result, err := s.clientSockets.callMCP(r.Context(), timeout, owner, device, req.Call.Name, req.Call.Arguments)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(result)
}

func decodeClientMCPCall(body io.Reader, req any) error {
	dec := json.NewDecoder(body)
	if err := dec.Decode(req); err != nil {
		return err
	}
	err := dec.Decode(new(any))
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("request must contain one JSON value")
	}
	return err
}

func (s *Server) resolveClientMCPSession(ctx context.Context, runnerID, sessionID string, owner, device *string) error {
	if sessionID == "" || runnerID == "" {
		return clientMCPError("mcp_unavailable")
	}
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT t.owner_subject_id,r.authorized_device_id FROM runs r JOIN tasks t ON t.id=r.task_id
		WHERE r.runtime_session_id=? AND r.runner_id=? AND r.harness='opencode' AND r.status='active'
		AND r.desired_control='run' AND r.stopped_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM runner_observations o WHERE o.run_id=r.id AND o.fence=r.fence
		  AND o.kind='runtime_terminated' AND json_extract(o.body,'$.runtime_session_id')=r.runtime_session_id)
		LIMIT 2`, sessionID, runnerID)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return clientMCPError("mcp_unavailable")
	}
	if err := rows.Scan(owner, device); err != nil {
		return err
	}
	if rows.Next() || *owner == "" || *device == "" {
		return clientMCPError("mcp_unavailable")
	}
	return rows.Err()
}

func (s *clientSockets) callMCP(ctx context.Context, timeout time.Duration, owner, device, name string, arguments json.RawMessage) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s.mu.Lock()
	c := s.devices[device]
	if c == nil || c.owner != owner || c.ctx.Err() != nil || c.catalog[name].id == "" {
		s.mu.Unlock()
		return nil, clientMCPError("mcp_unavailable")
	}
	tool := c.catalog[name]
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	id := hex.EncodeToString(nonce[:])
	pending := make(chan clientMCPResult, 1)
	c.pending[id] = pending
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(c.pending, id)
		s.mu.Unlock()
	}()
	params := map[string]any{"name": tool.name}
	if len(arguments) != 0 {
		params["arguments"] = arguments
	}
	call := map[string]any{"channel": "mcp", "type": "call", "request_id": id, "mcp_id": tool.id,
		"jsonrpc": map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": params}}
	err := c.write(ctx, call)
	if errors.Is(err, errClientFrameTooLarge) {
		return nil, clientMCPError("frame_too_large")
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && c.ctx.Err() == nil {
			return nil, clientMCPError("mcp_timeout")
		}
		return nil, clientMCPError("client_disconnected")
	}
	select {
	case result := <-pending:
		if *result.OK {
			return result.Result, nil
		}
		var failure struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(result.Error, &failure)
		fault := clientMCPError(failure.Code)
		if fault.Code == "mcp_error" {
			fault.Details = result.Error
		}
		return nil, fault
	case <-c.ctx.Done():
		return nil, clientMCPError("client_disconnected")
	case <-ctx.Done():
		if c.ctx.Err() != nil {
			return nil, clientMCPError("client_disconnected")
		}
		go func() { _ = c.write(c.ctx, map[string]string{"channel": "mcp", "type": "cancel", "request_id": id}) }()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, clientMCPError("mcp_timeout")
		}
		return nil, ctx.Err()
	}
}

func clientMCPError(code string) *APIError {
	status, message := http.StatusServiceUnavailable, "client MCP is unavailable for this session"
	switch code {
	case "mcp_unavailable":
	case "mcp_timeout":
		status, message = http.StatusGatewayTimeout, "client MCP call timed out"
	case "mcp_disabled":
		message = "client MCP was disabled"
	case "mcp_restarted":
		message = "client MCP was restarted"
	case "mcp_exited":
		message = "client MCP process exited"
	case "client_disconnected":
		message = "client disconnected"
	case "frame_too_large":
		status, message = http.StatusRequestEntityTooLarge, "client MCP frame exceeds the transport limit"
	default:
		code, message = "mcp_error", "client MCP tool failed"
	}
	return apiError(status, code, message, nil)
}
