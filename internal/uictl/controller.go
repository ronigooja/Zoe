// Package uictl defines the stateless UI controller boundary.
package uictl

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// Version is server-owned for every stored and emitted A2UI message.
const Version = "v0.9.1"
const CatalogID = "https://a2ui.org/specification/v0_9/basic_catalog.json"
const MaxOutput = 64 << 10
const MaxState = 256 << 10

type Size struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}
type Input struct {
	Now           string         `json:"now"`
	Requests      []string       `json:"requests"`
	Task          *Task          `json:"task"`
	Screen        *Size          `json:"screen,omitempty"`
	PreviousError *PreviousError `json:"previous_error,omitempty"`
}

type PreviousError struct {
	Category string `json:"category"`
	Field    string `json:"field,omitempty"`
}

type Task struct {
	TaskID     string  `json:"task_id"`
	Status     string  `json:"status"`
	Result     *string `json:"result"`
	QuestionID string  `json:"question_id"`
	Question   string  `json:"question"`
}

func BuildInput(now time.Time, requests []string, task *Task, screen *Size) ([]byte, error) {
	if requests == nil {
		requests = []string{}
	}
	for _, request := range requests {
		if strings.TrimSpace(request) == "" || strings.TrimSpace(request) == "none" {
			return nil, errors.New("request_required")
		}
	}
	b, err := json.Marshal(Input{Now: now.UTC().Format(time.RFC3339), Requests: requests, Task: task, Screen: screen})
	if len(b) > 16*MaxState {
		return nil, errors.New("input_limit")
	}
	return b, err
}

type Output struct {
	Messages  []json.RawMessage `json:"messages"`
	Container string            `json:"container"`
	Feedback  string            `json:"feedback"`
	SurfaceID string            `json:"-"`
	Size      *Size             `json:"size,omitempty"`
}

// object rejects duplicate fields at every nesting level before typed decoding.
func strictValue(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return errors.New("duplicate_field")
				}
				seen[s] = true
				if e = strictValue(d); e != nil {
					return e
				}
			}
			t, err = d.Token()
			if t != json.Delim('}') {
				return errors.New("invalid_object")
			}
		case '[':
			for d.More() {
				if err = strictValue(d); err != nil {
					return err
				}
			}
			t, err = d.Token()
			if t != json.Delim(']') {
				return errors.New("invalid_array")
			}
		default:
			return errors.New("invalid_json")
		}
	}
	return err
}
func decodeObject(b []byte, allowed ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	if err := strictValue(d); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing_data")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, errors.New("object_required")
	}
	for k := range m {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
			}
		}
		if !found {
			return nil, errors.New("unknown_field")
		}
	}
	return m, nil
}
func stringField(m map[string]json.RawMessage, k string) (string, error) {
	var s string
	b, ok := m[k]
	if !ok || len(b) == 0 || b[0] != '"' || json.Unmarshal(b, &s) != nil {
		return "", validationField("string_required", k)
	}
	return s, nil
}

// stripFence removes one enclosing Markdown code fence: an opening line of three
// backticks with an optional language tag and a closing line of three backticks.
// It is a container format, so the enclosed text is parsed by the strict rules.
func stripFence(text string) string {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "```") || !strings.HasSuffix(t, "```") {
		return text
	}
	nl := strings.IndexByte(t, '\n')
	if nl < 0 {
		return text
	}
	inner := t[nl+1 : len(t)-3]
	if strings.Contains(inner, "```") {
		return text
	}
	return strings.TrimSpace(inner)
}

// extractBlocks recognizes only the two protocol tags; diagnostics never contain model text.
func extractBlocks(text string) (string, string, error) {
	tags := []string{"<ui-meta>", "</ui-meta>", "<a2ui-json>", "</a2ui-json>"}
	positions := make([]int, len(tags))
	for i, tag := range tags {
		n := strings.Count(text, tag)
		if n > 1 {
			return "", "", errors.New("block_duplicate")
		}
		if n == 0 {
			if strings.Contains(text, tags[i^1]) {
				return "", "", errors.New("block_unclosed")
			}
			return "", "", errors.New("block_missing")
		}
		positions[i] = strings.Index(text, tag)
	}
	if positions[0] > positions[1] || positions[2] > positions[3] {
		return "", "", errors.New("block_unclosed")
	}
	if positions[1] > positions[2] {
		return "", "", errors.New("block_order")
	}
	return text[positions[0]+len(tags[0]) : positions[1]], text[positions[2]+len(tags[2]) : positions[3]], nil
}

// Parse permits exactly one target surface per invocation; multi-surface changes
// require separate jobs so every presentation has one version and render outcome.
type Mode string

const (
	Show         Mode = "show"
	TaskQuestion Mode = "task_question"
	ChatInput    Mode = "chat_input"
)

func Parse(text string, screen *Size, mode Mode) (Output, error) {
	var o Output
	if mode != Show && mode != TaskQuestion && mode != ChatInput {
		return o, errors.New("mode_invalid")
	}
	if len(text) > MaxOutput || !utf8.ValidString(text) {
		return o, errors.New("output_limit_or_utf8")
	}
	text = stripFence(text)
	meta, messages, err := extractBlocks(text)
	if err != nil {
		return o, err
	}
	m, err := decodeObject([]byte(meta), "container", "feedback", "size")
	if err != nil {
		return o, err
	}
	for _, key := range []string{"container", "size"} {
		if _, ok := m[key]; !ok {
			return o, validationField("missing_field", key)
		}
	}
	if raw, ok := m["size"]; ok {
		size, e := decodeObject(raw, "width", "height")
		var dimensions Size
		if e != nil || len(size) != 2 || string(size["width"]) == "null" || string(size["height"]) == "null" || json.Unmarshal(raw, &dimensions) != nil {
			return o, errors.New("size_invalid")
		}
	}
	if err = json.Unmarshal([]byte(meta), &o); err != nil {
		return o, err
	}
	keys := []string{"container"}
	if mode == Show || m["feedback"] != nil {
		keys = append(keys, "feedback")
	}
	for _, k := range keys {
		if _, err = stringField(m, k); err != nil {
			return o, err
		}
	}
	if o.Container != "popover" && o.Container != "window" {
		return o, errors.New("container_invalid")
	}
	if o.Size != nil {
		minW, maxW, minH, maxH := 320, 1200, 240, 1000
		if o.Container == "popover" {
			minW, maxW, minH, maxH = 280, 600, 200, 720
		}
		if o.Size.Width < minW || o.Size.Width > maxW || o.Size.Height < minH || o.Size.Height > maxH || (screen != nil && (o.Size.Width > screen.Width || o.Size.Height > screen.Height)) {
			return o, errors.New("size_invalid")
		}
	}
	if mode != Show {
		o.Feedback = ""
	} else {
		o.Feedback = strings.TrimSpace(o.Feedback)
		if o.Feedback == "" || len(o.Feedback) > 8000 {
			return o, errors.New("feedback_semantics")
		}
	}
	if err := json.Unmarshal([]byte(messages), &o.Messages); err != nil {
		return o, errors.New("invalid_array")
	}
	if len(o.Messages) == 0 || len(o.Messages) > 64 {
		return o, errors.New("message_limit")
	}
	for i, raw := range o.Messages {
		kind, id, err := validateMessage(raw)
		if err != nil {
			return o, err
		}
		if (i == 0) != (kind == "createSurface") {
			return o, errors.New("create_sequence")
		}
		if o.SurfaceID != "" && o.SurfaceID != id {
			return o, errors.New("mixed_surfaces")
		}
		o.SurfaceID = id
		o.Messages[i] = canonicalMessage(raw)
	}
	if mode != Show && !hasInputBinding(o.Messages) {
		return o, errors.New("input_binding_required")
	}
	return o, nil
}

// hasInputBinding requires one text field and an event context referencing its path.
func hasInputBinding(messages []json.RawMessage) bool {
	paths := map[string]bool{}
	var contexts []any
	for _, raw := range messages {
		var m struct {
			Update struct {
				Components []map[string]any `json:"components"`
			} `json:"updateComponents"`
		}
		_ = json.Unmarshal(raw, &m)
		for _, c := range m.Update.Components {
			if c["component"] == "TextField" {
				if value, ok := c["value"].(map[string]any); ok {
					if path, ok := value["path"].(string); ok && strings.HasPrefix(path, "/") {
						paths[path] = true
					}
				}
			}
			if c["component"] == "Button" {
				if action, ok := c["action"].(map[string]any); ok {
					if event, ok := action["event"].(map[string]any); ok {
						contexts = append(contexts, event["context"])
					}
				}
			}
		}
	}
	var references func(any) bool
	references = func(v any) bool {
		switch v := v.(type) {
		case map[string]any:
			if path, ok := v["path"].(string); ok && paths[path] {
				return true
			}
			for _, child := range v {
				if references(child) {
					return true
				}
			}
		case []any:
			for _, child := range v {
				if references(child) {
					return true
				}
			}
		}
		return false
	}
	for _, context := range contexts {
		if references(context) {
			return true
		}
	}
	return false
}

var components = map[string]bool{"Text": true, "Image": true, "Icon": true, "Video": true, "AudioPlayer": true, "Row": true, "Column": true, "List": true, "Card": true, "Tabs": true, "Divider": true, "Modal": true, "Button": true, "CheckBox": true, "TextField": true, "DateTimeInput": true, "ChoicePicker": true, "Slider": true}

func validateMessage(raw []byte) (string, string, error) {
	m, err := decodeObject(raw, "version", "createSurface", "updateComponents", "updateDataModel")
	if err != nil {
		return "", "", err
	}
	operations := len(m)
	if _, ok := m["version"]; ok {
		if _, err := stringField(m, "version"); err != nil {
			return "", "", validationField("string_required", "version")
		}
		operations--
	}
	if operations != 1 {
		return "", "", validationField("message_shape", "messages")
	}
	var kind string
	for k := range m {
		if k != "version" {
			kind = k
		}
	}
	allowed := []string{"surfaceId"}
	switch kind {
	case "createSurface":
		allowed = append(allowed, "catalogId", "theme", "sendDataModel")
	case "updateComponents":
		allowed = append(allowed, "components")
	case "updateDataModel":
		allowed = append(allowed, "path", "value")
	default:
		return "", "", errors.New("message_kind")
	}
	p, err := decodeObject(m[kind], allowed...)
	if err != nil {
		return "", "", err
	}
	id, err := stringField(p, "surfaceId")
	if err != nil || strings.TrimSpace(id) == "" || len(id) > 256 {
		return "", "", errors.New("surface_id")
	}
	switch kind {
	case "createSurface":
		if _, ok := p["catalogId"]; ok {
			catalog, e := stringField(p, "catalogId")
			if e != nil || catalog != CatalogID {
				return "", "", errors.New("catalog")
			}
		}
		if b, ok := p["sendDataModel"]; ok && string(b) != "true" && string(b) != "false" {
			return "", "", errors.New("send_data_model")
		}
		if b, ok := p["theme"]; ok && (len(b) == 0 || b[0] != '{') {
			return "", "", errors.New("theme")
		}
	case "updateComponents":
		var cs []map[string]json.RawMessage
		if json.Unmarshal(p["components"], &cs) != nil || len(cs) == 0 || len(cs) > 256 {
			return "", "", errors.New("components")
		}
		seen := map[string]bool{}
		for _, c := range cs {
			cid, e := stringField(c, "id")
			name, e2 := stringField(c, "component")
			if e != nil || e2 != nil || cid == "" || len(cid) > 256 || seen[cid] || !components[name] {
				return "", "", errors.New("component")
			}
			if err := validateComponent(c, name); err != nil {
				return "", "", err
			}
			seen[cid] = true
		}
	case "updateDataModel":
		if _, ok := p["path"]; ok {
			if _, err := stringField(p, "path"); err != nil {
				return "", "", err
			}
		}
	}
	return kind, id, nil
}

// canonicalMessage applies only after validation and preserves operation payloads.
func canonicalMessage(raw json.RawMessage) json.RawMessage {
	var message map[string]json.RawMessage
	_ = json.Unmarshal(raw, &message)
	message["version"], _ = json.Marshal(Version)
	if raw, ok := message["createSurface"]; ok {
		var payload map[string]json.RawMessage
		_ = json.Unmarshal(raw, &payload)
		if _, has := payload["catalogId"]; !has {
			payload["catalogId"], _ = json.Marshal(CatalogID)
			message["createSurface"], _ = json.Marshal(payload)
		}
	}
	canonical, _ := json.Marshal(message)
	return canonical
}

type State struct {
	Messages []json.RawMessage
}

// AssignSurfaceID replaces the model's placeholder in every validated message.
func AssignSurfaceID(o Output, id string) Output {
	o.SurfaceID = id
	o.Messages = append([]json.RawMessage(nil), o.Messages...)
	for i, raw := range o.Messages {
		var message map[string]json.RawMessage
		_ = json.Unmarshal(raw, &message)
		for kind, raw := range message {
			if kind == "version" {
				continue
			}
			var payload map[string]json.RawMessage
			_ = json.Unmarshal(raw, &payload)
			payload["surfaceId"], _ = json.Marshal(id)
			message[kind], _ = json.Marshal(payload)
		}
		o.Messages[i], _ = json.Marshal(message)
	}
	return o
}

// Apply builds one new document; no stored state can be continued or replaced.
func Apply(o Output) (State, error) {
	var state []json.RawMessage
	root := false
	if len(o.Messages) == 0 {
		return State{}, errors.New("create_sequence")
	}
	for i, raw := range o.Messages {
		kind, id, err := validateMessage(raw)
		if err != nil || id != o.SurfaceID {
			return State{}, errors.New("state_message")
		}
		if (i == 0) != (kind == "createSurface") {
			return State{}, errors.New("create_sequence")
		}
		raw = canonicalMessage(raw)
		if kind == "updateComponents" {
			var m struct {
				Update struct {
					Components []struct {
						ID string `json:"id"`
					} `json:"components"`
				} `json:"updateComponents"`
			}
			_ = json.Unmarshal(raw, &m)
			for _, c := range m.Update.Components {
				root = root || c.ID == "root"
			}
		}
		state = append(state, raw)
	}
	if !root {
		return State{}, errors.New("root_missing")
	}
	b, err := json.Marshal(state)
	if err != nil || len(b) > MaxState {
		return State{}, errors.New("state_limit")
	}
	return State{Messages: state}, nil
}
