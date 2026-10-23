package loop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

// The executor's MaxConcurrency bounds how many of one step's tools run at
// once; the rest queue, and every call still produces its result.
func TestDispatch_ConcurrencyBound(t *testing.T) {
	reg := tool.NewRegistry()
	var inflight, peak atomic.Int32
	const per = 30 * time.Millisecond
	names := []string{"a", "b", "c", "d", "e", "f"}
	for _, n := range names {
		reg.Register(&tool.Tool{Name: n, Schema: tool.Obj(nil), Fn: func(context.Context, json.RawMessage) (any, error) {
			cur := inflight.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(per)
			inflight.Add(-1)
			return 1, nil
		}})
	}
	exec := tool.NewLocalExecutor(reg, tool.LocalExecutorOptions{MaxConcurrency: 2})
	start := time.Now()
	results, pause, _ := Dispatch(context.Background(), nil, reg, exec, toolUseBlocks(names...))
	elapsed := time.Since(start)
	if pause != nil || len(results) != len(names) {
		t.Fatalf("results=%d pause=%v", len(results), pause)
	}
	if p := peak.Load(); p > 2 {
		t.Fatalf("peak parallelism %d exceeds the bound of 2", p)
	}
	if elapsed < 2*per {
		t.Fatalf("six calls two wide need three rounds; finished in %v", elapsed)
	}
	for _, r := range results {
		if r.Out != "1" {
			t.Fatalf("%s: out = %q", r.Block.ToolName, r.Out)
		}
	}
}

// A call still queued behind the semaphore when ctx ends is failed, not
// dropped: the tool turn stays complete and Dispatch returns promptly.
// Two blocking tools one wide: whichever wins the slot blocks until the
// cancel, the other is queued behind it.
func TestDispatch_CancelledWhileQueued(t *testing.T) {
	reg := tool.NewRegistry()
	block := func(ctx context.Context, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	reg.Register(&tool.Tool{Name: "block_a", Schema: tool.Obj(nil), Fn: block})
	reg.Register(&tool.Tool{Name: "block_b", Schema: tool.Obj(nil), Fn: block})
	exec := tool.NewLocalExecutor(reg, tool.LocalExecutorOptions{MaxConcurrency: 1})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	results, _, _ := Dispatch(ctx, nil, reg, exec, toolUseBlocks("block_a", "block_b"))
	if time.Since(start) > time.Second {
		t.Fatal("Dispatch did not unwind on cancellation")
	}
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	for _, r := range results {
		if !strings.Contains(r.Out, tool.ErrorCodeHardFailure) {
			t.Fatalf("%s: expected HARD_FAILURE on cancellation, got %s", r.Block.ToolName, r.Out)
		}
	}
}

// With no Executor configured the runner still goes through the hardened
// default: a panicking tool yields a TOOL_PANIC result instead of taking
// the goroutine down.
func TestRunner_NilExecutorDefaults(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&tool.Tool{Name: "boom", Schema: tool.Obj(nil), Fn: func(context.Context, json.RawMessage) (any, error) {
		panic("boom")
	}})
	r := Runner{Reg: reg}
	turn := agent.Turn{Role: agent.RoleAssistant, Blocks: toolUseBlocks("boom")}
	tt, traces, pause := r.HandleTools(context.Background(), turn, nil)
	if pause != nil || len(traces) != 1 || len(tt.Blocks) != 1 {
		t.Fatalf("pause=%v traces=%d blocks=%d", pause, len(traces), len(tt.Blocks))
	}
	if !strings.Contains(string(tt.Blocks[0].ToolResult), tool.ErrorCodeToolPanic) {
		t.Fatalf("expected TOOL_PANIC, got %s", tt.Blocks[0].ToolResult)
	}
}

type failingCheckpointer struct{ calls int }

func (f *failingCheckpointer) Save(context.Context, agent.Checkpoint) error {
	f.calls++
	return errors.New("disk full")
}

func TestRunner_Checkpoint(t *testing.T) {
	m := agent.NewMemCheckpointer()
	r := Runner{Reg: tool.NewRegistry(), Checkpointer: m}
	asst := agent.Turn{Role: agent.RoleAssistant, Blocks: []agent.Block{
		{Type: agent.BlockText, Text: "looking"},
		{Type: agent.BlockToolUse, ToolUseID: "u", ToolName: "get_weather", ToolInput: json.RawMessage(`{}`)},
	}}
	r.Checkpoint(context.Background(), "m1", 1, []agent.Turn{asst})
	cp, ok := m.Load("m1")
	if !ok || cp.Step != 1 || len(cp.Turns) != 1 || len(cp.Pending) != 1 || cp.Pending[0].ToolUseID != "u" || cp.At.IsZero() {
		t.Fatalf("assistant-boundary checkpoint wrong: ok=%v %+v", ok, cp)
	}
	tt := agent.Turn{Role: agent.RoleTool, Blocks: []agent.Block{{Type: agent.BlockToolResult, ToolUseID: "u", ToolName: "get_weather", ToolResult: json.RawMessage(`{}`)}}}
	run := []agent.Turn{asst, tt}
	r.Checkpoint(context.Background(), "m1", 1, run)
	cp, _ = m.Load("m1")
	if cp.Step != 1 || len(cp.Turns) != 2 || len(cp.Pending) != 0 {
		t.Fatalf("tool-boundary checkpoint wrong: %+v", cp)
	}
	// The checkpoint owns its slice: growing the run later must not leak
	// into what was saved.
	_ = append(run, agent.UserTurn("later"))
	if cp, _ = m.Load("m1"); len(cp.Turns) != 2 {
		t.Fatal("checkpoint aliases the run slice")
	}

	// A nil checkpointer is a no-op; a failing one is logged, not fatal.
	Runner{}.Checkpoint(context.Background(), "m2", 1, run)
	f := &failingCheckpointer{}
	Runner{Checkpointer: f}.Checkpoint(context.Background(), "m3", 1, run)
	if f.calls != 1 {
		t.Fatalf("failing checkpointer called %d times", f.calls)
	}
}
