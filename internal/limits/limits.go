// Package limits loads restart-only Zoe policy. Sets expose values, never mutable storage.
package limits

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const DefaultPath = "/etc/zoe/limits.json"
const MaxFileBytes = 64 << 10

// These widths also apply at the voice gateway wire boundary.
const (
	GatewayOpeningItemBytes     = 32 << 10
	GatewayOpeningItems         = 128
	GatewayTranscriptTurnBytes  = 64 << 10
	GatewayTranscriptBatchBytes = 512 << 10
	GatewayTranscriptTurns      = 200
	GatewayDelegationIDRunes    = 256
	GatewayTextChunkBytes       = 8000
	CallStartJSONOverhead       = 16_384
)

//go:embed schema.json
var schemaJSON []byte

var schema = func() map[string][3]int64 {
	var m map[string][3]int64
	if err := json.Unmarshal(schemaJSON, &m); err != nil {
		panic(err)
	}
	return m
}()

var keys = func() []string {
	k := make([]string, 0, len(schema))
	for key := range schema {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}()

type Set struct{ values map[string]int64 }

// Defaults is the immutable code-default snapshot, also represented in the release file.
func Defaults() Set { return Set{} }

// Resolve supplies defaults for helpers called without an explicit snapshot.
func Resolve(sets []Set) Set {
	if len(sets) == 0 {
		return Defaults()
	}
	return sets[0]
}

func (s Set) Int64(key string) int64 {
	if n, ok := s.values[key]; ok {
		return n
	}
	if entry, ok := schema[key]; ok {
		return entry[0]
	}
	panic("unknown limit key: " + key)
}

func (s Set) Int(key string) int { return int(s.Int64(key)) }

// With returns a validated replacement, useful for constructing test snapshots.
func (s Set) With(key string, value int64) (Set, error) {
	m := make(map[string]int64, len(keys))
	for _, k := range keys {
		m[k] = s.Int64(k)
	}
	m[key] = value
	return validate(m)
}

func Path() string {
	if path := os.Getenv("ZOE_LIMITS_FILE"); path != "" {
		return path
	}
	return DefaultPath
}

// Error contains fixed categories and known option names only, never file content.
type Error string

func (e Error) Error() string { return string(e) }

func RejectLegacyEnv(lookup func(string) (string, bool)) error {
	for _, entry := range [...]struct{ env, key string }{
		{"ZOE_COMPACT_MAX_CHARACTERS", "compact_max_characters"},
		{"ZOE_MAX_RUNS", "max_concurrent_runs"},
		{"ZOE_UI_CONTROLLER_MAX_RETRIES", "ui_output_attempts"},
		{"ZOE_AGENT_CONTEXT_TOKEN_BUDGET", "runner_context_max_tokens"},
		{"ZOE_AGENT_MAX_ROTATIONS", "runner_max_rotations"},
	} {
		if _, ok := lookup(entry.env); ok {
			return Error(entry.env + " was removed; move to " + entry.key + " in ZOE_LIMITS_FILE")
		}
	}
	return nil
}

func Load(path string) (Set, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Set{}, Error("limits_missing")
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0444 == 0 {
		return Set{}, Error("limits_unreadable")
	}
	f, err := os.Open(path)
	if err != nil {
		return Set{}, Error("limits_unreadable")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return Set{}, Error("limits_unreadable")
	}
	return Decode(data)
}

func Decode(data []byte) (Set, error) {
	switch {
	case len(data) > MaxFileBytes:
		return Set{}, Error("limits_too_large")
	case !utf8.Valid(data):
		return Set{}, Error("limits_invalid_utf8")
	case len(bytes.TrimSpace(data)) == 0:
		return Set{}, Error("limits_empty")
	}
	malformed := Error("limits_invalid_json")
	d := json.NewDecoder(bytes.NewReader(data))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return Set{}, malformed
	}
	m := make(map[string]int64, len(schema))
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return Set{}, malformed
		}
		key := token.(string)
		if _, ok := schema[key]; !ok {
			return Set{}, Error("limits_unknown_key")
		}
		if _, ok := m[key]; ok {
			return Set{}, Error("limits_duplicate_key: " + key)
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return Set{}, malformed
		}
		n, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return Set{}, Error("limits_out_of_range: " + key)
			}
			return Set{}, Error("limits_integer_required: " + key)
		}
		m[key] = n
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return Set{}, malformed
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return Set{}, malformed
	}
	return validate(m)
}

func validate(m map[string]int64) (Set, error) {
	if len(m) > len(keys) {
		return Set{}, Error("limits_unknown_key")
	}
	for _, key := range keys {
		n, ok := m[key]
		if !ok {
			return Set{}, Error("limits_missing_key: " + key)
		}
		r := schema[key]
		if n < r[1] || n > r[2] {
			return Set{}, Error("limits_out_of_range: " + key)
		}
	}
	bad := func(names ...string) (Set, error) {
		return Set{}, Error("limits_constraint: " + strings.Join(names, ", "))
	}
	if m["opening_context_runes"]*utf8.UTFMax > GatewayOpeningItemBytes {
		return bad("opening_context_runes", "gateway_opening_item_bytes")
	}
	if m["voice_sdp_max_bytes"]*6+CallStartJSONOverhead > m["http_call_start_max_bytes"] {
		return bad("voice_sdp_max_bytes", "http_call_start_max_bytes")
	}
	if m["task_text_max_bytes"]*12+4096 > m["http_request_max_bytes"] {
		return bad("task_text_max_bytes", "http_request_max_bytes")
	}
	if m["voice_text_max_bytes"]*6+4096 > m["http_request_max_bytes"] {
		return bad("voice_text_max_bytes", "http_request_max_bytes")
	}
	if m["runner_assistant_output_max_bytes"]*6+4096 > m["http_request_max_bytes"] {
		return bad("runner_assistant_output_max_bytes", "http_request_max_bytes")
	}
	if m["http_request_max_bytes"] < GatewayTranscriptBatchBytes {
		return bad("http_request_max_bytes", "gateway_transcript_batch_bytes")
	}
	for _, pair := range [...][2]string{
		{"memory_ingest_chunk_bytes", "memory_ingest_body_bytes"},
		{"memory_search_default", "memory_search_max"},
		{"runner_result_evidence_max_bytes", "http_request_max_bytes"},
		{"task_entries_page_default", "task_entries_page_max"},
		{"steward_candidate_default", "steward_candidate_max"},
		{"diagnostic_max_batch", "diagnostic_events_per_device"},
		{"diagnostic_event_max_bytes", "diagnostic_body_max_bytes"},
		{"entry_receipt_max_bytes", "http_request_max_bytes"},
		{"instance_fence_max_bytes", "http_request_max_bytes"},
		{"voice_lifecycle_max_bytes", "http_request_max_bytes"},
		{"diagnostic_body_max_bytes", "http_request_max_bytes"},
		{"capability_manifest_max_bytes", "http_request_max_bytes"},
		{"ui_feedback_max_bytes", "ui_output_max_bytes"},
		{"ui_output_max_bytes", "model_api_response_max_bytes"},
	} {
		if m[pair[0]] > m[pair[1]] {
			return bad(pair[0], pair[1])
		}
	}
	return Set{values: m}, nil
}
