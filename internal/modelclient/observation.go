package modelclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/modelmonitor"
)

type observationContextKey struct{}

// WithObservationComponent identifies the task-summary use of the compact client.
func WithObservationComponent(ctx context.Context, component string) context.Context {
	if component != "task_summary" {
		return ctx
	}
	return context.WithValue(ctx, observationContextKey{}, component)
}

func observationComponent(prefix string) string {
	switch prefix {
	case "ZOE_REGISTER":
		return "register"
	case "ZOE_COMPACT":
		return "compact"
	case "ZOE_STEWARD":
		return "steward"
	case "ZOE_UI_CONTROLLER":
		return "ui_controller"
	default:
		return ""
	}
}

// Text is inherently sensitive. This is a best-effort filter, not a guarantee
// that arbitrary credentials or private data cannot appear in observations.
var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(bearer\s+)[a-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|password|secret|authorization)\b["'\s]*[:=]["'\s]*[^\s,;"'}]+`),
	regexp.MustCompile(`\b(sk|pk|ghp|github_pat|xox[baprs])[-_][A-Za-z0-9_-]{8,}`),
}

const observationTextLimit = 8192

func observationText(text, key string) (string, bool) {
	// Omit oversized content altogether, so a credential is never cut in half
	// before filtering and filtering work stays bounded.
	if len(text) > observationTextLimit {
		return "[omitted: content too large]", true
	}
	if key != "" {
		text = strings.ReplaceAll(text, key, "[redacted]")
	}
	for _, pattern := range credentialPatterns {
		text = pattern.ReplaceAllString(text, "[redacted]")
	}
	return text, false
}

type attemptObservation struct {
	component, id, key string
	generation         uint64
}

func (c *Client) observeAttempt(ctx context.Context, style, instructions, input string) *attemptObservation {
	component := c.component
	if override, ok := ctx.Value(observationContextKey{}).(string); ok {
		component = override
	}
	if component == "" {
		return nil
	}
	generation := modelmonitor.Generation()
	if generation == 0 {
		return nil
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil
	}
	o := &attemptObservation{component: component, id: hex.EncodeToString(id[:]), key: c.key, generation: generation}
	i, ti := observationText(instructions, c.key)
	v, tv := observationText(input, c.key)
	o.publish("request", map[string]any{"style": style, "instructions": i, "input": v}, ti || tv)
	return o
}

func (o *attemptObservation) publish(kind string, payload any, truncated bool) {
	if o == nil || modelmonitor.Generation() != o.generation {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	modelmonitor.PublishFor(o.generation, modelmonitor.Event{Component: o.component, CallID: o.id, Time: time.Now(), Type: kind, Payload: b, Truncated: truncated})
}

func (o *attemptObservation) endError(kind string, status int) {
	if o == nil {
		return
	}
	o.publish("error", map[string]any{"kind": kind, "status": status}, false)
}

func (o *attemptObservation) endUnavailable(status int) {
	if o == nil {
		return
	}
	o.publish("response", map[string]any{"status": status, "content_available": false}, false)
}

func (o *attemptObservation) endResponse(status int, content string) {
	if o == nil || modelmonitor.Generation() != o.generation {
		return
	}
	content, truncated := observationText(content, o.key)
	o.publish("response", map[string]any{"status": status, "content_available": true, "content": content}, truncated)
}
