package steward

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func choiceServer(t *testing.T, reply string, inspect func([]byte)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if inspect != nil {
			inspect(body)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"output_text": reply})
	}))
}

func configured(t *testing.T, base, timeout string) *Client {
	t.Helper()
	env := map[string]string{"ZOE_STEWARD_BASE_URL": base, "ZOE_STEWARD_MODEL": "configured-model", "ZOE_STEWARD_API_KEY_FILE": "/test/key", "ZOE_STEWARD_TIMEOUT_MS": timeout, "ZOE_STEWARD_THINKING": "disabled", "ZOE_STEWARD_REASONING_EFFORT": "none"}
	c, err := FromEnv(func(k string) string { return env[k] }, func(string) ([]byte, error) { return []byte("private-fixture-key"), nil })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParsedDefaultStewardTimeout(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"", 30 * time.Second}, {"1000", time.Second}, {"10000", 10 * time.Second}, {"60000", 60 * time.Second}} {
		c := configured(t, "http://localhost", tc.value)
		c.SetHTTPClient(&http.Client{Transport: stewardDeadlineTransport{t, tc.want}})
		if _, err := c.Choose(context.Background(), "private sentence", "none", "", nil); err == nil {
			t.Fatal("expected transport error")
		}
	}
}

type stewardDeadlineTransport struct {
	t    *testing.T
	want time.Duration
}

func (f stewardDeadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	deadline, ok := r.Context().Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining > f.want || remaining < f.want-100*time.Millisecond {
		f.t.Errorf("parsed Steward deadline=%v, want %v", remaining, f.want)
	}
	return nil, errors.New("private transport failure")
}
