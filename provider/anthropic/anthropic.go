// Package anthropic is the Anthropic Messages API adapter for dialogram.
// It translates the neutral block IR to and from the /v1/messages wire
// shape, places prompt-cache breakpoints, and runs the shared tool loop.
package anthropic

import (
	"encoding/json"
	"fmt"
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
	// Model defaults to DefaultModel.
	Model string
	// BaseURL defaults to https://api.anthropic.com. Point it at a proxy
	// that speaks the Messages API if needed.
	BaseURL string
	// Version is the anthropic-version header; defaults to "2023-06-01".
	Version string
	// MaxTokens is the per-turn output cap; defaults to loop.DefaultMaxTokens.
	MaxTokens int
	// HTTPClient overrides the shared streaming-safe client.
	HTTPClient *http.Client

	Registry     *tool.Registry
	Config       *agent.Config
	Interceptors []agent.Interceptor
	Skills       *agent.SkillLibrary
	// Executor runs tool calls. nil selects tool.NewLocalExecutor with the
	// default limits (60 s per call, 64 KiB observation, 8 in parallel).
	Executor tool.Executor
	// Checkpointer, when non-nil, receives the run's block IR at every step
	// boundary (see agent.Checkpoint).
	Checkpointer agent.Checkpointer
}

// DefaultModel is used when Options.Model is empty.
const DefaultModel = "claude-sonnet-4-6"

// Provider implements agent.Provider over the Anthropic Messages API.
type Provider struct {
	apiKey    string
	model     string
	baseURL   string
	version   string
	maxTokens int
	http      *http.Client
	reg       *tool.Registry
	cfg       *agent.Config
	skills    *agent.SkillLibrary
	runner    loop.Runner
}

// New builds a Provider.
func New(o Options) *Provider {
	if o.Model == "" {
		o.Model = DefaultModel
	}
	if o.BaseURL == "" {
		o.BaseURL = "https://api.anthropic.com"
	}
	if o.Version == "" {
		o.Version = "2023-06-01"
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
	if o.Executor == nil {
		o.Executor = tool.NewLocalExecutor(o.Registry, tool.LocalExecutorOptions{})
	}
	return &Provider{
		apiKey:    o.APIKey,
		model:     o.Model,
		baseURL:   strings.TrimRight(o.BaseURL, "/"),
		version:   o.Version,
		maxTokens: o.MaxTokens,
		http:      o.HTTPClient,
		reg:       o.Registry,
		cfg:       o.Config,
		skills:    o.Skills,
		runner:    loop.Runner{Cfg: o.Config, Reg: o.Registry, Interceptors: o.Interceptors, Executor: o.Executor, Checkpointer: o.Checkpointer},
	}
}

// Name returns "anthropic".
func (p *Provider) Name() string { return "anthropic" }

// Model returns the configured model id.
func (p *Provider) Model() string { return p.model }

// SetHTTPClient swaps the HTTP client (tests inject a scripted transport).
func (p *Provider) SetHTTPClient(c *http.Client) { p.http = c }

func (p *Provider) newRequest(ctx ctxT, body []byte, stream bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytesReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", p.version)
	req.Header.Set("content-type", "application/json")
	if stream {
		req.Header.Set("accept", "text/event-stream")
	}
	return req, nil
}

// ---------------- wire shapes ----------------

type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
}

type response struct {
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Usage *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage,omitempty"`
}

// cachedSystem emits the system prompt as structured blocks: a STABLE block
// marked cache_control:ephemeral, followed (when present) by the per-turn
// VOLATILE block with NO cache_control.
//
// Anthropic caches by longest common prefix up to the breakpoint; the
// marker sits on the stable block, so the (large) base prompt + skills stay
// cache-eligible turn to turn even when the volatile block drifts. The
// volatile block lives after the breakpoint, so a state change re-reads
// only that small tail instead of busting the whole prompt. The volatile
// block carries loop.MemorySeparator as a leading join so the model sees
// the same text it would from the single concatenated string.
func cachedSystem(stable, memory string) []map[string]any {
	var blocks []map[string]any
	if stable != "" {
		blocks = append(blocks, map[string]any{
			"type":          "text",
			"text":          stable,
			"cache_control": map[string]string{"type": "ephemeral"},
		})
	}
	if memory != "" {
		text := memory
		if stable != "" {
			text = loop.MemorySeparator + memory
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	return blocks
}

// withMessageCacheBreakpoint returns a copy of msgs with
// cache_control:ephemeral stamped on the LAST content block of the LAST
// message, so Anthropic caches the whole conversation prefix across
// tool-loop steps. Within a single turn the loop re-POSTs the entire
// messages array up to MaxSteps times; without this every step re-pays
// full input price for all prior turns plus accumulated tool_results.
//
// A string Content is promoted to a one-element structured block so the
// marker has somewhere to live. The input slice and shared inner maps are
// never mutated. Breakpoint budget: stable system (1) + tools (1) + this
// message (1) = 3, within the 4-breakpoint cap.
func withMessageCacheBreakpoint(msgs []message) []message {
	if len(msgs) == 0 {
		return msgs
	}
	out := make([]message, len(msgs))
	copy(out, msgs)
	last := out[len(out)-1]

	var blocks []map[string]any
	switch c := last.Content.(type) {
	case string:
		blocks = []map[string]any{{"type": "text", "text": c}}
	case []map[string]any:
		if len(c) == 0 {
			return out
		}
		blocks = make([]map[string]any, len(c))
		copy(blocks, c)
		lastBlk := blocks[len(blocks)-1]
		cp := make(map[string]any, len(lastBlk)+1)
		for k, v := range lastBlk {
			cp[k] = v
		}
		blocks[len(blocks)-1] = cp
	default:
		return out
	}
	blocks[len(blocks)-1]["cache_control"] = map[string]string{"type": "ephemeral"}
	out[len(out)-1] = message{Role: last.Role, Content: blocks}
	return out
}

// cachedTools clones the spec slice and stamps cache_control on the last
// tool entry. Anthropic treats cache_control as "cache up to and including
// this block", so one mark on the last tool caches the whole array.
func cachedTools(spec []map[string]any) []map[string]any {
	if len(spec) == 0 {
		return spec
	}
	out := make([]map[string]any, len(spec))
	copy(out, spec)
	last := out[len(out)-1]
	cp := make(map[string]any, len(last)+1)
	for k, v := range last {
		cp[k] = v
	}
	cp["cache_control"] = map[string]string{"type": "ephemeral"}
	out[len(out)-1] = cp
	return out
}

// turnsToMessages translates the neutral Turn IR into the /v1/messages
// wire shape. The system prompt is carried separately on the request.
//
//   - "user" with a single text block collapses to string Content; any
//     other mix of text and image blocks is a content array.
//   - "assistant" always uses structured content so text and tool_use
//     blocks round-trip correctly.
//   - "tool" becomes a user-role message whose content is a list of
//     tool_result blocks — Anthropic's convention for replying to a
//     preceding assistant tool_use turn. Only model_observation is sent.
//
// An image block outside a user turn, or one that fails loop.CheckImage,
// is an error: the request would be rejected upstream with a less useful
// message.
func turnsToMessages(turns []agent.Turn) ([]message, error) {
	out := make([]message, 0, len(turns))
	for _, t := range turns {
		switch t.Role {
		case agent.RoleUser:
			if len(t.Blocks) == 1 && t.Blocks[0].Type == agent.BlockText {
				out = append(out, message{Role: "user", Content: t.Blocks[0].Text})
				continue
			}
			content := make([]map[string]any, 0, len(t.Blocks))
			for _, b := range t.Blocks {
				switch b.Type {
				case agent.BlockText:
					content = append(content, map[string]any{"type": "text", "text": b.Text})
				case agent.BlockImage:
					if err := loop.CheckImage(t.Role, b); err != nil {
						return nil, fmt.Errorf("anthropic: %w", err)
					}
					content = append(content, imageContent(b))
				}
			}
			out = append(out, message{Role: "user", Content: content})
		case agent.RoleAssistant:
			content := make([]map[string]any, 0, len(t.Blocks))
			for _, b := range t.Blocks {
				switch b.Type {
				case agent.BlockImage:
					return nil, fmt.Errorf("anthropic: %w", loop.CheckImage(t.Role, b))
				case agent.BlockText:
					content = append(content, map[string]any{"type": "text", "text": b.Text})
				case agent.BlockToolUse:
					var input any
					if len(b.ToolInput) == 0 {
						input = map[string]any{}
					} else {
						_ = json.Unmarshal(b.ToolInput, &input)
						if input == nil {
							input = map[string]any{}
						}
					}
					content = append(content, map[string]any{
						"type": "tool_use", "id": b.ToolUseID, "name": b.ToolName, "input": input,
					})
				}
			}
			out = append(out, message{Role: "assistant", Content: content})
		case agent.RoleTool:
			content := make([]map[string]any, 0, len(t.Blocks))
			for _, b := range t.Blocks {
				if b.Type == agent.BlockImage {
					return nil, fmt.Errorf("anthropic: %w", loop.CheckImage(t.Role, b))
				}
				if b.Type != agent.BlockToolResult {
					continue
				}
				obs := tool.ProjectModelObservation(b.ToolResult)
				content = append(content, map[string]any{
					"type":        "tool_result",
					"tool_use_id": b.ToolUseID,
					"content":     string(obs),
				})
			}
			out = append(out, message{Role: "user", Content: content})
		}
	}
	return out, nil
}

// imageContent renders a validated image block as a Messages API image
// part: a base64 source for inline data, a url source otherwise.
func imageContent(b agent.Block) map[string]any {
	if b.URL != "" {
		return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": b.URL}}
	}
	return map[string]any{"type": "image", "source": map[string]any{
		"type": "base64", "media_type": b.MediaType, "data": b.Data,
	}}
}

// responseToTurn converts a Messages response into an assistant Turn. The
// native tool_use id is discarded and replaced with agent.NewToolUseID.
func responseToTurn(parsed response) agent.Turn {
	blocks := make([]agent.Block, 0, len(parsed.Content))
	for _, c := range parsed.Content {
		switch c.Type {
		case "text":
			blocks = append(blocks, agent.Block{Type: agent.BlockText, Text: c.Text})
		case "tool_use":
			input := c.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			blocks = append(blocks, agent.Block{
				Type: agent.BlockToolUse, ToolUseID: agent.NewToolUseID(), ToolName: c.Name, ToolInput: input,
			})
		}
	}
	return agent.Turn{Role: agent.RoleAssistant, Blocks: blocks}
}

// breakdown classifies each message into TokenBreakdown buckets.
func breakdown(systemPrompt string, msgs []message, toolSpec []map[string]any) agent.TokenBreakdown {
	lastUser := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		if _, ok := msgs[i].Content.(string); ok {
			lastUser = i
			break
		}
	}
	var b agent.TokenBreakdown
	b.System = loop.EstimateTokens(systemPrompt)
	for i, m := range msgs {
		isToolCalling := false
		var s string
		switch c := m.Content.(type) {
		case string:
			s = c
		case []map[string]any:
			for _, blk := range c {
				if t, ok := blk["type"].(string); ok && (t == "tool_use" || t == "tool_result") {
					isToolCalling = true
					break
				}
			}
			j, _ := json.Marshal(c)
			s = string(j)
		default:
			j, _ := json.Marshal(c)
			s = string(j)
		}
		n := loop.EstimateTokens(s)
		switch {
		case isToolCalling:
			b.ToolCalling += n
		case i == lastUser:
			b.UserPrompt += n
		default:
			b.History += n
		}
	}
	if len(toolSpec) > 0 {
		j, _ := json.Marshal(toolSpec)
		b.Tools = loop.EstimateTokens(string(j))
	}
	b.Total = b.System + b.Tools + b.History + b.UserPrompt + b.ToolCalling
	return b
}
