package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

const resultCapabilityRefresh = 30 * time.Second
const resultCapabilityTimeout = 2 * time.Second

// gatewayResultCapability applies only to auto-discovered receipt adapters. A
// single probe runs at a time; expired positive evidence never authorizes sends.
type gatewayResultCapability struct {
	mu        sync.Mutex
	next      time.Time
	running   bool
	supported bool
	adapter   ResultReceiptAdapter
}

// receiptAdapter runs before checkpoint transactions, once per delivery rather
// than per chunk. Explicit injected adapters remain an isolated test boundary.
func (s *Service) receiptAdapter(ctx context.Context) ResultReceiptAdapter {
	if s.resultReceiptAdapter != nil {
		return s.resultReceiptAdapter
	}
	c := s.resultCapability
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return nil
	}
	if s.now().Before(c.next) {
		supported := c.supported
		c.mu.Unlock()
		if supported {
			return c.adapter
		}
		return nil
	}
	c.running = true
	c.supported = false
	c.mu.Unlock()
	supported := s.probeResultCapability(ctx)
	c.mu.Lock()
	c.running = false
	c.supported = supported
	c.next = s.now().Add(resultCapabilityRefresh)
	c.mu.Unlock()
	if supported {
		return c.adapter
	}
	return nil
}

func (s *Service) probeResultCapability(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, resultCapabilityTimeout)
	defer cancel()
	resp, _, err := s.callGateway(ctx, http.MethodGet, "/v1/health", nil)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(data) > 65536 {
		return false
	}
	health, valid := uniqueCapabilityObject(data)
	if !valid {
		return false
	}
	capability, valid := uniqueCapabilityObject(health["result_deliveries"])
	if !valid || len(capability) != 3 {
		return false
	}
	var version, evidence string
	var maxBytes int
	return json.Unmarshal(capability["version"], &version) == nil && version == "v1" &&
		json.Unmarshal(capability["max_text_bytes"], &maxBytes) == nil && maxBytes == 8000 &&
		json.Unmarshal(capability["applied_evidence"], &evidence) == nil && evidence == "local_transport_writes_and_durable_receipt"
}

// uniqueCapabilityObject rejects ambiguous duplicate keys and trailing JSON at
// either capability boundary; unrelated health fields remain forward-compatible.
func uniqueCapabilityObject(data []byte) (map[string]json.RawMessage, bool) {
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, false
		}
		if _, exists := fields[key]; exists {
			return nil, false
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, false
		}
		fields[key] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, false
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, false
	}
	return fields, true
}
