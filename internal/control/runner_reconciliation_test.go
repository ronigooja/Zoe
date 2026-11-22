package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

func TestReconciliationObservationValidatesAbsenceAndReleasesRun(t *testing.T) {
	for _, scenario := range []string{"complete", "incomplete-runtime", "container", "wrong-epoch", "unconfirmed", "session-bound", "unsafe-effect", "ordinary-proof", "retained-complete", "preexisting-complete", "retained-running", "retained-missing-state", "retained-no-selection", "retained-restart-policy", "retained-one-inspection", "retained-count", "preexisting-too-new", "preexisting-no-selection", "preexisting-unpinned-image", "preexisting-runtime-tag", "overlapping-artifacts"} {
		t.Run(scenario, func(t *testing.T) {
			server, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{})
			defer closeStore()
			if err := RegisterInstanceMaintenanceMigration(server.store); err != nil {
				t.Fatal(err)
			}
			if err := server.store.Migrate(context.Background()); err != nil {
				t.Fatal(err)
			}
			state, err := instanceStatus(t, server.store)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = server.store.DB().Exec(`UPDATE runs SET status='uncertain' WHERE id='budget-run'`); err != nil {
				t.Fatal(err)
			}
			proof := &contracts.NoEffectProof{JournalPhase: "effect_unknown", RuntimeCreateStarted: true, Outcome: "failed", Reconciliation: &contracts.NoEffectReconciliation{JournalUpdatedAt: time.Now().UTC().Add(-2 * time.Second), RunID: "budget-run", Actor: "operator", Confirmed: true, VerifiedAt: time.Now().UTC(), RuntimeQuery: "docker ps --filter io.zoe.run/attempt/fence", RuntimeInventoryComplete: true, DockerQuery: "docker ps -aq --no-trunc; docker inspect all IDs", DockerInventoryComplete: true, DockerInventoryChecks: 2, InstanceID: state.InstanceID, InstanceEpoch: state.Epoch}}
			if strings.HasPrefix(scenario, "retained-") || scenario == "overlapping-artifacts" {
				retained := retainedProofFixture()
				proof.Reconciliation.RetainedNeverStartedContainers = []contracts.RetainedNeverStartedContainer{retained}
				proof.Reconciliation.RetainNeverStartedContainerIDs = []string{retained.ID}
				proof.Reconciliation.DockerInventoryContainerCount = 1
			}
			if strings.HasPrefix(scenario, "preexisting-") || scenario == "overlapping-artifacts" {
				capability := preexistingProofFixture()
				if scenario == "overlapping-artifacts" {
					capability.ID = proof.Reconciliation.RetainNeverStartedContainerIDs[0]
				}
				proof.Reconciliation.PreexistingCapabilityContainers = []contracts.PreexistingCapabilityContainer{capability}
				proof.Reconciliation.UnrelatedPreexistingCapabilityContainerIDs = []string{capability.ID}
				proof.Reconciliation.DockerInventoryContainerCount++
			}
			observe := func(obs contracts.RunnerObservation) *httptest.ResponseRecorder {
				body, _ := json.Marshal(obs)
				req := httptest.NewRequest("POST", "/runner/v1/runs/budget-run/observations", bytes.NewReader(body))
				req.SetPathValue("run_id", "budget-run")
				req.Header.Set("Idempotency-Key", obs.ObservationID)
				req = req.WithContext(context.WithValue(req.Context(), runnerPrincipalKey{}, contracts.RunnerPrincipal{RunnerID: "budget-runner", Scopes: []string{"runner:observe"}}))
				w := httptest.NewRecorder()
				server.handleObservation(w, req)
				return w
			}
			unknown := contracts.RunnerObservation{ObservationID: "unknown", RunID: "budget-run", AttemptID: "budget-attempt", Fence: 91, Kind: "effect_unknown", ExternalEffects: []contracts.ExternalEffect{{ID: "create", Kind: "runtime_create", Status: "unknown"}}}
			if w := observe(unknown); w.Code != 200 {
				t.Fatalf("unknown: %d %s", w.Code, w.Body.String())
			}
			switch scenario {
			case "retained-running":
				*proof.Reconciliation.RetainedNeverStartedContainers[0].Running = true
			case "retained-missing-state":
				proof.Reconciliation.RetainedNeverStartedContainers[0].Dead = nil
			case "retained-no-selection":
				proof.Reconciliation.RetainNeverStartedContainerIDs = nil
			case "retained-restart-policy":
				proof.Reconciliation.RetainedNeverStartedContainers[0].RestartPolicy = "always"
			case "retained-one-inspection":
				proof.Reconciliation.DockerInventoryChecks = 1
			case "retained-count":
				proof.Reconciliation.DockerInventoryContainerCount = 0
			case "preexisting-too-new":
				proof.Reconciliation.PreexistingCapabilityContainers[0].CreatedAt = time.Now().UTC()
			case "preexisting-no-selection":
				proof.Reconciliation.UnrelatedPreexistingCapabilityContainerIDs = nil
			case "preexisting-unpinned-image":
				proof.Reconciliation.PreexistingCapabilityContainers[0].Image = "python:latest"
			case "preexisting-runtime-tag":
				proof.Reconciliation.PreexistingCapabilityContainers[0].Labels["io.zoe.runtime"] = ""

			case "incomplete-runtime":
				proof.Reconciliation.RuntimeInventoryComplete = false
			case "container":
				proof.Reconciliation.DockerRuntimeCount = 1
			case "wrong-epoch":
				proof.Reconciliation.InstanceEpoch++
			case "unconfirmed":
				proof.Reconciliation.Confirmed = false
			case "ordinary-proof":
				proof.Reconciliation = nil
				proof.JournalPhase = "claimed"
				proof.RuntimeCreateStarted = false
			case "session-bound":
				if _, err = server.store.DB().Exec(`UPDATE runs SET runtime_session_id='session' WHERE id='budget-run'`); err != nil {
					t.Fatal(err)
				}
			case "unsafe-effect":
				if _, err = server.store.DB().Exec(`UPDATE external_effects SET kind='send' WHERE run_id='budget-run'`); err != nil {
					t.Fatal(err)
				}
			}
			obs := contracts.RunnerObservation{ObservationID: "reconciled", RunID: "budget-run", AttemptID: "budget-attempt", Fence: 91, Kind: "no_effect_terminal", NoEffectProof: proof, Error: "No runtime exists"}
			w := observe(obs)
			var runStatus, effectStatus string
			if err = server.store.DB().QueryRow(`SELECT status FROM runs WHERE id='budget-run'`).Scan(&runStatus); err != nil {
				t.Fatal(err)
			}
			if err = server.store.DB().QueryRow(`SELECT status FROM external_effects WHERE id='create'`).Scan(&effectStatus); err != nil {
				t.Fatal(err)
			}
			if scenario != "complete" && !strings.HasSuffix(scenario, "-complete") {
				if w.Code != 409 || runStatus != "uncertain" || effectStatus != "unknown" {
					t.Fatalf("unsafe proof accepted: %d %s run=%s effect=%s", w.Code, w.Body.String(), runStatus, effectStatus)
				}
				return
			}
			expectedEffectStatus := "no_effect"
			if strings.HasPrefix(scenario, "retained-") {
				expectedEffectStatus = "runtime_not_started"
			}
			if w.Code != 200 || runStatus != "failed" || effectStatus != expectedEffectStatus {
				t.Fatalf("valid reconciliation: %d %s run=%s effect=%s", w.Code, w.Body.String(), runStatus, effectStatus)
			}
			if replay := observe(obs); replay.Code != 200 {
				t.Fatalf("lost ack replay: %d %s", replay.Code, replay.Body.String())
			}
			var receipts int
			if err = server.store.DB().QueryRow(`SELECT count(*) FROM runner_observations WHERE kind='no_effect_terminal'`).Scan(&receipts); err != nil || receipts != 1 {
				t.Fatalf("receipt count=%d err=%v", receipts, err)
			}
		})
	}
}

func TestRuntimeCreateRejectionReleasesCapacityOnlyForAllowlistedLocalCode(t *testing.T) {
	for _, status := range []int{409, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			// 409 models an allowlisted local rejection code; 503 models a code outside the allowlist.
			code := "container_create_rejected"
			if status == 503 {
				code = "docker_unreachable"
			}
			server, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{})
			defer closeStore()
			if _, err := server.store.DB().Exec(`UPDATE runs SET status='dispatching' WHERE id='budget-run'`); err != nil {
				t.Fatal(err)
			}
			observe := func(obs contracts.RunnerObservation) *httptest.ResponseRecorder {
				body, err := json.Marshal(obs)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest("POST", "/runner/v1/runs/budget-run/observations", bytes.NewReader(body))
				req.SetPathValue("run_id", "budget-run")
				req.Header.Set("Idempotency-Key", obs.ObservationID)
				req = req.WithContext(context.WithValue(req.Context(), runnerPrincipalKey{}, contracts.RunnerPrincipal{RunnerID: "budget-runner", Scopes: []string{"runner:observe"}}))
				w := httptest.NewRecorder()
				server.handleObservation(w, req)
				return w
			}
			started := contracts.RunnerObservation{ObservationID: "started", RunID: "budget-run", AttemptID: "budget-attempt", Fence: 91, Kind: "dispatch_started"}
			if w := observe(started); w.Code != 200 {
				t.Fatalf("dispatch_started: %d %s", w.Code, w.Body.String())
			}
			rejected := contracts.RunnerObservation{ObservationID: "rejected", RunID: "budget-run", AttemptID: "budget-attempt", Fence: 91, Kind: "create_rejected", CreateRejection: &contracts.CreateRejectionProof{Code: code}}
			w := observe(rejected)
			var runStatus, taskStatus string
			if err := server.store.DB().QueryRow(`SELECT runs.status,tasks.status FROM runs JOIN tasks ON tasks.id=runs.task_id WHERE runs.id='budget-run'`).Scan(&runStatus, &taskStatus); err != nil {
				t.Fatal(err)
			}
			if status == 503 {
				if w.Code != 409 || runStatus != "active" || taskStatus != "active" {
					t.Fatalf("untrusted 503 released run: %d %s run=%s task=%s", w.Code, w.Body.String(), runStatus, taskStatus)
				}
				unknown := contracts.RunnerObservation{ObservationID: "unknown", RunID: "budget-run", AttemptID: "budget-attempt", Fence: 91, Kind: "effect_unknown", ExternalEffects: []contracts.ExternalEffect{{ID: "create", Kind: "runtime_create", Status: "unknown"}}}
				if w := observe(unknown); w.Code != 200 {
					t.Fatalf("unknown: %d %s", w.Code, w.Body.String())
				}
			} else {
				if w.Code != 200 || runStatus != "failed" || taskStatus != "failed" {
					t.Fatalf("stable rejection: %d %s run=%s task=%s", w.Code, w.Body.String(), runStatus, taskStatus)
				}
				if replay := observe(rejected); replay.Code != 200 {
					t.Fatalf("rejection receipt replay: %d %s", replay.Code, replay.Body.String())
				}
			}
			var occupied, receipts int
			if err := server.store.DB().QueryRow(`SELECT count(*) FROM runs WHERE status IN ('dispatching','active','uncertain')`).Scan(&occupied); err != nil {
				t.Fatal(err)
			}
			if err := server.store.DB().QueryRow(`SELECT count(*) FROM runner_observations WHERE kind='create_rejected'`).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if status == 409 && (occupied != 0 || receipts != 1) {
				t.Fatalf("rejection leaked capacity or duplicated receipt: slots=%d receipts=%d", occupied, receipts)
			}
			if status == 503 {
				if err := server.store.DB().QueryRow(`SELECT status FROM runs WHERE id='budget-run'`).Scan(&runStatus); err != nil {
					t.Fatal(err)
				}
				if occupied != 1 || receipts != 0 || runStatus != "uncertain" {
					t.Fatalf("ambiguous 503 lost fencing: slots=%d receipts=%d status=%s", occupied, receipts, runStatus)
				}
			}
		})
	}
}

func retainedProofFixture() contracts.RetainedNeverStartedContainer {
	falseValue := false
	zero := 0
	empty := ""
	zeroTime := time.Time{}
	now := time.Now().UTC()
	return contracts.RetainedNeverStartedContainer{ID: strings.Repeat("a", 64), Managed: "true", Runtime: "opencode", Status: "created", Running: &falseValue, Paused: &falseValue, Restarting: &falseValue, Dead: &falseValue, OOMKilled: &falseValue, ExitCode: &zero, Error: &empty, PID: &zero, StartedAt: &zeroTime, FinishedAt: &zeroTime, CreatedAt: now.Add(-time.Hour), RestartCount: &zero, RestartPolicy: "no", FirstCheckedAt: now.Add(-500 * time.Millisecond), RecheckedAt: now.Add(-100 * time.Millisecond), SnapshotSHA256: strings.Repeat("c", 64)}
}
func preexistingProofFixture() contracts.PreexistingCapabilityContainer {
	falseValue := false
	trueValue := true
	pid := 42
	now := time.Now().UTC()
	startedAt := now.Add(-time.Hour)
	return contracts.PreexistingCapabilityContainer{ID: strings.Repeat("b", 64), Name: "/zoe-cap-" + strings.Repeat("2", 24), Image: "python@sha256:" + strings.Repeat("5", 64), Labels: map[string]string{"io.zoe.managed": "true", "io.zoe.workspace": strings.Repeat("1", 24), "io.zoe.deployment": strings.Repeat("2", 24), "io.zoe.attempt": strings.Repeat("3", 24), "io.zoe.release": strings.Repeat("4", 64), "io.zoe.fence": "1"}, CreatedAt: now.Add(-24 * time.Hour), Status: "running", Running: &trueValue, Paused: &falseValue, Restarting: &falseValue, Dead: &falseValue, PID: &pid, StartedAt: &startedAt, FirstCheckedAt: now.Add(-500 * time.Millisecond), RecheckedAt: now.Add(-100 * time.Millisecond), SnapshotSHA256: strings.Repeat("d", 64)}
}
