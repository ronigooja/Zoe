package triage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Category string

const (
	Progress Category = "progress"
	Recall   Category = "recall"
	Memory   Category = "memory"
	Task     Category = "task"
	Chat     Category = "chat"
)

const instructions = "Classify the sentence into exactly one word: progress, recall, memory, task, or chat. Reply with exactly one of those five words and nothing else."

func reasoningModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(model, "gpt-6") || strings.HasPrefix(model, "gpt-5.6") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3")
}

type Client struct {
	baseURL         string
	model           string
	reasoningEffort string
	apiKey          string
	http            *http.Client
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
	timeout := 2 * time.Second
	if value := strings.TrimSpace(getenv("ZOE_TRIAGE_TIMEOUT_MS")); value != "" {
		ms, parseErr := strconv.Atoi(value)
		if parseErr != nil || ms < 200 || ms > 10000 {
			return nil, errors.New("ZOE_TRIAGE_TIMEOUT_MS must be between 200 and 10000")
		}
		timeout = time.Duration(ms) * time.Millisecond
	}
	effort := strings.TrimSpace(getenv("ZOE_TRIAGE_REASONING_EFFORT"))
	if effort != "" && effort != "minimal" && effort != "low" && effort != "medium" && effort != "high" {
		return nil, errors.New("ZOE_TRIAGE_REASONING_EFFORT must be one of minimal, low, medium, high, or empty")
	}
	transport := &http.Transport{MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second}
	return &Client{
		baseURL:         baseURL,
		model:           model,
		reasoningEffort: effort,
		apiKey:          key,
		http:            &http.Client{Transport: transport, Timeout: timeout},
	}, nil
}

func (c *Client) Classify(ctx context.Context, text string) (Category, error) {
	if c == nil || c.http == nil || c.baseURL == "" || c.model == "" || c.apiKey == "" {
		return "", errors.New("triage client is not configured")
	}
	request := map[string]any{
		"model":             c.model,
		"instructions":      instructions,
		"input":             text,
		"max_output_tokens": 256,
		"store":             false,
	}
	if reasoningModel(c.model) {
		effort := c.reasoningEffort
		if effort == "" {
			effort = "low"
		}
		request["reasoning"] = map[string]string{"effort": effort}
	} else {
		request["temperature"] = 0
	}
	body, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &HTTPError{StatusCode: resp.StatusCode}
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
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
	word := strings.ToLower(strings.Trim(strings.TrimSpace(output), ".,!?;:\"'`"))
	if strings.ContainsAny(word, " \t\r\n") {
		return "", errors.New("triage response was not one word")
	}
	switch Category(word) {
	case Progress, Recall, Memory, Task, Chat:
		return Category(word), nil
	default:
		return "", errors.New("triage response was an unknown category")
	}
}
