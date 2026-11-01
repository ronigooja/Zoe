package ops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func catalogOptions(t *testing.T) (LifecycleOptions, string) {
	t.Helper()
	root := t.TempDir()
	o := LifecycleOptions{Root: root, DataRoot: root}
	if err := os.MkdirAll(filepath.Dir(openCodeConfigPath(o)), 0700); err != nil {
		t.Fatal(err)
	}
	return o, filepath.Join(root, "gateway")
}

func TestProviderModelMetadataOfficialFallbackAndSecretSafe(t *testing.T) {
	o, _ := catalogOptions(t)
	official := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"providers":{"p":{"models":{"gpt-test":{"limit":{"context":100,"input":80,"output":20}}}}}}`))
	}))
	defer official.Close()
	t.Setenv("ZOE_OPENCODE_OFFICIAL_CATALOG_URL", official.URL)
	oldTransport := http.DefaultTransport
	http.DefaultTransport = official.Client().Transport
	defer func() { http.DefaultTransport = oldTransport }()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":[{"id":"gpt-test"}]}`)) }))
	defer gateway.Close()
	result := Probe(context.Background(), ProviderConfig{BaseURL: gateway.URL, Model: "gpt-test", Secret: "secret"}, gateway.Client())
	if !result.OK() {
		t.Fatal(result)
	}
	enrichProviderModels(context.Background(), &result)
	if err := persistProviderModels(o, "p", result.Models); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.Root, "zoe-runner.env"), []byte("ZOE_OPENCODE_MODEL=\"p/gpt-test\"\nZOE_OPENCODE_PROVIDER_BASE_URL=\"https://example.test\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renderOpenCodeConfig(o); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{modelMetadataPath(o), openCodeConfigPath(o)} {
		b, _ := os.ReadFile(path)
		if !strings.Contains(string(b), `"context": 100`) || strings.Contains(string(b), "secret") {
			t.Fatalf("unsafe or missing metadata in %s: %s", path, b)
		}
	}
}

func TestProviderModelMetadataGatewayWinsAndFilters(t *testing.T) {
	official := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":{"gpt-test":{"limit":{"context":999,"input":999,"output":999}}}}`))
	}))
	defer official.Close()
	t.Setenv("ZOE_OPENCODE_OFFICIAL_CATALOG_URL", official.URL)
	oldTransport := http.DefaultTransport
	http.DefaultTransport = official.Client().Transport
	defer func() { http.DefaultTransport = oldTransport }()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-test","context_length":10},{"id":"gpt-image-x"},{"id":"bad key"},{"id":"default"}]}`))
	}))
	defer gateway.Close()
	result := Probe(context.Background(), ProviderConfig{BaseURL: gateway.URL, Model: "default", Secret: "secret"}, gateway.Client())
	if !result.OK() {
		t.Fatal(result)
	}
	enrichProviderModels(context.Background(), &result)
	b, _ := json.Marshal(result.Models)
	if strings.Contains(string(b), "gpt-image") || strings.Contains(string(b), "bad key") || !strings.Contains(string(b), `"context":10`) {
		t.Fatalf("metadata = %s", b)
	}
	if !strings.Contains(string(b), "default") {
		t.Fatalf("default missing: %s", b)
	}
}

func TestOfficialCatalogFailureDoesNotFailMetadata(t *testing.T) {
	o, _ := catalogOptions(t)
	t.Setenv("ZOE_OPENCODE_OFFICIAL_CATALOG_URL", "https://127.0.0.1:1/catalog.json")
	result := ProbeResult{Model: "gpt-test", Models: []ProviderModelMetadata{{ID: "gpt-test", Limit: ModelLimit{Context: 42}}}}
	enrichProviderModels(context.Background(), &result)
	if err := persistProviderModels(o, "p", result.Models); err != nil {
		t.Fatal(err)
	}
}

func TestRenderOpenCodeConfigWithoutMetadataKeepsSingleModel(t *testing.T) {
	o, _ := catalogOptions(t)
	if err := os.WriteFile(filepath.Join(o.Root, "zoe-runner.env"), []byte("ZOE_OPENCODE_MODEL=\"p/gpt-test\"\nZOE_OPENCODE_PROVIDER_BASE_URL=\"https://example.test\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renderOpenCodeConfig(o); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(openCodeConfigPath(o))
	if strings.Count(string(b), `"name": "gpt-test"`) != 1 {
		t.Fatalf("config = %s", b)
	}
}
