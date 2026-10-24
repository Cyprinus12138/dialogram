package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cyprinus12138/dialogram"
	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/streambuf"
	"github.com/Cyprinus12138/dialogram/tool"
)

// seatTool asks for a seat on a form, then sends the user to pay, then
// books: two rounds of input before it can answer.
func seatTool() *tool.Tool {
	return &tool.Tool{Name: "book_seat", Description: "Books a seat.", Schema: tool.Obj(map[string]any{"flight": tool.Str("Flight")}, "flight"),
		Fn: func(ctx context.Context, _ json.RawMessage) (any, error) {
			req, resp, ok := tool.InputResponseFrom(ctx)
			switch {
			case !ok:
				return nil, tool.NeedInput(tool.InputRequest{Message: "Which seat?", State: json.RawMessage(`"seat"`),
					Schema: json.RawMessage(`{"type":"object","properties":{"seat":{"type":"string"}},"required":["seat"]}`)})
			case resp.Action != tool.InputAccept:
				return map[string]any{"booked": false, "because": resp.Action}, nil
			case string(req.State) == `"seat"`:
				state, _ := json.Marshal(resp.Content["seat"])
				return nil, tool.NeedInput(tool.InputRequest{Mode: tool.InputModeURL, Message: "Pay for the seat", URL: "https://pay.example/1", State: state})
			default:
				var seat string
				_ = json.Unmarshal(req.State, &seat)
				return map[string]any{"booked": seat}, nil
			}
		}}
}

func inputServer(t *testing.T, u *upstream) string {
	t.Helper()
	llm := httptest.NewServer(u)
	t.Cleanup(llm.Close)
	deps, checkpoints, err := buildDeps()
	if err != nil {
		t.Fatal(err)
	}
	deps.Registry.Register(seatTool())
	ag, err := dialogram.New(dialogram.Settings{Provider: dialogram.ProviderOpenAI, OpenAIAPIKey: "test", OpenAIBaseURL: llm.URL, OpenAIModel: "m"}, deps)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app{ag: ag, exec: deps.Executor, checkpoints: checkpoints, buf: streambuf.NewMemStore(0)}.routes())
	t.Cleanup(srv.Close)
	return srv.URL
}

func respondJSON(t *testing.T, base string, body map[string]any) (int, map[string]any) {
	t.Helper()
	resp := respond(t, base, body)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestToolInputRoundsOverSSE(t *testing.T) {
	u := &upstream{streams: []string{
		chunk(callDelta(0, "call_b", "book_seat", `{"flight":"SQ1"}`)) + finish("tool_calls"),
		chunk(`{"role":"assistant","content":"Booked 12A."}`) + finish("stop"),
	}}
	base := inputServer(t, u)
	end, conv := ended(t, send(t, base, "m1", map[string]any{"message": "Book me a seat on SQ1."}))
	if end.event != "waiting_for_user" || end.data["kind"] != agent.PauseKindToolInput || end.data["name"] != "book_seat" || end.data["message"] != "Which seat?" {
		t.Fatalf("the tool's question pauses the run: %s %v", end.event, end.data)
	}
	id := end.data["tool_use_id"].(string)
	if c := storedConversation(t, base, conv); c.Pending == nil || !strings.Contains(string(c.Pending.Request), `"requested_schema"`) {
		t.Fatalf("a reload can render the form: %+v", c.Pending)
	}
	if code, _ := respondJSON(t, base, map[string]any{"conversation_id": conv, "tool_use_id": id, "action": "maybe"}); code != http.StatusBadRequest {
		t.Errorf("an unknown action is refused, got %d", code)
	}

	code, body := respondJSON(t, base, map[string]any{"conversation_id": conv, "tool_use_id": id, "action": "accept", "content": map[string]any{"seat": "12A"}})
	waiting, _ := body["waiting"].(map[string]any)
	if code != http.StatusOK || waiting == nil || waiting["message"] != "Pay for the seat" || !strings.Contains(string(jsonBody(waiting["request"])), "https://pay.example/1") {
		t.Fatalf("the tool asks again, and the answer says so: %d %v", code, body)
	}
	if u.count() != 1 {
		t.Fatalf("the model is not asked while the tool still waits: %d requests", u.count())
	}

	code, body = respondJSON(t, base, map[string]any{"conversation_id": conv, "tool_use_id": id, "message_id": "m2", "action": "accept"})
	if code != http.StatusOK || body["waiting"] != nil {
		t.Fatalf("the last answer resumes the run: %d %v", code, body)
	}
	if end, _ := ended(t, tail(t, base, "m2")); end.event != "done" {
		t.Fatalf("the resumed turn completes: %s %v", end.event, end.data)
	}
	if got := resultFor(u.messages(t, 1), "book_seat"); !strings.Contains(got, `"booked":"12A"`) {
		t.Errorf("the model reads the booking: %s", got)
	}
	if left := agent.DanglingToolUses(storedTurns(t, base, conv)); len(left) != 0 {
		t.Errorf("no call is left without a result: %+v", left)
	}
}

func TestToolInputDeclined(t *testing.T) {
	u := &upstream{streams: []string{
		chunk(callDelta(0, "call_b", "book_seat", `{"flight":"SQ1"}`)) + finish("tool_calls"),
		chunk(`{"role":"assistant","content":"Not booked."}`) + finish("stop"),
	}}
	base := inputServer(t, u)
	end, conv := ended(t, send(t, base, "m1", map[string]any{"message": "Book me a seat."}))
	code, _ := respondJSON(t, base, map[string]any{"conversation_id": conv, "tool_use_id": end.data["tool_use_id"], "message_id": "m2", "action": "decline"})
	if code != http.StatusOK {
		t.Fatalf("respond: %d", code)
	}
	if end, _ := ended(t, tail(t, base, "m2")); end.event != "done" {
		t.Fatalf("%s %v", end.event, end.data)
	}
	if got := resultFor(u.messages(t, 1), "book_seat"); !strings.Contains(got, `"because":"decline"`) {
		t.Errorf("the tool hears the decline: %s", got)
	}
}
