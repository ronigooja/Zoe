package runner

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

type fakeDocker struct {
	claim            contracts.DeploymentClaim
	commands         [][]string
	imageDigest      string
	containerRunning bool
	containerPaused  bool
	containerID      string
	created          bool
	ambiguousRun     bool
	missingContainer bool
	containerLabels  map[string]string
	replaceOnStop    bool
}

func (d *fakeDocker) Run(_ context.Context, args ...string) ([]byte, error) {
	d.commands = append(d.commands, append([]string(nil), args...))
	if len(args) == 0 {
		return nil, errors.New("empty docker command")
	}
	switch strings.Join(args, " ") {
	case "pull " + strings.TrimPrefix(strings.TrimPrefix(d.claim.Manifest.ArtifactRef, "oci://"), "https://"):
		return []byte("pulled"), nil
	}
	if args[0] == "pull" {
		return []byte("pulled"), nil
	}
	if len(args) >= 3 && args[0] == "image" && args[1] == "inspect" {
		raw, _ := json.Marshal([]string{"registry.example/app@" + d.imageDigest})
		return raw, nil
	}
	if args[0] == "volume" && args[1] == "create" {
		return []byte("volume"), nil
	}
	if args[0] == "volume" && args[1] == "inspect" {
		raw, _ := json.Marshal(map[string]string{"io.zoe.instance_lineage": labelValue(d.claim.InstanceLineageID), "io.zoe.volume": labelValue(d.claim.Manifest.Mounts[0].VolumeID)})
		return raw, nil
	}
	if args[0] == "run" {
		d.containerRunning = true
		d.created = true
		d.containerLabels = map[string]string{}
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--label" {
				parts := strings.SplitN(args[i+1], "=", 2)
				if len(parts) == 2 {
					d.containerLabels[parts[0]] = parts[1]
				}
			}
		}
		if d.ambiguousRun {
			d.ambiguousRun = false
			return nil, errors.New("reply lost after Docker created container")
		}
		return []byte("container-id"), nil
	}
	if args[0] == "start" {
		d.containerRunning = true
		return []byte("started"), nil
	}
	if args[0] == "unpause" {
		d.containerRunning, d.containerPaused = true, false
		return []byte("unpaused"), nil
	}
	if args[0] == "pause" {
		d.containerRunning, d.containerPaused = true, true
		return []byte("paused"), nil
	}
	if args[0] == "stop" {
		if d.replaceOnStop {
			d.replaceOnStop = false
			d.containerID = "replacement-container"
			d.containerRunning = true
			d.containerLabels = map[string]string{"io.zoe.deployment": "different-deployment"}
			return []byte("stopped original"), nil
		}
		d.containerRunning, d.containerPaused = false, false
		return []byte("stopped"), nil
	}
	if args[0] == "rm" {
		d.missingContainer = true
		return []byte("removed"), nil
	}
	if args[0] == "inspect" {
		if d.missingContainer || !d.created {
			return nil, ErrDockerObjectNotFound
		}
		if strings.Contains(strings.Join(args, " "), ".Config.Labels") {
			raw, _ := json.Marshal(d.containerLabels)
			return raw, nil
		}
		if len(args) > 1 && strings.Contains(args[1], ".RepoDigests") {
			raw, _ := json.Marshal([]string{"registry.example/app@" + d.imageDigest})
			return raw, nil
		}
		state := map[string]any{"Id": d.containerID, "Config": map[string]any{"Labels": d.containerLabels}, "State": map[string]any{"Running": d.containerRunning, "Paused": d.containerPaused, "Health": map[string]any{"Status": "healthy"}}}
		raw, _ := json.Marshal(state)
		return raw, nil
	}
	return nil, nil
}

func validDeploymentClaim() contracts.DeploymentClaim {
	budget := contracts.CapabilityResourceBudget{CPUMillis: 500, MemoryBytes: 128 << 20, DiskBytes: 512 << 20, TimeoutSeconds: 30, Concurrency: 1}
	return contracts.DeploymentClaim{
		InstanceLineageID: "lineage-one", DeploymentID: "deployment-one", AttemptID: "attempt-one", Fence: 2, ServiceID: "service-one",
		ReleaseDigest: "sha256:" + strings.Repeat("a", 64), DesiredOp: "install", RuntimeBackend: "docker",
		Manifest: contracts.CapabilityManifest{SchemaVersion: 1, ArtifactRef: "oci://registry.example/app@sha256:" + strings.Repeat("b", 64), ArtifactDigest: "sha256:" + strings.Repeat("b", 64), Entrypoint: "/app", Args: []string{"--serve"}, Capabilities: []string{"service"}, HealthCheck: &contracts.CapabilityHealthCheck{Kind: "exec", Target: "app-health", IntervalSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 3}, Budget: budget, Network: contracts.CapabilityNetworkPolicy{Mode: "none"}},
		Grant:    contracts.CapabilityGrant{GrantID: "grant-one", ScopeKind: "instance", Actions: []string{"install", "start", "stop", "pause", "resume", "update", "uninstall"}, Budget: budget, ExpiresAt: time.Now().UTC().Add(time.Hour)},
	}
}

func deploymentTestAuthority(t *testing.T) *DeploymentAuthority {
	t.Helper()
	a, err := OpenDeploymentAuthority(context.Background(), filepath.Join(t.TempDir(), "deployment-authority.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestDeploymentExecutorRejectsUnsupportedOrUnauthorizedClaimBeforeDocker(t *testing.T) {
	for name, mutate := range map[string]func(*contracts.DeploymentClaim){
		"grant action mismatch": func(c *contracts.DeploymentClaim) { c.Grant.Actions = []string{"start"} },
		"expired grant":         func(c *contracts.DeploymentClaim) { c.Grant.ExpiresAt = time.Now().Add(-time.Second) },
		"digest mismatch":       func(c *contracts.DeploymentClaim) { c.Manifest.ArtifactDigest = "sha256:" + strings.Repeat("c", 64) },
		"unsupported network": func(c *contracts.DeploymentClaim) {
			c.Manifest.Network.Mode = "egress_allowlist"
			c.Manifest.Network.AllowedHosts = []string{"api.example"}
		},
		"secret resolver missing": func(c *contracts.DeploymentClaim) {
			c.Manifest.SecretRefs = map[string]string{"API_KEY": "secret://gateway"}
		},
		"unresolvable grant scope": func(c *contracts.DeploymentClaim) { c.Grant.ScopeKind, c.Grant.ScopeID = "task", "task-one" },
		"missing health check":     func(c *contracts.DeploymentClaim) { c.Manifest.HealthCheck = nil },
		"unbounded writable volume": func(c *contracts.DeploymentClaim) {
			c.Manifest.Mounts = []contracts.CapabilityMount{{VolumeID: "volume-one", Target: "/data"}}
			c.VolumeIDs = []string{"volume-one"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			claim := validDeploymentClaim()
			mutate(&claim)
			docker := &fakeDocker{claim: claim}
			obs := (&DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}).Execute(context.Background(), claim)
			if obs.State != "failed" || obs.ErrorCode == "" || len(docker.commands) != 0 {
				t.Fatalf("observation=%+v commands=%v", obs, docker.commands)
			}
		})
	}
}

func TestDeploymentExecutorPinsArtifactAndEnforcesDockerLimits(t *testing.T) {
	claim := validDeploymentClaim()
	docker := &fakeDocker{claim: claim, imageDigest: claim.Manifest.ArtifactDigest, containerID: "container-id"}
	obs := (&DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}).Execute(context.Background(), claim)
	if obs.State != "running" || obs.Health.State != "healthy" || obs.Health.EvidenceRef != "docker:container-id" {
		t.Fatalf("observation=%+v commands=%v", obs, docker.commands)
	}
	var runArgs []string
	for _, args := range docker.commands {
		if len(args) > 0 && args[0] == "run" {
			runArgs = args
		}
	}
	for _, want := range []string{"--network", "none", "--read-only", "--user", "65532:65532", "--cap-drop", "ALL", "--memory", "--cpus", "--storage-opt", "size=536870912", "--pids-limit", "256"} {
		if !containsString(runArgs, want) {
			t.Fatalf("docker run args missing %q: %v", want, runArgs)
		}
	}
	if !containsString(docker.commands[0], "registry.example/app@"+claim.Manifest.ArtifactDigest) {
		t.Fatalf("pull was not digest-pinned: %v", docker.commands[0])
	}
}

func TestDeploymentExecutorAppliesValidatedManifestHealthCheck(t *testing.T) {
	claim := validDeploymentClaim()
	claim.Manifest.HealthCheck = &contracts.CapabilityHealthCheck{Kind: "exec", Target: "python3", Args: []string{"-c", "import sys; sys.exit(0)"}, IntervalSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 3}
	docker := &fakeDocker{claim: claim, imageDigest: claim.Manifest.ArtifactDigest, containerID: "container-id"}
	obs := (&DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}).Execute(context.Background(), claim)
	if obs.State != "running" {
		t.Fatalf("observation=%+v", obs)
	}
	var found bool
	for _, command := range docker.commands {
		for _, arg := range command {
			if arg == "--health-cmd" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("manifest health check was omitted from docker run: %v", docker.commands)
	}
}

func TestDeploymentExecutorRequiresVerifiedTerminationForStop(t *testing.T) {
	claim := validDeploymentClaim()
	claim.DesiredOp = "stop"
	priorLabels := deploymentOwnershipLabels(claim)
	priorLabels["io.zoe.release"] = strings.TrimPrefix(claim.ReleaseDigest, "sha256:")
	priorLabels["io.zoe.fence"] = "1"
	priorLabels["io.zoe.attempt"] = labelValue("attempt-old")
	docker := &fakeDocker{claim: claim, containerRunning: true, containerID: "container-id", created: true, containerLabels: priorLabels}
	obs := (&DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}).Execute(context.Background(), claim)
	if obs.State != "stopped" || obs.TerminationProof == nil || !obs.TerminationProof.CleanupComplete {
		t.Fatalf("observation=%+v", obs)
	}
	if obs.TerminationProof.EffectRef != "docker:container-id" {
		t.Fatalf("termination effect=%+v", obs.TerminationProof)
	}
}

func TestDeploymentExecutorCleanupOnlyBypassesGrantOnlyForOwnedStop(t *testing.T) {
	claim := validDeploymentClaim()
	claim.DesiredOp, claim.CleanupOnly = "stop", true
	claim.Fence = 2
	claim.Grant.Actions = []string{"install"}
	claim.Grant.ExpiresAt = time.Now().Add(-time.Hour)
	claim.Manifest.SecretRefs = map[string]string{"KEY": "secret://deployment"}
	claim.Manifest.Mounts = []contracts.CapabilityMount{{VolumeID: "persistent-data", Target: "/data"}}
	claim.VolumeIDs = []string{"persistent-data"}
	claim.ConfigRef, claim.ConfigSHA256 = "deployment-config", strings.Repeat("d", 64)
	labels := deploymentOwnershipLabels(claim)
	labels["io.zoe.release"] = "sha256:" + strings.Repeat("c", 64)
	labels["io.zoe.fence"] = "1"
	labels["io.zoe.attempt"] = labelValue("old-install")
	docker := &fakeDocker{claim: claim, containerRunning: true, containerID: "container-owned", created: true, containerLabels: labels}
	obs := (&DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}).Execute(context.Background(), claim)
	if obs.State != "stopped" || obs.TerminationProof == nil || !obs.TerminationProof.CleanupComplete {
		t.Fatalf("cleanup observation=%+v commands=%v", obs, docker.commands)
	}
	for _, command := range docker.commands {
		if len(command) > 0 && (command[0] == "run" || command[0] == "pull" || command[0] == "rm" || command[0] == "volume") {
			t.Fatalf("cleanup performed an operation beyond stop: %v", command)
		}
	}
	if !docker.missingContainer && docker.containerRunning {
		t.Fatalf("cleanup did not stop the owned container: %v", docker.commands)
	}
}

func TestDeploymentExecutorCleanupOnlyRejectsUnownedOrNewerFence(t *testing.T) {
	for name, mutate := range map[string]func(map[string]string, contracts.DeploymentClaim){
		"unowned labels": func(labels map[string]string, claim contracts.DeploymentClaim) {
			labels["io.zoe.deployment"] = labelValue("other-deployment")
		},
		"newer fence": func(labels map[string]string, claim contracts.DeploymentClaim) { labels["io.zoe.fence"] = "3" },
	} {
		t.Run(name, func(t *testing.T) {
			claim := validDeploymentClaim()
			claim.DesiredOp, claim.CleanupOnly, claim.Fence = "stop", true, 2
			claim.Grant.Actions = []string{"install"}
			claim.Grant.ExpiresAt = time.Now().Add(-time.Hour)
			labels := deploymentOwnershipLabels(claim)
			labels["io.zoe.release"] = strings.TrimPrefix(claim.ReleaseDigest, "sha256:")
			labels["io.zoe.fence"] = "1"
			labels["io.zoe.attempt"] = labelValue("old-install")
			mutate(labels, claim)
			docker := &fakeDocker{claim: claim, containerRunning: true, containerID: "container-owned", created: true, containerLabels: labels}
			obs := (&DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}).Execute(context.Background(), claim)
			if obs.State != "uncertain" || len(docker.commands) != 1 || docker.commands[0][0] != "inspect" {
				t.Fatalf("cleanup observation=%+v commands=%v", obs, docker.commands)
			}
		})
	}
}

func TestDeploymentExecutorCleanupOnlyProvesMissingContainerWithoutCreatingResources(t *testing.T) {
	claim := validDeploymentClaim()
	claim.DesiredOp, claim.CleanupOnly = "stop", true
	claim.Fence = 2
	claim.Grant.Actions = []string{"install"}
	claim.Grant.ExpiresAt = time.Now().Add(-time.Hour)
	docker := &fakeDocker{claim: claim, missingContainer: true}
	obs := (&DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}).Execute(context.Background(), claim)
	if obs.State != "stopped" || obs.TerminationProof == nil || !obs.TerminationProof.CleanupComplete || len(docker.commands) != 1 || docker.commands[0][0] != "inspect" {
		t.Fatalf("missing-container cleanup observation=%+v commands=%v", obs, docker.commands)
	}
}

func TestDeploymentExecutorCleanupStopsInspectedIDAndRejectsNameSwap(t *testing.T) {
	claim := validDeploymentClaim()
	claim.DesiredOp, claim.CleanupOnly = "stop", true
	claim.Fence = 2
	claim.Grant.Actions = []string{"install"}
	claim.Grant.ExpiresAt = time.Now().Add(-time.Hour)
	labels := deploymentOwnershipLabels(claim)
	labels["io.zoe.release"] = strings.TrimPrefix(claim.ReleaseDigest, "sha256:")
	labels["io.zoe.fence"] = "1"
	labels["io.zoe.attempt"] = labelValue("old-install")
	docker := &fakeDocker{claim: claim, containerRunning: true, containerID: "verified-container", created: true, containerLabels: labels, replaceOnStop: true}
	obs := (&DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}).Execute(context.Background(), claim)
	if obs.State != "uncertain" || len(docker.commands) != 3 || docker.commands[1][0] != "stop" || docker.commands[1][len(docker.commands[1])-1] != "verified-container" {
		t.Fatalf("cleanup did not fence stop to inspected container ID: observation=%+v commands=%v", obs, docker.commands)
	}
}

func TestDeploymentExecutorRejectsNewWorkAfterGrantExpiry(t *testing.T) {
	claim := validDeploymentClaim()
	claim.DesiredOp = "install"
	claim.ClaimState = "uncertain"
	claim.Grant.ExpiresAt = time.Now().Add(-time.Hour)
	docker := &fakeDocker{claim: claim, missingContainer: true}
	obs := (&DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}).Execute(context.Background(), claim)
	if obs.State != "uncertain" || len(docker.commands) != 1 || docker.commands[0][0] != "inspect" {
		t.Fatalf("expired grant dispatched new work: observation=%+v commands=%v", obs, docker.commands)
	}
}

func TestDeploymentExecutorDoesNotDispatchPendingWorkAfterRevocation(t *testing.T) {
	claim := validDeploymentClaim()
	claim.DesiredControl = "stopped"
	docker := &fakeDocker{claim: claim, missingContainer: true}
	obs := (&DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}).Execute(context.Background(), claim)
	if obs.State != "uncertain" || len(docker.commands) != 1 || docker.commands[0][0] != "inspect" {
		t.Fatalf("revoked claim dispatched work instead of read-only reconciliation: observation=%+v commands=%v", obs, docker.commands)
	}
}

func TestDeploymentExecutorRejectsStaleFenceAgainstNewerContainer(t *testing.T) {
	claim := validDeploymentClaim()
	claim.DesiredOp = "stop"
	labels := deploymentLabels(claim)
	labels["io.zoe.fence"] = "10"
	labels["io.zoe.attempt"] = labelValue("newer-attempt")
	docker := &fakeDocker{claim: claim, containerRunning: true, containerID: "container-id", created: true, containerLabels: labels}
	obs := (&DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}).Execute(context.Background(), claim)
	if obs.State != "failed" || len(docker.commands) != 1 || docker.commands[0][0] != "inspect" {
		t.Fatalf("observation=%+v commands=%v", obs, docker.commands)
	}
}

func TestDeploymentExecutorFenceHighWaterSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "deployment-authority.db")
	firstJournal, err := OpenJournal(ctx, filepath.Join(root, "runner-one.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer firstJournal.Close()
	first, err := OpenDeploymentAuthority(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	claim := validDeploymentClaim()
	claim.DesiredOp = "stop"
	claim.Fence = 10
	labels := deploymentOwnershipLabels(claim)
	labels["io.zoe.release"] = strings.TrimPrefix(claim.ReleaseDigest, "sha256:")
	labels["io.zoe.fence"] = "1"
	labels["io.zoe.attempt"] = labelValue("attempt-old")
	docker := &fakeDocker{claim: claim, containerRunning: true, containerID: "container-id", created: true, containerLabels: labels}
	obs := (&DeploymentExecutor{Docker: docker, Fences: first}).Execute(ctx, claim)
	if obs.State != "stopped" {
		t.Fatalf("newer stop failed: %+v", obs)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	secondJournal, err := OpenJournal(ctx, filepath.Join(root, "runner-two.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer secondJournal.Close()

	second, err := OpenDeploymentAuthority(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	old := claim
	old.DesiredOp = "start"
	old.Fence = 1
	old.AttemptID = "attempt-old"
	docker.claim = old
	docker.commands = nil
	obs = (&DeploymentExecutor{Docker: docker, Fences: second}).Execute(ctx, old)
	if obs.State != "failed" || obs.ErrorCode != "deployment_fence_rejected" {
		t.Fatalf("old start was not rejected after journal restart: %+v", obs)
	}
	if len(docker.commands) != 0 {
		t.Fatalf("stale claim reached Docker: %v", docker.commands)
	}
}

func TestDeploymentExecutorDoesNotRepeatUnobservedInstallAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "host-authority.db")
	first, err := OpenDeploymentAuthority(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	claim := validDeploymentClaim()
	docker := &fakeDocker{claim: claim, imageDigest: claim.Manifest.ArtifactDigest, containerID: "container-id"}
	obs := (&DeploymentExecutor{Docker: docker, Fences: first}).Execute(ctx, claim)
	if obs.State != "running" {
		t.Fatalf("initial install=%+v", obs)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenDeploymentAuthority(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	docker.commands = nil
	obs = (&DeploymentExecutor{Docker: docker, Fences: second}).Execute(ctx, claim)
	if obs.State != "running" {
		t.Fatalf("read-only replay reconciliation=%+v", obs)
	}
	for _, command := range docker.commands {
		if command[0] == "run" || command[0] == "rm" || command[0] == "pull" {
			t.Fatalf("replay repeated a write effect: %v", docker.commands)
		}
	}
}

type blockStartDocker struct {
	base    *fakeDocker
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *blockStartDocker) Run(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "start" {
		d.once.Do(func() { close(d.started); <-d.release })
	}
	return d.base.Run(ctx, args...)
}

func TestDeploymentExecutorSerializesFenceReservationThroughDockerEffect(t *testing.T) {
	ctx := context.Background()
	claim := validDeploymentClaim()
	claim.DesiredOp = "start"
	claim.Fence = 1
	labels := deploymentLabels(claim)
	base := &fakeDocker{claim: claim, containerID: "container-id", created: true, containerLabels: labels}
	docker := &blockStartDocker{base: base, started: make(chan struct{}), release: make(chan struct{})}
	executor := &DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}
	startDone := make(chan contracts.DeploymentObservation, 1)
	go func() { startDone <- executor.Execute(ctx, claim) }()
	<-docker.started
	newer := claim
	newer.DesiredOp = "stop"
	newer.Fence = 10
	stopDone := make(chan contracts.DeploymentObservation, 1)
	go func() { stopDone <- executor.Execute(ctx, newer) }()
	close(docker.release)
	startObs, stopObs := <-startDone, <-stopDone
	if startObs.State != "running" || stopObs.State != "stopped" || base.containerRunning {
		t.Fatalf("serialized operations did not leave the newest desired state: start=%+v stop=%+v running=%v commands=%v", startObs, stopObs, base.containerRunning, base.commands)
	}
}

func TestDeploymentExecutorRejectsForeignContainerWithoutSideEffect(t *testing.T) {
	claim := validDeploymentClaim()
	claim.DesiredOp = "stop"
	labels := deploymentOwnershipLabels(claim)
	labels["io.zoe.instance_lineage"] = labelValue("another-lineage")
	docker := &fakeDocker{claim: claim, containerRunning: true, containerID: "foreign-container", created: true, containerLabels: labels}
	obs := (&DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}).Execute(context.Background(), claim)
	if obs.State != "failed" {
		t.Fatalf("observation=%+v", obs)
	}
	if len(docker.commands) != 1 || docker.commands[0][0] != "inspect" {
		t.Fatalf("foreign container received side effect: %v", docker.commands)
	}
}

func TestDeploymentExecutorVerifiesMountVolumeOwnership(t *testing.T) {
	claim := validDeploymentClaim()
	claim.Manifest.Mounts = []contracts.CapabilityMount{{VolumeID: "vol-one", Target: "/data", ReadOnly: true}}
	claim.VolumeIDs = []string{"vol-one"}
	docker := &fakeDocker{claim: claim, imageDigest: claim.Manifest.ArtifactDigest, containerID: "container-id"}
	obs := (&DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}).Execute(context.Background(), claim)
	if obs.State != "running" {
		t.Fatalf("observation=%+v", obs)
	}
	found := false
	for _, args := range docker.commands {
		if containsString(args, "type=volume,src="+deploymentVolumeName(claim.InstanceLineageID, "vol-one")+",dst=/data,readonly") {
			found = true
		}
	}
	if !found {
		t.Fatalf("authorized readonly named volume was not attached: %v", docker.commands)
	}
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func TestDeploymentExecutorAllowsFencedStopAfterGrantExpiry(t *testing.T) {
	claim := validDeploymentClaim()
	claim.DesiredOp = "stop"
	claim.DesiredControl = "stopped"
	claim.ClaimState = "claimed"
	claim.Grant.ExpiresAt = time.Now().Add(-time.Hour)
	priorLabels := deploymentOwnershipLabels(claim)
	priorLabels["io.zoe.release"] = strings.TrimPrefix(claim.ReleaseDigest, "sha256:")
	priorLabels["io.zoe.fence"] = "1"
	priorLabels["io.zoe.attempt"] = labelValue("attempt-old")
	docker := &fakeDocker{claim: claim, containerRunning: true, containerID: "container-id", created: true, containerLabels: priorLabels}
	obs := (&DeploymentExecutor{Docker: docker, Fences: deploymentTestAuthority(t)}).Execute(context.Background(), claim)
	if obs.State != "stopped" || obs.TerminationProof == nil || !obs.TerminationProof.CleanupComplete {
		t.Fatalf("authorized expired-grant stop did not prove termination: observation=%+v commands=%v", obs, docker.commands)
	}
	foundStop := false
	for _, command := range docker.commands {
		if len(command) > 0 && command[0] == "stop" {
			foundStop = true
		}
	}
	if !foundStop {
		t.Fatalf("expired-grant stop was not dispatched to the owned container: %v", docker.commands)
	}
}
