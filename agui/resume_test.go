package agui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

// fakeExec is an Executor that records the calls it runs.
type fakeExec struct {
	mu      sync.Mutex
	calls   []tool.Call
	out     string
	entered chan struct{} // signalled when Execute starts, if set
	gate    chan struct{} // Execute waits for it, if set
}

func (f *fakeExec) Execute(_ context.Context, c tool.Call) (string, error) {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return f.out, nil
}

func (f *fakeExec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// script is a Runner that plays runs[i] on its i-th call and finishes with
// a short reply after the last scripted run. It records every input.
type script struct {
	mu     sync.Mutex
	runs   [][]agent.StreamEvent
	inputs []RunAgentInput
}

func (s *script) run(_ context.Context, in RunAgentInput, emit agent.EmitFn) error {
	s.mu.Lock()
	s.inputs = append(s.inputs, in)
	n := len(s.inputs)
	s.mu.Unlock()
	if _, err := MessagesToTurns(in.Messages); err != nil {
		return err
	}
	if n <= len(s.runs) {
		for _, ev := range s.runs[n-1] {
			emit(ev)
		}
		return nil
	}
	emit(agent.StreamEvent{Type: agent.EventText, Delta: "Done."})
	emit(agent.StreamEvent{Type: agent.EventDone})
	return nil
}

func (s *script) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inputs)
}

func (s *script) input(t *testing.T, i int) RunAgentInput {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.inputs) {
		t.Fatalf("the Runner ran %d times, want run %d", len(s.inputs), i+1)
	}
	return s.inputs[i]
}

const bookArgs = `{"carrier":"SKYLINE", "fare_eur": 120}`

var (
	userMsg = raw(`{"id":"u1","role":"user","content":"Book the cheapest flight to Lisbon."}`)

	approvalPause = []agent.StreamEvent{
		{Type: agent.EventText, Delta: "Booking it."},
		{Type: agent.EventToolCall, Name: "book_flight", ToolUseID: "dg_3", Input: raw(bookArgs)},
		{Type: agent.EventWaitingForUser, Kind: agent.PauseKindApproval, ToolUseID: "dg_3", Name: "book_flight", Input: raw(bookArgs), Message: "book_flight needs your approval"},
	}
	choicePause = []agent.StreamEvent{
		{Type: agent.EventWaitingForUser, Kind: agent.PauseKindHITLChoice, ToolUseID: "dg_2", Message: "Which city?",
			Input:   raw(`{"prompt":"Which city?","options":[{"label":"Lisbon","value":"lis"},{"label":"Porto","value":"opo"}],"allow_other":true}`),
			Options: []agent.ChoiceOption{{Label: "Lisbon", Value: "lis"}, {Label: "Porto", Value: "opo"}}},
	}
	formPause = []agent.StreamEvent{
		{Type: agent.EventWaitingForUser, Kind: agent.PauseKindHITLForm, ToolUseID: "dg_4", Message: "Trip details",
			Input: raw(`{"title":"Trip details","questions":[` +
				`{"id":"nights","type":"text","prompt":"How many nights?"},` +
				`{"id":"seat","type":"single_choice","prompt":"Seat?","options":[{"label":"Aisle","value":"aisle"},{"label":"Window","value":"window"}]},` +
				`{"id":"extras","type":"multi_choice","prompt":"Extras?","required":false,"options":[{"label":"Bag","value":"bag"},{"label":"Meal","value":"meal"}]}]}`)},
	}
	textPause = []agent.StreamEvent{
		{Type: agent.EventWaitingForUser, Kind: agent.PauseKindHITL, ToolUseID: "dg_5", Message: "What is your budget?"},
	}
	// twoCallPause is a turn of two calls where the second needs approval:
	// the loop announces both, runs neither, and pauses on the second.
	twoCallPause = []agent.StreamEvent{
		{Type: agent.EventToolCall, Name: "search_flights", ToolUseID: "dg_1", Input: raw(`{"to":"LIS"}`)},
		{Type: agent.EventToolCall, Name: "book_flight", ToolUseID: "dg_2", Input: raw(bookArgs)},
		{Type: agent.EventWaitingForUser, Kind: agent.PauseKindApproval, ToolUseID: "dg_2", Name: "book_flight", Input: raw(bookArgs), Message: "approve?"},
	}
)

func newResumeServer(t *testing.T, s *script, exec tool.Executor, opts ...func(*Options)) (*Handler, *httptest.Server) {
	t.Helper()
	o := Options{NewID: counter("id_"), Executor: exec, InterruptKey: []byte("test-interrupt-key")}
	for _, f := range opts {
		f(&o)
	}
	h := NewHandler(s.run, o)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, srv
}

func runBody(threadID, runID string, msgs []json.RawMessage, resume []ResumeEntry) string {
	b, _ := json.Marshal(map[string]any{"threadId": threadID, "runId": runID, "messages": msgs, "resume": resume})
	return string(b)
}

func doRun(t *testing.T, url, body string) []Event {
	t.Helper()
	resp := post(t, url, body)
	defer resp.Body.Close()
	return readAll(t, resp.Body)
}

// nth returns events[i], failing the test (not the binary) when the run
// produced fewer events.
func nth(t *testing.T, events []Event, i int) Event {
	t.Helper()
	if i < 0 || i >= len(events) {
		t.Fatalf("want event %d, the run produced %d: %s", i, len(events), typesOf(events))
	}
	return events[i]
}

func typesOf(events []Event) string {
	var out []string
	for _, ev := range events {
		out = append(out, ev.Type)
	}
	return strings.Join(out, " ")
}

// interrupted returns the interrupt a run ended on and the snapshot before
// it, re-encoded the way a client sends its history back.
func interrupted(t *testing.T, events []Event) (Interrupt, []json.RawMessage) {
	t.Helper()
	last := nth(t, events, len(events)-1)
	if last.Type != EventRunFinished || last.Outcome == nil || last.Outcome.Type != OutcomeInterrupt || len(last.Outcome.Interrupts) != 1 {
		t.Fatalf("run did not end on one interrupt: %s / %+v", typesOf(events), last)
	}
	snap := events[len(events)-2]
	if snap.Type != EventMessagesSnapshot {
		t.Fatalf("no snapshot before the interrupt: %s", typesOf(events))
	}
	var msgs []json.RawMessage
	for _, m := range snap.Messages {
		msgs = append(msgs, mustJSON(m))
	}
	return last.Outcome.Interrupts[0], msgs
}

func runError(t *testing.T, events []Event) Event {
	t.Helper()
	if typesOf(events) != "RUN_STARTED RUN_ERROR" {
		t.Fatalf("want a refusal, got %s", typesOf(events))
	}
	return nth(t, events, 1)
}

func compactJSON(s string) string {
	var b bytes.Buffer
	_ = json.Compact(&b, []byte(s))
	return b.String()
}

func TestResumeApprovalRunsTheToolOnce(t *testing.T) {
	exec := &fakeExec{out: `{"model_observation":{"booking":"ABC123"},"ui_payload":{"component":"metric_cards","data":{"cards":[]}}}`}
	s := &script{runs: [][]agent.StreamEvent{approvalPause}}
	_, srv := newResumeServer(t, s, exec)

	first := doRun(t, srv.URL, runBody("t1", "r1", []json.RawMessage{userMsg}, nil))
	checkWellFormed(t, first)
	in, history := interrupted(t, first)
	if !strings.HasPrefix(in.ID, "dgi1.") || in.Reason != ReasonToolCall || in.ToolCallID != "dg_3" || in.Message != "book_flight needs your approval" || in.ExpiresAt == "" {
		t.Fatalf("interrupt: %+v", in)
	}
	if len(history) != 2 || !strings.Contains(string(history[1]), `"toolCalls"`) {
		t.Fatalf("the snapshot holds the user message and the paused call: %s", history)
	}

	resume := []ResumeEntry{{InterruptID: in.ID, Status: ResumeResolved, Payload: raw(`{"approved":true}`)}}
	second := doRun(t, srv.URL, runBody("t1", "r2", history, resume))
	checkWellFormed(t, second, "dg_3")
	if got := typesOf(second); got != "RUN_STARTED TOOL_CALL_RESULT CUSTOM TEXT_MESSAGE_START TEXT_MESSAGE_CONTENT TEXT_MESSAGE_END RUN_FINISHED" {
		t.Fatalf("resumed run: %s", got)
	}
	result := nth(t, second, 1)
	if result.ToolCallID != "dg_3" || result.Content != exec.out {
		t.Errorf("the approved call's real output answers it: %+v", result)
	}
	if nth(t, second, 2).Name != CustomGenUI || compactJSON(string(nth(t, second, 2).Value)) != `{"component":"metric_cards","data":{"cards":[]}}` {
		t.Errorf("the approved call's ui_payload renders: %+v", nth(t, second, 2))
	}
	if exec.count() != 1 || exec.calls[0].Name != "book_flight" || exec.calls[0].ToolUseID != "dg_3" || compactJSON(string(exec.calls[0].Input)) != compactJSON(bookArgs) {
		t.Fatalf("executor calls: %+v", exec.calls)
	}
	msgs := s.input(t, 1).Messages
	if len(msgs) != 3 || msgs[2].Role != RoleTool || msgs[2].ToolCallID != "dg_3" || msgs[2].ID != result.MessageID || msgs[2].Content != exec.out {
		t.Fatalf("the Runner sees the result right after the call: %+v", msgs)
	}

	// The client never saw the result and sends the same resume again.
	again := doRun(t, srv.URL, runBody("t1", "r3", history, resume))
	if exec.count() != 1 || nth(t, again, 1).Type != EventToolCallResult || nth(t, again, 1).Content != exec.out {
		t.Errorf("a replay reuses the result instead of booking twice: %d runs, %s", exec.count(), typesOf(again))
	}
	// The client did see it: the history answers the call already.
	withResult := append(append([]json.RawMessage{}, history...), mustJSON(msgs[2]))
	done := doRun(t, srv.URL, runBody("t1", "r4", withResult, resume))
	if got := typesOf(done); got != "RUN_STARTED TEXT_MESSAGE_START TEXT_MESSAGE_CONTENT TEXT_MESSAGE_END RUN_FINISHED" || exec.count() != 1 {
		t.Errorf("an applied resume is a no-op: %s, %d runs", got, exec.count())
	}
}

func TestResumeApprovalDeclinedOrCancelled(t *testing.T) {
	for _, c := range []struct {
		name  string
		entry ResumeEntry
		want  string
	}{
		{"declined with a reason", ResumeEntry{Status: ResumeResolved, Payload: raw(`{"approved":false,"reason":"Too expensive."}`)}, agent.DeniedResultJSON("Too expensive.")},
		{"declined", ResumeEntry{Status: ResumeResolved, Payload: raw(`{"approved":false}`)}, agent.DeniedResultJSON("The user declined to run this tool.")},
		{"cancelled", ResumeEntry{Status: ResumeCancelled}, agent.DeniedResultJSON("The user dismissed the approval request without deciding.")},
		{"cancelled with a payload", ResumeEntry{Status: ResumeCancelled, Payload: raw(`{"approved":true}`)}, agent.DeniedResultJSON("The user dismissed the approval request without deciding.")},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &script{runs: [][]agent.StreamEvent{approvalPause}}
			_, srv := newResumeServer(t, s, nil) // no executor: declining needs none
			in, history := interrupted(t, doRun(t, srv.URL, runBody("t1", "r1", []json.RawMessage{userMsg}, nil)))
			c.entry.InterruptID = in.ID
			second := doRun(t, srv.URL, runBody("t1", "r2", history, []ResumeEntry{c.entry}))
			checkWellFormed(t, second, "dg_3")
			if nth(t, second, 1).Type != EventToolCallResult || nth(t, second, 1).Content != c.want || nth(t, second, 2).Type == EventCustom {
				t.Errorf("got %s %+v", typesOf(second), nth(t, second, 1))
			}
		})
	}
}

func TestResumeInputPauses(t *testing.T) {
	for _, c := range []struct {
		name    string
		pause   []agent.StreamEvent
		callID  string
		payload string
	}{
		{"free text", textPause, "dg_5", `{"user_response": "About 800 euros"}`},
		{"choice", choicePause, "dg_2", `{"selected": ["opo"]}`},
		{"choice, other only", choicePause, "dg_2", `{"selected": [], "other_text": "Faro"}`},
		{"form", formPause, "dg_4", `{"answers": {"nights": "3", "seat": {"value": "window"}, "extras": {"values": ["bag", "meal"]}}}`},
		{"form, optional left out", formPause, "dg_4", `{"answers": {"nights": "3", "seat": {"value": "aisle"}}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &script{runs: [][]agent.StreamEvent{c.pause}}
			_, srv := newResumeServer(t, s, nil)
			first := doRun(t, srv.URL, runBody("t1", "r1", []json.RawMessage{userMsg}, nil))
			checkWellFormed(t, first)
			in, history := interrupted(t, first)
			if in.Reason != ReasonInputRequired || in.ToolCallID != c.callID || len(in.ResponseSchema) == 0 {
				t.Fatalf("interrupt: %+v", in)
			}
			second := doRun(t, srv.URL, runBody("t1", "r2", history, []ResumeEntry{{InterruptID: in.ID, Status: ResumeResolved, Payload: raw(c.payload)}}))
			checkWellFormed(t, second, c.callID)
			if nth(t, second, 1).Type != EventToolCallResult || nth(t, second, 1).ToolCallID != c.callID || nth(t, second, 1).Content != compactJSON(c.payload) {
				t.Errorf("the payload becomes the pause tool's result: %s %+v", typesOf(second), nth(t, second, 1))
			}
		})
	}
}

func TestResumeRejectsPayloadsThatDoNotFit(t *testing.T) {
	for _, c := range []struct {
		pause   []agent.StreamEvent
		payload string
		want    string
	}{
		{choicePause, `{"selected": ["nyc"]}`, `"nyc" is not one of the options`},
		{choicePause, `{"selected": ["lis", "opo"]}`, "pick one option, not 2"},
		{choicePause, `{"selected": []}`, "pick an option"},
		{choicePause, `{"choice": "lis"}`, `unexpected field "choice"`},
		{textPause, `{"user_response": 5}`, `"user_response" has the wrong type`},
		{textPause, `"hello"`, "must be a JSON object"},
		{formPause, `{"answers": {"seat": {"value": "aisle"}}}`, `"nights" is required`},
		{formPause, `{"answers": {"nights": "3", "seat": {"value": "middle"}}}`, `"middle" is not one of the options`},
		{formPause, `{"answers": {"nights": "3", "seat": {"value": "aisle"}, "pets": "no"}}`, `"pets" is not a question`},
		{formPause, `{"answers": {"nights": 3, "seat": {"value": "aisle"}}}`, "a text answer must be a string"},
		{approvalPause, `{"approved": "yes"}`, `"approved" has the wrong type`},
		{approvalPause, `{"approved": true, "editedArgs": {"carrier": "X"}}`, "editing the arguments"},
		{approvalPause, `{}`, `needs "approved"`},
		{approvalPause, ``, "needs a payload"},
		{approvalPause, `null`, "needs a payload"},
	} {
		t.Run(c.want, func(t *testing.T) {
			s := &script{runs: [][]agent.StreamEvent{c.pause}}
			exec := &fakeExec{out: `{}`}
			_, srv := newResumeServer(t, s, exec)
			in, history := interrupted(t, doRun(t, srv.URL, runBody("t1", "r1", []json.RawMessage{userMsg}, nil)))
			entry := ResumeEntry{InterruptID: in.ID, Status: ResumeResolved}
			if c.payload != "" {
				entry.Payload = raw(c.payload)
			}
			refusal := runError(t, doRun(t, srv.URL, runBody("t1", "r2", history, []ResumeEntry{entry})))
			if refusal.Code != CodeResumeInvalid || !strings.Contains(refusal.Message, c.want) {
				t.Errorf("RUN_ERROR %s %q, want %s mentioning %q", refusal.Code, refusal.Message, CodeResumeInvalid, c.want)
			}
			if s.calls() != 1 || exec.count() != 0 {
				t.Errorf("a refused resume reaches neither the Runner nor a tool: %d runs, %d executions", s.calls(), exec.count())
			}
		})
	}
}

func TestResumeEnforcesTheContract(t *testing.T) {
	s := &script{runs: [][]agent.StreamEvent{approvalPause}}
	exec := &fakeExec{out: `{"ok":true}`}
	_, srv := newResumeServer(t, s, exec)
	in, history := interrupted(t, doRun(t, srv.URL, runBody("t1", "r1", []json.RawMessage{userMsg}, nil)))
	approve := ResumeEntry{InterruptID: in.ID, Status: ResumeResolved, Payload: raw(`{"approved":true}`)}

	tampered := append([]json.RawMessage{}, history...)
	tampered[1] = raw(strings.Replace(string(tampered[1]), `120`, `1`, 1))
	newUser := raw(`{"id":"u2","role":"user","content":"never mind"}`)

	for _, c := range []struct {
		name, thread string
		msgs         []json.RawMessage
		resume       []ResumeEntry
		code, want   string
	}{
		{"new input while the interrupt is open", "t1", append(append([]json.RawMessage{}, history...), newUser), nil, CodeInterruptPending, "waiting on an interrupt for tool call dg_3"},
		{"an id this agent never raised", "t1", history, []ResumeEntry{{InterruptID: "int-1", Status: ResumeResolved, Payload: raw(`{"approved":true}`)}}, CodeResumeInvalid, "not an interrupt this agent raised"},
		{"arguments changed in the history", "t1", tampered, []ResumeEntry{approve}, CodeResumeInvalid, "does not match a tool call"},
		{"another thread", "t2", history, []ResumeEntry{approve}, CodeResumeInvalid, "does not match a tool call"},
		{"answered twice", "t1", history, []ResumeEntry{approve, approve}, CodeResumeInvalid, "answered twice"},
		{"unknown status", "t1", history, []ResumeEntry{{InterruptID: in.ID, Status: "approved"}}, CodeResumeInvalid, `status must be "resolved" or "cancelled"`},
		{"an older call left unanswered", "t1", []json.RawMessage{userMsg, history[1], newUser,
			raw(`{"id":"a2","role":"assistant","content":"","toolCalls":[{"id":"dg_7","type":"function","function":{"name":"now","arguments":"{}"}}]}`),
			raw(`{"id":"r7","role":"tool","toolCallId":"dg_7","content":"{}"}`)}, nil, CodeHistoryInvalid, "tool call dg_3 (book_flight) has no result"},
	} {
		t.Run(c.name, func(t *testing.T) {
			refusal := runError(t, doRun(t, srv.URL, runBody(c.thread, "r2", c.msgs, c.resume)))
			if refusal.Code != c.code || !strings.Contains(refusal.Message, c.want) {
				t.Errorf("RUN_ERROR %s %q, want %s mentioning %q", refusal.Code, refusal.Message, c.code, c.want)
			}
		})
	}
	if s.calls() != 1 || exec.count() != 0 {
		t.Errorf("refusals reach neither the Runner nor a tool: %d runs, %d executions", s.calls(), exec.count())
	}

	noExec := &script{runs: [][]agent.StreamEvent{approvalPause}}
	_, bare := newResumeServer(t, noExec, nil)
	in2, history2 := interrupted(t, doRun(t, bare.URL, runBody("t1", "r1", []json.RawMessage{userMsg}, nil)))
	refusal := runError(t, doRun(t, bare.URL, runBody("t1", "r2", history2, []ResumeEntry{{InterruptID: in2.ID, Status: ResumeResolved, Payload: raw(`{"approved":true}`)}})))
	if refusal.Code != CodeResumeInvalid || !strings.Contains(refusal.Message, "cannot run approved tool calls") {
		t.Errorf("approving without an executor: %s %q", refusal.Code, refusal.Message)
	}
}

func TestResumeOfAnOldInterruptDoesNotSkipANewOne(t *testing.T) {
	second := []agent.StreamEvent{
		{Type: agent.EventToolCall, Name: "book_hotel", ToolUseID: "dg_9", Input: raw(`{"nights":3}`)},
		{Type: agent.EventWaitingForUser, Kind: agent.PauseKindApproval, ToolUseID: "dg_9", Name: "book_hotel", Input: raw(`{"nights":3}`)},
	}
	s := &script{runs: [][]agent.StreamEvent{approvalPause, second}}
	exec := &fakeExec{out: `{"ok":true}`}
	_, srv := newResumeServer(t, s, exec)
	in, history := interrupted(t, doRun(t, srv.URL, runBody("t1", "r1", []json.RawMessage{userMsg}, nil)))
	approve := []ResumeEntry{{InterruptID: in.ID, Status: ResumeResolved, Payload: raw(`{"approved":true}`)}}
	run2 := doRun(t, srv.URL, runBody("t1", "r2", history, approve))
	in2, history2 := interrupted(t, run2)
	if in2.ToolCallID != "dg_9" || in2.Message != "Approve running book_hotel?" {
		t.Fatalf("second interrupt: %+v", in2)
	}
	refusal := runError(t, doRun(t, srv.URL, runBody("t1", "r3", history2, approve)))
	if refusal.Code != CodeInterruptPending || !strings.Contains(refusal.Message, "dg_9") {
		t.Errorf("replaying the first resume leaves the second interrupt open: %s %q", refusal.Code, refusal.Message)
	}
}

func TestResumeAnswersTheOtherCallsOfTheTurnAsNotRun(t *testing.T) {
	s := &script{runs: [][]agent.StreamEvent{twoCallPause}}
	exec := &fakeExec{out: `{"booking":"ABC123"}`}
	_, srv := newResumeServer(t, s, exec)
	in, history := interrupted(t, doRun(t, srv.URL, runBody("t1", "r1", []json.RawMessage{userMsg}, nil)))
	if in.ToolCallID != "dg_2" {
		t.Fatalf("interrupt: %+v", in)
	}
	second := doRun(t, srv.URL, runBody("t1", "r2", history, []ResumeEntry{{InterruptID: in.ID, Status: ResumeResolved, Payload: raw(`{"approved":true}`)}}))
	checkWellFormed(t, second, "dg_1", "dg_2")
	if nth(t, second, 1).ToolCallID != "dg_1" || !strings.Contains(nth(t, second, 1).Content, ErrorCodeNotRun) || nth(t, second, 2).ToolCallID != "dg_2" || nth(t, second, 2).Content != exec.out {
		t.Fatalf("results in call order, the sibling not run: %+v / %+v", nth(t, second, 1), nth(t, second, 2))
	}
	if exec.count() != 1 || exec.calls[0].Name != "book_flight" {
		t.Errorf("only the approved call runs: %+v", exec.calls)
	}
	turns, err := MessagesToTurns(s.input(t, 1).Messages)
	if err != nil || len(turns) != 3 || len(turns[2].Blocks) != 2 {
		t.Errorf("both calls answered in one tool turn: %+v %v", turns, err)
	}
}

func TestResumeTheOldWayStillWorks(t *testing.T) {
	s := &script{runs: [][]agent.StreamEvent{twoCallPause}}
	_, srv := newResumeServer(t, s, nil)
	_, history := interrupted(t, doRun(t, srv.URL, runBody("t1", "r1", []json.RawMessage{userMsg}, nil)))
	// A client from before interrupts answers the paused call itself.
	answered := append(append([]json.RawMessage{}, history...), raw(`{"id":"c1","role":"tool","toolCallId":"dg_2","content":"{\"approved\":true}"}`))
	second := doRun(t, srv.URL, runBody("t1", "r2", answered, nil))
	checkWellFormed(t, second, "dg_1", "dg_2")
	if nth(t, second, 1).Type != EventToolCallResult || nth(t, second, 1).ToolCallID != "dg_1" || !strings.Contains(nth(t, second, 1).Content, ErrorCodeNotRun) {
		t.Fatalf("the unanswered sibling is closed: %s %+v", typesOf(second), nth(t, second, 1))
	}
	msgs := s.input(t, 1).Messages
	if len(msgs) != 4 || msgs[2].ToolCallID != "dg_2" || msgs[3].ToolCallID != "dg_1" {
		t.Errorf("the client's answer is kept and the sibling's follows it: %+v", msgs)
	}
}

func TestResumePlacesResultsRightAfterTheCall(t *testing.T) {
	s := &script{runs: [][]agent.StreamEvent{textPause}}
	_, srv := newResumeServer(t, s, nil)
	in, history := interrupted(t, doRun(t, srv.URL, runBody("t1", "r1", []json.RawMessage{userMsg}, nil)))
	later := append(append([]json.RawMessage{}, history...), raw(`{"id":"u2","role":"user","content":"and a window seat"}`))
	doRun(t, srv.URL, runBody("t1", "r2", later, []ResumeEntry{{InterruptID: in.ID, Status: ResumeResolved, Payload: raw(`{"user_response":"800"}`)}}))
	msgs := s.input(t, 1).Messages
	if len(msgs) != 4 || msgs[2].Role != RoleTool || msgs[3].Content != "and a window seat" {
		t.Fatalf("the result sits between the call and the later message: %+v", msgs)
	}
	if _, err := MessagesToTurns(msgs); err != nil {
		t.Fatal(err)
	}
}

func TestResumeExpiry(t *testing.T) {
	now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	for _, c := range []struct {
		entry ResumeEntry
		code  string
	}{
		{ResumeEntry{Status: ResumeResolved, Payload: raw(`{"approved":false}`)}, CodeResumeExpired},
		{ResumeEntry{Status: ResumeCancelled}, ""},
	} {
		s := &script{runs: [][]agent.StreamEvent{approvalPause}}
		h, srv := newResumeServer(t, s, nil, func(o *Options) { o.InterruptTTL = time.Minute })
		h.codec.now = clock
		now = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
		in, history := interrupted(t, doRun(t, srv.URL, runBody("t1", "r1", []json.RawMessage{userMsg}, nil)))
		if in.ExpiresAt != "2026-10-24T09:01:00Z" {
			t.Fatalf("expiresAt = %q", in.ExpiresAt)
		}
		now = now.Add(2 * time.Minute)
		c.entry.InterruptID = in.ID
		events := doRun(t, srv.URL, runBody("t1", "r2", history, []ResumeEntry{c.entry}))
		if c.code != "" {
			if refusal := runError(t, events); refusal.Code != c.code {
				t.Errorf("an expired resolved resume: %s %q", refusal.Code, refusal.Message)
			}
			continue
		}
		if nth(t, events, 1).Type != EventToolCallResult || nth(t, events, len(events)-1).Type != EventRunFinished {
			t.Errorf("an expired interrupt can still be cancelled: %s", typesOf(events))
		}
	}
}

func TestResumeConcurrentApprovalsRunOnce(t *testing.T) {
	exec := &fakeExec{out: `{"booking":"ABC123"}`, entered: make(chan struct{}, 2), gate: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(exec.gate) }) }
	s := &script{runs: [][]agent.StreamEvent{approvalPause}}
	_, srv := newResumeServer(t, s, exec)
	t.Cleanup(release) // runs before the server closes, so no request is left waiting
	in, history := interrupted(t, doRun(t, srv.URL, runBody("t1", "r1", []json.RawMessage{userMsg}, nil)))
	body := runBody("t1", "r2", history, []ResumeEntry{{InterruptID: in.ID, Status: ResumeResolved, Payload: raw(`{"approved":true}`)}})

	type response struct {
		body []byte
		err  error
	}
	results := make(chan response, 2)
	send := func() {
		resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
		if err != nil {
			results <- response{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		results <- response{b, err}
	}
	go send()
	select {
	case <-exec.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the approved call never started")
	}
	go send()
	time.Sleep(50 * time.Millisecond)
	release()
	for i := 0; i < 2; i++ {
		var r response
		select {
		case r = <-results:
		case <-time.After(10 * time.Second):
			t.Fatal("a resume never finished")
		}
		if r.err != nil {
			t.Fatal(r.err)
		}
		events := readAll(t, bytes.NewReader(r.body))
		if nth(t, events, 1).Type != EventToolCallResult || nth(t, events, 1).Content != exec.out {
			t.Errorf("response %d: %s", i, typesOf(events))
		}
	}
	if exec.count() != 1 {
		t.Errorf("two identical resumes ran the tool %d times", exec.count())
	}
}

func TestInterruptSnapshotEchoesClientMessages(t *testing.T) {
	image := `{"id":"u1","role":"user","content":[{"type":"text","text":"Book this one"},{"type":"image","source":{"type":"url","value":"https://example.com/ticket.png"}}]}`
	s := &script{runs: [][]agent.StreamEvent{approvalPause}}
	_, srv := newResumeServer(t, s, nil)
	resp := post(t, srv.URL, runBody("t1", "r1", []json.RawMessage{raw(image)}, nil))
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, frame := range strings.Split(string(body), "\n\n") {
		if !strings.Contains(frame, `"MESSAGES_SNAPSHOT"`) {
			continue
		}
		var snap struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &snap); err != nil {
			t.Fatal(err)
		}
		if len(snap.Messages) != 2 || string(snap.Messages[0]) != image {
			t.Fatalf("the client's message must come back byte for byte, got %s", snap.Messages[0])
		}
		return
	}
	t.Fatal("no MESSAGES_SNAPSHOT frame")
}

func TestInterruptCodec(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := &interruptCodec{key: []byte("k"), now: func() time.Time { return now }}
	p := Pause{Kind: agent.PauseKindApproval, ToolUseID: "dg.weird/id", Name: "book_flight", Input: raw(`{"b":1, "a":[1,2.50]}`)}
	id, expires := c.mint("t1", p)
	if !expires.IsZero() {
		t.Errorf("no TTL, no expiry: %v", expires)
	}
	ref, ok := c.parse(id)
	if !ok || ref.toolCallID != "dg.weird/id" || ref.kind != agent.PauseKindApproval {
		t.Fatalf("parse(%q) = %+v, %v", id, ref, ok)
	}
	if !c.verify("t1", ref, "book_flight", raw(`{"a":[1,2.50],"b":1}`)) {
		t.Error("key order and spacing must not matter")
	}
	for _, bad := range []struct {
		thread, name, args string
	}{
		{"t2", "book_flight", `{"a":[1,2.50],"b":1}`},
		{"t1", "book_hotel", `{"a":[1,2.50],"b":1}`},
		{"t1", "book_flight", `{"a":[1,2.5],"b":1}`},
		{"t1", "book_flight", `{"a":[1,2.50],"b":2}`},
	} {
		if c.verify(bad.thread, ref, bad.name, raw(bad.args)) {
			t.Errorf("verify must fail for %+v", bad)
		}
	}
	other := &interruptCodec{key: []byte("other"), now: c.now}
	if other.verify("t1", ref, "book_flight", p.Input) {
		t.Error("another key must not verify")
	}
	forged := strings.Replace(id, "."+agent.PauseKindApproval+".", "."+agent.PauseKindHITL+".", 1)
	if fref, ok := c.parse(forged); !ok || c.verify("t1", fref, "book_flight", p.Input) {
		t.Error("changing the kind must break the signature")
	}
	for _, malformed := range []string{"", "dg_3", "dgi1.x.approval.0", "dgi2." + strings.TrimPrefix(id, "dgi1."), strings.Replace(id, ".approval.", ".unknown.", 1), strings.Replace(id, ".0.", ".soon.", 1)} {
		if _, ok := c.parse(malformed); ok {
			t.Errorf("parse(%q) must fail", malformed)
		}
	}

	c.ttl = time.Minute
	id, expires = c.mint("t1", p)
	ref, _ = c.parse(id)
	if !expires.Equal(now.Add(time.Minute)) || c.expired(ref) {
		t.Errorf("expires %v, expired %v", expires, c.expired(ref))
	}
	now = now.Add(time.Minute)
	if !c.expired(ref) {
		t.Error("an interrupt is expired at expiresAt")
	}
	if got := string(canonicalJSON(raw(""))); got != "{}" {
		t.Errorf("no arguments canonicalise to {}, got %s", got)
	}
}

type panicExec struct{}

func (panicExec) Execute(context.Context, tool.Call) (string, error) { panic("boom") }

func TestResumeExecutorPanicIsAResult(t *testing.T) {
	s := &script{runs: [][]agent.StreamEvent{approvalPause}}
	_, srv := newResumeServer(t, s, panicExec{})
	in, history := interrupted(t, doRun(t, srv.URL, runBody("t1", "r1", []json.RawMessage{userMsg}, nil)))
	second := doRun(t, srv.URL, runBody("t1", "r2", history, []ResumeEntry{{InterruptID: in.ID, Status: ResumeResolved, Payload: raw(`{"approved":true}`)}}))
	if nth(t, second, 1).Type != EventToolCallResult || !strings.Contains(nth(t, second, 1).Content, tool.ErrorCodeHardFailure) || nth(t, second, len(second)-1).Type != EventRunFinished {
		t.Errorf("a panicking executor answers the call with a hard failure and the run goes on: %s %+v", typesOf(second), nth(t, second, 1))
	}
}

func TestResumeWithoutExpiry(t *testing.T) {
	s := &script{runs: [][]agent.StreamEvent{textPause}}
	_, srv := newResumeServer(t, s, nil, func(o *Options) { o.InterruptTTL = -1 })
	if in, _ := interrupted(t, doRun(t, srv.URL, runBody("t1", "r1", []json.RawMessage{userMsg}, nil))); in.ExpiresAt != "" {
		t.Errorf("a negative TTL means no expiresAt, got %q", in.ExpiresAt)
	}
}

func TestResultCacheShareAndExpire(t *testing.T) {
	now := time.Unix(0, 0)
	c := newResultCache(time.Hour, func() time.Time { return now })
	c.max = 2
	runs := 0
	run := func() string { runs++; return fmt.Sprint("out", runs) }
	if c.do("a", run) != "out1" || c.do("a", run) != "out1" || runs != 1 {
		t.Fatalf("a cached result is reused: runs=%d", runs)
	}
	c.do("b", run)
	c.do("c", run)
	if len(c.done) != 2 {
		t.Errorf("the cache keeps at most max entries, has %d", len(c.done))
	}
	now = now.Add(time.Hour)
	if c.do("c", run) == "out3" {
		t.Error("an expired entry runs again")
	}
}
