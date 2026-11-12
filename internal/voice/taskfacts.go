package voice

import (
	"context"

	"github.com/ronigooja/Zoe/internal/progressreply"
)

type taskFact struct {
	Request, Status, CreatedAt, FinishedAt, Question, Result string
	Additions                                                []string
	MemoryText                                               string
}

// taskFacts reads exact original requests across all calls for this owner.
// Each distinct request contributes at most ten tasks, newest first.
func taskFacts(ctx context.Context, q rosterQueryer, workspace, owner string, requests []string) ([]taskFact, error) {
	facts := []taskFact{}
	seen := map[string]bool{}
	for _, request := range requests {
		if seen[request] {
			continue
		}
		seen[request] = true
		rows, err := q.QueryContext(ctx, `SELECT t.id,t.title,t.status,coalesce(t.created_at,''),coalesce(t.finished_at,''),
CASE WHEN t.status IN ('waiting','awaiting_user') AND t.interaction_state IN ('awaiting_message','explicit_confirmation') THEN coalesce(t.interaction_question,'') ELSE '' END,
coalesce(t.result,'') FROM tasks t WHERE t.workspace_id=? AND t.owner_subject_id=? AND t.title=? COLLATE BINARY
ORDER BY julianday(t.created_at) DESC,t.created_at DESC,t.id DESC LIMIT 10`, workspace, owner, request)
		if err != nil {
			return nil, err
		}
		// Close the task cursor before reading entries so single-connection stores work.
		var ids []string
		start := len(facts)
		for rows.Next() {
			var id string
			var fact taskFact
			if err := rows.Scan(&id, &fact.Request, &fact.Status, &fact.CreatedAt, &fact.FinishedAt, &fact.Question, &fact.Result); err != nil {
				rows.Close()
				return nil, err
			}
			fact.Result = boundFactText(fact.Result, 2000)
			fact.Additions = []string{}
			ids = append(ids, id)
			facts = append(facts, fact)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		for i, id := range ids {
			rows, err := q.QueryContext(ctx, `SELECT coalesce(content,'') FROM task_entries
WHERE workspace_id=? AND task_id=? AND kind='user_message' AND julianday(created_at)>julianday(?)
ORDER BY julianday(created_at) DESC,created_at DESC,id DESC LIMIT 5`, workspace, id, facts[start+i].CreatedAt)
			if err != nil {
				return nil, err
			}
			var additions []string
			for rows.Next() {
				var content string
				if err := rows.Scan(&content); err != nil {
					rows.Close()
					return nil, err
				}
				additions = append(additions, boundFactText(content, 500))
			}
			err = rows.Err()
			closeErr := rows.Close()
			if err != nil {
				return nil, err
			}
			if closeErr != nil {
				return nil, closeErr
			}
			for j := len(additions) - 1; j >= 0; j-- {
				facts[start+i].Additions = append(facts[start+i].Additions, additions[j])
			}
		}
	}
	return facts, nil
}

func boundFactText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return text
}

func toProgressFacts(tasks []taskFact) []progressreply.Fact {
	facts := make([]progressreply.Fact, len(tasks))
	for i, task := range tasks {
		facts[i] = progressreply.Fact{Request: task.Request, Status: task.Status, CreatedAt: task.CreatedAt, FinishedAt: task.FinishedAt, Question: task.Question, Result: task.Result, Additions: task.Additions, MemoryText: task.MemoryText}
	}
	return facts
}
