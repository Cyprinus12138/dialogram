package loop

import (
	"context"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

// Runner bundles the provider-neutral dependencies a wire adapter needs to
// handle the tool half of a step: the pause policy, the registry and the
// interceptor chain.
type Runner struct {
	Cfg          *agent.Config
	Reg          *tool.Registry
	Interceptors []agent.Interceptor
}

// HasToolUse reports whether an assistant turn requests any tool.
func HasToolUse(t agent.Turn) bool {
	for _, b := range t.Blocks {
		if b.Type == agent.BlockToolUse {
			return true
		}
	}
	return false
}

// HandleTools performs everything that follows an assistant turn carrying
// tool_use blocks, identically for both providers and both paths:
//
//   - A MIXED turn (a pause tool co-requested alongside regular tools) runs
//     the regular tools, attaches their results, and pairs each pause block
//     with a deterministic "re-issue this on its own" correction, so the
//     real work is never dropped on the floor while pausing.
//   - A PURE pause turn returns the waiting_for_user event to emit; nothing
//     is dispatched.
//   - Otherwise every tool_use is dispatched through the interceptor chain
//     in parallel; an interceptor pause returns its approval event.
//
// When emit is non-nil (streaming), tool_call events fire for every regular
// tool up front in model order, then one tool_result per result. A non-nil
// pause means the provider must stop the loop after emitting usage and the
// pause event (streaming) or return the text so far (non-streaming).
func (r Runner) HandleTools(ctx context.Context, assistantTurn agent.Turn, emit agent.EmitFn) (toolTurn agent.Turn, traces []agent.ToolTrace, pause *agent.StreamEvent) {
	isPause := r.Cfg.IsPauseTool
	pauseBlocks, regular := agent.SplitPauseTurn(assistantTurn, isPause)
	if len(pauseBlocks) > 0 && len(regular) > 0 {
		if emit != nil {
			EmitToolCalls(emit, r.Reg, regular)
		}
		results, pb, reason := Dispatch(ctx, r.Interceptors, r.Reg, regular)
		if pb != nil {
			ev := ApprovalPauseEvent(pb, reason)
			return agent.Turn{}, nil, &ev
		}
		if emit != nil {
			EmitToolResults(emit, results)
		}
		tt := ToolTurn(results)
		tt.Blocks = DeferredPauseResultBlocks(tt.Blocks, pauseBlocks)
		return tt, Traces(results), nil
	}
	if p, ok := agent.DetectPause(assistantTurn, isPause); ok {
		ev := p.Event()
		return agent.Turn{}, nil, &ev
	}
	if emit != nil {
		EmitToolCalls(emit, r.Reg, assistantTurn.Blocks)
	}
	results, pb, reason := Dispatch(ctx, r.Interceptors, r.Reg, assistantTurn.Blocks)
	if pb != nil {
		ev := ApprovalPauseEvent(pb, reason)
		return agent.Turn{}, nil, &ev
	}
	if emit != nil {
		EmitToolResults(emit, results)
	}
	return ToolTurn(results), Traces(results), nil
}
