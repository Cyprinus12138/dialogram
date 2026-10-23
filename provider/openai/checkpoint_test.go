package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

type recordingCheckpointer struct {
	mu  sync.Mutex
	cps []agent.Checkpoint
}

func (r *recordingCheckpointer) Save(_ context.Context, cp agent.Checkpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cps = append(r.cps, cp)
	return nil
}

func (r *recordingCheckpointer) all() []agent.Checkpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agent.Checkpoint(nil), r.cps...)
}

// expectBoundaries asserts the (step, turns, pending) shape of each
// checkpoint in order.
func expectBoundaries(t *testing.T, cps []agent.Checkpoint, want [][3]int) {
	t.Helper()
	if len(cps) != len(want) {
		t.Fatalf("checkpoints = %d, want %d: %+v", len(cps), len(want), cps)
	}
	for i, w := range want {
		cp := cps[i]
		if cp.MessageID != "msg-1" || cp.Step != w[0] || len(cp.Turns) != w[1] || len(cp.Pending) != w[2] || cp.At.IsZero() {
			t.Fatalf("checkpoint %d = {id=%q step=%d turns=%d pending=%d at=%v}, want step=%d turns=%d pending=%d",
				i, cp.MessageID, cp.Step, len(cp.Turns), len(cp.Pending), cp.At, w[0], w[1], w[2])
		}
	}
}

// One tool round then a final answer: a checkpoint after the assistant
// turn (its call pending), after the tool turn (nothing pending) and after
// the closing assistant turn.
func TestLoop_CheckpointsEveryStepBoundary(t *testing.T) {
	rs := newRecordingServer(toolCallResp("search_flights"), finalResp("done"))
	defer rs.close()
	rec := &recordingCheckpointer{}
	reg := fixtureRegistry(t, map[string]any{"search_flights": map[string]any{"n": 1}})
	p := New(Options{APIKey: "k", BaseURL: rs.server.URL, Model: "m", Registry: reg, Checkpointer: rec})
	out, err := p.Chat(context.Background(), agent.ChatInput{MessageID: "msg-1", Messages: []agent.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	cps := rec.all()
	expectBoundaries(t, cps, [][3]int{{1, 1, 1}, {1, 2, 0}, {2, 3, 0}})
	if cps[0].Pending[0].ToolName != "search_flights" || cps[0].Turns[0].Role != agent.RoleAssistant || cps[1].Turns[1].Role != agent.RoleTool {
		t.Fatalf("checkpoint contents wrong: %+v", cps[:2])
	}
	// The last checkpoint is exactly what the call returns as its turns.
	if len(out.Turns) != 3 || cps[2].Turns[2].Blocks[0].Text != "done" || out.Turns[2].Blocks[0].Text != "done" {
		t.Fatalf("final checkpoint must match ChatOutput.Turns: %+v vs %+v", cps[2].Turns, out.Turns)
	}
}

func TestChatStream_CheckpointsEveryStepBoundary(t *testing.T) {
	var mu sync.Mutex
	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		mu.Lock()
		s := step
		step++
		mu.Unlock()
		if s == 0 {
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"search_flights","arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n"))
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" + "data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"done"},"finish_reason":null}]}` + "\n\n" +
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"))
	}))
	defer srv.Close()
	rec := &recordingCheckpointer{}
	reg := fixtureRegistry(t, map[string]any{"search_flights": map[string]any{"n": 1}})
	p := New(Options{APIKey: "k", BaseURL: srv.URL, Model: "m", Registry: reg, Checkpointer: rec})
	var turnEvents int
	err := p.ChatStream(context.Background(), agent.ChatInput{MessageID: "msg-1", Messages: []agent.ChatMessage{{Role: "user", Content: "hi"}}}, func(ev agent.StreamEvent) {
		if ev.Type == agent.EventTurn {
			turnEvents++
		}
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	expectBoundaries(t, rec.all(), [][3]int{{1, 1, 1}, {1, 2, 0}, {2, 3, 0}})
	if turnEvents != 3 {
		t.Fatalf("turn events = %d, want 3 (one per checkpoint)", turnEvents)
	}
}

// A pause ends the run at the assistant boundary with the pause tool
// pending — exactly the state CloseDangling must leave alone.
func TestLoop_CheckpointOnPause(t *testing.T) {
	rs := newRecordingServer(toolCallResp(tool.HITLInputToolName))
	defer rs.close()
	rec := &recordingCheckpointer{}
	p := New(Options{APIKey: "k", BaseURL: rs.server.URL, Model: "m", Registry: fixtureRegistry(t, nil), Checkpointer: rec})
	if _, err := p.Chat(context.Background(), agent.ChatInput{MessageID: "msg-1", Messages: []agent.ChatMessage{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	cps := rec.all()
	expectBoundaries(t, cps, [][3]int{{1, 1, 1}})
	if cps[0].Pending[0].ToolName != tool.HITLInputToolName {
		t.Fatalf("pending must be the pause tool: %+v", cps[0].Pending)
	}
	if closed := agent.CloseDangling(cps[0].Turns); len(closed) != 1 {
		t.Fatal("CloseDangling must not answer a pause on the model's behalf")
	}
}

// The step-cap synthesis turn is checkpointed as MaxSteps+1 so the
// recovered conversation includes the wrap-up.
func TestLoop_CheckpointAfterSynthesis(t *testing.T) {
	rs := newRecordingServer(toolCallResp("search_flights"), finalResp("wrap-up"))
	defer rs.close()
	rec := &recordingCheckpointer{}
	cfg := &agent.Config{Profiles: map[string]agent.Profile{"quick": {MaxToolSteps: 1}}}
	reg := fixtureRegistry(t, map[string]any{"search_flights": map[string]any{"n": 1}})
	p := New(Options{APIKey: "k", BaseURL: rs.server.URL, Model: "m", Registry: reg, Config: cfg, Checkpointer: rec})
	if _, err := p.Chat(context.Background(), agent.ChatInput{MessageID: "msg-1", Profile: "quick", Messages: []agent.ChatMessage{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	cps := rec.all()
	expectBoundaries(t, cps, [][3]int{{1, 1, 1}, {1, 2, 0}, {2, 3, 0}})
	if cps[2].Turns[2].Blocks[0].Text != "wrap-up" {
		t.Fatalf("synthesis turn missing from the final checkpoint: %+v", cps[2].Turns)
	}
}

// No Checkpointer and no MessageID: the loop runs exactly as before.
func TestLoop_NoCheckpointerIsNoop(t *testing.T) {
	rs := newRecordingServer(toolCallResp("search_flights"), finalResp("done"))
	defer rs.close()
	p := newProvider(rs.server.URL, fixtureRegistry(t, map[string]any{"search_flights": 1}), nil)
	out, err := p.Chat(context.Background(), agent.ChatInput{Messages: []agent.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil || out.Reply != "done" || len(out.Turns) != 3 {
		t.Fatalf("Chat without checkpointer: %v %+v", err, out)
	}
}
