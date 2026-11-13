package voice

import (
	"context"
	"testing"
)

func TestMemoryAndRecallProcessingFailure(t *testing.T) {
	svc := newService(nil, Config{})
	t.Cleanup(svc.Stop)
	text, count, _ := svc.recallAnswer(context.Background(), "default", "owner", "restatement")
	if text != "Processing error." || count != 0 {
		t.Fatalf("text=%q count=%d", text, count)
	}
}
