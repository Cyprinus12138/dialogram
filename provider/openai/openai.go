// Package openai is the OpenAI Chat Completions adapter for dialogram. Any
// endpoint that speaks this wire format works — OpenAI, Azure OpenAI,
// OpenRouter, Groq, DeepSeek, Together, Fireworks, Mistral, vLLM, Ollama's
// OpenAI shim, etc. Base URL and model are configurable.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/internal/loop"
	"github.com/Cyprinus12138/dialogram/tool"
)

// Options configure a Provider. APIKey and Registry are required; every
// other field has a default.
type Options struct {
	APIKey string
	// BaseURL defaults to https://api.openai.com/v1.
	BaseURL string
	// Model defaults to DefaultModel.
	Model string
	// MaxTokens is the per-turn output cap; defaults to loop.DefaultMaxTokens.
	MaxTokens int
	// HTTPClient overrides the shared streaming-safe client.
	HTTPClient *http.Client

	Registry     *tool.Registry
	Config       *agent.Config
	Interceptors []agent.Interceptor
	Skills       *agent.SkillLibrary
}

// DefaultModel is used when Options.Model is empty.
const DefaultModel = "gpt-4o-mini"

// Provider implements agent.Provider over the Chat Completions API.
type Provider struct {
	apiKey    string
	baseURL   string
	model     string
	maxTokens int
	http      *http.Client
	reg       *tool.Registry
	cfg       *agent.Config
	skills    *agent.SkillLibrary
	runner    loop.Runner
}

// New builds a Provider.
func New(o Options) *Provider {
	if o.BaseURL == "" {
		o.BaseURL = "https://api.openai.com/v1"
	}
	if o.Model == "" {
		o.Model = DefaultModel
	}
	if o.MaxTokens <= 0 {
		o.MaxTokens = loop.DefaultMaxTokens
	}
	if o.HTTPClient == nil {
		o.HTTPClient = loop.NewHTTPClient()
	}
	if o.Registry == nil {
		o.Registry = tool.NewRegistry()
	}
	return &Provider{
		apiKey:    o.APIKey,
		baseURL:   strings.TrimRight(o.BaseURL, "/"),
		model:     o.Model,
		maxTokens: o.MaxTokens,
		http:      o.HTTPClient,
		reg:       o.Registry,
		cfg:       o.Config,
		skills:    o.Skills,
		runner:    loop.Runner{Cfg: o.Config, Reg: o.Registry, Interceptors: o.Interceptors},
	}
}

// Name returns "openai".
func (p *Provider) Name() string { return "openai" }

// Model returns the configured model id.
func (p *Provider) Model() string { return p.model }

// SetHTTPClient swaps the HTTP client (tests inject a scripted transport).
func (p *Provider) SetHTTPClient(c *http.Client) { p.http = c }

func (p *Provider) newRequest(ctx context.Context, body []byte, stream bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("content-type", "application/json")
	if stream {
		req.Header.Set("accept", "text/event-stream")
	}
	return req, nil
}

// ---------------- wire shapes ----------------

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type response struct {
	Choices []struct {
		Message struct {
			Role      string     `json:"role"`
			Content   string     `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Usage *usage `json:"usage,omitempty"`
}

// turnsToMessages translates the neutral Turn IR into the chat/completions
// wire shape. OpenAI carries the system prompt as the first message, so the
// translator prepends it.
//
//   - "user"      — one message per user Turn; multi-text blocks concatenate.
//   - "assistant" — one message; text blocks concatenate into Content,
//     tool_use blocks become ToolCalls. Content is nil (not "") when only
//     tool_calls are present — OpenAI requires this.
//   - "tool"      — expanded into N messages (role "tool"), one per
//     tool_result block. Only model_observation is sent.
func turnsToMessages(sysPrompt string, turns []agent.Turn) []message {
	out := make([]message, 0, len(turns)+1)
	if sysPrompt != "" {
		out = append(out, message{Role: "system", Content: sysPrompt})
	}
	for _, t := range turns {
		switch t.Role {
		case agent.RoleUser:
			var text string
			for _, b := range t.Blocks {
				if b.Type == agent.BlockText {
					text += b.Text
				}
			}
			out = append(out, message{Role: "user", Content: text})
		case agent.RoleAssistant:
			var text string
			var calls []toolCall
			for _, b := range t.Blocks {
				switch b.Type {
				case agent.BlockText:
					text += b.Text
				case agent.BlockToolUse:
					args := string(b.ToolInput)
					if args == "" {
						args = "{}"
					}
					tc := toolCall{ID: b.ToolUseID, Type: "function"}
					tc.Function.Name = b.ToolName
					tc.Function.Arguments = args
					calls = append(calls, tc)
				}
			}
			msg := message{Role: "assistant"}
			switch {
			case text != "":
				msg.Content = text
			case len(calls) == 0:
				msg.Content = ""
			default:
				msg.Content = nil
			}
			if len(calls) > 0 {
				msg.ToolCalls = calls
			}
			out = append(out, msg)
		case agent.RoleTool:
			for _, b := range t.Blocks {
				if b.Type != agent.BlockToolResult {
					continue
				}
				obs := tool.ProjectModelObservation(b.ToolResult)
				out = append(out, message{Role: "tool", ToolCallID: b.ToolUseID, Name: b.ToolName, Content: string(obs)})
			}
		}
	}
	return out
}

// responseToTurn converts a non-streaming choice into an assistant Turn.
// The native tool_call id is discarded and replaced with agent.NewToolUseID.
func responseToTurn(content string, toolCalls []toolCall) agent.Turn {
	blocks := make([]agent.Block, 0, 1+len(toolCalls))
	if content != "" {
		blocks = append(blocks, agent.Block{Type: agent.BlockText, Text: content})
	}
	for _, tc := range toolCalls {
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		blocks = append(blocks, agent.Block{
			Type: agent.BlockToolUse, ToolUseID: agent.NewToolUseID(), ToolName: tc.Function.Name, ToolInput: json.RawMessage(args),
		})
	}
	return agent.Turn{Role: agent.RoleAssistant, Blocks: blocks}
}

// breakdown classifies each prepared message into TokenBreakdown buckets.
func breakdown(msgs []message, toolSpecs []map[string]any) agent.TokenBreakdown {
	lastUser := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUser = i
			break
		}
	}
	var b agent.TokenBreakdown
	for i, m := range msgs {
		var s string
		if m.Content != nil {
			switch c := m.Content.(type) {
			case string:
				s = c
			default:
				j, _ := json.Marshal(c)
				s = string(j)
			}
		}
		n := loop.EstimateTokens(s)
		if len(m.ToolCalls) > 0 {
			j, _ := json.Marshal(m.ToolCalls)
			n += loop.EstimateTokens(string(j))
		}
		switch {
		case m.Role == "system":
			b.System += n
		case m.Role == "tool", m.Role == "assistant" && len(m.ToolCalls) > 0:
			b.ToolCalling += n
		case i == lastUser:
			b.UserPrompt += n
		default:
			b.History += n
		}
	}
	if len(toolSpecs) > 0 {
		j, _ := json.Marshal(toolSpecs)
		b.Tools = loop.EstimateTokens(string(j))
	}
	b.Total = b.System + b.Tools + b.History + b.UserPrompt + b.ToolCalling
	return b
}
