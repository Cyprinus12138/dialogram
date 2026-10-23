package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/internal/loop"
	"github.com/Cyprinus12138/dialogram/tool"
)

// ---------- fixtures ----------

var fixtureTurns = []agent.Turn{
	agent.UserTurn("hi"),
	{Role: agent.RoleAssistant, Blocks: []agent.Block{
		{Type: agent.BlockText, Text: "one moment"},
		{Type: agent.BlockToolUse, ToolUseID: "tu_1", ToolName: "get_weather", ToolInput: json.RawMessage(`{"city":"Lisbon"}`)},
	}},
	{Role: agent.RoleTool, Blocks: []agent.Block{
		{Type: agent.BlockToolResult, ToolUseID: "tu_1", ToolName: "get_weather", ToolResult: json.RawMessage(`{"high_c":27}`)},
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

type recordingServer struct {
	mu      sync.Mutex
	bodies  []map[string]any
	scripts []string
	server  *httptest.Server
}

func newRecordingServer(scripts ...string) *recordingServer {
	rs := &recordingServer{scripts: scripts}
	rs.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		rs.mu.Lock()
		idx := len(rs.bodies)
		rs.bodies = append(rs.bodies, body)
		script := rs.scripts[len(rs.scripts)-1]
		if idx < len(rs.scripts) {
			script = rs.scripts[idx]
		}
		rs.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(script))
	}))
	return rs
}

func (rs *recordingServer) close()     { rs.server.Close() }
func (rs *recordingServer) count() int { rs.mu.Lock(); defer rs.mu.Unlock(); return len(rs.bodies) }
func (rs *recordingServer) body(i int) map[string]any {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.bodies[i]
}

func toolCallResp(names ...string) string {
	var calls []string
	for i, n := range names {
		calls = append(calls, `{"id":"call_`+string(rune('a'+i))+`","type":"function","function":{"name":"`+n+`","arguments":"{}"}}`)
	}
	return `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[` + strings.Join(calls, ",") + `]},"finish_reason":"tool_calls"}]}`
}

func finalResp(text string) string {
	return `{"choices":[{"message":{"role":"assistant","content":"` + text + `"},"finish_reason":"stop"}]}`
}

func newProvider(url string, reg *tool.Registry, cfg *agent.Config, ics ...agent.Interceptor) *Provider {
	return New(Options{APIKey: "k", BaseURL: url, Model: "m", Registry: reg, Config: cfg, Interceptors: ics})
}

// ---------- translators ----------

func TestTurnsToMessages_PrependsSystemAndExpandsTools(t *testing.T) {
	msgs := turnsToMessages("SYS", fixtureTurns)
	if len(msgs) != 4 || msgs[0].Role != "system" || msgs[0].Content != "SYS" || msgs[1].Content != "hi" {
		t.Fatalf("msgs = %#v", msgs)
	}
	asst := msgs[2]
	if asst.Content != "one moment" || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "tu_1" || asst.ToolCalls[0].Function.Arguments != `{"city":"Lisbon"}` {
		t.Fatalf("assistant = %#v", asst)
	}
	if msgs[3].Role != "tool" || msgs[3].ToolCallID != "tu_1" || msgs[3].Content != `{"high_c":27}` {
		t.Fatalf("tool msg = %#v", msgs[3])
	}
	// Assistant with only tool_calls has nil content (OpenAI rejects "").
	only := turnsToMessages("", []agent.Turn{{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockToolUse, ToolUseID: "x", ToolName: "t"}}}})
	if only[0].Content != nil || only[0].ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("content should be nil and args {}: %#v", only[0])
	}
	// Multiple tool results expand to separate messages in order; only
	// model_observation reaches the wire.
	multi := turnsToMessages("", []agent.Turn{{Role: agent.RoleTool, Blocks: []agent.Block{
		{Type: agent.BlockToolResult, ToolUseID: "a", ToolName: "t1", ToolResult: json.RawMessage(`{"model_observation":{"x":1},"runtime_state":{"rows":[1,2]}}`)},
		{Type: agent.BlockToolResult, ToolUseID: "b", ToolName: "t2", ToolResult: json.RawMessage(`{"y":2}`)},
	}}})
	if len(multi) != 2 || multi[0].ToolCallID != "a" || multi[1].ToolCallID != "b" || multi[0].Content != `{"x":1}` {
		t.Fatalf("multi = %#v", multi)
	}
}

func TestResponseToTurn_NeutralIDs(t *testing.T) {
	var tc toolCall
	tc.ID = "call_native"
	tc.Function.Name = "get_weather"
	tc.Function.Arguments = `{"city":"Lisbon"}`
	turn := responseToTurn("hello", []toolCall{tc})
	if len(turn.Blocks) != 2 || turn.Blocks[0].Text != "hello" {
		t.Fatalf("turn = %+v", turn)
	}
	b := turn.Blocks[1]
	if b.ToolUseID == "call_native" || !strings.HasPrefix(b.ToolUseID, agent.ToolUseIDPrefix) || string(b.ToolInput) != `{"city":"Lisbon"}` {
		t.Fatalf("tool_use block = %+v", b)
	}
}

func TestUsageParse(t *testing.T) {
	var r response
	if err := json.Unmarshal([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":40,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":320}}}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Usage == nil || r.Usage.PromptTokensDetails == nil || r.Usage.PromptTokensDetails.CachedTokens != 320 {
		t.Fatalf("usage = %+v", r.Usage)
	}
	var r2 response
	_ = json.Unmarshal([]byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":40}}`), &r2)
	if r2.Usage == nil || r2.Usage.PromptTokensDetails != nil {
		t.Fatalf("missing details must stay nil: %+v", r2.Usage)
	}
}

// ---------- loop evals (deterministic, no LLM) ----------

// When the model keeps asking for tools, the loop stops at the profile cap
// and makes exactly one tool-free synthesis call, returning that text.
func TestLoop_StepCapTriggersSynthesis(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		if _, hasTools := body["tools"]; hasTools {
			_, _ = w.Write([]byte(toolCallResp("search_flights")))
			return
		}
		_, _ = w.Write([]byte(finalResp("Here is my best answer from the data so far.")))
	}))
	defer srv.Close()

	cfg := &agent.Config{Profiles: map[string]agent.Profile{"quick": {MaxToolSteps: 4}}}
	p := newProvider(srv.URL, fixtureRegistry(t, map[string]any{"search_flights": map[string]any{"flights": []any{}}}), cfg)
	out, err := p.Chat(context.Background(), agent.ChatInput{Profile: "quick", Messages: []agent.ChatMessage{{Role: "user", Content: "lisbon next week"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 5 {
		t.Fatalf("LLM calls = %d, want 5 (4 tool steps + 1 synthesis)", got)
	}
	if out.Reply != "Here is my best answer from the data so far." || out.ToolSteps() != 5 {
		t.Fatalf("reply = %q steps = %d", out.Reply, out.ToolSteps())
	}
}

// After a tool round the NEXT request carries the assistant tool_calls
// message and a role:tool result message.
func TestLoop_HistoryAssembly(t *testing.T) {
	rs := newRecordingServer(toolCallResp("search_flights"), finalResp("done"))
	defer rs.close()
	p := newProvider(rs.server.URL, fixtureRegistry(t, map[string]any{"search_flights": map[string]any{"flights": []any{map[string]any{"carrier": "SKYLINE"}}}}), nil)
	out, err := p.Chat(context.Background(), agent.ChatInput{Messages: []agent.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil || rs.count() != 2 {
		t.Fatalf("err=%v calls=%d", err, rs.count())
	}
	msgs, _ := rs.body(1)["messages"].([]any)
	var sawToolCall, sawToolResult bool
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "assistant" {
			if _, ok := mm["tool_calls"]; ok {
				sawToolCall = true
			}
		}
		if mm["role"] == "tool" {
			sawToolResult = true
			if c, _ := mm["content"].(string); !strings.Contains(c, "SKYLINE") {
				t.Errorf("tool message content wrong: %q", c)
			}
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Fatalf("second request missing tool_calls (%v) or tool result (%v)", sawToolCall, sawToolResult)
	}
	if len(out.Turns) != 3 || out.Turns[1].Role != agent.RoleTool || len(out.ToolCalls) != 1 {
		t.Fatalf("output turns/trace wrong: %+v", out.Turns)
	}
	if _, hasMax := rs.body(0)["max_tokens"]; !hasMax {
		t.Fatal("non-streaming request must set max_tokens")
	}
}

// A request_user_input tool_use pauses immediately: no tools dispatched,
// no synthesis, the terminal assistant turn returned for persistence.
func TestLoop_HITLPauseStopsLoop(t *testing.T) {
	rs := newRecordingServer(toolCallResp(tool.HITLInputToolName))
	defer rs.close()
	p := newProvider(rs.server.URL, fixtureRegistry(t, map[string]any{"search_flights": map[string]any{}}), nil)
	out, err := p.Chat(context.Background(), agent.ChatInput{Messages: []agent.ChatMessage{{Role: "user", Content: "help"}}})
	if err != nil || rs.count() != 1 {
		t.Fatalf("err=%v calls=%d", err, rs.count())
	}
	if _, _, ok := agent.FindPendingHITL(out.Turns); !ok {
		t.Fatalf("output turns are not paused: %+v", out.Turns)
	}
}

// A mixed turn (pause + real tool) runs the real tool, pairs the pause
// block with a deferral, and continues; a pure pause on the next step
// pauses the loop.
func TestLoop_MixedPauseTurnDefersPause(t *testing.T) {
	rs := newRecordingServer(toolCallResp("search_flights", tool.HITLInputToolName), toolCallResp(tool.HITLInputToolName))
	defer rs.close()
	p := newProvider(rs.server.URL, fixtureRegistry(t, map[string]any{"search_flights": map[string]any{"n": 3}}), nil)
	out, err := p.Chat(context.Background(), agent.ChatInput{Messages: []agent.ChatMessage{{Role: "user", Content: "lisbon"}}})
	if err != nil || rs.count() != 2 {
		t.Fatalf("err=%v calls=%d", err, rs.count())
	}
	msgs, _ := rs.body(1)["messages"].([]any)
	var sawReal, sawDeferral bool
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] != "tool" {
			continue
		}
		c, _ := mm["content"].(string)
		if strings.Contains(c, `"n":3`) {
			sawReal = true
		}
		if strings.Contains(c, loop.PauseCoRequestErrorCode) {
			sawDeferral = true
		}
	}
	if !sawReal || !sawDeferral {
		t.Fatalf("second request must carry the real result (%v) and the deferral (%v)", sawReal, sawDeferral)
	}
	if _, _, ok := agent.FindPendingHITL(out.Turns); !ok || len(out.ToolCalls) != 1 {
		t.Fatalf("expected a final HITL pause with one real trace, got %+v", out.Turns)
	}
}

// ChatInput.Tools narrows the on-the-wire tool envelope.
func TestLoop_ToolsNarrowsEnvelope(t *testing.T) {
	rs := newRecordingServer(
		`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":null}]}` + "\n\n" +
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n")
	defer rs.close()
	p := newProvider(rs.server.URL, fixtureRegistry(t, map[string]any{"search_flights": map[string]any{}, "get_weather": map[string]any{}}), nil)
	var done bool
	err := p.ChatStream(context.Background(), agent.ChatInput{
		Messages: []agent.ChatMessage{{Role: "user", Content: "weather?"}},
		Tools:    []string{"get_weather"},
	}, func(ev agent.StreamEvent) {
		if ev.Type == agent.EventDone {
			done = true
		}
	})
	if err != nil || !done {
		t.Fatalf("err=%v done=%v", err, done)
	}
	specs, _ := rs.body(0)["tools"].([]any)
	if len(specs) != 1 {
		t.Fatalf("want 1 tool on the wire, got %d", len(specs))
	}
	fn, _ := specs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("narrowed tool = %v", fn["name"])
	}
	if _, ok := rs.body(0)["stream_options"]; !ok {
		t.Fatal("stream request must ask for usage chunks")
	}
}

// Every tool_call surfaces up front in model order ahead of any
// tool_result, even with a denied tool in the middle.
func TestChatStream_ToolCallOrderMatchesLLM(t *testing.T) {
	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		if step == 0 {
			step++
			for i, n := range []string{"allow_a", "deny_b", "allow_c"} {
				_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":` + string(rune('0'+i)) + `,"id":"call_x","type":"function","function":{"name":"` + n + `","arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n"))
			}
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" + "data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"done"},"finish_reason":null}]}` + "\n\n" +
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":8}}}` + "\n\n" + "data: [DONE]\n\n"))
	}))
	defer srv.Close()

	reg := fixtureRegistry(t, map[string]any{"allow_a": 1, "deny_b": 2, "allow_c": 3})
	ic := agent.NewApprovalListInterceptor(nil)
	_ = ic
	deny := denyInterceptor{"deny_b"}
	p := newProvider(srv.URL, reg, nil, deny)

	var order []string
	var resultBeforeAllCalls bool
	var usage agent.StreamEvent
	calls := 0
	err := p.ChatStream(context.Background(), agent.ChatInput{Messages: []agent.ChatMessage{{Role: "user", Content: "hi"}}}, func(ev agent.StreamEvent) {
		switch ev.Type {
		case agent.EventToolCall:
			order = append(order, ev.Name)
			calls++
		case agent.EventToolResult:
			if calls < 3 {
				resultBeforeAllCalls = true
			}
			if ev.Name == "deny_b" && !strings.Contains(ev.Output, tool.ErrorCodeDenied) {
				t.Errorf("denied tool must carry DENIED result: %s", ev.Output)
			}
		case agent.EventUsage:
			usage = ev
		}
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if strings.Join(order, ",") != "allow_a,deny_b,allow_c" || resultBeforeAllCalls {
		t.Fatalf("order = %v, resultBeforeAllCalls = %v", order, resultBeforeAllCalls)
	}
	if usage.InputTokens != 10 || usage.CacheReadTokens != 8 {
		t.Fatalf("usage not accumulated: %+v", usage)
	}
}

type denyInterceptor struct{ name string }

func (d denyInterceptor) InspectToolUse(_ context.Context, b agent.Block) agent.InterceptDecision {
	if b.ToolName == d.name {
		return agent.InterceptDecision{Action: agent.InterceptDeny, Reason: "nope"}
	}
	return agent.InterceptDecision{Action: agent.InterceptAllow}
}

// A custom pause tool and an approval interceptor both end the stream with
// a waiting_for_user event and no "done".
func TestChatStream_PauseKinds(t *testing.T) {
	stream := func(name string) string {
		return `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"` + name + `","arguments":"{\"x\":1}"}}]},"finish_reason":null}]}` + "\n\n" +
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" + "data: [DONE]\n\n"
	}
	run := func(t *testing.T, script string, cfg *agent.Config, ics ...agent.Interceptor) agent.StreamEvent {
		rs := newRecordingServer(script)
		defer rs.close()
		p := newProvider(rs.server.URL, fixtureRegistry(t, map[string]any{"send_email": 1, "propose_change": 2}), cfg, ics...)
		var last agent.StreamEvent
		var sawDone bool
		if err := p.ChatStream(context.Background(), agent.ChatInput{Messages: []agent.ChatMessage{{Role: "user", Content: "go"}}}, func(ev agent.StreamEvent) {
			if ev.Type == agent.EventWaitingForUser {
				last = ev
			}
			if ev.Type == agent.EventDone {
				sawDone = true
			}
		}); err != nil {
			t.Fatal(err)
		}
		if sawDone || rs.count() != 1 {
			t.Fatalf("a pause must be terminal: done=%v calls=%d", sawDone, rs.count())
		}
		return last
	}
	ev := run(t, stream("propose_change"), &agent.Config{PauseTools: []string{"propose_change"}})
	if ev.Kind != agent.PauseKindCustom || ev.Name != "propose_change" || string(ev.Input) != `{"x":1}` {
		t.Fatalf("custom pause wrong: %+v", ev)
	}
	ev = run(t, stream("send_email"), nil, agent.NewApprovalListInterceptor([]string{"send_email"}))
	if ev.Kind != agent.PauseKindApproval || ev.Name != "send_email" {
		t.Fatalf("approval pause wrong: %+v", ev)
	}
	ev = run(t, stream(tool.HITLChoiceToolName), nil)
	if ev.Kind != agent.PauseKindHITLChoice {
		t.Fatalf("choice pause wrong: %+v", ev)
	}
}

// A transient 503 before any text is retried silently; a 400 is not.
func TestChatStream_RetryPolicy(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	p := newProvider(srv.URL, tool.NewRegistry(), nil)
	var text string
	if err := p.ChatStream(context.Background(), agent.ChatInput{Messages: []agent.ChatMessage{{Role: "user", Content: "hi"}}}, func(ev agent.StreamEvent) {
		if ev.Type == agent.EventText {
			text += ev.Delta
		}
	}); err != nil || text != "ok" || calls != 2 {
		t.Fatalf("err=%v text=%q calls=%d", err, text, calls)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) }))
	defer bad.Close()
	p = newProvider(bad.URL, tool.NewRegistry(), nil)
	if err := p.ChatStream(context.Background(), agent.ChatInput{Messages: []agent.ChatMessage{{Role: "user", Content: "hi"}}}, func(agent.StreamEvent) {}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("400 must surface, got %v", err)
	}
}
