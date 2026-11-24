package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const catalogPollInterval = 5 * time.Second

type runnerClient struct {
	base  *url.URL
	token string
	http  *http.Client
}

func newRunnerClient(base, token string) (runnerClient, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return runnerClient{}, errors.New("ZOE_CONTROL_URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if token == "" {
		return runnerClient{}, errors.New("runner bearer is required")
	}
	client := &http.Client{
		Timeout:       61 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return runnerClient{base: u, token: token, http: client}, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "zoe-client-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	token := strings.TrimSpace(os.Getenv("ZOE_RUNNER_TOKEN"))
	if path := os.Getenv("ZOE_RUNNER_TOKEN_FILE"); token == "" && path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return errors.New("cannot read ZOE_RUNNER_TOKEN_FILE")
		}
		token = strings.TrimSpace(string(b))
	}
	runner, err := newRunnerClient(os.Getenv("ZOE_CONTROL_URL"), token)
	if err != nil {
		return err
	}
	return serve(context.Background(), os.Stdin, os.Stdout, runner, catalogPollInterval)
}

type rpcRequest struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type toolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type callParams struct {
	toolCall
	Meta struct {
		SessionID string `json:"ai.opencode/sessionID"`
	} `json:"_meta"`
}

type stdioServer struct {
	runner runnerClient
	mu     sync.Mutex
	out    *json.Encoder
	listed json.RawMessage
}

func serve(ctx context.Context, in io.Reader, out io.Writer, runner runnerClient, poll time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	s := &stdioServer{runner: runner, out: json.NewEncoder(out)}
	s.out.SetEscapeHTML(false)
	var work sync.WaitGroup
	defer func() {
		cancel()
		work.Wait()
	}()
	if poll > 0 {
		work.Go(func() { s.pollCatalog(ctx, poll) })
	}
	var activeMu sync.Mutex
	active := make(map[string]context.CancelFunc)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 34<<20)
	for scanner.Scan() {
		var req rpcRequest
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req.Version != "2.0" || req.Method == "" {
			s.reply(req.ID, nil, -32600, "invalid request")
			continue
		}
		if len(req.ID) == 0 {
			if req.Method == "notifications/cancelled" {
				var params struct {
					ID json.RawMessage `json:"requestId"`
				}
				if json.Unmarshal(req.Params, &params) == nil {
					activeMu.Lock()
					stop := active[string(params.ID)]
					activeMu.Unlock()
					if stop != nil {
						stop()
					}
				}
			}
			continue
		}
		if req.Method == "tools/call" {
			id := string(req.ID)
			callCtx, stop := context.WithCancel(ctx)
			activeMu.Lock()
			if active[id] != nil {
				activeMu.Unlock()
				stop()
				continue
			}
			active[id] = stop
			activeMu.Unlock()
			work.Go(func() {
				defer func() {
					stop()
					activeMu.Lock()
					delete(active, id)
					activeMu.Unlock()
				}()
				s.handle(callCtx, req)
			})
		} else {
			s.handle(ctx, req)
		}
	}
	return scanner.Err()
}

func (s *stdioServer) handle(ctx context.Context, req rpcRequest) {
	switch req.Method {
	case "initialize":
		var params struct {
			Version string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		switch params.Version {
		case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
		default:
			params.Version = "2025-11-25"
		}
		s.reply(req.ID, map[string]any{"protocolVersion": params.Version,
			"capabilities": map[string]any{"tools": map[string]bool{"listChanged": true}},
			"serverInfo":   map[string]string{"name": "zoe-client", "version": "1.0.0"}}, 0, "")
	case "ping":
		s.reply(req.ID, map[string]any{}, 0, "")
	case "tools/list":
		result, err := s.list(ctx)
		if err != nil {
			s.reply(req.ID, nil, -32603, "client MCP catalog unavailable")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.listed = result
		s.write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	case "tools/call":
		var params callParams
		if json.Unmarshal(req.Params, &params) != nil || params.Meta.SessionID == "" {
			s.reply(req.ID, toolError("mcp_unavailable", "missing OpenCode session ID", nil), 0, "")
			return
		}
		body, _ := json.Marshal(struct {
			SessionID string   `json:"session_id"`
			Call      toolCall `json:"call"`
		}{params.Meta.SessionID, params.toolCall})
		result, err := s.runner.request(ctx, http.MethodPost, "call", body)
		if err != nil {
			var failure *runnerError
			if !errors.As(err, &failure) {
				failure = &runnerError{Code: "mcp_unavailable", Message: "runner MCP route unavailable"}
			}
			s.reply(req.ID, toolError(failure.Code, failure.Message, failure.Details), 0, "")
			return
		}
		s.reply(req.ID, result, 0, "")
	default:
		s.reply(req.ID, nil, -32601, "method not found")
	}
}

func (s *stdioServer) list(ctx context.Context) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return s.runner.request(ctx, http.MethodGet, "tools", nil)
}

func (s *stdioServer) pollCatalog(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			baseline := s.listed
			s.mu.Unlock()
			if baseline == nil {
				continue
			}
			result, err := s.list(ctx)
			if err != nil || bytes.Equal(result, baseline) {
				continue
			}
			s.mu.Lock()
			if bytes.Equal(s.listed, baseline) {
				s.listed = result
				s.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
			}
			s.mu.Unlock()
		}
	}
}

func (s *stdioServer) reply(id json.RawMessage, result any, code int, message string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if code != 0 {
		response["error"] = map[string]any{"code": code, "message": message}
	} else {
		response["result"] = result
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.write(response)
}

func (s *stdioServer) write(v any) {
	_ = s.out.Encode(v)
}

func toolError(code, message string, details json.RawMessage) any {
	result := map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": code + ": " + message}}}
	if len(details) != 0 {
		result["structuredContent"] = map[string]any{"error": details}
	}
	return result
}

type runnerError struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

func (e *runnerError) Error() string { return e.Code }

func (c runnerClient) request(ctx context.Context, method, route string, body []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base.JoinPath("runner/v1/client-mcp", route).String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(io.LimitReader(resp.Body, 34<<20))
	if resp.StatusCode != http.StatusOK {
		var envelope struct {
			Error *runnerError `json:"error"`
		}
		if err := dec.Decode(&envelope); err == nil && envelope.Error != nil {
			return nil, envelope.Error
		}
		return nil, errors.New("runner MCP route unavailable")
	}
	var result json.RawMessage
	if err := dec.Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}
