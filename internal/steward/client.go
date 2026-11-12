// Package steward manages task admission; it never executes work or reviews results.
package steward

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/modelclient"
)

type Action string

const (
	ActionNew    Action = "new"
	ActionAttach Action = "attach"
	ActionAnswer Action = "answer"
	ActionNone   Action = "none"
)

type Client struct {
	transport *modelclient.Client
	timeout   time.Duration
	http      *http.Client
}

// FromEnv applies only to ZOE_STEWARD_*; an empty base URL disables the model.
func FromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (*Client, error) {
	if getenv == nil || readFile == nil {
		return nil, errors.New("steward configuration functions are required")
	}
	timeout, err := modelclient.Timeout("ZOE_STEWARD", getenv, 30000, 200, 60000)
	if err != nil {
		return nil, err
	}
	c, err := modelclient.FromEnv("ZOE_STEWARD", getenv, readFile, 30000, 200, 60000)
	if err != nil || c == nil {
		return nil, err
	}
	if err := c.SetStyle(strings.TrimSpace(getenv("ZOE_STEWARD_API_STYLE"))); err != nil {
		return nil, errors.New("ZOE_STEWARD_API_STYLE must be responses or chat")
	}
	h := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second}, Timeout: timeout}
	c.SetHTTPClient(h)
	return &Client{transport: c, timeout: timeout, http: h}, nil
}

func (c *Client) SetHTTPClient(h *http.Client) {
	if h != nil {
		c.http = h
		c.transport.SetHTTPClient(h)
	}
}
func (c *Client) Style() string { return c.transport.Style() }

// CallBudget applies before Choose starts its deadline.
func (c *Client) CallBudget(ctx context.Context) (time.Duration, time.Duration) {
	configured := c.timeout
	effective := configured
	if deadline, ok := ctx.Deadline(); ok {
		effective = min(effective, max(0, time.Until(deadline)))
	}
	if c.http != nil && c.http.Timeout > 0 {
		effective = min(effective, c.http.Timeout)
	}
	return configured, effective
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

type requestError struct{ err error }

func (e *requestError) Error() string { return "steward request failed: " + errorKind(e.err) }
func (e *requestError) Unwrap() error { return e.err }

// errorKind applies to diagnostics and never includes provider data or URLs.
func errorKind(err error) string {
	if err == nil {
		return "none"
	}
	var timeout interface{ Timeout() bool }
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var status *modelclient.HTTPError
	if errors.As(err, &status) {
		return "http_status"
	}
	return "transport"
}
