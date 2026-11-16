package uictl

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const create = `{"version":"v0.9.1","createSurface":{"surfaceId":"screen","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}}`
const root = `{"version":"v0.9.1","updateComponents":{"surfaceId":"screen","components":[{"id":"root","component":"Text","text":"Title"}]}}`
const update = `{"version":"v0.9.1","updateDataModel":{"surfaceId":"screen","path":"/value","value":"data"}}`
const remove = `{"version":"v0.9.1","deleteSurface":{"surfaceId":"screen"}}`

func fixture(messages string) string {
	return `{"messages":[` + messages + `],"container":"window","feedback":"Shown.","awaiting_input":false,"input_note":""}`
}

func TestParse(t *testing.T) {
	valid := fixture(create + "," + root)
	for _, tc := range []struct {
		name, text string
		valid      bool
	}{
		{"valid", valid, true},
		{"popover", strings.Replace(valid, `"window"`, `"popover"`, 1), true},
		{"input", strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(valid, `"Shown."`, `""`), `"awaiting_input":false`, `"awaiting_input":true`), `"input_note":""`, `"input_note":"Requested value"`), true},
		{"fence_json", "```json\n" + valid + "\n```", true},
		{"fence_plain", "```\n" + valid + "\n```", true},
		{"fence_outer_space", "  \n```json\n" + valid + "\n```\n  ", true},
		{"fence_open_only", "```json\n" + valid, false},
		{"fence_close_only", valid + "\n```", false},
		{"fence_text_after", "```json\n" + valid + "\n```\nDone.", false},
		{"fence_text_before", "Here:\n```json\n" + valid + "\n```", false},
		{"fence_nested", "```json\n```json\n" + valid + "\n```\n```", false},
		{"fence_one_line", "```" + valid + "```", false},
		{"fence_invalid_inside", "```json\n" + strings.Replace(valid, `"container"`, `"extra":0,"container"`, 1) + "\n```", false},
		{"unknown", strings.Replace(valid, `"container"`, `"extra":0,"container"`, 1), false},
		{"duplicate", strings.Replace(valid, `"container"`, `"container":"window","container"`, 1), false},
		{"nested_duplicate", strings.Replace(valid, `"surfaceId":"screen"`, `"surfaceId":"screen","surfaceId":"screen"`, 1), false},
		{"trailing", valid + `{}`, false},
		{"array", "[" + valid + "]", false},
		{"missing", strings.Replace(valid, `,"input_note":""`, "", 1), false},
		{"null_string", strings.Replace(valid, `"feedback":"Shown."`, `"feedback":null`, 1), false},
		{"null_bool", strings.Replace(valid, `"awaiting_input":false`, `"awaiting_input":null`, 1), false},
		{"wrong_bool", strings.Replace(valid, `"awaiting_input":false`, `"awaiting_input":"false"`, 1), false},
		{"container", strings.Replace(valid, `"window"`, `"other"`, 1), false},
		{"empty_feedback", strings.Replace(valid, `"Shown."`, `" "`, 1), false},
		{"large_feedback", strings.Replace(valid, `"Shown."`, `"`+strings.Repeat("x", 8001)+`"`, 1), false},
		{"unexpected_note", strings.Replace(valid, `"input_note":""`, `"input_note":"note"`, 1), false},
		{"input_missing_note", strings.Replace(valid, `"awaiting_input":false`, `"awaiting_input":true`, 1), false},
		{"empty_messages", fixture(""), false},
		{"null_messages", strings.Replace(valid, `"messages":[`+create+","+root+`]`, `"messages":null`, 1), false},
		{"many_messages", fixture(strings.TrimSuffix(strings.Repeat(update+",", 65), ",")), false},
		{"version", strings.Replace(valid, `v0.9.1`, `v0.9`, 1), false},
		{"missing_version", strings.Replace(valid, `"version":"v0.9.1",`, "", 1), false},
		{"multiple_operations", fixture(`{"version":"v0.9.1","deleteSurface":{"surfaceId":"screen"},"updateDataModel":{"surfaceId":"screen"}}`), false},
		{"mixed_surfaces", fixture(create + "," + strings.ReplaceAll(root, `"screen"`, `"another"`)), false},
		{"catalog", strings.Replace(valid, CatalogID, "other", 1), false},
		{"surface_empty", strings.Replace(valid, `"surfaceId":"screen"`, `"surfaceId":""`, 1), false},
		{"surface_long", strings.Replace(valid, `"surfaceId":"screen"`, `"surfaceId":"`+strings.Repeat("a", 257)+`"`, 1), false},
		{"unknown_payload", strings.Replace(valid, `"catalogId"`, `"extra":true,"catalogId"`, 1), false},
		{"send_data_model", strings.Replace(valid, `"catalogId"`, `"sendDataModel":null,"catalogId"`, 1), false},
		{"theme", strings.Replace(valid, `"catalogId"`, `"theme":[],"catalogId"`, 1), false},
		{"unknown_component", strings.Replace(valid, `"component":"Text"`, `"component":"Custom"`, 1), false},
		{"missing_component", strings.Replace(valid, `"component":"Text",`, "", 1), false},
		{"id_number", strings.Replace(valid, `"id":"root"`, `"id":1`, 1), false},
		{"empty_components", fixture(strings.Replace(root, `[{"id":"root","component":"Text","text":"Title"}]`, `[]`, 1)), false},
		{"duplicate_components", fixture(strings.Replace(root, `[{"id":"root","component":"Text","text":"Title"}]`, `[{"id":"root","component":"Text"},{"id":"root","component":"Text"}]`, 1)), false},
		{"component_long_id", strings.Replace(valid, `"id":"root"`, `"id":"`+strings.Repeat("a", 257)+`"`, 1), false},
		{"component_empty_id", strings.Replace(valid, `"id":"root"`, `"id":""`, 1), false},
		{"path_type", fixture(strings.Replace(update, `"/value"`, `false`, 1)), false},
		{"invalid_utf8", valid + string([]byte{255}), false},
		{"oversized", valid + strings.Repeat(" ", MaxOutput), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.text, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
		})
	}
}

func TestParseRejectsStatusItem(t *testing.T) {
	text := strings.Replace(fixture(create+","+root), `"window"`, `"statusItem"`, 1)
	if _, err := Parse(text, nil); err == nil || err.Error() != "container_invalid" {
		t.Fatalf("expected container_invalid, got %v", err)
	}
}

func TestCatalogComponentNames(t *testing.T) {
	if len(components) != 18 {
		t.Fatal(len(components))
	}
	for name, d := range definitions {
		c := map[string]any{"id": "root", "component": name}
		for _, key := range d.required {
			switch d.properties[key] {
			case "dynamicString", "string":
				c[key] = "value"
			case "dynamicBool":
				c[key] = false
			case "dynamicNumber", "number":
				c[key] = 1
			case "dynamicStrings", "children":
				c[key] = []string{}
			case "tabs":
				c[key] = []any{map[string]string{"title": "title", "child": "child"}}
			case "options":
				c[key] = []any{}
			case "action":
				c[key] = map[string]any{"event": map[string]string{"name": "submit"}}
			}
		}
		b, _ := json.Marshal(c)
		message := `{"version":"v0.9.1","updateComponents":{"surfaceId":"screen","components":[` + string(b) + `]}}`
		if _, err := Parse(fixture(message), nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestApply(t *testing.T) {
	initial, _ := Parse(fixture(create+","+root), nil)
	state, err := Apply(nil, initial)
	if err != nil || len(state.Messages) != 2 {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, messages   string
		stored           []json.RawMessage
		count            int
		deleted, wantErr bool
	}{
		{"append", update, state.Messages, 3, false, false},
		{"reset", create + "," + root, state.Messages, 2, false, false},
		{"delete", remove, state.Messages, 1, true, false},
		{"missing", update, nil, 0, false, true},
		{"missing_root", create, nil, 0, false, true},
		{"delete_missing", remove, nil, 0, false, true},
		{"update_deleted", remove + "," + update, state.Messages, 0, false, true},
		{"recreate", remove + "," + create + "," + root, state.Messages, 2, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, e := Parse(fixture(tc.messages), nil)
			if e != nil {
				t.Fatal(e)
			}
			got, e := Apply(tc.stored, o)
			if (e != nil) != tc.wantErr {
				t.Fatal(e)
			}
			if e == nil && (len(got.Messages) != tc.count || got.Deleted != tc.deleted) {
				t.Fatalf("state=%+v", got)
			}
		})
	}
	large := append([]json.RawMessage{}, state.Messages...)
	for i := 0; i < 40; i++ {
		large = append(large, json.RawMessage(strings.Replace(update, `"data"`, `"`+strings.Repeat("x", 8000)+`"`, 1)))
	}
	o, _ := Parse(fixture(update), nil)
	if _, err := Apply(large, o); err == nil {
		t.Fatal("accepted oversized state")
	}
}

func TestBuildInput(t *testing.T) {
	b, err := BuildInput(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"now":"2026-01-02T03:04:05Z","requests":[],"task":null,"surfaces":[]}` {
		t.Fatal(string(b))
	}
	if _, err = BuildInput(time.Now(), nil, nil, make([]Surface, 65), nil); err == nil {
		t.Fatal("accepted too many surfaces")
	}
	if _, err = BuildInput(time.Now(), []string{strings.Repeat("x", 16*MaxState+1)}, nil, nil, nil); err == nil {
		t.Fatal("accepted oversized input")
	}
	for _, request := range []string{"", "  ", "none"} {
		if _, err = BuildInput(time.Now(), []string{request}, nil, nil, nil); err == nil {
			t.Fatalf("accepted incomplete request %q", request)
		}
	}
}
