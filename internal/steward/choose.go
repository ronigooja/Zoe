package steward

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/prompts"
)

// Input applies to one retained register request and scoped authoritative facts.
type Input struct {
	Request          string      `json:"request"`
	DisplayRequested bool        `json:"display_requested"`
	Tasks            []Candidate `json:"tasks"`
}

// Candidate preserves full task facts and stable server-owned identities.
type Candidate struct {
	TaskID           string     `json:"task_id"`
	Goal             string     `json:"goal"`
	Status           string     `json:"status"`
	PendingQuestions []Question `json:"pending_questions"`
	Result           *string    `json:"result"`
	Additions        []string   `json:"additions"`
}

// Question identifies one currently pending question within its task.
type Question struct {
	QuestionID string `json:"question_id"`
	Text       string `json:"text"`
}

// Choice is a decision only; irrelevant fields remain nil and serialize as null.
type Choice struct {
	Action     Action  `json:"action"`
	TaskID     *string `json:"task_id"`
	QuestionID *string `json:"question_id"`
	Reply      *string `json:"reply"`
}

var ErrNoMatch = errors.New("no exact steward candidate identity match")
var ErrNoQuestion = errors.New("no exact steward pending question identity match")

// chooseInstructions applies only to the approved Steward decision boundary.
var chooseInstructions = prompts.Default("steward_choose")

// ConfigurePrompt applies only before serving requests.
func ConfigurePrompt(text string) { chooseInstructions = text }

// Choose validates structure and identity membership, not semantic action eligibility.
// Callers must transactionally revalidate ownership, eligibility and pending state.
func (c *Client) Choose(ctx context.Context, input Input) (choice Choice, resultErr error) {
	if c == nil || c.transport == nil {
		return Choice{}, errors.New("steward is disabled")
	}
	started := time.Now()
	configured, effective := c.CallBudget(ctx)
	defer func() {
		logging.Infof("steward_call configured_ms=%d effective_ms=%d elapsed_ms=%d error_kind=%s", configured.Milliseconds(), effective.Milliseconds(), time.Since(started).Milliseconds(), errorKind(resultErr))
	}()
	ctx, cancel := context.WithTimeout(ctx, effective)
	defer cancel()
	input, refs, err := prepareInput(input)
	if err != nil {
		return Choice{}, err
	}
	data, err := json.Marshal(input)
	if err != nil {
		return Choice{}, errors.New("could not encode steward choice input")
	}
	call := logging.ModelCallFromContext(ctx, "steward")
	call.Input(chooseInstructions, string(data))
	defer func() {
		call.Result(choice, errorKind(resultErr))
		if resultErr != nil {
			logging.Errorf("steward status=choose_failed error_kind=%s", errorKind(resultErr))
		}
	}()
	output, err := c.transport.Complete(ctx, chooseInstructions, string(data))
	call.Output(output)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return Choice{}, &requestError{err: err}
	}
	parsed, err := parseChoice(output)
	if err != nil {
		return Choice{}, err
	}
	if parsed.TaskID != nil {
		questions, ok := refs[*parsed.TaskID]
		if !ok {
			return Choice{}, ErrNoMatch
		}
		if parsed.QuestionID != nil {
			if _, ok := questions[*parsed.QuestionID]; !ok {
				return Choice{}, ErrNoQuestion
			}
		}
	}
	return parsed, nil
}

// prepareInput applies structural checks and normalizes nil arrays without
// truncating facts, selecting tasks or modifying caller-owned slices.
func prepareInput(input Input) (Input, map[string]map[string]struct{}, error) {
	invalid := errors.New("invalid steward input")
	if strings.TrimSpace(input.Request) == "" {
		return Input{}, nil, invalid
	}
	tasks := make([]Candidate, len(input.Tasks))
	refs := make(map[string]map[string]struct{}, len(tasks))
	for i, task := range input.Tasks {
		if strings.TrimSpace(task.TaskID) == "" || strings.TrimSpace(task.Goal) == "" || strings.TrimSpace(task.Status) == "" {
			return Input{}, nil, invalid
		}
		if _, exists := refs[task.TaskID]; exists {
			return Input{}, nil, invalid
		}
		questions := make(map[string]struct{}, len(task.PendingQuestions))
		for _, question := range task.PendingQuestions {
			if strings.TrimSpace(question.QuestionID) == "" || strings.TrimSpace(question.Text) == "" {
				return Input{}, nil, invalid
			}
			if _, exists := questions[question.QuestionID]; exists {
				return Input{}, nil, invalid
			}
			questions[question.QuestionID] = struct{}{}
		}
		refs[task.TaskID] = questions
		task.PendingQuestions = append([]Question{}, task.PendingQuestions...)
		task.Additions = append([]string{}, task.Additions...)
		tasks[i] = task
	}
	input.Tasks = tasks
	return input, refs, nil
}

// parseChoice requires exact, nonduplicate keys and action-specific nullability.
func parseChoice(output string) (Choice, error) {
	invalid := errors.New("invalid steward choice JSON")
	decoder := json.NewDecoder(strings.NewReader(output))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return Choice{}, invalid
	}
	fields := make(map[string]*string, 4)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil || (key != "action" && key != "task_id" && key != "question_id" && key != "reply") {
			return Choice{}, invalid
		}
		name := key.(string)
		if _, exists := fields[name]; exists {
			return Choice{}, invalid
		}
		var value *string
		if err := decoder.Decode(&value); err != nil || (value != nil && strings.TrimSpace(*value) == "") {
			return Choice{}, invalid
		}
		fields[name] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return Choice{}, invalid
	}
	if _, err := decoder.Token(); err != io.EOF || len(fields) != 4 || fields["action"] == nil {
		return Choice{}, invalid
	}
	choice := Choice{Action: Action(*fields["action"]), TaskID: fields["task_id"], QuestionID: fields["question_id"], Reply: fields["reply"]}
	var task, question, reply bool
	switch choice.Action {
	case ActionNew:
	case ActionAppend, ActionContinue, ActionCancel, ActionRedisplay:
		task = true
	case ActionAnswer:
		task, question = true, true
	case ActionReport:
		task, reply = true, true
	case ActionClarify:
		reply = true
	default:
		return Choice{}, invalid
	}
	if (choice.TaskID != nil) != task || (choice.QuestionID != nil) != question || (choice.Reply != nil) != reply {
		return Choice{}, invalid
	}
	return choice, nil
}
