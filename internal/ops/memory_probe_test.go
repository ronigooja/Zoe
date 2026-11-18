package ops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMemoryProbeResponsesOnly(t *testing.T) {
	for _, tc := range []struct {
		status         int
		body, category string
	}{
		{200, `{"output_text":"OK"}`, ""},
		{200, `{}`, "invalid_response"},
		{401, `{}`, "authentication_failed"},
		{429, `{}`, "provider_http_error"},
		{500, `{}`, "provider_http_error"},
		{404, `{}`, "provider_http_error"},
	} {
		t.Run(tc.category+http.StatusText(tc.status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			result := probeMemory(context.Background(), MemoryConfig{BaseURL: server.URL + "/v1", Model: "test", Secret: "test-token"}, server.Client())
			if calls != 1 || result.ErrorCategory != tc.category || result.ModelAvailable != (tc.category == "") {
				t.Fatalf("calls=%d result=%+v", calls, result)
			}
		})
	}
}
