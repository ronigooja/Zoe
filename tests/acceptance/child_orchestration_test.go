package acceptance

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

func TestChildProposalAdmissionIsIdempotentAndClaimedThroughOrdinaryRunnerPath(t *testing.T) {
	bootstrapCode, deviceToken := reviewToken(t), reviewToken(t)
	runnerOne, runnerTwo, runnerThree, runnerFour := reviewToken(t), reviewToken(t), reviewToken(t), reviewToken(t)
	policy := control.ChildPolicy{AllowedProfileIDs: []string{"child"}, CapabilityIDs: []string{"task.read"}, MaxChildRuns: 3, MaxDepth: 3}
	parentProfile := control.RuntimeProfile{ID: "parent", RuntimeRouteID: "native", ModelRouteID: "model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1", CapabilityIDs: []string{"task.read"}, ChildPolicy: policy}
	childProfile := control.RuntimeProfile{ID: "child", RuntimeRouteID: "native", ModelRouteID: "model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1", CapabilityIDs: []string{"task.read"}, ChildPolicy: policy}
	store, h := reviewServer(t, control.Config{DefaultProfileID: "parent", Profiles: map[string]control.RuntimeProfile{"parent": parentProfile, "child": childProfile}}, func(s *control.Server) {
		for i, token := range []string{runnerOne, runnerTwo, runnerThree, runnerFour} {
			if err := s.RegisterRunnerCredential(context.Background(), token, "runner-"+string(rune('1'+i)), []string{"runner:claim", "runner:propose"}); err != nil {
				t.Fatal(err)
			}
		}
	})
	reviewInitializeInstance(t, h, bootstrapCode, deviceToken)
	created := reviewRequest(h, http.MethodPost, "/v1/commands", deviceToken, "root-create", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"root","goal":"root goal","source":"user"}`)})
	if created.Code != http.StatusOK {
		t.Fatalf("root create=%d %s", created.Code, created.Body.String())
	}
	var rootReceipt contracts.CommandReceipt
	if err := json.Unmarshal(created.Body.Bytes(), &rootReceipt); err != nil {
		t.Fatal(err)
	}
	claimed := reviewRequest(h, http.MethodPost, "/runner/v1/claims", runnerOne, "", nil)
	if claimed.Code != http.StatusOK {
		t.Fatalf("root claim=%d %s", claimed.Code, claimed.Body.String())
	}
	var rootClaim contracts.JobClaim
	if err := json.Unmarshal(claimed.Body.Bytes(), &rootClaim); err != nil {
		t.Fatal(err)
	}
	proposal := contracts.ChildTaskProposalRequest{ProposalID: "proposal-1", Attempt: rootClaim.AttemptID, Fence: rootClaim.Fence, Kind: "child_task", Title: "child", Goal: "child goal", ProfileID: "child", RequestedCapabilityIDs: []string{"task.read"}, Budget: contracts.ChildTaskBudget{MaxChildRuns: 2, MaxDepth: 2}}
	path := "/runner/v1/runs/" + rootClaim.RunID + "/proposals"
	first := reviewRequest(h, http.MethodPost, path, runnerOne, "", proposal)
	if first.Code != http.StatusOK {
		t.Fatalf("child proposal=%d %s", first.Code, first.Body.String())
	}
	var firstReceipt contracts.ChildTaskProposalReceipt
	if err := json.Unmarshal(first.Body.Bytes(), &firstReceipt); err != nil {
		t.Fatal(err)
	}
	if firstReceipt.Status != "admitted" || firstReceipt.TaskID == "" {
		t.Fatalf("unexpected admission receipt: %+v", firstReceipt)
	}
	retry := reviewRequest(h, http.MethodPost, path, runnerOne, "", proposal)
	var retryReceipt contracts.ChildTaskProposalReceipt
	if retry.Code != http.StatusOK || json.Unmarshal(retry.Body.Bytes(), &retryReceipt) != nil || retryReceipt.TaskID != firstReceipt.TaskID {
		t.Fatalf("exact proposal retry changed result: status=%d body=%s", retry.Code, retry.Body.String())
	}
	// A second runner with the same scopes must not be able to retrieve the
	// first runner's durable receipt, even when it knows the complete tuple.
	foreignReplay := reviewRequest(h, http.MethodPost, path, runnerTwo, "", proposal)
	if foreignReplay.Code != http.StatusNotFound {
		t.Fatalf("foreign runner replay disclosed proposal receipt: %d %s", foreignReplay.Code, foreignReplay.Body.String())
	}
	changed := proposal
	changed.Goal = "changed content"
	if response := reviewRequest(h, http.MethodPost, path, runnerOne, "", changed); response.Code != http.StatusConflict {
		t.Fatalf("changed content reused proposal identity: %d %s", response.Code, response.Body.String())
	}
	var lineageRoot, lineageParent, source string
	var depth, reservations int
	if err := store.DB().QueryRow(`SELECT lineage_root_task_id,lineage_parent_task_id,lineage_depth,source FROM tasks WHERE id=?`, firstReceipt.TaskID).Scan(&lineageRoot, &lineageParent, &depth, &source); err != nil {
		t.Fatal(err)
	}
	if lineageRoot != rootReceipt.ResourceID || lineageParent != rootReceipt.ResourceID || depth != 1 || source != "child" {
		t.Fatalf("child lineage was not bound to parent: root=%s parent=%s depth=%d source=%s", lineageRoot, lineageParent, depth, source)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM child_budget_reservations WHERE scope_task_id=?`, rootReceipt.ResourceID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 1 {
		t.Fatalf("child reservation count=%d", reservations)
	}
	childClaimResponse := reviewRequest(h, http.MethodPost, "/runner/v1/claims", runnerTwo, "", nil)
	if childClaimResponse.Code != http.StatusOK {
		t.Fatalf("child claim=%d %s", childClaimResponse.Code, childClaimResponse.Body.String())
	}
	var childClaim contracts.JobClaim
	if err := json.Unmarshal(childClaimResponse.Body.Bytes(), &childClaim); err != nil {
		t.Fatal(err)
	}
	if childClaim.TaskID != firstReceipt.TaskID || childClaim.LineageRootTaskID != rootReceipt.ResourceID || childClaim.LineageParentTaskID != rootReceipt.ResourceID || childClaim.LineageDepth != 1 || len(childClaim.Authorization.CapabilityIDs) != 1 || childClaim.Authorization.CapabilityIDs[0] != "task.read" {
		t.Fatalf("child did not use ordinary claim with bounded lineage grants: %+v", childClaim)
	}
	dependencyResponse := reviewRequest(h, http.MethodPost, "/v1/commands", deviceToken, "dependency-create", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"dependency","goal":"dependency goal","source":"user"}`)})
	var dependencyReceipt contracts.CommandReceipt
	if dependencyResponse.Code != http.StatusOK || json.Unmarshal(dependencyResponse.Body.Bytes(), &dependencyReceipt) != nil {
		t.Fatalf("dependency create=%d %s", dependencyResponse.Code, dependencyResponse.Body.String())
	}
	dependentProposal := proposal
	dependentProposal.ProposalID = "proposal-waiting"
	dependentProposal.DependencyTaskIDs = []string{dependencyReceipt.ResourceID}
	waitingResponse := reviewRequest(h, http.MethodPost, path, runnerOne, "", dependentProposal)
	var waitingReceipt contracts.ChildTaskProposalReceipt
	if waitingResponse.Code != http.StatusOK || json.Unmarshal(waitingResponse.Body.Bytes(), &waitingReceipt) != nil {
		t.Fatalf("dependent child proposal=%d %s", waitingResponse.Code, waitingResponse.Body.String())
	}
	var childStatus string
	if err := store.DB().QueryRow(`SELECT status FROM tasks WHERE id=?`, waitingReceipt.TaskID).Scan(&childStatus); err != nil {
		t.Fatal(err)
	}
	if childStatus != "waiting" {
		t.Fatalf("child with unfinished dependency status=%q", childStatus)
	}
	dependencyClaim := reviewRequest(h, http.MethodPost, "/runner/v1/claims", runnerThree, "", nil)
	var claimedDependency contracts.JobClaim
	if dependencyClaim.Code != http.StatusOK || json.Unmarshal(dependencyClaim.Body.Bytes(), &claimedDependency) != nil || claimedDependency.TaskID != dependencyReceipt.ResourceID {
		t.Fatalf("unfinished dependency was not claimed independently: %d %s", dependencyClaim.Code, dependencyClaim.Body.String())
	}
	if blocked := reviewRequest(h, http.MethodPost, "/runner/v1/claims", runnerFour, "", nil); blocked.Code != http.StatusNoContent {
		t.Fatalf("dependency-gated child escaped claim queue: %d %s", blocked.Code, blocked.Body.String())
	}
	ancestorDependency := dependentProposal
	ancestorDependency.ProposalID = "ancestor-dependency"
	ancestorDependency.DependencyTaskIDs = []string{rootReceipt.ResourceID}
	if invalid := reviewRequest(h, http.MethodPost, path, runnerOne, "", ancestorDependency); invalid.Code != http.StatusBadRequest {
		t.Fatalf("ancestor dependency was not rejected: %d %s", invalid.Code, invalid.Body.String())
	}
	if _, err := store.DB().Exec(`UPDATE tasks SET status='complete' WHERE id=?`, rootReceipt.ResourceID); err != nil {
		t.Fatal(err)
	}
	ownerAfterStop := reviewRequest(h, http.MethodPost, path, runnerOne, "", proposal)
	if ownerAfterStop.Code != http.StatusOK {
		t.Fatalf("authorized owner could not recover exact receipt after parent termination: %d %s", ownerAfterStop.Code, ownerAfterStop.Body.String())
	}
}

func TestOutOfPolicyChildRequiresExpectedVersionApprovalAndCannotReviveParent(t *testing.T) {
	bootstrapCode, deviceToken, runnerToken := reviewToken(t), reviewToken(t), reviewToken(t)
	policy := control.ChildPolicy{AllowedProfileIDs: []string{"worker"}, CapabilityIDs: []string{"task.read"}, MaxChildRuns: 2, MaxDepth: 2}
	worker := control.RuntimeProfile{ID: "worker", RuntimeRouteID: "native", ModelRouteID: "model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1", CapabilityIDs: []string{"task.read"}, ChildPolicy: policy}
	admin := control.RuntimeProfile{ID: "admin-worker", RuntimeRouteID: "native", ModelRouteID: "model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1", CapabilityIDs: []string{"admin"}}
	store, h := reviewServer(t, control.Config{DefaultProfileID: "worker", Profiles: map[string]control.RuntimeProfile{"worker": worker, "admin-worker": admin}}, func(s *control.Server) {
		if err := s.RegisterRunnerCredential(context.Background(), runnerToken, "runner", []string{"runner:claim", "runner:propose"}); err != nil {
			t.Fatal(err)
		}
	})
	reviewInitializeInstance(t, h, bootstrapCode, deviceToken)
	created := reviewRequest(h, http.MethodPost, "/v1/commands", deviceToken, "root-create", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"root","goal":"root goal","source":"user"}`)})
	var root contracts.CommandReceipt
	if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &root) != nil {
		t.Fatalf("root create=%d %s", created.Code, created.Body.String())
	}
	claimed := reviewRequest(h, http.MethodPost, "/runner/v1/claims", runnerToken, "", nil)
	var claim contracts.JobClaim
	if claimed.Code != http.StatusOK || json.Unmarshal(claimed.Body.Bytes(), &claim) != nil {
		t.Fatalf("root claim=%d %s", claimed.Code, claimed.Body.String())
	}
	proposal := contracts.ChildTaskProposalRequest{ProposalID: "needs-approval", Attempt: claim.AttemptID, Fence: claim.Fence, Kind: "child_task", Title: "admin child", Goal: "admin goal", ProfileID: "admin-worker", RequestedCapabilityIDs: []string{"admin"}, Budget: contracts.ChildTaskBudget{MaxChildRuns: 1, MaxDepth: 1}}
	path := "/runner/v1/runs/" + claim.RunID + "/proposals"
	pending := reviewRequest(h, http.MethodPost, path, runnerToken, "", proposal)
	var pendingReceipt contracts.ChildTaskProposalReceipt
	if pending.Code != http.StatusOK || json.Unmarshal(pending.Body.Bytes(), &pendingReceipt) != nil || pendingReceipt.Status != "pending" || pendingReceipt.Version != 1 {
		t.Fatalf("out-of-policy proposal was not pending: %d %s", pending.Code, pending.Body.String())
	}
	missingVersion := reviewRequest(h, http.MethodPost, "/v1/commands", deviceToken, "approve-child-missing-version", contracts.CommandRequest{Kind: "child.approve", Payload: json.RawMessage(`{"proposal_id":"needs-approval"}`)})
	if missingVersion.Code != http.StatusBadRequest {
		t.Fatalf("child approval accepted without expected_version: %d %s", missingVersion.Code, missingVersion.Body.String())
	}
	approval := reviewRequest(h, http.MethodPost, "/v1/commands", deviceToken, "approve-child", contracts.CommandRequest{Kind: "child.approve", ExpectedVersion: ptrInt64(1), Payload: json.RawMessage(`{"proposal_id":"needs-approval"}`)})
	if approval.Code != http.StatusOK {
		t.Fatalf("approve child=%d %s", approval.Code, approval.Body.String())
	}
	var taskID, pregrant string
	if err := store.DB().QueryRow(`SELECT id,pregrant_capabilities_json FROM tasks WHERE lineage_parent_task_id=? ORDER BY created_at DESC LIMIT 1`, root.ResourceID).Scan(&taskID, &pregrant); err != nil {
		t.Fatal(err)
	}
	if pregrant != `["admin"]` {
		t.Fatalf("explicitly approved child grant was lost: %s", pregrant)
	}
	proposal.ProposalID = "pending-at-shutdown"
	pendingAfterApproval := reviewRequest(h, http.MethodPost, path, runnerToken, "", proposal)
	if pendingAfterApproval.Code != http.StatusOK {
		t.Fatalf("second out-of-policy proposal=%d %s", pendingAfterApproval.Code, pendingAfterApproval.Body.String())
	}
	if _, err := store.DB().Exec(`UPDATE tasks SET status='complete' WHERE id=?`, root.ResourceID); err != nil {
		t.Fatal(err)
	}
	tooLate := reviewRequest(h, http.MethodPost, "/v1/commands", deviceToken, "approve-after-terminal", contracts.CommandRequest{Kind: "child.approve", ExpectedVersion: ptrInt64(1), Payload: json.RawMessage(`{"proposal_id":"pending-at-shutdown"}`)})
	if tooLate.Code != http.StatusConflict {
		t.Fatalf("terminal parent approval did not fail: %d %s", tooLate.Code, tooLate.Body.String())
	}
}

func TestConcurrentChildProposalsCannotOverspendRootBudget(t *testing.T) {
	bootstrapCode, deviceToken, runnerToken := reviewToken(t), reviewToken(t), reviewToken(t)
	policy := control.ChildPolicy{AllowedProfileIDs: []string{"worker"}, CapabilityIDs: []string{"task.read"}, MaxChildRuns: 1, MaxDepth: 2}
	profile := control.RuntimeProfile{ID: "worker", RuntimeRouteID: "native", ModelRouteID: "model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1", CapabilityIDs: []string{"task.read"}, ChildPolicy: policy}
	store, h := reviewServer(t, control.Config{DefaultProfileID: "worker", Profiles: map[string]control.RuntimeProfile{"worker": profile}}, func(s *control.Server) {
		if err := s.RegisterRunnerCredential(context.Background(), runnerToken, "runner", []string{"runner:claim", "runner:propose"}); err != nil {
			t.Fatal(err)
		}
	})
	reviewInitializeInstance(t, h, bootstrapCode, deviceToken)
	created := reviewRequest(h, http.MethodPost, "/v1/commands", deviceToken, "root-create", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"root","goal":"root goal","source":"user"}`)})
	var root contracts.CommandReceipt
	if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &root) != nil {
		t.Fatalf("root create=%d %s", created.Code, created.Body.String())
	}
	claimed := reviewRequest(h, http.MethodPost, "/runner/v1/claims", runnerToken, "", nil)
	var claim contracts.JobClaim
	if claimed.Code != http.StatusOK || json.Unmarshal(claimed.Body.Bytes(), &claim) != nil {
		t.Fatalf("root claim=%d %s", claimed.Code, claimed.Body.String())
	}
	path := "/runner/v1/runs/" + claim.RunID + "/proposals"
	statusCodes := make(chan int, 2)
	var wait sync.WaitGroup
	for _, id := range []string{"race-a", "race-b"} {
		wait.Add(1)
		go func(proposalID string) {
			defer wait.Done()
			proposal := contracts.ChildTaskProposalRequest{ProposalID: proposalID, Attempt: claim.AttemptID, Fence: claim.Fence, Kind: "child_task", Title: proposalID, Goal: "goal", ProfileID: "worker", RequestedCapabilityIDs: []string{"task.read"}, Budget: contracts.ChildTaskBudget{MaxChildRuns: 1, MaxDepth: 2}}
			statusCodes <- reviewRequest(h, http.MethodPost, path, runnerToken, "", proposal).Code
		}(id)
	}
	wait.Wait()
	close(statusCodes)
	var admitted, exhausted int
	for code := range statusCodes {
		if code == http.StatusOK {
			admitted++
		} else if code == http.StatusConflict {
			exhausted++
		} else {
			t.Fatalf("unexpected concurrent proposal status=%d", code)
		}
	}
	var children, reservations int
	if err := store.DB().QueryRow(`SELECT count(*) FROM tasks WHERE lineage_parent_task_id=?`, root.ResourceID).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM child_budget_reservations WHERE scope_task_id=?`, root.ResourceID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if admitted != 1 || exhausted != 1 || children != 1 || reservations != 1 {
		t.Fatalf("concurrent admission overspent budget: admitted=%d exhausted=%d children=%d reservations=%d", admitted, exhausted, children, reservations)
	}
}

func ptrInt64(value int64) *int64 { return &value }
