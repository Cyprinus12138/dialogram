package loop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

type fakeInterceptor struct {
	deny  map[string]string
	pause map[string]string
}

func (f fakeInterceptor) InspectToolUse(_ context.Context, b agent.Block) agent.InterceptDecision {
	if r, ok := f.pause[b.ToolName]; ok {
		return agent.InterceptDecision{Action: agent.InterceptPause, Reason: r}
	}
	if r, ok := f.deny[b.ToolName]; ok {
		return agent.InterceptDecision{Action: agent.InterceptDeny, Reason: r}
	}
	return agent.InterceptDecision{Action: agent.InterceptAllow}
}

func sleepyTool(name string, d time.Duration) *tool.Tool {
	return &tool.Tool{
		Name: name, Description: name, Schema: tool.Obj(nil),
		Fn: func(_ context.Context, _ json.RawMessage) (any, error) {
			time.Sleep(d)
			return map[string]any{"tool": name}, nil
		},
	}
}

func toolUseBlocks(names ...string) []agent.Block {
	out := make([]agent.Block, 0, len(names))
	for _, n := range names {
		out = append(out, agent.Block{Type: agent.BlockToolUse, ToolUseID: agent.NewToolUseID(), ToolName: n, ToolInput: json.RawMessage(`{}`)})
	}
	return out
}

// Independent tools must run concurrently, not sum their latency.
func TestDispatch_RunsInParallel(t *testing.T) {
	reg := tool.NewRegistry()
	const per = 60 * time.Millisecond
	for _, n := range []string{"a", "b", "c"} {
		reg.Register(sleepyTool(n, per))
	}
	start := time.Now()
	results, pause, _ := Dispatch(context.Background(), nil, reg, toolUseBlocks("a", "b", "c"))
	elapsed := time.Since(start)
	if pause != nil || len(results) != 3 {
		t.Fatalf("results=%d pause=%v", len(results), pause)
	}
	if elapsed >= 3*per {
		t.Fatalf("tools ran serially: elapsed %v >= %v", elapsed, 3*per)
	}
}

// Results keep model-emitted order even with a denied tool in the middle,
// and the denied slot carries the synthetic DENIED body.
func TestDispatch_PreservesOrderWithDeny(t *testing.T) {
	reg := tool.NewRegistry()
	for _, n := range []string{"allow_a", "deny_b", "allow_c"} {
		reg.Register(sleepyTool(n, 0))
	}
	ic := fakeInterceptor{deny: map[string]string{"deny_b": "nope"}}
	results, pause, _ := Dispatch(context.Background(), []agent.Interceptor{ic}, reg, toolUseBlocks("allow_a", "deny_b", "allow_c"))
	if pause != nil {
		t.Fatal("unexpected pause")
	}
	got := []string{results[0].Block.ToolName, results[1].Block.ToolName, results[2].Block.ToolName}
	if strings.Join(got, ",") != "allow_a,deny_b,allow_c" {
		t.Fatalf("order = %v", got)
	}
	if !strings.Contains(results[1].Out, tool.ErrorCodeDenied) || strings.Contains(results[0].Out, tool.ErrorCodeDenied) {
		t.Fatalf("denial misplaced: %+v", results)
	}
}

func TestDispatch_PauseShortCircuits(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(sleepyTool("gated", 0))
	ic := fakeInterceptor{pause: map[string]string{"gated": "needs approval"}}
	results, pause, reason := Dispatch(context.Background(), []agent.Interceptor{ic}, reg, toolUseBlocks("gated"))
	if pause == nil || pause.ToolName != "gated" || reason != "needs approval" || results != nil {
		t.Fatalf("pause=%v reason=%q results=%v", pause, reason, results)
	}
	ev := ApprovalPauseEvent(pause, reason)
	if ev.Kind != agent.PauseKindApproval || ev.Name != "gated" || string(ev.Input) != "{}" {
		t.Fatalf("approval event wrong: %+v", ev)
	}
}

func TestDispatch_HardFailureAndUnknownTool(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&tool.Tool{Name: "boom", Schema: tool.Obj(nil), Fn: func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("kaboom")
	}})
	results, _, _ := Dispatch(context.Background(), nil, reg, toolUseBlocks("boom", "missing"))
	for _, r := range results {
		if !strings.Contains(r.Out, tool.ErrorCodeHardFailure) {
			t.Errorf("%s: expected HARD_FAILURE body, got %s", r.Block.ToolName, r.Out)
		}
	}
	tt := ToolTurn(results)
	if tt.Role != agent.RoleTool || len(tt.Blocks) != 2 || tt.Blocks[0].Type != agent.BlockToolResult {
		t.Fatalf("ToolTurn wrong: %+v", tt)
	}
	if tr := Traces(results); len(tr) != 2 || tr[1].Name != "missing" {
		t.Fatalf("Traces wrong: %+v", tr)
	}
}

func TestDeferredPauseResultBlocks(t *testing.T) {
	base := []agent.Block{{Type: agent.BlockToolResult, ToolUseID: "real", ToolName: "get_weather", ToolResult: json.RawMessage(`{"ok":true}`)}}
	pause := []agent.Block{
		{Type: agent.BlockToolUse, ToolUseID: "p1", ToolName: tool.HITLInputToolName},
		{Type: agent.BlockToolUse, ToolUseID: "p2", ToolName: "propose_change"},
	}
	got := DeferredPauseResultBlocks(base, pause)
	if len(got) != 3 || got[0].ToolUseID != "real" || got[1].ToolUseID != "p1" || got[2].ToolUseID != "p2" {
		t.Fatalf("order/pairing wrong: %+v", got)
	}
	for _, b := range got[1:] {
		var env struct {
			ErrorCode    string `json:"error_code"`
			ErrorMessage string `json:"error_message"`
		}
		if err := json.Unmarshal(b.ToolResult, &env); err != nil || env.ErrorCode != PauseCoRequestErrorCode || !strings.Contains(env.ErrorMessage, b.ToolName) {
			t.Errorf("synthetic result wrong for %s: %+v %v", b.ToolName, env, err)
		}
	}
	if got := DeferredPauseResultBlocks(base, nil); len(got) != 1 {
		t.Fatal("no pause must leave blocks unchanged")
	}
}

func TestEmitHelpersAndText(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&tool.Tool{Name: "x", Label: "Ex", Schema: tool.Obj(nil), Fn: func(context.Context, json.RawMessage) (any, error) { return 1, nil }})
	var evs []agent.StreamEvent
	emit := func(ev agent.StreamEvent) { evs = append(evs, ev) }
	blocks := []agent.Block{{Type: agent.BlockText, Text: "hi"}, {Type: agent.BlockToolUse, ToolUseID: "u", ToolName: "x"}}
	EmitToolCalls(emit, reg, blocks)
	if len(evs) != 1 || evs[0].Type != agent.EventToolCall || evs[0].Label != "Ex" || string(evs[0].Input) != "{}" {
		t.Fatalf("tool_call event wrong: %+v", evs)
	}
	EmitToolResults(emit, []Dispatched{{Block: blocks[1], RawJSON: "{}", Out: "1"}})
	if evs[1].Type != agent.EventToolResult || evs[1].Output != "1" {
		t.Fatalf("tool_result event wrong: %+v", evs[1])
	}
	if ExtractText(agent.Turn{Blocks: blocks}) != "hi" {
		t.Fatal("ExtractText wrong")
	}
}

func TestComposeSystem(t *testing.T) {
	lib, _ := agent.NewSkillLibrary(&agent.Skill{ID: "alpha", Name: "Alpha", Description: "use when asked about X"})
	if ComposeStableSystem("BASE", nil) != "BASE" || ComposeSystemPrompt("BASE", nil, "") != "BASE" {
		t.Fatal("nil lib / empty memory must be byte-equal to the base prompt")
	}
	stable := ComposeStableSystem("BASE", lib)
	if !strings.HasPrefix(stable, "BASE\n\n## Available skills") || !strings.Contains(stable, "alpha") {
		t.Fatalf("stable system wrong: %q", stable)
	}
	full := ComposeSystemPrompt("BASE", lib, "MEM")
	if full != stable+MemorySeparator+"MEM" {
		t.Fatalf("full prompt drift: %q", full)
	}
	if ComposeSystemPrompt("", nil, "MEM") != "MEM" || ComposeStableSystem("", lib) != agent.SkillsSummary(lib) {
		t.Fatal("empty base must collapse cleanly")
	}
}

func TestTransientAndBackoff(t *testing.T) {
	if IsTransientStreamErr(nil) || IsTransientStreamErr(context.Canceled) || IsTransientStreamErr(context.DeadlineExceeded) {
		t.Fatal("nil/cancel/deadline must not be transient")
	}
	if !IsTransientStreamErr(io.ErrUnexpectedEOF) || !IsTransientStreamErr(errors.New("read: connection reset by peer")) {
		t.Fatal("network-shaped errors must be transient")
	}
	if !Retryable(429) || !Retryable(503) || Retryable(400) || Retryable(401) {
		t.Fatal("Retryable classification wrong")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitBackoff(ctx, 3); err == nil {
		t.Fatal("cancelled ctx must abort backoff")
	}
	if err := WaitBackoff(context.Background(), 0); err != nil {
		t.Fatal("attempt 0 is a no-op")
	}
	if EstimateTokens("") != 0 || EstimateTokens("abcde") != 2 || IntFromAny(3.0) != 3 || IntFromAny("x") != 0 || AsStr(1) != "" {
		t.Fatal("small helpers wrong")
	}
}
