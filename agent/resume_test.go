package agent

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Cyprinus12138/dialogram/tool"
)

func use(id, name string) Block {
	return Block{Type: BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: json.RawMessage(`{}`)}
}

func result(id, name, body string) Block {
	return Block{Type: BlockToolResult, ToolUseID: id, ToolName: name, ToolResult: json.RawMessage(body)}
}

func TestAnswerPause(t *testing.T) {
	approved := json.RawMessage(`{"booking":"ABC123"}`)
	user := UserTurn("book it")
	for _, c := range []struct {
		name  string
		turns []Turn
		id    string
		want  []Turn
	}{
		{
			name:  "a lone pause",
			turns: []Turn{user, {Role: RoleAssistant, Blocks: []Block{use("dg_1", "request_user_input")}}},
			id:    "dg_1",
			want: []Turn{user, {Role: RoleAssistant, Blocks: []Block{use("dg_1", "request_user_input")}},
				{Role: RoleTool, Blocks: []Block{result("dg_1", "request_user_input", `{"booking":"ABC123"}`)}}},
		},
		{
			name: "an approval in a turn of several calls",
			turns: []Turn{user, {Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: "On it."},
				use("dg_1", "search_flights"), use("dg_2", "book_flight"), use("dg_3", "get_weather")}}},
			id: "dg_2",
			want: []Turn{user, {Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: "On it."},
				use("dg_1", "search_flights"), use("dg_2", "book_flight"), use("dg_3", "get_weather")}},
				{Role: RoleTool, Blocks: []Block{
					result("dg_1", "search_flights", NotRunResultJSON("search_flights")),
					result("dg_2", "book_flight", `{"booking":"ABC123"}`),
					result("dg_3", "get_weather", NotRunResultJSON("get_weather")),
				}}},
		},
		{
			name:  "two pauses in one turn",
			turns: []Turn{user, {Role: RoleAssistant, Blocks: []Block{use("dg_1", "request_user_input"), use("dg_2", "request_user_choice")}}},
			id:    "dg_1",
			want: []Turn{user, {Role: RoleAssistant, Blocks: []Block{use("dg_1", "request_user_input"), use("dg_2", "request_user_choice")}},
				{Role: RoleTool, Blocks: []Block{
					result("dg_1", "request_user_input", `{"booking":"ABC123"}`),
					result("dg_2", "request_user_choice", NotRunResultJSON("request_user_choice")),
				}}},
		},
		{
			name: "a message sent since the pause",
			turns: []Turn{user, {Role: RoleAssistant, Blocks: []Block{use("dg_1", "book_flight")}},
				UserTurn("actually, make it Porto")},
			id: "dg_1",
			want: []Turn{user, {Role: RoleAssistant, Blocks: []Block{use("dg_1", "book_flight")}},
				{Role: RoleTool, Blocks: []Block{result("dg_1", "book_flight", `{"booking":"ABC123"}`)}},
				UserTurn("actually, make it Porto")},
		},
		{
			name: "a tool turn already started",
			turns: []Turn{user, {Role: RoleAssistant, Blocks: []Block{use("dg_1", "get_weather"), use("dg_2", "book_flight")}},
				{Role: RoleTool, Blocks: []Block{result("dg_1", "get_weather", `{"high_c":27}`)}}},
			id: "dg_2",
			want: []Turn{user, {Role: RoleAssistant, Blocks: []Block{use("dg_1", "get_weather"), use("dg_2", "book_flight")}},
				{Role: RoleTool, Blocks: []Block{result("dg_1", "get_weather", `{"high_c":27}`), result("dg_2", "book_flight", `{"booking":"ABC123"}`)}}},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			before, _ := json.Marshal(c.turns)
			got, err := AnswerPause(c.turns, c.id, approved)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				gb, _ := json.Marshal(got)
				wb, _ := json.Marshal(c.want)
				t.Errorf("turns\n got %s\nwant %s", gb, wb)
			}
			if after, _ := json.Marshal(c.turns); string(after) != string(before) {
				t.Error("AnswerPause must not modify its input")
			}
			if left := DanglingToolUses(got); len(left) != 0 {
				t.Errorf("every call of the paused turn has a result, left: %+v", left)
			}
		})
	}

	paused := []Turn{user, {Role: RoleAssistant, Blocks: []Block{use("dg_1", "book_flight")}}}
	if _, err := AnswerPause(paused, "dg_9", approved); !errors.Is(err, ErrNotPending) {
		t.Errorf("an unknown call: %v", err)
	}
	done, _ := AnswerPause(paused, "dg_1", approved)
	if _, err := AnswerPause(done, "dg_1", approved); !errors.Is(err, ErrNotPending) || !strings.Contains(err.Error(), "already has a result") {
		t.Errorf("an answered call: %v", err)
	}
}

func TestPauseResultBodies(t *testing.T) {
	decode := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := decode(NotRunResultJSON("get_weather")); m["error_code"] != tool.ErrorCodeNotRun || m["retryable"] != false ||
		!strings.Contains(m["error_message"].(string), "get_weather was not run") || ResultIsSuccess(NotRunResultJSON("x")) {
		t.Errorf("not run: %v", m)
	}
	if m := decode(DismissedResultJSON(PauseKindApproval, "")); m["error_code"] != tool.ErrorCodeDenied || m["user_rejected"] != true {
		t.Errorf("a dismissed approval reads as declined: %v", m)
	}
	if m := decode(DismissedResultJSON(PauseKindHITLChoice, "The user wrote instead.")); m["error_code"] != tool.ErrorCodeCancelled || m["error_message"] != "The user wrote instead." {
		t.Errorf("a dismissed question is cancelled: %v", m)
	}
}
