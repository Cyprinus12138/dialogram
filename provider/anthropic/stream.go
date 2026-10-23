package anthropic

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

// ChatStream implements the Messages streaming protocol. Per step it
// forwards each text_delta as a text event, reassembles tool_use blocks,
// hands them to the shared runner, and loops.
//
// Events consumed: message_start (input + cache usage), content_block_start,
// content_block_delta (text_delta / input_json_delta), message_delta
// (stop_reason + output usage), message_stop.
func (p *Provider) ChatStream(ctx context.Context, in agent.ChatInput, emit agent.EmitFn) error {
	turns := in.InputTurns()
	st := p.prepare(in)

	var totalIn, totalOut, totalCacheRead, totalCacheCreation int
	var lastBreakdown agent.TokenBreakdown
	emitUsage := func() {
		bd := lastBreakdown
		emit(agent.StreamEvent{
			Type: agent.EventUsage, InputTokens: totalIn, OutputTokens: totalOut,
			CacheReadTokens: totalCacheRead, CacheCreationTokens: totalCacheCreation, Breakdown: &bd,
		})
	}

	for step := 0; step < st.profile.MaxSteps(); step++ {
		msgs := turnsToMessages(turns)
		lastBreakdown = breakdown(st.sysPrompt, msgs, st.toolSpec)
		loop.LogBreakdown(p.Name(), "stream", step, lastBreakdown)
		body := p.requestBody(st, msgs, true)

		type blk struct {
			typ      string
			text     strings.Builder
			toolID   string
			toolName string
			toolJSON strings.Builder
		}
		var (
			blocks                           map[int]*blk
			maxIdx                           int
			stopReason                       string
			stepIn, stepOut                  int
			stepCacheRead, stepCacheCreation int
			emittedText                      bool
		)

		var lastErr error
		for attempt := 0; attempt <= loop.MaxStreamRetries; attempt++ {
			if attempt > 0 {
				slog.Info("anthropic stream retry", "step", step, "attempt", attempt, "prev_err", lastErr)
				if err := loop.WaitBackoff(ctx, attempt); err != nil {
					return err
				}
			}
			blocks = map[int]*blk{}
			maxIdx = -1
			stopReason = ""
			stepIn, stepOut, stepCacheRead, stepCacheCreation = 0, 0, 0, 0

			req, err := p.newRequest(ctx, body, true)
			if err != nil {
				return err
			}
			started := time.Now()
			resp, err := p.http.Do(req)
			if err != nil {
				slog.Warn("anthropic stream failed", "err", err, "attempt", attempt, "duration_ms", time.Since(started).Milliseconds())
				if loop.IsTransientStreamErr(err) && attempt < loop.MaxStreamRetries && !emittedText {
					lastErr = err
					continue
				}
				return fmt.Errorf("anthropic stream request: %w", err)
			}
			if resp.StatusCode != http200 {
				raw, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				slog.Warn("anthropic stream non-200", "status", resp.StatusCode, "body", string(raw))
				if loop.Retryable(resp.StatusCode) && attempt < loop.MaxStreamRetries && !emittedText {
					lastErr = fmt.Errorf("anthropic %d", resp.StatusCode)
					continue
				}
				return fmt.Errorf("anthropic %d: %s", resp.StatusCode, string(raw))
			}

			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 1<<20), 1<<24)
			var eventType string
			var dataBuf strings.Builder

			flush := func() {
				if eventType == "" && dataBuf.Len() == 0 {
					return
				}
				et, data := eventType, dataBuf.String()
				eventType = ""
				dataBuf.Reset()
				if data == "" {
					return
				}
				var payload map[string]any
				_ = json.Unmarshal([]byte(data), &payload)
				switch et {
				case "message_start":
					if msg, ok := payload["message"].(map[string]any); ok {
						if u, ok := msg["usage"].(map[string]any); ok {
							stepIn = loop.IntFromAny(u["input_tokens"])
							// Cache counters ride in the same usage object;
							// input_tokens excludes them, so they are additive.
							stepCacheRead = loop.IntFromAny(u["cache_read_input_tokens"])
							stepCacheCreation = loop.IntFromAny(u["cache_creation_input_tokens"])
						}
					}
				case "content_block_start":
					idx := loop.IntFromAny(payload["index"])
					cb, _ := payload["content_block"].(map[string]any)
					bk := &blk{typ: loop.AsStr(cb["type"])}
					if bk.typ == "tool_use" {
						bk.toolID = agent.NewToolUseID()
						bk.toolName = loop.AsStr(cb["name"])
					}
					blocks[idx] = bk
					if idx > maxIdx {
						maxIdx = idx
					}
				case "content_block_delta":
					bk := blocks[loop.IntFromAny(payload["index"])]
					if bk == nil {
						return
					}
					delta, _ := payload["delta"].(map[string]any)
					switch loop.AsStr(delta["type"]) {
					case "text_delta":
						text := loop.AsStr(delta["text"])
						bk.text.WriteString(text)
						emit(agent.StreamEvent{Type: agent.EventText, Delta: text})
						emittedText = true
					case "input_json_delta":
						bk.toolJSON.WriteString(loop.AsStr(delta["partial_json"]))
					}
				case "message_delta":
					if delta, ok := payload["delta"].(map[string]any); ok {
						if sr, ok := delta["stop_reason"].(string); ok {
							stopReason = sr
						}
					}
					if u, ok := payload["usage"].(map[string]any); ok {
						stepOut = loop.IntFromAny(u["output_tokens"])
					}
				}
			}

			for scanner.Scan() {
				line := scanner.Text()
				switch {
				case line == "":
					flush()
				case strings.HasPrefix(line, "event: "):
					eventType = strings.TrimPrefix(line, "event: ")
				case strings.HasPrefix(line, "data: "):
					if dataBuf.Len() > 0 {
						dataBuf.WriteByte('\n')
					}
					dataBuf.WriteString(strings.TrimPrefix(line, "data: "))
				}
			}
			flush()
			_ = resp.Body.Close()

			sErr := scanner.Err()
			if sErr == nil {
				lastErr = nil
				break
			}
			if loop.IsTransientStreamErr(sErr) && attempt < loop.MaxStreamRetries && !emittedText {
				lastErr = sErr
				slog.Warn("anthropic stream read transient, retrying", "err", sErr, "attempt", attempt)
				continue
			}
			if emittedText {
				return fmt.Errorf("anthropic stream interrupted after partial reply — please retry: %w", sErr)
			}
			return fmt.Errorf("anthropic stream read: %w", sErr)
		}
		if lastErr != nil {
			return fmt.Errorf("anthropic stream read: %w", lastErr)
		}

		totalIn += stepIn
		totalOut += stepOut
		totalCacheRead += stepCacheRead
		totalCacheCreation += stepCacheCreation

		// Fold the accumulators into a neutral assistant Turn in index
		// order so tool_use blocks replay in the order the model emitted.
		assistantTurn := agent.Turn{Role: agent.RoleAssistant}
		for i := 0; i <= maxIdx; i++ {
			bk := blocks[i]
			if bk == nil {
				continue
			}
			switch bk.typ {
			case "text":
				assistantTurn.Blocks = append(assistantTurn.Blocks, agent.Block{Type: agent.BlockText, Text: bk.text.String()})
			case "tool_use":
				raw := bk.toolJSON.String()
				if raw == "" {
					raw = "{}"
				}
				assistantTurn.Blocks = append(assistantTurn.Blocks, agent.Block{
					Type: agent.BlockToolUse, ToolUseID: bk.toolID, ToolName: bk.toolName, ToolInput: json.RawMessage(raw),
				})
			}
		}
		turns = append(turns, assistantTurn)
		snapshot := assistantTurn
		emit(agent.StreamEvent{Type: agent.EventTurn, Turn: &snapshot})

		if stopReason != "tool_use" || !loop.HasToolUse(assistantTurn) {
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

	final, sin, sout, scr, scc, err := p.synthesizeFinal(ctx, st, turns)
	if err != nil {
		return fmt.Errorf("synthesis after step cap: %w", err)
	}
	totalIn += sin
	totalOut += sout
	totalCacheRead += scr
	totalCacheCreation += scc
	if final != "" {
		emit(agent.StreamEvent{Type: agent.EventText, Delta: final})
		synth := agent.Turn{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockText, Text: final}}}
		emit(agent.StreamEvent{Type: agent.EventTurn, Turn: &synth})
	}
	emitUsage()
	emit(agent.StreamEvent{Type: agent.EventDone})
	return nil
}
