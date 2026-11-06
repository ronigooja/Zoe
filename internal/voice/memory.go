package voice

import (
	"context"
	"strings"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/answer"
)

const memoryNotFound = "I do not have that saved."
const memoryUnavailable = "I could not check right now."
const memorySaveUnavailable = "I could not save that right now."

type MemoryAnswerer interface {
	AnswerMemory(context.Context, string, []contracts.MemoryContextItem) (answer.Result, error)
}

func (s *Service) memoryAnswer(ctx context.Context, workspace, owner, question string) (string, int, string) {
	fail := func(n int) (string, int, string) { return recallPrefix + memoryUnavailable, n, "unavailable" }
	if s.contextAssembler == nil {
		return fail(0)
	}
	p := contracts.Principal{WorkspaceID: workspace, SubjectID: owner}
	memory, err := s.contextAssembler(ctx, p, "", owner, question, contracts.MemoryContextDefaultBudgetTokens)
	if err != nil {
		return fail(0)
	}
	n := len(memory.Items)
	if n == 0 {
		return recallPrefix + memoryNotFound, 0, "not_found"
	}
	model, ok := s.recall.Answerer.(MemoryAnswerer)
	if !ok {
		return fail(n)
	}
	r, err := model.AnswerMemory(ctx, question, memory.Items)
	if err != nil {
		return fail(n)
	}
	if r.Status == "not_found" && r.Text == "" && len(r.SummaryIDs) == 0 {
		return recallPrefix + memoryNotFound, n, "not_found"
	}
	if r.Status != "answer" || strings.TrimSpace(r.Text) == "" || len(r.SummaryIDs) != 0 || strings.ContainsAny(r.Text, "*#`[]<>_~|") {
		return fail(n)
	}
	return recallPrefix + boundedResultText(r.Text), n, "answer"
}
