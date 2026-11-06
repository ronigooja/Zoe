package ops

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTriageProbeFourCategoriesOnly(t *testing.T) {
	modelKeyReadableFixture(t)
	for _, category := range []string{"progress", "recall", "memory", "task", "chat", "unknown"} {
		t.Run(category, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"output_text":%q}`, category)
			}))
			defer s.Close()
			key := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(key, []byte("test-key"), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := TestTriage(context.Background(), TriageConfig{BaseURL: s.URL, Model: "m", SecretPath: key}, nil)
			if category == "chat" || category == "unknown" {
				if err == nil {
					t.Fatalf("probe accepted removed/unknown category %q: %+v", category, result)
				}
			} else if err != nil || result.Category != category {
				t.Fatalf("probe rejected %q: result=%+v err=%v", category, result, err)
			}
		})
	}
}
