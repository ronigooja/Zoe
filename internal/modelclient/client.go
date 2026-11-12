// Package modelclient implements the stateless endpoint protocol for recall models.
package modelclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Client struct {
	base, model, effort, thinking, key, style string
	http                                      *http.Client
	mu                                        sync.Mutex
}

type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string {
	return fmt.Sprintf("model request returned HTTP %d", e.StatusCode)
}

func Timeout(prefix string, getenv func(string) string, def, min, max int) (time.Duration, error) {
	ms := def
	if value := strings.TrimSpace(getenv(prefix + "_TIMEOUT_MS")); value != "" {
		var err error
		ms, err = strconv.Atoi(value)
		if err != nil || ms < min || ms > max {
			return 0, fmt.Errorf("%s_TIMEOUT_MS must be between %d and %d", prefix, min, max)
		}
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func FromEnv(prefix string, getenv func(string) string, readFile func(string) ([]byte, error), def, min, max int) (*Client, error) {
	if getenv == nil || readFile == nil {
		return nil, errors.New("model configuration functions are required")
	}
	base := strings.TrimRight(strings.TrimSpace(getenv(prefix+"_BASE_URL")), "/")
	if base == "" {
		return nil, nil
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("base URL must be absolute without credentials, query, or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return nil, errors.New("base URL must use HTTPS or local HTTP")
	}
	model := strings.TrimSpace(getenv(prefix + "_MODEL"))
	if model == "" {
		return nil, fmt.Errorf("%s_MODEL is required", prefix)
	}
	path := strings.TrimSpace(getenv(prefix + "_API_KEY_FILE"))
	if path == "" {
		return nil, fmt.Errorf("%s_API_KEY_FILE is required", prefix)
	}
	b, err := readFile(path)
	if err != nil {
		return nil, errors.New("could not read model API key file")
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return nil, errors.New("model API key file is empty")
	}
	effort := strings.TrimSpace(getenv(prefix + "_REASONING_EFFORT"))
	if effort != "" && effort != "none" && effort != "low" && effort != "medium" && effort != "high" && effort != "max" {
		return nil, errors.New("reasoning effort must be none, low, medium, high, max, or empty")
	}
	thinking := strings.TrimSpace(getenv(prefix + "_THINKING"))
	if thinking != "" && thinking != "enabled" && thinking != "disabled" {
		return nil, errors.New("thinking must be enabled, disabled, or empty")
	}
	timeout, err := Timeout(prefix, getenv, def, min, max)
	if err != nil {
		return nil, err
	}
	return &Client{base: base, model: model, effort: effort, thinking: thinking, key: key, http: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second}, Timeout: timeout}}, nil
}

func (c *Client) Style() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.style == "" {
		return "auto"
	}
	return c.style
}

// SetStyle applies when an adapter exposes an explicit endpoint-style setting.
// An empty style retains automatic Responses/Chat Completions negotiation.
func (c *Client) SetStyle(style string) error {
	if style != "" && style != "responses" && style != "chat" {
		return errors.New("API style must be responses or chat")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.style = style
	return nil
}
func (c *Client) SetHTTPClient(client *http.Client) {
	if c != nil && client != nil {
		c.http = client
	}
}

func (c *Client) Complete(ctx context.Context, instructions, input string) (string, error) {
	if c == nil || c.http == nil {
		return "", errors.New("model client is not configured")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	style := c.style
	if style == "" {
		style = "responses"
	}
	body, status, err := c.call(ctx, style, instructions, input)
	if err != nil && c.style == "" && (status == 404 || status == 405 || status == 501) {
		style = "chat"
		body, _, err = c.call(ctx, style, instructions, input)
	}
	if err != nil {
		return "", err
	}
	c.style = style
	if style == "chat" {
		var response struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(body, &response) != nil || len(response.Choices) == 0 {
			return "", errors.New("invalid model response")
		}
		choice := response.Choices[0]
		if choice.FinishReason != "" && choice.FinishReason != "stop" && choice.FinishReason != "length" {
			return "", errors.New("model response was incomplete")
		}
		return choice.Message.Content, nil
	}
	var response struct {
		Status     string `json:"status"`
		OutputText string `json:"output_text"`
		Output     []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if json.Unmarshal(body, &response) != nil {
		return "", errors.New("invalid model response")
	}
	if response.Status == "incomplete" {
		return "", errors.New("model response was incomplete")
	}
	for _, item := range response.Output {
		if item.Type == "message" {
			for _, content := range item.Content {
				if content.Type == "output_text" && content.Text != "" {
					return content.Text, nil
				}
			}
		}
	}
	return response.OutputText, nil
}

func (c *Client) call(ctx context.Context, style, instructions, input string) ([]byte, int, error) {
	request := map[string]any{"model": c.model}
	path := "/responses"
	if style == "chat" {
		path = "/chat/completions"
		request["messages"] = []map[string]string{{"role": "system", "content": instructions}, {"role": "user", "content": input}}
		request["max_tokens"] = 1024
		request["stream"] = false
		if c.effort != "" {
			request["reasoning_effort"] = c.effort
		}
		if c.thinking != "" {
			request["thinking"] = map[string]string{"type": c.thinking}
		}
	} else {
		request["instructions"] = instructions
		request["input"] = input
		request["max_output_tokens"] = 8192
		request["store"] = false
	}
	m := strings.ToLower(strings.TrimSpace(c.model))
	if style == "responses" && (c.thinking != "" || strings.HasPrefix(m, "gpt-6") || strings.HasPrefix(m, "gpt-5.6") || strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3")) {
		effort := c.effort
		if c.thinking == "disabled" {
			effort = "none"
		} else if c.thinking == "" && effort == "" {
			effort = "low"
		}
		if effort != "" {
			request["reasoning"] = map[string]string{"effort": effort}
		}
	} else {
		request["temperature"] = 0
	}
	b, err := json.Marshal(request)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return nil, 0, errors.New("could not create model request")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, errors.New("model request failed or timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, &HTTPError{StatusCode: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, errors.New("could not read model response")
	}
	return body, resp.StatusCode, nil
}

// Decode accepts exactly one JSON value with no unknown object fields.
func Decode(text string, value any) error {
	d := json.NewDecoder(strings.NewReader(text))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return errors.New("invalid model output JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("model output contains extra data")
	}
	return nil
}
