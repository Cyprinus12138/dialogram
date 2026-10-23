package loop

import (
	"context"
	"log/slog"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

// Runner bundles the provider-neutral dependencies a wire adapter needs to
// handle the tool half of a step: the pause policy, the registry, the
// interceptor chain, the executor tools run through and the checkpointer
// that sees the IR at every step boundary.
type Runner struct {
	Cfg          *agent.Config
	Reg          *tool.Registry
	Interceptors []agent.Interceptor
	// Executor runs the surviving tool calls of a step. nil selects
	// tool.NewLocalExecutor over Reg with default limits.
	Executor tool.Executor
	// Checkpointer, when non-nil, receives the run's turns at every step
	// boundary through Checkpoint.
	Checkpointer agent.Checkpointer
}

// executor returns the configured Executor or the default over Reg.
func (r Runner) executor() tool.Executor {
	if r.Executor != nil {
		return r.Executor
	}
	return tool.NewLocalExecutor(r.Reg, tool.LocalExecutorOptions{})
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
	exec := r.executor()
	pauseBlocks, regular := agent.SplitPauseTurn(assistantTurn, isPause)
	if len(pauseBlocks) > 0 && len(regular) > 0 {
		if emit != nil {
			EmitToolCalls(emit, r.Reg, regular)
		}
		results, pb, reason := Dispatch(ctx, r.Interceptors, r.Reg, exec, regular)
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
	results, pb, reason := Dispatch(ctx, r.Interceptors, r.Reg, exec, assistantTurn.Blocks)
	if pb != nil {
		ev := ApprovalPauseEvent(pb, reason)
		return agent.Turn{}, nil, &ev
	}
	if emit != nil {
		EmitToolResults(emit, results)
	}
	return ToolTurn(results), Traces(results), nil
}

// Checkpoint hands the turns this run has produced so far to the
// Checkpointer, if one is configured. Providers call it after every
// assistant turn is finalised and again after the tool turn is appended.
// step is 1-based; Pending is derived from run (tool_use blocks without a
// tool_result), so it holds the assistant turn's calls at the first
// boundary and nothing at the second. A Save error is logged, never
// returned — checkpointing is a durability aid, not a correctness gate.
func (r Runner) Checkpoint(ctx context.Context, messageID string, step int, run []agent.Turn) {
	if r.Checkpointer == nil {
		return
	}
	cp := agent.Checkpoint{
		MessageID: messageID,
		Step:      step,
		Turns:     append([]agent.Turn(nil), run...),
		Pending:   agent.DanglingToolUses(run),
		At:        time.Now(),
	}
	if err := r.Checkpointer.Save(ctx, cp); err != nil {
		slog.Warn("checkpoint save failed", "message_id", messageID, "step", step, "err", err)
	}
}
