package ops

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOpenCodeVersionOutput(t *testing.T) {
	tests := []struct {
		name, output string
		valid        bool
	}{
		{"upstream", "opencode v2.2.5", true},
		{"fork", "opencode v2.2.5-ronigooja.2", true},
		{"empty", "", false}, {"garbage", "garbage", false},
		{"multiline", "opencode v2.2.5\nnoise", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager(func(context.Context, string, ...string) ([]byte, error) { return []byte(tt.output), nil })
			err := m.checkOpenCodeVersion(context.Background(), LifecycleOptions{OpenCodeBinary: "opencode"})
			if (err == nil) != tt.valid {
				t.Fatalf("checkOpenCodeVersion(%q) error = %v", tt.output, err)
			}
		})
	}
}

func TestOpenCodeVersionCommandFailure(t *testing.T) {
	m := NewManager(func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("failed") })
	if err := m.checkOpenCodeVersion(context.Background(), LifecycleOptions{OpenCodeBinary: "opencode"}); err == nil || !strings.Contains(err.Error(), "cannot determine host OpenCode version") {
		t.Fatalf("error = %v", err)
	}
}
