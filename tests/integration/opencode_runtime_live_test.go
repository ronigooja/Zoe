package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/agents/opencode"
)

func TestOpenCodeDirectRuntimeOptIn(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("ZOE_LIVE_OPENCODE_URL"))
	password := strings.TrimSpace(os.Getenv("ZOE_LIVE_OPENCODE_PASSWORD"))
	if base == "" || password == "" {
		t.Skip("set ZOE_LIVE_OPENCODE_URL and ZOE_LIVE_OPENCODE_PASSWORD for the opt-in OpenCode integration test")
	}
	c, err := opencode.New(opencode.Config{BaseURL: base, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := c.CreateSession(ctx, "zoe-integration-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Prompt(ctx, s.ID, "Reply with exactly opencode-integration-ok"); err != nil {
		t.Fatal(err)
	}
}
