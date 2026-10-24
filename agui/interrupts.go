package agui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

// Run outcomes, the type of RUN_FINISHED.outcome. An absent outcome means
// success, so the Encoder sets one only when a run ends on an interrupt.
const (
	OutcomeSuccess   = "success"
	OutcomeInterrupt = "interrupt"
	OutcomeCancelled = "cancelled"
)

// Interrupt reasons. ReasonToolCall, ReasonInputRequired and
// ReasonConfirmation are the spec's core values. An application-defined
// pause tool's interrupt uses CustomReasonPrefix plus the tool's name,
// namespaced as the spec asks of custom reasons.
const (
	ReasonToolCall      = "tool_call"
	ReasonInputRequired = "input_required"
	ReasonConfirmation  = "confirmation"
	CustomReasonPrefix  = "dialogram:"
)

// Resume statuses, RunAgentInput.resume[].status.
const (
	ResumeResolved  = "resolved"
	ResumeCancelled = "cancelled"
)

// Error codes of the tool results the Handler writes when it resumes a run:
// the ones agent.AnswerPause and agent.DismissedResultJSON write on the sse
// path, so the model reads the same thing on either transport.
const (
	// ErrorCodeCancelled answers an interrupt the user dismissed.
	ErrorCodeCancelled = tool.ErrorCodeCancelled
	// ErrorCodeNotRun answers a tool call requested in the same turn as the
	// interrupted one. The call never ran; the model may make it again.
	ErrorCodeNotRun = tool.ErrorCodeNotRun
)

// MetadataKey is the key under which an Interrupt's metadata describes the
// dialogram pause behind it (see InterruptMetadata).
const MetadataKey = "dialogram"

// Outcome is RUN_FINISHED.outcome: why the run ended.
type Outcome struct {
	Type       string      `json:"type"`
	Interrupts []Interrupt `json:"interrupts,omitempty"`
}

// Interrupt is something a run needs from outside before it can continue.
// The Encoder builds one per pause: Reason follows the pause kind,
// ToolCallID names the paused tool call, and ResponseSchema describes the
// payload a resume must carry.
type Interrupt struct {
	ID             string          `json:"id"`
	Reason         string          `json:"reason"`
	Message        string          `json:"message,omitempty"`
	ToolCallID     string          `json:"toolCallId,omitempty"`
	ResponseSchema json.RawMessage `json:"responseSchema,omitempty"`
	ExpiresAt      string          `json:"expiresAt,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
}

// ResumeEntry answers one interrupt on the run that continues from it.
type ResumeEntry struct {
	InterruptID string          `json:"interruptId"`
	Status      string          `json:"status"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}

// InterruptMetadata is the value under MetadataKey in Interrupt.Metadata:
// the pause behind the interrupt, for clients that render dialogram's
// pause kinds themselves. Kind is one of the agent.PauseKind* constants.
type InterruptMetadata struct {
	Kind  string          `json:"kind"`
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input,omitempty"`
}

// interruptReason maps a pause kind to an interrupt reason.
func interruptReason(p Pause) string {
	switch p.Kind {
	case agent.PauseKindApproval:
		return ReasonToolCall
	case agent.PauseKindHITL, agent.PauseKindHITLChoice, agent.PauseKindHITLForm:
		return ReasonInputRequired
	}
	return CustomReasonPrefix + p.Name
}

// interruptMessage is the prompt shown to whoever answers.
func interruptMessage(p Pause) string {
	if p.Message != "" {
		return p.Message
	}
	switch p.Kind {
	case agent.PauseKindApproval:
		return fmt.Sprintf("Approve running %s?", p.Name)
	case agent.PauseKindHITLForm:
		return "Please answer these questions."
	}
	return ""
}

// choiceInput and formInput are the parts of the HITL pause inputs the
// response schema and the payload check need.
type choiceInput struct {
	Prompt     string               `json:"prompt"`
	Options    []agent.ChoiceOption `json:"options"`
	Multiple   bool                 `json:"multiple"`
	AllowOther bool                 `json:"allow_other"`
}

type formQuestion struct {
	ID         string               `json:"id"`
	Type       string               `json:"type"`
	Prompt     string               `json:"prompt"`
	Required   *bool                `json:"required"`
	Options    []agent.ChoiceOption `json:"options"`
	AllowOther bool                 `json:"allow_other"`
}

// required reports whether the question must be answered; the tool schema
// makes required default to true.
func (q formQuestion) required() bool { return q.Required == nil || *q.Required }

type formInput struct {
	Title     string         `json:"title"`
	Questions []formQuestion `json:"questions"`
}

func decodeInput(raw json.RawMessage, v any) {
	if len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, v)
	}
}

// labelled renders choice options as a JSON Schema string enum with titles,
// which generic form builders show as labelled choices.
func labelled(opts []agent.ChoiceOption) map[string]any {
	one := make([]any, 0, len(opts))
	for _, o := range opts {
		one = append(one, map[string]any{"const": o.Value, "title": o.Label})
	}
	s := map[string]any{"type": "string"}
	if len(one) > 0 {
		s["oneOf"] = one
	}
	return s
}

func otherText() map[string]any {
	return map[string]any{"type": "string", "title": "Other", "description": "The user's own answer, when none of the options fits."}
}

// responseSchema describes the payload a resolved resume must carry for p.
// The payload of an input pause becomes the pause tool's result verbatim,
// so the shapes are the result bodies the loop's prompts expect; an
// approval payload is the decision. A custom pause tool defines its own
// result, so it gets no schema.
func responseSchema(p Pause) json.RawMessage {
	var s map[string]any
	switch p.Kind {
	case agent.PauseKindApproval:
		s = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"approved": map[string]any{"type": "boolean", "title": "Approve", "description": fmt.Sprintf("Run %s with the arguments shown.", p.Name)},
				"reason":   map[string]any{"type": "string", "title": "Reason", "description": "Optional. Why the call was declined; the assistant reads it."},
			},
			"required":             []string{"approved"},
			"additionalProperties": false,
		}
	case agent.PauseKindHITL:
		s = map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"user_response": map[string]any{"type": "string", "title": p.Message}},
			"required":             []string{"user_response"},
			"additionalProperties": false,
		}
	case agent.PauseKindHITLChoice:
		var in choiceInput
		decodeInput(p.Input, &in)
		if len(in.Options) == 0 {
			in.Options = p.Options
		}
		selected := map[string]any{"type": "array", "title": p.Message, "items": labelled(in.Options), "uniqueItems": true}
		if !in.AllowOther {
			selected["minItems"] = 1
		}
		if !(in.Multiple || p.Multiple) {
			selected["maxItems"] = 1
		}
		props := map[string]any{"selected": selected}
		if in.AllowOther {
			props["other_text"] = otherText()
		}
		s = map[string]any{"type": "object", "properties": props, "required": []string{"selected"}, "additionalProperties": false}
	case agent.PauseKindHITLForm:
		var in formInput
		decodeInput(p.Input, &in)
		props := map[string]any{}
		required := []string{}
		for _, q := range in.Questions {
			props[q.ID] = questionSchema(q)
			if q.required() {
				required = append(required, q.ID)
			}
		}
		answers := map[string]any{"type": "object", "title": in.Title, "properties": props, "required": required, "additionalProperties": false}
		s = map[string]any{"type": "object", "properties": map[string]any{"answers": answers}, "required": []string{"answers"}, "additionalProperties": false}
	default:
		return nil
	}
	return mustJSON(s)
}

func questionSchema(q formQuestion) map[string]any {
	switch q.Type {
	case "single_choice":
		props := map[string]any{"value": labelled(q.Options)}
		s := map[string]any{"type": "object", "title": q.Prompt, "properties": props, "additionalProperties": false}
		if q.AllowOther {
			props["other_text"] = otherText()
		} else {
			s["required"] = []string{"value"}
		}
		return s
	case "multi_choice":
		props := map[string]any{"values": map[string]any{"type": "array", "items": labelled(q.Options), "uniqueItems": true}}
		if q.AllowOther {
			props["other_text"] = otherText()
		}
		return map[string]any{"type": "object", "title": q.Prompt, "properties": props, "required": []string{"values"}, "additionalProperties": false}
	}
	return map[string]any{"type": "string", "title": q.Prompt}
}

// approvalAnswer is the payload of a resolved approval interrupt.
type approvalAnswer struct {
	Approved bool
	Reason   string
}

// checkPayload validates a resolved resume's payload against the interrupt
// it answers: the checks responseSchema promises, done by hand so the
// handler needs no JSON Schema engine. args is the paused call's input.
func checkPayload(kind string, payload, args json.RawMessage) (approvalAnswer, error) {
	switch kind {
	case agent.PauseKindApproval:
		fields, err := objectFields(payload, "approved", "reason")
		if err != nil {
			if strings.Contains(err.Error(), `"editedArgs"`) {
				return approvalAnswer{}, fmt.Errorf("editing the arguments of a call before approving it is not supported")
			}
			return approvalAnswer{}, err
		}
		var a approvalAnswer
		if err := field(fields, "approved", true, &a.Approved); err != nil {
			return a, err
		}
		return a, field(fields, "reason", false, &a.Reason)
	case agent.PauseKindHITL:
		fields, err := objectFields(payload, "user_response")
		if err != nil {
			return approvalAnswer{}, err
		}
		var s string
		return approvalAnswer{}, field(fields, "user_response", true, &s)
	case agent.PauseKindHITLChoice:
		var in choiceInput
		decodeInput(args, &in)
		fields, err := objectFields(payload, "selected", "other_text")
		if err != nil {
			return approvalAnswer{}, err
		}
		var selected []string
		var other string
		if err := field(fields, "selected", true, &selected); err != nil {
			return approvalAnswer{}, err
		}
		if err := field(fields, "other_text", false, &other); err != nil {
			return approvalAnswer{}, err
		}
		return approvalAnswer{}, checkChoice("selected", selected, other, in.Options, in.Multiple, in.AllowOther, true)
	case agent.PauseKindHITLForm:
		var in formInput
		decodeInput(args, &in)
		fields, err := objectFields(payload, "answers")
		if err != nil {
			return approvalAnswer{}, err
		}
		var answers map[string]json.RawMessage
		if err := field(fields, "answers", true, &answers); err != nil {
			return approvalAnswer{}, err
		}
		return approvalAnswer{}, checkForm(in.Questions, answers)
	}
	return approvalAnswer{}, nil
}

// objectFields decodes payload as a JSON object whose keys are all in
// allowed.
func objectFields(payload json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("the payload must be a JSON object with %s", strings.Join(quoted(allowed), ", "))
	}
	for k := range fields {
		if !contains(allowed, k) {
			return nil, fmt.Errorf("unexpected field %q in the payload; expected %s", k, strings.Join(quoted(allowed), ", "))
		}
	}
	return fields, nil
}

// field decodes fields[name] into dst, failing when a required field is
// missing or a field has the wrong JSON type.
func field(fields map[string]json.RawMessage, name string, required bool, dst any) error {
	raw, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if required {
			return fmt.Errorf("the payload needs %q", name)
		}
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("payload field %q has the wrong type", name)
	}
	return nil
}

// checkChoice validates picked option values. atLeastOne requires an answer:
// a value, or other_text when the options allow it.
func checkChoice(name string, picked []string, other string, opts []agent.ChoiceOption, multiple, allowOther, atLeastOne bool) error {
	if other != "" && !allowOther {
		return fmt.Errorf("%s: this question does not take other_text", name)
	}
	if !multiple && len(picked) > 1 {
		return fmt.Errorf("%s: pick one option, not %d", name, len(picked))
	}
	if atLeastOne && len(picked) == 0 && other == "" {
		return fmt.Errorf("%s: pick an option", name)
	}
	seen := map[string]bool{}
	for _, v := range picked {
		if seen[v] {
			return fmt.Errorf("%s: %q is picked twice", name, v)
		}
		seen[v] = true
		ok := false
		for _, o := range opts {
			ok = ok || o.Value == v
		}
		if !ok {
			return fmt.Errorf("%s: %q is not one of the options", name, v)
		}
	}
	return nil
}

func checkForm(questions []formQuestion, answers map[string]json.RawMessage) error {
	byID := map[string]formQuestion{}
	for _, q := range questions {
		byID[q.ID] = q
	}
	for id := range answers {
		if _, ok := byID[id]; !ok {
			return fmt.Errorf("answers: %q is not a question of this form", id)
		}
	}
	for _, q := range questions {
		raw, ok := answers[q.ID]
		answered := ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
		if !answered {
			if q.required() {
				return fmt.Errorf("answers: %q is required", q.ID)
			}
			continue
		}
		name := "answers." + q.ID
		switch q.Type {
		case "single_choice":
			fields, err := objectFields(raw, "value", "other_text")
			if err != nil {
				return fmt.Errorf("%s: %v", name, err)
			}
			var v, other string
			if err := field(fields, "value", false, &v); err != nil {
				return fmt.Errorf("%s: %v", name, err)
			}
			if err := field(fields, "other_text", false, &other); err != nil {
				return fmt.Errorf("%s: %v", name, err)
			}
			var picked []string
			if v != "" {
				picked = []string{v}
			}
			if err := checkChoice(name, picked, other, q.Options, false, q.AllowOther, q.required()); err != nil {
				return err
			}
		case "multi_choice":
			fields, err := objectFields(raw, "values", "other_text")
			if err != nil {
				return fmt.Errorf("%s: %v", name, err)
			}
			var values []string
			var other string
			if err := field(fields, "values", true, &values); err != nil {
				return fmt.Errorf("%s: %v", name, err)
			}
			if err := field(fields, "other_text", false, &other); err != nil {
				return fmt.Errorf("%s: %v", name, err)
			}
			if err := checkChoice(name, values, other, q.Options, true, q.AllowOther, q.required()); err != nil {
				return err
			}
		default:
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return fmt.Errorf("%s: a text answer must be a string", name)
			}
			if q.required() && strings.TrimSpace(s) == "" {
				return fmt.Errorf("%s: is required", name)
			}
		}
	}
	return nil
}

func quoted(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// softJSON renders a soft-failure tool result.
func softJSON(code, msg string) string {
	return string(mustJSON(tool.Soft(code, msg)))
}
