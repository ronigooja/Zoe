package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

type SessionInventoryReader interface{}

func splitContainerIDs(output string) []string {
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) != "" {
			ids = append(ids, strings.TrimSpace(line))
		}
	}
	return ids
}

type ReconciliationOptions struct {
	UnrelatedPreexistingCapabilityContainerIDs []string
	RetainNeverStartedContainerIDs             []string
	DeploymentAuthority                        *DeploymentAuthority
}

// ReconcileNoEffect never creates, stops, or removes a runtime or Docker container.
// The caller must hold the normal deployment-host and journal process locks.
func (e *Engine) ReconcileNoEffect(ctx context.Context, runIDs []string, actor string, confirmed bool, inventory SessionInventoryReader, docker DockerCommand, options ...ReconciliationOptions) error {
	actor = strings.TrimSpace(actor)
	if len(runIDs) == 0 || actor == "" || len(actor) > 200 || strings.ContainsAny(actor, "\r\n") || !confirmed {
		return errors.New("explicit run IDs, actor, and no-effect confirmation are required")
	}
	if e.Journal == nil || e.Control == nil || e.Instance == nil || docker == nil {
		return errors.New("reconciliation requires journal, control, instance fence, and Docker inventory")
	}
	opts := ReconciliationOptions{}
	if len(options) > 1 {
		return errors.New("only one reconciliation options set is supported")
	}
	if len(options) == 1 {
		opts = options[0]
	}
	retainedIDs := map[string]bool{}
	for _, id := range opts.RetainNeverStartedContainerIDs {
		if !contracts.ValidFullContainerID(id) || retainedIDs[id] {
			return errors.New("retained container IDs must be unique full lowercase Docker IDs")
		}
		retainedIDs[id] = true
	}
	for _, id := range opts.UnrelatedPreexistingCapabilityContainerIDs {
		if !contracts.ValidFullContainerID(id) || retainedIDs[id] {
			return errors.New("preexisting capability IDs must be unique full lowercase Docker IDs without retained runtime overlap")
		}
		retainedIDs[id] = true
	}
	seen := map[string]bool{}
	for _, id := range runIDs {
		if id == "" || strings.TrimSpace(id) != id || seen[id] {
			return errors.New("run IDs must be explicit, nonempty and unique")
		}
		seen[id] = true
	}
	for _, id := range runIDs {
		if err := e.reconcileNoEffect(ctx, id, actor, inventory, docker, opts); err != nil {
			return fmt.Errorf("reconcile run %s: %w", id, err)
		}
	}
	return nil
}

func (e *Engine) reconcileNoEffect(ctx context.Context, id, actor string, inventory SessionInventoryReader, docker DockerCommand, opts ReconciliationOptions) (resultErr error) {
	checkedAt := time.Now().UTC()
	auditID := observationID(id, actor, 0, "reconciliation_audit", checkedAt.Format(time.RFC3339Nano))
	audit := map[string]any{"confirmed": true, "outcome": "started", "runtime_query": "docker ps --filter io.zoe.run/attempt/fence", "runtime_result": "not_checked", "docker_query": "docker ps -aq --no-trunc; docker inspect all IDs", "docker_result": "not_checked", "stage": "journal", "retain_never_started_container_ids": opts.RetainNeverStartedContainerIDs, "unrelated_preexisting_capability_container_ids": opts.UnrelatedPreexistingCapabilityContainerIDs}
	if err := e.Journal.SaveReconciliationAudit(ctx, auditID, id, actor, checkedAt, audit); err != nil {
		return err
	}
	defer func() {
		audit["outcome"] = "refused"
		if resultErr == nil {
			audit["outcome"] = "completed"
		}
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := e.Journal.SaveReconciliationAudit(auditCtx, auditID, id, actor, checkedAt, audit); err != nil {
			resultErr = fmt.Errorf("persist reconciliation audit: %w", err)
		}
	}()

	entry, ok, err := e.Journal.Entry(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("run has no durable journal claim")
	}
	audit["attempt_id"] = entry.Claim.AttemptID
	audit["fence"] = entry.Claim.Fence
	audit["instance_id"] = entry.InstanceID
	audit["instance_epoch"] = entry.Epoch
	audit["journal_updated_at"] = entry.Updated
	if err := e.checkInstance(ctx, entry.Claim, false); err != nil {
		return err
	}
	audit["stage"] = "receipt_replay"
	receipt, err := e.Journal.NoEffectReceipt(ctx, id)
	if err != nil {
		return err
	}
	if receipt != nil {
		if receipt.NoEffectProof == nil || receipt.NoEffectProof.Reconciliation == nil {
			return errors.New("run has a different terminal receipt")
		}
		if err := e.Journal.SetPhase(ctx, id, entry.Claim.AttemptID, entry.Claim.Fence, "no_effect_terminal"); err != nil {
			return err
		}
		return e.flush(ctx, id)
	}
	if entry.Phase != "effect_unknown" || entry.Session != "" || entry.Container != "" || entry.Claim.RuntimeSessionID != "" || entry.Spec.RunID != id {
		return errors.New("only an effect_unknown run with an exact create request and no session or container binding can be reconciled")
	}
	audit["stage"] = "server_claim"
	current, err := e.Control.Current(ctx)
	if err != nil {
		return err
	}
	matches := 0
	for _, claim := range current {
		if claim.RunID == id {
			if !sameRecoveredClaim(entry.Claim, claim, "") {
				return errors.New("server claim differs from durable fence")
			}
			matches++
		}
	}
	if matches != 1 {
		return errors.New("run is not a current server-owned claim")
	}
	audit["stage"] = "runtime_inventory"
	audit["runtime_result"] = "label lookup"
	// A process incarnation newer than the unknown journal write rules out an
	// original HTTP request delayed before the runtime handler or inventory gate.
	audit["stage"] = "docker_inventory"
	audit["docker_result"] = "unavailable_or_runtime_present"
	var originalDispatchAt time.Time
	if len(opts.UnrelatedPreexistingCapabilityContainerIDs) > 0 {
		originalDispatchAt, err = e.originalDispatchTime(ctx, entry.Claim)
		if err != nil {
			return err
		}
		audit["original_dispatch_at"] = originalDispatchAt
	}
	retained, unrelated, preexisting, unmanaged, err := e.verifyNoLaunchedRuntime(ctx, docker, opts, entry.Updated, originalDispatchAt)
	if err != nil {
		audit["docker_result"] = err.Error()
		return err
	}
	audit["retained_never_started_containers"] = retained
	audit["unrelated_capability_container_ids"] = unrelated
	audit["preexisting_capability_containers"] = preexisting
	audit["unmanaged_container_ids"] = unmanaged

	if err := e.checkInstance(ctx, entry.Claim, false); err != nil {
		return err
	}
	audit["docker_result"] = "complete_no_launched_runtime"
	audit["stage"] = "terminal_receipt"
	proof := &contracts.NoEffectProof{JournalPhase: "effect_unknown", RuntimeCreateStarted: true, Outcome: "failed", Reconciliation: &contracts.NoEffectReconciliation{
		JournalUpdatedAt: entry.Updated, RunID: id, Actor: actor, Confirmed: true, VerifiedAt: time.Now().UTC(), RuntimeQuery: "docker ps --filter io.zoe.run/attempt/fence", RuntimeInventoryComplete: true, RuntimeMatches: 0,
		DockerQuery: "docker ps -aq --no-trunc; docker inspect all IDs", DockerInventoryComplete: true, DockerInventoryChecks: 2, DockerInventoryContainerCount: len(retained) + len(unrelated) + len(preexisting) + len(unmanaged), UnmanagedContainerIDs: unmanaged, UnrelatedPreexistingCapabilityContainerIDs: opts.UnrelatedPreexistingCapabilityContainerIDs, PreexistingCapabilityContainers: preexisting, RetainNeverStartedContainerIDs: opts.RetainNeverStartedContainerIDs, RetainedNeverStartedContainers: retained, UnrelatedCapabilityContainerIDs: unrelated, InstanceID: entry.InstanceID, InstanceEpoch: entry.Epoch,
	}}
	// Persist the complete immutable receipt before changing phase or sending it.
	// emit queues before delivery; any crash is recovered by NoEffectReceipt.
	err = e.emitNoEffectTerminal(ctx, entry.Claim, "operator_reconciled_absence", "Read-only inventories proved no launched runtime. Explicitly selected never-started Docker records were preserved; the operator reconciled the unknown create outcome.", proof)
	durable, receiptErr := e.Journal.NoEffectReceipt(ctx, id)
	if receiptErr != nil {
		return receiptErr
	}
	if durable == nil {
		return err
	}
	if phaseErr := e.Journal.SetPhase(ctx, id, entry.Claim.AttemptID, entry.Claim.Fence, "no_effect_terminal"); phaseErr != nil {
		return phaseErr
	}
	return err
}

type reconciliationContainerInspect struct {
	ID           string    `json:"Id"`
	Name         string    `json:"Name"`
	Created      time.Time `json:"Created"`
	RestartCount *int      `json:"RestartCount"`
	Config       struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
	State struct {
		Status     string     `json:"Status"`
		Running    *bool      `json:"Running"`
		Paused     *bool      `json:"Paused"`
		Restarting *bool      `json:"Restarting"`
		Dead       *bool      `json:"Dead"`
		OOMKilled  *bool      `json:"OOMKilled"`
		ExitCode   *int       `json:"ExitCode"`
		Error      *string    `json:"Error"`
		PID        *int       `json:"Pid"`
		StartedAt  *time.Time `json:"StartedAt"`
		FinishedAt *time.Time `json:"FinishedAt"`
	} `json:"State"`
}

type reconciliationDockerSnapshot struct {
	ids         []string
	retained    []contracts.RetainedNeverStartedContainer
	unrelated   []string
	hashes      map[string]string
	unmanaged   []string
	preexisting []contracts.PreexistingCapabilityContainer
}

func (e *Engine) verifyNoLaunchedRuntime(ctx context.Context, docker DockerCommand, opts ReconciliationOptions, runtimeStartedAt, runStartedAt time.Time) ([]contracts.RetainedNeverStartedContainer, []string, []contracts.PreexistingCapabilityContainer, []string, error) {
	first, err := e.reconciliationDockerSnapshot(ctx, docker, opts, runtimeStartedAt, runStartedAt)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	second, err := e.reconciliationDockerSnapshot(ctx, docker, opts, runtimeStartedAt, runStartedAt)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if !reflect.DeepEqual(first.hashes, second.hashes) || !reflect.DeepEqual(first.ids, second.ids) || !reflect.DeepEqual(first.unmanaged, second.unmanaged) || !reflect.DeepEqual(first.unrelated, second.unrelated) || len(first.retained) != len(second.retained) || len(first.preexisting) != len(second.preexisting) {
		return nil, nil, nil, nil, errors.New("Docker inventory changed between read-only checks")
	}
	for i := range first.retained {
		if first.retained[i].ID != second.retained[i].ID || first.retained[i].SnapshotSHA256 != second.retained[i].SnapshotSHA256 {
			return nil, nil, nil, nil, errors.New("retained Docker container changed between read-only checks")
		}
		first.retained[i].RecheckedAt = second.retained[i].FirstCheckedAt
	}
	for i := range first.preexisting {
		if first.preexisting[i].ID != second.preexisting[i].ID || first.preexisting[i].SnapshotSHA256 != second.preexisting[i].SnapshotSHA256 {
			return nil, nil, nil, nil, errors.New("preexisting capability container changed between read-only checks")
		}
		first.preexisting[i].RecheckedAt = second.preexisting[i].FirstCheckedAt
	}
	return first.retained, first.unrelated, first.preexisting, first.unmanaged, nil
}

func (e *Engine) reconciliationDockerSnapshot(ctx context.Context, docker DockerCommand, opts ReconciliationOptions, runtimeStartedAt, runStartedAt time.Time) (reconciliationDockerSnapshot, error) {
	snapshot := reconciliationDockerSnapshot{hashes: map[string]string{}}
	output, err := docker.Run(ctx, "ps", "-aq", "--no-trunc")
	if err != nil {
		return snapshot, errors.New("complete Docker inventory unavailable")
	}
	ids := splitContainerIDs(string(output))
	sort.Strings(ids)
	snapshot.ids = ids
	retainedIDs := map[string]bool{}
	for _, id := range opts.RetainNeverStartedContainerIDs {
		retainedIDs[id] = false
	}
	preexistingIDs := map[string]bool{}
	for _, id := range opts.UnrelatedPreexistingCapabilityContainerIDs {
		preexistingIDs[id] = false
	}
	for index, id := range ids {
		if !contracts.ValidFullContainerID(id) || index > 0 && ids[index-1] == id {
			return snapshot, errors.New("Docker inventory identity is incomplete")
		}
		body, err := docker.Run(ctx, "inspect", "--format", "{{json .}}", id)
		if err != nil {
			return snapshot, errors.New("Docker inventory changed or inspection failed")
		}
		var container reconciliationContainerInspect
		if json.Unmarshal(body, &container) != nil || container.ID != id || container.Name == "" || container.Config.Image == "" || container.Config.Labels == nil || container.Created.IsZero() || container.State.Status == "" || container.State.Running == nil || container.State.Paused == nil || container.State.Restarting == nil || container.State.Dead == nil || container.State.PID == nil || container.State.StartedAt == nil || container.State.FinishedAt == nil {
			return snapshot, errors.New("Docker inventory identity is incomplete")
		}
		canonicalBody, canonicalErr := canonicalDockerInspection(body)
		if canonicalErr != nil {
			return snapshot, canonicalErr
		}
		fullDigest := sha256.Sum256(canonicalBody)
		snapshot.hashes[id] = hex.EncodeToString(fullDigest[:])
		labels := container.Config.Labels
		if _, selected := preexistingIDs[id]; selected {
			checkedAt := time.Now().UTC()

			proof := contracts.PreexistingCapabilityContainer{ID: id, Name: container.Name, Image: container.Config.Image, Labels: labels, CreatedAt: container.Created, Status: container.State.Status, Running: container.State.Running, Paused: container.State.Paused, Restarting: container.State.Restarting, Dead: container.State.Dead, PID: container.State.PID, StartedAt: container.State.StartedAt, FirstCheckedAt: checkedAt, RecheckedAt: checkedAt, SnapshotSHA256: hex.EncodeToString(fullDigest[:])}
			if !proof.Valid(runStartedAt, runtimeStartedAt, checkedAt) {
				return snapshot, errors.New("explicit preexisting capability container identity, state or creation boundary is unproven")
			}
			// Persist only the public ownership labels, never arbitrary Docker labels.
			proof.Labels = map[string]string{}
			for _, key := range []string{"io.zoe.managed", "io.zoe.workspace", "io.zoe.deployment", "io.zoe.release", "io.zoe.fence", "io.zoe.attempt"} {
				proof.Labels[key] = labels[key]
			}
			preexistingIDs[id] = true
			snapshot.preexisting = append(snapshot.preexisting, proof)
			continue
		}
		_, selected := retainedIDs[id]
		_, hasRuntimeLabel := labels[runtimeLabel]
		runtimeContainer := hasRuntimeLabel
		if selected {
			checkedAt := time.Now().UTC()

			proof := contracts.RetainedNeverStartedContainer{ID: id, Managed: labels[runtimeManagedLabel], Runtime: labels[runtimeLabel], Status: container.State.Status, Running: container.State.Running, Paused: container.State.Paused, Restarting: container.State.Restarting, Dead: container.State.Dead, OOMKilled: container.State.OOMKilled, ExitCode: container.State.ExitCode, Error: container.State.Error, PID: container.State.PID, StartedAt: container.State.StartedAt, FinishedAt: container.State.FinishedAt, CreatedAt: container.Created, RestartCount: container.RestartCount, RestartPolicy: container.HostConfig.RestartPolicy.Name, FirstCheckedAt: checkedAt, RecheckedAt: checkedAt, SnapshotSHA256: hex.EncodeToString(fullDigest[:])}
			if !proof.Valid(runtimeStartedAt, checkedAt) {
				return snapshot, errors.New("explicitly retained runtime container is not fully verified as never started")
			}
			retainedIDs[id] = true
			snapshot.retained = append(snapshot.retained, proof)
			continue
		}
		if runtimeContainer {
			return snapshot, errors.New("uncorrelated runtime container exists; launched-runtime absence is unproven")
		}
		if labels[runtimeManagedLabel] == "true" {
			if err := e.verifyUnrelatedCapabilityContainer(ctx, container, opts.DeploymentAuthority); err != nil {
				return snapshot, err
			}
			snapshot.unrelated = append(snapshot.unrelated, id)
		} else {
			if labels[runtimeManagedLabel] != "" && labels[runtimeManagedLabel] != "false" {
				return snapshot, errors.New("container managed ownership is ambiguous")
			}
			snapshot.unmanaged = append(snapshot.unmanaged, id)
		}
	}
	for _, found := range retainedIDs {
		if !found {
			return snapshot, errors.New("explicitly retained container is absent from the complete Docker inventory")
		}
	}
	for _, found := range preexistingIDs {
		if !found {
			return snapshot, errors.New("explicit preexisting capability container is absent from complete inventory")
		}
	}
	return snapshot, nil
}

func (e *Engine) verifyUnrelatedCapabilityContainer(ctx context.Context, container reconciliationContainerInspect, authority *DeploymentAuthority) error {
	if authority == nil || authority.db == nil {
		return errors.New("managed capability container lacks host deployment authority")
	}
	rows, err := e.Journal.db.QueryContext(ctx, `SELECT claim_json FROM journal_deployments`)
	if err != nil {
		return err
	}
	var claims []contracts.DeploymentClaim
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			rows.Close()
			return err
		}
		var claim contracts.DeploymentClaim
		if json.Unmarshal(body, &claim) != nil {
			rows.Close()
			return errors.New("deployment journal claim is invalid")
		}
		claims = append(claims, claim)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, claim := range claims {
		if strings.TrimPrefix(container.Name, "/") != deploymentContainerName(claim.DeploymentID) {
			continue
		}
		expected := deploymentLabels(claim)
		for key, value := range expected {
			if container.Config.Labels[key] != value {
				return errors.New("capability container ownership differs from durable deployment claim")
			}
		}
		fingerprint, err := deploymentClaimFingerprint(claim)
		if err != nil {
			return err
		}
		var fence int64
		var actualFingerprint string
		var started int
		if err := authority.db.QueryRowContext(ctx, `SELECT fence,fingerprint,effect_started FROM deployment_fences WHERE deployment_id=?`, claim.DeploymentID).Scan(&fence, &actualFingerprint, &started); err != nil {
			return errors.New("capability container deployment authority is unavailable")
		}
		if fence != claim.Fence || actualFingerprint != fingerprint || started != 1 {
			return errors.New("capability container deployment authority conflicts with its journal")
		}
		return nil
	}
	return errors.New("managed container has no positively matched capability deployment claim")
}

func (e *Engine) originalDispatchTime(ctx context.Context, claim contracts.JobClaim) (time.Time, error) {
	rows, err := e.Journal.db.QueryContext(ctx, `SELECT body FROM journal_observations WHERE run_id=? ORDER BY created_at`, claim.RunID)
	if err != nil {
		return time.Time{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return time.Time{}, err
		}
		var obs contracts.RunnerObservation
		if json.Unmarshal(body, &obs) != nil {
			return time.Time{}, errors.New("journal observation is invalid")
		}
		if obs.Kind == "dispatch_started" && obs.AttemptID == claim.AttemptID && obs.Fence == claim.Fence {
			_, offset := obs.OccurredAt.Zone()
			if obs.OccurredAt.IsZero() || offset != 0 {
				return time.Time{}, errors.New("original dispatch timestamp is invalid")
			}
			return obs.OccurredAt, nil
		}
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, err
	}
	return time.Time{}, errors.New("preexisting capability exclusion requires the original journaled dispatch observation")
}

// canonicalDockerInspection normalizes only Docker's unordered top-level
// Mounts inventory. All values and every other array retain their order.
func canonicalDockerInspection(body []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var inspection map[string]any
	if err := decoder.Decode(&inspection); err != nil || inspection == nil {
		return nil, errors.New("Docker container inspection is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("Docker container inspection contains trailing data")
	}
	if rawMounts, exists := inspection["Mounts"]; exists && rawMounts != nil {
		mounts, ok := rawMounts.([]any)
		if !ok {
			return nil, errors.New("Docker mount inventory is invalid")
		}
		type sortableMount struct {
			value                                map[string]any
			destination, source, kind, canonical string
		}
		ordered := make([]sortableMount, 0, len(mounts))
		for _, raw := range mounts {
			mount, ok := raw.(map[string]any)
			if !ok || mount == nil {
				return nil, errors.New("Docker mount inventory entry is invalid")
			}
			encoded, err := json.Marshal(mount)
			if err != nil {
				return nil, err
			}
			destination, _ := mount["Destination"].(string)
			source, _ := mount["Source"].(string)
			kind, _ := mount["Type"].(string)
			ordered = append(ordered, sortableMount{mount, destination, source, kind, string(encoded)})
		}
		sort.Slice(ordered, func(i, j int) bool {
			a, b := ordered[i], ordered[j]
			if a.destination != b.destination {
				return a.destination < b.destination
			}
			if a.source != b.source {
				return a.source < b.source
			}
			if a.kind != b.kind {
				return a.kind < b.kind
			}
			return a.canonical < b.canonical
		})
		for i, mount := range ordered {
			mounts[i] = mount.value
		}
	}
	return json.Marshal(inspection)
}
