package agui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

func TestTurnsMessagesRoundTrip(t *testing.T) {
	turns := []agent.Turn{
		agent.UserTurn("weather in Lisbon?"),
		{Role: agent.RoleAssistant, Blocks: []agent.Block{
			{Type: agent.BlockText, Text: "Checking."},
			{Type: agent.BlockToolUse, ToolUseID: "dg_1", ToolName: "get_weather", ToolInput: raw(`{"city":"Lisbon"}`)},
			{Type: agent.BlockToolUse, ToolUseID: "dg_2", ToolName: "now", ToolInput: raw(`{}`)},
		}},
		{Role: agent.RoleTool, Blocks: []agent.Block{
			{Type: agent.BlockToolResult, ToolUseID: "dg_1", ToolName: "get_weather", ToolResult: raw(`{"high_c":24}`)},
			{Type: agent.BlockToolResult, ToolUseID: "dg_2", ToolName: "now", ToolResult: raw(`"noon"`)},
		}},
		{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockText, Text: "24 degrees at noon."}}},
	}
	msgs := TurnsToMessages(turns)
	wantRoles := []string{RoleUser, RoleAssistant, RoleTool, RoleTool, RoleAssistant}
	if len(msgs) != len(wantRoles) {
		t.Fatalf("want %d messages, got %d: %+v", len(wantRoles), len(msgs), msgs)
	}
	for i, m := range msgs {
		if m.Role != wantRoles[i] || m.ID != "msg_"+string(rune('1'+i)) {
			t.Errorf("message %d: role %q id %q", i, m.Role, m.ID)
		}
	}
	if a := msgs[1]; a.Content != "Checking." || len(a.ToolCalls) != 2 || a.ToolCalls[0].Type != ToolCallTypeFunction ||
		a.ToolCalls[0].Function.Name != "get_weather" || a.ToolCalls[0].Function.Arguments != `{"city":"Lisbon"}` {
		t.Errorf("assistant message: %+v", a)
	}
	if tm := msgs[3]; tm.ToolCallID != "dg_2" || tm.Content != `"noon"` {
		t.Errorf("tool message: %+v", tm)
	}
	back, err := MessagesToTurns(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, turns) {
		t.Fatalf("round trip changed the turns:\n got %+v\nwant %+v", back, turns)
	}
	// And the messages survive a wire round trip.
	b, _ := json.Marshal(msgs)
	var decoded []Message
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, msgs) {
		t.Fatalf("JSON round trip changed the messages:\n got %+v\nwant %+v", decoded, msgs)
	}
}

// hardFailure is the result a tool message with only an error becomes.
func hardFailure(msg string) json.RawMessage {
	b, _ := json.Marshal(tool.Soft(tool.ErrorCodeHardFailure, msg))
	return b
}

func TestMessagesToTurnsFromClientJSON(t *testing.T) {
	const body = `[
	  {"id":"s1","role":"system","content":"Be brief."},
	  {"id":"d1","role":"developer","content":"Prefer metric units."},
	  {"id":"u1","role":"user","content":[{"type":"text","text":"hi"},{"type":"image","url":"x"},{"type":"text","text":"there"}]},
	  {"id":"a1","role":"assistant","toolCalls":[
	    {"id":"c1","type":"function","function":{"name":"ping","arguments":""}},
	    {"id":"c2","type":"function","function":{"name":"pong","arguments":"{\"n\":1}"}}]},
	  {"id":"t1","role":"tool","toolCallId":"c1","content":"plain text"},
	  {"id":"t2","role":"tool","toolCallId":"c2","content":"","error":"boom"},
	  {"id":"x1","role":"activity","activityType":"a","content":{}},
	  {"id":"a2","role":"assistant","content":"done"}
	]`
	var msgs []Message
	if err := json.Unmarshal([]byte(body), &msgs); err != nil {
		t.Fatal(err)
	}
	turns, err := MessagesToTurns(msgs)
	if err != nil {
		t.Fatal(err)
	}
	want := []agent.Turn{
		agent.UserTurn("hi\nthere"),
		{Role: agent.RoleAssistant, Blocks: []agent.Block{
			{Type: agent.BlockToolUse, ToolUseID: "c1", ToolName: "ping", ToolInput: raw(`{}`)},
			{Type: agent.BlockToolUse, ToolUseID: "c2", ToolName: "pong", ToolInput: raw(`{"n":1}`)},
		}},
		{Role: agent.RoleTool, Blocks: []agent.Block{
			{Type: agent.BlockToolResult, ToolUseID: "c1", ToolName: "ping", ToolResult: raw(`"plain text"`)},
			{Type: agent.BlockToolResult, ToolUseID: "c2", ToolName: "pong", ToolResult: hardFailure("boom")},
		}},
		{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockText, Text: "done"}}},
	}
	if !reflect.DeepEqual(turns, want) {
		t.Fatalf("turns:\n got %+v\nwant %+v", turns, want)
	}
	if got := SystemText(msgs); got != "Be brief.\n\nPrefer metric units." {
		t.Fatalf("SystemText = %q", got)
	}
}

func TestMessagesToTurnsErrors(t *testing.T) {
	cases := map[string][]Message{
		"tool without call": {{ID: "t", Role: RoleTool, ToolCallID: "nope", Content: "x"}},
		"tool without id":   {{ID: "t", Role: RoleTool, Content: "x"}},
		"call without name": {{ID: "a", Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Type: ToolCallTypeFunction}}}},
		"bad arguments":     {{ID: "a", Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Type: ToolCallTypeFunction, Function: FunctionCall{Name: "f", Arguments: "{"}}}}},
	}
	for name, msgs := range cases {
		if _, err := MessagesToTurns(msgs); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	var m Message
	if err := json.Unmarshal([]byte(`{"id":"u","role":"user","content":42}`), &m); err == nil || !strings.Contains(err.Error(), "content") {
		t.Errorf("numeric content should be rejected, got %v", err)
	}
	if err := json.Unmarshal([]byte(`{"id":"a","role":"assistant","content":null}`), &m); err != nil || m.Content != "" {
		t.Errorf("null content should decode as empty, got %v / %q", err, m.Content)
	}
}

func TestTurnsToMessagesSkipsEmptyAndNormalisesRole(t *testing.T) {
	msgs := TurnsToMessages([]agent.Turn{
		{Role: agent.RoleAssistant},
		{Role: "weird", Blocks: []agent.Block{{Type: agent.BlockText, Text: "x"}}},
		{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockToolUse, ToolUseID: "c", ToolName: "f"}}},
	})
	if len(msgs) != 2 || msgs[0].Role != RoleUser || msgs[0].ID != "msg_1" || msgs[1].ID != "msg_2" ||
		msgs[1].ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("messages: %+v", msgs)
	}
}
