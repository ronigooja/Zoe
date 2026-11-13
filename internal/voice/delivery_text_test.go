package voice

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDeliveredResultIsPlainChineseText(t *testing.T) {
	for _, text := range []string{"今天东京有雨。", strings.Repeat("天气晴朗。", 200)} {
		t.Run(string([]rune(text)[:5]), func(t *testing.T) {
			r := newDeliveryRig(t)
			r.setup(t, text)
			r.exec(t, `UPDATE tasks SET status='complete' WHERE id='task-1'`)
			r.seedCall(t, "s1", "user-1", 1)
			r.run(t)
			r.run(t)
			if r.posts.Load() != 1 {
				t.Fatalf("gateway posts=%d", r.posts.Load())
			}
			sent := r.texts[0]
			assertPlainChineseControlText(t, sent, text)
			if !utf8.ValidString(sent) || sent != text {
				t.Fatalf("invalid or changed result: bytes=%d runes=%d", len(sent), len([]rune(sent)))
			}
			if status, _, attempts := r.row(t, "entry-1"); status != "delivered" || attempts != 1 {
				t.Fatalf("status=%q attempts=%d", status, attempts)
			}
		})
	}
}
