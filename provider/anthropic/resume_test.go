package anthropic

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/provider/internal/replay"
	"github.com/Cyprinus12138/dialogram/tool"
)

// unansweredOnWire translates turns into Messages API messages and returns
// the tool_use ids the immediately following user message does not answer
// with a tool_result: the history the API rejects.
func unansweredOnWire(t *testing.T, turns []agent.Turn) []string {
	t.Helper()
	msgs, err := turnsToMessages(turns)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(msgs)
	var wire []map[string]any
	_ = json.Unmarshal(b, &wire)
	var out []string
	for i, m := range wire {
		blocks, _ := m["content"].([]any)
		var uses []string
		for _, bl := range blocks {
			if bm := bl.(map[string]any); bm["type"] == "tool_use" {
				uses = append(uses, bm["id"].(string))
			}
		}
		answered := map[string]bool{}
		if i+1 < len(wire) && wire[i+1]["role"] == "user" {
			next, _ := wire[i+1]["content"].([]any)
			for _, bl := range next {
				if bm := bl.(map[string]any); bm["type"] == "tool_result" {
					answered[bm["tool_use_id"].(string)] = true
				}
			}
		}
		for _, id := range uses {
			if !answered[id] {
				out = append(out, id)
			}
		}
	}
	return out
}

// The Anthropic twin of the OpenAI scenario: an approval in a parallel
// turn resumes with every tool_use answered in the next user message.
func TestReplay_ApprovalInAParallelTurnResumes(t *testing.T) {
	const ask = "lisbon weather and flights to madrid"
	ctx := context.Background()
	srv := replay.New(t, replay.File("testdata/parallel_tools.sse"), replay.File("testdata/parallel_tools_final.sse"))
	reg := replay.Registry(t, nil)
	p := New(Options{APIKey: "test-key", Model: "claude-sonnet-4-6", BaseURL: srv.URL, Registry: reg, Config: replay.Config(),
		Interceptors: []agent.Interceptor{agent.NewApprovalListInterceptor([]string{"search_flights"})}})

	var first replay.Events
	if err := p.ChatStream(ctx, replayInput(ask), first.Emit); err != nil {
		t.Fatal(err)
	}
	if first.Pause == nil || first.Pause.Kind != agent.PauseKindApproval || first.Pause.Name != "search_flights" || len(first.ToolResults) != 0 || srv.Count() != 1 {
		t.Fatalf("the turn pauses for approval and runs nothing: %s %+v", first.Order(), first.Pause)
	}
	asst := first.Turns[0]
	var weatherID, flightsID string
	var flightsInput json.RawMessage
	for _, b := range asst.Blocks {
		switch b.ToolName {
		case "get_weather":
			weatherID = b.ToolUseID
		case "search_flights":
			flightsID, flightsInput = b.ToolUseID, b.ToolInput
		}
	}
	history := []agent.Turn{agent.UserTurn(ask), asst}

	out, err := reg.Call(ctx, "search_flights", string(flightsInput))
	if err != nil {
		t.Fatal(err)
	}
	onlyApproved := append(append([]agent.Turn(nil), history...), agent.Turn{Role: agent.RoleTool, Blocks: []agent.Block{
		{Type: agent.BlockToolResult, ToolUseID: flightsID, ToolName: "search_flights", ToolResult: json.RawMessage(out)},
	}})
	if got := unansweredOnWire(t, onlyApproved); !reflect.DeepEqual(got, []string{weatherID}) {
		t.Fatalf("control: answering only the approved call leaves get_weather unanswered, got %v", got)
	}
	resumed, err := agent.AnswerPause(history, flightsID, json.RawMessage(out))
	if err != nil {
		t.Fatal(err)
	}
	if got := unansweredOnWire(t, resumed); len(got) != 0 {
		t.Fatalf("AnswerPause leaves nothing unanswered, got %v", got)
	}

	var second replay.Events
	if err := p.ChatStream(ctx, agent.ChatInput{MemoryContext: replay.Memory, Turns: resumed}, second.Emit); err != nil {
		t.Fatal(err)
	}
	if !second.Done || second.Text != replyBoth {
		t.Fatalf("the resumed turn completes: %s %q", second.Order(), second.Text)
	}
	if c := toolResultOnWire(t, srv, 1, 0); replay.Get(c, "error_code") != tool.ErrorCodeNotRun {
		t.Errorf("get_weather never ran: %v", c)
	}
	if c := toolResultOnWire(t, srv, 1, 1); replay.Get(c, "cheapest_eur") != float64(89) {
		t.Errorf("the approved call's observation: %v", c)
	}
}
