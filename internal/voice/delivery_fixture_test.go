package voice

import "testing"

func assertPlainChineseControlText(t *testing.T, text, want string) {
	t.Helper()
	if text != want {
		t.Fatalf("reply=%q want=%q", text, want)
	}
	for _, r := range text {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			t.Fatalf("ASCII English in reply %q", text)
		}
	}
}
