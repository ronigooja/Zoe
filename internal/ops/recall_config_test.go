package ops

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/answer"
	"github.com/ronigooja/Zoe/internal/modelclient"
	"github.com/ronigooja/Zoe/internal/summarize"
)

func TestSummaryAndAnswerConfigureWriteIndependentEnvAndSecrets(t *testing.T) {
	recallOwnedKeyFixture(t)
	root := t.TempDir()
	d := Deployment{Root: root, StackEnv: filepath.Join(root, "zoe-stack.env")}
	configs := map[string]RecallModelConfig{}
	for _, kind := range []string{"summary", "answer"} {
		c := RecallModelConfig{BaseURL: "https://" + kind + ".example/v1", Model: kind + "-model", ReasoningEffort: "low", Thinking: "disabled", SecretPath: filepath.Join(root, "secrets", kind+"-api-key")}
		if kind == "summary" {
			c.ReasoningEffort = "high"
			c.Thinking = "enabled"
			c.TimeoutMS = 200000
		} else {
			c.TimeoutMS = 1000
		}
		configs[kind] = c
		if err := SaveRecallModel(context.Background(), d, kind, c, []byte(kind+"-private")); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(configs["answer"].SecretPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := configs["summary"]
	changed.Model = "new-summary"
	changed.BaseURL = "https://new-summary.example/v1"
	changed.ReasoningEffort = "medium"
	changed.Thinking = "disabled"
	if err := SaveRecallModel(context.Background(), d, "summary", changed, []byte("new-summary-private")); err != nil {
		t.Fatal(err)
	}
	configs["summary"] = changed
	for kind, want := range configs {
		got, err := d.RecallModelConfig(kind)
		if err != nil || got != want {
			t.Fatalf("%s got=%+v want=%+v err=%v", kind, got, want, err)
		}
		info, err := os.Stat(got.SecretPath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s secret permissions err=%v", kind, err)
		}
	}
	after, _ := os.ReadFile(configs["answer"].SecretPath)
	if string(before) != string(after) {
		t.Fatal("summary update changed answer key")
	}
	if err := WriteRecallModelEnvAtomic(d.StackEnv, "summary", changed, true); err != nil {
		t.Fatal(err)
	}
	summary, _ := d.SummaryConfig()
	answer, _ := d.AnswerConfig()
	if summary.BaseURL != "" || answer != configs["answer"] {
		t.Fatal("disable changed another model")
	}
}

func TestSummaryAndAnswerSettingsSurviveUpdate(t *testing.T) {
	recallOwnedKeyFixture(t)
	f := newUpdateFixture(t)
	d := Deployment{Root: f.root, StackEnv: filepath.Join(f.root, "zoe-stack.env")}
	for _, kind := range []string{"summary", "answer"} {
		c := RecallModelConfig{BaseURL: "https://" + kind + ".example/v1", Model: kind + "-model", ReasoningEffort: "medium", Thinking: "disabled", SecretPath: filepath.Join(f.root, "secrets", kind+"-api-key")}
		if kind == "summary" {
			c.TimeoutMS = 125000
		} else {
			c.TimeoutMS = 9000
		}
		if err := SaveRecallModel(context.Background(), d, kind, c, []byte(kind+"-private")); err != nil {
			t.Fatal(err)
		}
	}
	before, err := readEnv(d.StackEnv)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ValidateRelease(f.release)
	if err != nil {
		t.Fatal(err)
	}
	o := LifecycleOptions{Root: f.root, OptRoot: f.opt, DataRoot: f.data, OpenCodeBinary: f.binary}
	if err := (Manager{}).writeDeploymentFiles(o, manifest); err != nil {
		t.Fatal(err)
	}
	after, err := readEnv(d.StackEnv)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range before {
		if strings.HasPrefix(key, "ZOE_SUMMARY_") || strings.HasPrefix(key, "ZOE_ANSWER_") {
			if after[key] != value || !isOperatorOwnedEnvKey(key) {
				t.Errorf("setting %s changed or not operator-owned", key)
			}
		}
	}
	for _, kind := range []string{"summary", "answer"} {
		key, err := os.ReadFile(filepath.Join(f.root, "secrets", kind+"-api-key"))
		if err != nil || string(key) != kind+"-private\n" {
			t.Errorf("%s key changed err=%v", kind, err)
		}
	}
}

func TestTimeoutDefaultsAndRanges(t *testing.T) {
	for _, tc := range []struct {
		kind          string
		def, min, max int
	}{{"summary", 120000, 5000, 600000}, {"answer", 8000, 500, 30000}} {
		t.Run(tc.kind, func(t *testing.T) {
			prefix := "ZOE_" + strings.ToUpper(tc.kind)
			duration, err := modelclient.Timeout(prefix, func(string) string { return "" }, tc.def, tc.min, tc.max)
			if err != nil || duration != time.Duration(tc.def)*time.Millisecond {
				t.Fatalf("default=%s err=%v", duration, err)
			}
			for _, ms := range []int{tc.min, tc.max, tc.min - 1, tc.max + 1, -1} {
				c := RecallModelConfig{BaseURL: "https://gateway.example/v1", Model: "m", SecretPath: "/key", TimeoutMS: ms}
				_, err := c.Normalized(tc.kind)
				valid := ms >= tc.min && ms <= tc.max
				if (err == nil) != valid {
					t.Errorf("timeout=%d err=%v", ms, err)
				}
				values := map[string]string{prefix + "_BASE_URL": "https://gateway.example/v1", prefix + "_MODEL": "m", prefix + "_API_KEY_FILE": "/key", prefix + "_TIMEOUT_MS": strconv.Itoa(ms)}
				get := func(name string) string { return values[name] }
				read := func(string) ([]byte, error) { return []byte("private"), nil }
				if tc.kind == "summary" {
					_, err = summarize.FromEnv(get, read)
				} else {
					_, err = answer.FromEnv(get, read)
				}
				if (err == nil) != valid {
					t.Errorf("runtime timeout=%d err=%v", ms, err)
				}
			}
		})
	}
}
