package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/internal/loop"
	"github.com/Cyprinus12138/dialogram/internal/telemetry"
)

type ctxT = context.Context

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// turnState is the per-call bookkeeping both paths share.
type turnState struct {
	stableSys string
	sysPrompt string
	toolSpec  []map[string]any
	profile   agent.Profile
	memory    string
}

func (p *Provider) prepare(in agent.ChatInput) turnState {
	prof := p.cfg.Resolve(in.Profile)
	return turnState{
		profile:   prof,
		memory:    in.MemoryContext,
		stableSys: loop.ComposeStableSystem(prof.SystemPrompt, p.skills),
		sysPrompt: loop.ComposeSystemPrompt(prof.SystemPrompt, p.skills, in.MemoryContext),
		toolSpec:  p.reg.AnthropicSpecFor(p.cfg.ToolAllow(in)),
	}
}

func (p *Provider) requestBody(st turnState, msgs []message, stream bool) []byte {
	body := map[string]any{
		"model":      p.model,
		"max_tokens": p.maxTokens,
		"messages":   withMessageCacheBreakpoint(msgs),
	}
	if sys := cachedSystem(st.stableSys, st.memory); len(sys) > 0 {
		body["system"] = sys
	}
	if len(st.toolSpec) > 0 {
		body["tools"] = cachedTools(st.toolSpec)
	}
	if stream {
		body["stream"] = true
	}
	b, _ := json.Marshal(body)
	return b
}

// call performs one non-streaming request under a model-call span and
// decodes the response. step is 0-based; the synthesis request passes
// MaxSteps.
func (p *Provider) call(ctx context.Context, body []byte, phase string, step int) (response, error) {
	var parsed response
	ctx, mc := telemetry.StartModelCall(ctx, telemetry.ModelRequest{
		Provider: p.Name(), Model: p.model, Step: step + 1, MaxTokens: p.maxTokens, BaseURL: p.baseURL,
	})
	mc.Attempt()
	req, err := p.newRequest(ctx, body, false)
	if err != nil {
		return parsed, mc.Fail(0, err)
	}
	started := time.Now()
	resp, err := p.http.Do(req)
	if err != nil {
		slog.Warn("anthropic request failed", "phase", phase, "step", step, "err", err, "duration_ms", time.Since(started).Milliseconds())
		return parsed, mc.Fail(0, fmt.Errorf("anthropic request: %w", err))
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	slog.Debug("anthropic response", "phase", phase, "step", step, "status", resp.StatusCode, "bytes", len(raw), "duration_ms", time.Since(started).Milliseconds())
	if resp.StatusCode != http200 {
		return parsed, mc.Fail(resp.StatusCode, fmt.Errorf("anthropic %d: %s", resp.StatusCode, string(raw)))
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return parsed, mc.Fail(0, fmt.Errorf("anthropic decode: %w (body=%s)", err, string(raw)))
	}
	if parsed.Error != nil {
		return parsed, mc.Fail(0, fmt.Errorf("anthropic api error: %s — %s", parsed.Error.Type, parsed.Error.Message))
	}
	var u telemetry.Usage
	if parsed.Usage != nil {
		u = spanUsage(parsed.Usage.InputTokens, parsed.Usage.OutputTokens,
			parsed.Usage.CacheReadInputTokens, parsed.Usage.CacheCreationInputTokens)
	}
	mc.Done(parsed.StopReason, u)
	return parsed, nil
}

// spanUsage converts Messages API usage to span usage. Anthropic reports
// input_tokens without the cache counters; the conventions' input count
// includes them (docs/gen-ai/anthropic.md in the GenAI semconv repo).
func spanUsage(in, out, cacheRead, cacheCreation int) telemetry.Usage {
	return telemetry.Usage{Input: in + cacheRead + cacheCreation, Output: out, CacheRead: cacheRead, CacheWrite: cacheCreation}
}

const http200 = 200

// Chat runs the non-streaming tool loop.
func (p *Provider) Chat(ctx context.Context, in agent.ChatInput) (*agent.ChatOutput, error) {
	turns := in.InputTurns()
	inputLen := len(turns)
	st := p.prepare(in)

	var trace []agent.ToolTrace
	var lastBreakdown agent.TokenBreakdown
	var cacheRead, cacheCreation int

	result := func(reply string) *agent.ChatOutput {
		bd := lastBreakdown
		return &agent.ChatOutput{
			Reply:               reply,
			ToolCalls:           trace,
			Turns:               append([]agent.Turn(nil), turns[inputLen:]...),
			Provider:            p.Name(),
			Model:               p.model,
			TokenBreakdown:      &bd,
			CacheReadTokens:     cacheRead,
			CacheCreationTokens: cacheCreation,
		}
	}

	for step := 0; step < st.profile.MaxSteps(); step++ {
		msgs, err := turnsToMessages(turns)
		if err != nil {
			return nil, err
		}
		lastBreakdown = breakdown(st.sysPrompt, msgs, st.toolSpec)
		loop.LogBreakdown(p.Name(), "chat", step, lastBreakdown)

		parsed, err := p.call(ctx, p.requestBody(st, msgs, false), "chat", step)
		if err != nil {
			return nil, err
		}
		if len(parsed.Content) == 0 {
			return nil, fmt.Errorf("anthropic: empty content")
		}
		if parsed.Usage != nil {
			cacheRead += parsed.Usage.CacheReadInputTokens
			cacheCreation += parsed.Usage.CacheCreationInputTokens
		}
		assistantTurn := responseToTurn(parsed)
		turns = append(turns, assistantTurn)
		p.runner.Checkpoint(ctx, in.MessageID, step+1, turns[inputLen:])

		if parsed.StopReason != "tool_use" || !loop.HasToolUse(assistantTurn) {
			return result(loop.ExtractText(assistantTurn)), nil
		}
		toolTurn, traces, pause := p.runner.HandleTools(ctx, assistantTurn, nil)
		if pause != nil {
			return result(loop.ExtractText(assistantTurn)), nil
		}
		trace = append(trace, traces...)
		turns = append(turns, toolTurn)
		p.runner.Checkpoint(ctx, in.MessageID, step+1, turns[inputLen:])
	}

	// Step cap reached with the model still wanting tools: one tool-free
	// synthesis call so the model wraps up from the data it already has.
	final, _, _, scr, scc, err := p.synthesizeFinal(ctx, st, turns)
	if err != nil {
		return nil, fmt.Errorf("synthesis after step cap: %w", err)
	}
	cacheRead += scr
	cacheCreation += scc
	turns = append(turns, agent.Turn{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockText, Text: final}}})
	p.runner.Checkpoint(ctx, in.MessageID, st.profile.MaxSteps()+1, turns[inputLen:])
	return result(final), nil
}

// synthesizeFinal makes one tool-free completion after the loop hits its
// step cap. Stripping the tools forces the model to write a grounded
// wrap-up from the results it already gathered. The nudge is appended to the
// request only — never persisted into the conversation.
func (p *Provider) synthesizeFinal(ctx context.Context, st turnState, turns []agent.Turn) (text string, inTok, outTok, cacheRead, cacheCreation int, err error) {
	msgs, err := turnsToMessages(turns)
	if err != nil {
		return "", 0, 0, 0, 0, err
	}
	msgs = append(msgs, message{Role: "user", Content: st.profile.Nudge()})
	body := map[string]any{
		"model":      p.model,
		"max_tokens": p.maxTokens,
		"messages":   msgs,
	}
	if sys := cachedSystem(st.stableSys, st.memory); len(sys) > 0 {
		body["system"] = sys
	}
	b, _ := json.Marshal(body)
	parsed, err := p.call(ctx, b, "synthesis", st.profile.MaxSteps())
	if err != nil {
		return "", 0, 0, 0, 0, err
	}
	if parsed.Usage != nil {
		inTok, outTok = parsed.Usage.InputTokens, parsed.Usage.OutputTokens
		cacheRead, cacheCreation = parsed.Usage.CacheReadInputTokens, parsed.Usage.CacheCreationInputTokens
	}
	return loop.ExtractText(responseToTurn(parsed)), inTok, outTok, cacheRead, cacheCreation, nil
}
