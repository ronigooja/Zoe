package uictl

import (
	"encoding/json"
	"errors"
)

type componentDefinition struct {
	required   []string
	properties map[string]string
}

// Component property rules apply to the basic-catalog subset emitted by the controller.
var definitions = map[string]componentDefinition{
	"Text":          {[]string{"text"}, map[string]string{"text": "dynamicString", "variant": "string"}},
	"Image":         {[]string{"url"}, map[string]string{"url": "dynamicString", "description": "dynamicString", "fit": "string", "variant": "string"}},
	"Icon":          {[]string{"name"}, map[string]string{"name": "dynamicString"}},
	"Video":         {[]string{"url"}, map[string]string{"url": "dynamicString"}},
	"AudioPlayer":   {[]string{"url"}, map[string]string{"url": "dynamicString", "description": "dynamicString"}},
	"Row":           {[]string{"children"}, map[string]string{"children": "children", "justify": "string", "align": "string"}},
	"Column":        {[]string{"children"}, map[string]string{"children": "children", "justify": "string", "align": "string"}},
	"List":          {[]string{"children"}, map[string]string{"children": "children", "direction": "string", "align": "string"}},
	"Card":          {[]string{"child"}, map[string]string{"child": "string"}},
	"Tabs":          {[]string{"tabs"}, map[string]string{"tabs": "tabs"}},
	"Divider":       {nil, map[string]string{"axis": "string"}},
	"Modal":         {[]string{"trigger", "content"}, map[string]string{"trigger": "string", "content": "string"}},
	"Button":        {[]string{"child", "action"}, map[string]string{"child": "string", "action": "action", "variant": "string"}},
	"CheckBox":      {[]string{"label", "value"}, map[string]string{"label": "dynamicString", "value": "dynamicBool"}},
	"TextField":     {[]string{"label"}, map[string]string{"label": "dynamicString", "value": "dynamicString", "variant": "string", "validationRegexp": "string"}},
	"DateTimeInput": {[]string{"value"}, map[string]string{"value": "dynamicString", "label": "dynamicString", "min": "dynamicString", "max": "dynamicString", "enableDate": "bool", "enableTime": "bool"}},
	"ChoicePicker":  {[]string{"options", "value"}, map[string]string{"options": "options", "value": "dynamicStrings", "label": "dynamicString", "variant": "string", "displayStyle": "string", "filterable": "bool"}},
	"Slider":        {[]string{"value", "max"}, map[string]string{"value": "dynamicNumber", "label": "dynamicString", "min": "number", "max": "number"}},
}

func validateComponent(c map[string]json.RawMessage, name string) error {
	d := definitions[name]
	for _, key := range d.required {
		if _, ok := c[key]; !ok {
			return errors.New("component_required_property")
		}
	}
	for key, value := range c {
		kind := d.properties[key]
		switch key {
		case "id", "component":
			kind = "string"
		case "weight":
			kind = "number"
		}
		if !validProperty(value, kind) {
			return errors.New("component_property")
		}
		if options := propertyEnums[name+"."+key]; len(options) > 0 {
			var text string
			_ = json.Unmarshal(value, &text)
			found := false
			for _, option := range options {
				found = found || text == option
			}
			if !found {
				return errors.New("component_enum")
			}
		}
	}
	return nil
}

var propertyEnums = map[string][]string{
	"Text.variant":              {"h1", "h2", "h3", "h4", "h5", "caption", "body"},
	"Image.fit":                 {"contain", "cover", "fill", "none", "scaleDown"},
	"Image.variant":             {"icon", "avatar", "smallFeature", "mediumFeature", "largeFeature", "header"},
	"Row.justify":               {"start", "center", "end", "spaceBetween", "spaceAround", "spaceEvenly", "stretch"},
	"Column.justify":            {"start", "center", "end", "spaceBetween", "spaceAround", "spaceEvenly", "stretch"},
	"Row.align":                 {"start", "center", "end", "stretch"},
	"Column.align":              {"start", "center", "end", "stretch"},
	"List.align":                {"start", "center", "end", "stretch"},
	"List.direction":            {"vertical", "horizontal"},
	"Divider.axis":              {"horizontal", "vertical"},
	"Button.variant":            {"default", "primary", "borderless"},
	"TextField.variant":         {"longText", "number", "shortText", "obscured"},
	"ChoicePicker.variant":      {"multipleSelection", "mutuallyExclusive"},
	"ChoicePicker.displayStyle": {"checkbox", "chips"},
}

func validProperty(raw json.RawMessage, kind string) bool {
	if len(raw) == 0 {
		return false
	}
	if len(kind) > 7 && kind[:7] == "dynamic" && raw[0] == '{' {
		// The controller subset permits data-model bindings, not executable functions.
		m, err := decodeObject(raw, "path")
		if err != nil || len(m) != 1 {
			return false
		}
		path, err := stringField(m, "path")
		return err == nil && path != "" && len(path) <= 1024
	}
	switch kind {
	case "string", "dynamicString":
		var value string
		return raw[0] == '"' && json.Unmarshal(raw, &value) == nil
	case "number", "dynamicNumber":
		var value float64
		return raw[0] != 'n' && json.Unmarshal(raw, &value) == nil
	case "bool", "dynamicBool":
		return string(raw) == "true" || string(raw) == "false"
	case "dynamicStrings", "children":
		var values []string
		return raw[0] == '[' && json.Unmarshal(raw, &values) == nil && values != nil && len(values) <= 256
	case "action":
		m, err := decodeObject(raw, "event")
		if err != nil || len(m) != 1 {
			return false
		}
		e, err := decodeObject(m["event"], "name", "context")
		if err != nil {
			return false
		}
		name, err := stringField(e, "name")
		if err != nil || name == "" {
			return false
		}
		if context, ok := e["context"]; ok {
			var fields map[string]json.RawMessage
			if json.Unmarshal(context, &fields) != nil || fields == nil {
				return false
			}
			for _, v := range fields {
				if len(v) > 0 && v[0] == '{' && !validProperty(v, "dynamicString") {
					return false
				}
			}
		}
		return true
	case "tabs", "options":
		var items []map[string]json.RawMessage
		if raw[0] != '[' || json.Unmarshal(raw, &items) != nil || len(items) > 256 || (kind == "tabs" && len(items) == 0) {
			return false
		}
		for _, item := range items {
			first, second := "title", "child"
			if kind == "options" {
				first, second = "label", "value"
			}
			if len(item) != 2 || !validProperty(item[first], "dynamicString") || !validProperty(item[second], "string") {
				return false
			}
		}
		return true
	}
	return false
}
