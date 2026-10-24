package agui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Event type names, spelled as the AG-UI EventType enum spells them. The
// JSON "type" field of every event carries one of these.
const (
	EventRunStarted         = "RUN_STARTED"
	EventRunFinished        = "RUN_FINISHED"
	EventRunError           = "RUN_ERROR"
	EventStepStarted        = "STEP_STARTED"
	EventStepFinished       = "STEP_FINISHED"
	EventTextMessageStart   = "TEXT_MESSAGE_START"
	EventTextMessageContent = "TEXT_MESSAGE_CONTENT"
	EventTextMessageEnd     = "TEXT_MESSAGE_END"
	EventToolCallStart      = "TOOL_CALL_START"
	EventToolCallArgs       = "TOOL_CALL_ARGS"
	EventToolCallEnd        = "TOOL_CALL_END"
	EventToolCallResult     = "TOOL_CALL_RESULT"
	EventStateSnapshot      = "STATE_SNAPSHOT"
	EventStateDelta         = "STATE_DELTA"
	EventMessagesSnapshot   = "MESSAGES_SNAPSHOT"
	EventRaw                = "RAW"
	EventCustom             = "CUSTOM"
)

// Names of the CUSTOM events the Encoder emits for dialogram concepts that
// have no AG-UI event of their own. Value shapes: CustomGenUI carries the
// gen_ui payload verbatim, CustomWaitingForUser a Pause, CustomUsage a
// Usage, CustomMeta a Meta.
const (
	CustomGenUI          = "gen_ui"
	CustomWaitingForUser = "waiting_for_user"
	CustomUsage          = "usage"
	CustomMeta           = "meta"
)

// ContentType is the media type of an AG-UI SSE response.
const ContentType = "text/event-stream"

// Event is one AG-UI event: the union of every event's fields, with Type
// saying which apply. Field names and casing follow the spec; fields that
// do not apply are omitted from the JSON.
//
//	RUN_STARTED            ThreadID, RunID
//	RUN_FINISHED           ThreadID, RunID, Result (optional), Outcome (optional)
//	RUN_ERROR              Message, Code (optional)
//	STEP_STARTED/FINISHED  StepName
//	TEXT_MESSAGE_START     MessageID, Role ("assistant")
//	TEXT_MESSAGE_CONTENT   MessageID, Delta (non-empty)
//	TEXT_MESSAGE_END       MessageID
//	TOOL_CALL_START        ToolCallID, ToolCallName, ParentMessageID (optional)
//	TOOL_CALL_ARGS         ToolCallID, Delta (JSON text)
//	TOOL_CALL_END          ToolCallID
//	TOOL_CALL_RESULT       MessageID, ToolCallID, Content, Role ("tool")
//	STATE_SNAPSHOT         Snapshot
//	STATE_DELTA            StateDelta (on the wire: "delta", a JSON Patch array)
//	MESSAGES_SNAPSHOT      Messages
//	RAW                    RawPayload (on the wire: "event"), Source (optional)
//	CUSTOM                 Name, Value
//
// Timestamp and RawEvent are the optional BaseEvent fields any event may
// carry. The Encoder leaves Timestamp unset so its output is deterministic.
type Event struct {
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp,omitempty"`
	RawEvent  json.RawMessage `json:"rawEvent,omitempty"`

	ThreadID string          `json:"threadId,omitempty"`
	RunID    string          `json:"runId,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	// Outcome says why a RUN_FINISHED run ended; nil means success.
	Outcome  *Outcome `json:"outcome,omitempty"`
	Message  string   `json:"message,omitempty"`
	Code     string   `json:"code,omitempty"`
	StepName string   `json:"stepName,omitempty"`

	MessageID       string `json:"messageId,omitempty"`
	Role            string `json:"role,omitempty"`
	ToolCallID      string `json:"toolCallId,omitempty"`
	ToolCallName    string `json:"toolCallName,omitempty"`
	ParentMessageID string `json:"parentMessageId,omitempty"`
	// Delta is the text of TEXT_MESSAGE_CONTENT and TOOL_CALL_ARGS. The
	// JSON Patch of STATE_DELTA shares the "delta" key on the wire but is
	// an array, so it lives in StateDelta.
	Delta   string `json:"delta,omitempty"`
	Content string `json:"content,omitempty"`

	Snapshot   json.RawMessage `json:"snapshot,omitempty"`
	StateDelta json.RawMessage `json:"-"`
	Messages   []Message       `json:"messages,omitempty"`

	RawPayload json.RawMessage `json:"event,omitempty"`
	Source     string          `json:"source,omitempty"`

	Name  string          `json:"name,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`

	// snapshot, when set, is what a MESSAGES_SNAPSHOT carries instead of
	// Messages: the client's own messages as it sent them, so a snapshot
	// never rewrites content Message cannot represent.
	snapshot []json.RawMessage
}

// MarshalJSON emits the event with the spec's required-but-possibly-empty
// fields always present: "content" on TOOL_CALL_RESULT, "delta" (an
// array) on STATE_DELTA, "messages" on MESSAGES_SNAPSHOT and "value" on
// CUSTOM. Everything else is plain struct encoding. The wrapper structs
// rely on encoding/json's rule that a field at a shallower depth wins over
// the promoted field of the same name.
func (e Event) MarshalJSON() ([]byte, error) {
	type plain Event
	switch e.Type {
	case EventToolCallResult:
		return json.Marshal(struct {
			plain
			Content string `json:"content"`
		}{plain(e), e.Content})
	case EventStateDelta:
		d := e.StateDelta
		if len(bytes.TrimSpace(d)) == 0 {
			d = json.RawMessage("[]")
		}
		return json.Marshal(struct {
			plain
			Delta json.RawMessage `json:"delta"`
		}{plain(e), d})
	case EventMessagesSnapshot:
		if e.snapshot != nil {
			return json.Marshal(struct {
				plain
				Messages []json.RawMessage `json:"messages"`
			}{plain(e), e.snapshot})
		}
		msgs := e.Messages
		if msgs == nil {
			msgs = []Message{}
		}
		return json.Marshal(struct {
			plain
			Messages []Message `json:"messages"`
		}{plain(e), msgs})
	case EventCustom:
		v := e.Value
		if len(bytes.TrimSpace(v)) == 0 {
			v = json.RawMessage("null")
		}
		return json.Marshal(struct {
			plain
			Value json.RawMessage `json:"value"`
		}{plain(e), v})
	}
	return json.Marshal(plain(e))
}

// UnmarshalJSON is the inverse of MarshalJSON: a string "delta" lands in
// Delta, an array "delta" in StateDelta.
func (e *Event) UnmarshalJSON(b []byte) error {
	type plain Event
	var w struct {
		plain
		Delta json.RawMessage `json:"delta"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*e = Event(w.plain)
	d := bytes.TrimSpace(w.Delta)
	switch {
	case len(d) == 0 || bytes.Equal(d, []byte("null")):
	case d[0] == '"':
		if err := json.Unmarshal(d, &e.Delta); err != nil {
			return err
		}
	default:
		e.StateDelta = d
	}
	return nil
}

// EncodeFrame renders ev as one SSE frame: "data: <json>\n\n". AG-UI's SSE
// encoding carries no "event:" or "id:" lines; the event type is inside the
// JSON. json.Marshal escapes newlines, so the payload is always one line.
func EncodeFrame(ev Event) ([]byte, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("agui: encode %s: %w", ev.Type, err)
	}
	out := make([]byte, 0, len(b)+8)
	out = append(out, "data: "...)
	out = append(out, b...)
	out = append(out, "\n\n"...)
	return out, nil
}

// WriteFrame writes ev to w as one SSE frame.
func WriteFrame(w io.Writer, ev Event) error {
	b, err := EncodeFrame(ev)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
