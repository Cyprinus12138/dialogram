# dialogram

**Conversations that draw.** A provider-neutral agent runtime for Go with inline generative UI, human-in-the-loop pauses, and streams that survive the connection.

[![ci](https://github.com/Cyprinus12138/dialogram/actions/workflows/ci.yml/badge.svg)](https://github.com/Cyprinus12138/dialogram/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/Cyprinus12138/dialogram)](https://goreportcard.com/report/github.com/Cyprinus12138/dialogram)

Extracted from [ProProp](https://proprop.algonavis.com/), an AI property advisor for Singapore running in production, where this runtime drives 53 tools, inline generative UI and human-in-the-loop across web, iOS and Telegram.

## What it does

- **One conversation shape, two providers.** Conversations are persisted as a provider-neutral block IR (text / tool_use / tool_result / image). A turn started on OpenAI can be resumed on Anthropic, and a paused tool call can be resumed later, because neither wire format is baked into the stored state. `provider/anthropic` and `provider/openai` are thin adapters over one shared loop, and both run end to end in CI against recorded wire-format fixtures, cross-provider resume and image turns included, with no network or API key.
- **Tool results split into three channels.** A tool returns an `Envelope`: *what the model hears* (`model_observation`), *what the user sees* (`ui_payload`, rendered inline as a chart or table), and *what the server keeps* (`runtime_state`, the raw rows, addressable through opaque artifact handles). The model asks for a table to be shown by handle; the rows never enter its context. An envelope can also be marked untrusted: its observation then reaches the model fenced as data, not as instructions.
- **Policy from annotations.** Tools carry MCP-style hints (`ReadOnly`, `Destructive`, `Idempotent`, `OpenWorld`). `agent.PolicyInterceptor` turns them into pause or deny decisions in the same interceptor chain as the approval list. The hints never reach the model.
- **Tool calls with limits, turns that survive a crash.** The default `tool.LocalExecutor` gives each call a timeout, recovers panics, caps the bytes the model reads back and bounds how many calls run at once. Timeouts and panics reach the model as soft failures. A `Checkpointer` saves the turns at every step boundary, and `CloseDangling` pairs unanswered calls after a crash so the conversation continues on either provider. There is no sandbox: isolation belongs to the tool.
- **MCP servers as tool sources.** `tool/mcp` connects to a server over stdio or streamable HTTP and registers its tools under a per-server prefix. Server annotations map onto the policy hints, and results from servers you do not operate can be fenced as untrusted.
- **Human-in-the-loop as tools.** `request_user_input`, `request_user_choice` and `request_user_form` end the turn and wait; an interceptor chain can pause any tool for approval; applications add their own pause tools. Mixed turns (a pause co-requested with real tools) run the real tools and defer the pause deterministically instead of dropping work.
- **Streams that survive the connection.** The model call is a detached server-side producer writing to a `streambuf.Store` (memory or Redis). The HTTP connection is disposable: a client reconnects with the last `id:` it saw and gets exactly the events it missed. Tab switches, radio sleep and load-balancer hops stop losing replies.
- **Prompt-cache aware.** The stable system prompt, the tool list and the growing conversation each carry a cache breakpoint; the per-turn volatile context is sent as a separate un-cached block so state drift does not bust the cache.
- **An honesty guard you can measure.** `agent.Guard` redacts regulated figures stated on a turn with no *successful* grounding tool call. The `eval` package ships the paired before/after harness that measured the first version of this guard in production and found it did nothing — and the two lessons that fixed it are built in.
- **Traces and metrics per run.** Every `Chat` / `ChatStream` is an OpenTelemetry span tree following the GenAI semantic conventions: an `invoke_agent` run span, a `chat {model}` span per upstream request with token and cache usage, and an `execute_tool {name}` span per tool call with its outcome (success, soft failure, timeout, panic, denied). The sse producer joins the trace of the request that launched it. Nothing is recorded until you install an SDK. Opt-in Prometheus metrics track the per-turn budgets the eval harness asserts.
- **Skills on demand.** `SKILL.md` bundles are advertised as one-line summaries and loaded through a `load_skill` tool, so the stable prompt stays small.

```
browser ── POST /chat/stream?message_id=X&since=N ──▶ sse.Handler ──▶ streambuf.Store (mem | redis)
   ▲                                                      │  Start(X) wins? → go produce()      ▲
   └──── id:<seq> event:<type> data:{…} ◀──── Tail(since) ┘                                    │
                                                          ▼                                    │
                                              agent.Agent.ChatStream ── provider loop ── Append(X, event)
                                              (guard · metrics)        (tools · pauses · cache breakpoints)
```

## How it compares

dialogram is a single-agent runtime: one tool loop, its transport and its persisted state. It is not a LangChain-style integration library, and it is not a graph orchestrator.

**Vercel AI SDK** (TypeScript)
- Shared: tool approval that ends the call, typed UI parts per tool result, a separate model-facing view of tool output, resumable streams.
- dialogram adds: a stored IR both adapters resume from, per-tool timeouts and output caps, annotation-driven policy, an eval harness.
- dialogram lacks: the AI SDK's provider list, its React, Vue and Svelte integrations, the JavaScript ecosystem.

**OpenAI Agents SDK** (Python, JS/TS)
- Shared: approval interruptions with serialisable run state, MCP over stdio and streamable HTTP, per-call tool timeouts, tracing.
- dialogram adds: a resumable stream transport, a UI channel for tool results, annotation-driven policy, an eval harness that needs no platform.
- dialogram lacks: handoffs and agents-as-tools, general guardrails, session backends, hosted MCP tools, realtime voice.

**Eino** (Go)
- Shared: Go, interrupt and resume through a pluggable checkpoint store, MCP tools, callback hooks.
- dialogram adds: a resumable stream transport, the three-channel envelope, annotation policy and provenance fencing, the measured honesty guard.
- dialogram lacks: graphs and workflows, multi-agent, retrieval components, the eino-ext integration catalogue.

[docs/COMPARISON.md](docs/COMPARISON.md) covers nine projects and four protocols, with a capability matrix and sources. "Adds" means not found in the other project's own docs as read on 2026-10-23.

## Quickstart

```
go get github.com/Cyprinus12138/dialogram
```

```go
reg := tool.NewRegistry()
tool.RegisterHITL(reg)         // request_user_input / _choice / _form
tool.RegisterPresentData(reg)  // inline metric cards, bar/line charts, tables
tool.RegisterRenderFromRef(reg)
reg.Register(&tool.Tool{
    Name: "get_weather", Description: "Five-day forecast for a city.",
    Schema: tool.Obj(map[string]any{"city": tool.Str("City")}, "city"),
    Fn: func(ctx context.Context, in json.RawMessage) (any, error) {
        rows := lookup(in)
        return &tool.Envelope{
            ModelObservation: summary(rows),                                   // what the model hears
            UIPayload:        &tool.UIPayload{Component: "bar_chart", Data: bars(rows)}, // what the user sees
            RuntimeState:     table(rows),                                     // what the server keeps
            Artifacts:        []tool.ArtifactRef{{ID: tool.ToolUseIDFromContext(ctx), Type: tool.ShapeTableRecord}},
        }, nil
    },
})

cfg := &agent.Config{
    Profiles:       map[string]agent.Profile{"assistant": {SystemPrompt: "You are a concise travel assistant…"}},
    DefaultProfile: "assistant",
}
ag, err := dialogram.New(dialogram.SettingsFromEnv(), dialogram.Deps{
    Registry:     reg,
    Config:       cfg,
    Interceptors: []agent.Interceptor{agent.NewApprovalListInterceptor([]string{"book_flight"})},
    Guard:        agent.NewGuard("get_weather"), // optional
})

out, err := ag.Chat(ctx, agent.ChatInput{Turns: history})
// or stream:
err = ag.ChatStream(ctx, agent.ChatInput{Turns: history}, func(ev agent.StreamEvent) { … })
```

Serve it resumably:

```go
buf := streambuf.NewMemStore(0) // or streambuf.NewRedisStore(ctx, streambuf.RedisOptions{URL: redisURL})
h := sse.NewHandler(buf, func(ctx context.Context, msgID string, body []byte, emit agent.EmitFn) error {
    history := loadConversation(body)
    return ag.ChatStream(ctx, agent.ChatInput{Turns: history}, emit)
}, sse.Options{OnFinish: func(ctx context.Context, r sse.Result) { persist(r.Turns, r.Paused) }})
mux.Handle("/chat/stream", h)
mux.Handle("/chat/cancel", h.CancelHandler())
```

Run the reference server and open http://localhost:8080:

```
ANTHROPIC_API_KEY=… go run ./examples/chatserver
# or any OpenAI-compatible endpoint:
LLM_PROVIDER=openai OPENAI_API_KEY=… OPENAI_BASE_URL=https://api.groq.com/openai/v1 OPENAI_MODEL=llama-3.3-70b-versatile go run ./examples/chatserver
```

The example has a weather tool that renders a bar chart, a flight search whose rows are shown by artifact handle, an approval-gated booking tool, a choice pause, an in-memory conversation store, and a vanilla-JS client that resumes a stream after a reload.

## Packages

| Package | What it is |
|---|---|
| `agent` | Block IR, `Provider` contract, `Agent` wrapper, profiles, interceptors, pause detection, skills, the honesty `Guard`, opt-in Prometheus metrics, OpenTelemetry run spans (`Options.TracerProvider`) |
| `tool` | `Tool`, `Registry`, the three-channel `Envelope`, `ArtifactStore`, built-in tools (HITL ×3, `present_data`, `render_from_ref`, `now`) |
| `tool/mcp` | MCP client as a tool source: `Connect` to a server over stdio or streamable HTTP, `Register` its tools under a per-server prefix, results mapped to the registry's shapes |
| `provider/anthropic`, `provider/openai` | Wire adapters: translators, streaming parsers, cache breakpoints, retries |
| `streambuf` | `Store` contract with `MemStore` and `RedisStore` |
| `sse` | The resumable HTTP handler: detached producer, idle watchdog, cancel, gen_ui conversion, `OnFinish` persistence hook |
| `agui` | [AG-UI protocol](https://docs.ag-ui.com) transport for CopilotKit-compatible frontends: `Encoder` from stream events to AG-UI events, messages ⇄ turns conversion, one-run-per-request handler |
| `eval` | Grounding checker, LLM-as-judge scenarios, manifest pins, budgets, honesty-guard A/B runner |
| `dialogram` (root) | `New` / `NewProvider` / `SettingsFromEnv` |

## Docs

- [docs/STREAMING.md](docs/STREAMING.md) — the resumable stream protocol and the store contract
- [docs/AGUI.md](docs/AGUI.md) — serving a run over the AG-UI protocol, and when to prefer it over `sse`
- [docs/BLOCK_IR.md](docs/BLOCK_IR.md) — the block IR, the three-channel envelope and artifact handles
- [docs/SKILLS.md](docs/SKILLS.md) — SKILL.md bundles and on-demand loading
- [docs/MCP.md](docs/MCP.md) — MCP servers as tool sources: transports, registration, result mapping, the isolation stance
- [docs/EVAL.md](docs/EVAL.md) — the three-layer evaluation harness
- [docs/COMPARISON.md](docs/COMPARISON.md) — how dialogram relates to nine agent frameworks and four protocols, gaps included
- [docs/HONESTY_AB.md](docs/HONESTY_AB.md) — how to measure an honesty guard, and what the first measurement taught
- [docs/OBSERVABILITY.md](docs/OBSERVABILITY.md) — Prometheus metrics and the OpenTelemetry span model

## Status

Pre-1.0. The public surface may still move; see [CHANGELOG.md](CHANGELOG.md).

## License

PolyForm Noncommercial 1.0.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).

Noncommercial use is free: personal projects, research, education, and use by noncommercial organisations. Commercial use requires a separate licence; contact the author via the GitHub profile linked in the notice. Attribution is required: keep the `Required Notice: Copyright 2026 Mr.Cyprinus (https://github.com/Cyprinus12138)` line with any copy, in whole or in part.
