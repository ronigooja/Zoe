package voice

import (
	"context"
	"testing"
)

func TestMemoryAndRecallProcessingFailure(t *testing.T) {
	svc := newService(nil, Config{})
	t.Cleanup(svc.Stop)
	for _, call := range []func(context.Context, string, string, string) (string, int, string){svc.recallAnswer, svc.memoryAnswer} {
		text, count, _ := call(context.Background(), "default", "owner", "restatement")
		if text != "Processing error." || count != 0 {
			t.Fatalf("text=%q count=%d", text, count)
		}
	}
}
