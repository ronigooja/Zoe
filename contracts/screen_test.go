package contracts

import "testing"

func TestScreenValidationRejectsUnknownPropTypeSizeDepthAndNodes(t *testing.T) {
	cases := []ScreenDocument{
		{Version: 1, Root: ScreenNode{ID: "x", Type: "text", Props: map[string]any{"bad": "x"}}},
		{Version: 1, Root: ScreenNode{ID: "x", Type: "text", Props: map[string]any{"text": 1}}},
		{Version: 1, Root: ScreenNode{ID: "x", Type: "text", Props: map[string]any{"text": string(make([]byte, MaxScreenText+1))}}},
		{Version: 1, Root: deepScreenNode(MaxScreenDepth + 1)},
	}
	for _, doc := range cases {
		if err := ValidateScreenDocument(doc); err == nil {
			t.Fatalf("expected validation error: %#v", doc)
		}
	}
	root := ScreenNode{ID: "root", Type: "stack"}
	for i := 0; i < MaxScreenNodes; i++ {
		root.Children = append(root.Children, ScreenNode{ID: string(rune(i + 1)), Type: "divider"})
	}
	if err := ValidateScreenDocument(ScreenDocument{Version: 1, Root: root}); err == nil {
		t.Fatal("expected node count error")
	}
}

func TestScreenValidationRejectsURLAction(t *testing.T) {
	if err := ValidateScreenDocument(ScreenDocument{Version: 1, Root: ScreenNode{ID: "x", Type: "button", Props: map[string]any{"label": "go"}, Action: &ScreenAction{Intent: "https://bad"}}}); err == nil {
		t.Fatal("expected URL action rejection")
	}
}
func TestComponentDescribeExamplesValidate(t *testing.T) {
	for _, c := range DescribeComponents() {
		if err := ValidateScreenDocument(ScreenDocument{Version: 1, Root: c.Example}); err != nil {
			t.Fatalf("%s example: %v", c.Type, err)
		}
	}
}
func deepScreenNode(n int) ScreenNode {
	root := ScreenNode{ID: "0", Type: "divider"}
	for i := 1; i < n; i++ {
		root = ScreenNode{ID: string(rune(i)), Type: "stack", Children: []ScreenNode{root}}
	}
	return root
}
