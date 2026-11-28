package memoryapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Observation is a worker-produced, allowlisted model event.
type Observation struct {
	Component         string          `json:"component"`
	CallID            string          `json:"call_id"`
	Time              time.Time       `json:"time"`
	Type              string          `json:"type"`
	Payload           json.RawMessage `json:"payload"`
	Truncated         bool            `json:"truncated"`
	Incomplete        bool            `json:"incomplete"`
	ConversationStart bool            `json:"conversation_start"`
}

// DrainObservations renews a short-lived worker lease. The lease must be a
// fresh random 32-character lowercase hexadecimal subscription generation.
// Unlike call, this path never logs its request or response.
func (c *Client) DrainObservations(ctx context.Context, lease string) ([]Observation, bool, error) {
	if c == nil {
		return nil, false, &Error{"disabled"}
	}
	token, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return nil, false, &Error{"credential"}
	}
	secret := strings.TrimSpace(string(token))
	if secret == "" || strings.ContainsAny(secret, "\r\n") {
		return nil, false, &Error{"credential"}
	}
	body, err := json.Marshal(map[string]string{"lease": lease})
	if err != nil {
		return nil, false, &Error{"request_invalid"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/memory/observe", bytes.NewReader(body))
	if err != nil {
		return nil, false, &Error{"configuration"}
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, &Error{"transport"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, &Error{"http_status"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return nil, false, &Error{"response_read"}
	}
	var result struct {
		Events   []Observation `json:"events"`
		Overflow bool          `json:"overflow"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Events) > 64 {
		return nil, false, &Error{"response_invalid"}
	}
	return result.Events, result.Overflow, nil
}
