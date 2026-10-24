package agui_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/agui"
	"github.com/Cyprinus12138/dialogram/provider/openai"
	"github.com/Cyprinus12138/dialogram/tool"
)

// upstream is a fake chat/completions endpoint: it answers the n-th request
// with streams[n] and records every request body.
type upstream struct {
	mu      sync.Mutex
	streams []string
	bodies  []map[string]any
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	u.mu.Lock()
	n := len(u.bodies)
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()
	if n >= len(u.streams) {
		http.Error(w, "unexpected request", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, u.streams[n])
}

// sse renders chat.completion.chunk deltas as an OpenAI stream.
func sse(finish string, deltas ...string) string {
	var b strings.Builder
	for _, d := range deltas {
		fmt.Fprintf(&b, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":null}]}\n\n", d)
	}
	fmt.Fprintf(&b, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n\n", finish)
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func toolCallDelta(name, args string) string {
	a, _ := json.Marshal(args)
	return fmt.Sprintf(`{"role":"assistant","tool_calls":[{"index":0,"id":"call_native","type":"function","function":{"name":%q,"arguments":%s}}]}`, name, a)
}

// nth returns events[i], failing the test when the run produced fewer.
func nth(t *testing.T, events []agui.Event, i int) agui.Event {
	t.Helper()
	if i < 0 || i >= len(events) {
		t.Fatalf("want event %d, the run produced %d", i, len(events))
	}
	return events[i]
}

// readEvents reads an AG-UI response into events.
func readEvents(t *testing.T, resp *http.Response) []agui.Event {
	t.Helper()
	defer resp.Body.Close()
	var out []agui.Event
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev agui.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func postRun(t *testing.T, url string, msgs []json.RawMessage, resume []agui.ResumeEntry, runID string) []agui.Event {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"threadId": "thread-e2e", "runId": runID, "messages": msgs, "resume": resume})
	resp, err := http.Post(url, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return readEvents(t, resp)
}

// endOnInterrupt returns the interrupt a run ended on and the snapshot
// messages, as a client would send them back.
func endOnInterrupt(t *testing.T, events []agui.Event) (agui.Interrupt, []json.RawMessage) {
	t.Helper()
	n := len(events)
	if n < 2 {
		t.Fatalf("run produced %d events", n)
	}
	if events[n-1].Outcome == nil || events[n-1].Outcome.Type != agui.OutcomeInterrupt || events[n-2].Type != agui.EventMessagesSnapshot {
		t.Fatalf("run did not end on an interrupt: %+v", events[n-1])
	}
	var msgs []json.RawMessage
	for _, m := range events[n-2].Messages {
		b, _ := json.Marshal(m)
		msgs = append(msgs, b)
	}
	return events[n-1].Outcome.Interrupts[0], msgs
}

// stack wires a real agent (OpenAI adapter, shared loop, policy
// interceptor, executor) behind the AG-UI handler.
func stack(t *testing.T, up *upstream, booked *int) string {
	t.Helper()
	llm := httptest.NewServer(up)
	t.Cleanup(llm.Close)
	reg := tool.NewRegistry()
	tool.RegisterHITL(reg)
	reg.Register(&tool.Tool{
		Name: "book_table", Description: "Book a table.",
		Schema: tool.Obj(map[string]any{"restaurant": tool.Str("Restaurant"), "time": tool.Str("HH:MM")}, "restaurant", "time"),
		Fn: func(_ context.Context, input json.RawMessage) (any, error) {
			*booked++
			var in struct {
				Restaurant string `json:"restaurant"`
				Time       string `json:"time"`
			}
			_ = json.Unmarshal(input, &in)
			return map[string]any{"booked": true, "reference": "DEMO-1", "time": in.Time}, nil
		},
		Annotations: tool.Annotations{Destructive: true},
	})
	cfg := &agent.Config{Profiles: map[string]agent.Profile{"assistant": {SystemPrompt: "Be brief."}}, DefaultProfile: "assistant"}
	exec := tool.NewLocalExecutor(reg, tool.LocalExecutorOptions{})
	ag := agent.New(openai.New(openai.Options{
		APIKey: "test", BaseURL: llm.URL, Registry: reg, Config: cfg, Executor: exec,
		Interceptors: []agent.Interceptor{agent.NewPolicyInterceptor(reg, agent.Policy{PauseDestructive: true})},
	}), agent.Options{})
	run := func(ctx context.Context, in agui.RunAgentInput, emit agent.EmitFn) error {
		turns, err := agui.MessagesToTurns(in.Messages)
		if err != nil {
			return err
		}
		return ag.ChatStream(ctx, agent.ChatInput{Turns: turns}, emit)
	}
	srv := httptest.NewServer(agui.NewHandler(run, agui.Options{Executor: exec, InterruptKey: []byte("e2e-key")}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// messagesOf returns the "messages" of an upstream request body.
func messagesOf(body map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range body["messages"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

func TestEndToEndApprovalThroughTheRealLoop(t *testing.T) {
	up := &upstream{streams: []string{
		sse("tool_calls", `{"role":"assistant","content":"Booking it."}`, toolCallDelta("book_table", `{"restaurant":"Ramiro","time":"20:00"}`)),
		sse("stop", `{"role":"assistant","content":"Booked: DEMO-1 at 20:00."}`),
	}}
	booked := 0
	url := stack(t, up, &booked)
	user := json.RawMessage(`{"id":"u1","role":"user","content":"Book Ramiro at 20:00."}`)

	first := postRun(t, url, []json.RawMessage{user}, nil, "r1")
	in, history := endOnInterrupt(t, first)
	if in.Reason != agui.ReasonToolCall || !strings.HasPrefix(in.ToolCallID, agent.ToolUseIDPrefix) || booked != 0 {
		t.Fatalf("the booking waits for approval: %+v, booked %d", in, booked)
	}

	second := postRun(t, url, history, []agui.ResumeEntry{{InterruptID: in.ID, Status: agui.ResumeResolved, Payload: json.RawMessage(`{"approved":true}`)}}, "r2")
	if booked != 1 {
		t.Fatalf("approving runs the tool once, ran %d times", booked)
	}
	if nth(t, second, 1).Type != agui.EventToolCallResult || nth(t, second, 1).ToolCallID != in.ToolCallID || !strings.Contains(nth(t, second, 1).Content, "DEMO-1") {
		t.Fatalf("the resumed run opens with the call's result: %+v", nth(t, second, 1))
	}
	if last := nth(t, second, len(second)-1); last.Type != agui.EventRunFinished || last.Outcome != nil {
		t.Fatalf("the resumed run completes: %+v", last)
	}

	if len(up.bodies) != 2 {
		t.Fatalf("upstream saw %d requests", len(up.bodies))
	}
	msgs := messagesOf(up.bodies[1])
	var sawCall, sawResult bool
	for _, m := range msgs {
		if calls, ok := m["tool_calls"].([]any); ok && len(calls) == 1 {
			c := calls[0].(map[string]any)
			sawCall = c["id"] == in.ToolCallID && c["function"].(map[string]any)["name"] == "book_table"
		}
		if m["role"] == "tool" {
			sawResult = m["tool_call_id"] == in.ToolCallID && strings.Contains(m["content"].(string), "DEMO-1")
		}
	}
	if !sawCall || !sawResult {
		t.Errorf("the second model request carries the call and its result: %v", msgs)
	}
}

func TestEndToEndChoiceThroughTheRealLoop(t *testing.T) {
	choice := `{"prompt":"Which city?","options":[{"label":"Lisbon","value":"lis"},{"label":"Porto","value":"opo"}]}`
	up := &upstream{streams: []string{
		sse("tool_calls", toolCallDelta("request_user_choice", choice)),
		sse("stop", `{"role":"assistant","content":"Lisbon it is."}`),
	}}
	booked := 0
	url := stack(t, up, &booked)
	user := json.RawMessage(`{"id":"u1","role":"user","content":"Weather please."}`)

	in, history := endOnInterrupt(t, postRun(t, url, []json.RawMessage{user}, nil, "r1"))
	if in.Reason != agui.ReasonInputRequired || in.Message != "Which city?" {
		t.Fatalf("interrupt: %+v", in)
	}
	second := postRun(t, url, history, []agui.ResumeEntry{{InterruptID: in.ID, Status: agui.ResumeResolved, Payload: json.RawMessage(`{"selected":["lis"]}`)}}, "r2")
	if nth(t, second, 1).Type != agui.EventToolCallResult || nth(t, second, 1).Content != `{"selected":["lis"]}` {
		t.Fatalf("the answer is the pause tool's result: %+v", nth(t, second, 1))
	}
	var sawResult bool
	for _, m := range messagesOf(up.bodies[1]) {
		sawResult = sawResult || (m["role"] == "tool" && m["tool_call_id"] == in.ToolCallID && m["content"] == `{"selected":["lis"]}`)
	}
	if !sawResult {
		t.Errorf("the model reads the user's pick: %v", up.bodies[1]["messages"])
	}
}
