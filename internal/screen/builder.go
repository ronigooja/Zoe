package screen

import (
	"fmt"
	"sort"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

// Build renders authoritative task and notification views. It never includes
// task.Error: detailed runner/provider failures remain server-side diagnostics.
func Build(tasks []contracts.Task, notifications []contracts.Notification, revision int64, now time.Time) (contracts.ScreenResponse, error) {
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	sort.SliceStable(notifications, func(i, j int) bool { return notifications[i].ID < notifications[j].ID })
	homeChildren := make([]contracts.ScreenNode, 0, 2)
	items := make([]any, 0, len(tasks))
	for _, task := range tasks {
		items = append(items, map[string]any{"id": task.ID, "title": task.Title, "status": task.Status})
	}
	homeChildren = append(homeChildren, contracts.ScreenNode{ID: "home.tasks", Type: contracts.NodeCollection, Props: map[string]any{"items": items, "layout": "list"}})
	status := "No active tasks"
	for _, task := range tasks {
		if task.Status != "complete" && task.Status != "canceled" && task.Status != "failed" {
			status = fmt.Sprintf("%d active task(s)", len(tasks))
			break
		}
	}
	homeChildren = append(homeChildren, contracts.ScreenNode{ID: "home.status", Type: contracts.NodeStatus, Props: map[string]any{"state": "info", "description": status}})
	for _, notice := range notifications {
		homeChildren = append(homeChildren, contracts.ScreenNode{ID: "home.notice." + notice.ID, Type: contracts.NodeNotice, Props: map[string]any{"text": notice.Title + ": " + notice.Body, "tone": "attention"}})
	}
	result := contracts.ScreenResponse{Revision: revision, Surfaces: []contracts.SurfaceSpec{{ID: "home", Kind: contracts.SurfaceWindow, Document: contracts.ScreenDocument{Version: contracts.ScreenLibraryVersion, Revision: revision, Root: contracts.ScreenNode{ID: "home.root", Type: contracts.NodeStack, Props: map[string]any{"axis": "vertical"}, Children: homeChildren}}}}}
	for _, task := range tasks {
		if task.Status == "awaiting_user" && task.InteractionState != "none" {
			expires := ""
			if task.InteractionExpiresAt != nil {
				expires = task.InteractionExpiresAt.UTC().Format(time.RFC3339Nano)
			}
			options := []any{map[string]any{"label": "Confirm", "intent": "task.confirm", "args": map[string]any{"task_id": task.ID, "run_id": task.InteractionRunID, "action_digest": task.InteractionActionDigest, "expected_version": task.InteractionExpectedVersion, "expiry": expires}, "consequence": "Submit for server validation"}}
			if task.InteractionState == "awaiting_message" {
				options = []any{map[string]any{"label": "Answer", "intent": "task.append", "args": map[string]any{"task_id": task.ID, "run_id": task.InteractionRunID, "action_digest": task.InteractionActionDigest, "expected_version": task.InteractionExpectedVersion, "expiry": expires}, "consequence": "Send an answer for server validation"}}
			}
			question := task.Title
			if task.InteractionQuestion != "" {
				question = task.InteractionQuestion
			}
			result.Surfaces = append(result.Surfaces, contracts.SurfaceSpec{ID: "pending_decision." + task.ID, Kind: contracts.SurfacePanel, Document: contracts.ScreenDocument{Version: contracts.ScreenLibraryVersion, Revision: revision, Root: contracts.ScreenNode{ID: "decision." + task.ID, Type: contracts.NodeDecision, Props: map[string]any{"question": question, "options": options}}}})
		}
		if task.Result != "" && task.Status == "complete" {
			result.Surfaces = append(result.Surfaces, contracts.SurfaceSpec{ID: "result." + task.ID, Kind: contracts.SurfaceWindow, Document: contracts.ScreenDocument{Version: contracts.ScreenLibraryVersion, Revision: revision, Root: contracts.ScreenNode{ID: "result." + task.ID, Type: contracts.NodeArticle, Props: map[string]any{"title": task.Title, "body": task.Result}}}})
		}
	}
	_ = now // kept in the pure builder API for deterministic expiry policies.
	for i := range result.Surfaces {
		if err := contracts.ValidateKnownScreenDocument(result.Surfaces[i].Document); err != nil {
			return contracts.ScreenResponse{}, err
		}
	}
	return result, nil
}
