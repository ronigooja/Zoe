package triage

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

type Category string

const (
	Progress Category = "progress"
	Recall   Category = "recall"
	Memory   Category = "memory"
	Task     Category = "task"
)

const instructions = `Classify one sentence delegated by the upstream voice model. Upstream alone decides when to delegate. The input is speech-to-text and may contain typos, filler words, or no punctuation.
progress: asks about the state, duration, or result of work already requested.
recall: asks what was said or done earlier in past conversations or calls.
memory: asks to store, update, or retrieve a stable personal fact or preference.
task: any delegated request that is not progress, recall, or memory, including new work or lookup requiring tools, research, files, devices, or the web. High-impact or destructive requests are task.
If a request could be both progress and a new task, or you are unsure, choose task. Reply with exactly one lowercase word: progress, recall, memory, or task. Output nothing else and do not explain.`

func reasoningModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(model, "gpt-6") || strings.HasPrefix(model, "gpt-5.6") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3")
}

type Client struct {
	baseURL         string
	model           string
	reasoningEffort string
	apiStyle        string // empty means auto
	thinking        string
	styleMu         sync.RWMutex
	apiKey          string
	http            *http.Client
}

func (c *Client) Style() string {
	if c == nil {
		return ""
	}
	c.styleMu.RLock()
	defer c.styleMu.RUnlock()
	if c.apiStyle == "" {
		return "auto"
	}
	return c.apiStyle
}

// SetHTTPClient is used by bounded operator probes and tests.
func (c *Client) SetHTTPClient(client *http.Client) {
	if c != nil && client != nil {
		c.http = client
	}
}

// HTTPError reports an unsuccessful HTTP response without exposing response data.
type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string {
	return fmt.Sprintf("triage request returned HTTP %d", e.StatusCode)
}

func FromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (*Client, error) {
	if getenv == nil || readFile == nil {
		return nil, errors.New("triage configuration functions are required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(getenv("ZOE_TRIAGE_BASE_URL")), "/")
	if baseURL == "" {
		return nil, nil
	}
	model := strings.TrimSpace(getenv("ZOE_TRIAGE_MODEL"))
	if model == "" {
		return nil, errors.New("ZOE_TRIAGE_MODEL is required when triage is enabled")
	}
	keyPath := strings.TrimSpace(getenv("ZOE_TRIAGE_API_KEY_FILE"))
	if keyPath == "" {
		return nil, errors.New("ZOE_TRIAGE_API_KEY_FILE is required when triage is enabled")
	}
	keyBytes, err := readFile(keyPath)
	if err != nil {
		return nil, errors.New("could not read triage API key file")
	}
	key := strings.TrimSpace(string(keyBytes))
	if key == "" {
		return nil, errors.New("triage API key file is empty")
	}
	timeout := 3 * time.Second
	if value := strings.TrimSpace(getenv("ZOE_TRIAGE_TIMEOUT_MS")); value != "" {
		ms, parseErr := strconv.Atoi(value)
		if parseErr != nil || ms < 200 || ms > 10000 {
			return nil, errors.New("ZOE_TRIAGE_TIMEOUT_MS must be between 200 and 10000")
		}
		timeout = time.Duration(ms) * time.Millisecond
	}
	effort := strings.TrimSpace(getenv("ZOE_TRIAGE_REASONING_EFFORT"))
	if effort != "" && effort != "none" && effort != "low" && effort != "medium" && effort != "high" && effort != "max" {
		return nil, errors.New("ZOE_TRIAGE_REASONING_EFFORT must be one of none, low, medium, high, max, or empty")
	}
	apiStyle := strings.TrimSpace(getenv("ZOE_TRIAGE_API_STYLE"))
	if apiStyle != "" && apiStyle != "responses" && apiStyle != "chat" {
		return nil, errors.New("ZOE_TRIAGE_API_STYLE must be responses or chat")
	}
	if err := validateBaseURL(baseURL); err != nil {
		return nil, err
	}
	thinking := strings.TrimSpace(getenv("ZOE_TRIAGE_THINKING"))
	if thinking != "" && thinking != "enabled" && thinking != "disabled" {
		return nil, errors.New("ZOE_TRIAGE_THINKING must be enabled, disabled, or empty")
	}
	transport := &http.Transport{MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second}
	return &Client{
		baseURL:         baseURL,
		model:           model,
		reasoningEffort: effort,
		apiStyle:        apiStyle,
		thinking:        thinking,
		apiKey:          key,
		http:            &http.Client{Transport: transport, Timeout: timeout},
	}, nil
}

func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("base URL must be an absolute URL without credentials, query, or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return errors.New("base URL must use HTTPS or local HTTP")
	}
	return nil
}

func (c *Client) Classify(ctx context.Context, text string) (Category, error) {
	output, err := c.Complete(ctx, text, instructions)
	if err != nil {
		return "", err
	}
	return parseCategory(output)
}

// Complete applies to independent bounded classifiers sharing this transport.
// It sends no conversation identifiers or previous messages.
func (c *Client) Complete(ctx context.Context, text, prompt string) (string, error) {
	if c == nil || c.http == nil || c.baseURL == "" || c.model == "" || c.apiKey == "" {
		return "", errors.New("triage client is not configured")
	}
	c.styleMu.Lock()
	defer c.styleMu.Unlock()
	style := c.apiStyle
	if style == "" {
		style = "responses"
	}
	_, body, status, err := c.call(ctx, style, text, prompt)
	if err != nil && c.apiStyle == "" && (status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented) {
		_, body, status, err = c.call(ctx, "chat", text, prompt)
		if err == nil {
			c.apiStyle = "chat"
			style = "chat"
		}
	}
	if err != nil {
		return "", err
	}
	if style == "responses" && c.apiStyle == "" {
		c.apiStyle = "responses"
	}
	if style == "chat" {
		return chatOutput(body)
	}
	return responsesOutput(body)
}

func (c *Client) call(ctx context.Context, style, text, prompt string) (Category, []byte, int, error) {
	request := map[string]any{"model": c.model}
	path := "/responses"
	if style == "chat" {
		path = "/chat/completions"
		request["messages"] = []map[string]string{{"role": "system", "content": prompt}, {"role": "user", "content": text}}
		request["max_tokens"] = 1024
		request["stream"] = false
		if c.reasoningEffort != "" {
			request["reasoning_effort"] = c.reasoningEffort
		}
		if c.thinking != "" {
			request["thinking"] = map[string]string{"type": c.thinking}
		}
	} else {
		request["instructions"] = prompt
		request["input"] = text
		request["max_output_tokens"] = 256
		request["store"] = false
	}
	if style == "responses" && (reasoningModel(c.model) || c.thinking != "") {
		effort := c.reasoningEffort
		if c.thinking == "disabled" {
			effort = "none"
		} else if effort == "" && c.thinking == "" {
			effort = "low"
		}
		if effort != "" {
			request["reasoning"] = map[string]string{"effort": effort}
		}
	} else {
		request["temperature"] = 0
	}
	body, err := json.Marshal(request)
	if err != nil {
		return "", nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return "", nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, resp.StatusCode, &HTTPError{StatusCode: resp.StatusCode}
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, resp.StatusCode, err
	}
	return "", responseBody, resp.StatusCode, nil
}

func parseResponsesResponse(responseBody []byte) (Category, error) {
	text, err := responsesOutput(responseBody)
	if err != nil {
		return "", err
	}
	return parseCategory(text)
}

func responsesOutput(responseBody []byte) (string, error) {
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
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return "", errors.New("invalid triage response")
	}
	if response.Status == "incomplete" {
		return "", errors.New("triage response was incomplete")
	}
	var output string
	for _, item := range response.Output {
		if item.Type != "message" {
			continue
		}
		for _, content := range item.Content {
			if content.Type == "output_text" && content.Text != "" {
				output = content.Text
				break
			}
		}
		if output != "" {
			break
		}
	}
	if output == "" {
		output = response.OutputText
	}
	return output, nil
}

func parseChatResponse(body []byte) (Category, error) {
	text, err := chatOutput(body)
	if err != nil {
		return "", err
	}
	return parseCategory(text)
}

func chatOutput(body []byte) (string, error) {
	var response struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", errors.New("invalid triage response")
	}
	if len(response.Choices) == 0 {
		return "", errors.New("triage response was incomplete")
	}
	if response.Choices[0].FinishReason != "" && response.Choices[0].FinishReason != "stop" && response.Choices[0].FinishReason != "length" {
		return "", errors.New("triage response was incomplete")
	}
	return response.Choices[0].Message.Content, nil
}

func parseCategory(output string) (Category, error) {
	word := strings.ToLower(strings.Trim(strings.TrimSpace(output), ".,!?;:\"'`"))
	if word == "" {
		return "", errors.New("triage response was empty")
	}
	if strings.ContainsAny(word, " \t\r\n") {
		return "", errors.New("triage response was not one word")
	}
	switch Category(word) {
	case Progress, Recall, Memory, Task:
		return Category(word), nil
	default:
		return "", errors.New("triage response was an unknown category")
	}
}
