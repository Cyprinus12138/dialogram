// Package loop holds the provider-neutral machinery both wire translators
// share: tool dispatch through the interceptor chain, pause handling for
// mixed turns, system-prompt composition, retry/backoff and token
// estimation. It is internal — applications use provider/anthropic and
// provider/openai, which are thin wire adapters over this package.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/internal/telemetry"
	"github.com/Cyprinus12138/dialogram/tool"
)

const (
	// DefaultMaxTokens is the per-turn output cap sent to every provider on
	// both paths. It must be set explicitly: when omitted, several
	// OpenAI-compatible proxies fall back to a low default (~1024) that
	// clips a turn mid-reasoning.
	DefaultMaxTokens = 4096
	// MaxStreamRetries is the number of additional attempts on transient
	// streaming failures before any text has been emitted.
	MaxStreamRetries = 2
	// PauseCoRequestErrorCode is stamped on the synthetic tool_result fed
	// back for a pause tool the model co-requested alongside regular tools.
	PauseCoRequestErrorCode = "PAUSE_COREQUESTED_WITH_TOOLS"
)

// Dispatched carries one tool_use block's resolution after the interceptor
// chain and (parallel) execution.
type Dispatched struct {
	Block   agent.Block
	RawJSON string
	Out     string
}

// Dispatch runs the interceptor chain on every tool_use block, then
// executes the surviving tools concurrently through exec and waits for all
// to finish. Results preserve the original block order so callers
// emit/trace in the model's intended sequence.
//
// A nil exec selects tool.NewLocalExecutor over reg with default limits.
// When exec implements tool.ConcurrencyLimiter, at most that many calls of
// this step run at once; the rest queue. Every surviving block gets a
// result — an executor error becomes a HARD_FAILURE body, and a call still
// queued when ctx ends is failed the same way rather than dropped — so the
// tool turn is always complete.
//
// If any interceptor decides InterceptPause, Dispatch returns the offending
// block and reason with nil results; the caller short-circuits the step.
// Tools are assumed independent — a tool that needs a prior tool's result
// in the same turn has to be issued by the model in a separate turn anyway.
//
// Every result Dispatch produces gets one execute_tool span, a child of
// the span in ctx: an executed call's span starts once it holds a
// semaphore slot (the wait is dialogram.tool.queue_ms) and wraps the
// executor call; a denied call's span is a zero-work one with outcome
// denied. The span's context reaches the executor, so spans a tool starts
// nest under it.
func Dispatch(ctx context.Context, interceptors []agent.Interceptor, reg *tool.Registry, exec tool.Executor, blocks []agent.Block) (results []Dispatched, pauseBlock *agent.Block, pauseReason string) {
	type job struct {
		block   agent.Block
		rawJSON string
		denied  string
	}
	var jobs []job
	for _, b := range blocks {
		if b.Type != agent.BlockToolUse {
			continue
		}
		raw := string(b.ToolInput)
		if raw == "" {
			raw = "{}"
		}
		decision := agent.RunInterceptors(ctx, interceptors, b)
		switch decision.Action {
		case agent.InterceptPause:
			bb := b
			return nil, &bb, decision.Reason
		case agent.InterceptDeny:
			jobs = append(jobs, job{block: b, rawJSON: raw, denied: agent.DeniedResultJSON(decision.Reason)})
		default:
			jobs = append(jobs, job{block: b, rawJSON: raw})
		}
	}
	if exec == nil {
		exec = tool.NewLocalExecutor(reg, tool.LocalExecutorOptions{})
	}
	var store tool.ArtifactStore
	if reg != nil {
		store = reg.Artifacts()
	}
	limit := len(jobs)
	if cl, ok := exec.(tool.ConcurrencyLimiter); ok {
		if n := cl.MaxConcurrency(); n > 0 && n < limit {
			limit = n
		}
	}
	sem := make(chan struct{}, limit)
	results = make([]Dispatched, len(jobs))
	var wg sync.WaitGroup
	for i := range jobs {
		j := jobs[i]
		results[i].Block = j.block
		results[i].RawJSON = j.rawJSON
		if j.denied != "" {
			results[i].Out = j.denied
			_, span := telemetry.StartTool(ctx, j.block.ToolName, j.block.ToolUseID, 0)
			telemetry.EndTool(span, j.denied, nil)
			continue
		}
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			queued := time.Now()
			var err error
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				// Do not start new work on a context that ended while
				// this call was queued; fail it the same way.
				err = ctx.Err()
			case <-ctx.Done():
				err = ctx.Err()
			}
			spanCtx, span := telemetry.StartTool(ctx, j.block.ToolName, j.block.ToolUseID, time.Since(queued))
			var out string
			if err == nil {
				callCtx := tool.WithCallContext(spanCtx, j.block.ToolUseID, store)
				out, err = exec.Execute(callCtx, tool.Call{ToolUseID: j.block.ToolUseID, Name: j.block.ToolName, Input: json.RawMessage(j.rawJSON)})
			}
			if err != nil {
				out = HardFailureJSON(err)
			}
			telemetry.EndTool(span, out, err)
			results[i].Out = out
		}(i, j)
	}
	wg.Wait()
	return results, nil, ""
}

// HardFailureJSON formats a Go-level tool error as a tool_result body in the
// soft-failure shape the model already knows how to read.
func HardFailureJSON(err error) string {
	return fmt.Sprintf(`{"error_code":%q,"error_message":%q}`, tool.ErrorCodeHardFailure, err.Error())
}

// PauseDeferredResultJSON is the tool_result body fed back for a pause tool
// the model co-requested alongside regular tool calls in one assistant
// turn. The regular calls already ran — their results are attached in the
// same tool turn — so this tells the model, deterministically rather than
// via a prompt nudge, to re-issue the pause on its own turn if it still
// needs the user.
func PauseDeferredResultJSON(toolName string) string {
	b, _ := json.Marshal(map[string]any{
		"error_code": PauseCoRequestErrorCode,
		"error_message": fmt.Sprintf(
			"%s cannot run in the same turn as other tool calls. The other tools in this turn were executed and their results follow. If you still need input from the user, call %s on its own in your next turn.",
			toolName, toolName),
	})
	return string(b)
}

// DeferredPauseResultBlocks appends one synthetic tool_result per
// co-requested pause tool_use so that EVERY tool_use in a mixed turn is
// paired before the loop continues — both wire protocols reject a follow-up
// request that leaves a tool_use without a matching tool_result.
func DeferredPauseResultBlocks(blocks []agent.Block, pauseBlocks []agent.Block) []agent.Block {
	for _, pb := range pauseBlocks {
		blocks = append(blocks, agent.Block{
			Type:       agent.BlockToolResult,
			ToolUseID:  pb.ToolUseID,
			ToolName:   pb.ToolName,
			ToolResult: json.RawMessage(PauseDeferredResultJSON(pb.ToolName)),
		})
	}
	return blocks
}

// ToolTurn folds dispatch results into a tool-role Turn.
func ToolTurn(results []Dispatched) agent.Turn {
	t := agent.Turn{Role: agent.RoleTool}
	for _, r := range results {
		t.Blocks = append(t.Blocks, agent.Block{
			Type:       agent.BlockToolResult,
			ToolUseID:  r.Block.ToolUseID,
			ToolName:   r.Block.ToolName,
			ToolResult: json.RawMessage(r.Out),
		})
	}
	return t
}

// Traces converts dispatch results into ToolTraces.
func Traces(results []Dispatched) []agent.ToolTrace {
	out := make([]agent.ToolTrace, 0, len(results))
	for _, r := range results {
		out = append(out, agent.ToolTrace{Name: r.Block.ToolName, Input: json.RawMessage(r.RawJSON), Output: r.Out})
	}
	return out
}

// EmitToolCalls emits a tool_call event for every tool_use block, in model
// order, so a UI renders every spinner immediately — independent of
// interceptor decisions (a denied tool still shows its call; its synthetic
// result follows).
func EmitToolCalls(emit agent.EmitFn, reg *tool.Registry, blocks []agent.Block) {
	for _, b := range blocks {
		if b.Type != agent.BlockToolUse {
			continue
		}
		raw := string(b.ToolInput)
		if raw == "" {
			raw = "{}"
		}
		emit(agent.StreamEvent{Type: agent.EventToolCall, Name: b.ToolName, Label: reg.Label(b.ToolName), ToolUseID: b.ToolUseID, Input: json.RawMessage(raw)})
	}
}

// EmitToolResults emits a tool_result event per dispatch result, in order.
func EmitToolResults(emit agent.EmitFn, results []Dispatched) {
	for _, r := range results {
		emit(agent.StreamEvent{Type: agent.EventToolResult, Name: r.Block.ToolName, ToolUseID: r.Block.ToolUseID, Input: json.RawMessage(r.RawJSON), Output: r.Out})
	}
}

// ApprovalPauseEvent renders an interceptor pause as its waiting_for_user
// event, with the intercepted tool's name and input stashed so the
// application can dispatch it verbatim on approve.
func ApprovalPauseEvent(b *agent.Block, reason string) agent.StreamEvent {
	raw := string(b.ToolInput)
	if raw == "" {
		raw = "{}"
	}
	return agent.StreamEvent{
		Type:      agent.EventWaitingForUser,
		Kind:      agent.PauseKindApproval,
		ToolUseID: b.ToolUseID,
		Name:      b.ToolName,
		Input:     json.RawMessage(raw),
		Message:   reason,
	}
}

// CheckImage validates an image block for the turn role it sits in. Both
// adapters apply the same rule, so a conversation one provider accepts is
// never rejected by the other on resume: images are valid in user turns
// only, carry exactly one of Data or URL, and an inline image names one of
// the media types agent.IsImageMediaType accepts.
func CheckImage(role string, b agent.Block) error {
	if role != agent.RoleUser {
		return fmt.Errorf("image block in %s turn: images are only valid in user turns", role)
	}
	switch {
	case b.Data != "" && b.URL != "":
		return errors.New("image block sets both data and url: set exactly one")
	case b.Data == "" && b.URL == "":
		return errors.New("image block sets neither data nor url: set exactly one")
	case b.Data != "" && b.MediaType == "":
		return errors.New("inline image block needs a media type (image/png, image/jpeg, image/gif or image/webp)")
	case b.MediaType != "" && !agent.IsImageMediaType(b.MediaType):
		return fmt.Errorf("image block media type %q is not supported (want image/png, image/jpeg, image/gif or image/webp)", b.MediaType)
	}
	return nil
}

// ExtractText concatenates the text blocks of a turn.
func ExtractText(t agent.Turn) string {
	var out strings.Builder
	for _, b := range t.Blocks {
		if b.Type == agent.BlockText {
			out.WriteString(b.Text)
		}
	}
	return out.String()
}

// BackoffBase is the unit WaitBackoff scales: attempt n waits
// BackoffBase << n. Tests shrink it so a retry scenario does not sleep.
var BackoffBase = 200 * time.Millisecond

// WaitBackoff sleeps for an exponentially increasing duration based on the
// attempt number, or returns early if ctx is cancelled. attempt=0 is a
// no-op — callers use this *between* retries.
func WaitBackoff(ctx context.Context, attempt int) error {
	if attempt <= 0 {
		return nil
	}
	d := BackoffBase << attempt
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// NewHTTPClient builds the client provider implementations share.
// Streaming calls must NOT have an absolute body timeout — the round-trip
// can sit idle while the model deliberates or tools run. Cancellation comes
// from the request context; the pre-stream phases (dial, TLS, headers) are
// bounded so an upstream stall does not wedge the producer until its ctx
// ceiling.
func NewHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
	return &http.Client{Transport: tr, Timeout: 0}
}

// IsTransientStreamErr reports whether a streaming read/connection error is
// worth retrying. Context cancellation is never retryable — the caller went
// away. Everything network-shaped (reset, unexpected EOF, idle timeout) is.
func IsTransientStreamErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "connection reset") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "unexpected EOF") ||
		strings.Contains(s, "stream error") ||
		strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "http2: ")
}

// Retryable reports whether an upstream HTTP status is worth a retry: 429
// and 5xx are; other 4xx are caller errors and must not be retried.
func Retryable(status int) bool { return status == 429 || status >= 500 }

// EstimateTokens is a cheap approximation (~4 chars per token). Accurate
// enough to compute ratios; never use it for billing.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// LogBreakdown writes the segment ratios alongside raw counts so they are
// easy to eyeball in logs.
func LogBreakdown(provider, phase string, step int, b agent.TokenBreakdown) {
	total := b.Total
	if total == 0 {
		total = 1
	}
	pct := func(n int) string { return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(total)) }
	slog.Debug("token breakdown",
		"provider", provider, "phase", phase, "step", step, "total", b.Total,
		"system", b.System, "system_pct", pct(b.System),
		"tools", b.Tools, "tools_pct", pct(b.Tools),
		"history", b.History, "history_pct", pct(b.History),
		"user_prompt", b.UserPrompt, "user_prompt_pct", pct(b.UserPrompt),
		"tool_calling", b.ToolCalling, "tool_calling_pct", pct(b.ToolCalling),
	)
}

// IntFromAny coerces a decoded JSON number to int.
func IntFromAny(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}

// AsStr coerces a decoded JSON value to string ("" when not a string).
func AsStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
