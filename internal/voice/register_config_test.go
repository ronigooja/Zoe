package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegisterSeparateModelsAndRawWindow(t *testing.T) {
	f := newRegisterFixture(t)
	f.session("separate", "default", "owner")
	f.turn("separate", 0, "user", "eligible", f.now.Add(-45*time.Minute))
	f.turn("separate", 1, "user", "raw", f.now.Add(-15*time.Minute))
	routeCalls, compactCalls := 0, 0
	server := func(model, output string, calls *int) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct{ Model, Input string }
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.Model != model {
				t.Errorf("model = %q, want %q", request.Model, model)
			}
			*calls++
			_ = json.NewEncoder(w).Encode(map[string]string{"output_text": output})
		}))
		t.Cleanup(s.Close)
		return s
	}
	route := server("routing-only", `{"route":"chat","restatement":""}`, &routeCalls)
	compact := server("compaction-only", "compact", &compactCalls)
	var err error
	f.r, err = NewRegister(f.db, RegisterConfig{
		Model:        RegisterModelConfig{BaseURL: route.URL, Model: "routing-only", APIKey: "fixture", Timeout: time.Second},
		CompactModel: RegisterModelConfig{BaseURL: compact.URL, Model: "compaction-only", APIKey: "fixture", Timeout: 5 * time.Second},
		RawWindow:    30 * time.Minute, CompactCharacters: 8000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.r.compactTimeout != 5*time.Second {
		t.Fatal("compaction timeout not propagated")
	}
	if !f.rebuild(f.now) {
		t.Fatal("custom raw window did not admit eligible source")
	}
	layers := f.layers(f.now)
	if !layers.CompactThrough.Equal(f.now.Add(-30*time.Minute)) || !layers.RawFrom.Equal(layers.CompactThrough) || strings.Contains(layers.Raw, "eligible") || !strings.Contains(layers.Raw, "raw") {
		t.Fatal("custom compact edge is not contiguous with raw")
	}
	if _, err := f.r.Route(context.Background(), f.owner, "hello", f.now); err != nil {
		t.Fatal(err)
	}
	if routeCalls != 1 || compactCalls != 1 {
		t.Fatalf("route calls=%d compact calls=%d", routeCalls, compactCalls)
	}
}

func TestRegisterRequiresCompactModel(t *testing.T) {
	f := newRegisterFixture(t)
	_, err := NewRegister(f.db, RegisterConfig{Model: RegisterModelConfig{BaseURL: "https://route.invalid", Model: "route", APIKey: "fixture"}, CompactCharacters: 8000})
	if err == nil || !strings.Contains(err.Error(), "ZOE_COMPACT_BASE_URL") {
		t.Fatalf("missing compaction model accepted: %v", err)
	}
}
