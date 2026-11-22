package acceptance

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

func TestCapabilityHTTPCompositionRequiresConfiguredCapabilityID(t *testing.T) {
	store := openStore(t, ":memory:")
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	code, deviceToken := reviewToken(t), reviewToken(t)
	runnerToken := reviewToken(t)
	server, err := control.NewServer(store, control.Config{
		InstanceScopes: []string{
			"client:read", "client:command", "capability:read", "capability:release:submit",
			"capability:deployment:request", "capability:deployment:authorize",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = capabilities.Register(server, store, capabilities.Config{}); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterRunnerCredentialIfMissing(t.Context(), store, runnerToken, "capability-runner", []string{"capability:deploy"}); err != nil {
		t.Fatal(err)
	}
	handler := bindReviewHandler(server.Handler(), store)
	reviewInitializeInstance(t, handler, code, deviceToken)

	list := reviewRequest(handler, http.MethodGet, "/v1/capabilities/releases", deviceToken, "", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("release list route status=%d body=%s", list.Code, list.Body.String())
	}
	var page struct {
		Items []contracts.CapabilityRelease `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("empty capability registry exposed unexpected releases: %d", len(page.Items))
	}

	digest := "sha256:" + strings.Repeat("a", 64)
	manifest := contracts.CapabilityManifest{
		SchemaVersion: 1, ArtifactRef: "oci://registry.example/app@" + digest,
		ArtifactDigest: digest, Entrypoint: "/app/run", Capabilities: []string{"mail.read"},
		Budget:      contracts.CapabilityResourceBudget{CPUMillis: 1, MemoryBytes: 1, DiskBytes: 1, TimeoutSeconds: 1, Concurrency: 1},
		Network:     contracts.CapabilityNetworkPolicy{Mode: "none"},
		HealthCheck: &contracts.CapabilityHealthCheck{Kind: "http", Target: "/health", Port: 8080, IntervalSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 2},
	}
	request := contracts.CommandRequest{Kind: "capability.release.submit", Payload: mustJSON(t, map[string]any{"manifest": manifest})}
	response := reviewRequest(handler, http.MethodPost, "/v1/commands", deviceToken, "unregistered-capability", request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("release with unconfigured capability ID status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope contracts.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "capability_not_registered" {
		t.Fatalf("unconfigured capability ID error=%q body=%s", envelope.Error.Code, response.Body.String())
	}

	claim := reviewRequest(handler, http.MethodPost, "/runner/v1/deployments/claims", runnerToken, "", nil)
	if claim.Code != http.StatusServiceUnavailable {
		t.Fatalf("claim without a configured runtime adapter status=%d body=%s", claim.Code, claim.Body.String())
	}
	if err := json.Unmarshal(claim.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "capability_runner_unavailable" {
		t.Fatalf("unconfigured runtime adapter error=%q body=%s", envelope.Error.Code, claim.Body.String())
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
