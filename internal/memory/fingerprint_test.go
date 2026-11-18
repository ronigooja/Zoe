package memory

import (
	"github.com/ronigooja/Zoe/internal/backoff"
	"testing"
)

func TestWorkerFingerprintVector(t *testing.T) {
	got := backoff.Fingerprint("https://example.test/v1", "model<&", "high", "", []byte("fixture-key"))
	if got != "43f3dcbcd23a6f32af6ad6f797e65694eb3a1886b9d574e9e8399ad6c5e551b1" {
		t.Fatalf("fingerprint mismatch: %s", got)
	}
}
