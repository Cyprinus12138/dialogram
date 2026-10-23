package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/internal/loop"
)

type toolAcc struct{ ID, Name, Args string }

// ChatStream implements the Chat Completions streaming protocol: `data:
// {json}` per event, ending with `data: [DONE]`. Each chunk's delta may
// carry incremental content or partial tool_calls where arguments is a
// growing string and id/name appear once. Tool calls are accumulated by
// index and handed to the shared runner on finish_reason=tool_calls.
func (p *Provider) ChatStream(ctx context.Context, in agent.ChatInput, emit agent.EmitFn) error {
	turns := in.InputTurns()
	st := p.prepare(in)

	var totalIn, totalOut, totalCacheRead int
	var lastBreakdown agent.TokenBreakdown
	emitUsage := func() {
		bd := lastBreakdown
		emit(agent.StreamEvent{
			Type: agent.EventUsage, InputTokens: totalIn, OutputTokens: totalOut,
			CacheReadTokens: totalCacheRead, Breakdown: &bd,
		})
	}

	for step := 0; step < st.profile.MaxSteps(); step++ {
		msgs := turnsToMessages(st.sysPrompt, turns)
		lastBreakdown = breakdown(msgs, st.toolSpecs)
		loop.LogBreakdown(p.Name(), "stream", step, lastBreakdown)
		body := p.requestBody(st, msgs, true)

		var (
			contentBuf      strings.Builder
			toolCalls       map[int]*toolAcc
			toolIdxOrder    []int
			stepIn, stepOut int
			stepCacheRead   int
			emittedText     bool
		)

		var lastErr error
		for attempt := 0; attempt <= loop.MaxStreamRetries; attempt++ {
			if attempt > 0 {
				slog.Info("openai stream retry", "step", step, "attempt", attempt, "prev_err", lastErr)
				if err := loop.WaitBackoff(ctx, attempt); err != nil {
					return err
				}
			}
			contentBuf.Reset()
			toolCalls = map[int]*toolAcc{}
			toolIdxOrder = nil
			stepIn, stepOut, stepCacheRead = 0, 0, 0

			req, err := p.newRequest(ctx, body, true)
			if err != nil {
				return err
			}
			started := time.Now()
			resp, err := p.http.Do(req)
			if err != nil {
				slog.Warn("openai stream failed", "err", err, "attempt", attempt, "duration_ms", time.Since(started).Milliseconds())
				if loop.IsTransientStreamErr(err) && attempt < loop.MaxStreamRetries && !emittedText {
					lastErr = err
					continue
				}
				return fmt.Errorf("openai stream request: %w", err)
			}
			if resp.StatusCode != 200 {
				raw, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				slog.Warn("openai stream non-200", "status", resp.StatusCode, "body", string(raw))
				if loop.Retryable(resp.StatusCode) && attempt < loop.MaxStreamRetries && !emittedText {
					lastErr = fmt.Errorf("openai %d", resp.StatusCode)
					continue
				}
				return fmt.Errorf("openai %d: %s", resp.StatusCode, string(raw))
			}

			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 1<<20), 1<<24)
			for scanner.Scan() {
				line := scanner.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				data := strings.TrimPrefix(line, "data: ")
				if data == "[DONE]" {
					break
				}
				var chunk struct {
					Choices []struct {
						Delta struct {
							Content   string `json:"content"`
							ToolCalls []struct {
								Index    int    `json:"index"`
								ID       string `json:"id"`
								Type     string `json:"type"`
								Function struct {
									Name      string `json:"name"`
									Arguments string `json:"arguments"`
								} `json:"function"`
							} `json:"tool_calls"`
						} `json:"delta"`
						FinishReason string `json:"finish_reason"`
					} `json:"choices"`
					Usage *usage `json:"usage"`
				}
				if err := json.Unmarshal([]byte(data), &chunk); err != nil {
					continue
				}
				if chunk.Usage != nil {
					stepIn, stepOut = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
					if chunk.Usage.PromptTokensDetails != nil {
						stepCacheRead = chunk.Usage.PromptTokensDetails.CachedTokens
					}
				}
				if len(chunk.Choices) == 0 {
					continue
				}
				ch := chunk.Choices[0]
				if ch.Delta.Content != "" {
					contentBuf.WriteString(ch.Delta.Content)
					emit(agent.StreamEvent{Type: agent.EventText, Delta: ch.Delta.Content})
					emittedText = true
				}
				for _, tc := range ch.Delta.ToolCalls {
					cur, ok := toolCalls[tc.Index]
					if !ok {
						cur = &toolAcc{ID: agent.NewToolUseID()}
						toolCalls[tc.Index] = cur
						toolIdxOrder = append(toolIdxOrder, tc.Index)
					}
					if tc.Function.Name != "" {
						cur.Name = tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						cur.Args += tc.Function.Arguments
					}
				}
			}
			_ = resp.Body.Close()

			sErr := scanner.Err()
			if sErr == nil {
				lastErr = nil
				break
			}
			// Retry silently only if nothing has been streamed yet —
			// otherwise the next attempt would duplicate text.
			if loop.IsTransientStreamErr(sErr) && attempt < loop.MaxStreamRetries && !emittedText {
				lastErr = sErr
				slog.Warn("openai stream read transient, retrying", "err", sErr, "attempt", attempt)
				continue
			}
			if emittedText {
				return fmt.Errorf("openai stream interrupted after partial reply — please retry: %w", sErr)
			}
			return fmt.Errorf("openai stream read: %w", sErr)
		}
		if lastErr != nil {
			return fmt.Errorf("openai stream read: %w", lastErr)
		}

		totalIn += stepIn
		totalOut += stepOut
		totalCacheRead += stepCacheRead

		assistantTurn := agent.Turn{Role: agent.RoleAssistant}
		if contentBuf.Len() > 0 {
			assistantTurn.Blocks = append(assistantTurn.Blocks, agent.Block{Type: agent.BlockText, Text: contentBuf.String()})
		}
		for _, idx := range toolIdxOrder {
			cur := toolCalls[idx]
			args := cur.Args
			if args == "" {
				args = "{}"
			}
			assistantTurn.Blocks = append(assistantTurn.Blocks, agent.Block{
				Type: agent.BlockToolUse, ToolUseID: cur.ID, ToolName: cur.Name, ToolInput: json.RawMessage(args),
			})
		}
		turns = append(turns, assistantTurn)
		snapshot := assistantTurn
		emit(agent.StreamEvent{Type: agent.EventTurn, Turn: &snapshot})

		if len(toolIdxOrder) == 0 {
			emitUsage()
			emit(agent.StreamEvent{Type: agent.EventDone})
			return nil
		}
		toolTurn, _, pause := p.runner.HandleTools(ctx, assistantTurn, emit)
		if pause != nil {
			emitUsage()
			emit(*pause)
			return nil
		}
		turns = append(turns, toolTurn)
		toolSnapshot := toolTurn
		emit(agent.StreamEvent{Type: agent.EventTurn, Turn: &toolSnapshot})
	}

	final, sin, sout, scr, err := p.synthesizeFinal(ctx, st, turns)
	if err != nil {
		return fmt.Errorf("synthesis after step cap: %w", err)
	}
	totalIn += sin
	totalOut += sout
	totalCacheRead += scr
	if final != "" {
		emit(agent.StreamEvent{Type: agent.EventText, Delta: final})
		synth := agent.Turn{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockText, Text: final}}}
		emit(agent.StreamEvent{Type: agent.EventTurn, Turn: &synth})
	}
	emitUsage()
	emit(agent.StreamEvent{Type: agent.EventDone})
	return nil
}
