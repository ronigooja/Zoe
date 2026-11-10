package voice

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/answer"
)

func assertPlainChineseRecall(t *testing.T, text string) {
	t.Helper()
	if text == "" || strings.Contains(text, "Read this to the user") {
		t.Fatalf("not plain speech: %q", text)
	}
	for _, r := range text {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			t.Fatalf("English sentence or prefix in speech: %q", text)
		}
	}
}

func TestRecallChineseOutputSentences(t *testing.T) {
	for _, status := range []string{"no_summaries", "not_found", "unconfigured", "timezone", "summary_error", "model_error", "invalid_request", "timeout", "transcript_error"} {
		t.Run(status, func(t *testing.T) { assertPlainChineseRecall(t, recallFailureSpeech(status)) })
	}
	for _, text := range []string{"我们聊过花园。", "", "**花园**", "selected-summary", "12345678-1234-1234-1234-123456789abc"} {
		assertPlainChineseRecall(t, recallSpeech(text, []answer.Candidate{{ID: "selected-summary"}}))
	}
	_, store, _ := handoffHTTPServerWithConfig(t, Config{})
	_, err := store.DB().Exec(`INSERT INTO voice_call_summaries(id,workspace_id,session_id,topic,started_at,ended_at,first_seq,last_seq,summary,created_at) VALUES('chinese-summary','default','handoff-session','花园','2026-11-05T12:00:00Z','2026-11-05T12:00:00Z',1,1,'我们聊过花园。','2026-11-05T12:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	svc := newService(store.DB(), Config{Answerer: recallModelFunc(func(context.Context, string, []answer.Candidate, []answer.Excerpt) (answer.Result, error) {
		return answer.Result{Status: "answer", Text: "我们聊过花园。"}, nil
	})})
	t.Cleanup(svc.Stop)
	text, count, status := svc.recallAnswer(context.Background(), "default", "owner", "我们聊过什么？")
	if status != "answer" || count != 1 || text != "对话记录里记着：我们聊过花园。" {
		t.Fatalf("status=%s count=%d text=%q", status, count, text)
	}
	assertPlainChineseRecall(t, text)
}

func TestMemoryChineseOutputSentences(t *testing.T) {
	for _, mode := range []string{"answer", "not_found", "empty", "context_error", "model_error", "invalid_answer", "unconfigured", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			cfg := Config{ContextAssembler: func(context.Context, contracts.Principal, string, string, string, int) (contracts.MemoryContext, error) {
				if mode == "context_error" {
					return contracts.MemoryContext{}, errors.New("unavailable")
				}
				if mode == "empty" {
					return contracts.MemoryContext{}, nil
				}
				return contracts.MemoryContext{Items: []contracts.MemoryContextItem{{Content: "喜欢美式咖啡"}}}, nil
			}, Answerer: memoryModel{fn: func(context.Context, string, []contracts.MemoryContextItem) (answer.Result, error) {
				switch mode {
				case "not_found":
					return answer.Result{Status: "not_found"}, nil
				case "model_error":
					return answer.Result{}, errors.New("unavailable")
				case "timeout":
					return answer.Result{}, context.DeadlineExceeded
				case "invalid_answer":
					return answer.Result{Status: "answer", Text: "**咖啡**"}, nil
				}
				return answer.Result{Status: "answer", Text: "你喜欢美式咖啡。"}, nil
			}}}
			if mode == "unconfigured" {
				cfg.ContextAssembler = nil
			}
			svc := newService(nil, cfg)
			t.Cleanup(svc.Stop)
			text, _, status := svc.memoryAnswer(context.Background(), "default", "owner", "我喜欢什么饮品？")
			want := "我现在没法查看保存的笔记。"
			if mode == "empty" || mode == "not_found" {
				want = "我还没有保存这条信息。"
			}
			if mode == "answer" {
				want = "保存的笔记里记着：你喜欢美式咖啡。"
			}
			if text != want {
				t.Fatalf("status=%s text=%q, want %q", status, text, want)
			}
			assertPlainChineseRecall(t, text)
		})
	}
	assertPlainChineseRecall(t, evidenceFailureSpeech("unavailable", memoryNotFound, memorySaveUnavailable))
}
