package agui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

// NewID returns 32 random hex characters — the default minter for message
// ids and for thread/run ids a client left blank.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// EncoderOptions configure an Encoder.
type EncoderOptions struct {
	// ThreadID is echoed on RUN_STARTED and RUN_FINISHED.
	ThreadID string
	// RunID is echoed on RUN_STARTED and RUN_FINISHED. Empty means take
	// the MessageID of the first meta event, or mint one.
	RunID string
	// NewID mints message ids. Default NewID; tests inject a counter.
	NewID func() string
}

// Encoder is a stateful mapper from dialogram stream events to AG-UI
// events. Feed it every agent.StreamEvent in order with Push and write what
// comes back; call Finish when the producer returns. It is not safe for
// concurrent use — providers call emit serially, which is enough.
//
// Guarantees, whatever the input order:
//
//   - RUN_STARTED is the first event out (the first meta opens the run; any
//     other event opens it implicitly).
//   - Every TEXT_MESSAGE_START is closed by TEXT_MESSAGE_END before a tool
//     call, a pause, the end of the run or an error.
//   - Every TOOL_CALL_START is followed by exactly one TOOL_CALL_ARGS and a
//     TOOL_CALL_END, and a TOOL_CALL_RESULT always refers to a call that
//     was started (one is synthesised if the producer never announced it).
//   - Exactly one terminal event, RUN_FINISHED or RUN_ERROR; everything
//     pushed afterwards is dropped.
type Encoder struct {
	threadID string
	runID    string
	newID    func() string

	started  bool
	finished bool
	// openMsg is the id of the TEXT_MESSAGE currently open, "" when none.
	openMsg string
	// turnMsg is the assistant message id tool calls of the current
	// assistant turn attach to (parentMessageId): the latest text segment
	// of the turn, or an id minted when a turn starts with a tool call.
	// Cleared by a tool result, which ends the assistant turn.
	turnMsg string
	// calls records every tool call id already started, so a pause or a
	// result for a call the producer announced does not start it twice.
	calls map[string]bool
	// rendered records the tool call ids a gen_ui payload was already
	// emitted for, so a producer that derives gen_ui events itself (as
	// the sse package does) does not render the same payload twice.
	rendered map[string]bool
}

// NewEncoder builds an Encoder.
func NewEncoder(opts EncoderOptions) *Encoder {
	if opts.NewID == nil {
		opts.NewID = NewID
	}
	return &Encoder{threadID: opts.ThreadID, runID: opts.RunID, newID: opts.NewID, calls: map[string]bool{}, rendered: map[string]bool{}}
}

// ThreadID returns the thread id stamped on the run events.
func (e *Encoder) ThreadID() string { return e.threadID }

// RunID returns the run id, once known (after the first Push).
func (e *Encoder) RunID() string { return e.runID }

// Finished reports whether the terminal event has been emitted.
func (e *Encoder) Finished() bool { return e.finished }

// Usage is the value of a CUSTOM "usage" event: dialogram's running token
// counters, in dialogram's own field names.
type Usage struct {
	InputTokens         int                   `json:"input_tokens"`
	OutputTokens        int                   `json:"output_tokens"`
	CacheReadTokens     int                   `json:"cache_read_tokens"`
	CacheCreationTokens int                   `json:"cache_creation_tokens"`
	Model               string                `json:"model,omitempty"`
	Provider            string                `json:"provider,omitempty"`
	Breakdown           *agent.TokenBreakdown `json:"breakdown,omitempty"`
}

// Pause is the value of a CUSTOM "waiting_for_user" event. Kind is one of
// the agent.PauseKind* constants; ToolUseID is the toolCallId the client
// answers with a tool message on the next run; Name is the pause tool's
// name; Input is its full input; Message is the prompt or reason; Options
// and Multiple are set for a choice pause.
type Pause struct {
	Kind      string               `json:"kind"`
	ToolUseID string               `json:"tool_use_id"`
	Name      string               `json:"name"`
	Input     json.RawMessage      `json:"input,omitempty"`
	Message   string               `json:"message,omitempty"`
	Options   []agent.ChoiceOption `json:"options,omitempty"`
	Multiple  bool                 `json:"multiple,omitempty"`
}

// Meta is the value of a CUSTOM "meta" event: a meta event that arrived
// after the run had already started (an application-level meta such as a
// conversation id, or the provider's model announcement).
type Meta struct {
	MessageID string `json:"message_id,omitempty"`
	Message   string `json:"message,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
}

// Push maps one dialogram event to zero or more AG-UI events.
func (e *Encoder) Push(ev agent.StreamEvent) []Event {
	if e.finished {
		return nil
	}
	var out []Event
	if ev.Type == agent.EventMeta {
		if !e.started {
			if e.runID == "" {
				e.runID = ev.MessageID
			}
			out = append(out, e.start())
			// The run id already carries the message id.
			ev.MessageID = ""
		}
		m := Meta{MessageID: ev.MessageID, Message: ev.Message, Provider: ev.Provider, Model: ev.Model}
		if m != (Meta{}) {
			out = append(out, Event{Type: EventCustom, Name: CustomMeta, Value: mustJSON(m)})
		}
		return out
	}
	if !e.started {
		out = append(out, e.start())
	}
	switch ev.Type {
	case agent.EventText:
		if ev.Delta == "" {
			return out
		}
		if e.openMsg == "" {
			e.openMsg = e.newID()
			e.turnMsg = e.openMsg
			out = append(out, Event{Type: EventTextMessageStart, MessageID: e.openMsg, Role: RoleAssistant})
		}
		out = append(out, Event{Type: EventTextMessageContent, MessageID: e.openMsg, Delta: ev.Delta})
	case agent.EventToolCall:
		out = append(out, e.closeMessage()...)
		out = append(out, e.toolCall(ev.ToolUseID, ev.Name, ev.Input)...)
	case agent.EventToolResult:
		out = append(out, e.closeMessage()...)
		out = append(out, e.toolCall(ev.ToolUseID, ev.Name, ev.Input)...)
		out = append(out, Event{Type: EventToolCallResult, MessageID: e.newID(), ToolCallID: ev.ToolUseID, Content: ev.Output, Role: RoleTool})
		out = append(out, e.genUI(ev.ToolUseID, uiPayload(ev))...)
		e.turnMsg = ""
	case agent.EventGenUI:
		out = append(out, e.genUI(ev.ToolUseID, ev.GenUI)...)
	case agent.EventUsage:
		u := Usage{InputTokens: ev.InputTokens, OutputTokens: ev.OutputTokens, CacheReadTokens: ev.CacheReadTokens,
			CacheCreationTokens: ev.CacheCreationTokens, Model: ev.Model, Provider: ev.Provider, Breakdown: ev.Breakdown}
		out = append(out, Event{Type: EventCustom, Name: CustomUsage, Value: mustJSON(u)})
	case agent.EventWaitingForUser:
		out = append(out, e.closeMessage()...)
		p := Pause{Kind: ev.Kind, ToolUseID: ev.ToolUseID, Name: pauseToolName(ev), Input: pauseInput(ev),
			Message: ev.Message, Options: ev.Options, Multiple: ev.Multiple}
		if p.ToolUseID != "" {
			out = append(out, e.toolCall(p.ToolUseID, p.Name, p.Input)...)
		}
		out = append(out, Event{Type: EventCustom, Name: CustomWaitingForUser, Value: mustJSON(p)})
		out = append(out, e.finish())
	case agent.EventDone:
		out = append(out, e.closeMessage()...)
		out = append(out, e.finish())
	case agent.EventError:
		out = append(out, e.closeMessage()...)
		msg := ev.Message
		if msg == "" {
			msg = "the run failed"
		}
		e.finished = true
		out = append(out, Event{Type: EventRunError, Message: msg})
	}
	return out
}

// Finish closes whatever is still open and ends the run with RUN_FINISHED,
// for producers that return without a done event. It is a no-op after a
// terminal event, and when nothing was ever pushed.
func (e *Encoder) Finish() []Event {
	if e.finished || !e.started {
		return nil
	}
	out := e.closeMessage()
	return append(out, e.finish())
}

func (e *Encoder) start() Event {
	e.started = true
	if e.runID == "" {
		e.runID = e.newID()
	}
	return Event{Type: EventRunStarted, ThreadID: e.threadID, RunID: e.runID}
}

func (e *Encoder) finish() Event {
	e.finished = true
	e.turnMsg = ""
	return Event{Type: EventRunFinished, ThreadID: e.threadID, RunID: e.runID}
}

func (e *Encoder) closeMessage() []Event {
	if e.openMsg == "" {
		return nil
	}
	id := e.openMsg
	e.openMsg = ""
	return []Event{{Type: EventTextMessageEnd, MessageID: id}}
}

// toolCall emits the START / ARGS / END triple for one tool call, once per
// id. dialogram announces complete inputs, so the arguments travel as a
// single TOOL_CALL_ARGS delta.
func (e *Encoder) toolCall(id, name string, input json.RawMessage) []Event {
	if id == "" || e.calls[id] {
		return nil
	}
	e.calls[id] = true
	if e.turnMsg == "" {
		e.turnMsg = e.newID()
	}
	args := strings.TrimSpace(string(input))
	if args == "" {
		args = "{}"
	}
	return []Event{
		{Type: EventToolCallStart, ToolCallID: id, ToolCallName: name, ParentMessageID: e.turnMsg},
		{Type: EventToolCallArgs, ToolCallID: id, Delta: args},
		{Type: EventToolCallEnd, ToolCallID: id},
	}
}

// genUI emits a CUSTOM "gen_ui" event for payload, once per tool call id
// (an empty id is never deduplicated).
func (e *Encoder) genUI(toolUseID string, payload json.RawMessage) []Event {
	if len(payload) == 0 {
		return nil
	}
	if toolUseID != "" {
		if e.rendered[toolUseID] {
			return nil
		}
		e.rendered[toolUseID] = true
	}
	return []Event{{Type: EventCustom, Name: CustomGenUI, Value: payload}}
}

// uiPayload returns the gen_ui payload a tool_result carries, as the sse
// package derives it: present_data renders its input; any other tool
// renders the ui_payload channel of its envelope.
func uiPayload(ev agent.StreamEvent) json.RawMessage {
	if ev.Name == tool.PresentDataToolName {
		return ev.Input
	}
	if ev.Output == "" {
		return nil
	}
	return tool.ExtractUIPayload(json.RawMessage(ev.Output))
}

// pauseToolName names the tool behind a pause. Approval and custom pauses
// carry it on the event; the built-in HITL kinds are identified by Kind.
func pauseToolName(ev agent.StreamEvent) string {
	if ev.Name != "" {
		return ev.Name
	}
	switch ev.Kind {
	case agent.PauseKindHITL:
		return tool.HITLInputToolName
	case agent.PauseKindHITLChoice:
		return tool.HITLChoiceToolName
	case agent.PauseKindHITLForm:
		return tool.HITLFormToolName
	}
	if ev.Kind != "" {
		return ev.Kind
	}
	return "pause"
}

// pauseInput returns the pause tool's input. A free-text pause carries only
// its prompt on the event, so the input is rebuilt from it.
func pauseInput(ev agent.StreamEvent) json.RawMessage {
	if len(strings.TrimSpace(string(ev.Input))) > 0 {
		return ev.Input
	}
	if ev.Kind == agent.PauseKindHITL && ev.Message != "" {
		return mustJSON(map[string]string{"prompt": ev.Message})
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
