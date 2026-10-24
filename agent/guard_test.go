package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func testGuard() *Guard { return NewGuard("search_flights", "get_fx_rate") }

func TestResultIsSuccess(t *testing.T) {
	cases := map[string]bool{
		``:                                    true,
		`not json`:                            true,
		`{"fare":412}`:                        true,
		`{"error_code":"UPSTREAM_TIMEOUT"}`:   false,
		`{"soft_failure":{"error_code":"X"}}`: false,
		`{"model_observation":{"error_code":"NOT_FOUND"}}`:     false,
		`{"model_observation":{"fare":412},"runtime_state":1}`: true,
		`{"error_code":""}`:   true,
		`{"error_code":null}`: true,
	}
	for raw, want := range cases {
		if got := ResultIsSuccess(raw); got != want {
			t.Errorf("ResultIsSuccess(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestGuard_Flag(t *testing.T) {
	g := testGuard()
	ok := `{"fare_usd":412}`
	failed := `{"error_code":"UPSTREAM_TIMEOUT","error_message":"slow"}`
	cases := []struct {
		name     string
		text     string
		calls    []ToolTrace
		wantFlag bool
		wantKind string
	}{
		{"money no tool", "The fare is about $1,250 for that route.", nil, true, "money"},
		{"money iso code", "Expect roughly 1,250 USD return.", nil, true, "money"},
		{"percent no tool", "Prices usually rise 12% in August.", nil, true, "percent"},
		{"money grounded by successful tool", "The cheapest fare is $412.", []ToolTrace{{Name: "search_flights", Output: ok}}, false, ""},
		{"money after soft-failed tool is NOT grounded", "The fare is typically $412 on that route.", []ToolTrace{{Name: "search_flights", Output: failed}}, true, "money"},
		{"non-grounding tool does not vouch", "The fare is about $412.", []ToolTrace{{Name: "present_data", Output: ok}}, true, "money"},
		{"same-turn call with unknown outcome counts", "Roughly $412 — pulling the live fares now.", []ToolTrace{{Name: "search_flights"}}, false, ""},
		{"no figure at all", "Want me to widen the dates by a day either side?", nil, false, ""},
		{"honest refusal", "I can't quote a fare without a live search.", nil, false, ""},
		{"bare number is not a figure", "There are 3 direct flights a day.", nil, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			flag, kind := g.Flag(c.text, c.calls)
			if flag != c.wantFlag || (c.wantFlag && kind != c.wantKind) {
				t.Errorf("Flag = (%v,%q), want (%v,%q)", flag, kind, c.wantFlag, c.wantKind)
			}
		})
	}
	var nilG *Guard
	if f, _ := nilG.Flag("$5", nil); f {
		t.Error("nil guard must never flag")
	}
}

func TestGuard_KindGrounding(t *testing.T) {
	g := testGuard()
	g.KindGrounding = map[string]map[string]bool{"percent": {"get_fx_rate": true}}
	// A money tool does not vouch for a percentage when kind grounding is set.
	if f, _ := g.Flag("The rate moved 2%.", []ToolTrace{{Name: "search_flights"}}); !f {
		t.Error("search_flights must not ground a percent figure")
	}
	if f, _ := g.Flag("The rate moved 2%.", []ToolTrace{{Name: "get_fx_rate"}}); f {
		t.Error("get_fx_rate must ground a percent figure")
	}
}

func TestGuard_Redact(t *testing.T) {
	g := testGuard()
	in := "The fare is about $1,250 and taxes add 12%. There are 3 direct flights."
	out, reds := g.Redact(in, nil)
	if len(reds) != 2 {
		t.Fatalf("want 2 redactions, got %d: %q", len(reds), out)
	}
	if strings.Contains(out, "1,250") || strings.Contains(out, "12%") {
		t.Errorf("figures survived: %q", out)
	}
	if !strings.Contains(out, "3 direct flights") || !strings.Contains(out, DefaultPlaceholder) {
		t.Errorf("prose or placeholder lost: %q", out)
	}
	clean := "Want me to widen the search?"
	if out, reds := g.Redact(clean, nil); len(reds) != 0 || out != clean {
		t.Errorf("clean text must pass through untouched: %q", out)
	}
	// Custom placeholder per signature and on the guard.
	g.Placeholder = "[redacted]"
	g.Signatures[1].Placeholder = "[pct]"
	out, _ = g.Redact("$9 and 9%", nil)
	if !strings.Contains(out, "[redacted]") || !strings.Contains(out, "[pct]") {
		t.Errorf("custom placeholders not applied: %q", out)
	}
}

func TestGuard_RestatementKept(t *testing.T) {
	g := testGuard()
	grounded := map[string]bool{"412": true}
	text := "As found above, the fare is $412. A business seat would be about $1,980."
	out, reds := g.Redact(text, grounded)
	if !strings.Contains(out, "$412") {
		t.Errorf("restated grounded figure must survive, got %q", out)
	}
	if strings.Contains(out, "1,980") || len(reds) != 1 {
		t.Errorf("newly invented figure must still be redacted, got %q (%d)", out, len(reds))
	}
	// Minutes a tool produced (two digits) count as grounded by default.
	out, reds = g.Redact("That's 27% faster.", map[string]bool{"27": true})
	if len(reds) != 0 || !strings.Contains(out, "27%") {
		t.Errorf("two-digit tool number must be restatable, got %q", out)
	}
	// With MinRestatementDigits=3 the same restatement is redacted.
	g3 := testGuard()
	g3.MinRestatementDigits = 3
	if _, reds := g3.Redact("That's 27% faster.", map[string]bool{"27": true}); len(reds) != 1 {
		t.Error("min digits 3 must not exempt a two-digit number")
	}
}

func TestCollectGroundedNumbers(t *testing.T) {
	turns := []Turn{
		{Role: RoleTool, Blocks: []Block{{Type: BlockToolResult, ToolName: "search_flights", ToolResult: []byte(`{"fare_usd":412,"duration_minutes":95}`)}}},
		{Role: RoleAssistant, Blocks: []Block{{Type: BlockToolUse, ToolName: "search_flights", ToolInput: []byte(`{"max_price":1500,"adults":2}`)}}},
	}
	g := CollectGroundedNumbers(turns, 2)
	if !g["412"] || !g["95"] || !g["1500"] {
		t.Errorf("expected 412, 95 and 1500 grounded, got %v", g)
	}
	if g["2"] {
		t.Errorf("single digits must not be collected at min 2")
	}
}

func TestGuard_GuardReply(t *testing.T) {
	g := testGuard()
	out := &ChatOutput{
		Reply: "The fare is about $1,250 on that route.",
		Turns: []Turn{{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: "The fare is about $1,250 on that route."}}}},
	}
	reds := g.GuardReply(nil, out)
	if len(reds) != 1 || strings.Contains(out.Reply, "1,250") {
		t.Errorf("reply figure not redacted: %q", out.Reply)
	}
	if strings.Contains(assistantText(out.Turns[0].Blocks), "1,250") {
		t.Errorf("persisted turn figure not redacted: %q", assistantText(out.Turns[0].Blocks))
	}

	grounded := &ChatOutput{Reply: "The cheapest fare is $412.", ToolCalls: []ToolTrace{{Name: "search_flights", Output: `{"fare_usd":412}`}}}
	if reds := g.GuardReply(nil, grounded); len(reds) != 0 || !strings.Contains(grounded.Reply, "$412") {
		t.Errorf("grounded reply must survive, got %q", grounded.Reply)
	}

	// Lesson 1: a soft-failed grounding tool does not vouch; the figure goes.
	softFailed := &ChatOutput{Reply: "Fares on that route are typically $412.", ToolCalls: []ToolTrace{{Name: "search_flights", Output: `{"error_code":"UPSTREAM_TIMEOUT"}`}}}
	if reds := g.GuardReply(nil, softFailed); len(reds) != 1 || strings.Contains(softFailed.Reply, "$412") {
		t.Errorf("figure after a soft-failed tool must be redacted, got %q", softFailed.Reply)
	}

	// History seeding: a figure a tool produced on an earlier turn may be restated.
	history := []Turn{{Role: RoleTool, Blocks: []Block{{Type: BlockToolResult, ToolName: "search_flights", ToolResult: []byte(`{"fare_usd":412}`)}}}}
	restated := &ChatOutput{Reply: "As I said, the fare is $412 — due at booking."}
	if reds := g.GuardReply(history, restated); len(reds) != 0 || !strings.Contains(restated.Reply, "$412") {
		t.Errorf("restated grounded figure wrongly redacted: %q", restated.Reply)
	}
	if g.GuardReply(nil, nil) != nil {
		t.Error("nil output must be a no-op")
	}
}

func textTurn(text string, tools ...string) StreamEvent {
	blocks := []Block{{Type: BlockText, Text: text}}
	for _, tn := range tools {
		blocks = append(blocks, Block{Type: BlockToolUse, ToolName: tn, ToolUseID: "u_" + tn})
	}
	return StreamEvent{Type: EventTurn, Turn: &Turn{Role: RoleAssistant, Blocks: blocks}}
}

func collectText(sink *[]StreamEvent) EmitFn {
	return func(ev StreamEvent) { *sink = append(*sink, ev) }
}

func TestGuard_WrapEmit_RedactsLiveAndPersisted(t *testing.T) {
	var got []StreamEvent
	g := testGuard().WrapEmit(nil, collectText(&got))
	g(StreamEvent{Type: EventText, Delta: "The fare is "})
	g(StreamEvent{Type: EventText, Delta: "about $1,250."})
	turn := textTurn("The fare is about $1,250.")
	g(turn)
	g(StreamEvent{Type: EventDone})

	var texts []StreamEvent
	for _, e := range got {
		if e.Type == EventText {
			texts = append(texts, e)
		}
	}
	if len(texts) != 1 {
		t.Fatalf("want exactly 1 re-emitted text event, got %d", len(texts))
	}
	if strings.Contains(texts[0].Delta, "1,250") || !strings.Contains(texts[0].Delta, DefaultPlaceholder) {
		t.Errorf("live text not redacted: %q", texts[0].Delta)
	}
	if strings.Contains(assistantText(turn.Turn.Blocks), "1,250") {
		t.Errorf("persisted turn not redacted")
	}
	if got[len(got)-1].Type != EventDone {
		t.Error("done must pass through")
	}
}

func TestGuard_WrapEmit_GroundingPaths(t *testing.T) {
	// Successful earlier tool turn grounds a later restatement.
	var texts []string
	sink := func(ev StreamEvent) {
		if ev.Type == EventText {
			texts = append(texts, ev.Delta)
		}
	}
	g := testGuard().WrapEmit(nil, sink)
	g(StreamEvent{Type: EventTurn, Turn: &Turn{Role: RoleTool, Blocks: []Block{{Type: BlockToolResult, ToolName: "search_flights", ToolResult: []byte(`{"fare_usd":412}`)}}}})
	g(textTurn("So the fare works out to $412 in total."))
	if len(texts) != 1 || !strings.Contains(texts[0], "$412") {
		t.Fatalf("in-call harvested figure must survive, got %+v", texts)
	}

	// A soft-failed earlier tool does NOT ground a new figure.
	texts = nil
	g = testGuard().WrapEmit(nil, sink)
	g(StreamEvent{Type: EventTurn, Turn: &Turn{Role: RoleTool, Blocks: []Block{{Type: BlockToolResult, ToolName: "search_flights", ToolResult: []byte(`{"error_code":"UPSTREAM_TIMEOUT"}`)}}}})
	g(textTurn("Fares there are typically around $1,250."))
	if len(texts) != 1 || strings.Contains(texts[0], "1,250") {
		t.Fatalf("figure after soft-failed tool must be redacted, got %+v", texts)
	}

	// Same-turn tool_use grounds the text streamed alongside it.
	texts = nil
	g = testGuard().WrapEmit(nil, sink)
	g(textTurn("Roughly $412 — pulling the live fares now.", "search_flights"))
	if len(texts) != 1 || !strings.Contains(texts[0], "$412") {
		t.Fatalf("same-turn grounded figure must not be redacted, got %+v", texts)
	}

	// History seeding.
	texts = nil
	g = testGuard().WrapEmit([]Turn{{Role: RoleTool, Blocks: []Block{{Type: BlockToolResult, ToolName: "search_flights", ToolResult: []byte(`{"fare_usd":412}`)}}}}, sink)
	g(textTurn("As found earlier, $412 return."))
	if len(texts) != 1 || !strings.Contains(texts[0], "$412") {
		t.Fatalf("history-grounded restatement wrongly redacted, got %+v", texts)
	}

	// nil guard passes deltas straight through.
	var nilG *Guard
	var deltas []string
	pass := nilG.WrapEmit(nil, func(ev StreamEvent) { deltas = append(deltas, ev.Delta) })
	pass(StreamEvent{Type: EventText, Delta: "one "})
	pass(StreamEvent{Type: EventText, Delta: "two"})
	if len(deltas) != 2 {
		t.Fatalf("nil guard must not buffer deltas, got %+v", deltas)
	}
}

// fakeProvider replays a scripted ChatOutput / event sequence so Agent's
// wrapping (guard + metrics) can be exercised without a network.
type fakeProvider struct {
	out    *ChatOutput
	events []StreamEvent
}

func (f fakeProvider) Name() string  { return "fake" }
func (f fakeProvider) Model() string { return "fake-1" }
func (f fakeProvider) Chat(context.Context, ChatInput) (*ChatOutput, error) {
	cp := *f.out
	return &cp, nil
}
func (f fakeProvider) ChatStream(_ context.Context, _ ChatInput, emit EmitFn) error {
	for _, ev := range f.events {
		emit(ev)
	}
	return nil
}

func TestAgent_WrapsGuardAndMetrics(t *testing.T) {
	m, _ := NewMetrics(nil)
	a := New(fakeProvider{
		out: &ChatOutput{Reply: "About $1,250.", Turns: []Turn{{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: "About $1,250."}}}}},
		events: []StreamEvent{
			{Type: EventText, Delta: "About $1,250."},
			textTurn("About $1,250."),
			{Type: EventUsage, OutputTokens: 7, CacheReadTokens: 70, CacheCreationTokens: 30},
			{Type: EventDone},
		},
	}, Options{Guard: testGuard(), Metrics: m})

	out, err := a.Chat(context.Background(), ChatInput{Profile: "assistant"})
	if err != nil || strings.Contains(out.Reply, "1,250") {
		t.Fatalf("Chat must apply the guard: %q %v", out.Reply, err)
	}
	var streamed []StreamEvent
	if err := a.ChatStream(context.Background(), ChatInput{Profile: "assistant"}, collectText(&streamed)); err != nil {
		t.Fatal(err)
	}
	for _, ev := range streamed {
		if ev.Type == EventText && strings.Contains(ev.Delta, "1,250") {
			t.Fatalf("ChatStream must apply the guard: %q", ev.Delta)
		}
	}
	if got := counterValue(t, m.turnsTotal.WithLabelValues("assistant", "fake")); got != 2 {
		t.Fatalf("metrics must see both turns, got %v", got)
	}
	if a.Provider() != "fake" || a.Model() != "fake-1" || a.Underlying() == nil {
		t.Fatal("accessors wrong")
	}
	_ = json.Marshal
}

// pauseTurn is an assistant turn of text plus one pause tool call.
func pauseTurn(text, name, input string) StreamEvent {
	return StreamEvent{Type: EventTurn, Turn: &Turn{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockText, Text: text},
		{Type: BlockToolUse, ToolUseID: "dg_p", ToolName: name, ToolInput: json.RawMessage(input)},
	}}}
}

func TestGuard_WrapEmit_RedactsPauseText(t *testing.T) {
	var got []StreamEvent
	g := testGuard().WrapEmit(nil, func(ev StreamEvent) { got = append(got, ev) })
	input := `{"prompt":"Book the package for $3,000?","options":[{"label":"Yes, $3,000","value":"yes"},{"label":"No","value":"no"}]}`
	turn := pauseTurn("One question first.", "request_user_choice", input)
	g(turn)
	// The loop builds the event from the unredacted call.
	g(StreamEvent{Type: EventWaitingForUser, Kind: PauseKindHITLChoice, ToolUseID: "dg_p", Message: "Book the package for $3,000?",
		Input:   json.RawMessage(input),
		Options: []ChoiceOption{{Label: "Yes, $3,000", Value: "yes"}, {Label: "No", Value: "no"}}})

	if in := string(turn.Turn.Blocks[1].ToolInput); strings.Contains(in, "3,000") || !strings.Contains(in, `"value":"yes"`) {
		t.Errorf("persisted pause input not redacted: %s", in)
	}
	w := got[len(got)-1]
	if w.Type != EventWaitingForUser || strings.Contains(w.Message+string(w.Input)+w.Options[0].Label, "3,000") || w.Options[1].Label != "No" {
		t.Errorf("the waiting_for_user event must show the same redaction: %+v", w)
	}
}

func TestGuard_PauseTextIsNotGrounding(t *testing.T) {
	var texts []string
	sink := func(ev StreamEvent) {
		if ev.Type == EventText {
			texts = append(texts, ev.Delta)
		}
	}
	// A figure the model put in a pause earlier does not ground the same
	// figure stated later.
	history := []Turn{{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockToolUse, ToolName: "request_user_input", ToolInput: json.RawMessage(`{"prompt":"Is a 20% discount fine?"}`)}}}}
	g := testGuard().WrapEmit(history, sink)
	g(textTurn("You will get 20% off."))
	if len(texts) != 1 || strings.Contains(texts[0], "20%") {
		t.Errorf("a figure laundered through a pause survived: %q", texts)
	}
}

func TestGuard_ApplicationPauseTools(t *testing.T) {
	reason := `{"project_name":"kestrel gardens","reason":"A non-mature estate with 20% upside."}`
	run := func(g *Guard) string {
		turn := pauseTurn("", "propose_shortlist", reason)
		g.WrapEmit(nil, func(StreamEvent) {})(turn)
		return string(turn.Turn.Blocks[1].ToolInput)
	}
	if got := run(testGuard()); !strings.Contains(got, "20%") {
		t.Errorf("an unlisted tool's input is an argument and stays: %s", got)
	}
	g := testGuard()
	g.PauseTools = map[string]bool{"propose_shortlist": true}
	if got := run(g); strings.Contains(got, "20%") || !strings.Contains(got, "kestrel gardens") {
		t.Errorf("a listed pause tool is checked like the reply: %s", got)
	}
}

func TestGuard_GuardReply_RedactsPauseInput(t *testing.T) {
	out := &ChatOutput{Reply: "One question.", Turns: []Turn{{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockText, Text: "One question."},
		{Type: BlockToolUse, ToolName: "request_user_input", ToolInput: json.RawMessage(`{"prompt":"Is $500 a month fine?"}`)}}}},
		ToolCalls: []ToolTrace{{Name: "request_user_input", Input: json.RawMessage(`{"prompt":"Is $500 a month fine?"}`)}}}
	if reds := testGuard().GuardReply(nil, out); len(reds) != 1 || strings.Contains(string(out.Turns[0].Blocks[1].ToolInput), "$500") {
		t.Errorf("redactions %v, input %s", reds, out.Turns[0].Blocks[1].ToolInput)
	}

	// The same question after a successful grounding call is left alone.
	out.Turns[0].Blocks[1].ToolInput = json.RawMessage(`{"prompt":"Is $500 a month fine?"}`)
	out.ToolCalls = append(out.ToolCalls, ToolTrace{Name: "get_fx_rate", Output: `{"rate":1.35}`})
	if reds := testGuard().GuardReply(nil, out); len(reds) != 0 {
		t.Errorf("a grounded turn must not be redacted: %v", reds)
	}
}
