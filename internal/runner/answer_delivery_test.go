package runner

import (
	"context"
	"errors"
	"github.com/ronigooja/Zoe/contracts"
	"testing"
)

type answerJournalFake struct {
	state     string
	failState string
}

func (j *answerJournalFake) AnswerState(context.Context, string, string, string, contracts.RunQuestionBinding) (string, error) {
	return j.state, nil
}
func (j *answerJournalFake) SetAnswerState(_ context.Context, _, _, _ string, _ contracts.RunQuestionBinding, state string) error {
	if j.failState == state {
		return errors.New("disk failure")
	}
	j.state = state
	return nil
}

func TestAnswerDeliveryDeterministicCrashBoundaries(t *testing.T) {
	for _, failure := range []string{"none", "before-send", "reply-unknown", "after-reply", "ack-response-lost"} {
		t.Run(failure, func(t *testing.T) {
			j := &answerJournalFake{}
			if failure == "before-send" {
				j.failState = "attempted"
			}
			if failure == "after-reply" {
				j.failState = "confirmed"
			}
			c := testClaim()
			b := RuntimeBinding{SessionID: "session"}
			entry := contracts.RunEntry{ID: "answer", Question: &contracts.RunQuestionBinding{QuestionID: "q", RunID: c.RunID, RequestID: "form", ActionDigest: "digest"}}
			sends, acks, inspects := 0, 0, 0
			send := func() error {
				sends++
				if j.state != "attempted" {
					t.Fatal("send without checkpoint")
				}
				if failure == "reply-unknown" {
					return errors.New("lost reply response")
				}
				return nil
			}
			inspect := func() (bool, error) { inspects++; return false, nil }
			ack := func() error {
				acks++
				if failure == "ack-response-lost" && acks == 1 {
					return errors.New("lost ack response")
				}
				return nil
			}
			first := deliverAnswer(context.Background(), j, c, b, entry, send, inspect, ack)
			if failure == "none" && first != nil {
				t.Fatal(first)
			}
			if failure != "none" && first == nil {
				t.Fatal("failure not observed")
			}
			j.failState = ""
			// A fresh invocation has only the durable state, never an in-memory success flag.
			second := deliverAnswer(context.Background(), j, c, b, entry, send, inspect, ack)
			if failure == "reply-unknown" || failure == "after-reply" {
				if !errors.Is(second, ErrOutcomePending) || sends != 1 || acks != 0 || inspects != 1 {
					t.Fatalf("unsafe replay/ack: sends=%d acks=%d inspections=%d err=%v", sends, acks, inspects, second)
				}
			} else {
				if second != nil || sends != 1 || j.state != "acknowledged" {
					t.Fatalf("sends=%d state=%s err=%v", sends, j.state, second)
				}
				previous := acks
				if err := deliverAnswer(context.Background(), j, c, b, entry, send, inspect, ack); err != nil || acks != previous || sends != 1 {
					t.Fatal("duplicate reply/ack")
				}
			}
		})
	}
}

func TestAnswerDeliveryPendingQuestionDoesNotPermitReplay(t *testing.T) {
	j := &answerJournalFake{state: "attempted"}
	c := testClaim()
	err := deliverAnswer(context.Background(), j, c, RuntimeBinding{SessionID: "session"}, contracts.RunEntry{ID: "answer", Question: &contracts.RunQuestionBinding{RunID: c.RunID}}, func() error { t.Fatal("blind replay"); return nil }, func() (bool, error) { return true, nil }, func() error { t.Fatal("unproven acknowledgement"); return nil })
	if !errors.Is(err, ErrOutcomePending) {
		t.Fatal(err)
	}
}
