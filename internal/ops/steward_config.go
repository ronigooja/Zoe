package ops

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/steward"
)

// TestSteward applies the existing bounded menu probe pattern to task admission.
func TestSteward(ctx context.Context, c ModelConfig, h *http.Client, text ...string) (ModelProbeResult, error) {
	c, err := c.Normalized()
	if err != nil {
		return ModelProbeResult{}, err
	}
	if err := ValidateModelKeyReadable(c.SecretPath); err != nil {
		return ModelProbeResult{}, err
	}
	env := map[string]string{"BASE_URL": c.BaseURL, "MODEL": c.Model, "API_KEY_FILE": c.SecretPath, "REASONING_EFFORT": c.ReasoningEffort, "THINKING": c.Thinking, "API_STYLE": c.APIStyle}
	client, err := steward.FromEnv(func(key string) string { return env[strings.TrimPrefix(key, "ZOE_STEWARD_")] }, os.ReadFile)
	if err != nil {
		return ModelProbeResult{}, errors.New("invalid steward configuration")
	}
	if client == nil {
		return ModelProbeResult{}, errors.New("steward is disabled")
	}
	if h != nil {
		client.SetHTTPClient(h)
	}
	input := "research the current release notes"
	if len(text) > 0 {
		input = text[0]
	}
	start := time.Now()
	decision, err := client.Decide(ctx, input)
	result := ModelProbeResult{Latency: time.Since(start), Style: client.Style(), Category: string(decision)}
	if err == nil {
		result.HTTPStatus = http.StatusOK
	}
	return result, err
}
