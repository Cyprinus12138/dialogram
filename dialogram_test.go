package dialogram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

func TestNewProvider_Selection(t *testing.T) {
	if _, err := NewProvider(Settings{Provider: "bogus"}, Deps{}); err == nil {
		t.Error("unknown provider must error")
	}
	if _, err := NewProvider(Settings{Provider: ProviderAnthropic}, Deps{}); err == nil {
		t.Error("missing anthropic key must error")
	}
	if _, err := NewProvider(Settings{Provider: ProviderOpenAI}, Deps{}); err == nil {
		t.Error("missing openai key must error")
	}
	p, err := NewProvider(Settings{AnthropicAPIKey: "k"}, Deps{})
	if err != nil || p.Name() != ProviderAnthropic {
		t.Errorf("anthropic key must select anthropic by default: %v %v", p, err)
	}
	p, err = NewProvider(Settings{OpenAIAPIKey: "k"}, Deps{})
	if err != nil || p.Name() != ProviderOpenAI {
		t.Errorf("openai key alone must select openai: %v %v", p, err)
	}
	t.Setenv("LLM_PROVIDER", "OpenAI")
	t.Setenv("OPENAI_MODEL", "m")
	if s := SettingsFromEnv(); s.Provider != "openai" || s.OpenAIModel != "m" {
		t.Errorf("SettingsFromEnv wrong: %+v", s)
	}
}

// The same block-IR history produces a valid request on BOTH providers —
// the cross-provider failover invariant, exercised through the public
// entry point against scripted upstreams.
func TestCrossProviderResume(t *testing.T) {
	history := []agent.Turn{
		agent.UserTurn("weather in lisbon?"),
		{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockToolUse, ToolUseID: "dg_1", ToolName: "get_weather", ToolInput: json.RawMessage(`{"city":"Lisbon"}`)}}},
		{Role: agent.RoleTool, Blocks: []agent.Block{{Type: agent.BlockToolResult, ToolUseID: "dg_1", ToolName: "get_weather", ToolResult: json.RawMessage(`{"high_c":27}`)}}},
		agent.UserTurn("and tomorrow?"),
	}
	reg := tool.NewRegistry()
	reg.Register(&tool.Tool{Name: "get_weather", Schema: tool.Obj(nil), Fn: func(context.Context, json.RawMessage) (any, error) { return 1, nil }})

	var oaBody, anBody map[string]any
	oa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &oaBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"sunny"},"finish_reason":"stop"}]}`))
	}))
	defer oa.Close()
	an := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &anBody)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"sunny"}],"stop_reason":"end_turn"}`))
	}))
	defer an.Close()

	for _, s := range []Settings{
		{Provider: ProviderOpenAI, OpenAIAPIKey: "k", OpenAIBaseURL: oa.URL},
		{Provider: ProviderAnthropic, AnthropicAPIKey: "k", AnthropicBaseURL: an.URL},
	} {
		ag, err := New(s, Deps{Registry: reg})
		if err != nil {
			t.Fatal(err)
		}
		out, err := ag.Chat(context.Background(), agent.ChatInput{Turns: history})
		if err != nil || out.Reply != "sunny" {
			t.Fatalf("%s: err=%v out=%+v", s.Provider, err, out)
		}
	}
	if msgs, _ := oaBody["messages"].([]any); len(msgs) != 4 {
		t.Errorf("openai body must carry 4 messages (user, assistant, tool, user), got %d", len(msgs))
	}
	if msgs, _ := anBody["messages"].([]any); len(msgs) != 4 {
		t.Errorf("anthropic body must carry 4 messages, got %d", len(msgs))
	}
}
