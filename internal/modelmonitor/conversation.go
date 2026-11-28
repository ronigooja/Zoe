package modelmonitor

import (
	"encoding/json"
	"unicode/utf8"
)

// RequestMessage is the redacted text of an actual request message. An event
// may include these only when they describe a complete request snapshot.
type RequestMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// RequestMessagesWithinBudget checks the combined payload and structured
// representation. Optional messages never displace the existing payload.
func RequestMessagesWithinBudget(payloadBytes, limit int, messages []RequestMessage) bool {
	left := limit - payloadBytes - len(`,"request_messages":`)
	if len(messages) == 0 || left <= 0 || len(messages) > left {
		return false
	}
	for _, m := range messages {
		switch m.Role {
		case "system", "developer", "user", "assistant", "tool", "function", "tool_call", "tool_result":
		default:
			return false
		}
		if len(m.Text) > left || !utf8.ValidString(m.Text) {
			return false
		}
		left -= len(m.Text)
	}
	b, err := json.Marshal(messages)
	return err == nil && payloadBytes+len(`,"request_messages":`)+len(b) <= limit
}

func boundConversation(e *Event) {
	if len(e.ConversationID) > 128 || !utf8.ValidString(e.ConversationID) {
		e.ConversationID = ""
	}
	if e.Type != "request" {
		e.ConversationStart = false
	}
	if e.Type != "request" || e.Truncated || e.Incomplete || !RequestMessagesWithinBudget(len(e.Payload), maxPayload, e.RequestMessages) {
		e.RequestMessages = nil
	} else {
		e.RequestMessages = append([]RequestMessage(nil), e.RequestMessages...)
	}
}
