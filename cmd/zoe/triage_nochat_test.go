package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTriageCommandFourCategoriesOnly(t *testing.T) {
	containerKeyFixture(t)
	for _, category := range []string{"progress", "recall", "memory", "task", "cancel", "confirm", "decline", "chat", "unknown"} {
		t.Run(category, func(t *testing.T) {
			root := t.TempDir()
			key := filepath.Join(root, "key")
			if err := os.WriteFile(key, []byte("test-key"), 0600); err != nil {
				t.Fatal(err)
			}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"output_text":%q}`, category)
			}))
			defer s.Close()
			if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_TRIAGE_BASE_URL="+s.URL+"\nZOE_TRIAGE_MODEL=m\nZOE_TRIAGE_API_KEY_FILE="+key+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			err := triageTestOperation([]string{"--root", root}, nil, &out, &errOut)
			if category == "chat" || category == "unknown" || category == "confirm" || category == "decline" {
				if err == nil || !strings.Contains(out.String()+errOut.String(), "FAIL") {
					t.Fatalf("command must fail for %q: err=%v out=%s stderr=%s", category, err, out.String(), errOut.String())
				}
			} else if err != nil || !strings.Contains(out.String(), "category="+category) {
				t.Fatalf("command rejected %q: err=%v out=%s stderr=%s", category, err, out.String(), errOut.String())
			}
		})
	}
}
