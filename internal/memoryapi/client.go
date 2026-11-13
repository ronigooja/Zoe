// Package memoryapi accesses the memory worker without owning memory storage.
package memoryapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/logging"
)

type Client struct {
	base, tokenFile string
	http            *http.Client
}
type Item struct {
	ID    string  `json:"id"`
	Text  string  `json:"text"`
	Score float64 `json:"score"`
}
type Error struct{ Kind string }

func (e *Error) Error() string { return "memory API: " + e.Kind }

func New(base, tokenFile string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, &Error{"configuration"}
	}
	if timeout <= 0 || timeout > 120*time.Second {
		timeout = 60 * time.Second
	}
	return &Client{strings.TrimRight(base, "/"), tokenFile, &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) call(ctx context.Context, kind string, input, output any) error {
	if c == nil {
		return &Error{"disabled"}
	}
	token, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return &Error{"credential"}
	}
	secret := strings.TrimSpace(string(token))
	if secret == "" || strings.ContainsAny(secret, "\r\n") {
		return &Error{"credential"}
	}
	body, err := json.Marshal(input)
	if err != nil {
		return &Error{"request_invalid"}
	}
	call := logging.ModelCallFromContext(ctx, "memory_"+kind)
	call.Input("", string(body))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/memory/"+kind, bytes.NewReader(body))
	if err != nil {
		return &Error{"configuration"}
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return &Error{"deadline_exceeded"}
		}
		return &Error{"transport"}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return &Error{"response_read"}
	}
	call.Output(string(data))
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Kind string `json:"error_kind"`
		}
		_ = json.Unmarshal(data, &failure)
		// Only allow protocol labels, never arbitrary server text, into errors.
		switch failure.Kind {
		case "unauthorized", "busy", "request_invalid", "body_too_large", "not_found", "deadline_exceeded", "backend_unavailable":
			return &Error{failure.Kind}
		}
		return &Error{"http_status"}
	}
	if json.Unmarshal(data, output) != nil {
		return &Error{"response_invalid"}
	}
	return nil
}

func (c *Client) Search(ctx context.Context, user, query string) ([]Item, error) {
	var out struct {
		Items []Item `json:"items"`
	}
	err := c.call(ctx, "search", map[string]string{"user": user, "query": query}, &out)
	return out.Items, err
}
func (c *Client) Write(ctx context.Context, user, sentence, restatement string) error {
	var out struct {
		Operations []json.RawMessage `json:"operations"`
	}
	return c.call(ctx, "write", map[string]string{"user": user, "sentence": sentence, "restatement": restatement}, &out)
}

func (c *Client) Ingest(ctx context.Context, user, text string) (added, updated int, err error) {
	var out struct {
		Added   int `json:"added"`
		Updated int `json:"updated"`
	}
	err = c.call(ctx, "ingest", map[string]string{"user": user, "text": text}, &out)
	return out.Added, out.Updated, err
}

// User uses a JSON tuple so workspace/owner separators cannot collide.
func User(workspace, owner string) string {
	data, _ := json.Marshal([]string{workspace, owner})
	return string(data)
}
