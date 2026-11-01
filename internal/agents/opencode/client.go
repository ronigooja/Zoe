// Package opencode is the narrow HTTP client for Zoe's host OpenCode V2 server.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

var ErrOutcomeUnknown = errors.New("OpenCode operation outcome is unknown")

type HTTPError struct {
	Status     int
	Code, Body string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("OpenCode HTTP %d%s", e.Status, func() string {
		if e.Code == "" {
			return ""
		}
		return " (" + e.Code + ")"
	}())
}

type Config struct {
	BaseURL, Username, Password string
	Client                      *http.Client
	Transport                   http.RoundTripper
}
type Client struct {
	base               *url.URL
	username, password string
	client             *http.Client
}

type Session struct {
	ID       string `json:"id"`
	Title    string `json:"title,omitempty"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location,omitempty"`
}
type Status struct {
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}
type Message struct {
	Type    string `json:"type,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	Error   any    `json:"error,omitempty"`
	Info    struct {
		ID   string `json:"id"`
		Role string `json:"role"`
		Time struct {
			Created   int64 `json:"created"`
			Completed int64 `json:"completed"`
		} `json:"time"`
		Finish string `json:"finish"`
		Error  any    `json:"error,omitempty"`
	} `json:"info"`
	Parts []map[string]any `json:"parts,omitempty"`
}
type Event struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties,omitempty"`
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("OpenCode base URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if strings.TrimSpace(cfg.Password) == "" {
		return nil, errors.New("OpenCode server password is required")
	}
	hc := cfg.Client
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second, Transport: cfg.Transport}
	}
	if cfg.Username == "" {
		cfg.Username = "opencode"
	}
	return &Client{base: u, username: cfg.Username, password: cfg.Password, client: hc}, nil
}

func (c *Client) CreateSession(ctx context.Context, title string, options ...any) (Session, error) {
	var out struct {
		Data Session `json:"data"`
	}
	body := map[string]any{"title": title}
	if len(options) > 0 {
		if directory, ok := options[0].(string); ok && directory != "" {
			body["location"] = map[string]string{"directory": directory}
		}
	}
	if len(options) > 1 {
		if permissions, ok := options[1].([]map[string]string); ok {
			body["permissions"] = permissions
		}
	}
	b, _ := json.Marshal(body)
	_, err := c.do(ctx, http.MethodPost, "/api/session", b, &out)
	if err != nil {
		return out.Data, err
	}
	if out.Data.ID == "" {
		return out.Data, fmt.Errorf("%w: session response has no id", ErrOutcomeUnknown)
	}
	return out.Data, nil
}
func (c *Client) MoveSession(ctx context.Context, sessionID, directory string) error {
	b, _ := json.Marshal(map[string]string{"directory": directory})
	_, err := c.do(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sessionID)+"/move", b, nil)
	return err
}
func (c *Client) Prompt(ctx context.Context, sessionID, text string) error {
	b, _ := json.Marshal(map[string]string{"text": text})
	_, err := c.do(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sessionID)+"/prompt", b, nil)
	if err != nil {
		return fmt.Errorf("%w: prompt: %v", ErrOutcomeUnknown, err)
	}
	return nil
}
func (c *Client) PromptAsync(ctx context.Context, sessionID string, prompt any) error {
	b, err := json.Marshal(prompt)
	if err != nil {
		return err
	}
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	if text, ok := v["text"].(string); ok {
		return c.Prompt(ctx, sessionID, text)
	}
	return c.Prompt(ctx, sessionID, string(b))
}
func (c *Client) Active(ctx context.Context) (map[string]Status, error) {
	var out struct {
		Data map[string]Status `json:"data"`
	}
	_, err := c.do(ctx, http.MethodGet, "/api/session/active", nil, &out)
	return out.Data, err
}
func (c *Client) Status(ctx context.Context) (map[string]Status, error) { return c.Active(ctx) }
func (c *Client) Messages(ctx context.Context, id string) ([]Message, error) {
	var out struct {
		Data []Message `json:"data"`
	}
	_, err := c.do(ctx, http.MethodGet, "/api/session/"+url.PathEscape(id)+"/message", nil, &out)
	return out.Data, err
}
func (c *Client) Interrupt(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodPost, "/api/session/"+url.PathEscape(id)+"/interrupt", []byte("{}"), nil)
	if err != nil {
		return fmt.Errorf("%w: interrupt: %v", ErrOutcomeUnknown, err)
	}
	return nil
}
func (c *Client) Abort(ctx context.Context, id string) error { return c.Interrupt(ctx, id) }
func (c *Client) Events(context.Context) (<-chan Event, <-chan error) {
	out := make(chan Event)
	errs := make(chan error)
	close(out)
	close(errs)
	return out, errs
}

// Completion recognizes the V2 idle outcome and rejects all non-success outcomes.
func Completion(args ...any) (Message, bool, error) {
	var messages []Message
	if len(args) == 1 {
		messages, _ = args[0].([]Message)
	} else if len(args) == 2 {
		messages, _ = args[1].([]Message)
	}
	if len(messages) == 0 {
		return Message{}, false, nil
	}
	m := messages[len(messages)-1]
	if m.Type == "" && m.Info.Time.Completed != 0 {
		if m.Info.Error != nil {
			return m, true, fmt.Errorf("OpenCode assistant error: %v", m.Info.Error)
		}
		return m, true, nil
	}
	if m.Type != "idle" {
		return Message{}, false, nil
	}
	if m.Outcome == "" {
		return Message{}, false, nil
	}
	if m.Outcome == "interrupted" {
		return m, true, context.Canceled
	}
	if m.Outcome != "succeeded" {
		detail := "OpenCode execution failed"
		if m.Error != nil {
			detail += fmt.Sprintf(": %v", m.Error)
		}
		return m, true, errors.New(detail)
	}
	if m.Error != nil || m.Info.Error != nil {
		return m, true, fmt.Errorf("OpenCode assistant error")
	}
	return m, true, nil
}

func (c *Client) request(ctx context.Context, method, endpoint string, body []byte) (*http.Request, error) {
	u := *c.base
	p, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	u.Path = path.Join(c.base.Path, p.Path)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.username, c.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}
func (c *Client) do(ctx context.Context, method, endpoint string, body []byte, out any) (int, error) {
	req, err := c.request(ctx, method, endpoint, body)
	if err != nil {
		return 0, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(b, &e)
		return resp.StatusCode, &HTTPError{Status: resp.StatusCode, Code: e.Code, Body: string(b)}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode OpenCode response: %w", err)
	}
	return resp.StatusCode, nil
}
