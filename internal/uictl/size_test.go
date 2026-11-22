package uictl

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		name, container, size string
		screen                *Size
		valid                 bool
	}{
		{"window_min", "window", `{"width":320,"height":240}`, nil, true},
		{"window_max", "window", `{"width":1200,"height":1000}`, nil, true},
		{"popover_min", "popover", `{"width":280,"height":200}`, nil, true},
		{"popover_max", "popover", `{"width":600,"height":720}`, nil, true},
		{"screen_equal", "window", `{"width":400,"height":300}`, &Size{400, 300}, true},
		{"screen_width", "window", `{"width":401,"height":300}`, &Size{400, 300}, false},
		{"screen_height", "window", `{"width":400,"height":301}`, &Size{400, 300}, false},
		{"float", "window", `{"width":400.0,"height":300}`, nil, false},
		{"string", "window", `{"width":"400","height":300}`, nil, false},
		{"extra", "window", `{"width":400,"height":300,"extra":1}`, nil, false},
		{"missing_height", "window", `{"width":400}`, nil, false},
		{"missing_width", "window", `{"height":300}`, nil, false},
		{"null", "window", `null`, nil, false},
		{"null_width", "window", `{"width":null,"height":300}`, nil, false},
		{"duplicate", "window", `{"width":400,"width":500,"height":300}`, nil, false},
		{"window_narrow", "window", `{"width":319,"height":300}`, nil, false},
		{"window_wide", "window", `{"width":1201,"height":300}`, nil, false},
		{"window_short", "window", `{"width":400,"height":239}`, nil, false},
		{"window_tall", "window", `{"width":400,"height":1001}`, nil, false},
		{"popover_narrow", "popover", `{"width":279,"height":300}`, nil, false},
		{"popover_wide", "popover", `{"width":601,"height":300}`, nil, false},
		{"popover_short", "popover", `{"width":400,"height":199}`, nil, false},
		{"popover_tall", "popover", `{"width":400,"height":721}`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Replace(fixture(create+","+root), `"container":"window"`, `"container":"`+tc.container+`"`, 1)
			text = strings.Replace(text, `"size":{"width":400,"height":300}`, `"size":`+tc.size, 1)
			o, err := Parse(text, tc.screen, Show)
			if (err == nil) != tc.valid || (err == nil && o.Size == nil) {
				t.Fatalf("output=%+v err=%v", o, err)
			}
		})
	}
}

func TestBuildInputScreen(t *testing.T) {
	for _, screen := range []*Size{nil, {Width: 1440, Height: 900}} {
		b, err := BuildInput(nil, nil, screen)
		if err != nil {
			t.Fatal(err)
		}
		var input map[string]json.RawMessage
		if err = json.Unmarshal(b, &input); err != nil {
			t.Fatal(err)
		}
		_, exists := input["screen"]
		if exists != (screen != nil) {
			t.Fatal(string(b))
		}
		if screen != nil && string(input["screen"]) != `{"width":1440,"height":900}` {
			t.Fatal(string(b))
		}
		if _, exists := input["surfaces"]; exists {
			t.Fatal(string(b))
		}
	}
}
