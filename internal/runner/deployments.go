package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

// DockerCommand is the narrow privileged boundary used by the capability
// executor. Production uses the Docker CLI; tests record its fixed argument
// vectors without requiring a daemon.
type DockerCommand interface {
	Run(context.Context, ...string) ([]byte, error)
}

var ErrDockerObjectNotFound = errors.New("docker object not found")

type uncertainDeploymentEffect struct{ err error }

func (e uncertainDeploymentEffect) Error() string { return "deployment effect could not be reconciled" }
func (e uncertainDeploymentEffect) Unwrap() error { return e.err }

type ExecDockerCommand struct{ Binary string }

func (d ExecDockerCommand) Run(ctx context.Context, args ...string) ([]byte, error) {
	bin := strings.TrimSpace(d.Binary)
	if bin == "" {
		bin = "docker"
	}
	command := exec.CommandContext(ctx, bin, args...)
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		message := strings.ToLower(stderr.String())
		if strings.Contains(message, "no such object") || strings.Contains(message, "no such container") {
			return nil, ErrDockerObjectNotFound
		}
		return nil, errors.New("docker command failed")
	}
	return output, nil
}

type DeploymentExecutor struct {
	Docker DockerCommand
	Fences interface {
		LockDeployment(context.Context, string) (func(), error)
		BeginDeploymentEffect(context.Context, string, int64, string) (bool, error)
	}
	Now func() time.Time
}

// Execute applies one immutable, fenced deployment claim. Unsupported
// policies are rejected before any Docker side effect.
func (e *DeploymentExecutor) Execute(ctx context.Context, claim contracts.DeploymentClaim) contracts.DeploymentObservation {
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	obs := contracts.DeploymentObservation{
		DeploymentID: claim.DeploymentID, AttemptID: claim.AttemptID, Fence: claim.Fence,
		ObservationID: deploymentObservationID(claim), Operation: claim.DesiredOp,
		ObservedAt: now().UTC(),
	}
	if err := validateDeploymentClaim(claim, now()); err != nil {
		obs.State, obs.ErrorCode = cleanupFailure(claim, "failed", deploymentErrorCode(err))
		return obs
	}
	if e.Docker == nil {
		obs.State, obs.ErrorCode = cleanupFailure(claim, "failed", "docker_unavailable")
		return obs
	}
	if e.Fences == nil {
		obs.State, obs.ErrorCode = cleanupFailure(claim, "failed", "deployment_fence_authority_unavailable")
		return obs
	}
	release, err := e.Fences.LockDeployment(ctx, claim.DeploymentID)
	if err != nil {
		obs.State, obs.ErrorCode = cleanupFailure(claim, "failed", "deployment_fence_authority_unavailable")
		return obs
	}
	defer release()
	fingerprint, err := deploymentClaimFingerprint(claim)
	if err != nil {
		obs.State, obs.ErrorCode = cleanupFailure(claim, "failed", "deployment_claim_invalid")
		return obs
	}
	deadline := claim.Manifest.Budget.TimeoutSeconds
	if claim.Grant.Budget.TimeoutSeconds < deadline {
		deadline = claim.Grant.Budget.TimeoutSeconds
	}
	if claim.CleanupOnly {
		deadline = 30
	}
	opCtx, cancel := context.WithTimeout(ctx, time.Duration(deadline)*time.Second)
	defer cancel()
	grantExpired := !claim.Grant.ExpiresAt.After(now())
	if !claim.CleanupOnly && (grantExpired && claim.DesiredOp != "stop" || claim.DesiredControl == "stopped" && claim.DesiredOp != "stop" && claim.DesiredOp != "uninstall") {
		return e.reconcileOnly(opCtx, claim, obs)
	}
	fresh, err := e.Fences.BeginDeploymentEffect(ctx, claim.DeploymentID, claim.Fence, fingerprint)
	if err != nil {
		obs.State, obs.ErrorCode = cleanupFailure(claim, "failed", "deployment_fence_rejected")
		return obs
	}
	if !fresh {
		if claim.CleanupOnly {
			return e.reconcileCleanupOnly(opCtx, claim, obs)
		}
		return e.reconcileOnly(opCtx, claim, obs)
	}
	container := deploymentContainerName(claim.InstanceLineageID, claim.DeploymentID)
	labels := deploymentLabels(claim)
	if claim.CleanupOnly {
		return e.stopCleanupOnly(opCtx, claim, container, obs, now)
	}

	switch claim.DesiredOp {
	case "install", "update":
		if err := e.install(opCtx, claim, container, labels); err != nil {
			obs.State, obs.ErrorCode = "failed", deploymentErrorCode(err)
			var uncertain uncertainDeploymentEffect
			if errors.As(err, &uncertain) {
				obs.State, obs.ErrorCode = "uncertain", "docker_outcome_unverified"
			}
			return obs
		}
		obs.ExternalEffectRef = []string{"docker:" + container}
		state, health, effect, err := e.inspectRunning(opCtx, container, claim)
		if err != nil {
			obs.State, obs.ErrorCode = "uncertain", "docker_outcome_unverified"
			return obs
		}
		obs.State, obs.Health = state, health
		obs.ExternalEffectRef = []string{effect}
	case "start", "resume":
		if err := e.requireOwned(opCtx, container, claim); err != nil {
			obs.State, obs.ErrorCode = "failed", deploymentErrorCode(err)
			return obs
		}
		verb := "start"
		if claim.DesiredOp == "resume" {
			verb = "unpause"
		}
		if _, err := e.Docker.Run(opCtx, verb, container); err != nil {
			obs.State, obs.ErrorCode = "uncertain", "docker_start_unverified"
			return obs
		}
		state, health, effect, err := e.inspectRunning(opCtx, container, claim)
		if err != nil {
			obs.State, obs.ErrorCode = "uncertain", "docker_outcome_unverified"
			return obs
		}
		obs.State, obs.Health = state, health
		obs.ExternalEffectRef = []string{effect}
	case "stop", "pause":
		if err := e.requireOwned(opCtx, container, claim); err != nil {
			obs.State, obs.ErrorCode = "failed", deploymentErrorCode(err)
			return obs
		}
		verb := "stop"
		if claim.DesiredOp == "pause" {
			verb = "pause"
		}
		var err error
		if verb == "stop" {
			_, err = e.Docker.Run(opCtx, verb, "--time", "10", container)
		} else {
			_, err = e.Docker.Run(opCtx, verb, container)
		}
		if err != nil {
			obs.State, obs.ErrorCode = "uncertain", "docker_termination_unverified"
			return obs
		}
		state, effect, err := e.inspectStopped(opCtx, container, claim)
		if claim.DesiredOp == "pause" {
			state, effect, err = e.inspectPaused(opCtx, container, claim)
		}
		if err != nil {
			obs.State, obs.ErrorCode = "uncertain", "docker_termination_unverified"
			return obs
		}
		obs.State, obs.ExternalEffectRef = state, []string{effect}
		if claim.DesiredOp == "pause" {
			obs.TerminationProof = nil
		} else {
			obs.TerminationProof = &contracts.CapabilityTerminationProof{EffectRef: effect, TerminatedAt: now().UTC(), CleanupComplete: true}
		}
	case "uninstall":
		if err := e.requireOwned(opCtx, container, claim); err != nil {
			obs.State, obs.ErrorCode = "failed", deploymentErrorCode(err)
			return obs
		}
		if _, _, err := e.inspectStopped(opCtx, container, claim); err != nil {
			obs.State, obs.ErrorCode = "failed", "deployment_requires_stop_before_uninstall"
			return obs
		}
		if _, err := e.Docker.Run(opCtx, "rm", container); err != nil {
			obs.State, obs.ErrorCode = "uncertain", "docker_removal_unverified"
			return obs
		}
		if _, err := e.Docker.Run(opCtx, "inspect", container); !errors.Is(err, ErrDockerObjectNotFound) {
			obs.State, obs.ErrorCode = "uncertain", "docker_removal_unverified"
			return obs
		}
		effect := "docker:" + container
		obs.State, obs.ExternalEffectRef = "removed", []string{effect}
		obs.TerminationProof = &contracts.CapabilityTerminationProof{EffectRef: effect, TerminatedAt: now().UTC(), CleanupComplete: true}
	default:
		obs.State, obs.ErrorCode = "failed", "unsupported_operation"
	}
	return obs
}

func cleanupFailure(claim contracts.DeploymentClaim, state, code string) (string, string) {
	if claim.CleanupOnly {
		return "uncertain", code
	}
	return state, code
}

func deploymentClaimFingerprint(claim contracts.DeploymentClaim) (string, error) {
	claim.ClaimState = ""
	claim.DesiredControl = ""
	body, err := json.Marshal(claim)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(body)), nil
}

// reconcileOnly is used when the host authority shows this exact operation
// was already started. It may inspect owned state, but never repeats a Docker
// write such as start, stop, rm, pull, or run.
func (e *DeploymentExecutor) reconcileOnly(ctx context.Context, claim contracts.DeploymentClaim, obs contracts.DeploymentObservation) contracts.DeploymentObservation {
	name := deploymentContainerName(claim.InstanceLineageID, claim.DeploymentID)
	body, err := e.Docker.Run(ctx, "inspect", "--format", "{{json .}}", name)
	if errors.Is(err, ErrDockerObjectNotFound) {
		if claim.DesiredOp == "uninstall" {
			effect := "docker:" + name
			obs.State, obs.ExternalEffectRef = "removed", []string{effect}
			obs.TerminationProof = &contracts.CapabilityTerminationProof{EffectRef: effect, TerminatedAt: obs.ObservedAt, CleanupComplete: true}
			return obs
		}
		obs.State, obs.ErrorCode = "uncertain", "docker_outcome_unverified"
		return obs
	}
	if err != nil {
		obs.State, obs.ErrorCode = "uncertain", "docker_outcome_unverified"
		return obs
	}
	var state struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Running bool `json:"Running"`
			Paused  bool `json:"Paused"`
			Health  *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
	}
	if json.Unmarshal(body, &state) != nil || state.ID == "" {
		obs.State, obs.ErrorCode = "uncertain", "docker_outcome_unverified"
		return obs
	}
	for key, value := range deploymentOwnershipLabels(claim) {
		if state.Config.Labels[key] != value {
			obs.State, obs.ErrorCode = "uncertain", "docker_ownership_unverified"
			return obs
		}
	}
	if claim.Fence < parseFence(state.Config.Labels["io.zoe.fence"]) || (claim.Fence == parseFence(state.Config.Labels["io.zoe.fence"]) && state.Config.Labels["io.zoe.attempt"] != labelValue(claim.AttemptID)) {
		obs.State, obs.ErrorCode = "uncertain", "docker_ownership_unverified"
		return obs
	}
	if state.Config.Labels["io.zoe.release"] != strings.TrimPrefix(claim.ReleaseDigest, "sha256:") {
		obs.State, obs.ErrorCode = "uncertain", "docker_release_unverified"
		return obs
	}
	effect := "docker:" + state.ID
	switch claim.DesiredOp {
	case "install", "update", "start", "resume":
		_, health, runningEffect, err := e.inspectRunning(ctx, name, claim)
		if err == nil {
			obs.State, obs.Health = "running", health
			obs.ExternalEffectRef = []string{runningEffect}
			return obs
		}
	case "stop":
		if !state.State.Running {
			obs.State = "stopped"
			obs.ExternalEffectRef = []string{effect}
			obs.TerminationProof = &contracts.CapabilityTerminationProof{EffectRef: effect, TerminatedAt: obs.ObservedAt, CleanupComplete: true}
			return obs
		}
	case "pause":
		if state.State.Paused {
			obs.State = "paused"
			obs.ExternalEffectRef = []string{effect}
			return obs
		}
	case "uninstall":
		// A present container means removal was not proven; never force-remove it
		// during replay because the prior outcome may be partially complete.
	}
	obs.State, obs.ErrorCode = "uncertain", "docker_outcome_unverified"
	return obs
}

func (e *DeploymentExecutor) stopCleanupOnly(ctx context.Context, claim contracts.DeploymentClaim, name string, obs contracts.DeploymentObservation, now func() time.Time) contracts.DeploymentObservation {
	body, err := e.Docker.Run(ctx, "inspect", "--format", "{{json .}}", name)
	if errors.Is(err, ErrDockerObjectNotFound) {
		effect := "docker:" + name
		obs.State, obs.ExternalEffectRef = "stopped", []string{effect}
		obs.TerminationProof = &contracts.CapabilityTerminationProof{EffectRef: effect, TerminatedAt: now().UTC(), CleanupComplete: true}
		return obs
	}
	if err != nil {
		obs.State, obs.ErrorCode = "uncertain", "docker_ownership_unverified"
		return obs
	}
	var state struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	if json.Unmarshal(body, &state) != nil || state.ID == "" || !cleanupOwned(state.Config.Labels, claim) || !cleanupFenceAllows(state.Config.Labels, claim) {
		obs.State, obs.ErrorCode = "uncertain", "docker_ownership_unverified"
		return obs
	}
	if state.State.Running {
		if _, err = e.Docker.Run(ctx, "stop", "--time", "10", state.ID); err != nil {
			obs.State, obs.ErrorCode = "uncertain", "docker_termination_unverified"
			return obs
		}
	}
	return e.reconcileCleanupOnlyExpected(ctx, claim, obs, state.ID)
}

func (e *DeploymentExecutor) reconcileCleanupOnly(ctx context.Context, claim contracts.DeploymentClaim, obs contracts.DeploymentObservation) contracts.DeploymentObservation {
	return e.reconcileCleanupOnlyExpected(ctx, claim, obs, "")
}

func (e *DeploymentExecutor) reconcileCleanupOnlyExpected(ctx context.Context, claim contracts.DeploymentClaim, obs contracts.DeploymentObservation, expectedID string) contracts.DeploymentObservation {
	name := deploymentContainerName(claim.InstanceLineageID, claim.DeploymentID)
	body, err := e.Docker.Run(ctx, "inspect", "--format", "{{json .}}", name)
	if errors.Is(err, ErrDockerObjectNotFound) {
		if expectedID != "" {
			obs.State, obs.ErrorCode = "uncertain", "docker_ownership_unverified"
			return obs
		}
		effect := "docker:" + name
		obs.State, obs.ExternalEffectRef = "stopped", []string{effect}
		obs.TerminationProof = &contracts.CapabilityTerminationProof{EffectRef: effect, TerminatedAt: obs.ObservedAt, CleanupComplete: true}
		return obs
	}
	if err != nil {
		obs.State, obs.ErrorCode = "uncertain", "docker_termination_unverified"
		return obs
	}
	var state struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	if json.Unmarshal(body, &state) != nil || state.ID == "" || expectedID != "" && state.ID != expectedID || !cleanupOwned(state.Config.Labels, claim) || !cleanupFenceAllows(state.Config.Labels, claim) {
		obs.State, obs.ErrorCode = "uncertain", "docker_ownership_unverified"
		return obs
	}
	if state.State.Running {
		obs.State, obs.ErrorCode = "uncertain", "docker_termination_unverified"
		return obs
	}
	effect := "docker:" + state.ID
	obs.State, obs.ExternalEffectRef = "stopped", []string{effect}
	obs.TerminationProof = &contracts.CapabilityTerminationProof{EffectRef: effect, TerminatedAt: obs.ObservedAt, CleanupComplete: true}
	return obs
}

func cleanupOwned(labels map[string]string, claim contracts.DeploymentClaim) bool {
	for key, value := range deploymentOwnershipLabels(claim) {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func cleanupFenceAllows(labels map[string]string, claim contracts.DeploymentClaim) bool {
	priorFence, err := strconv.ParseInt(labels["io.zoe.fence"], 10, 64)
	if err != nil || priorFence < 1 || claim.Fence < priorFence {
		return false
	}
	if claim.Fence == priorFence && labels["io.zoe.attempt"] != labelValue(claim.AttemptID) {
		return false
	}
	return true
}

func parseFence(value string) int64 { parsed, _ := strconv.ParseInt(value, 10, 64); return parsed }

func (e *DeploymentExecutor) install(ctx context.Context, claim contracts.DeploymentClaim, name string, labels map[string]string) error {
	image, err := deploymentImage(claim.Manifest.ArtifactRef, claim.Manifest.ArtifactDigest)
	if err != nil {
		return err
	}
	if _, err := e.Docker.Run(ctx, "pull", image); err != nil {
		return fmt.Errorf("pull pinned artifact: %w", err)
	}
	var digests []string
	inspection, err := e.Docker.Run(ctx, "image", "inspect", "--format", "{{json .RepoDigests}}", image)
	if err != nil || json.Unmarshal(inspection, &digests) != nil || !containsDigest(digests, claim.Manifest.ArtifactDigest) {
		return errors.New("pulled artifact digest did not match authorized release")
	}
	if _, err := e.Docker.Run(ctx, "inspect", name); err == nil {
		if claim.DesiredOp != "update" {
			return errors.New("install cannot replace an existing deployment container")
		}
		if err := e.requireOwned(ctx, name, claim); err != nil {
			return err
		}
		if _, _, err := e.inspectStopped(ctx, name, claim); err != nil {
			return errors.New("update requires a trusted stopped-state proof for the existing deployment")
		}
		if _, err := e.Docker.Run(ctx, "rm", name); err != nil {
			return uncertainDeploymentEffect{err: err}
		}
	} else if !errors.Is(err, ErrDockerObjectNotFound) {
		return errors.New("could not establish whether an existing deployment container is present")
	}
	for _, mount := range claim.Manifest.Mounts {
		volume := deploymentVolumeName(claim.InstanceLineageID, mount.VolumeID)
		if _, err := e.Docker.Run(ctx, "volume", "create", "--label", "io.zoe.instance_lineage="+labelValue(claim.InstanceLineageID), "--label", "io.zoe.volume="+labelValue(mount.VolumeID), volume); err != nil {
			return fmt.Errorf("create managed volume: %w", err)
		}
		var got map[string]string
		body, err := e.Docker.Run(ctx, "volume", "inspect", "--format", "{{json .Labels}}", volume)
		if err != nil || json.Unmarshal(body, &got) != nil || got["io.zoe.instance_lineage"] != labelValue(claim.InstanceLineageID) || got["io.zoe.volume"] != labelValue(mount.VolumeID) {
			return errors.New("volume ownership labels do not match authorized mount")
		}
	}
	args := []string{"run", "--detach", "--name", name, "--restart", "unless-stopped", "--network", "none", "--read-only", "--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "256", "--memory", strconv.FormatInt(claim.Grant.Budget.MemoryBytes, 10), "--cpus", fmt.Sprintf("%.3f", float64(claim.Grant.Budget.CPUMillis)/1000), "--storage-opt", "size=" + strconv.FormatInt(claim.Grant.Budget.DiskBytes, 10)}
	if check := claim.Manifest.HealthCheck; check != nil {
		args = append(args, dockerHealthCheckArgs(*check)...)
	}
	for key, value := range labels {
		args = append(args, "--label", key+"="+value)
	}
	for _, mount := range claim.Manifest.Mounts {
		volume := deploymentVolumeName(claim.InstanceLineageID, mount.VolumeID)
		arg := "type=volume,src=" + volume + ",dst=" + mount.Target
		if mount.ReadOnly {
			arg += ",readonly"
		}
		args = append(args, "--mount", arg)
	}
	args = append(args, "--entrypoint", claim.Manifest.Entrypoint, image)
	args = append(args, claim.Manifest.Args...)
	if _, err := e.Docker.Run(ctx, args...); err != nil {
		return uncertainDeploymentEffect{err: err}
	}
	return nil
}

func (e *DeploymentExecutor) requireOwned(ctx context.Context, name string, claim contracts.DeploymentClaim) error {
	var got map[string]string
	body, err := e.Docker.Run(ctx, "inspect", "--format", "{{json .Config.Labels}}", name)
	if err != nil || json.Unmarshal(body, &got) != nil {
		return errors.New("deployment container is missing or its ownership cannot be verified")
	}
	for key, value := range deploymentOwnershipLabels(claim) {
		if got[key] != value {
			return errors.New("deployment container ownership labels do not match")
		}
	}
	priorFence, err := strconv.ParseInt(got["io.zoe.fence"], 10, 64)
	if err != nil || priorFence < 1 || claim.Fence < priorFence {
		return errors.New("deployment claim is stale relative to the container fence")
	}
	if claim.DesiredOp != "update" && got["io.zoe.release"] != strings.TrimPrefix(claim.ReleaseDigest, "sha256:") {
		return errors.New("deployment container release does not match the requested release")
	}
	if claim.Fence == priorFence && (got["io.zoe.attempt"] != labelValue(claim.AttemptID) || got["io.zoe.release"] != strings.TrimPrefix(claim.ReleaseDigest, "sha256:")) {
		return errors.New("deployment attempt does not own the current container fence")
	}
	return nil
}

func (e *DeploymentExecutor) inspectRunning(ctx context.Context, name string, claim contracts.DeploymentClaim) (string, contracts.CapabilityHealth, string, error) {
	var state struct {
		ID    string `json:"Id"`
		State struct {
			Running bool   `json:"Running"`
			Status  string `json:"Status"`
			Health  *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
	}
	interval := time.NewTicker(time.Second)
	defer interval.Stop()
	for {
		if err := e.requireOwned(ctx, name, claim); err != nil {
			return "", contracts.CapabilityHealth{}, "", err
		}
		body, err := e.Docker.Run(ctx, "inspect", "--format", "{{json .}}", name)
		if err == nil && json.Unmarshal(body, &state) == nil && state.ID != "" && state.State.Running && state.State.Health != nil && state.State.Health.Status == "healthy" {
			at := time.Now().UTC()
			effect := "docker:" + state.ID
			return "running", contracts.CapabilityHealth{State: "healthy", CheckedAt: at, EvidenceRef: effect}, effect, nil
		}
		if err == nil && state.ID != "" && !state.State.Running && (state.State.Status == "exited" || state.State.Status == "dead") {
			return "", contracts.CapabilityHealth{}, "", errors.New("container exited before its Docker healthcheck became healthy")
		}
		select {
		case <-ctx.Done():
			return "", contracts.CapabilityHealth{}, "", errors.New("container did not become healthy before the deployment operation timed out")
		case <-interval.C:
		}
	}
}

func (e *DeploymentExecutor) inspectStopped(ctx context.Context, name string, claim contracts.DeploymentClaim) (string, string, error) {
	if err := e.requireOwned(ctx, name, claim); err != nil {
		return "", "", err
	}
	var state struct {
		ID    string `json:"Id"`
		State struct {
			Running bool `json:"Running"`
			Paused  bool `json:"Paused"`
		} `json:"State"`
	}
	body, err := e.Docker.Run(ctx, "inspect", "--format", "{{json .}}", name)
	if err != nil || json.Unmarshal(body, &state) != nil || state.ID == "" || state.State.Running {
		return "", "", errors.New("container process tree termination was not proven")
	}
	return "stopped", "docker:" + state.ID, nil
}

func (e *DeploymentExecutor) inspectPaused(ctx context.Context, name string, claim contracts.DeploymentClaim) (string, string, error) {
	if err := e.requireOwned(ctx, name, claim); err != nil {
		return "", "", err
	}
	var state struct {
		ID    string `json:"Id"`
		State struct {
			Running bool `json:"Running"`
			Paused  bool `json:"Paused"`
		} `json:"State"`
	}
	body, err := e.Docker.Run(ctx, "inspect", "--format", "{{json .}}", name)
	if err != nil || json.Unmarshal(body, &state) != nil || state.ID == "" || !state.State.Running || !state.State.Paused {
		return "", "", errors.New("container pause was not verified")
	}
	return "paused", "docker:" + state.ID, nil
}

func validateDeploymentClaim(c contracts.DeploymentClaim, now time.Time) error {
	if c.InstanceLineageID == "" || c.DeploymentID == "" || c.AttemptID == "" || c.Fence < 1 || c.ServiceID == "" || c.RuntimeBackend != "docker" {
		return errors.New("invalid deployment identity or runtime backend")
	}
	if c.CleanupOnly {
		if c.DesiredOp != "stop" || c.Grant.GrantID == "" {
			return errors.New("cleanup-only claim must identify a revoked grant and request stop")
		}
		return nil
	}
	if c.Manifest.SchemaVersion != 1 || c.ReleaseDigest == "" || c.Manifest.ArtifactDigest == "" || strings.TrimSpace(c.Manifest.Entrypoint) == "" || c.Manifest.HealthCheck == nil {
		return errors.New("release or artifact digest or entrypoint is invalid")
	}
	if _, err := deploymentImage(c.Manifest.ArtifactRef, c.Manifest.ArtifactDigest); err != nil {
		return err
	}
	if c.Manifest.Network.Mode != "none" || len(c.Manifest.Network.AllowedHosts) != 0 {
		return errors.New("network policy is unsupported by this executor")
	}
	if c.Manifest.HealthCheck != nil {
		if err := validateRunnerHealthCheck(*c.Manifest.HealthCheck); err != nil {
			return err
		}
	}
	if len(c.Manifest.SecretRefs) != 0 || c.ConfigRef != "" || c.ConfigSHA256 != "" {
		return errors.New("external configuration and secret resolution are not configured")
	}
	if c.Grant.GrantID == "" || (!c.Grant.ExpiresAt.After(now) && c.ClaimState != "claimed" && c.ClaimState != "uncertain") || !hasString(c.Grant.Actions, c.DesiredOp) {
		return errors.New("authorization grant is expired or does not allow the requested operation")
	}
	if c.Grant.ScopeKind != "instance" && c.Grant.ScopeKind != "personal" {
		return errors.New("authorization scope cannot be resolved by this claim")
	}
	if c.Grant.ScopeID != "" || c.Grant.PrincipalID != "" {
		return errors.New("authorization scope is not supported by this runner")
	}
	if !budgetWithin(c.Grant.Budget, c.Manifest.Budget) {
		return errors.New("grant budget exceeds the immutable manifest budget")
	}
	if c.Manifest.Budget.CPUMillis <= 0 || c.Grant.Budget.CPUMillis <= 0 || c.Manifest.Budget.MemoryBytes <= 0 || c.Grant.Budget.MemoryBytes <= 0 || c.Manifest.Budget.DiskBytes <= 0 || c.Grant.Budget.DiskBytes <= 0 || c.Manifest.Budget.TimeoutSeconds <= 0 || c.Grant.Budget.TimeoutSeconds <= 0 {
		return errors.New("resource budgets must be positive")
	}
	if c.Manifest.Budget.Concurrency != 1 || c.Grant.Budget.Concurrency != 1 {
		return errors.New("service concurrency above one is unsupported")
	}
	if len(c.VolumeIDs) != len(c.Manifest.Mounts) {
		return errors.New("authorized volume set does not match manifest mounts")
	}
	ids := map[string]bool{}
	for _, id := range c.VolumeIDs {
		if id == "" || ids[id] {
			return errors.New("authorized volume set is invalid")
		}
		ids[id] = true
	}
	for _, m := range c.Manifest.Mounts {
		if !ids[m.VolumeID] || m.VolumeID == "" || !strings.HasPrefix(m.Target, "/") || m.Target == "/" || strings.Contains(m.Target, "..") {
			return errors.New("manifest mount is not authorized or safe")
		}
		if !m.ReadOnly {
			return errors.New("writable volume mounts cannot be bounded by the configured disk quota")
		}
	}
	switch c.DesiredOp {
	case "install", "update", "start", "resume", "stop", "pause", "uninstall":
	default:
		return errors.New("unsupported deployment operation")
	}
	return nil
}

func validateRunnerHealthCheck(check contracts.CapabilityHealthCheck) error {
	if check.IntervalSeconds < 5 || check.IntervalSeconds > 300 || check.TimeoutSeconds < 1 || check.TimeoutSeconds > 30 || check.FailureThreshold < 1 || check.FailureThreshold > 10 {
		return errors.New("health check bounds are invalid")
	}
	switch check.Kind {
	case "http":
		if check.Port < 1 || check.Port > 65535 || !safeHealthPath(check.Target) || len(check.Args) != 0 {
			return errors.New("HTTP health check is invalid")
		}
	case "tcp":
		if check.Port < 1 || check.Port > 65535 || check.Target != "" || len(check.Args) != 0 {
			return errors.New("TCP health check is invalid")
		}
	case "exec":
		if !safeExecutable(check.Target) || check.Port != 0 || len(check.Args) > 32 {
			return errors.New("exec health check is invalid")
		}
		for _, arg := range check.Args {
			if len(arg) > 1024 || strings.ContainsAny(arg, "\x00\r\n") {
				return errors.New("exec health check argument is invalid")
			}
		}
	default:
		return errors.New("health check kind is unsupported")
	}
	return nil
}

func dockerHealthCheckArgs(check contracts.CapabilityHealthCheck) []string {
	command := ""
	switch check.Kind {
	case "http":
		command = "curl -fsS --max-time " + strconv.Itoa(check.TimeoutSeconds) + " http://127.0.0.1:" + strconv.Itoa(check.Port) + check.Target + " >/dev/null"
	case "tcp":
		command = "nc -z -w " + strconv.Itoa(check.TimeoutSeconds) + " 127.0.0.1 " + strconv.Itoa(check.Port)
	case "exec":
		parts := []string{shellQuote(check.Target)}
		for _, arg := range check.Args {
			parts = append(parts, shellQuote(arg))
		}
		command = "exec " + strings.Join(parts, " ")
	}
	return []string{"--health-cmd", command, "--health-interval", strconv.Itoa(check.IntervalSeconds) + "s", "--health-timeout", strconv.Itoa(check.TimeoutSeconds) + "s", "--health-retries", strconv.Itoa(check.FailureThreshold)}
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func safeHealthPath(value string) bool {
	if value == "" || len(value) > 1024 || value[0] != '/' {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/-._~%", r)) {
			return false
		}
	}
	return !strings.Contains(value, "..")
}
func safeExecutable(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._+-", r)) {
			return false
		}
	}
	return true
}

func deploymentImage(ref, artifactDigest string) (string, error) {
	ref = strings.TrimPrefix(ref, "oci://")
	if strings.Contains(ref, "://") || !strings.Contains(ref, "@") || !strings.HasSuffix(ref, "@"+artifactDigest) || !strings.HasPrefix(artifactDigest, "sha256:") || len(artifactDigest) != 71 {
		return "", errors.New("artifact must be an OCI reference pinned to the authorized sha256 digest")
	}
	return ref, nil
}

func deploymentContainerName(lineage, id string) string {
	return "zoe-cap-" + shortHash(lineage+"\x00"+id)
}
func deploymentVolumeName(lineage, id string) string {
	return "zoe-vol-" + shortHash(lineage+"\x00"+id)
}
func labelValue(v string) string { return shortHash(v) }
func shortHash(v string) string  { sum := sha256.Sum256([]byte(v)); return hex.EncodeToString(sum[:12]) }
func deploymentLabels(c contracts.DeploymentClaim) map[string]string {
	return map[string]string{"io.zoe.managed": "true", "io.zoe.instance_lineage": labelValue(c.InstanceLineageID), "io.zoe.deployment": labelValue(c.DeploymentID), "io.zoe.release": strings.TrimPrefix(c.ReleaseDigest, "sha256:"), "io.zoe.fence": strconv.FormatInt(c.Fence, 10), "io.zoe.attempt": labelValue(c.AttemptID)}
}

func deploymentOwnershipLabels(c contracts.DeploymentClaim) map[string]string {
	return map[string]string{"io.zoe.managed": "true", "io.zoe.instance_lineage": labelValue(c.InstanceLineageID), "io.zoe.deployment": labelValue(c.DeploymentID)}
}
func deploymentObservationID(c contracts.DeploymentClaim) string {
	return fmt.Sprintf("deploy-%s-%d", shortHash(c.AttemptID), c.Fence)
}
func deploymentErrorCode(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case strings.Contains(err.Error(), "grant") || strings.Contains(err.Error(), "authorization"):
		return "deployment_authorization_denied"
	case strings.Contains(err.Error(), "digest") || strings.Contains(err.Error(), "artifact"):
		return "artifact_digest_invalid"
	case strings.Contains(err.Error(), "network") || strings.Contains(err.Error(), "scope") || strings.Contains(err.Error(), "unsupported"):
		return "deployment_policy_unsupported"
	default:
		return "deployment_execution_failed"
	}
}
func containsDigest(refs []string, digest string) bool {
	for _, ref := range refs {
		if strings.HasSuffix(ref, "@"+digest) {
			return true
		}
	}
	return false
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func budgetWithin(grant, manifest contracts.CapabilityResourceBudget) bool {
	return grant.CPUMillis <= manifest.CPUMillis && grant.MemoryBytes <= manifest.MemoryBytes && grant.DiskBytes <= manifest.DiskBytes && grant.TimeoutSeconds <= manifest.TimeoutSeconds && grant.Concurrency <= manifest.Concurrency
}
