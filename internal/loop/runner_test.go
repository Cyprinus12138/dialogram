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
