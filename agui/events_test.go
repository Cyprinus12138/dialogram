package agui

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEventMarshalRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
		want string
	}{
		{"tool result keeps empty content", Event{Type: EventToolCallResult, MessageID: "m", ToolCallID: "c", Role: RoleTool},
			`{"type":"TOOL_CALL_RESULT","messageId":"m","role":"tool","toolCallId":"c","content":""}`},
		{"state delta is an array", Event{Type: EventStateDelta, StateDelta: json.RawMessage(`[{"op":"add","path":"/a","value":1}]`)},
			`{"type":"STATE_DELTA","delta":[{"op":"add","path":"/a","value":1}]}`},
		{"state delta defaults to empty array", Event{Type: EventStateDelta},
			`{"type":"STATE_DELTA","delta":[]}`},
		{"custom keeps null value", Event{Type: EventCustom, Name: "x"},
			`{"type":"CUSTOM","name":"x","value":null}`},
		{"messages snapshot keeps empty list", Event{Type: EventMessagesSnapshot},
			`{"type":"MESSAGES_SNAPSHOT","messages":[]}`},
		{"raw uses event key", Event{Type: EventRaw, RawPayload: json.RawMessage(`{"a":1}`), Source: "s"},
			`{"type":"RAW","event":{"a":1},"source":"s"}`},
		{"text content", Event{Type: EventTextMessageContent, MessageID: "m", Delta: "hi\n"},
			`{"type":"TEXT_MESSAGE_CONTENT","messageId":"m","delta":"hi\n"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := json.Marshal(c.ev)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Fatalf("got  %s\nwant %s", got, c.want)
			}
			var back Event
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			again, _ := json.Marshal(back)
			if string(again) != c.want {
				t.Fatalf("round trip changed the event:\n%s\n%s", again, c.want)
			}
		})
	}
}

func TestEventUnmarshalDelta(t *testing.T) {
	var ev Event
	if err := json.Unmarshal([]byte(`{"type":"TOOL_CALL_ARGS","toolCallId":"c","delta":"{\"a\":1}"}`), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Delta != `{"a":1}` || ev.StateDelta != nil {
		t.Fatalf("string delta misrouted: %+v", ev)
	}
	ev = Event{}
	if err := json.Unmarshal([]byte(`{"type":"STATE_DELTA","delta":[1,2]}`), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Delta != "" || string(ev.StateDelta) != "[1,2]" {
		t.Fatalf("array delta misrouted: %+v", ev)
	}
}

func TestEncodeFrame(t *testing.T) {
	b, err := EncodeFrame(Event{Type: EventRunFinished, ThreadID: "t", RunID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	want := "data: {\"type\":\"RUN_FINISHED\",\"threadId\":\"t\",\"runId\":\"r\"}\n\n"
	if string(b) != want {
		t.Fatalf("frame:\n%q\nwant\n%q", b, want)
	}
	// A newline inside a delta must not break the frame into two lines.
	b, _ = EncodeFrame(Event{Type: EventTextMessageContent, MessageID: "m", Delta: "a\nb"})
	if strings.Count(string(b), "\n") != 2 || !strings.HasSuffix(string(b), "\n\n") {
		t.Fatalf("frame is not a single data line: %q", b)
	}
}

func TestEventJSONKeysAreCamelCase(t *testing.T) {
	ev := Event{Type: EventToolCallStart, Timestamp: 1, RawEvent: json.RawMessage(`1`), ThreadID: "t", RunID: "r",
		Result: json.RawMessage(`1`), Message: "m", Code: "c", StepName: "s", MessageID: "mid", Role: "assistant",
		Delta: "d", ToolCallID: "tc", ToolCallName: "tn", ParentMessageID: "p", Content: "x",
		Snapshot: json.RawMessage(`1`), Messages: []Message{{ID: "1", Role: "user"}}, RawPayload: json.RawMessage(`1`),
		Source: "src", Name: "n", Value: json.RawMessage(`1`)}
	b, _ := json.Marshal(ev)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, key := range []string{"type", "timestamp", "rawEvent", "threadId", "runId", "result", "message", "code",
		"stepName", "messageId", "role", "delta", "toolCallId", "toolCallName", "parentMessageId", "content",
		"snapshot", "messages", "event", "source", "name", "value"} {
		if _, ok := m[key]; !ok {
			t.Errorf("key %q missing from %s", key, b)
		}
	}
	for key := range m {
		if strings.Contains(key, "_") {
			t.Errorf("snake_case key %q leaked onto the wire", key)
		}
	}
}
