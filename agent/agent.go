// Package agent is the provider-neutral core of dialogram: the block IR a
// conversation is persisted in, the Provider contract every model backend
// implements, the Agent wrapper that layers the honesty guard and metrics
// on top of a provider, and the supporting pieces the tool loop relies on
// (profiles, interceptors, skills, pause detection).
//
// Nothing in this package speaks a provider wire format. The Anthropic and
// OpenAI translators live in provider/anthropic and provider/openai; the
// shared loop machinery they both use lives in internal/loop.
package agent

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/Cyprinus12138/dialogram/internal/telemetry"
)

// ChatMessage is the legacy flat history shape: one text message per turn.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatInput is one request to the agent: the conversation so far plus the
// per-turn knobs.
type ChatInput struct {
	// Messages is the flat text history — each entry becomes a single text
	// block in a Turn. Callers that need to round-trip tool_use /
	// tool_result state across turns should populate Turns instead.
	Messages []ChatMessage `json:"messages,omitempty"`
	// Turns is the block-IR form of the conversation history. When set, it
	// takes precedence over Messages. Use this when replaying a conversation
	// whose blocks include tool_use / tool_result pairs (cross-provider
	// resume, human-in-the-loop resume).
	Turns []Turn `json:"turns,omitempty"`
	// Profile names the Profile (system prompt, tool allow-list, step cap)
	// this turn runs under. Empty selects Config.DefaultProfile.
	Profile string `json:"profile,omitempty"`
	// MemoryContext is the VOLATILE system block: a rendered text block the
	// caller assembles per turn (user memory, workspace state, retrieved
	// context). Providers append it after the stable system prompt and keep
	// it OUT of the prompt-cache breakpoint, so the large stable prefix stays
	// cache-eligible while this part drifts. Empty means no volatile block.
	MemoryContext string `json:"memory_context,omitempty"`
	// Tools, when non-nil, narrows the per-turn tool envelope further than
	// the Profile allow-list does — e.g. an intent router that only ships
	// the two tools relevant to the latest user message. Nil means the full
	// profile set.
	Tools []string `json:"tools,omitempty"`
	// MessageID identifies this run — the id the sse handler hands its
	// Producer — so step checkpoints (see Checkpointer) are keyed to it.
	// It may stay empty when no Checkpointer is configured; with one,
	// every run must set it or concurrent runs overwrite each other's
	// checkpoints.
	MessageID string `json:"message_id,omitempty"`
}

// ToolTrace is one executed tool call: its name, raw input and serialised
// output.
type ToolTrace struct {
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input"`
	Output string          `json:"output"`
}

// ChatOutput is the non-streaming result of one chat turn.
type ChatOutput struct {
	Reply     string      `json:"reply"`
	ToolCalls []ToolTrace `json:"tool_calls"`
	// Turns is the assistant + tool Turn sequence produced by this call, in
	// order (excluding the user turn that was input). Persist it as block
	// IR; it carries the tool_use_id pairings a resume needs.
	Turns          []Turn          `json:"turns,omitempty"`
	Provider       string          `json:"provider"`
	Model          string          `json:"model"`
	TokenBreakdown *TokenBreakdown `json:"token_breakdown,omitempty"`
	// CacheReadTokens / CacheCreationTokens are the provider-reported
	// prompt-cache counters summed across this call's tool-loop steps.
	// Unlike TokenBreakdown (a char-count estimate) these are authoritative.
	// OpenAI reports only cache reads, so CacheCreationTokens stays 0 there.
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
}

// TokenBreakdown estimates how the input-token budget of a request is split
// across its semantic segments. Providers only return a single prompt token
// count, so each assembled segment is char-counted and approximated. The
// absolute numbers are rough; the ratios are what is useful for debugging
// "why is this request expensive?".
type TokenBreakdown struct {
	System      int `json:"system"`
	Tools       int `json:"tools"`
	History     int `json:"history"`
	UserPrompt  int `json:"user_prompt"`
	ToolCalling int `json:"tool_calling"`
	Total       int `json:"total"`
}

// Stream event types emitted by providers and the streaming layer.
const (
	EventMeta           = "meta"
	EventText           = "text"
	EventToolCall       = "tool_call"
	EventToolResult     = "tool_result"
	EventGenUI          = "gen_ui"
	EventUsage          = "usage"
	EventTurn           = "turn"
	EventWaitingForUser = "waiting_for_user"
	EventDone           = "done"
	EventError          = "error"
)

// StreamEvent is the unified event type emitted during a streaming chat.
// Providers translate their upstream formats into these events.
//
//	meta             — stream metadata (Provider, Model, MessageID)
//	text             — incremental text delta (Delta)
//	tool_call        — a tool is about to be invoked (Name, Input, ToolUseID)
//	tool_result      — a tool finished (Name, Input, Output, ToolUseID)
//	gen_ui           — an inline UI payload to render (GenUI); emitted by the
//	                   streaming layer, never by providers
//	usage            — running token usage (InputTokens, OutputTokens,
//	                   CacheReadTokens, CacheCreationTokens, Breakdown)
//	turn             — a completed Turn (assistant or tool) ready to persist
//	                   as block IR. Internal boundary event: the streaming
//	                   layer filters these off the wire.
//	waiting_for_user — the loop paused for human input. Terminal for this
//	                   producer; no "done" follows. Kind distinguishes a
//	                   free-text pause, a choice, a form, an interceptor
//	                   approval, or a custom pause tool.
//	done             — the assistant finished its turn
//	error            — a terminal error (Message)
type StreamEvent struct {
	Type         string          `json:"type"`
	Delta        string          `json:"delta,omitempty"`
	Name         string          `json:"name,omitempty"`
	Label        string          `json:"label,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	Output       string          `json:"output,omitempty"`
	Message      string          `json:"message,omitempty"`
	MessageID    string          `json:"message_id,omitempty"`
	InputTokens  int             `json:"input_tokens,omitempty"`
	OutputTokens int             `json:"output_tokens,omitempty"`
	// CacheReadTokens / CacheCreationTokens carry the running prompt-cache
	// counters on usage events, parallel to InputTokens / OutputTokens.
	CacheReadTokens     int             `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int             `json:"cache_creation_tokens,omitempty"`
	Model               string          `json:"model,omitempty"`
	Provider            string          `json:"provider,omitempty"`
	Breakdown           *TokenBreakdown `json:"breakdown,omitempty"`
	Turn                *Turn           `json:"turn,omitempty"`
	// Kind qualifies waiting_for_user: PauseKindHITL, PauseKindHITLChoice,
	// PauseKindHITLForm, PauseKindApproval or PauseKindCustom.
	Kind string `json:"kind,omitempty"`
	// Options and Multiple are populated for PauseKindHITLChoice so a client
	// can render the option buttons without re-parsing Input.
	Options  []ChoiceOption `json:"options,omitempty"`
	Multiple bool           `json:"multiple,omitempty"`
	// GenUI carries the structured UI payload for gen_ui events.
	GenUI json.RawMessage `json:"gen_ui,omitempty"`
	// Request is the tool.InputRequest of a PauseKindToolInput pause.
	Request json.RawMessage `json:"request,omitempty"`
}

// EmitFn receives stream events in order. Providers call it serially.
type EmitFn func(StreamEvent)

// Provider is a model backend capable of a tool-use conversation loop. Every
// provider sees the same tool.Registry and Profile; the only difference is
// the wire protocol framing.
type Provider interface {
	Name() string
	Model() string
	Chat(ctx context.Context, in ChatInput) (*ChatOutput, error)
	ChatStream(ctx context.Context, in ChatInput, emit EmitFn) error
}

// Kinds of waiting_for_user pauses.
const (
	PauseKindHITL       = "hitl"
	PauseKindHITLChoice = "hitl_choice"
	PauseKindHITLForm   = "hitl_form"
	PauseKindApproval   = "approval"
	// PauseKindCustom marks a pause raised by an application-defined pause
	// tool (Config.PauseTools). The event carries the tool's Name and Input
	// so the application can render it and resolve it on resume.
	PauseKindCustom = "custom"
	// PauseKindToolInput marks a pause raised by a tool that needs the
	// user's input to finish a call (tool.NeedInput; an MCP server's
	// elicitation). The event carries the tool's Name and Input, and the
	// tool.InputRequest as Request; resume with tool.ExecuteWithInput and
	// agent.AnswerPause.
	PauseKindToolInput = "tool_input"
)

// Agent wraps a Provider with the cross-cutting layers that belong above the
// wire protocol: the honesty Guard, turn metrics and the run's root trace
// span. HTTP layers should talk to an Agent, not to a Provider directly.
type Agent struct {
	provider Provider
	guard    *Guard
	metrics  *Metrics
	tracer   trace.Tracer
}

// Options are the optional layers New installs around a Provider. A zero
// Options is valid: no guard, no metrics, spans from the global
// TracerProvider (a no-op until an OpenTelemetry SDK is installed).
type Options struct {
	// Guard, when non-nil, redacts un-grounded regulated figures from
	// replies (see Guard). nil disables guarding entirely.
	Guard *Guard
	// Metrics, when non-nil, receives per-turn budget signals.
	Metrics *Metrics
	// TracerProvider is the source of each run's root span; the model-call
	// and tool spans under it come from the same provider. nil uses
	// otel.GetTracerProvider(), which records nothing until an SDK is
	// installed. See docs/OBSERVABILITY.md for the span model.
	TracerProvider trace.TracerProvider
}

// New wraps p with the given options.
func New(p Provider, opts Options) *Agent {
	tp := opts.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	return &Agent{provider: p, guard: opts.Guard, metrics: opts.Metrics, tracer: tp.Tracer(telemetry.Scope)}
}

// Provider returns the underlying provider's name.
func (a *Agent) Provider() string { return a.provider.Name() }

// Model returns the underlying provider's model identifier.
func (a *Agent) Model() string { return a.provider.Model() }

// Underlying returns the wrapped Provider.
func (a *Agent) Underlying() Provider { return a.provider }

// Chat runs one non-streaming turn under the run span, then applies the
// guard (if any) and records metrics.
func (a *Agent) Chat(ctx context.Context, in ChatInput) (*ChatOutput, error) {
	ctx, span := a.startRun(ctx, in)
	out, err := a.provider.Chat(ctx, in)
	if err != nil {
		endRun(span, err)
		return out, err
	}
	if a.guard != nil {
		if reds := a.guard.GuardReply(in.Turns, out); len(reds) > 0 {
			redactedEvent(span, reds[0].Kind, len(reds))
		}
	}
	if a.metrics != nil && out != nil {
		a.metrics.ObserveTurn(in.Profile, a.provider.Name(), out.ToolSteps(),
			EstOutputTokens(out.Reply), out.CacheReadTokens, out.CacheCreationTokens)
	}
	// A non-streaming pause leaves the pausing tool_use unanswered; any
	// other successful return is the end of the assistant's turn.
	if span.IsRecording() && out != nil && len(DanglingToolUses(out.Turns)) == 0 {
		span.AddEvent(telemetry.EventDone)
	}
	endRun(span, nil)
	return out, nil
}

// ChatStream runs one streaming turn under the run span. When a Guard is
// installed, text deltas are held and re-emitted at each assistant turn
// boundary so an un-grounded figure is redacted from both the live stream
// and the persisted turn snapshot before either reaches the client.
func (a *Agent) ChatStream(ctx context.Context, in ChatInput, emit EmitFn) error {
	ctx, span := a.startRun(ctx, in)
	e := emit
	if span.IsRecording() {
		inner := e
		e = func(ev StreamEvent) {
			if ev.Type == EventDone {
				span.AddEvent(telemetry.EventDone)
			}
			inner(ev)
		}
	}
	var acc Accumulator
	if a.metrics != nil {
		inner := e
		e = func(ev StreamEvent) {
			acc.Observe(ev)
			inner(ev)
		}
	}
	if a.guard != nil {
		e = a.guard.wrapEmit(in.Turns, e, func(reds []Redaction) {
			redactedEvent(span, reds[0].Kind, len(reds))
		})
	}
	err := a.provider.ChatStream(ctx, in, e)
	if err == nil && a.metrics != nil && !acc.Paused {
		a.metrics.ObserveTurn(in.Profile, a.provider.Name(), CountAssistantTurns(acc.Turns),
			acc.OutputTokens, acc.CacheReadTokens, acc.CacheCreationTokens)
	}
	endRun(span, err)
	return err
}
