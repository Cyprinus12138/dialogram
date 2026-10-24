package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

func TestRunner_HandleTools(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(sleepyTool("get_weather", 0))
	tool.RegisterHITL(reg)
	r := Runner{Cfg: &agent.Config{PauseTools: []string{"propose_change"}}, Reg: reg}

	// Mixed turn: real tool runs, pause block gets a deferral, nothing pauses.
	mixed := agent.Turn{Role: agent.RoleAssistant, Blocks: []agent.Block{
		{Type: agent.BlockToolUse, ToolUseID: "r", ToolName: "get_weather", ToolInput: json.RawMessage(`{}`)},
		{Type: agent.BlockToolUse, ToolUseID: "p", ToolName: tool.HITLInputToolName, ToolInput: json.RawMessage(`{"prompt":"?"}`)},
	}}
	var evs []agent.StreamEvent
	emit := func(ev agent.StreamEvent) { evs = append(evs, ev) }
	tt, traces, pause := r.HandleTools(context.Background(), mixed, emit)
	if pause != nil || len(traces) != 1 || len(tt.Blocks) != 2 {
		t.Fatalf("mixed: pause=%v traces=%d blocks=%d", pause, len(traces), len(tt.Blocks))
	}
	if !strings.Contains(string(tt.Blocks[1].ToolResult), PauseCoRequestErrorCode) {
		t.Fatalf("pause block not deferred: %s", tt.Blocks[1].ToolResult)
	}
	if len(evs) != 2 || evs[0].Type != agent.EventToolCall || evs[1].Type != agent.EventToolResult {
		t.Fatalf("mixed events wrong: %+v", evs)
	}

	// Pure pause turn: no dispatch, pause event returned.
	pure := agent.Turn{Role: agent.RoleAssistant, Blocks: []agent.Block{
		{Type: agent.BlockToolUse, ToolUseID: "p", ToolName: "propose_change", ToolInput: json.RawMessage(`{"x":1}`)},
	}}
	evs = nil
	_, _, pause = r.HandleTools(context.Background(), pure, emit)
	if pause == nil || pause.Kind != agent.PauseKindCustom || pause.Name != "propose_change" || len(evs) != 0 {
		t.Fatalf("pure pause wrong: %+v evs=%d", pause, len(evs))
	}

	// Regular turn with an approval interceptor.
	r.Interceptors = []agent.Interceptor{agent.NewApprovalListInterceptor([]string{"get_weather"})}
	regular := agent.Turn{Role: agent.RoleAssistant, Blocks: []agent.Block{
		{Type: agent.BlockToolUse, ToolUseID: "r", ToolName: "get_weather", ToolInput: json.RawMessage(`{"city":"Lisbon"}`)},
	}}
	evs = nil
	_, _, pause = r.HandleTools(context.Background(), regular, emit)
	if pause == nil || pause.Kind != agent.PauseKindApproval || string(pause.Input) != `{"city":"Lisbon"}` {
		t.Fatalf("approval pause wrong: %+v", pause)
	}
	if len(evs) != 1 || evs[0].Type != agent.EventToolCall {
		t.Fatalf("approval path must still surface the tool_call: %+v", evs)
	}

	// Non-streaming (nil emit) regular path.
	r.Interceptors = nil
	tt, traces, pause = r.HandleTools(context.Background(), regular, nil)
	if pause != nil || len(traces) != 1 || len(tt.Blocks) != 1 || !HasToolUse(regular) || HasToolUse(agent.UserTurn("x")) {
		t.Fatalf("regular path wrong: pause=%v traces=%d", pause, len(traces))
	}
}

// askingTool needs a seat number before it can book; given one it books.
func askingTool() *tool.Tool {
	return &tool.Tool{Name: "book_seat", Schema: tool.Obj(nil), Fn: func(ctx context.Context, _ json.RawMessage) (any, error) {
		if req, resp, ok := tool.InputResponseFrom(ctx); ok {
			return map[string]any{"booked": resp.Content["seat"], "state": string(req.State)}, nil
		}
		return nil, tool.NeedInput(tool.InputRequest{Message: "Which seat?", Schema: json.RawMessage(`{"type":"object"}`), State: json.RawMessage(`"s1"`)})
	}}
}

func TestRunner_ToolInputPause(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(sleepyTool("get_weather", 0))
	reg.Register(askingTool())
	r := Runner{Reg: reg}
	turn := agent.Turn{Role: agent.RoleAssistant, Blocks: []agent.Block{
		{Type: agent.BlockToolUse, ToolUseID: "w", ToolName: "get_weather", ToolInput: json.RawMessage(`{}`)},
		{Type: agent.BlockToolUse, ToolUseID: "b", ToolName: "book_seat", ToolInput: json.RawMessage(`{"flight":"X1"}`)},
	}}
	var evs []agent.StreamEvent
	tt, traces, pause := r.HandleTools(context.Background(), turn, func(ev agent.StreamEvent) { evs = append(evs, ev) })
	if pause == nil || pause.Kind != agent.PauseKindToolInput || pause.ToolUseID != "b" || pause.Name != "book_seat" ||
		string(pause.Input) != `{"flight":"X1"}` || pause.Message != "Which seat?" {
		t.Fatalf("pause = %+v", pause)
	}
	var req tool.InputRequest
	if err := json.Unmarshal(pause.Request, &req); err != nil || req.Mode != tool.InputModeForm || string(req.State) != `"s1"` {
		t.Fatalf("request = %s (%v)", pause.Request, err)
	}
	if len(tt.Blocks) != 1 || tt.Blocks[0].ToolUseID != "w" || len(traces) != 1 {
		t.Fatalf("the call that ran keeps its result: %+v", tt.Blocks)
	}
	results := 0
	for _, ev := range evs {
		if ev.Type == agent.EventToolResult {
			results++
			if ev.ToolUseID == "b" {
				t.Errorf("the paused call has no result: %+v", ev)
			}
		}
	}
	if results != 1 {
		t.Errorf("one tool_result for the call that ran, got %d", results)
	}

	// The second run, with the user's answer, finishes the call.
	out, again, err := tool.ExecuteWithInput(context.Background(), tool.NewLocalExecutor(reg, tool.LocalExecutorOptions{}),
		tool.Call{ToolUseID: "b", Name: "book_seat", Input: pause.Input}, req, tool.InputResponse{Action: tool.InputAccept, Content: map[string]any{"seat": "12A"}})
	if err != nil || again != nil || !strings.Contains(out, `"booked":"12A"`) || !strings.Contains(out, `"state":"\"s1\""`) {
		t.Fatalf("resumed: out=%s again=%v err=%v", out, again, err)
	}

	// Alone in its turn, the call pauses with no tool turn at all.
	alone := agent.Turn{Role: agent.RoleAssistant, Blocks: turn.Blocks[1:]}
	if tt, _, pause := r.HandleTools(context.Background(), alone, nil); pause == nil || len(tt.Blocks) != 0 {
		t.Fatalf("alone: %+v %+v", tt, pause)
	}
}
