package tool

import (
	"context"
	"encoding/json"
	"errors"
)

// Human-in-the-loop pause tools. The agent loop intercepts these by name
// before dispatch — their Fn is never called. Calling one ends the model's
// turn; the user's answer later arrives as the tool's result.
const (
	// HITLInputToolName pauses the loop for free-text user input.
	HITLInputToolName = "request_user_input"
	// HITLChoiceToolName pauses the loop for a single- or multi-select
	// choice among predefined options.
	HITLChoiceToolName = "request_user_choice"
	// HITLFormToolName pauses the loop for a multi-question form.
	HITLFormToolName = "request_user_form"
	// HITLChoiceOtherValue is the sentinel option value a client sends back
	// when the user picked the free-text "Other" escape hatch.
	HITLChoiceOtherValue = "__other__"
)

// HITLToolNames is the canonical set of built-in pause tools.
var HITLToolNames = []string{HITLInputToolName, HITLChoiceToolName, HITLFormToolName}

// IsHITLTool reports whether name is one of the built-in pause tools.
func IsHITLTool(name string) bool {
	for _, n := range HITLToolNames {
		if n == name {
			return true
		}
	}
	return false
}

// ErrHITLShortCircuit is returned if a pause tool's Fn is ever executed —
// the loop must intercept these before dispatch.
var ErrHITLShortCircuit = errors.New("tool: pause tools must be intercepted by the agent loop before execution")

const (
	descHITLInput = "Pause the conversation and ask the user a clarifying question or request a decision before continuing. " +
		"Use sparingly — prefer sensible defaults when possible. " +
		"Your current turn ends immediately after calling this tool; do not combine it with other tool calls in the same turn. " +
		"Once the user responds, their answer will arrive as this tool's result and you can continue."

	descHITLChoice = "Pause the conversation and present the user with a list of predefined options to choose from " +
		"(single or multi-select). Use when the answer must come from a known set of values rather than " +
		"free-form text. Set allow_other=true if the listed options may not cover every case — the UI will " +
		"render an extra 'Other' option with an inline text field so the user can describe their answer; the " +
		"typed value comes back in the tool_result's other_text field. Your current turn ends immediately " +
		"after calling this tool; do not combine it with other tool calls in the same turn. Once the user " +
		"selects, their choice arrives as this tool's result and you can continue."

	descHITLForm = "Pause the conversation and present the user with a form of multiple questions to answer " +
		"in one turn. Use when you need to gather several pieces of information at once and resolving them " +
		"with separate request_user_input / request_user_choice calls would be a worse experience. Each " +
		"question can be free-text, single-choice, or multi-choice; choice questions support allow_other for " +
		"an 'Other' escape hatch. Your current turn ends immediately after calling this tool; do not combine " +
		"it with other tool calls in the same turn. Once the user submits the form, all answers arrive " +
		"together as this tool's result and you can continue."

	descPrompt       = "The question or request to display to the user. Write it so a non-technical user can answer in one or two sentences."
	descChoicePrompt = "The question to display. Write it so a non-technical user understands what they are choosing."
	descReason       = "Optional brief explanation of why this input is needed. Shown to the user alongside the prompt."
	descChoiceReason = "Optional brief explanation of why this choice is needed. Shown alongside the options."
	descOptions      = "The options the user may choose from. Each item has a label (display text) and a value (machine-readable identifier)."
	descLabel        = "Human-readable display text shown in the UI."
	descValue        = "Machine-readable identifier returned when the user picks this option."
	descMultiple     = "When true the user may select more than one option before confirming. Defaults to false (single-select, immediate confirm)."
	descAllowOther   = "When true the UI adds an 'Other' option with an inline text field so the user can describe an answer not covered by the listed options. Defaults to false. The user's typed value comes back as other_text in the tool_result."

	descFormTitle     = "Optional short title shown above the form."
	descFormReason    = "Optional brief explanation of why these answers are needed. Shown alongside the form."
	descFormQuestions = "The list of questions to ask. Each question has an id (unique within this form), a type (text|single_choice|multi_choice), and a prompt. Choice questions also carry options and an optional allow_other flag."
	descFormQID       = "Stable id used as the key in the answers map you receive back. Must be unique within this form. Use snake_case."
	descFormQType     = "One of: text (free-form string), single_choice (one option), multi_choice (zero or more options)."
	descFormQPrompt   = "The question text shown to the user."
	descFormQRequired = "When true the user cannot submit the form without answering. Defaults to true."
	descFormQOptions  = "Required for single_choice and multi_choice. Ignored for text."
)

// RegisterHITL registers the three pause tools.
func RegisterHITL(r *Registry) {
	r.Register(hitlInput())
	r.Register(hitlChoice())
	r.Register(hitlForm())
}

func shortCircuit(context.Context, json.RawMessage) (any, error) { return nil, ErrHITLShortCircuit }

func hitlInput() *Tool {
	return &Tool{
		Name:        HITLInputToolName,
		Label:       "Ask the user",
		Description: descHITLInput,
		Schema: map[string]any{
			"type":     "object",
			"required": []string{"prompt"},
			"properties": map[string]any{
				"prompt": map[string]any{"type": "string", "description": descPrompt},
				"reason": map[string]any{"type": "string", "description": descReason},
			},
		},
		Fn:          shortCircuit,
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
	}
}

func hitlChoice() *Tool {
	return &Tool{
		Name:        HITLChoiceToolName,
		Label:       "Offer a choice",
		Description: descHITLChoice,
		Schema: map[string]any{
			"type":     "object",
			"required": []string{"prompt", "options"},
			"properties": map[string]any{
				"prompt": map[string]any{"type": "string", "description": descChoicePrompt},
				"options": map[string]any{
					"type":        "array",
					"description": descOptions,
					"minItems":    2,
					"maxItems":    10,
					"items":       choiceOptionSchema(),
				},
				"multiple":    map[string]any{"type": "boolean", "description": descMultiple, "default": false},
				"allow_other": map[string]any{"type": "boolean", "description": descAllowOther, "default": false},
				"reason":      map[string]any{"type": "string", "description": descChoiceReason},
			},
		},
		Fn:          shortCircuit,
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
	}
}

func hitlForm() *Tool {
	return &Tool{
		Name:        HITLFormToolName,
		Label:       "Ask a few questions",
		Description: descHITLForm,
		Schema: map[string]any{
			"type":     "object",
			"required": []string{"questions"},
			"properties": map[string]any{
				"title":  map[string]any{"type": "string", "description": descFormTitle},
				"reason": map[string]any{"type": "string", "description": descFormReason},
				"questions": map[string]any{
					"type":        "array",
					"description": descFormQuestions,
					"minItems":    1,
					"maxItems":    10,
					"items": map[string]any{
						"type":     "object",
						"required": []string{"id", "type", "prompt"},
						"properties": map[string]any{
							"id":     map[string]any{"type": "string", "description": descFormQID},
							"type":   map[string]any{"type": "string", "description": descFormQType, "enum": []string{"text", "single_choice", "multi_choice"}},
							"prompt": map[string]any{"type": "string", "description": descFormQPrompt},
							"required": map[string]any{
								"type": "boolean", "description": descFormQRequired, "default": true,
							},
							"options": map[string]any{
								"type":        "array",
								"description": descFormQOptions,
								"minItems":    2,
								"maxItems":    10,
								"items":       choiceOptionSchema(),
							},
							"allow_other": map[string]any{"type": "boolean", "description": descAllowOther, "default": false},
							"placeholder": map[string]any{"type": "string", "description": "Optional hint text for free-text questions."},
						},
					},
				},
			},
		},
		Fn:          shortCircuit,
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
	}
}

func choiceOptionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"label", "value"},
		"properties": map[string]any{
			"label": map[string]any{"type": "string", "description": descLabel},
			"value": map[string]any{"type": "string", "description": descValue},
		},
	}
}
