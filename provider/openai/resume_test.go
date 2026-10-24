package openai

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/provider/internal/replay"
	"github.com/Cyprinus12138/dialogram/tool"
)

// unansweredOnWire translates turns into Chat Completions messages and
// returns the tool_call ids no tool message answers: the history the API
// rejects.
func unansweredOnWire(t *testing.T, turns []agent.Turn) []string {
	t.Helper()
	msgs, err := turnsToMessages("system", turns)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(msgs)
	var wire []map[string]any
	_ = json.Unmarshal(b, &wire)
	answered := map[string]bool{}
	for _, m := range wire {
		if m["role"] == "tool" {
			answered[m["tool_call_id"].(string)] = true
		}
	}
	var out []string
	for _, m := range wire {
		calls, _ := m["tool_calls"].([]any)
		for _, c := range calls {
			if id := c.(map[string]any)["id"].(string); !answered[id] {
				out = append(out, id)
			}
		}
	}
	return out
}

// An approval in a parallel turn: search_flights needs the user, so the
// loop pauses and runs neither call. Resuming with agent.AnswerPause must
// give the follow-up request a tool message for both calls; appending only
// the approved call's result, as the docs used to say, leaves get_weather
// unanswered on the wire.
func TestReplay_ApprovalInAParallelTurnResumes(t *testing.T) {
	const ask = "lisbon weather and flights to madrid"
	ctx := context.Background()
	srv := replay.New(t, replay.File("testdata/parallel_tools.sse"), replay.File("testdata/parallel_tools_final.sse"))
	reg := replay.Registry(t, nil)
	p := New(Options{APIKey: "test-key", Model: "gpt-4o-mini", BaseURL: srv.URL, Registry: reg, Config: replay.Config(),
		Interceptors: []agent.Interceptor{agent.NewApprovalListInterceptor([]string{"search_flights"})}})

	var first replay.Events
	if err := p.ChatStream(ctx, replayInput(ask), first.Emit); err != nil {
		t.Fatal(err)
	}
	if first.Pause == nil || first.Pause.Kind != agent.PauseKindApproval || first.Pause.Name != "search_flights" || len(first.ToolResults) != 0 || srv.Count() != 1 {
		t.Fatalf("the turn pauses for approval and runs nothing: %s %+v", first.Order(), first.Pause)
	}
	asst := first.Turns[0]
	weatherID, flightsID := asst.Blocks[0].ToolUseID, asst.Blocks[1].ToolUseID
	history := []agent.Turn{agent.UserTurn(ask), asst}

	// The user approves: the application runs the call and resumes.
	out, err := reg.Call(ctx, "search_flights", string(asst.Blocks[1].ToolInput))
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
	req := srv.Request(1)
	if replay.Len(req, "messages") != 5 || replay.Str(req, "messages", 3, "tool_call_id") != weatherID || replay.Str(req, "messages", 4, "tool_call_id") != flightsID {
		t.Fatalf("both calls are answered on the wire: %v", req["messages"])
	}
	if c := toolMessageContent(t, srv, 1, 3); replay.Get(c, "error_code") != tool.ErrorCodeNotRun {
		t.Errorf("get_weather never ran: %v", c)
	}
	if c := toolMessageContent(t, srv, 1, 4); replay.Get(c, "cheapest_eur") != float64(89) {
		t.Errorf("the approved call's observation: %v", c)
	}
}
