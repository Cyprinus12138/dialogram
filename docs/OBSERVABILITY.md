# Observability

dialogram emits two signals, both opt-in:

- **Prometheus metrics**: per-turn budget signals, recorded when you hand
  `agent.New` a `*agent.Metrics`.
- **OpenTelemetry traces**: one span tree per run, following the GenAI
  semantic conventions. Spans come from the API's no-op tracer until you
  install an SDK.

## Metrics

`agent.NewMetrics(reg)` builds four collectors and registers them on `reg`
(a nil `reg` leaves them unregistered, which is handy in tests). Nothing is
registered anywhere else.

```go
m, err := agent.NewMetrics(prometheus.DefaultRegisterer)
if err != nil { return err } // e.g. a second Metrics on the same registry
ag := agent.New(p, agent.Options{Metrics: m}) // or dialogram.Deps{Metrics: m}
```

| Metric | Type | Meaning |
|---|---|---|
| `dialogram_agent_turns_total` | counter | Completed chat turns. |
| `dialogram_agent_turn_tool_steps` | histogram (1…12) | Upstream completions (assistant turns) per chat turn: the step-cap budget. |
| `dialogram_agent_turn_output_tokens` | histogram (50…1500) | Reply length per turn. `Chat` estimates it from the reply (~4 chars per token); `ChatStream` uses the provider-reported output tokens. |
| `dialogram_agent_turn_cache_read_ratio` | histogram (0…1 by 0.1) | `cache_read / (cache_read + cache_creation)`. Observed only when the turn reported cache activity. |

Every metric is labelled `profile` (`default` when the input names none) and
`provider`. Both are small fixed sets, so the label cardinality is bounded.
A turn that returns an error is not observed, and neither is a streaming
turn that ends in a pause. These are the same budgets the `eval` package
asserts, so an eval threshold breach and an SLO regression show up on the
same numbers.

## Traces

### Turning them on

Install an SDK and point it at your collector. The Agent picks up the
global provider, or takes one through `agent.Options.TracerProvider` /
`dialogram.Deps.TracerProvider`:

```go
exp, err := otlptracehttp.New(ctx) // honours OTEL_EXPORTER_OTLP_ENDPOINT
if err != nil {
	return err
}
tp := sdktrace.NewTracerProvider(
	sdktrace.WithBatcher(exp),
	sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "chatserver"))),
)
defer tp.Shutdown(ctx)
otel.SetTracerProvider(tp) // or agent.Options{TracerProvider: tp}
```

The packages are `go.opentelemetry.io/otel`, `.../otel/sdk/trace`,
`.../otel/sdk/resource` and `.../otel/exporters/otlp/otlptrace/otlptracehttp`.
dialogram's packages import only the API (`go.opentelemetry.io/otel` and
`.../otel/trace`). Only its tests import the SDK, so the SDK is never linked
into your binary unless you import it yourself.

Every tracer uses the instrumentation scope `github.com/Cyprinus12138/dialogram`
(`agent.InstrumentationName`), so you can route or filter dialogram's spans
by scope.

### The span tree

```
invoke_agent assistant              INTERNAL   agent.Agent.Chat / ChatStream
├── chat gpt-4o-mini                CLIENT     step 1: the model asks for two tools
├── execute_tool get_weather        INTERNAL   ┐ run in parallel,
├── execute_tool search_flights     INTERNAL   ┘ both children of the run
└── chat gpt-4o-mini                CLIENT     step 2: the answer
```

The `Agent` wrapper opens the run span. The model-call and tool spans are
started from the TracerProvider of the span already in the context:
`trace.SpanFromContext(ctx).TracerProvider().Tracer(scope)`. So they need
no tracer of their own and always share the root's provider. If you call a
provider directly, without the wrapper, those spans attach to whatever span
your context carries (an HTTP server span, say). With no span in the
context, nothing is recorded.

Tool spans are siblings of the chat spans, not children: the loop runs
tools after one request ends and before the next begins.

### Run span: `invoke_agent {profile}`

Named plain `invoke_agent` when the input names no profile. Kind INTERNAL.

| Attribute | Value |
|---|---|
| `gen_ai.operation.name` | `invoke_agent` |
| `gen_ai.provider.name` | `Provider.Name()`: `openai`, `anthropic`, or yours |
| `gen_ai.request.model` | `Provider.Model()` |
| `gen_ai.agent.name` | `ChatInput.Profile`, when set |
| `dialogram.profile` | `ChatInput.Profile`, `default` when empty (the metrics label) |
| `dialogram.message_id` | `ChatInput.MessageID`, when set |
| `error.type` | on failure: `timeout`, `canceled` or `_OTHER` |

A failed run has status Error with the error's message.

| Event | When | Attributes |
|---|---|---|
| `checkpoint` | after every `Checkpointer.Save` (none without a Checkpointer) | `dialogram.step`, `dialogram.checkpoint.pending`; `dialogram.checkpoint.error` if the save failed |
| `waiting_for_user` | the loop paused (HITL tool, custom pause tool, or an interceptor asking for approval) | `dialogram.pause.kind`, `gen_ai.tool.name`, `gen_ai.tool.call.id` |
| `guard.redacted` | the honesty guard redacted figures from a turn | `dialogram.guard.kind`, `dialogram.guard.redactions` |
| `done` | the assistant finished its turn | none |

A paused run ends with `waiting_for_user` and no `done`.

### Model-call span: `chat {model}`

One span per logical upstream request, kind CLIENT, including the step-cap
synthesis request. Per the conventions the span covers the whole operation,
retries included. Each retried attempt adds a `retry` event to the span,
with `dialogram.attempt` (1-based), `error.type` and, for an HTTP failure,
`http.response.status_code`. The span's context reaches the HTTP request, so
an instrumented transport (otelhttp) nests the request under it.

| Attribute | Value |
|---|---|
| `gen_ai.operation.name` | `chat` |
| `gen_ai.provider.name`, `gen_ai.request.model` | as on the run span |
| `gen_ai.request.max_tokens` | the per-turn output cap |
| `gen_ai.request.stream` | `true` on the streaming path (absent otherwise) |
| `server.address`, `server.port` | the endpoint host and port; they tell OpenAI-compatible endpoints apart |
| `dialogram.step` | 1-based step, matching `Checkpoint.Step`; the synthesis request is `MaxSteps+1` |
| `dialogram.attempts` | attempts made, retries included |
| `gen_ai.response.finish_reasons` | the provider's stop reason, e.g. `["tool_calls"]`, `["end_turn"]` |
| `gen_ai.usage.input_tokens` | input tokens, cached ones included |
| `gen_ai.usage.output_tokens` | output tokens |
| `gen_ai.usage.cache_read.input_tokens` | when non-zero |
| `gen_ai.usage.cache_write.input_tokens` | when non-zero (Anthropic only; OpenAI reports no cache writes) |
| `error.type`, `http.response.status_code` | on failure: the HTTP status (`429`, `500`, …), or `timeout`, `canceled`, `_OTHER` |

Anthropic reports `input_tokens` without its cache counters, so the span
carries `input_tokens + cache_read_input_tokens + cache_creation_input_tokens`,
as the conventions' Anthropic page requires. OpenAI's `prompt_tokens`
already includes `cached_tokens`.

### Tool span: `execute_tool {name}`

One span per tool call the loop dispatches, executed or denied, kind
INTERNAL. An executed call's span starts once it holds a slot in the step's
concurrency limit and wraps the `tool.Executor` call. The span's context reaches the tool, so spans a
tool starts nest under it.

| Attribute | Value |
|---|---|
| `gen_ai.operation.name` | `execute_tool` |
| `gen_ai.tool.name` | the tool |
| `gen_ai.tool.call.id` | the neutral `dg_…` tool_use id: the id persisted in the block IR, carried on stream events and sent back to the model (the native id is discarded at parse time) |
| `gen_ai.tool.type` | `function` |
| `dialogram.tool.queue_ms` | time spent waiting for a concurrency slot (executed calls only) |
| `dialogram.tool.outcome` | `success`, `soft_failure`, `hard_failure`, `timeout`, `panic` or `denied` |
| `dialogram.tool.error_code` | the result's `error_code`, when there is one (`NOT_FOUND`, `TOOL_TIMEOUT`, `DENIED`, …) |
| `dialogram.tool.result_bytes` | bytes of the serialised result the loop recorded |
| `dialogram.tool.truncated` | whether `CapObservation` replaced the observation with its truncation marker |
| `error.type` | `HARD_FAILURE`, `TOOL_TIMEOUT` or `TOOL_PANIC`, with status Error |

A soft failure or a denial is a result the model reads and recovers from,
so it carries `dialogram.tool.error_code` and leaves the span status unset.
Only execution failures (hard failure, timeout, panic) set Error, so tool
error rates count broken tools, not "no data for that city".

Interceptor decisions are recorded where their effect lands. A deny
produces a tool result, so it is a zero-work `execute_tool` span with outcome
`denied`. A pause produces no result and ends the run, so it is the run
span's `waiting_for_user` event, with `dialogram.pause.kind` set to
`approval`.

### The sse handler and detached producers

`sse.Handler` runs the producer on a detached context, so the HTTP request
can go away while the run continues. To keep the run in the request's trace,
the handler adds the request's span context to the producer's base context,
unless your `Options.ProducerContext` already put one there. It is added as
a **parent**, with `trace.ContextWithSpanContext`, not as a link:

- One turn is then one trace: `POST /chat/stream` → `invoke_agent` → chat
  and tool spans. Most backends render a parent inline. A link would split
  every turn into two traces joined by a reference many UIs show only on
  request.
- A ParentBased sampler follows the request's decision, so a sampled request
  never loses its run.
- A child may outlive its parent in OpenTelemetry, which is exactly what a
  detached run does when the client disconnects.
- Only the span *context* is attached, as a local parent (the request span
  lives in this process). The live request span is not, because it ends when
  the connection does. This is why the Agent's run span, not the request
  span, is what the loop's children inherit their provider from.

A request that resumes the stream (a GET tail, a POST with an empty body,
or a retried connect for an id that already has a producer) is a different
request with its own trace. It is correlated two ways:

- Every request span the handler sees gets `dialogram.message_id`, the same
  attribute the run span carries. This works across processes.
- When the producer for that id is running in this process, the tailing
  request's span also gets a link to the span that launched it.

Nothing about this changes the wire protocol. `Handler.Launch` uses the
span context of the base context you pass. For a continuation turn launched
from a request handler, pass
`trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(r.Context()))`.

### What is not recorded

- Prompt and tool content (`gen_ai.input.messages`, `gen_ai.output.messages`,
  `gen_ai.tool.call.arguments` / `.result`). The conventions make these
  opt-in because they can hold user data; dialogram does not emit them.
- `gen_ai.conversation.id`. dialogram knows the run (`message_id`), not the
  conversation, and the conventions say not to substitute a generated id.
  An application that has one can copy it onto every span from an SDK
  `SpanProcessor`: `OnStart` sees the context the span was started with.

### Cost when off

With no SDK installed, the run span comes from the API's no-op tracer, and
so does every child, because a no-op span's TracerProvider is the no-op
provider. Nothing is recorded or exported. Each model call and tool call
pays a few small allocations for span options and the context value.
Result classification, which parses the tool result's JSON, runs only for a
recording span. An SDK that does not sample a run costs the same.

### Conventions followed

The GenAI semantic conventions are at status *Development* and have moved
out of the core repository into
[open-telemetry/semantic-conventions-genai](https://github.com/open-telemetry/semantic-conventions-genai).
The pages these spans follow:

- [client-inference.md](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/client-inference.md):
  the `chat {model}` span, usage and cache attributes, `error.type`
- [gen-ai-spans.md](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-spans.md):
  `execute_tool`, and the rule that one span covers every retry of an operation
- [gen-ai-agent-spans.md](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-agent-spans.md):
  the in-process `invoke_agent` span
- [anthropic.md](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/anthropic.md),
  [openai.md](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/openai.md):
  provider names and token accounting
- [registry/attributes/gen-ai.md](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/registry/attributes/gen-ai.md):
  the attribute registry

`gen_ai.system` is deprecated there in favour of `gen_ai.provider.name`,
which is what dialogram emits. Attributes the conventions do not define live
under `dialogram.*`. Because the conventions are still in development, names
may move; this page and the `internal/telemetry` package are the single
place they are defined.
