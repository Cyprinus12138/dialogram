package agent

import "encoding/json"

// Block is the provider-neutral content unit of a conversation turn. A turn
// is a sequence of blocks; a conversation is a sequence of turns. This is
// the internal representation the agent loop manipulates — each provider
// has a translator that maps Blocks to its own wire format.
//
// The IR exists so mid-turn state (tool_use / tool_result pairs) can be
// persisted in a provider-agnostic shape, enabling two things a flat
// message list cannot:
//
//   - Human-in-the-loop: a paused tool_use can be resumed by appending a
//     tool_result block later, without re-running the whole turn.
//   - Cross-provider failover: a conversation started on OpenAI can be
//     resumed on Anthropic (and vice versa) because neither wire format is
//     baked into the persisted state.
//
// ToolUseID is always a neutral id minted by NewToolUseID. The
// provider-native id is only meaningful within a single in-flight request
// and is never stored.
type Block struct {
	Type      BlockType       `json:"type"`
	Text      string          `json:"text,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	// ToolResult holds the raw JSON returned by tool.Registry.Call — already
	// serialised by the tool. OpenAI's tool-role content takes this string
	// verbatim; Anthropic's tool_result.content accepts the same string.
	ToolResult json.RawMessage `json:"tool_result,omitempty"`
}

// BlockType enumerates the block kinds the IR supports.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
)

// Turn is one role-scoped sequence of blocks. Role is one of:
//
//   - "user"      — user-sent text (Blocks are all BlockText)
//   - "assistant" — model output (may mix BlockText and BlockToolUse)
//   - "tool"      — tool execution results (Blocks are all BlockToolResult,
//     one per BlockToolUse in the immediately preceding assistant turn)
//
// A "tool" turn may aggregate results from multiple tool_use blocks in the
// preceding assistant turn. Translators that need one message per result
// (OpenAI) expand; translators that pack them into one user message
// (Anthropic) wrap.
type Turn struct {
	Role   string  `json:"role"`
	Blocks []Block `json:"blocks"`
}

// Roles a Turn may carry.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// UserTurn builds a single-text user turn.
func UserTurn(text string) Turn {
	return Turn{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: text}}}
}

// normalizeRole coerces an incoming chat role to "user" or "assistant".
// Anything else (including empty) collapses to "user" — the only safe
// default for untrusted input.
func normalizeRole(role string) string {
	if role == RoleUser || role == RoleAssistant {
		return role
	}
	return RoleUser
}

// normalizeTurnRole is the Turn-level equivalent of normalizeRole, adding
// the "tool" role that is valid for Turns but not for ChatMessage.
func normalizeTurnRole(role string) string {
	switch role {
	case RoleUser, RoleAssistant, RoleTool:
		return role
	}
	return RoleUser
}

// MessagesToTurns lifts the flat ChatInput.Messages shape into Turns: each
// message becomes one turn with a single text block. Empty content is
// preserved rather than filtered so translators produce the same wire bytes
// a caller would get from sending the message directly.
func MessagesToTurns(msgs []ChatMessage) []Turn {
	out := make([]Turn, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, Turn{
			Role:   normalizeRole(m.Role),
			Blocks: []Block{{Type: BlockText, Text: m.Content}},
		})
	}
	return out
}

// InputTurns returns the Turn sequence to feed the provider loop. If Turns
// is set it is used (empty turns dropped, roles normalised); otherwise the
// flat Messages are lifted. Never returns nil — an empty slice means no
// history.
func (in ChatInput) InputTurns() []Turn {
	if len(in.Turns) > 0 {
		out := make([]Turn, 0, len(in.Turns))
		for _, t := range in.Turns {
			if len(t.Blocks) == 0 {
				continue
			}
			t.Role = normalizeTurnRole(t.Role)
			out = append(out, t)
		}
		return out
	}
	return MessagesToTurns(in.Messages)
}

// ChoiceOption is one selectable item in a request_user_choice prompt.
type ChoiceOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// FormQuestion is one question in a request_user_form prompt. Type
// constrains the input shape: "text" → free-form string; "single_choice" →
// exactly one option value (or other_text when the user picks "Other");
// "multi_choice" → a list of option values (with optional other_text).
type FormQuestion struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	Prompt      string         `json:"prompt"`
	Required    bool           `json:"required,omitempty"`
	Options     []ChoiceOption `json:"options,omitempty"`
	AllowOther  bool           `json:"allow_other,omitempty"`
	Placeholder string         `json:"placeholder,omitempty"`
}

// SplitPauseTurn partitions the tool_use blocks of an assistant turn into
// pause tools (as decided by isPause) and regular tools. A turn is "mixed"
// when both slices are non-empty: the model co-requested a pause alongside
// real tool calls. The loop runs the regular tools, attaches their results,
// and feeds a deterministic correction back for each pause block instead
// of silently discarding the real work. Non-tool_use blocks are ignored.
func SplitPauseTurn(t Turn, isPause func(name string) bool) (pause, regular []Block) {
	for _, b := range t.Blocks {
		if b.Type != BlockToolUse {
			continue
		}
		if isPause != nil && isPause(b.ToolName) {
			pause = append(pause, b)
		} else {
			regular = append(regular, b)
		}
	}
	return pause, regular
}

// tailAssistant returns the last turn when it is an assistant turn.
func tailAssistant(turns []Turn) (Turn, bool) {
	if len(turns) == 0 {
		return Turn{}, false
	}
	last := turns[len(turns)-1]
	if last.Role != RoleAssistant {
		return Turn{}, false
	}
	return last, true
}

// FindPendingHITL returns the details of a request_user_input tool_use
// block at the tail of turns that has no paired tool_result. ok=false when
// the conversation is not paused on a free-text question.
//
// Pairing is scoped to the most recent assistant turn: any tool_use in an
// earlier assistant turn is, by construction, already paired by the loop's
// own tool turn that follows it.
func FindPendingHITL(turns []Turn) (toolUseID, prompt string, ok bool) {
	last, ok := tailAssistant(turns)
	if !ok {
		return "", "", false
	}
	for _, b := range last.Blocks {
		if b.Type != BlockToolUse || b.ToolName != hitlInputName {
			continue
		}
		return b.ToolUseID, extractPromptField(b.ToolInput), true
	}
	return "", "", false
}

// FindPendingChoice is FindPendingHITL for the structured-choice variant.
// rawInput is the full tool_input JSON so callers can store it without
// re-scanning the turn.
func FindPendingChoice(turns []Turn) (toolUseID string, rawInput json.RawMessage, prompt string, options []ChoiceOption, multiple bool, ok bool) {
	last, found := tailAssistant(turns)
	if !found {
		return
	}
	for _, b := range last.Blocks {
		if b.Type != BlockToolUse || b.ToolName != hitlChoiceName {
			continue
		}
		prompt, options, multiple = extractChoiceFields(b.ToolInput)
		return b.ToolUseID, b.ToolInput, prompt, options, multiple, true
	}
	return
}

// FindPendingForm is FindPendingHITL for the multi-question form variant.
func FindPendingForm(turns []Turn) (toolUseID string, rawInput json.RawMessage, title, reason string, questions []FormQuestion, ok bool) {
	last, found := tailAssistant(turns)
	if !found {
		return
	}
	for _, b := range last.Blocks {
		if b.Type != BlockToolUse || b.ToolName != hitlFormName {
			continue
		}
		title, reason, questions = extractFormFields(b.ToolInput)
		return b.ToolUseID, b.ToolInput, title, reason, questions, true
	}
	return
}

// FindPendingPause detects an application-defined pause tool (see
// Config.PauseTools) at the tail of turns with no paired tool_result.
// isPause decides which tool names count; the built-in HITL tools are
// handled by the three Find* functions above and are not matched here.
func FindPendingPause(turns []Turn, isPause func(name string) bool) (toolUseID, toolName string, rawInput json.RawMessage, ok bool) {
	last, found := tailAssistant(turns)
	if !found || isPause == nil {
		return
	}
	for _, b := range last.Blocks {
		if b.Type != BlockToolUse || isBuiltinHITL(b.ToolName) || !isPause(b.ToolName) {
			continue
		}
		return b.ToolUseID, b.ToolName, b.ToolInput, true
	}
	return
}

// Pause describes a detected pause in an assistant turn, in the shape the
// waiting_for_user stream event carries.
type Pause struct {
	Kind      string
	ToolUseID string
	ToolName  string
	Input     json.RawMessage
	Message   string
	Options   []ChoiceOption
	Multiple  bool
}

// DetectPause inspects a PURE pause turn (no co-requested regular tools —
// callers split mixed turns first with SplitPauseTurn) and returns the
// pause it carries, if any. The built-in HITL kinds are checked first, then
// isPause for application-defined pause tools.
func DetectPause(turn Turn, isPause func(name string) bool) (*Pause, bool) {
	one := []Turn{turn}
	if id, prompt, ok := FindPendingHITL(one); ok {
		return &Pause{Kind: PauseKindHITL, ToolUseID: id, ToolName: hitlInputName, Message: prompt}, true
	}
	if id, raw, prompt, options, multiple, ok := FindPendingChoice(one); ok {
		return &Pause{Kind: PauseKindHITLChoice, ToolUseID: id, ToolName: hitlChoiceName, Input: raw,
			Message: prompt, Options: options, Multiple: multiple}, true
	}
	if id, raw, title, _, _, ok := FindPendingForm(one); ok {
		// The full form definition lives in Input; Message carries the
		// optional title so a pending row has a prompt column populated.
		return &Pause{Kind: PauseKindHITLForm, ToolUseID: id, ToolName: hitlFormName, Input: raw, Message: title}, true
	}
	if id, name, raw, ok := FindPendingPause(one, isPause); ok {
		return &Pause{Kind: PauseKindCustom, ToolUseID: id, ToolName: name, Input: raw}, true
	}
	return nil, false
}

// Event renders the pause as its waiting_for_user stream event.
func (p *Pause) Event() StreamEvent {
	ev := StreamEvent{
		Type:      EventWaitingForUser,
		Kind:      p.Kind,
		ToolUseID: p.ToolUseID,
		Message:   p.Message,
		Input:     p.Input,
		Options:   p.Options,
		Multiple:  p.Multiple,
	}
	// Name is carried for the kinds a client dispatches on by tool name
	// (an approval or a custom pause); the built-in HITL kinds are
	// identified by Kind alone.
	if p.Kind == PauseKindApproval || p.Kind == PauseKindCustom {
		ev.Name = p.ToolName
	}
	return ev
}

func extractChoiceFields(raw json.RawMessage) (prompt string, options []ChoiceOption, multiple bool) {
	if len(raw) == 0 {
		return
	}
	var payload struct {
		Prompt   string         `json:"prompt"`
		Options  []ChoiceOption `json:"options"`
		Multiple bool           `json:"multiple"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	return payload.Prompt, payload.Options, payload.Multiple
}

func extractFormFields(raw json.RawMessage) (title, reason string, questions []FormQuestion) {
	if len(raw) == 0 {
		return
	}
	var payload struct {
		Title     string         `json:"title"`
		Reason    string         `json:"reason"`
		Questions []FormQuestion `json:"questions"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	return payload.Title, payload.Reason, payload.Questions
}

// extractPromptField pulls .prompt out of a request_user_input payload.
// Returns "" on any parse failure so a client falls back to generic copy.
func extractPromptField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var payload struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return payload.Prompt
}
