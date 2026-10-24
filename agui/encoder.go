package agui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

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
	// History is the conversation the run started from, as the client sent
	// it: RunAgentInput.messages, one raw JSON object per message. The spec
	// requires a MESSAGES_SNAPSHOT before a run ends on an interrupt; the
	// snapshot echoes History verbatim and appends the messages this run
	// produced, so it never rewrites a message the client holds. The
	// Handler fills it.
	History []json.RawMessage
	// InterruptID names the interrupt raised for a pause and says when it
	// expires (zero for never). Default: the pause's tool call id and no
	// expiry. The Handler signs the id so that a resume can only answer a
	// call it paused on.
	InterruptID func(p Pause) (id string, expiresAt time.Time)
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
//   - A pause ends the run with MESSAGES_SNAPSHOT and a RUN_FINISHED whose
//     outcome is an interrupt bound to the paused tool call.
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

	history     []json.RawMessage
	interruptID func(Pause) (string, time.Time)
	// log is what this run added to the conversation, folded the way the
	// reference client folds the stream: an assistant message per text
	// segment or tool-call group, a tool message per result. logIndex
	// finds an assistant message by id.
	log      []Message
	logIndex map[string]int
}

// NewEncoder builds an Encoder.
func NewEncoder(opts EncoderOptions) *Encoder {
	if opts.NewID == nil {
		opts.NewID = NewID
	}
	if opts.InterruptID == nil {
		opts.InterruptID = func(p Pause) (string, time.Time) { return p.ToolUseID, time.Time{} }
	}
	return &Encoder{threadID: opts.ThreadID, runID: opts.RunID, newID: opts.NewID, calls: map[string]bool{}, rendered: map[string]bool{},
		history: opts.History, interruptID: opts.InterruptID, logIndex: map[string]int{}}
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
			e.logAssistant(e.openMsg)
			out = append(out, Event{Type: EventTextMessageStart, MessageID: e.openMsg, Role: RoleAssistant})
		}
		e.log[e.logIndex[e.openMsg]].Content += ev.Delta
		out = append(out, Event{Type: EventTextMessageContent, MessageID: e.openMsg, Delta: ev.Delta})
	case agent.EventToolCall:
		out = append(out, e.closeMessage()...)
		out = append(out, e.toolCall(ev.ToolUseID, ev.Name, ev.Input)...)
	case agent.EventToolResult:
		out = append(out, e.closeMessage()...)
		out = append(out, e.toolCall(ev.ToolUseID, ev.Name, ev.Input)...)
		msgID := e.newID()
		// runtime_state is the server's; the client, and the history it
		// sends back, get the rest of the envelope.
		content := string(tool.ClientView(json.RawMessage(ev.Output)))
		e.log = append(e.log, Message{ID: msgID, Role: RoleTool, ToolCallID: ev.ToolUseID, Content: content})
		out = append(out, Event{Type: EventToolCallResult, MessageID: msgID, ToolCallID: ev.ToolUseID, Content: content, Role: RoleTool})
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
		if p.ToolUseID == "" {
			// Nothing to bind a resume to: report the pause the CUSTOM way
			// only.
			out = append(out, Event{Type: EventCustom, Name: CustomWaitingForUser, Value: mustJSON(p)})
			out = append(out, e.finish(nil))
			break
		}
		out = append(out, e.toolCall(p.ToolUseID, p.Name, p.Input)...)
		out = append(out, Event{Type: EventCustom, Name: CustomWaitingForUser, Value: mustJSON(p)})
		out = append(out, e.snapshot())
		out = append(out, e.finish(&Outcome{Type: OutcomeInterrupt, Interrupts: []Interrupt{e.interrupt(p)}}))
	case agent.EventDone:
		out = append(out, e.closeMessage()...)
		out = append(out, e.finish(nil))
	case agent.EventError:
		msg := ev.Message
		if msg == "" {
			msg = "the run failed"
		}
		out = append(out, e.fail(msg, "")...)
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
	return append(out, e.finish(nil))
}

// fail ends the run with RUN_ERROR, opening it first if nothing was pushed.
func (e *Encoder) fail(msg, code string) []Event {
	if e.finished {
		return nil
	}
	var out []Event
	if !e.started {
		out = append(out, e.start())
	}
	out = append(out, e.closeMessage()...)
	e.finished = true
	return append(out, Event{Type: EventRunError, Message: msg, Code: code})
}

// answered announces the tool messages a resume added before the run
// continues. Their calls were announced by an earlier run, and the spec
// forbids starting them again, so each gets a TOOL_CALL_RESULT alone, plus
// the gen_ui of an approved call's real output. The messages themselves
// are already part of History.
func (e *Encoder) answered(results []resumeResult) []Event {
	if e.finished {
		return nil
	}
	var out []Event
	if !e.started {
		out = append(out, e.start())
	}
	for _, r := range results {
		e.calls[r.msg.ToolCallID] = true
		out = append(out, Event{Type: EventToolCallResult, MessageID: r.msg.ID, ToolCallID: r.msg.ToolCallID, Content: r.msg.Content, Role: RoleTool})
		if r.executed {
			out = append(out, e.genUI(r.msg.ToolCallID, uiPayload(agent.StreamEvent{Name: r.name, Input: r.args, Output: r.msg.Content}))...)
		}
	}
	return out
}

// interrupt builds the interrupt a pause raises.
func (e *Encoder) interrupt(p Pause) Interrupt {
	id, expires := e.interruptID(p)
	in := Interrupt{
		ID:             id,
		Reason:         interruptReason(p),
		Message:        interruptMessage(p),
		ToolCallID:     p.ToolUseID,
		ResponseSchema: responseSchema(p),
		Metadata:       mustJSON(map[string]InterruptMetadata{MetadataKey: {Kind: p.Kind, Tool: p.Name, Input: p.Input}}),
	}
	if !expires.IsZero() {
		in.ExpiresAt = expires.UTC().Format(time.RFC3339)
	}
	return in
}

// snapshot is the MESSAGES_SNAPSHOT an interrupt requires: the history as
// the client sent it, then what this run added. The wire form echoes the
// history byte for byte; Messages holds the decoded view for Go callers.
func (e *Encoder) snapshot() Event {
	wire := make([]json.RawMessage, 0, len(e.history)+len(e.log))
	decoded := make([]Message, 0, len(e.history)+len(e.log))
	for _, raw := range e.history {
		var m Message
		_ = json.Unmarshal(raw, &m)
		wire, decoded = append(wire, raw), append(decoded, m)
	}
	for _, m := range e.log {
		wire, decoded = append(wire, mustJSON(m)), append(decoded, m)
	}
	return Event{Type: EventMessagesSnapshot, Messages: decoded, snapshot: wire}
}

// logAssistant opens an assistant message in the run's log.
func (e *Encoder) logAssistant(id string) int {
	e.log = append(e.log, Message{ID: id, Role: RoleAssistant})
	e.logIndex[id] = len(e.log) - 1
	return len(e.log) - 1
}

func (e *Encoder) start() Event {
	e.started = true
	if e.runID == "" {
		e.runID = e.newID()
	}
	return Event{Type: EventRunStarted, ThreadID: e.threadID, RunID: e.runID}
}

func (e *Encoder) finish(outcome *Outcome) Event {
	e.finished = true
	e.turnMsg = ""
	return Event{Type: EventRunFinished, ThreadID: e.threadID, RunID: e.runID, Outcome: outcome}
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
	idx, ok := e.logIndex[e.turnMsg]
	if !ok {
		idx = e.logAssistant(e.turnMsg)
	}
	e.log[idx].ToolCalls = append(e.log[idx].ToolCalls, ToolCall{ID: id, Type: ToolCallTypeFunction, Function: FunctionCall{Name: name, Arguments: args}})
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
