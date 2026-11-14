package uictl

import (
	"encoding/json"
	"testing"
)

func TestComponentProperties(t *testing.T) {
	for _, tc := range []struct {
		name, component string
		valid           bool
	}{
		{"binding", `{"id":"root","component":"TextField","label":"Input","value":{"path":"/input"}}`, true},
		{"action", `{"id":"root","component":"Button","child":"label","action":{"event":{"name":"submit","context":{"value":{"path":"/input"}}}}}`, true},
		{"text_missing", `{"id":"root","component":"Text"}`, false},
		{"text_type", `{"id":"root","component":"Text","text":23}`, false},
		{"unknown_property", `{"id":"root","component":"Text","text":"title","extra":true}`, false},
		{"binding_type", `{"id":"root","component":"TextField","label":"Input","value":{"path":false}}`, false},
		{"binding_extra", `{"id":"root","component":"TextField","label":"Input","value":{"path":"/input","extra":true}}`, false},
		{"wrong_children", `{"id":"root","component":"Column","children":[{}]}`, false},
		{"missing_event", `{"id":"root","component":"Button","child":"label","action":{}}`, false},
		{"missing_name", `{"id":"root","component":"Button","child":"label","action":{"event":{"context":{}}}}`, false},
		{"context_null", `{"id":"root","component":"Button","child":"label","action":{"event":{"name":"submit","context":null}}}`, false},
		{"tabs_empty", `{"id":"root","component":"Tabs","tabs":[]}`, false},
		{"tabs_valid", `{"id":"root","component":"Tabs","tabs":[{"title":"Title","child":"content"}]}`, true},
		{"tabs_extra", `{"id":"root","component":"Tabs","tabs":[{"title":"Title","child":"content","extra":false}]}`, false},
		{"weight_null", `{"id":"root","component":"Divider","weight":null}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.component), &c); err != nil {
				t.Fatal(err)
			}
			var name string
			_ = json.Unmarshal(c["component"], &name)
			err := validateComponent(c, name)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
		})
	}
}
