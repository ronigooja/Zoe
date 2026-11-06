package ops

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/answer"
	"github.com/ronigooja/Zoe/internal/modelclient"
	"github.com/ronigooja/Zoe/internal/summarize"
)

const DefaultSummarySecretPath = "/etc/zoe/secrets/summary-api-key"
const DefaultAnswerSecretPath = "/etc/zoe/secrets/answer-api-key"

// RecallModelConfig applies to independently configured summary and answer endpoints.
type RecallModelConfig struct {
	BaseURL, Model, ReasoningEffort, Thinking, SecretPath string
	TimeoutMS                                             int
}

func recallPrefix(kind string) (string, error) {
	if kind != "summary" && kind != "answer" {
		return "", errors.New("model kind must be summary or answer")
	}
	return "ZOE_" + strings.ToUpper(kind), nil
}

func (c RecallModelConfig) Normalized(kind string) (RecallModelConfig, error) {
	prefix, err := recallPrefix(kind)
	if err != nil {
		return c, err
	}
	validated, err := (TriageConfig{BaseURL: c.BaseURL, Model: c.Model, ReasoningEffort: c.ReasoningEffort, Thinking: c.Thinking, SecretPath: c.SecretPath}).Normalized()
	if err != nil {
		return c, err
	}
	c.BaseURL, c.Model, c.ReasoningEffort, c.Thinking, c.SecretPath = validated.BaseURL, validated.Model, validated.ReasoningEffort, validated.Thinking, validated.SecretPath
	get := func(string) string {
		if c.TimeoutMS == 0 {
			return ""
		}
		return strconv.Itoa(c.TimeoutMS)
	}
	if kind == "summary" {
		_, err = modelclient.Timeout(prefix, get, 120000, 5000, 600000)
	} else {
		_, err = modelclient.Timeout(prefix, get, 8000, 500, 30000)
	}
	return c, err
}

func (d Deployment) RecallModelConfig(kind string) (RecallModelConfig, error) {
	prefix, err := recallPrefix(kind)
	if err != nil {
		return RecallModelConfig{}, err
	}
	c := RecallModelConfig{SecretPath: filepath.Join(d.Root, "secrets", kind+"-api-key")}
	v, err := readEnv(d.StackEnv)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	c.BaseURL = v[prefix+"_BASE_URL"]
	c.Model = v[prefix+"_MODEL"]
	c.ReasoningEffort = v[prefix+"_REASONING_EFFORT"]
	c.Thinking = v[prefix+"_THINKING"]
	if v[prefix+"_API_KEY_FILE"] != "" {
		c.SecretPath = v[prefix+"_API_KEY_FILE"]
	}
	if raw := v[prefix+"_TIMEOUT_MS"]; raw != "" {
		c.TimeoutMS, err = strconv.Atoi(raw)
		if err != nil || c.TimeoutMS == 0 {
			return c, errors.New("invalid model timeout")
		}
	}
	return c, nil
}

func (d Deployment) SummaryConfig() (RecallModelConfig, error) { return d.RecallModelConfig("summary") }
func (d Deployment) AnswerConfig() (RecallModelConfig, error)  { return d.RecallModelConfig("answer") }

// SaveRecallModel applies one model change under the deployment lock and restores
// both files if persistence fails. It never changes another model credential.
func SaveRecallModel(ctx context.Context, d Deployment, kind string, c RecallModelConfig, secret []byte) error {
	var err error
	c, err = c.Normalized(kind)
	if err != nil {
		return err
	}
	unlock, err := lockDeployment(ctx, d)
	if err != nil {
		return err
	}
	defer unlock()
	otherKind := "summary"
	if kind == "summary" {
		otherKind = "answer"
	}
	other, err := d.RecallModelConfig(otherKind)
	if err != nil {
		return err
	}
	if filepath.Clean(other.SecretPath) == filepath.Clean(c.SecretPath) || filepath.Clean(d.StackEnv) == filepath.Clean(c.SecretPath) {
		return errors.New("recall model secret paths must be independent")
	}
	env, err := snapshot(d.StackEnv)
	if err != nil {
		return err
	}
	key, err := snapshot(c.SecretPath)
	if err != nil {
		return err
	}
	if len(secret) > 0 {
		if err = WriteTriageSecretAtomic(c.SecretPath, secret); err != nil {
			return err
		}
	} else if err = ValidateKeyFile(c.SecretPath); err != nil {
		return err
	} else if err = ValidateModelKeyReadable(c.SecretPath); err != nil {
		return err
	}
	if err = WriteRecallModelEnvAtomic(d.StackEnv, kind, c, false); err != nil {
		return errors.Join(err, key.restore(), env.restore())
	}
	return nil
}

// WriteRecallModelEnvAtomic changes only one recall model and retains other settings.
func WriteRecallModelEnvAtomic(path, kind string, c RecallModelConfig, disabled bool) error {
	prefix, err := recallPrefix(kind)
	if err != nil {
		return err
	}
	if disabled {
		c.BaseURL = ""
	} else {
		c, err = c.Normalized(kind)
		if err != nil {
			return err
		}
	}
	updates := map[string]string{"BASE_URL": c.BaseURL, "MODEL": c.Model, "REASONING_EFFORT": c.ReasoningEffort, "THINKING": c.Thinking, "API_KEY_FILE": c.SecretPath}
	if c.TimeoutMS != 0 {
		updates["TIMEOUT_MS"] = strconv.Itoa(c.TimeoutMS)
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	kept := []string{}
	for _, line := range strings.Split(string(old), "\n") {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.HasPrefix(key, prefix+"_") {
			suffix := strings.TrimPrefix(key, prefix+"_")
			if _, replace := updates[suffix]; replace {
				continue
			}
			if suffix == "TIMEOUT_MS" && c.TimeoutMS != 0 {
				continue
			}
		}
		kept = append(kept, line)
	}
	for len(kept) > 0 && kept[len(kept)-1] == "" {
		kept = kept[:len(kept)-1]
	}
	for _, suffix := range []string{"BASE_URL", "MODEL", "REASONING_EFFORT", "THINKING", "API_KEY_FILE", "TIMEOUT_MS"} {
		if value := updates[suffix]; value != "" {
			kept = append(kept, prefix+"_"+suffix+"="+quoteEnv(value))
		}
	}
	mode := os.FileMode(0600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return WriteFileAtomic(path, []byte(strings.Join(kept, "\n")+"\n"), mode)
}

type RecallProbeResult struct {
	Style      string
	Latency    time.Duration
	HTTPStatus int
}

// TestRecallModel invokes the production runtime parser with a synthetic fixture.
func TestRecallModel(ctx context.Context, kind string, c RecallModelConfig, client *http.Client) (RecallProbeResult, error) {
	c, err := c.Normalized(kind)
	if err != nil {
		return RecallProbeResult{}, err
	}
	if err := ValidateModelKeyReadable(c.SecretPath); err != nil {
		return RecallProbeResult{}, err
	}
	prefix, _ := recallPrefix(kind)
	values := map[string]string{prefix + "_BASE_URL": c.BaseURL, prefix + "_MODEL": c.Model, prefix + "_REASONING_EFFORT": c.ReasoningEffort, prefix + "_THINKING": c.Thinking, prefix + "_API_KEY_FILE": c.SecretPath}
	if c.TimeoutMS != 0 {
		values[prefix+"_TIMEOUT_MS"] = strconv.Itoa(c.TimeoutMS)
	}
	get := func(key string) string { return values[key] }
	start := time.Now()
	result := RecallProbeResult{}
	if kind == "summary" {
		impl, e := summarize.FromEnv(get, os.ReadFile)
		if e != nil {
			return result, e
		}
		if impl == nil {
			return result, errors.New("summary model is disabled")
		}
		impl.SetHTTPClient(client)
		_, err = impl.Summarize(ctx, []summarize.Turn{{Seq: 1, Role: "user", Text: "The project meeting is on Monday."}, {Seq: 2, Role: "assistant", Text: "The project meeting is scheduled for Monday."}})
		result.Style = impl.Style()
	} else {
		impl, e := answer.FromEnv(get, os.ReadFile)
		if e != nil {
			return result, e
		}
		if impl == nil {
			return result, errors.New("answer model is disabled")
		}
		impl.SetHTTPClient(client)
		_, err = impl.Answer(ctx, "When is the project meeting?", []answer.Candidate{{ID: "fixture-1", Topic: "Project meeting", StartTime: "2026-01-01T10:00:00Z", EndTime: "2026-01-01T10:01:00Z", Summary: "The project meeting is on Monday."}}, nil)
		result.Style = impl.Style()
	}
	result.Latency = time.Since(start)
	if err == nil {
		result.HTTPStatus = http.StatusOK
	} else {
		var httpErr *modelclient.HTTPError
		if errors.As(err, &httpErr) {
			result.HTTPStatus = httpErr.StatusCode
		}
	}
	return result, err
}
