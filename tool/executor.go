package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"time"
	"unicode/utf8"
)

// Call is one tool invocation as the agent loop hands it to an Executor:
// the neutral tool_use id, the tool name and the raw JSON input.
type Call struct {
	ToolUseID string
	Name      string
	Input     json.RawMessage
}

// Executor runs one tool call and returns the serialised result exactly as
// Registry.Call does: a JSON string on success (soft failures included), or
// an error for a Go-level failure the loop turns into a HARD_FAILURE
// result. The loop pairs every tool_use with a tool_result, so an Executor
// must always return — never hang on a misbehaving tool.
//
// LocalExecutor is the default: it runs tools in-process through a Registry
// with a per-call timeout, panic recovery and a cap on what reaches the
// model. A different Executor can route calls to a sandbox, a remote worker
// or a durable-execution engine; the loop does not care where a tool ran.
//
// An Executor may also implement ConcurrencyLimiter to bound how many calls
// the loop runs in parallel within one step.
type Executor interface {
	Execute(ctx context.Context, call Call) (string, error)
}

// ConcurrencyLimiter is optionally implemented by an Executor. The loop
// sizes the semaphore guarding one step's parallel dispatch from
// MaxConcurrency; a value <= 0 leaves the step unbounded.
type ConcurrencyLimiter interface {
	MaxConcurrency() int
}

// Defaults for LocalExecutorOptions.
const (
	// DefaultToolTimeout bounds one tool call unless the tool or the options
	// say otherwise.
	DefaultToolTimeout = 60 * time.Second
	// DefaultMaxObservationBytes caps the model-visible bytes of one result.
	DefaultMaxObservationBytes = 64 << 10
	// DefaultMaxConcurrency bounds the parallel tool calls of one step.
	DefaultMaxConcurrency = 8
)

// LocalExecutorOptions tune a LocalExecutor. Every field is optional: zero
// selects the default, a negative value disables the limit.
type LocalExecutorOptions struct {
	// Timeout is the per-call deadline; Tool.Timeout overrides it per tool.
	// 0 → DefaultToolTimeout, < 0 → no deadline.
	Timeout time.Duration
	// MaxObservationBytes caps what the model sees of one result (see
	// CapObservation). 0 → DefaultMaxObservationBytes, < 0 → no cap.
	MaxObservationBytes int
	// MaxConcurrency bounds how many calls the loop runs in parallel within
	// one step. 0 → DefaultMaxConcurrency, < 0 → unbounded.
	MaxConcurrency int
}

// LocalExecutor runs tools in-process through a Registry. Per call it:
//
//   - stamps the call context (tool_use id + artifact store) so
//     envelope-returning tools and render_from_ref keep working;
//   - enforces a deadline — the tool's own Timeout, else the option — and
//     returns a TOOL_TIMEOUT soft failure when it passes;
//   - recovers a panic into a TOOL_PANIC soft failure and logs the stack,
//     so one bad tool never takes the producer down;
//   - caps the model-visible bytes of the result (CapObservation).
//
// Registry.Call itself is unchanged; LocalExecutor is the hardened path
// the loop dispatches through.
type LocalExecutor struct {
	reg     *Registry
	timeout time.Duration
	maxObs  int
	maxPar  int
}

// NewLocalExecutor builds the default Executor over reg. A nil reg is
// replaced by an empty registry (every call fails as an unknown tool).
func NewLocalExecutor(reg *Registry, opts LocalExecutorOptions) *LocalExecutor {
	if reg == nil {
		reg = NewRegistry()
	}
	e := &LocalExecutor{reg: reg, timeout: opts.Timeout, maxObs: opts.MaxObservationBytes, maxPar: opts.MaxConcurrency}
	if e.timeout == 0 {
		e.timeout = DefaultToolTimeout
	}
	if e.maxObs == 0 {
		e.maxObs = DefaultMaxObservationBytes
	}
	if e.maxPar == 0 {
		e.maxPar = DefaultMaxConcurrency
	}
	return e
}

// MaxConcurrency implements ConcurrencyLimiter.
func (e *LocalExecutor) MaxConcurrency() int { return e.maxPar }

// Execute runs one call. The tool runs in its own goroutine so a tool that
// ignores its context cannot wedge the loop: on deadline the call is
// abandoned (the goroutine finishes on its own) and a soft failure is
// returned; on parent-context cancellation ctx.Err() is returned so the
// loop records a hard failure and unwinds.
func (e *LocalExecutor) Execute(ctx context.Context, call Call) (string, error) {
	ctx = WithCallContext(ctx, call.ToolUseID, e.reg.Artifacts())
	timeout := e.timeout
	if t, ok := e.reg.Lookup(call.Name); ok && t.Timeout > 0 {
		timeout = t.Timeout
	}
	callCtx, cancel := ctx, context.CancelFunc(func() {})
	if timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	type outcome struct {
		out      string
		err      error
		panicked any
		stack    []byte
	}
	done := make(chan outcome, 1)
	input := string(call.Input)
	go func() {
		var o outcome
		defer func() {
			if r := recover(); r != nil {
				o.panicked, o.stack = r, debug.Stack()
			}
			done <- o
		}()
		o.out, o.err = e.reg.Call(callCtx, call.Name, input)
	}()

	var o outcome
	select {
	case o = <-done:
	case <-callCtx.Done():
		// Prefer a result that landed in the same instant.
		select {
		case o = <-done:
		default:
			if err := ctx.Err(); err != nil {
				return "", err
			}
			slog.Warn("tool call timed out", "name", call.Name, "tool_use_id", call.ToolUseID, "timeout", timeout)
			return softJSON(ErrorCodeToolTimeout, fmt.Sprintf("%s did not finish within %s and was abandoned.", call.Name, timeout)), nil
		}
	}
	if o.panicked != nil {
		slog.Error("tool call panicked", "name", call.Name, "tool_use_id", call.ToolUseID,
			"panic", o.panicked, "stack", string(o.stack))
		return softJSON(ErrorCodeToolPanic, fmt.Sprintf("%s failed with an internal error: %v", call.Name, o.panicked)), nil
	}
	if o.err != nil {
		return "", o.err
	}
	return CapObservation(o.out, e.maxObs), nil
}

func softJSON(code, msg string) string {
	b, _ := json.Marshal(Soft(code, msg))
	return string(b)
}

// TruncatedObservation is the marker CapObservation substitutes for an
// observation that exceeded the cap.
type TruncatedObservation struct {
	Truncated     bool   `json:"truncated"`
	OriginalBytes int    `json:"original_bytes"`
	Preview       string `json:"preview"`
}

// envelopeOut re-serialises an envelope after its observation was capped.
// omitempty keeps absent channels absent — a literal null ui_payload would
// otherwise read as a render directive downstream.
type envelopeOut struct {
	ModelObservation json.RawMessage `json:"model_observation,omitempty"`
	UIPayload        json.RawMessage `json:"ui_payload,omitempty"`
	RuntimeState     json.RawMessage `json:"runtime_state,omitempty"`
	Artifacts        json.RawMessage `json:"artifacts,omitempty"`
}

// CapObservation bounds the model-visible bytes of a serialised tool
// result. max <= 0 disables the cap; a result within the cap is returned
// byte for byte.
//
// For an Envelope the cap applies to model_observation only — the slice
// ProjectModelObservation puts on the wire — while ui_payload,
// runtime_state and artifacts pass through untouched: the UI still renders
// and the server still has the rows. For a bare result the whole JSON is
// the observation.
//
// An observation over the cap is replaced by a marker that is itself valid
// JSON and fits within max:
//
//	{"truncated":true,"original_bytes":<n>,"preview":"<leading JSON text>"}
//
// original_bytes is the observation's size before capping; preview is a
// prefix of its JSON text cut on a UTF-8 boundary, so the model can still
// tell what shape the result had and say it was too large, instead of
// reasoning over a silently clipped document. Tools that can return large
// results should return an Envelope with an artifact handle (see
// ArtifactStore) so the rows never reach the model at all.
func CapObservation(raw string, max int) string {
	if max <= 0 || len(raw) <= max {
		return raw
	}
	var w envelopeWire
	if err := json.Unmarshal([]byte(raw), &w); err == nil && w.isEnvelope() {
		if len(w.ModelObservation) <= max {
			return raw
		}
		out := envelopeOut{
			ModelObservation: truncationMarker(w.ModelObservation, max),
			UIPayload:        w.UIPayload,
			RuntimeState:     w.RuntimeState,
			Artifacts:        w.Artifacts,
		}
		b, err := json.Marshal(out)
		if err != nil {
			return raw
		}
		return string(b)
	}
	return string(truncationMarker([]byte(raw), max))
}

// truncationMarker builds the TruncatedObservation for obs so that the
// marshalled marker fits in max. JSON escaping can inflate the preview, so
// the budget is retried downward until the marker fits (or the preview is
// empty — a max smaller than the frame itself cannot be honoured).
func truncationMarker(obs []byte, max int) json.RawMessage {
	n := len(obs)
	frame := len(`{"truncated":true,"original_bytes":,"preview":""}`) + len(strconv.Itoa(n))
	budget := max - frame
	if budget < 0 {
		budget = 0
	}
	if budget > n {
		budget = n
	}
	preview := obs[:budget]
	for {
		preview = trimToRune(preview)
		b, _ := json.Marshal(TruncatedObservation{Truncated: true, OriginalBytes: n, Preview: string(preview)})
		if len(b) <= max || len(preview) == 0 {
			return b
		}
		preview = preview[:len(preview)*3/4]
	}
}

// trimToRune drops a trailing partial UTF-8 sequence.
func trimToRune(p []byte) []byte {
	for i := 0; i < utf8.UTFMax && len(p) > 0; i++ {
		if r, size := utf8.DecodeLastRune(p); r != utf8.RuneError || size > 1 {
			return p
		}
		p = p[:len(p)-1]
	}
	return p
}
