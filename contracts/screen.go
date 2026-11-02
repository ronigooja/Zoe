package contracts

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const ScreenLibraryVersion = 1

const (
	NodeStack      = "stack"
	NodeGrid       = "grid"
	NodeSplit      = "split"
	NodeGroup      = "group"
	NodeSpacer     = "spacer"
	NodeCanvas     = "canvas"
	NodeLayer      = "layer"
	NodeScroll     = "scroll"
	NodeFrame      = "frame"
	NodeBackground = "background"
	NodeTitle      = "title"
	NodeText       = "text"
	NodeRichText   = "richText"
	NodeList       = "list"
	NodeTable      = "table"
	NodeProgress   = "progress"
	NodeStatusDot  = "statusDot"
	NodeImage      = "image"
	NodeIcon       = "icon"
	NodeLink       = "link"
	NodeCountdown  = "countdown"
	NodeDivider    = "divider"
	NodeButton     = "button"
	NodeChoice     = "choice"
	NodeInput      = "input"
	NodeToggle     = "toggle"
	NodeMetric     = "metric"
	NodeTimeline   = "timeline"
	NodeSteps      = "steps"
	NodeDecision   = "decision"
	NodeCompare    = "compare"
	NodeCollection = "collection"
	NodeArticle    = "article"
	NodeChart      = "chart"
	NodePlace      = "place"
	NodeMedia      = "media"
	NodeNotice     = "notice"
	NodeFacts      = "facts"
	NodeStatus     = "status"
	NodeForm       = "form"
	NodeQuote      = "quote"
	NodeCode       = "code"
	NodeGauge      = "gauge"
	NodeCalendar   = "calendar"
	NodeMap        = "map"
	NodeGallery    = "gallery"
	NodeRanking    = "ranking"
	NodeChecklist  = "checklist"
)

const (
	SurfaceWindow     = "window"
	SurfacePanel      = "panel"
	SurfaceStatusItem = "statusItem"
	SurfacePopover    = "popover"
	SurfaceDockBadge  = "dockBadge"
)

type ScreenAction struct {
	Intent string         `json:"intent"`
	Args   map[string]any `json:"args,omitempty"`
}
type ScreenNode struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Props      map[string]any `json:"props,omitempty"`
	Children   []ScreenNode   `json:"children,omitempty"`
	Emphasis   string         `json:"emphasis,omitempty"`
	Tone       string         `json:"tone,omitempty"`
	TTLSeconds *float64       `json:"ttlSeconds,omitempty"`
	Transition string         `json:"transition,omitempty"`
	Action     *ScreenAction  `json:"action,omitempty"`
	Bind       string         `json:"bind,omitempty"`
}
type ScreenDocument struct {
	Version  int            `json:"version"`
	Revision int64          `json:"revision"`
	Root     ScreenNode     `json:"root"`
	Data     map[string]any `json:"data,omitempty"`
}
type SurfaceWindowSpec struct {
	Visible       *bool    `json:"visible,omitempty"`
	Width         *float64 `json:"width,omitempty"`
	Height        *float64 `json:"height,omitempty"`
	X             *float64 `json:"x,omitempty"`
	Y             *float64 `json:"y,omitempty"`
	Display       *int     `json:"display,omitempty"`
	Floating      *bool    `json:"floating,omitempty"`
	Opacity       *float64 `json:"opacity,omitempty"`
	ClickThrough  *bool    `json:"clickThrough,omitempty"`
	TakesFocus    *bool    `json:"takesFocus,omitempty"`
	Fullscreen    *bool    `json:"fullscreen,omitempty"`
	ShowInDock    *bool    `json:"showInDock,omitempty"`
	ShowInMenuBar *bool    `json:"showInMenuBar,omitempty"`
	Material      string   `json:"material,omitempty"`
}
type SurfaceSpec struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	Document ScreenDocument    `json:"document"`
	Window   SurfaceWindowSpec `json:"window"`
}
type ScreenResponse struct {
	Revision int64         `json:"revision"`
	Surfaces []SurfaceSpec `json:"surfaces"`
}

type ComponentDescriptor struct {
	Type           string         `json:"type"`
	Purpose        string         `json:"purpose"`
	PropsSchema    map[string]any `json:"propsSchema"`
	Example        ScreenNode     `json:"example"`
	Fallback       string         `json:"fallback"`
	LibraryVersion int            `json:"libraryVersion"`
}

const (
	MaxScreenNodes      = 512
	MaxScreenDepth      = 32
	MaxScreenText       = 16384
	MaxScreenPropsBytes = 65536
)

var screenURL = regexp.MustCompile(`(?i)(?:https?://|ftp://|www\.)`)

type propRule struct {
	kind     string
	required bool
	enum     []string
}

var screenRules = map[string]map[string]propRule{
	"stack": {"axis": {kind: "string", required: true}, "spacing": {kind: "number"}, "alignment": {kind: "string"}}, "grid": {"columns": {kind: "number", required: true}}, "split": {"orientation": {kind: "string", required: true}, "ratio": {kind: "number"}}, "group": {"label": {kind: "string", required: true}}, "spacer": {"minLength": {kind: "number"}}, "canvas": {"anchor": {kind: "string"}, "x": {kind: "number"}, "y": {kind: "number"}}, "layer": {}, "scroll": {"axis": {kind: "string"}}, "frame": {"minWidth": {kind: "number"}, "maxWidth": {kind: "number"}, "minHeight": {kind: "number"}, "maxHeight": {kind: "number"}}, "background": {"material": {kind: "string"}, "opacity": {kind: "number"}, "gradient": {kind: "string"}},
	"title": {"text": {kind: "string", required: true}, "level": {kind: "number"}}, "text": {"text": {kind: "string", required: true}}, "richText": {"markdown": {kind: "string", required: true}}, "list": {"items": {kind: "array", required: true}}, "table": {"columns": {kind: "array", required: true}, "rows": {kind: "array", required: true}}, "progress": {"value": {kind: "number", required: true}, "label": {kind: "string"}}, "statusDot": {"state": {kind: "string", required: true}, "label": {kind: "string"}}, "image": {"url": {kind: "string", required: true}, "alt": {kind: "string"}}, "icon": {"name": {kind: "string", required: true}}, "link": {"label": {kind: "string", required: true}, "url": {kind: "string", required: true}}, "countdown": {"until": {kind: "string", required: true}, "label": {kind: "string"}}, "divider": {},
	"button": {"label": {kind: "string", required: true}, "style": {kind: "string"}}, "choice": {"options": {kind: "array", required: true}, "selected": {kind: "string"}}, "input": {"label": {kind: "string", required: true}, "value": {kind: "string"}, "placeholder": {kind: "string"}, "inputType": {kind: "string"}}, "toggle": {"label": {kind: "string", required: true}, "value": {kind: "bool"}},
	"metric": {"value": {kind: "any", required: true}, "unit": {kind: "string"}, "delta": {kind: "number"}, "baseline": {kind: "number"}, "label": {kind: "string"}}, "timeline": {"events": {kind: "array", required: true}}, "steps": {"steps": {kind: "array", required: true}, "current": {kind: "number"}, "fraction": {kind: "number"}}, "decision": {"question": {kind: "string", required: true}, "options": {kind: "array", required: true}}, "compare": {"items": {kind: "array", required: true}, "dimensions": {kind: "array", required: true}, "values": {kind: "array", required: true}}, "collection": {"items": {kind: "array", required: true}, "groupBy": {kind: "string"}, "sortBy": {kind: "string"}, "layout": {kind: "string", enum: []string{"list", "grid", "cards"}}}, "article": {"title": {kind: "string", required: true}, "body": {kind: "string"}, "sections": {kind: "array"}, "sources": {kind: "array"}}, "chart": {"kind": {kind: "string", required: true}, "series": {kind: "array", required: true}, "axes": {kind: "array"}}, "place": {"coordinate": {kind: "array", required: true}, "name": {kind: "string"}, "annotations": {kind: "array"}}, "media": {"kind": {kind: "string", required: true}, "url": {kind: "string", required: true}, "caption": {kind: "string"}}, "notice": {"text": {kind: "string", required: true}, "tone": {kind: "string"}, "action": {kind: "object"}}, "facts": {"pairs": {kind: "array", required: true}, "groups": {kind: "array"}}, "status": {"state": {kind: "string", required: true}, "description": {kind: "string"}, "updatedAt": {kind: "string"}}, "form": {"fields": {kind: "array", required: true}, "submit": {kind: "object"}}, "quote": {"text": {kind: "string", required: true}, "source": {kind: "string"}}, "code": {"text": {kind: "string", required: true}, "language": {kind: "string"}}, "gauge": {"value": {kind: "number", required: true}, "min": {kind: "number", required: true}, "max": {kind: "number", required: true}, "zones": {kind: "array"}}, "calendar": {"range": {kind: "string", required: true}, "events": {kind: "array", required: true}}, "map": {"points": {kind: "array", required: true}, "route": {kind: "array"}}, "gallery": {"items": {kind: "array", required: true}, "layout": {kind: "string"}}, "ranking": {"items": {kind: "array", required: true}}, "checklist": {"items": {kind: "array", required: true}},
}

func KnownScreenType(typ string) bool { _, ok := screenRules[typ]; return ok }
func ValidateScreenDocument(d ScreenDocument) error {
	if d.Version != ScreenLibraryVersion {
		return fmt.Errorf("unsupported screen version %d", d.Version)
	}
	if d.Revision < 0 {
		return errors.New("screen revision must be non-negative")
	}
	n := 0
	return validateScreenNode(d.Root, 1, &n)
}
func ValidateKnownScreenDocument(d ScreenDocument) error {
	if err := ValidateScreenDocument(d); err != nil {
		return err
	}
	return validateKnownNode(d.Root)
}
func validateKnownNode(n ScreenNode) error {
	if !KnownScreenType(n.Type) {
		return fmt.Errorf("unknown server screen type %q", n.Type)
	}
	for _, c := range n.Children {
		if err := validateKnownNode(c); err != nil {
			return err
		}
	}
	return nil
}
func validateScreenNode(n ScreenNode, depth int, count *int) error {
	if depth > MaxScreenDepth {
		return errors.New("screen node depth exceeds limit")
	}
	*count++
	if *count > MaxScreenNodes {
		return errors.New("screen node count exceeds limit")
	}
	if n.ID == "" || n.Type == "" {
		return errors.New("screen node id and type are required")
	}
	if n.Emphasis != "" && !oneOf(n.Emphasis, "low", "normal", "high") {
		return errors.New("invalid screen emphasis")
	}
	if n.Tone != "" && !oneOf(n.Tone, "neutral", "info", "attention", "success", "critical") {
		return errors.New("invalid screen tone")
	}
	if n.Action != nil {
		if n.Action.Intent == "" || containsURL(n.Action.Intent) || containsURLValue(n.Action.Args) {
			return errors.New("screen action contains a URL")
		}
		if len(n.Action.Args) > 64 {
			return errors.New("screen action args exceed limit")
		}
	}
	rules, known := screenRules[n.Type]
	if known {
		for key, v := range n.Props {
			rule, ok := rules[key]
			if !ok {
				return fmt.Errorf("unknown prop %q for type %q", key, n.Type)
			}
			if !validProp(v, rule.kind) {
				return fmt.Errorf("prop %q for type %q has wrong type", key, n.Type)
			}
			if len(rule.enum) > 0 {
				s, _ := v.(string)
				if !oneOf(s, rule.enum...) {
					return fmt.Errorf("prop %q for type %q has invalid enum", key, n.Type)
				}
			}
		}
		for key, rule := range rules {
			if rule.required {
				if _, ok := n.Props[key]; !ok {
					return fmt.Errorf("required prop %q missing for type %q", key, n.Type)
				}
			}
		}
	}
	if b, _ := json.Marshal(n.Props); len(b) > MaxScreenPropsBytes {
		return errors.New("screen props exceed size limit")
	}
	for _, v := range n.Props {
		if textTooLong(v) {
			return errors.New("screen text exceeds limit")
		}
	}
	for _, c := range n.Children {
		if err := validateScreenNode(c, depth+1, count); err != nil {
			return err
		}
	}
	return nil
}
func validProp(v any, kind string) bool {
	switch kind {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		switch v.(type) {
		case float64, float32, int, int64, json.Number:
			return true
		}
		return false
	case "bool":
		_, ok := v.(bool)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	default:
		return true
	}
}
func textTooLong(v any) bool {
	switch x := v.(type) {
	case string:
		return len(x) > MaxScreenText
	case []any:
		for _, e := range x {
			if textTooLong(e) {
				return true
			}
		}
	case map[string]any:
		for _, e := range x {
			if textTooLong(e) {
				return true
			}
		}
	}
	return false
}
func containsURLValue(v any) bool {
	switch x := v.(type) {
	case string:
		return containsURL(x)
	case []any:
		for _, e := range x {
			if containsURLValue(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if strings.Contains(strings.ToLower(k), "url") || containsURLValue(e) {
				return true
			}
		}
	}
	return false
}
func containsURL(s string) bool {
	if screenURL.MatchString(s) {
		return true
	}
	_, err := url.ParseRequestURI(s)
	return err == nil && strings.Contains(s, "://")
}
func oneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}

var componentPurposes = map[string]string{"stack": "Ordered layout of children", "grid": "Grid layout", "split": "Split layout", "group": "Labeled group of children", "spacer": "Flexible or minimum space", "canvas": "Positioned children", "layer": "Children stacked on top of one another", "scroll": "Scrollable children", "frame": "Size-constrained children", "background": "Background around children", "title": "A heading", "text": "Plain text", "richText": "Markdown text", "list": "A list of values", "table": "Rows and columns of values", "progress": "Progress toward a value", "statusDot": "A compact state indicator", "image": "An image resource", "icon": "A named icon", "link": "A labeled URL", "countdown": "Time remaining until a date", "divider": "A visual divider", "button": "An action control", "choice": "A choice among options", "input": "A text or typed input", "toggle": "A boolean choice", "metric": "A value with trend", "timeline": "Time-ordered events", "steps": "Staged progress", "decision": "A choice needing an answer", "compare": "Several objects by dimension", "collection": "Same-kind items", "article": "Long text", "chart": "Numeric series", "place": "A location", "media": "Image, audio, or video", "notice": "A one-off alert", "facts": "Key-value detail", "status": "Live state", "form": "User-filled fields", "quote": "Cited or quoted text", "code": "Short literal text", "gauge": "A value within a range", "calendar": "Dates and events", "map": "Several places or a route", "gallery": "Many images", "ranking": "Ordered scored items", "checklist": "Items with done state"}

func DescribeComponents() []ComponentDescriptor {
	keys := make([]string, 0, len(screenRules))
	for k := range screenRules {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]ComponentDescriptor, 0, len(keys))
	for _, k := range keys {
		schema := map[string]any{}
		for p, r := range screenRules[k] {
			entry := map[string]any{"type": r.kind}
			if r.required {
				entry["required"] = true
			}
			if len(r.enum) > 0 {
				entry["enum"] = r.enum
			}
			schema[p] = entry
		}
		ex := exampleNode(k)
		out = append(out, ComponentDescriptor{Type: k, Purpose: componentPurposes[k], PropsSchema: schema, Example: ex, Fallback: fallbackFor(k), LibraryVersion: ScreenLibraryVersion})
	}
	return out
}
func fallbackFor(typ string) string {
	if typ == "stack" || typ == "grid" || typ == "split" || typ == "group" || typ == "canvas" || typ == "layer" || typ == "scroll" || typ == "frame" || typ == "background" || typ == "collection" {
		return "stack"
	}
	return "text"
}
func exampleNode(typ string) ScreenNode {
	r := screenRules[typ]
	p := map[string]any{}
	for k, v := range r {
		if !v.required {
			continue
		}
		switch v.kind {
		case "string":
			p[k] = "example"
		case "number":
			p[k] = 1
		case "bool":
			p[k] = true
		case "array":
			p[k] = []any{}
		case "object":
			p[k] = map[string]any{}
		default:
			p[k] = "example"
		}
	}
	if typ == "text" {
		p["text"] = "example"
	}
	return ScreenNode{ID: "example", Type: typ, Props: p}
}
