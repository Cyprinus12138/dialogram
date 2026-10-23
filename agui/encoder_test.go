package agui

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// counter mints deterministic ids: prefix1, prefix2, …
func counter(prefix string) func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("%s%d", prefix, n)
	}
}

func testEncoder() *Encoder {
	return NewEncoder(EncoderOptions{ThreadID: "thread_1", RunID: "run_1", NewID: counter("id_")})
}

func drive(enc *Encoder, evs []agent.StreamEvent) []Event {
	var out []Event
	for _, ev := range evs {
		out = append(out, enc.Push(ev)...)
	}
	return append(out, enc.Finish()...)
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func checkGolden(t *testing.T, name string, events []Event) {
	t.Helper()
	got, err := json.MarshalIndent(events, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name+".golden.json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run go test -update to create it)", path, err)
	}
	if string(want) != string(got) {
		t.Errorf("%s differs from golden\n--- got\n%s--- want\n%s", name, got, want)
	}
}

// The scripted sequences. Each is also checked for well-formedness.
var scripted = map[string][]agent.StreamEvent{
	"basic": {
		{Type: agent.EventMeta, MessageID: "run_1"},
		{Type: agent.EventText, Delta: "Here is "},
		{Type: agent.EventText, Delta: "the forecast."},
		{Type: agent.EventToolCall, Name: "get_weather", Label: "Weather", ToolUseID: "dg_1", Input: raw(`{"city":"Lisbon"}`)},
		{Type: agent.EventToolResult, Name: "get_weather", ToolUseID: "dg_1", Input: raw(`{"city":"Lisbon"}`), Output: `{"model_observation":{"high_c":24}}`},
		{Type: agent.EventGenUI, Name: "get_weather", ToolUseID: "dg_1", GenUI: raw(`{"type":"bar_chart","data":[{"label":"Mon","value":24}]}`)},
		{Type: agent.EventText, Delta: "Warm week ahead."},
		{Type: agent.EventUsage, InputTokens: 120, OutputTokens: 40, CacheReadTokens: 100, Model: "model-x", Provider: "anthropic"},
		{Type: agent.EventDone},
	},
	"pause_choice": {
		{Type: agent.EventMeta, MessageID: "run_1"},
		{Type: agent.EventText, Delta: "One question first."},
		{Type: agent.EventWaitingForUser, Kind: agent.PauseKindHITLChoice, ToolUseID: "dg_2", Message: "Which city?",
			Input:   raw(`{"prompt":"Which city?","options":[{"label":"Lisbon","value":"lis"},{"label":"Porto","value":"opo"}]}`),
			Options: []agent.ChoiceOption{{Label: "Lisbon", Value: "lis"}, {Label: "Porto", Value: "opo"}}},
		// Nothing after a pause reaches the wire.
		{Type: agent.EventText, Delta: "ignored"},
		{Type: agent.EventDone},
	},
	"pause_free_text": {
		{Type: agent.EventMeta, MessageID: "run_1"},
		{Type: agent.EventWaitingForUser, Kind: agent.PauseKindHITL, ToolUseID: "dg_5", Message: "What is your budget?"},
	},
	"pause_approval": {
		{Type: agent.EventMeta, MessageID: "run_1"},
		{Type: agent.EventText, Delta: "Booking it."},
		{Type: agent.EventToolCall, Name: "book_flight", ToolUseID: "dg_3", Input: raw(`{"carrier":"SKYLINE"}`)},
		{Type: agent.EventWaitingForUser, Kind: agent.PauseKindApproval, Name: "book_flight", ToolUseID: "dg_3",
			Input: raw(`{"carrier":"SKYLINE"}`), Message: "book_flight needs your approval"},
	},
	"error": {
		{Type: agent.EventMeta, MessageID: "run_1"},
		{Type: agent.EventText, Delta: "Let me "},
		{Type: agent.EventError, Message: "upstream failed"},
		{Type: agent.EventDone},
	},
	"present_data": {
		{Type: agent.EventMeta, MessageID: "run_1"},
		{Type: agent.EventToolCall, Name: tool.PresentDataToolName, ToolUseID: "dg_4", Input: raw(`{"type":"metric_cards","cards":[{"label":"High","value":"24 °C"}]}`)},
		{Type: agent.EventToolResult, Name: tool.PresentDataToolName, ToolUseID: "dg_4", Input: raw(`{"type":"metric_cards","cards":[{"label":"High","value":"24 °C"}]}`), Output: `{"displayed":true}`},
		{Type: agent.EventToolCall, Name: "get_weather", ToolUseID: "dg_6", Input: raw(`{"city":"Porto"}`)},
		{Type: agent.EventToolResult, Name: "get_weather", ToolUseID: "dg_6", Output: `{"model_observation":{"high_c":21},"ui_payload":{"component":"bar_chart","data":{"title":"Highs"}}}`},
		{Type: agent.EventText, Delta: "Shown above."},
		{Type: agent.EventTurn, Turn: &agent.Turn{Role: agent.RoleAssistant}},
		{Type: agent.EventDone},
	},
	"meta_forwarding": {
		{Type: agent.EventMeta, MessageID: "run_1"},
		{Type: agent.EventMeta, Message: "conv_9", Provider: "openai", Model: "model-y"},
		{Type: agent.EventText, Delta: "ok"},
		{Type: agent.EventDone},
	},
	"no_done": {
		{Type: agent.EventMeta, MessageID: "run_1"},
		{Type: agent.EventToolCall, Name: "ping", ToolUseID: "dg_7"},
		{Type: agent.EventToolResult, Name: "ping", ToolUseID: "dg_7", Output: `"pong"`},
		{Type: agent.EventText, Delta: "pong"},
	},
}

func TestEncoderGolden(t *testing.T) {
	for name, seq := range scripted {
		t.Run(name, func(t *testing.T) {
			events := drive(testEncoder(), seq)
			checkWellFormed(t, events)
			checkGolden(t, name, events)
		})
	}
}

func TestEncoderImplicitStart(t *testing.T) {
	enc := NewEncoder(EncoderOptions{ThreadID: "t", NewID: counter("id_")})
	events := drive(enc, []agent.StreamEvent{{Type: agent.EventText, Delta: "hi"}, {Type: agent.EventDone}})
	checkWellFormed(t, events)
	if events[0].RunID != "id_1" || enc.RunID() != "id_1" {
		t.Fatalf("run id should be minted when no meta names one: %+v", events[0])
	}
	if events[1].MessageID != "id_2" {
		t.Fatalf("message id should follow the run id: %+v", events[1])
	}
}

func TestEncoderRunIDFromMeta(t *testing.T) {
	enc := NewEncoder(EncoderOptions{ThreadID: "t", NewID: counter("id_")})
	events := enc.Push(agent.StreamEvent{Type: agent.EventMeta, MessageID: "msg_abc", Provider: "p"})
	if len(events) != 2 || events[0].RunID != "msg_abc" || events[1].Name != CustomMeta {
		t.Fatalf("expected RUN_STARTED with the meta message id then CUSTOM meta, got %+v", events)
	}
	if string(events[1].Value) != `{"provider":"p"}` {
		t.Fatalf("CUSTOM meta should not repeat the run id: %s", events[1].Value)
	}
}

func TestEncoderTerminalIsFinal(t *testing.T) {
	enc := testEncoder()
	enc.Push(agent.StreamEvent{Type: agent.EventMeta})
	if got := enc.Push(agent.StreamEvent{Type: agent.EventError}); len(got) != 1 || got[0].Type != EventRunError || got[0].Message == "" {
		t.Fatalf("expected RUN_ERROR with a message, got %+v", got)
	}
	if !enc.Finished() {
		t.Fatal("Finished should be true after RUN_ERROR")
	}
	if got := enc.Push(agent.StreamEvent{Type: agent.EventText, Delta: "late"}); got != nil {
		t.Fatalf("events after the terminal must be dropped, got %+v", got)
	}
	if got := enc.Finish(); got != nil {
		t.Fatalf("Finish after the terminal must be a no-op, got %+v", got)
	}
	if got := NewEncoder(EncoderOptions{}).Finish(); got != nil {
		t.Fatalf("Finish on an unstarted encoder must be a no-op, got %+v", got)
	}
}

func TestEncoderParallelToolCallsShareParent(t *testing.T) {
	events := drive(testEncoder(), []agent.StreamEvent{
		{Type: agent.EventToolCall, Name: "a", ToolUseID: "dg_a", Input: raw(`{}`)},
		{Type: agent.EventToolCall, Name: "b", ToolUseID: "dg_b", Input: raw(`{}`)},
		{Type: agent.EventToolResult, Name: "a", ToolUseID: "dg_a", Output: `1`},
		{Type: agent.EventToolResult, Name: "b", ToolUseID: "dg_b", Output: `2`},
		{Type: agent.EventText, Delta: "done"},
		{Type: agent.EventToolCall, Name: "c", ToolUseID: "dg_c", Input: raw(`{}`)},
	})
	checkWellFormed(t, events)
	var parents []string
	for _, ev := range events {
		if ev.Type == EventToolCallStart {
			parents = append(parents, ev.ParentMessageID)
		}
	}
	if len(parents) != 3 || parents[0] == "" || parents[0] != parents[1] || parents[2] == parents[0] {
		t.Fatalf("calls of one turn must share a parent and a later turn must not: %v", parents)
	}
	// The third call attaches to the text segment that preceded it.
	var textID string
	for _, ev := range events {
		if ev.Type == EventTextMessageStart {
			textID = ev.MessageID
		}
	}
	if parents[2] != textID {
		t.Fatalf("call after text should attach to that message: %q vs %q", parents[2], textID)
	}
}

func TestEncoderGenUIDedupe(t *testing.T) {
	events := drive(testEncoder(), []agent.StreamEvent{
		{Type: agent.EventGenUI, ToolUseID: "dg_1", GenUI: raw(`{"a":1}`)},
		{Type: agent.EventToolResult, Name: "x", ToolUseID: "dg_1", Output: `{"ui_payload":{"a":1}}`},
		{Type: agent.EventGenUI, GenUI: raw(`{"b":2}`)},
		{Type: agent.EventGenUI, GenUI: raw(`{"b":2}`)},
		{Type: agent.EventDone},
	})
	n := 0
	for _, ev := range events {
		if ev.Type == EventCustom && ev.Name == CustomGenUI {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("want one gen_ui per tool call id plus every anonymous one (3), got %d: %+v", n, events)
	}
}

// checkWellFormed asserts the AG-UI run invariants over an event list.
func checkWellFormed(t *testing.T, events []Event) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events")
	}
	if events[0].Type != EventRunStarted || events[0].RunID == "" {
		t.Fatalf("first event must be RUN_STARTED with a run id, got %+v", events[0])
	}
	last := events[len(events)-1]
	if last.Type != EventRunFinished && last.Type != EventRunError {
		t.Fatalf("last event must be terminal, got %+v", last)
	}
	openMsg := ""
	openCalls := map[string]bool{}
	started := map[string]bool{}
	for i, ev := range events {
		if i > 0 && ev.Type == EventRunStarted {
			t.Errorf("event %d: duplicate RUN_STARTED", i)
		}
		if i < len(events)-1 && (ev.Type == EventRunFinished || ev.Type == EventRunError) {
			t.Errorf("event %d: terminal event is not last", i)
		}
		switch ev.Type {
		case EventTextMessageStart:
			if openMsg != "" || ev.MessageID == "" || ev.Role != RoleAssistant {
				t.Errorf("event %d: bad TEXT_MESSAGE_START (open=%q): %+v", i, openMsg, ev)
			}
			openMsg = ev.MessageID
		case EventTextMessageContent:
			if ev.MessageID != openMsg || ev.Delta == "" {
				t.Errorf("event %d: TEXT_MESSAGE_CONTENT outside an open message or empty: %+v", i, ev)
			}
		case EventTextMessageEnd:
			if ev.MessageID != openMsg {
				t.Errorf("event %d: TEXT_MESSAGE_END for %q but open is %q", i, ev.MessageID, openMsg)
			}
			openMsg = ""
		case EventToolCallStart:
			if openMsg != "" || ev.ToolCallID == "" || ev.ToolCallName == "" || started[ev.ToolCallID] {
				t.Errorf("event %d: bad TOOL_CALL_START (open=%q): %+v", i, openMsg, ev)
			}
			started[ev.ToolCallID] = true
			openCalls[ev.ToolCallID] = true
		case EventToolCallArgs:
			if !openCalls[ev.ToolCallID] || !json.Valid([]byte(ev.Delta)) {
				t.Errorf("event %d: TOOL_CALL_ARGS for a call that is not open, or not JSON: %+v", i, ev)
			}
		case EventToolCallEnd:
			if !openCalls[ev.ToolCallID] {
				t.Errorf("event %d: TOOL_CALL_END for a call that is not open: %+v", i, ev)
			}
			delete(openCalls, ev.ToolCallID)
		case EventToolCallResult:
			if !started[ev.ToolCallID] || openCalls[ev.ToolCallID] || ev.MessageID == "" || ev.Role != RoleTool {
				t.Errorf("event %d: TOOL_CALL_RESULT for an unknown or still-open call: %+v", i, ev)
			}
		case EventCustom:
			if ev.Name == "" {
				t.Errorf("event %d: CUSTOM without a name", i)
			}
		case EventRunFinished, EventRunError:
			if openMsg != "" || len(openCalls) > 0 {
				t.Errorf("event %d: terminal with open message %q / calls %v", i, openMsg, openCalls)
			}
			if ev.Type == EventRunError && ev.Message == "" {
				t.Errorf("event %d: RUN_ERROR without a message", i)
			}
		}
		b, err := json.Marshal(ev)
		if err != nil {
			t.Errorf("event %d: marshal: %v", i, err)
		}
		var back Event
		if err := json.Unmarshal(b, &back); err != nil || back.Type != ev.Type {
			t.Errorf("event %d: round trip failed: %v", i, err)
		}
	}
}

// TestEncoderWellFormedProperty drives the encoder with pseudo-random
// event sequences, including ones that never end, and checks the
// invariants hold for every one of them.
func TestEncoderWellFormedProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	ids := []string{"dg_1", "dg_2", "dg_3"}
	kinds := []string{agent.PauseKindHITL, agent.PauseKindHITLChoice, agent.PauseKindHITLForm, agent.PauseKindApproval, agent.PauseKindCustom}
	gen := func() agent.StreamEvent {
		id := ids[rng.Intn(len(ids))]
		switch rng.Intn(11) {
		case 0:
			return agent.StreamEvent{Type: agent.EventMeta, MessageID: "m", Provider: "p"}
		case 1, 2, 3:
			deltas := []string{"", "a", "bc\n", " "}
			return agent.StreamEvent{Type: agent.EventText, Delta: deltas[rng.Intn(len(deltas))]}
		case 4:
			return agent.StreamEvent{Type: agent.EventToolCall, Name: "tool", ToolUseID: id, Input: raw(`{"x":1}`)}
		case 5:
			return agent.StreamEvent{Type: agent.EventToolResult, Name: "tool", ToolUseID: id, Output: `{"ui_payload":{"k":1}}`}
		case 6:
			return agent.StreamEvent{Type: agent.EventGenUI, ToolUseID: id, GenUI: raw(`{"k":1}`)}
		case 7:
			return agent.StreamEvent{Type: agent.EventUsage, InputTokens: rng.Intn(100)}
		case 8:
			return agent.StreamEvent{Type: agent.EventTurn}
		case 9:
			k := kinds[rng.Intn(len(kinds))]
			ev := agent.StreamEvent{Type: agent.EventWaitingForUser, Kind: k, ToolUseID: id, Message: "why"}
			if k == agent.PauseKindApproval || k == agent.PauseKindCustom {
				ev.Name = "tool"
			}
			return ev
		default:
			if rng.Intn(2) == 0 {
				return agent.StreamEvent{Type: agent.EventDone}
			}
			return agent.StreamEvent{Type: agent.EventError, Message: "boom"}
		}
	}
	for i := 0; i < 300; i++ {
		n := 1 + rng.Intn(30)
		seq := make([]agent.StreamEvent, n)
		for j := range seq {
			seq[j] = gen()
		}
		events := drive(NewEncoder(EncoderOptions{ThreadID: "t", NewID: counter("id_")}), seq)
		checkWellFormed(t, events)
		if t.Failed() {
			t.Fatalf("sequence %d: %+v", i, seq)
		}
	}
}
