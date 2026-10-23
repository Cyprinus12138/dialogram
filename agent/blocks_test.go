package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Cyprinus12138/dialogram/tool"
)

func TestInputTurns_PrefersTurnsAndNormalisesRoles(t *testing.T) {
	in := ChatInput{
		Messages: []ChatMessage{{Role: "user", Content: "ignored"}},
		Turns: []Turn{
			{Role: "user", Blocks: []Block{{Type: BlockText, Text: "hi"}}},
			{Role: "bogus", Blocks: []Block{{Type: BlockText, Text: "x"}}},
			{Role: "assistant"}, // empty → dropped
		},
	}
	got := in.InputTurns()
	if len(got) != 2 {
		t.Fatalf("want 2 turns, got %d", len(got))
	}
	if got[1].Role != RoleUser {
		t.Errorf("unknown role must collapse to user, got %q", got[1].Role)
	}
}

func TestMessagesToTurns_PreservesEmptyContent(t *testing.T) {
	turns := MessagesToTurns([]ChatMessage{{Role: "user", Content: ""}, {Role: "assistant", Content: "ack"}, {Role: "system", Content: "x"}})
	if len(turns) != 3 || turns[0].Blocks[0].Text != "" {
		t.Fatalf("empty content not preserved: %+v", turns)
	}
	if turns[2].Role != RoleUser {
		t.Errorf("non user/assistant role must collapse to user")
	}
}

func TestNewToolUseID_UniqueAndPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewToolUseID()
		if !strings.HasPrefix(id, ToolUseIDPrefix) || len(id) != len(ToolUseIDPrefix)+24 {
			t.Fatalf("bad id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id after %d: %q", i, id)
		}
		seen[id] = true
	}
}

func TestFindPendingHITL(t *testing.T) {
	turns := []Turn{
		UserTurn("help me plan a trip"),
		{Role: RoleAssistant, Blocks: []Block{
			{Type: BlockText, Text: "need one detail"},
			{Type: BlockToolUse, ToolUseID: "dg_abc", ToolName: tool.HITLInputToolName,
				ToolInput: json.RawMessage(`{"prompt":"what is your budget?"}`)},
		}},
	}
	useID, prompt, ok := FindPendingHITL(turns)
	if !ok || useID != "dg_abc" || prompt != "what is your budget?" {
		t.Fatalf("got %q %q %v", useID, prompt, ok)
	}

	// Resolved pause (tool turn follows) is not pending.
	resolved := append(turns, Turn{Role: RoleTool, Blocks: []Block{
		{Type: BlockToolResult, ToolUseID: "dg_abc", ToolName: tool.HITLInputToolName, ToolResult: json.RawMessage(`{"user_response":"2k"}`)},
	}})
	if _, _, ok := FindPendingHITL(resolved); ok {
		t.Fatal("resolved pause should not be flagged as pending")
	}
	// A regular tool_use is not a pause.
	if _, _, ok := FindPendingHITL([]Turn{{Role: RoleAssistant, Blocks: []Block{{Type: BlockToolUse, ToolName: "get_weather"}}}}); ok {
		t.Fatal("regular tool must not be a pause")
	}
}

func TestFindPendingChoiceAndForm(t *testing.T) {
	choice := Turn{Role: RoleAssistant, Blocks: []Block{{Type: BlockToolUse, ToolUseID: "dg_c", ToolName: tool.HITLChoiceToolName,
		ToolInput: json.RawMessage(`{"prompt":"which city?","options":[{"label":"Lisbon","value":"lis"},{"label":"Porto","value":"opo"}],"multiple":false}`)}}}
	id, raw, prompt, opts, multiple, ok := FindPendingChoice([]Turn{choice})
	if !ok || id != "dg_c" || prompt != "which city?" || len(opts) != 2 || multiple || len(raw) == 0 {
		t.Fatalf("choice detection wrong: %v %q %d %v", ok, prompt, len(opts), multiple)
	}

	form := Turn{Role: RoleAssistant, Blocks: []Block{{Type: BlockToolUse, ToolUseID: "dg_f", ToolName: tool.HITLFormToolName,
		ToolInput: json.RawMessage(`{"title":"Tell me about the trip","reason":"to filter flights","questions":[
			{"id":"budget","type":"text","prompt":"Budget?","required":true,"placeholder":"e.g. 1200"},
			{"id":"cities","type":"multi_choice","prompt":"Which cities?","options":[{"label":"Lisbon","value":"lis"},{"label":"Porto","value":"opo"}],"allow_other":true}]}`)}}}
	fid, fraw, title, reason, qs, ok := FindPendingForm([]Turn{form})
	if !ok || fid != "dg_f" || title != "Tell me about the trip" || reason == "" || len(qs) != 2 || len(fraw) == 0 {
		t.Fatalf("form detection wrong: %v %q %q %d", ok, title, reason, len(qs))
	}
	if qs[0].Placeholder != "e.g. 1200" || !qs[1].AllowOther || qs[1].Type != "multi_choice" {
		t.Fatalf("form questions mis-parsed: %+v", qs)
	}
}

func TestSplitPauseTurnAndDetectPause(t *testing.T) {
	cfg := &Config{PauseTools: []string{"propose_change"}}
	turn := Turn{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockText, Text: "looking into it"},
		{Type: BlockToolUse, ToolUseID: "r1", ToolName: "get_weather"},
		{Type: BlockToolUse, ToolUseID: "p1", ToolName: tool.HITLInputToolName, ToolInput: json.RawMessage(`{"prompt":"budget?"}`)},
		{Type: BlockToolUse, ToolUseID: "p2", ToolName: "propose_change", ToolInput: json.RawMessage(`{"x":1}`)},
		{Type: BlockToolUse, ToolUseID: "r2", ToolName: "search_flights"},
	}}
	pause, regular := SplitPauseTurn(turn, cfg.IsPauseTool)
	if len(pause) != 2 || len(regular) != 2 || regular[0].ToolUseID != "r1" || regular[1].ToolUseID != "r2" {
		t.Fatalf("split wrong: pause=%d regular=%d", len(pause), len(regular))
	}

	custom := Turn{Role: RoleAssistant, Blocks: []Block{{Type: BlockToolUse, ToolUseID: "p2", ToolName: "propose_change", ToolInput: json.RawMessage(`{"x":1}`)}}}
	p, ok := DetectPause(custom, cfg.IsPauseTool)
	if !ok || p.Kind != PauseKindCustom || p.ToolName != "propose_change" {
		t.Fatalf("custom pause not detected: %+v %v", p, ok)
	}
	ev := p.Event()
	if ev.Type != EventWaitingForUser || ev.Name != "propose_change" || string(ev.Input) != `{"x":1}` {
		t.Fatalf("event wrong: %+v", ev)
	}

	hitl := Turn{Role: RoleAssistant, Blocks: []Block{{Type: BlockToolUse, ToolUseID: "p1", ToolName: tool.HITLInputToolName, ToolInput: json.RawMessage(`{"prompt":"budget?"}`)}}}
	p, ok = DetectPause(hitl, cfg.IsPauseTool)
	if !ok || p.Kind != PauseKindHITL || p.Message != "budget?" || p.Event().Name != "" {
		t.Fatalf("hitl pause wrong: %+v", p)
	}
	if _, ok := DetectPause(Turn{Role: RoleAssistant, Blocks: []Block{{Type: BlockToolUse, ToolName: "get_weather"}}}, cfg.IsPauseTool); ok {
		t.Fatal("regular tool turn must not detect a pause")
	}
	// A nil config still recognises the built-in HITL tools and nothing else.
	var nilCfg *Config
	if !nilCfg.IsPauseTool(tool.HITLChoiceToolName) || nilCfg.IsPauseTool("propose_change") {
		t.Fatal("nil config pause policy wrong")
	}
}

func TestConfig_ResolveAndToolAllow(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]Profile{
			"assistant": {SystemPrompt: "A", Tools: []string{"get_weather", "search_flights"}},
			"writer":    {SystemPrompt: "W", MaxToolSteps: 3},
		},
		DefaultProfile: "assistant",
	}
	if p := cfg.Resolve(""); p.Name != "assistant" || p.SystemPrompt != "A" {
		t.Errorf("empty name must resolve to default, got %+v", p)
	}
	if p := cfg.Resolve("unknown"); p.Name != "assistant" {
		t.Errorf("unknown name must resolve to default, got %+v", p)
	}
	if p := cfg.Resolve("writer"); p.MaxSteps() != 3 || p.Nudge() != DefaultFinalSynthesisNudge {
		t.Errorf("writer profile wrong: %+v", p)
	}
	if (Profile{}).MaxSteps() != DefaultMaxToolSteps {
		t.Error("zero profile must use default step cap")
	}
	var nilCfg *Config
	if p := nilCfg.Resolve("x"); p.Name != "x" || p.SystemPrompt != "" {
		t.Errorf("nil config must yield an empty profile, got %+v", p)
	}

	// Allow-list intersection.
	if got := cfg.ToolAllow(ChatInput{Profile: "assistant"}); len(got) != 2 {
		t.Errorf("profile-only allow = %v", got)
	}
	if got := cfg.ToolAllow(ChatInput{Profile: "assistant", Tools: []string{"search_flights", "now"}}); len(got) != 1 || got[0] != "search_flights" {
		t.Errorf("intersection wrong: %v", got)
	}
	if got := cfg.ToolAllow(ChatInput{Profile: "writer", Tools: []string{"now"}}); len(got) != 1 || got[0] != "now" {
		t.Errorf("profile without list must defer to per-turn list: %v", got)
	}
	if got := cfg.ToolAllow(ChatInput{Profile: "writer"}); got != nil {
		t.Errorf("no lists must mean all tools (nil), got %v", got)
	}
}

func TestPromptFingerprint(t *testing.T) {
	a := PromptFingerprint("You are a travel helper.")
	if a != PromptFingerprint("You are a travel helper.") || len(a) != 64 {
		t.Fatal("fingerprint must be a stable 64-char hex digest")
	}
	if a == PromptFingerprint("You are a weather helper.") {
		t.Fatal("different prompts must differ")
	}
	if (Profile{SystemPrompt: "x"}).Fingerprint() != PromptFingerprint("x") {
		t.Fatal("profile fingerprint must hash the system prompt")
	}
}

func TestAccumulator(t *testing.T) {
	var acc Accumulator
	acc.Observe(StreamEvent{Type: EventText, Delta: "hel"})
	acc.Observe(StreamEvent{Type: EventText, Delta: "lo"})
	acc.Observe(StreamEvent{Type: EventTurn, Turn: &Turn{Role: RoleAssistant}})
	acc.Observe(StreamEvent{Type: EventUsage, InputTokens: 10, OutputTokens: 4, CacheReadTokens: 8})
	acc.Observe(StreamEvent{Type: EventUsage, InputTokens: 12, OutputTokens: 9})
	acc.Observe(StreamEvent{Type: EventWaitingForUser, Kind: PauseKindHITL, ToolUseID: "dg_1", Message: "budget?"})
	if acc.Text.String() != "hello" || len(acc.Turns) != 1 {
		t.Fatalf("text/turns wrong: %q %d", acc.Text.String(), len(acc.Turns))
	}
	if acc.InputTokens != 12 || acc.OutputTokens != 9 || acc.CacheReadTokens != 8 {
		t.Fatalf("usage must keep the last non-zero values: %+v", acc)
	}
	if !acc.Paused || acc.PausedKind != PauseKindHITL || acc.PausedToolUseID != "dg_1" || acc.PausedPrompt != "budget?" {
		t.Fatalf("pause not captured: %+v", acc)
	}
}

func TestInterceptors(t *testing.T) {
	ic := NewApprovalListInterceptor([]string{"send_email", ""})
	d := RunInterceptors(t.Context(), []Interceptor{ic}, Block{Type: BlockToolUse, ToolName: "send_email"})
	if d.Action != InterceptPause || !strings.Contains(d.Reason, "send_email") {
		t.Fatalf("expected pause, got %+v", d)
	}
	if d := RunInterceptors(t.Context(), []Interceptor{ic}, Block{ToolName: "get_weather"}); d.Action != InterceptAllow {
		t.Fatalf("expected allow, got %+v", d)
	}
	if d := RunInterceptors(t.Context(), nil, Block{ToolName: "x"}); d.Action != InterceptAllow {
		t.Fatal("empty chain must allow")
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(DeniedResultJSON("")), &env); err != nil || env["error_code"] != tool.ErrorCodeDenied || env["user_rejected"] != true {
		t.Fatalf("denied body wrong: %v %v", env, err)
	}
}
