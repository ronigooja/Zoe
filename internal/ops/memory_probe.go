package ops

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/modelclient"
)

func probeMemory(ctx context.Context, c MemoryConfig, h *http.Client) ProbeResult {
	c, err := c.Normalized()
	if err != nil {
		return ProbeResult{Model: c.Model, ErrorCategory: "invalid_configuration"}
	}
	result := ProbeResult{Model: c.Model}
	path := c.SecretPath
	if c.Secret != "" {
		if !validToken(c.Secret) {
			result.ErrorCategory = "credential_unavailable"
			return result
		}
		file, err := os.CreateTemp("", "zoe-memory-probe-*")
		if err != nil {
			result.ErrorCategory = "credential_unavailable"
			return result
		}
		path = file.Name()
		defer os.Remove(path)
		_, writeErr := file.WriteString(c.Secret)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			result.ErrorCategory = "credential_unavailable"
			return result
		}
	}
	env := map[string]string{"PROBE_BASE_URL": c.BaseURL, "PROBE_MODEL": c.Model, "PROBE_API_KEY_FILE": path, "PROBE_REASONING_EFFORT": c.ReasoningEffort}
	client, err := modelclient.FromEnv("PROBE", func(k string) string { return env[k] }, os.ReadFile, 8000, 500, 60000)
	if err != nil {
		result.ErrorCategory = "credential_unavailable"
		return result
	}
	_ = client.SetStyle("responses")
	bounded := http.Client{Timeout: 8 * time.Second}
	if h != nil {
		bounded = *h
		if bounded.Timeout <= 0 || bounded.Timeout > 8*time.Second {
			bounded.Timeout = 8 * time.Second
		}
	}
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.SetHTTPClient(&bounded)
	text, err := client.Complete(ctx, "Reply with OK to confirm endpoint connectivity.", "Operator connectivity test.")
	if err != nil {
		var httpErr *modelclient.HTTPError
		var netErr net.Error
		switch {
		case errors.As(err, &httpErr):
			result.HTTPStatus = httpErr.StatusCode
			result.ErrorCategory = "provider_http_error"
			if httpErr.StatusCode == 401 || httpErr.StatusCode == 403 {
				result.ErrorCategory = "authentication_failed"
			}
		case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
			result.ErrorCategory = "timeout"
		case errors.As(err, &netErr):
			result.ErrorCategory = "network_error"
		default:
			result.HTTPStatus = http.StatusOK
			result.ErrorCategory = "invalid_response"
		}
		return result
	}
	result.HTTPStatus = http.StatusOK
	if strings.TrimSpace(text) == "" {
		result.ErrorCategory = "invalid_response"
	} else {
		result.ModelAvailable = true
	}
	return result
}
