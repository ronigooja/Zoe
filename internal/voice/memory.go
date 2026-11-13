package voice

import (
	"context"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/answer"
)

const memoryNotFound = "我还没有保存这条信息。"
const memoryUnavailable = "我现在没法查看保存的笔记。"
const memorySaveUnavailable = "我现在没法保存这条信息。"

// MemoryAnswerer remains for compatibility until the answer-model cleanup.
type MemoryAnswerer interface {
	AnswerMemory(context.Context, string, []contracts.MemoryContextItem) (answer.Result, error)
}

func (s *Service) memoryAnswer(ctx context.Context, workspace, owner, question string) (string, int, string) {
	return s.recallAnswer(ctx, workspace, owner, question)
}
