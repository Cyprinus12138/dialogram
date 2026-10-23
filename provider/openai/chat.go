package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/internal/loop"
)

type turnState struct {
	sysPrompt string
	toolSpecs []map[string]any
	profile   agent.Profile
}

func (p *Provider) prepare(in agent.ChatInput) turnState {
	prof := p.cfg.Resolve(in.Profile)
	return turnState{
		profile:   prof,
		sysPrompt: loop.ComposeSystemPrompt(prof.SystemPrompt, p.skills, in.MemoryContext),
		toolSpecs: p.reg.OpenAISpecFor(p.cfg.ToolAllow(in)),
	}
}

func (p *Provider) requestBody(st turnState, msgs []message, stream bool) []byte {
	body := map[string]any{
		"model":    p.model,
		"messages": msgs,
		// Set explicitly on both paths: proxies that default low clip a
		// turn mid-reasoning.
		"max_tokens": p.maxTokens,
	}
	if len(st.toolSpecs) > 0 {
		body["tools"] = st.toolSpecs
	}
	if stream {
		body["stream"] = true
		// Most compatible providers honour this to send a final usage
		// chunk; those that do not silently ignore it.
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	b, _ := json.Marshal(body)
	return b
}

func (p *Provider) call(ctx context.Context, body []byte, phase string, step int) (response, error) {
	var parsed response
	req, err := p.newRequest(ctx, body, false)
	if err != nil {
		return parsed, err
	}
	started := time.Now()
	resp, err := p.http.Do(req)
	if err != nil {
		slog.Warn("openai request failed", "phase", phase, "step", step, "err", err, "duration_ms", time.Since(started).Milliseconds())
		return parsed, fmt.Errorf("openai request: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	slog.Debug("openai response", "phase", phase, "step", step, "status", resp.StatusCode, "bytes", len(raw), "duration_ms", time.Since(started).Milliseconds())
	if resp.StatusCode != 200 {
		return parsed, fmt.Errorf("openai %d: %s", resp.StatusCode, string(raw))
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return parsed, fmt.Errorf("openai decode: %w (body=%s)", err, string(raw))
	}
	if parsed.Error != nil {
		return parsed, fmt.Errorf("openai api error: %s — %s", parsed.Error.Type, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return parsed, fmt.Errorf("openai: empty choices")
	}
	return parsed, nil
}

// Chat runs the non-streaming tool loop.
func (p *Provider) Chat(ctx context.Context, in agent.ChatInput) (*agent.ChatOutput, error) {
	turns := in.InputTurns()
	inputLen := len(turns)
	st := p.prepare(in)

	var trace []agent.ToolTrace
	var lastBreakdown agent.TokenBreakdown
	var cacheRead int

	result := func(reply string) *agent.ChatOutput {
		bd := lastBreakdown
		return &agent.ChatOutput{
			Reply:           reply,
			ToolCalls:       trace,
			Turns:           append([]agent.Turn(nil), turns[inputLen:]...),
			Provider:        p.Name(),
			Model:           p.model,
			TokenBreakdown:  &bd,
			CacheReadTokens: cacheRead,
		}
	}

	for step := 0; step < st.profile.MaxSteps(); step++ {
		msgs := turnsToMessages(st.sysPrompt, turns)
		lastBreakdown = breakdown(msgs, st.toolSpecs)
		loop.LogBreakdown(p.Name(), "chat", step, lastBreakdown)

		parsed, err := p.call(ctx, p.requestBody(st, msgs, false), "chat", step)
		if err != nil {
			return nil, err
		}
		if parsed.Usage != nil && parsed.Usage.PromptTokensDetails != nil {
			cacheRead += parsed.Usage.PromptTokensDetails.CachedTokens
		}
		choice := parsed.Choices[0]
		assistantTurn := responseToTurn(choice.Message.Content, choice.Message.ToolCalls)
		turns = append(turns, assistantTurn)

		if len(choice.Message.ToolCalls) == 0 {
			return result(choice.Message.Content), nil
		}
		toolTurn, traces, pause := p.runner.HandleTools(ctx, assistantTurn, nil)
		if pause != nil {
			return result(loop.ExtractText(assistantTurn)), nil
		}
		trace = append(trace, traces...)
		turns = append(turns, toolTurn)
	}

	final, _, _, scr, err := p.synthesizeFinal(ctx, st, turns)
	if err != nil {
		return nil, fmt.Errorf("synthesis after step cap: %w", err)
	}
	cacheRead += scr
	turns = append(turns, agent.Turn{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockText, Text: final}}})
	return result(final), nil
}

// synthesizeFinal makes one tool-free completion after the loop hits its
// step cap. Omitting the tools forces a grounded wrap-up from the results
// already gathered. The nudge is request-only, never persisted.
func (p *Provider) synthesizeFinal(ctx context.Context, st turnState, turns []agent.Turn) (text string, inTok, outTok, cacheRead int, err error) {
	msgs := turnsToMessages(st.sysPrompt, turns)
	msgs = append(msgs, message{Role: "user", Content: st.profile.Nudge()})
	body, _ := json.Marshal(map[string]any{
		"model":      p.model,
		"messages":   msgs,
		"max_tokens": p.maxTokens,
	})
	parsed, err := p.call(ctx, body, "synthesis", 0)
	if err != nil {
		return "", 0, 0, 0, err
	}
	if parsed.Usage != nil {
		inTok, outTok = parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens
		if parsed.Usage.PromptTokensDetails != nil {
			cacheRead = parsed.Usage.PromptTokensDetails.CachedTokens
		}
	}
	return parsed.Choices[0].Message.Content, inTok, outTok, cacheRead, nil
}
