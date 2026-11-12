package ops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/modelclient"
)

func menuModelPrefix(kind string) (string, error) {
	switch kind {
	case "Register", "Compact", "Steward":
		return "ZOE_" + strings.ToUpper(kind), nil
	default:
		return "", errors.New("model kind must be Register, Compact, or Steward")
	}
}

func menuModelTimeout(kind string, c ModelConfig) (time.Duration, error) {
	prefix, err := menuModelPrefix(kind)
	if err != nil {
		return 0, err
	}
	def, min, max := 30000, 500, 60000
	if kind == "Compact" {
		def, min, max = 120000, 5000, 600000
	}
	if kind == "Steward" {
		def, min, max = 30000, 200, 60000
	}
	return modelclient.Timeout(prefix, func(string) string { return c.TimeoutMS }, def, min, max)
}

// TestMenuModel probes only the selected group's endpoint and credential.
func TestMenuModel(ctx context.Context, c ModelConfig, kind string, h *http.Client) (ModelProbeResult, error) {
	if kind == "Steward" {
		return TestSteward(ctx, c, h)
	}
	prefix, err := menuModelPrefix(kind)
	if err != nil {
		return ModelProbeResult{}, err
	}
	for _, item := range []struct{ suffix, value string }{{"BASE_URL", c.BaseURL}, {"MODEL", c.Model}, {"API_KEY_FILE", c.SecretPath}} {
		if strings.TrimSpace(item.value) == "" {
			return ModelProbeResult{}, fmt.Errorf("%s_%s is required; the server will not start without it", prefix, item.suffix)
		}
	}
	if err := ValidateModelKeyReadable(c.SecretPath); err != nil {
		return ModelProbeResult{}, err
	}
	env := map[string]string{prefix + "_BASE_URL": c.BaseURL, prefix + "_MODEL": c.Model, prefix + "_API_KEY_FILE": c.SecretPath, prefix + "_REASONING_EFFORT": c.ReasoningEffort, prefix + "_THINKING": c.Thinking, prefix + "_TIMEOUT_MS": c.TimeoutMS}
	def, min, max := 30000, 500, 60000
	if kind == "Compact" {
		def, min, max = 120000, 5000, 600000
	}
	client, err := modelclient.FromEnv(prefix, func(key string) string { return env[key] }, os.ReadFile, def, min, max)
	if err != nil {
		return ModelProbeResult{}, err
	}
	// A menu probe makes one HTTP request. Disable protocol retry without changing
	// the shared client's runtime behavior or falling back to another model group.
	if h == nil {
		h = &http.Client{}
	}
	copyHTTP := *h
	transport := copyHTTP.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copyHTTP.Transport = probeTransport{transport}
	timeout, err := modelclient.Timeout(prefix, func(key string) string { return env[key] }, def, min, max)
	if err != nil {
		return ModelProbeResult{}, err
	}
	copyHTTP.Timeout = timeout
	client.SetHTTPClient(&copyHTTP)
	start := time.Now()
	text, err := client.Complete(ctx, "Reply with OK to confirm endpoint connectivity.", "Operator connectivity test.")
	r := ModelProbeResult{Style: client.Style(), Latency: time.Since(start)}
	if err != nil {
		var httpErr *modelclient.HTTPError
		if errors.As(err, &httpErr) {
			r.HTTPStatus = httpErr.StatusCode
		}
		return r, err
	}
	if strings.TrimSpace(text) == "" {
		return r, errors.New("model returned an empty response")
	}
	r.HTTPStatus = http.StatusOK
	return r, nil
}

type probeTransport struct{ base http.RoundTripper }

func (p probeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := p.base.RoundTrip(req)
	if err == nil && (resp.StatusCode == 404 || resp.StatusCode == 405 || resp.StatusCode == 501) {
		_ = resp.Body.Close()
		return nil, &modelclient.HTTPError{StatusCode: resp.StatusCode}
	}
	return resp, err
}
