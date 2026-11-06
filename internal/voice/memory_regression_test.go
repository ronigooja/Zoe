package voice

import "testing"

func TestMemoryRouteRegression(t *testing.T) {
	for _, category := range []string{"memory_read", "memory_write"} {
		t.Run(category, func(t *testing.T) {
			ts, _, key := handoffHTTPServerWithConfig(t, Config{Classifier: &fakeClassifier{category: category}})
			r := commitAndHandoff(t, ts, key, "memory-route", "private request")
			if r.ResourceType != "voice_turn" {
				t.Fatalf("memory route created %s", r.ResourceType)
			}
		})
	}
}
