package agui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

// RunAgentInput is the body an AG-UI client POSTs to start a run.
type RunAgentInput struct {
	ThreadID    string `json:"threadId"`
	RunID       string `json:"runId"`
	ParentRunID string `json:"parentRunId,omitempty"`
	// State is the client's shared-state object, forwarded verbatim.
	// dialogram has no shared-state concept; a Runner may read it.
	State json.RawMessage `json:"state,omitempty"`
	// Messages is the conversation so far, including the assistant and
	// tool messages of earlier runs. Convert with MessagesToTurns.
	Messages []Message `json:"messages"`
	// Tools are client-side tool definitions (frontend actions). dialogram
	// dispatches server-side tools from its Registry; a Runner that wants
	// to honour client tools must register them itself.
	Tools   []Tool    `json:"tools"`
	Context []Context `json:"context"`
	// ForwardedProps is the application-defined extra payload.
	ForwardedProps json.RawMessage `json:"forwardedProps,omitempty"`
}

// Message roles.
const (
	RoleDeveloper = "developer"
	RoleSystem    = "system"
	RoleAssistant = "assistant"
	RoleUser      = "user"
	RoleTool      = "tool"
)

// ToolCallTypeFunction is the only ToolCall.Type the spec defines.
const ToolCallTypeFunction = "function"

// Message is one AG-UI message: the union of the user, assistant, tool,
// system and developer shapes, with Role saying which fields apply.
//
// Content is always a string here. The spec also allows an array of
// content parts for user and tool messages; UnmarshalJSON accepts that
// form and keeps the text parts (joined with newlines), dropping image,
// audio, video and document parts, which the block IR cannot carry. The
// object content of an activity message decodes as empty text (those
// messages are dropped by MessagesToTurns anyway).
type Message struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
	// ToolCalls is set on assistant messages that requested tools.
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
	// ToolCallID is set on tool messages: the ToolCall.ID this answers.
	ToolCallID string `json:"toolCallId,omitempty"`
	// Error is the optional failure text of a tool message.
	Error string `json:"error,omitempty"`
}

// UnmarshalJSON accepts "content" as a string or as a content-part array.
func (m *Message) UnmarshalJSON(b []byte) error {
	type plain Message
	var w struct {
		plain
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	text, err := contentText(w.Content)
	if err != nil {
		return err
	}
	*m = Message(w.plain)
	m.Content = text
	return nil
}

func contentText(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	switch raw[0] {
	case '"':
		var s string
		err := json.Unmarshal(raw, &s)
		return s, err
	case '[':
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &parts); err != nil {
			return "", fmt.Errorf("agui: message content parts: %w", err)
		}
		var sb strings.Builder
		for _, p := range parts {
			if p.Type != "text" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(p.Text)
		}
		return sb.String(), nil
	case '{':
		if !json.Valid(raw) {
			return "", fmt.Errorf("agui: message content: invalid JSON object")
		}
		return "", nil
	}
	return "", fmt.Errorf("agui: message content must be a string or an array of content parts")
}

// ToolCall is a tool request carried by an assistant message.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names the tool and carries its JSON-encoded arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is a client-declared tool definition (JSON Schema parameters).
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Context is one piece of client-supplied context.
type Context struct {
	Description string `json:"description"`
	Value       string `json:"value"`
}

// MessagesToTurns lifts AG-UI messages into the block IR:
//
//   - user      → a user turn with one text block
//   - assistant → an assistant turn: a text block when content is
//     non-empty, then one tool_use block per toolCall (arguments parsed as
//     JSON; empty means {})
//   - tool      → a tool_result block; consecutive tool messages aggregate
//     into one tool turn, matching how the loop records them. The tool
//     name is recovered from the assistant toolCall with the same id.
//   - system / developer → dropped. dialogram's system prompt comes from
//     the Profile; see SystemText to forward them as MemoryContext.
//   - anything else (activity, reasoning, unknown) → dropped.
//
// It fails on a tool call without an id or name, on arguments that are not
// valid JSON, and on a tool message whose toolCallId no earlier assistant
// message requested — a provider would reject that history anyway.
func MessagesToTurns(msgs []Message) ([]agent.Turn, error) {
	out := make([]agent.Turn, 0, len(msgs))
	names := map[string]string{}
	for i, m := range msgs {
		switch m.Role {
		case RoleUser:
			out = append(out, agent.UserTurn(m.Content))
		case RoleAssistant:
			var blocks []agent.Block
			if m.Content != "" {
				blocks = append(blocks, agent.Block{Type: agent.BlockText, Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				if tc.ID == "" || tc.Function.Name == "" {
					return nil, fmt.Errorf("agui: messages[%d]: tool call needs an id and a function name", i)
				}
				input, err := argumentsJSON(tc.Function.Arguments)
				if err != nil {
					return nil, fmt.Errorf("agui: messages[%d]: tool call %s: %w", i, tc.ID, err)
				}
				names[tc.ID] = tc.Function.Name
				blocks = append(blocks, agent.Block{Type: agent.BlockToolUse, ToolUseID: tc.ID, ToolName: tc.Function.Name, ToolInput: input})
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, agent.Turn{Role: agent.RoleAssistant, Blocks: blocks})
		case RoleTool:
			if m.ToolCallID == "" {
				return nil, fmt.Errorf("agui: messages[%d]: tool message needs a toolCallId", i)
			}
			name, ok := names[m.ToolCallID]
			if !ok {
				return nil, fmt.Errorf("agui: messages[%d]: tool message answers unknown tool call %s", i, m.ToolCallID)
			}
			block := agent.Block{Type: agent.BlockToolResult, ToolUseID: m.ToolCallID, ToolName: name, ToolResult: resultJSON(m)}
			if n := len(out); n > 0 && out[n-1].Role == agent.RoleTool {
				out[n-1].Blocks = append(out[n-1].Blocks, block)
			} else {
				out = append(out, agent.Turn{Role: agent.RoleTool, Blocks: []agent.Block{block}})
			}
		}
	}
	return out, nil
}

// argumentsJSON validates a toolCall's arguments string as JSON. Empty
// means no arguments, which the loop spells {}.
func argumentsJSON(args string) (json.RawMessage, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid([]byte(args)) {
		return nil, fmt.Errorf("arguments are not valid JSON")
	}
	return json.RawMessage(args), nil
}

// resultJSON turns a tool message into the raw JSON a tool_result block
// carries: the content verbatim when it already is JSON (the common case —
// tool outputs are serialised by the Registry), otherwise the content as a
// JSON string. A tool message with an error and no content becomes the
// standard hard-failure object so the model sees a failure, not silence.
func resultJSON(m Message) json.RawMessage {
	if m.Content == "" && m.Error != "" {
		b, _ := json.Marshal(tool.Soft(tool.ErrorCodeHardFailure, m.Error))
		return b
	}
	if json.Valid([]byte(m.Content)) {
		return json.RawMessage(m.Content)
	}
	b, _ := json.Marshal(m.Content)
	return b
}

// TurnsToMessages renders turns as AG-UI messages, the inverse of
// MessagesToTurns. Message ids are positional ("msg_1", "msg_2", …) so the
// same history always renders the same ids. An assistant turn becomes one
// message: its text blocks concatenated as content (text first, then the
// tool calls, whatever the block order was), a tool turn becomes one tool
// message per tool_result block. A turn with nothing to say is skipped.
func TurnsToMessages(turns []agent.Turn) []Message {
	out := make([]Message, 0, len(turns))
	next := func() string { return fmt.Sprintf("msg_%d", len(out)+1) }
	for _, t := range turns {
		switch t.Role {
		case agent.RoleAssistant:
			m := Message{Role: RoleAssistant}
			var text strings.Builder
			for _, b := range t.Blocks {
				switch b.Type {
				case agent.BlockText:
					text.WriteString(b.Text)
				case agent.BlockToolUse:
					args := strings.TrimSpace(string(b.ToolInput))
					if args == "" {
						args = "{}"
					}
					m.ToolCalls = append(m.ToolCalls, ToolCall{ID: b.ToolUseID, Type: ToolCallTypeFunction, Function: FunctionCall{Name: b.ToolName, Arguments: args}})
				}
			}
			m.Content = text.String()
			if m.Content == "" && len(m.ToolCalls) == 0 {
				continue
			}
			m.ID = next()
			out = append(out, m)
		case agent.RoleTool:
			for _, b := range t.Blocks {
				if b.Type != agent.BlockToolResult {
					continue
				}
				out = append(out, Message{ID: next(), Role: RoleTool, ToolCallID: b.ToolUseID, Content: string(b.ToolResult)})
			}
		default:
			var text strings.Builder
			for _, b := range t.Blocks {
				if b.Type == agent.BlockText {
					text.WriteString(b.Text)
				}
			}
			out = append(out, Message{ID: next(), Role: RoleUser, Content: text.String()})
		}
	}
	return out
}

// SystemText joins the content of the system and developer messages in
// msgs, in order, separated by blank lines. MessagesToTurns drops those
// messages; a Runner that wants the client's instructions to reach the
// model can pass this as agent.ChatInput.MemoryContext.
func SystemText(msgs []Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		if m.Role != RoleSystem && m.Role != RoleDeveloper {
			continue
		}
		if m.Content == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(m.Content)
	}
	return sb.String()
}
