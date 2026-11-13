package voice

import (
	"context"
	"errors"
	"strings"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/answer"
)

const memoryNotFound = "我还没有保存这条信息。"
const memoryUnavailable = "我现在没法查看保存的笔记。"
const memorySaveUnavailable = "我现在没法保存这条信息。"

type MemoryAnswerer interface {
	AnswerMemory(context.Context, string, []contracts.MemoryContextItem) (answer.Result, error)
}

func (s *Service) memoryAnswer(ctx context.Context, workspace, owner, question string) (string, int, string) {
	fail := func(n int, status string) (string, int, string) {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = "timeout"
		}
		return evidenceFailureSpeech(status, memoryNotFound, memoryUnavailable), n, status
	}
	if s.contextAssembler == nil {
		return fail(0, "unconfigured")
	}
	p := contracts.Principal{WorkspaceID: workspace, SubjectID: owner, Scopes: []string{"memory:read:personal"}}
	memory, err := s.contextAssembler(ctx, p, "", owner, question, contracts.MemoryContextDefaultBudgetTokens)
	if err != nil || ctx.Err() != nil {
		return fail(0, "context_error")
	}
	n := len(memory.Items)
	if n == 0 {
		return fail(0, "not_found")
	}
	model, ok := s.recall.Answerer.(MemoryAnswerer)
	if !ok {
		return fail(n, "unconfigured")
	}
	r, err := model.AnswerMemory(ctx, question, memory.Items)
	if err != nil || ctx.Err() != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fail(n, "timeout")
		}
		return fail(n, "model_error")
	}
	if r.Status == "not_found" && r.Text == "" {
		return fail(n, "not_found")
	}
	if r.Status != "answer" || strings.TrimSpace(r.Text) == "" || strings.ContainsAny(r.Text, "*#`[]<>_~|") {
		return fail(n, "invalid_answer")
	}
	return "保存的笔记里记着：" + r.Text, n, "answer"
}
