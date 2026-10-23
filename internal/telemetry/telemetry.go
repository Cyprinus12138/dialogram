// Package telemetry is the OpenTelemetry vocabulary dialogram's spans
// share: the instrumentation scope, operation and event names, attribute
// keys, and the helpers the loop and the provider adapters use to start
// child spans.
//
// Child spans never take a tracer as an argument. They are started from
// the TracerProvider of the span already in the context — the run span the
// agent.Agent wrapper opened — so they inherit whatever provider the root
// used, and are no-ops when nothing is installed:
//
//	trace.SpanFromContext(ctx).TracerProvider().Tracer(Scope)
//
// Names follow the OpenTelemetry GenAI semantic conventions (status:
// Development), which moved out of the core semantic-conventions repository
// into https://github.com/open-telemetry/semantic-conventions-genai:
//
//   - inference span: docs/gen-ai/client-inference.md
//   - execute_tool span: docs/gen-ai/gen-ai-spans.md#execute-tool-span
//   - invoke_agent span: docs/gen-ai/gen-ai-agent-spans.md
//   - attribute registry: docs/registry/attributes/gen-ai.md
//   - Anthropic token accounting: docs/gen-ai/anthropic.md
//
// gen_ai.system is deprecated there in favour of gen_ai.provider.name, and
// the cache counters are gen_ai.usage.cache_read.input_tokens and
// gen_ai.usage.cache_write.input_tokens. Everything the conventions do not
// name lives under dialogram.*.
package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/Cyprinus12138/dialogram/tool"
)

// Scope is the instrumentation scope name of every dialogram tracer: the
// module path.
const Scope = "github.com/Cyprinus12138/dialogram"

// gen_ai.operation.name values.
const (
	OpInvokeAgent = "invoke_agent"
	OpChat        = "chat"
	OpExecuteTool = "execute_tool"
)

// Span event names.
const (
	EventRetry          = "retry"
	EventCheckpoint     = "checkpoint"
	EventWaitingForUser = "waiting_for_user"
	EventGuardRedacted  = "guard.redacted"
	EventDone           = "done"
)

// Semantic-convention attribute keys.
const (
	OperationName    = attribute.Key("gen_ai.operation.name")
	ProviderName     = attribute.Key("gen_ai.provider.name")
	RequestModel     = attribute.Key("gen_ai.request.model")
	RequestStream    = attribute.Key("gen_ai.request.stream")
	RequestMaxTokens = attribute.Key("gen_ai.request.max_tokens")
	AgentName        = attribute.Key("gen_ai.agent.name")
	FinishReasons    = attribute.Key("gen_ai.response.finish_reasons")
	InputTokens      = attribute.Key("gen_ai.usage.input_tokens")
	OutputTokens     = attribute.Key("gen_ai.usage.output_tokens")
	CacheReadTokens  = attribute.Key("gen_ai.usage.cache_read.input_tokens")
	CacheWriteTokens = attribute.Key("gen_ai.usage.cache_write.input_tokens")
	ToolName         = attribute.Key("gen_ai.tool.name")
	ToolCallID       = attribute.Key("gen_ai.tool.call.id")
	ToolType         = attribute.Key("gen_ai.tool.type")
	ErrorType        = attribute.Key("error.type")
	HTTPStatusCode   = attribute.Key("http.response.status_code")
	ServerAddress    = attribute.Key("server.address")
	ServerPort       = attribute.Key("server.port")
)

// dialogram attribute keys.
const (
	MessageID         = attribute.Key("dialogram.message_id")
	Profile           = attribute.Key("dialogram.profile")
	Step              = attribute.Key("dialogram.step")
	Attempt           = attribute.Key("dialogram.attempt")
	Attempts          = attribute.Key("dialogram.attempts")
	ToolOutcome       = attribute.Key("dialogram.tool.outcome")
	ToolErrorCode     = attribute.Key("dialogram.tool.error_code")
	ToolResultBytes   = attribute.Key("dialogram.tool.result_bytes")
	ToolTruncated     = attribute.Key("dialogram.tool.truncated")
	ToolQueueMS       = attribute.Key("dialogram.tool.queue_ms")
	PauseKind         = attribute.Key("dialogram.pause.kind")
	GuardKind         = attribute.Key("dialogram.guard.kind")
	GuardRedactions   = attribute.Key("dialogram.guard.redactions")
	CheckpointPending = attribute.Key("dialogram.checkpoint.pending")
	CheckpointError   = attribute.Key("dialogram.checkpoint.error")
)

// dialogram.tool.outcome values.
const (
	OutcomeSuccess     = "success"
	OutcomeSoftFailure = "soft_failure"
	OutcomeHardFailure = "hard_failure"
	OutcomeTimeout     = "timeout"
	OutcomePanic       = "panic"
	OutcomeDenied      = "denied"
)

// error.type values beyond HTTP status codes and tool error codes.
const (
	ErrTimeout  = "timeout"
	ErrCanceled = "canceled"
	ErrOther    = "_OTHER"
)

// Tracer returns the tracer of the span in ctx, so a child span inherits
// the root's TracerProvider. With no span in ctx it is the API's no-op
// tracer.
func Tracer(ctx context.Context) trace.Tracer {
	return trace.SpanFromContext(ctx).TracerProvider().Tracer(Scope)
}

// Event adds a named event to the span in ctx. A non-recording span (no
// SDK, or not sampled) is left alone.
func Event(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	if s := trace.SpanFromContext(ctx); s.IsRecording() {
		s.AddEvent(name, trace.WithAttributes(attrs...))
	}
}

// ClassifyError maps a Go error to a low-cardinality error.type value:
// timeout, canceled, or _OTHER.
func ClassifyError(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ErrTimeout
	case errors.Is(err, context.Canceled):
		return ErrCanceled
	case errors.As(err, &ne) && ne.Timeout():
		return ErrTimeout
	}
	return ErrOther
}

// Fail marks span as failed: Error status with err's message and error.type
// (the HTTP status code when status is a non-200 upstream status,
// ClassifyError otherwise). It does not end the span.
func Fail(span trace.Span, status int, err error) {
	if err == nil || !span.IsRecording() {
		return
	}
	et := ClassifyError(err)
	if status != 0 && status != 200 {
		et = strconv.Itoa(status)
		span.SetAttributes(HTTPStatusCode.Int(status))
	}
	span.SetAttributes(ErrorType.String(et))
	span.SetStatus(codes.Error, err.Error())
}

// ---------- model calls ----------

// ModelRequest describes one logical upstream request: the first attempt
// and every retry of it.
type ModelRequest struct {
	Provider string
	Model    string
	// Step is 1-based, matching agent.Checkpoint.Step; the step-cap
	// synthesis request is MaxSteps+1.
	Step      int
	Stream    bool
	MaxTokens int
	// BaseURL is the endpoint the request goes to; it fills server.address
	// and server.port, which tell OpenAI-compatible endpoints apart.
	BaseURL string
}

// Usage is one request's token usage in semantic-convention terms: Input
// includes cached tokens (the adapter adds Anthropic's separately reported
// cache counters before handing it over).
type Usage struct {
	Input, Output, CacheRead, CacheWrite int
}

// ModelCall is the span around one ModelRequest. Per the conventions the
// span covers the whole logical operation, retries included; each failed
// attempt that is retried is a "retry" event on it.
type ModelCall struct {
	span     trace.Span
	attempts int
}

// StartModelCall starts the "chat {model}" CLIENT span for r under ctx.
// The returned context carries it, so an instrumented HTTP transport nests
// the request under it.
func StartModelCall(ctx context.Context, r ModelRequest) (context.Context, *ModelCall) {
	attrs := []attribute.KeyValue{
		OperationName.String(OpChat),
		ProviderName.String(r.Provider),
		RequestModel.String(r.Model),
		Step.Int(r.Step),
	}
	if r.Stream {
		attrs = append(attrs, RequestStream.Bool(true))
	}
	if r.MaxTokens > 0 {
		attrs = append(attrs, RequestMaxTokens.Int(r.MaxTokens))
	}
	ctx, span := Tracer(ctx).Start(ctx, OpChat+" "+r.Model,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	if span.IsRecording() {
		if u, err := url.Parse(r.BaseURL); err == nil && u.Hostname() != "" {
			span.SetAttributes(ServerAddress.String(u.Hostname()), ServerPort.Int(port(u)))
		}
	}
	return ctx, &ModelCall{span: span}
}

func port(u *url.URL) int {
	if p, err := strconv.Atoi(u.Port()); err == nil {
		return p
	}
	if u.Scheme == "http" {
		return 80
	}
	return 443
}

// Attempt marks the start of one attempt.
func (m *ModelCall) Attempt() { m.attempts++ }

// Retry records the current attempt as failed and about to be retried.
// status is the upstream HTTP status, 0 for a transport or read error.
func (m *ModelCall) Retry(status int, err error) {
	if !m.span.IsRecording() {
		return
	}
	attrs := []attribute.KeyValue{Attempt.Int(m.attempts)}
	if status != 0 {
		attrs = append(attrs, ErrorType.String(strconv.Itoa(status)), HTTPStatusCode.Int(status))
	} else if err != nil {
		attrs = append(attrs, ErrorType.String(ClassifyError(err)))
	}
	m.span.AddEvent(EventRetry, trace.WithAttributes(attrs...))
}

// Fail ends the span as failed (see Fail) and returns err, so a call site
// can `return mc.Fail(status, err)`.
func (m *ModelCall) Fail(status int, err error) error {
	Fail(m.span, status, err)
	m.end()
	return err
}

// Done ends the span with the stop reason and usage of a successful
// response.
func (m *ModelCall) Done(finishReason string, u Usage) {
	if m.span.IsRecording() {
		attrs := []attribute.KeyValue{
			InputTokens.Int(u.Input),
			OutputTokens.Int(u.Output),
		}
		if finishReason != "" {
			attrs = append(attrs, FinishReasons.StringSlice([]string{finishReason}))
		}
		if u.CacheRead > 0 {
			attrs = append(attrs, CacheReadTokens.Int(u.CacheRead))
		}
		if u.CacheWrite > 0 {
			attrs = append(attrs, CacheWriteTokens.Int(u.CacheWrite))
		}
		m.span.SetAttributes(attrs...)
	}
	m.end()
}

func (m *ModelCall) end() {
	if m.span.IsRecording() {
		m.span.SetAttributes(Attempts.Int(max(m.attempts, 1)))
	}
	m.span.End()
}

// ---------- tool calls ----------

// StartTool starts the "execute_tool {name}" INTERNAL span for one
// dispatched tool_use. queued is how long the call waited for the step's
// concurrency semaphore; the span itself starts once it has a slot.
func StartTool(ctx context.Context, name, toolUseID string, queued time.Duration) (context.Context, trace.Span) {
	attrs := []attribute.KeyValue{
		OperationName.String(OpExecuteTool),
		ToolName.String(name),
		ToolType.String("function"),
	}
	if toolUseID != "" {
		attrs = append(attrs, ToolCallID.String(toolUseID))
	}
	if queued > 0 {
		attrs = append(attrs, ToolQueueMS.Int64(queued.Milliseconds()))
	}
	return Tracer(ctx).Start(ctx, OpExecuteTool+" "+name,
		trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
}

// EndTool classifies a serialised tool result and ends span. err is the
// executor's Go-level error (the loop turns it into a HARD_FAILURE body).
// A hard failure, timeout or panic sets Error status and error.type to the
// tool error code; a soft failure or a denial is a result the model
// handles, so it carries dialogram.tool.error_code with status left unset.
// Parsing happens only for a recording span.
func EndTool(span trace.Span, out string, err error) {
	defer span.End()
	if !span.IsRecording() {
		return
	}
	code, truncated := inspectResult(out)
	outcome := OutcomeSuccess
	switch {
	case err != nil:
		outcome, code = OutcomeHardFailure, tool.ErrorCodeHardFailure
	case code == "":
	case code == tool.ErrorCodeHardFailure:
		outcome = OutcomeHardFailure
	case code == tool.ErrorCodeToolTimeout:
		outcome = OutcomeTimeout
	case code == tool.ErrorCodeToolPanic:
		outcome = OutcomePanic
	case code == tool.ErrorCodeDenied:
		outcome = OutcomeDenied
	default:
		outcome = OutcomeSoftFailure
	}
	attrs := []attribute.KeyValue{
		ToolOutcome.String(outcome),
		ToolResultBytes.Int(len(out)),
		ToolTruncated.Bool(truncated),
	}
	if code != "" {
		attrs = append(attrs, ToolErrorCode.String(code))
	}
	span.SetAttributes(attrs...)
	switch outcome {
	case OutcomeHardFailure, OutcomeTimeout, OutcomePanic:
		span.SetAttributes(ErrorType.String(code))
		msg := code
		if err != nil {
			msg = err.Error()
		}
		span.SetStatus(codes.Error, msg)
	}
}

// resultShape is the part of a serialised tool result EndTool reads: the
// soft-failure keys and the CapObservation marker, at the top level of a
// bare result or inside an envelope's model_observation.
type resultShape struct {
	ErrorCode        string          `json:"error_code"`
	SoftFailure      json.RawMessage `json:"soft_failure"`
	Truncated        bool            `json:"truncated"`
	ModelObservation json.RawMessage `json:"model_observation"`
}

func inspectResult(out string) (code string, truncated bool) {
	var top resultShape
	if json.Unmarshal([]byte(out), &top) != nil {
		return "", false
	}
	code, truncated = top.code(), top.Truncated
	if len(top.ModelObservation) > 0 && top.ModelObservation[0] == '{' {
		var obs resultShape
		if json.Unmarshal(top.ModelObservation, &obs) == nil {
			if code == "" {
				code = obs.code()
			}
			truncated = truncated || obs.Truncated
		}
	}
	return code, truncated
}

func (r resultShape) code() string {
	if r.ErrorCode != "" {
		return r.ErrorCode
	}
	if len(r.SoftFailure) > 0 && string(r.SoftFailure) != "null" && string(r.SoftFailure) != "false" {
		return "SOFT_FAILURE"
	}
	return ""
}
