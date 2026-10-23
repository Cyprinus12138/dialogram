package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/internal/loop"
	"github.com/Cyprinus12138/dialogram/tool"
)

var fixtureTurns = []agent.Turn{
	agent.UserTurn("hi"),
	{Role: agent.RoleAssistant, Blocks: []agent.Block{
		{Type: agent.BlockText, Text: "one moment"},
		{Type: agent.BlockToolUse, ToolUseID: "tu_1", ToolName: "get_weather", ToolInput: json.RawMessage(`{"city":"Lisbon"}`)},
	}},
	{Role: agent.RoleTool, Blocks: []agent.Block{
		{Type: agent.BlockToolResult, ToolUseID: "tu_1", ToolName: "get_weather", ToolResult: json.RawMessage(`{"model_observation":{"high_c":27},"runtime_state":{"rows":1}}`)},
	}},
}

func fixtureRegistry(t *testing.T, fixtures map[string]any) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry()
	for name, out := range fixtures {
		out := out
		reg.Register(&tool.Tool{Name: name, Description: "fixture " + name, Schema: tool.Obj(nil),
			Fn: func(context.Context, json.RawMessage) (any, error) { return out, nil }})
	}
	tool.RegisterHITL(reg)
	return reg
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader([]byte(body))), Header: make(http.Header)}
}

// ---------- translators ----------

func TestTurnsToMessages(t *testing.T) {
	msgs := turnsToMessages(fixtureTurns)
	if len(msgs) != 3 {
		t.Fatalf("want 3 msgs, got %d", len(msgs))
	}
	if s, ok := msgs[0].Content.(string); !ok || s != "hi" {
		t.Fatalf("single-text user turn must collapse to string content, got %#v", msgs[0].Content)
	}
	content, ok := msgs[1].Content.([]map[string]any)
	if !ok || len(content) != 2 || content[0]["text"] != "one moment" || content[1]["type"] != "tool_use" || content[1]["id"] != "tu_1" {
		t.Fatalf("assistant content = %#v", msgs[1].Content)
	}
	if input, ok := content[1]["input"].(map[string]any); !ok || input["city"] != "Lisbon" {
		t.Fatalf("tool_use input must be decoded JSON: %#v", content[1]["input"])
	}
	results := msgs[2].Content.([]map[string]any)
	if msgs[2].Role != "user" || results[0]["type"] != "tool_result" || results[0]["tool_use_id"] != "tu_1" {
		t.Fatalf("tool turn must become a user message with tool_result blocks: %#v", msgs[2])
	}
	if results[0]["content"] != `{"high_c":27}` {
		t.Fatalf("only model_observation may reach the wire, got %v", results[0]["content"])
	}
	// Translators must not mutate the shared fixture.
	if string(fixtureTurns[1].Blocks[1].ToolInput) != `{"city":"Lisbon"}` {
		t.Fatal("translator mutated fixture")
	}
}

func TestResponseToTurn_NeutralIDs(t *testing.T) {
	turn := responseToTurn(response{Content: []contentBlock{
		{Type: "text", Text: "hello"},
		{Type: "tool_use", ID: "toolu_native", Name: "get_weather", Input: json.RawMessage(`{"city":"Lisbon"}`)},
		{Type: "tool_use", ID: "toolu_2", Name: "now"},
	}})
	if len(turn.Blocks) != 3 || turn.Blocks[0].Text != "hello" {
		t.Fatalf("turn = %+v", turn)
	}
	b := turn.Blocks[1]
	if b.ToolUseID == "toolu_native" || !strings.HasPrefix(b.ToolUseID, agent.ToolUseIDPrefix) || string(b.ToolInput) != `{"city":"Lisbon"}` {
		t.Fatalf("tool_use block = %+v", b)
	}
	if string(turn.Blocks[2].ToolInput) != "{}" {
		t.Fatalf("empty input must become {}: %s", turn.Blocks[2].ToolInput)
	}
}

func TestUsageParse(t *testing.T) {
	var r response
	if err := json.Unmarshal([]byte(`{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":12,"output_tokens":7,"cache_read_input_tokens":1024,"cache_creation_input_tokens":256}}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Usage == nil || r.Usage.InputTokens != 12 || r.Usage.CacheReadInputTokens != 1024 || r.Usage.CacheCreationInputTokens != 256 {
		t.Fatalf("usage = %+v", r.Usage)
	}
	var r2 response
	_ = json.Unmarshal([]byte(`{"content":[{"type":"text","text":"hi"}]}`), &r2)
	if r2.Usage != nil {
		t.Fatal("absent usage must stay nil")
	}
}

// ---------- cache helpers ----------

func TestCachedSystem(t *testing.T) {
	blocks := cachedSystem("STABLE", "")
	if len(blocks) != 1 || blocks[0]["text"] != "STABLE" || blocks[0]["cache_control"] == nil {
		t.Fatalf("no-memory blocks = %v", blocks)
	}
	blocks = cachedSystem("STABLE", "MEM")
	if len(blocks) != 2 || blocks[0]["cache_control"] == nil {
		t.Fatalf("with-memory blocks = %v", blocks)
	}
	if _, ok := blocks[1]["cache_control"]; ok || blocks[1]["text"] != loop.MemorySeparator+"MEM" {
		t.Fatalf("volatile block must be un-cached with the separator join: %v", blocks[1])
	}
	if got := cachedSystem("", "MEM"); len(got) != 1 || got[0]["text"] != "MEM" {
		t.Fatalf("empty stable must emit just the volatile block: %v", got)
	}
	if got := cachedSystem("", ""); len(got) != 0 {
		t.Fatal("no prompt must emit no system blocks")
	}
}

func TestWithMessageCacheBreakpoint(t *testing.T) {
	in := []message{{Role: "user", Content: "hello"}}
	out := withMessageCacheBreakpoint(in)
	if in[0].Content != "hello" {
		t.Error("input message mutated")
	}
	blocks, ok := out[0].Content.([]map[string]any)
	if !ok || len(blocks) != 1 || blocks[0]["text"] != "hello" || blocks[0]["cache_control"] == nil {
		t.Fatalf("string content not promoted + marked: %#v", out[0].Content)
	}

	in = []message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: []map[string]any{{"type": "text", "text": "a"}, {"type": "text", "text": "b"}}},
	}
	out = withMessageCacheBreakpoint(in)
	if s, ok := out[0].Content.(string); !ok || s != "first" {
		t.Error("earlier message must stay untouched")
	}
	last := out[1].Content.([]map[string]any)
	if _, marked := last[0]["cache_control"]; marked || last[1]["cache_control"] == nil {
		t.Error("only the last block of the last message may be marked")
	}
	if _, marked := in[1].Content.([]map[string]any)[1]["cache_control"]; marked {
		t.Error("input inner map mutated")
	}
	if got := withMessageCacheBreakpoint(nil); got != nil {
		t.Error("nil in → nil out")
	}
	empty := withMessageCacheBreakpoint([]message{{Role: "user", Content: []map[string]any{}}})
	if len(empty[0].Content.([]map[string]any)) != 0 {
		t.Error("empty structured content must stay empty")
	}
	odd := withMessageCacheBreakpoint([]message{{Role: "user", Content: 42}})
	if odd[0].Content != 42 {
		t.Error("unknown shape must be left untouched")
	}
	spec := []map[string]any{{"name": "a"}, {"name": "b"}}
	ct := cachedTools(spec)
	if ct[1]["cache_control"] == nil || spec[1]["cache_control"] != nil || ct[0]["cache_control"] != nil {
		t.Error("cachedTools must mark only the last entry of a copy")
	}
}

// The assembled /v1/messages body wires the cache breakpoints correctly:
// on the STABLE system block (not the volatile one), on the last tool, and
// on the last message block.
func TestChat_CacheControlPlacement(t *testing.T) {
	var captured map[string]any
	cfg := &agent.Config{Profiles: map[string]agent.Profile{"assistant": {SystemPrompt: "You are a travel helper."}}, DefaultProfile: "assistant"}
	p := New(Options{APIKey: "k", Model: "claude-test", Registry: fixtureRegistry(t, map[string]any{"get_weather": map[string]any{}}), Config: cfg})
	p.SetHTTPClient(&http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured)
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("auth headers missing")
		}
		return jsonResp(`{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":4,"cache_creation_input_tokens":1}}`), nil
	})})

	out, err := p.Chat(context.Background(), agent.ChatInput{
		MemoryContext: "## Memory\n- prefers window seats\n",
		Messages:      []agent.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out.Reply != "hi" || out.CacheReadTokens != 4 || out.CacheCreationTokens != 1 {
		t.Fatalf("output = %+v", out)
	}
	sys, _ := captured["system"].([]any)
	if len(sys) != 2 {
		t.Fatalf("want 2 system blocks, got %d", len(sys))
	}
	if _, ok := sys[0].(map[string]any)["cache_control"]; !ok {
		t.Error("stable system block must carry cache_control")
	}
	if _, ok := sys[1].(map[string]any)["cache_control"]; ok {
		t.Error("volatile block must NOT carry cache_control")
	}
	toolSpecs, _ := captured["tools"].([]any)
	if len(toolSpecs) == 0 || toolSpecs[len(toolSpecs)-1].(map[string]any)["cache_control"] == nil {
		t.Error("last tool must carry cache_control")
	}
	msgs, _ := captured["messages"].([]any)
	blocks, _ := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	if len(blocks) == 0 || blocks[len(blocks)-1].(map[string]any)["cache_control"] == nil {
		t.Error("last message block must carry the cache breakpoint")
	}
	if captured["max_tokens"] == nil {
		t.Error("max_tokens must be set")
	}
}

// Non-streaming tool loop: tool_use → tool runs → results threaded → final.
func TestChat_ToolLoop(t *testing.T) {
	var bodies []map[string]any
	scripts := []string{
		`{"content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Lisbon"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":2}}`,
		`{"content":[{"type":"text","text":"It is 27 degrees."}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":4}}`,
	}
	p := New(Options{APIKey: "k", Registry: fixtureRegistry(t, map[string]any{"get_weather": &tool.Envelope{ModelObservation: map[string]any{"high_c": 27}, RuntimeState: []int{1, 2}}})})
	p.SetHTTPClient(&http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		return jsonResp(scripts[len(bodies)-1]), nil
	})})
	out, err := p.Chat(context.Background(), agent.ChatInput{Messages: []agent.ChatMessage{{Role: "user", Content: "weather in lisbon?"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out.Reply != "It is 27 degrees." || len(out.ToolCalls) != 1 || len(out.Turns) != 3 {
		t.Fatalf("out = %+v", out)
	}
	msgs, _ := bodies[1]["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	results := last["content"].([]any)[0].(map[string]any)
	if results["type"] != "tool_result" || !strings.Contains(results["content"].(string), "27") || strings.Contains(results["content"].(string), "runtime_state") {
		t.Fatalf("tool_result on the wire wrong: %#v", results)
	}
	if !strings.Contains(out.ToolCalls[0].Output, "runtime_state") {
		t.Fatal("the trace must keep the full envelope")
	}
}

// Streaming: text deltas, a tool_use assembled from input_json_delta, the
// tool round, and a final step with usage then done.
func TestChatStream_Basic(t *testing.T) {
	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		if step == 0 {
			step++
			_, _ = w.Write([]byte("event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":11,\"cache_read_input_tokens\":6}}}\n\n" +
				"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
				"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Let me \"}}\n\n" +
				"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"check.\"}}\n\n" +
				"event: content_block_start\ndata: {\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\"}}\n\n" +
				"event: content_block_delta\ndata: {\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\"}}\n\n" +
				"event: content_block_delta\ndata: {\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"Lisbon\\\"}\"}}\n\n" +
				"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\n" +
				"event: message_stop\ndata: {}\n\n"))
			return
		}
		_, _ = w.Write([]byte("event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":20}}}\n\n" +
			"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
			"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"27 degrees.\"}}\n\n" +
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n"))
	}))
	defer srv.Close()

	p := New(Options{APIKey: "k", BaseURL: srv.URL, Registry: fixtureRegistry(t, map[string]any{"get_weather": map[string]any{"high_c": 27}})})
	var types []string
	var text string
	var toolInput string
	var usage agent.StreamEvent
	var turns []agent.Turn
	err := p.ChatStream(context.Background(), agent.ChatInput{Messages: []agent.ChatMessage{{Role: "user", Content: "lisbon?"}}}, func(ev agent.StreamEvent) {
		types = append(types, ev.Type)
		switch ev.Type {
		case agent.EventText:
			text += ev.Delta
		case agent.EventToolCall:
			toolInput = string(ev.Input)
		case agent.EventUsage:
			usage = ev
		case agent.EventTurn:
			turns = append(turns, *ev.Turn)
		}
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	want := "text,text,turn,tool_call,tool_result,turn,text,turn,usage,done"
	if got := strings.Join(types, ","); got != want {
		t.Fatalf("event order\n got %s\nwant %s", got, want)
	}
	if text != "Let me check.27 degrees." || toolInput != `{"city":"Lisbon"}` {
		t.Fatalf("text=%q toolInput=%q", text, toolInput)
	}
	if usage.InputTokens != 31 || usage.OutputTokens != 7 || usage.CacheReadTokens != 6 {
		t.Fatalf("usage = %+v", usage)
	}
	if len(turns) != 3 || turns[1].Role != agent.RoleTool || !strings.HasPrefix(turns[0].Blocks[1].ToolUseID, agent.ToolUseIDPrefix) {
		t.Fatalf("turns = %+v", turns)
	}
}

// A HITL tool_use on the stream ends with waiting_for_user, not done.
func TestChatStream_HITLPause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t\",\"name\":\"" + tool.HITLInputToolName + "\"}}\n\n" +
			"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"prompt\\\":\\\"Which dates?\\\"}\"}}\n\n" +
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":4}}\n\n"))
	}))
	defer srv.Close()
	p := New(Options{APIKey: "k", BaseURL: srv.URL, Registry: fixtureRegistry(t, nil)})
	var pause agent.StreamEvent
	var done bool
	if err := p.ChatStream(context.Background(), agent.ChatInput{Messages: []agent.ChatMessage{{Role: "user", Content: "book it"}}}, func(ev agent.StreamEvent) {
		if ev.Type == agent.EventWaitingForUser {
			pause = ev
		}
		if ev.Type == agent.EventDone {
			done = true
		}
	}); err != nil {
		t.Fatal(err)
	}
	if done || pause.Kind != agent.PauseKindHITL || pause.Message != "Which dates?" || pause.ToolUseID == "" {
		t.Fatalf("pause wrong: done=%v %+v", done, pause)
	}
}

func TestNew_Defaults(t *testing.T) {
	p := New(Options{APIKey: "k"})
	if p.Model() != DefaultModel || p.Name() != "anthropic" || p.baseURL != "https://api.anthropic.com" || p.maxTokens != loop.DefaultMaxTokens {
		t.Fatalf("defaults wrong: %+v", p)
	}
}
