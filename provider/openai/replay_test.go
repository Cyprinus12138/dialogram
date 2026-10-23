package openai

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/internal/loop"
	"github.com/Cyprinus12138/dialogram/provider/anthropic"
	"github.com/Cyprinus12138/dialogram/provider/internal/replay"
	"github.com/Cyprinus12138/dialogram/tool"
)

// The replay scenarios drive the real adapter against hand-written copies
// of the Chat Completions wire format in testdata/ — request building,
// chunk and JSON parsing, tool dispatch, the follow-up request — with no
// network and no key. Each scenario runs on both paths where both exist.

const (
	replyHello   = "Hello! How can I help you today?"
	replyWeather = "It is 27°C and sunny in Lisbon."
	replyBoth    = "Lisbon is 27°C and sunny; the cheapest flight to Madrid is €89."
	replySoft    = "The weather service has no data for that city right now, so I can't give you a reading."
	replyImage   = "The image shows a single red square on a white background."
	askWeather   = "weather in lisbon?"
)

func replayProvider(t *testing.T, srv *replay.Server, reg *tool.Registry) *Provider {
	t.Helper()
	return New(Options{APIKey: "test-key", Model: "gpt-4o-mini", BaseURL: srv.URL, Registry: reg, Config: replay.Config()})
}

func replayInput(text string) agent.ChatInput {
	return agent.ChatInput{MemoryContext: replay.Memory, Messages: []agent.ChatMessage{{Role: "user", Content: text}}}
}

// checkOpeningRequest asserts the shape every scenario's first request
// shares: the bearer header, model and max_tokens, stream_options only on
// the streaming path, the composed system prompt as the first message, the
// user text as plain string content, and every tool as a function spec.
func checkOpeningRequest(t *testing.T, srv *replay.Server, stream bool, userText string) {
	t.Helper()
	if got := srv.Header(0).Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q", got)
	}
	req := srv.Request(0)
	if req["model"] != "gpt-4o-mini" || req["max_tokens"] == nil {
		t.Errorf("model = %v max_tokens = %v", req["model"], req["max_tokens"])
	}
	_, hasStream := req["stream"]
	_, hasOpts := req["stream_options"]
	if hasStream != stream || hasOpts != stream || (stream && replay.Get(req, "stream_options", "include_usage") != true) {
		t.Errorf("stream = %v stream_options = %v, want both %v", hasStream, req["stream_options"], stream)
	}
	if replay.Len(req, "messages") != 2 || replay.Str(req, "messages", 0, "role") != "system" ||
		replay.Get(req, "messages", 0, "content") != replay.SystemPrompt+loop.MemorySeparator+replay.Memory {
		t.Errorf("system message = %v", replay.Get(req, "messages", 0))
	}
	if replay.Str(req, "messages", 1, "role") != "user" || replay.Get(req, "messages", 1, "content") != userText {
		t.Errorf("user message = %v", replay.Get(req, "messages", 1))
	}
	n := replay.Len(req, "tools")
	if n != 5 || replay.Str(req, "tools", 0, "type") != "function" ||
		replay.Str(req, "tools", 0, "function", "name") != "get_weather" ||
		replay.Str(req, "tools", 0, "function", "parameters", "type") != "object" ||
		replay.Str(req, "tools", n-1, "function", "name") != tool.HITLFormToolName {
		t.Errorf("tools = %v", req["tools"])
	}
}

// checkToolTurns asserts the IR of a one-tool step. The tool_call fixture
// carries no text, so the assistant turn is a single tool_use block under
// a neutral id; the tool turn is one tool_result paired by that id.
func checkToolTurns(t *testing.T, asst, res agent.Turn) string {
	t.Helper()
	if asst.Role != agent.RoleAssistant || len(asst.Blocks) != 1 || asst.Blocks[0].Type != agent.BlockToolUse {
		t.Fatalf("assistant turn = %+v", asst)
	}
	tu := asst.Blocks[0]
	if !strings.HasPrefix(tu.ToolUseID, agent.ToolUseIDPrefix) || tu.ToolName != "get_weather" ||
		replay.Str(replay.Decode(t, tu.ToolInput), "city") != "Lisbon" {
		t.Errorf("tool_use block = %+v", tu)
	}
	if res.Role != agent.RoleTool || len(res.Blocks) != 1 || res.Blocks[0].Type != agent.BlockToolResult ||
		res.Blocks[0].ToolUseID != tu.ToolUseID || res.Blocks[0].ToolName != "get_weather" {
		t.Errorf("tool turn = %+v", res)
	}
	return tu.ToolUseID
}

// checkToolRoundTrip asserts the second request of a one-tool scenario:
// the assistant message carries tool_calls under the neutral id with
// content omitted (not ""), one role:tool message follows paired by
// tool_call_id, and no native id is anywhere on the wire.
func checkToolRoundTrip(t *testing.T, srv *replay.Server, id, userText string) {
	t.Helper()
	req := srv.Request(1)
	if replay.Len(req, "messages") != 4 || replay.Get(req, "messages", 1, "content") != userText {
		t.Fatalf("messages = %v", req["messages"])
	}
	asst := replay.Get(req, "messages", 2)
	if _, has := asst.(map[string]any)["content"]; has {
		t.Errorf("a tool-only assistant message must omit content: %v", asst)
	}
	if replay.Str(asst, "role") != "assistant" || replay.Len(asst, "tool_calls") != 1 ||
		replay.Str(asst, "tool_calls", 0, "id") != id || replay.Str(asst, "tool_calls", 0, "type") != "function" ||
		replay.Str(asst, "tool_calls", 0, "function", "name") != "get_weather" ||
		replay.Str(replay.Decode(t, []byte(replay.Str(asst, "tool_calls", 0, "function", "arguments"))), "city") != "Lisbon" {
		t.Errorf("assistant message = %v", asst)
	}
	res := replay.Get(req, "messages", 3)
	if replay.Str(res, "role") != "tool" || replay.Str(res, "tool_call_id") != id || replay.Str(res, "name") != "get_weather" {
		t.Errorf("tool message = %v", res)
	}
	if strings.Contains(string(srv.Raw(1)), "call_Native") || !strings.HasPrefix(id, agent.ToolUseIDPrefix) {
		t.Errorf("native id on the wire or neutral id missing: id = %q", id)
	}
}

// toolMessageContent decodes the content of message m in request r.
func toolMessageContent(t *testing.T, srv *replay.Server, r, m int) any {
	t.Helper()
	return replay.Decode(t, []byte(replay.Str(srv.Request(r), "messages", m, "content")))
}

// 1. Text only: usage counted, cached prompt tokens included.
func TestReplay_TextOnly(t *testing.T) {
	t.Run("chat", func(t *testing.T) {
		srv := replay.New(t, replay.File("testdata/text_only.json"))
		out, err := replayProvider(t, srv, replay.Registry(t, nil)).Chat(context.Background(), replayInput("hi"))
		if err != nil {
			t.Fatal(err)
		}
		if out.Reply != replyHello || len(out.ToolCalls) != 0 || len(out.Turns) != 1 || out.Provider != "openai" || out.CacheReadTokens != 1024 {
			t.Fatalf("out = %+v", out)
		}
		checkOpeningRequest(t, srv, false, "hi")
	})
	t.Run("stream", func(t *testing.T) {
		srv := replay.New(t, replay.File("testdata/text_only.sse"))
		var ev replay.Events
		if err := replayProvider(t, srv, replay.Registry(t, nil)).ChatStream(context.Background(), replayInput("hi"), ev.Emit); err != nil {
			t.Fatal(err)
		}
		if ev.Order() != "text,text,turn,usage,done" || ev.Text != replyHello {
			t.Fatalf("events = %s text = %q", ev.Order(), ev.Text)
		}
		if u := ev.Usage; u.InputTokens != 25 || u.OutputTokens != 12 || u.CacheReadTokens != 1024 {
			t.Errorf("usage = %+v", u)
		}
		if len(ev.Turns) != 1 || ev.Turns[0].Role != agent.RoleAssistant || ev.Turns[0].Blocks[0].Text != replyHello {
			t.Errorf("turns = %+v", ev.Turns)
		}
		checkOpeningRequest(t, srv, true, "hi")
	})
}

// 2. One tool call → tool message → final text.
func TestReplay_ToolCall(t *testing.T) {
	t.Run("chat", func(t *testing.T) {
		srv := replay.New(t, replay.File("testdata/tool_call.json"), replay.File("testdata/tool_call_final.json"))
		out, err := replayProvider(t, srv, replay.Registry(t, nil)).Chat(context.Background(), replayInput(askWeather))
		if err != nil {
			t.Fatal(err)
		}
		if out.Reply != replyWeather || len(out.ToolCalls) != 1 || len(out.Turns) != 3 || out.CacheReadTokens != 256 {
			t.Fatalf("out = %+v", out)
		}
		if tc := out.ToolCalls[0]; tc.Name != "get_weather" || replay.Str(replay.Decode(t, tc.Input), "city") != "Lisbon" || !strings.Contains(tc.Output, `"high_c":27`) {
			t.Errorf("trace = %+v", tc)
		}
		id := checkToolTurns(t, out.Turns[0], out.Turns[1])
		checkToolRoundTrip(t, srv, id, askWeather)
		if got := toolMessageContent(t, srv, 1, 3); replay.Get(got, "high_c") != float64(27) {
			t.Errorf("tool message content = %v", got)
		}
	})
	t.Run("stream", func(t *testing.T) {
		srv := replay.New(t, replay.File("testdata/tool_call.sse"), replay.File("testdata/tool_call_final.sse"))
		var ev replay.Events
		if err := replayProvider(t, srv, replay.Registry(t, nil)).ChatStream(context.Background(), replayInput(askWeather), ev.Emit); err != nil {
			t.Fatal(err)
		}
		if want := "turn,tool_call,tool_result,turn,text,text,turn,usage,done"; ev.Order() != want {
			t.Fatalf("events\n got %s\nwant %s", ev.Order(), want)
		}
		if ev.Text != replyWeather {
			t.Errorf("text = %q", ev.Text)
		}
		id := checkToolTurns(t, ev.Turns[0], ev.Turns[1])
		// The arguments were reassembled from two chunks of the same index.
		if c := ev.ToolCalls[0]; c.Name != "get_weather" || c.ToolUseID != id || replay.Str(replay.Decode(t, c.Input), "city") != "Lisbon" {
			t.Errorf("tool_call event = %+v", c)
		}
		if r := ev.ToolResults[0]; r.ToolUseID != id || !strings.Contains(r.Output, `"high_c":27`) {
			t.Errorf("tool_result event = %+v", r)
		}
		if u := ev.Usage; u.InputTokens != 662 || u.OutputTokens != 32 || u.CacheReadTokens != 256 {
			t.Errorf("usage summed over both steps = %+v", u)
		}
		checkToolRoundTrip(t, srv, id, askWeather)
	})
}

// 3. Two parallel tool calls in one assistant turn; results in model order.
func TestReplay_ParallelTools(t *testing.T) {
	const ask = "lisbon weather and flights to madrid"
	check := func(t *testing.T, srv *replay.Server, asst, res agent.Turn) {
		t.Helper()
		if len(asst.Blocks) != 2 || asst.Blocks[0].ToolName != "get_weather" || asst.Blocks[1].ToolName != "search_flights" {
			t.Fatalf("assistant turn = %+v", asst)
		}
		a, b := asst.Blocks[0].ToolUseID, asst.Blocks[1].ToolUseID
		if a == b || len(res.Blocks) != 2 || res.Blocks[0].ToolUseID != a || res.Blocks[1].ToolUseID != b {
			t.Fatalf("results must follow the model's order: %+v", res)
		}
		if !strings.Contains(string(res.Blocks[1].ToolResult), "runtime_state") {
			t.Error("the persisted result must keep the full envelope")
		}
		req := srv.Request(1)
		if replay.Len(req, "messages") != 5 ||
			replay.Str(req, "messages", 2, "tool_calls", 0, "id") != a || replay.Str(req, "messages", 2, "tool_calls", 1, "id") != b ||
			replay.Str(req, "messages", 3, "tool_call_id") != a || replay.Str(req, "messages", 4, "tool_call_id") != b ||
			replay.Str(req, "messages", 4, "name") != "search_flights" {
			t.Errorf("second request pairing = %v", req["messages"])
		}
		if flights := toolMessageContent(t, srv, 1, 4); replay.Get(flights, "cheapest_eur") != float64(89) || replay.Get(flights, "runtime_state") != nil {
			t.Errorf("only model_observation may reach the wire: %v", flights)
		}
	}
	t.Run("chat", func(t *testing.T) {
		srv := replay.New(t, replay.File("testdata/parallel_tools.json"), replay.File("testdata/parallel_tools_final.json"))
		out, err := replayProvider(t, srv, replay.Registry(t, nil)).Chat(context.Background(), replayInput(ask))
		if err != nil {
			t.Fatal(err)
		}
		if out.Reply != replyBoth || len(out.ToolCalls) != 2 || out.ToolCalls[0].Name != "get_weather" || out.ToolCalls[1].Name != "search_flights" {
			t.Fatalf("out = %+v", out)
		}
		check(t, srv, out.Turns[0], out.Turns[1])
	})
	t.Run("stream", func(t *testing.T) {
		srv := replay.New(t, replay.File("testdata/parallel_tools.sse"), replay.File("testdata/parallel_tools_final.sse"))
		var ev replay.Events
		if err := replayProvider(t, srv, replay.Registry(t, nil)).ChatStream(context.Background(), replayInput(ask), ev.Emit); err != nil {
			t.Fatal(err)
		}
		if want := "turn,tool_call,tool_call,tool_result,tool_result,turn,text,turn,usage,done"; ev.Order() != want {
			t.Fatalf("events\n got %s\nwant %s", ev.Order(), want)
		}
		if ev.ToolCalls[0].Name != "get_weather" || ev.ToolCalls[1].Name != "search_flights" ||
			ev.ToolResults[0].Name != "get_weather" || ev.ToolResults[1].Name != "search_flights" || ev.Text != replyBoth {
			t.Errorf("calls %+v results %+v text %q", ev.ToolCalls, ev.ToolResults, ev.Text)
		}
		check(t, srv, ev.Turns[0], ev.Turns[1])
	})
}

// 4. request_user_input pauses the loop: waiting_for_user, no done, one request.
func TestReplay_Pause(t *testing.T) {
	const prompt = "Which dates are you travelling?"
	t.Run("chat", func(t *testing.T) {
		srv := replay.New(t, replay.File("testdata/pause_request_user_input.json"))
		out, err := replayProvider(t, srv, replay.Registry(t, nil)).Chat(context.Background(), replayInput("book a flight"))
		if err != nil {
			t.Fatal(err)
		}
		id, got, ok := agent.FindPendingHITL(out.Turns)
		if !ok || got != prompt || !strings.HasPrefix(id, agent.ToolUseIDPrefix) || len(out.Turns) != 1 || len(out.ToolCalls) != 0 {
			t.Fatalf("pause not persisted: %+v", out)
		}
		if out.Reply != "" || srv.Count() != 1 {
			t.Errorf("reply = %q requests = %d", out.Reply, srv.Count())
		}
	})
	t.Run("stream", func(t *testing.T) {
		srv := replay.New(t, replay.File("testdata/pause_request_user_input.sse"))
		var ev replay.Events
		if err := replayProvider(t, srv, replay.Registry(t, nil)).ChatStream(context.Background(), replayInput("book a flight"), ev.Emit); err != nil {
			t.Fatal(err)
		}
		if ev.Order() != "turn,usage,waiting_for_user" || ev.Done || ev.Pause == nil || srv.Count() != 1 {
			t.Fatalf("events = %s done = %v requests = %d", ev.Order(), ev.Done, srv.Count())
		}
		p := ev.Pause
		if p.Kind != agent.PauseKindHITL || p.Message != prompt || p.Name != "" || p.ToolUseID != ev.Turns[0].Blocks[0].ToolUseID {
			t.Errorf("pause = %+v", p)
		}
	})
}

// 5. A soft tool failure reaches the model as the failure JSON.
func TestReplay_SoftFailure(t *testing.T) {
	soft := tool.Soft(tool.ErrorCodeNotFound, "no weather station for this city")
	checkWire := func(t *testing.T, srv *replay.Server) {
		t.Helper()
		got := toolMessageContent(t, srv, 1, 3)
		if replay.Str(got, "error_code") != tool.ErrorCodeNotFound || replay.Get(got, "retryable") != false || replay.Str(got, "error_message") == "" {
			t.Errorf("failure JSON on the wire = %v", got)
		}
	}
	t.Run("chat", func(t *testing.T) {
		srv := replay.New(t, replay.File("testdata/tool_call.json"), replay.File("testdata/soft_failure_final.json"))
		out, err := replayProvider(t, srv, replay.Registry(t, soft)).Chat(context.Background(), replayInput(askWeather))
		if err != nil {
			t.Fatal(err)
		}
		if out.Reply != replySoft || len(out.ToolCalls) != 1 || agent.ResultIsSuccess(out.ToolCalls[0].Output) {
			t.Fatalf("out = %+v", out)
		}
		checkWire(t, srv)
	})
	t.Run("stream", func(t *testing.T) {
		srv := replay.New(t, replay.File("testdata/tool_call.sse"), replay.File("testdata/soft_failure_final.sse"))
		var ev replay.Events
		if err := replayProvider(t, srv, replay.Registry(t, soft)).ChatStream(context.Background(), replayInput(askWeather), ev.Emit); err != nil {
			t.Fatal(err)
		}
		if ev.Text != replySoft || !strings.Contains(ev.ToolResults[0].Output, tool.ErrorCodeNotFound) || !ev.Done {
			t.Fatalf("text = %q result = %+v", ev.Text, ev.ToolResults)
		}
		checkWire(t, srv)
	})
}

// 6. A 429 before any text is retried with the same body; the stream then
// completes normally. (The non-streaming path has no retry.)
func TestReplay_RetryAfterRateLimit(t *testing.T) {
	replay.ShortBackoff(t)
	srv := replay.New(t, replay.Status(429, "testdata/rate_limited_429.json"), replay.File("testdata/text_only.sse"))
	var ev replay.Events
	if err := replayProvider(t, srv, replay.Registry(t, nil)).ChatStream(context.Background(), replayInput("hi"), ev.Emit); err != nil {
		t.Fatal(err)
	}
	if ev.Text != replyHello || !ev.Done || srv.Count() != 2 {
		t.Fatalf("text = %q done = %v requests = %d", ev.Text, ev.Done, srv.Count())
	}
	if !bytes.Equal(srv.Raw(0), srv.Raw(1)) {
		t.Error("the retry must resend the identical body")
	}
}

// 7. The turns persisted after the tool step on OpenAI resume on Anthropic
// as an assistant tool_use block plus a user message of tool_result blocks.
func TestReplay_ResumeOnAnthropic(t *testing.T) {
	reg := replay.Registry(t, nil)
	srv := replay.New(t, replay.File("testdata/tool_call.json"), replay.File("testdata/tool_call_final.json"))
	out, err := replayProvider(t, srv, reg).Chat(context.Background(), replayInput(askWeather))
	if err != nil {
		t.Fatal(err)
	}
	// What a server persists after the step that called the tool.
	history := append([]agent.Turn{agent.UserTurn(askWeather)}, out.Turns[:2]...)
	id := out.Turns[0].Blocks[0].ToolUseID

	asrv := replay.New(t, replay.File("../anthropic/testdata/text_only.json"))
	ap := anthropic.New(anthropic.Options{APIKey: "test-key", Model: "claude-sonnet-4-6", BaseURL: asrv.URL, Registry: reg, Config: replay.Config()})
	aout, err := ap.Chat(context.Background(), agent.ChatInput{MemoryContext: replay.Memory, Turns: history})
	if err != nil || aout.Reply != replyHello {
		t.Fatalf("resume on anthropic: err = %v out = %+v", err, aout)
	}
	req := asrv.Request(0)
	if replay.Len(req, "messages") != 3 || replay.Get(req, "messages", 0, "content") != askWeather {
		t.Fatalf("messages = %v", req["messages"])
	}
	asst := replay.Get(req, "messages", 1)
	if replay.Str(asst, "role") != "assistant" || replay.Len(asst, "content") != 1 ||
		replay.Str(asst, "content", 0, "type") != "tool_use" || replay.Str(asst, "content", 0, "id") != id ||
		replay.Str(asst, "content", 0, "name") != "get_weather" || replay.Str(asst, "content", 0, "input", "city") != "Lisbon" {
		t.Errorf("assistant message = %v", asst)
	}
	res := replay.Get(req, "messages", 2)
	if replay.Str(res, "role") != "user" || replay.Len(res, "content") != 1 ||
		replay.Str(res, "content", 0, "type") != "tool_result" || replay.Str(res, "content", 0, "tool_use_id") != id ||
		replay.Get(replay.Decode(t, []byte(replay.Str(res, "content", 0, "content"))), "high_c") != float64(27) ||
		replay.Str(res, "content", 0, "cache_control", "type") != "ephemeral" {
		t.Errorf("tool_result message = %v", res)
	}
	if strings.Contains(string(asrv.Raw(0)), "call_Native") {
		t.Error("the OpenAI-native id must not reach Anthropic")
	}
}

// 8. A user turn with text and an image becomes a content array of parts;
// the adapter refuses an image anywhere else before making a request.
func TestReplay_ImageTurn(t *testing.T) {
	const ask = "What is in this image?"
	inline := agent.ChatInput{MemoryContext: replay.Memory, Turns: []agent.Turn{
		agent.UserTurnBlocks(agent.TextBlock(ask), agent.ImageBlock("image/png", replay.PNG)),
	}}
	checkInline := func(t *testing.T, srv *replay.Server) {
		t.Helper()
		msg := replay.Get(srv.Request(0), "messages", 1)
		if replay.Str(msg, "role") != "user" || replay.Len(msg, "content") != 2 ||
			replay.Str(msg, "content", 0, "type") != "text" || replay.Str(msg, "content", 0, "text") != ask ||
			replay.Str(msg, "content", 1, "type") != "image_url" ||
			replay.Str(msg, "content", 1, "image_url", "url") != "data:image/png;base64,"+replay.PNG {
			t.Errorf("user message = %v", msg)
		}
	}
	t.Run("chat", func(t *testing.T) {
		srv := replay.New(t, replay.File("testdata/image_described.json"))
		out, err := replayProvider(t, srv, replay.Registry(t, nil)).Chat(context.Background(), inline)
		if err != nil {
			t.Fatal(err)
		}
		if out.Reply != replyImage || len(out.Turns) != 1 || len(out.Turns[0].Blocks) != 1 {
			t.Fatalf("out = %+v", out)
		}
		checkInline(t, srv)
	})
	t.Run("stream", func(t *testing.T) {
		srv := replay.New(t, replay.File("testdata/image_described.sse"))
		var ev replay.Events
		if err := replayProvider(t, srv, replay.Registry(t, nil)).ChatStream(context.Background(), inline, ev.Emit); err != nil {
			t.Fatal(err)
		}
		if ev.Order() != "text,turn,usage,done" || ev.Text != replyImage {
			t.Fatalf("events = %s text = %q", ev.Order(), ev.Text)
		}
		checkInline(t, srv)
	})
	t.Run("url source keeps block order", func(t *testing.T) {
		srv := replay.New(t, replay.File("testdata/image_described.json"))
		in := agent.ChatInput{Turns: []agent.Turn{agent.UserTurnBlocks(agent.ImageURLBlock("https://example.com/red.png"), agent.TextBlock("Describe it."))}}
		if _, err := replayProvider(t, srv, replay.Registry(t, nil)).Chat(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		msg := replay.Get(srv.Request(0), "messages", 1)
		if replay.Str(msg, "content", 0, "type") != "image_url" || replay.Str(msg, "content", 0, "image_url", "url") != "https://example.com/red.png" ||
			replay.Str(msg, "content", 1, "text") != "Describe it." {
			t.Errorf("user message = %v", msg)
		}
	})
	t.Run("rejected before any request", func(t *testing.T) {
		srv := replay.New(t, replay.File("testdata/image_described.json"))
		p := replayProvider(t, srv, replay.Registry(t, nil))
		bad := agent.ChatInput{Turns: []agent.Turn{
			agent.UserTurn("look"),
			{Role: agent.RoleAssistant, Blocks: []agent.Block{agent.ImageBlock("image/png", replay.PNG)}},
		}}
		if _, err := p.Chat(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "only valid in user turns") {
			t.Errorf("chat err = %v", err)
		}
		if err := p.ChatStream(context.Background(), bad, func(agent.StreamEvent) {}); err == nil || !strings.Contains(err.Error(), "only valid in user turns") {
			t.Errorf("stream err = %v", err)
		}
		if srv.Count() != 0 {
			t.Errorf("requests made = %d, want 0", srv.Count())
		}
	})
}
