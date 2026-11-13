package voice

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/answer"
)

type memoryModel struct {
	Answerer
	fn func(context.Context, string, []contracts.MemoryContextItem) (answer.Result, error)
}

func (m memoryModel) AnswerMemory(ctx context.Context, q string, items []contracts.MemoryContextItem) (answer.Result, error) {
	return m.fn(ctx, q, items)
}

func TestMemoryAnswerEvidenceFailures(t *testing.T) {
	for _, mode := range []string{"answer", "full_answer", "empty", "not_found", "model_error", "invalid_answer", "context_error", "unconfigured"} {
		t.Run(mode, func(t *testing.T) {
			cfg := Config{}
			cfg.ContextAssembler = func(_ context.Context, p contracts.Principal, task, owner, query string, budget int) (contracts.MemoryContext, error) {
				if p.WorkspaceID != "default" || p.SubjectID != "owner" || owner != "owner" || task != "" || query != "question" {
					t.Fatal("incorrect memory scope")
				}
				if mode == "empty" {
					return contracts.MemoryContext{}, nil
				}
				if mode == "context_error" {
					return contracts.MemoryContext{}, errors.New("private detail")
				}
				return contracts.MemoryContext{Items: []contracts.MemoryContextItem{{Subject: "owner", Key: "drink", Content: "coffee"}}}, nil
			}
			cfg.Answerer = memoryModel{fn: func(context.Context, string, []contracts.MemoryContextItem) (answer.Result, error) {
				switch mode {
				case "not_found":
					return answer.Result{Status: "not_found"}, nil
				case "model_error":
					return answer.Result{}, errors.New("private detail")
				case "invalid_answer":
					return answer.Result{Status: "need_transcript"}, nil
				case "full_answer":
					return answer.Result{Status: "answer", Text: " \n" + strings.Repeat("coffee 🙂\n\t", 2000) + "\x00 "}, nil
				}
				return answer.Result{Status: "answer", Text: "你喜欢咖啡。"}, nil
			}}
			if mode == "unconfigured" {
				cfg.Answerer = nil
			}
			_, svc := newVoiceTestService(t, cfg)
			text, _, _ := svc.memoryAnswer(context.Background(), "default", "owner", "question")
			// Legacy assemblers/answerers must never bypass the worker boundary.
			want := stewardErrorSpeech
			if text != want {
				t.Fatalf("text=%q want=%q", text, want)
			}
		})
	}
}
