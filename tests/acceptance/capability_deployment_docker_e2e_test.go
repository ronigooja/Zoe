package acceptance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/runner"
	"github.com/ronigooja/Zoe/internal/storage"
)

// This opt-in local-live acceptance test uses Docker and a public pinned base
// image. Capability containers have no network, no host mounts, and no Docker
// socket; Docker is used only by the trusted test/runner host.
func TestDockerBackedCapabilityDeploymentHTTPRecoveryLifecycle(t *testing.T) {
	if os.Getenv("ZOE_TEST_DOCKER_CAPABILITIES") != "1" {
		t.Skip("set ZOE_TEST_DOCKER_CAPABILITIES=1 to run the local Docker acceptance")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is unavailable")
	}
	if output, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("Docker daemon is unavailable: %s", strings.TrimSpace(string(output)))
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	image := capabilityTestImage(t, ctx)
	root := t.TempDir()
	databasePath := filepath.Join(root, "control.sqlite")
	journalPath := filepath.Join(root, "runner-journal.sqlite")
	bootstrapCode, deviceToken, runnerToken := reviewToken(t), reviewToken(t), reviewToken(t)
	runnerBinary := buildCapabilityRunner(t, root)
	faults := &capabilityHTTPFaults{}

	store, httpServer := startCapabilityHTTPFixture(t, databasePath, bootstrapCode, runnerToken, faults)
	defer func() {
		httpServer.Close()
		_ = store.Close()
	}()
	if _, _, err := control.InitializeInstanceAuth(ctx, store, deviceToken); err != nil {
		t.Fatal(err)
	}
	lineage, err := storage.InstanceLineage(ctx, store.DB())
	if err != nil {
		t.Fatal(err)
	}

	manifestV1 := capabilityServiceManifest(image, "--directory", "/tmp")
	releaseV1 := submitCapabilityRelease(t, httpServer.Client(), httpServer.URL, deviceToken, manifestV1, "release-v1")
	deploymentID, version := requestCapabilityDeployment(t, httpServer.Client(), httpServer.URL, deviceToken, "local-service", releaseV1, "install", nil, "install-request")
	containerName := "zoe-cap-" + capabilityShortHash(lineage+"\x00"+deploymentID)
	assertDockerObjectAbsent(t, ctx, "inspect", containerName)
	var containerID string
	t.Cleanup(func() {
		raw, err := exec.Command("docker", "inspect", "--format", "{{json .}}", containerName).Output()
		if err != nil {
			return
		}
		var inspected struct {
			ID     string `json:"Id"`
			Config struct {
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
		}
		if json.Unmarshal(raw, &inspected) == nil && inspected.ID != "" && (containerID == "" || inspected.ID == containerID) && inspected.Config.Labels["io.zoe.managed"] == "true" && inspected.Config.Labels["io.zoe.deployment"] == capabilityShortHash(deploymentID) && inspected.Config.Labels["io.zoe.instance_lineage"] == capabilityShortHash(lineage) {
			_ = exec.Command("docker", "rm", "--force", inspected.ID).Run()
		}
	})
	version = authorizeCapabilityDeployment(t, httpServer.Client(), httpServer.URL, deviceToken, deploymentID, version, "install", manifestV1.Budget, "install-authorize")

	faults.failNextDeploymentObservation.Store(true)
	if err := runCapabilityRunnerOnce(t, runnerBinary, httpServer.URL, runnerToken, journalPath); err == nil {
		t.Fatal("runner process unexpectedly accepted a deliberately failed receipt request")
	}
	observationState, acknowledged := capabilityJournalReceipt(t, journalPath, deploymentID)
	if observationState != "running" || acknowledged {
		t.Fatalf("runner journal did not preserve unacknowledged install receipt: state=%q acknowledged=%v", observationState, acknowledged)
	}
	containerID = capabilityContainerID(t, ctx, containerName)
	if err := runCapabilityRunnerOnce(t, runnerBinary, httpServer.URL, runnerToken, journalPath); err != nil {
		t.Fatalf("restarted runner failed to submit its durable receipt: %v", err)
	}
	if state, wasAcknowledged := capabilityJournalReceipt(t, journalPath, deploymentID); state != "running" || !wasAcknowledged {
		t.Fatalf("restarted runner did not acknowledge its durable install receipt: state=%q acknowledged=%v", state, wasAcknowledged)
	}
	if got := capabilityContainerID(t, ctx, containerName); got != containerID {
		t.Fatalf("runner receipt recovery replaced the installed container: before=%s after=%s", containerID, got)
	}

	// Restart the control process against the same SQLite database. The
	// deployment state and observation receipt must remain authoritative.
	httpServer.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, httpServer = startCapabilityHTTPFixture(t, databasePath, bootstrapCode, runnerToken, faults)
	defer func() {
		httpServer.Close()
		_ = store.Close()
	}()
	version = assertCapabilityDeploymentState(t, httpServer.Client(), httpServer.URL, deviceToken, deploymentID, "running", version)

	for i, op := range []string{"stop", "start", "stop"} {
		version = performCapabilityOperation(t, runnerBinary, httpServer.Client(), httpServer.URL, deviceToken, runnerToken, journalPath, deploymentID, "local-service", releaseV1, version, op, fmt.Sprintf("v1-op-%d", i))
	}

	manifestV2 := capabilityServiceManifest(image, "--directory", "/")
	releaseV2 := submitCapabilityRelease(t, httpServer.Client(), httpServer.URL, deviceToken, manifestV2, "release-v2")
	version = performCapabilityOperation(t, runnerBinary, httpServer.Client(), httpServer.URL, deviceToken, runnerToken, journalPath, deploymentID, "local-service", releaseV2, version, "update", "update")
	version = performCapabilityOperation(t, runnerBinary, httpServer.Client(), httpServer.URL, deviceToken, runnerToken, journalPath, deploymentID, "local-service", releaseV2, version, "stop", "final-stop")
	version = performCapabilityOperation(t, runnerBinary, httpServer.Client(), httpServer.URL, deviceToken, runnerToken, journalPath, deploymentID, "local-service", releaseV2, version, "uninstall", "uninstall")
	_ = version

	var observations []contracts.DeploymentObservation
	rows, err := store.DB().QueryContext(ctx, `SELECT body FROM capability_observations WHERE deployment_id=? ORDER BY observed_at`, deploymentID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		var observation contracts.DeploymentObservation
		if err = json.Unmarshal(raw, &observation); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		observations = append(observations, observation)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	wantStates := []string{"running", "stopped", "running", "stopped", "running", "stopped", "removed"}
	if len(observations) != len(wantStates) {
		t.Fatalf("persisted %d deployment receipts, want %d", len(observations), len(wantStates))
	}
	for i, want := range wantStates {
		if observations[i].State != want || observations[i].Fence != int64(i+1) || observations[i].AttemptID == "" {
			t.Fatalf("receipt %d has state/fence/attempt %q/%d/%t, want %q/%d/nonempty", i, observations[i].State, observations[i].Fence, observations[i].AttemptID != "", want, i+1)
		}
		if want == "stopped" || want == "removed" {
			if observations[i].TerminationProof == nil || !observations[i].TerminationProof.CleanupComplete {
				t.Fatalf("receipt %d omitted complete termination proof", i)
			}
		}
	}
	if _, err = (runner.ExecDockerCommand{}).Run(ctx, "inspect", containerName); !errors.Is(err, runner.ErrDockerObjectNotFound) {
		t.Fatalf("uninstall left a capability container behind (err=%v)", err)
	}
}

type capabilityHTTPFaults struct{ failNextDeploymentObservation atomic.Bool }

func startCapabilityHTTPFixture(t *testing.T, databasePath, bootstrapCode, runnerToken string, faults *capabilityHTTPFaults) (*storage.Store, *httptest.Server) {
	t.Helper()
	ctx := t.Context()
	store, err := storage.Open(ctx, storage.Config{Path: databasePath})
	if err != nil {
		t.Fatal(err)
	}
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	deviceScopes := []string{"client:read", "client:command", "capability:read", "capability:release:submit", "capability:deployment:request", "capability:deployment:authorize", "capability:deployment:revoke"}
	server, err := control.NewServer(store, control.Config{RunnerToken: runnerToken, RunnerID: "local-capability-runner", RunnerScopes: []string{"runner:claim", "runner:observe", "runner:control:read", "capability:deploy"}, InstanceScopes: deviceScopes})
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err = capabilities.Register(server, store, capabilities.Config{AvailableCapabilityIDs: []string{"local.service"}, RuntimeBackend: "docker"}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err = control.RegisterInstanceRoutes(server); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err = server.RegisterRunnerCredential(ctx, runnerToken, "local-capability-runner", []string{"runner:claim", "runner:observe", "runner:control:read", "capability:deploy"}); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/runner/v1/deployments/") && strings.HasSuffix(r.URL.Path, "/observations") && faults.failNextDeploymentObservation.CompareAndSwap(true, false) {
			http.Error(w, "injected local test receipt interruption", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	return store, httpServer
}

func capabilityTestImage(t *testing.T, ctx context.Context) string {
	t.Helper()
	if _, err := exec.CommandContext(ctx, "docker", "image", "inspect", "python:3.12-alpine").Output(); err != nil {
		if output, pullErr := exec.CommandContext(ctx, "docker", "pull", "python:3.12-alpine").CombinedOutput(); pullErr != nil {
			t.Fatalf("pull public test image failed: %s", strings.TrimSpace(string(output)))
		}
	}
	output, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{index .RepoDigests 0}}", "python:3.12-alpine").Output()
	if err != nil {
		t.Fatalf("read pinned test image digest: %v", err)
	}
	ref := strings.TrimSpace(string(output))
	if !strings.Contains(ref, "@sha256:") {
		t.Fatal("Docker test image does not have a repository digest")
	}
	return "oci://" + ref
}

func capabilityServiceManifest(image, directoryFlag, directory string) contracts.CapabilityManifest {
	artifactDigest := image[strings.LastIndex(image, "@")+1:]
	budget := contracts.CapabilityResourceBudget{CPUMillis: 250, MemoryBytes: 128 << 20, DiskBytes: 256 << 20, TimeoutSeconds: 90, Concurrency: 1}
	return contracts.CapabilityManifest{
		SchemaVersion: 1, ArtifactRef: image, ArtifactDigest: artifactDigest,
		Entrypoint: "python3", Args: []string{"-m", "http.server", "8080", "--bind", "0.0.0.0", directoryFlag, directory},
		Capabilities: []string{"local.service"}, Budget: budget,
		Network:     contracts.CapabilityNetworkPolicy{Mode: "none"},
		HealthCheck: &contracts.CapabilityHealthCheck{Kind: "exec", Target: "python3", Args: []string{"-c", "import socket; s=socket.create_connection(('127.0.0.1',8080),2); s.close()"}, IntervalSeconds: 5, TimeoutSeconds: 2, FailureThreshold: 3},
	}
}

func submitCapabilityRelease(t *testing.T, client *http.Client, base, device string, manifest contracts.CapabilityManifest, key string) string {
	t.Helper()
	var result contracts.CommandReceipt
	capabilityHTTP(t, client, http.MethodPost, base+"/v1/commands", device, key, contracts.CommandRequest{Kind: "capability.release.submit", Payload: jsonRaw(t, map[string]any{"manifest": manifest})}, http.StatusOK, &result)
	if result.ResourceID == "" {
		t.Fatal("release receipt omitted its resource id")
	}
	var page struct {
		Items []contracts.CapabilityRelease `json:"items"`
	}
	var releases []contracts.CapabilityRelease
	capabilityHTTP(t, client, http.MethodGet, base+"/v1/capabilities/releases", device, "", nil, http.StatusOK, &page)
	releases = page.Items
	for _, release := range releases {
		if release.ReleaseID == result.ResourceID {
			return release.Digest
		}
	}
	t.Fatal("submitted release was not visible through the control API")
	return ""
}

func requestCapabilityDeployment(t *testing.T, client *http.Client, base, device, service, digest, operation string, version *int64, key string) (string, int64) {
	t.Helper()
	payload := map[string]any{"service_id": service, "release_digest": digest, "operation": operation}
	var receipt contracts.CommandReceipt
	capabilityHTTP(t, client, http.MethodPost, base+"/v1/commands", device, key, contracts.CommandRequest{Kind: "capability.deployment.request", ExpectedVersion: version, Payload: jsonRaw(t, payload)}, http.StatusOK, &receipt)
	return receipt.ResourceID, receipt.ResourceVersion
}

func authorizeCapabilityDeployment(t *testing.T, client *http.Client, base, device, deployment string, version int64, operation string, budget contracts.CapabilityResourceBudget, key string) int64 {
	t.Helper()
	payload := map[string]any{"deployment_id": deployment, "scope_kind": "instance", "actions": []string{"install", "start", "stop", "update", "uninstall"}, "budget": budget, "expires_at": time.Now().UTC().Add(30 * time.Minute)}
	var receipt contracts.CommandReceipt
	capabilityHTTP(t, client, http.MethodPost, base+"/v1/commands", device, key, contracts.CommandRequest{Kind: "capability.deployment.authorize", ExpectedVersion: &version, Payload: jsonRaw(t, payload)}, http.StatusOK, &receipt)
	return receipt.ResourceVersion
}

func performCapabilityOperation(t *testing.T, runnerBinary string, client *http.Client, base, device, runnerToken, journalPath, deployment, service, releaseDigest string, version int64, operation, key string) int64 {
	t.Helper()
	current, nextVersion := requestCapabilityDeployment(t, client, base, device, service, releaseDigest, operation, &version, key+"-request")
	if current != deployment {
		t.Fatalf("deployment request changed identity from %q to %q", deployment, current)
	}
	grantBudget := capabilityServiceManifestBudget()
	version = authorizeCapabilityDeployment(t, client, base, device, deployment, nextVersion, operation, grantBudget, key+"-authorize")
	if err := runCapabilityRunnerOnce(t, runnerBinary, base, runnerToken, journalPath); err != nil {
		t.Fatalf("runner failed during %s: %v", operation, err)
	}
	wantState := map[string]string{"install": "running", "start": "running", "update": "running", "stop": "stopped", "uninstall": "removed"}[operation]
	return assertCapabilityDeploymentState(t, client, base, device, deployment, wantState, version)
}

func buildCapabilityRunner(t *testing.T, root string) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	moduleRoot := filepath.Clean(filepath.Join(workingDirectory, "../.."))
	binary := filepath.Join(root, "zoe-runner")
	goBinary := filepath.Join(goruntime.GOROOT(), "bin", "go")
	build := exec.Command(goBinary, "build", "-o", binary, "./cmd/zoe-runner")
	build.Dir = moduleRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build independent runner executable failed: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	return binary
}

func runCapabilityRunnerOnce(t *testing.T, binary, controlURL, runnerToken, journalPath string) error {
	t.Helper()
	routeConfig, err := json.Marshal(map[string]runner.Route{"worker": {ProfileID: "worker", RuntimeRouteID: "fixture-runtime", ModelRouteID: "fixture-model", IsolationPolicy: runner.OpenCodeWorkspaceWritePolicy, Harness: "opencode", Model: "fixture-model"}})
	if err != nil {
		return err
	}
	command := exec.CommandContext(t.Context(), binary, "--once")
	command.Env = append(os.Environ(),
		"ZOE_CONTROL_URL="+controlURL,
		"ZOE_RUNNER_TOKEN="+runnerToken,
		"ZOE_OPENCODE_IMAGE=fixture-opencode-image",
		"ZOE_OPENCODE_MODEL=fixture-model",
		"ZOE_OPENCODE_PROVIDER_SOCKET=/tmp/zoe-provider.sock",
		"ZOE_OPENCODE_WORKSPACE_ROOT="+filepath.Dir(journalPath),
		"ZOE_RUNNER_JOURNAL_PATH="+journalPath,
		"ZOE_RUNNER_ROUTES="+string(routeConfig),
		"ZOE_RUNNER_IDLE_POLL_INTERVAL=10ms",
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("runner process failed (%s): %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

func capabilityServiceManifestBudget() contracts.CapabilityResourceBudget {
	return contracts.CapabilityResourceBudget{CPUMillis: 250, MemoryBytes: 128 << 20, DiskBytes: 256 << 20, TimeoutSeconds: 90, Concurrency: 1}
}

func assertCapabilityDeploymentState(t *testing.T, client *http.Client, base, device, deployment, want string, priorVersion int64) int64 {
	t.Helper()
	var got struct {
		DeploymentID string `json:"deployment_id"`
		Observed     string `json:"observed_state"`
		LastError    string `json:"last_error_code"`
		Version      int64  `json:"version"`
	}
	capabilityHTTP(t, client, http.MethodGet, base+"/v1/capabilities/deployments/"+deployment, device, "", nil, http.StatusOK, &got)
	if got.DeploymentID != deployment || got.Observed != want || got.Version != priorVersion {
		t.Fatalf("deployment state=%q error=%q version=%d id=%q, want %q version=%d", got.Observed, got.LastError, got.Version, got.DeploymentID, want, priorVersion)
	}
	return got.Version
}

func capabilityHTTP(t *testing.T, client *http.Client, method, endpoint, bearer, key string, body any, status int, dst any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, endpoint, strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("control HTTP request failed: %v", err)
	}
	defer response.Body.Close()
	var raw json.RawMessage
	if err = json.NewDecoder(response.Body).Decode(&raw); err != nil && response.StatusCode != http.StatusNoContent {
		t.Fatalf("decode control HTTP response: %v", err)
	}
	if response.StatusCode != status {
		var envelope contracts.ErrorEnvelope
		_ = json.Unmarshal(raw, &envelope)
		t.Fatalf("control HTTP %s %s returned %d, want %d", method, response.Request.URL.Path, response.StatusCode, status)
	}
	if dst != nil {
		if err = json.Unmarshal(raw, dst); err != nil {
			t.Fatalf("decode control response payload: %v", err)
		}
	}
}

func jsonRaw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func capabilityJournalReceipt(t *testing.T, journalPath, deploymentID string) (string, bool) {
	t.Helper()
	ctx := t.Context()
	journal, err := runner.OpenJournal(ctx, journalPath)
	if err != nil {
		t.Fatalf("open runner journal: %v", err)
	}
	defer journal.Close()
	entry, found, err := journal.DeploymentEntry(ctx, deploymentID)
	if err != nil {
		t.Fatalf("read deployment journal entry: %v", err)
	}
	if !found || entry.Observation == nil {
		t.Fatalf("runner journal has no deployment receipt for %q", deploymentID)
	}
	return entry.Observation.State, entry.Acknowledged
}

func capabilityContainerID(t *testing.T, ctx context.Context, containerName string) string {
	t.Helper()
	output, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Id}}", containerName).Output()
	if err != nil {
		t.Fatalf("inspect capability container %q: %v", containerName, err)
	}
	containerID := strings.TrimSpace(string(output))
	if containerID == "" {
		t.Fatalf("Docker returned an empty ID for capability container %q", containerName)
	}
	return containerID
}

func capabilityShortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}
