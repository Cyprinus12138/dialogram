# How dialogram compares

dialogram is a runtime for one tool-using agent in Go. It covers the tool
loop, the state that loop persists, the transport that streams it to a
client, and the checks around tool calls. It does not orchestrate graphs or
teams of agents, and it is not a catalogue of integrations.

This page sets it beside nine agent frameworks and four protocols. Every
claim about another project comes from that project's own docs or repository,
read on 2026-10-23 or on the later dates noted below, and listed under
[Sources](#sources). Where I could not confirm something, the text says "not verified". "dialogram adds" means the
feature is not on the pages I read for that project. That is not proof that
the project lacks it, and these projects change quickly.

## What dialogram has

- A provider-neutral block IR (text, tool_use, tool_result, image). The same
  stored conversation runs on the Anthropic Messages API and on any
  OpenAI-compatible Chat Completions endpoint, and hand-written-fixture replay
  tests exercise the resume from one to the other.
- One shared loop with parallel tool dispatch through a `tool.Executor`. The
  default executor applies a per-tool timeout, recovers panics, caps the
  bytes the model reads back and bounds concurrency per step.
- A three-channel tool result (`model_observation`, `ui_payload`,
  `runtime_state`) with artifact handles, and a provenance flag that fences
  untrusted observations as data.
- Tool annotations (MCP-style hints), `PolicyInterceptor` and
  `ApprovalListInterceptor`.
- Pause tools for input, choice and forms, plus custom pause tools. A pause
  ends the turn; the application resumes it with `agent.AnswerPause`, which
  answers every call of the paused turn.
- Step checkpoints (`Checkpointer`) and `CloseDangling` for crash recovery.
- A resumable SSE transport over a memory or Redis buffer, with cursor
  resume, cancel and an idle watchdog. An AG-UI transport next to it.
- Prompt-cache breakpoints with a stable/volatile prompt split.
- Agent Skills (`SKILL.md`) in the specification's three tiers: a catalog in
  the stable prompt, then instructions and bundled files through
  `load_skill`, with a strict validator for CI.
- An honesty guard and the paired A/B harness that measures it.
- An `eval` package: deterministic loop evals, a grounding checker,
  LLM-as-judge scenarios, manifest pins and budgets.
- MCP servers as tool sources over stdio and streamable HTTP.
- OpenTelemetry spans for the run, each model call and each tool call, using
  GenAI semantic-convention attribute names; opt-in Prometheus metrics.

## What dialogram does not have

- No graph or workflow orchestration. One agent runs one loop. There are
  no nodes, edges, sequential or parallel workflow agents.
- No multi-agent support. No handoffs, subagents or agents-as-tools.
- No memory store. The application renders per-turn context into
  `ChatInput.MemoryContext` itself.
- No retrieval. No retrievers, embeddings, vector stores or document
  loaders.
- No structured-output helper. Replies are text; typed data travels
  through tool calls.
- Two providers. Anthropic Messages and OpenAI-compatible Chat
  Completions. No Gemini, Bedrock or Vertex adapter, and no Responses API.
- Go only.
- No sandbox. Isolating a tool's process, files or network is the tool's
  job (see the trust boundary in [BLOCK_IR.md](BLOCK_IR.md)).
- No hosted platform. No dashboard, deployment tooling or managed runs.
- Pre-1.0. The API may still change.

## Frameworks

### Vercel AI SDK

The AI SDK is a TypeScript toolkit for AI apps and agents (Apache-2.0), with
framework support for React, Next.js, Vue and Svelte. `ToolLoopAgent` runs its own
model-and-tools loop; `HarnessAgent` runs an existing harness such as Claude
Code or Codex. Much of the design matches dialogram. `toModelOutput` turns a
tool result into the content the model receives while the app keeps the raw
output. Tool results render as typed message parts (`tool-<name>`) that a
component switches on. `toolApproval` with `'user-approval'` completes the
call with approval-request parts, and a new call carries the response. A
`useChat` created with `resume: true` reconnects to a stream that keeps
running after the client drops, and stopping needs a separate endpoint. The
AI SDK builds resumption from the `resumable-stream` package, Redis, an
active-stream id the application persists, and two endpoints the application
writes. dialogram ships the handler and the store, and resumes from the
client's last event id. dialogram adds a provider-neutral stored IR, which
matters because the AI SDK recommends persisting its UI message format and
its persistence page does not discuss replaying it on another provider. It also adds per-tool
timeouts and an observation cap (the AI SDK tools page documents an abort
signal and `toolCallConcurrency`, but no timeout), annotation-driven policy
and an eval harness. dialogram lacks the AI SDK's provider list, its frontend
hooks and the JavaScript ecosystem.

### OpenAI Agents SDK

The Agents SDK is a Python library (MIT), with a separate JS/TS SDK. Its
primitives are agents, handoffs and agents-as-tools, and guardrails. Around
them are sessions, tracing, MCP and realtime voice agents. A tool with
`needs_approval` stops the run with `interruptions`. The `RunState` can be
saved with `to_json`, loaded in another process with `from_json`, approved
or rejected, and run again. This is close to dialogram's pause and
resume. Async function tools take a per-call `timeout`. MCP servers
connect over stdio, streamable HTTP or the legacy SSE transport, or run as
hosted tools on OpenAI's side, with tool filters and `require_approval`.
Sessions store items in the SDK's input-item format, in SQLite, Redis,
SQLAlchemy, MongoDB, Dapr, OpenAI Conversations or an encrypted wrapper.
Other providers work through OpenAI-compatible endpoints or the beta LiteLLM
and Any-LLM adapters. The docs warn that Responses and Chat Completions
differ in features and recommend one model shape per workflow. Tracing is on
by default and goes to the OpenAI backend unless you replace the trace
processors; the tracing page does not mention OpenTelemetry. dialogram adds
a resumable stream transport and a UI channel for tool results (neither is
on the pages I read), an IR that both of its adapters translate, an
observation size cap, annotation-driven policy, and an eval harness that runs
without a platform. dialogram lacks handoffs, agents-as-tools, general input
and output guardrails (its `Guard` covers one output check: ungrounded
figures), the session backends, hosted MCP tools and realtime voice.

### Pydantic AI

Pydantic AI is a Python agent framework (MIT). Its docs state that the
message format is independent of the model, and show a history from one
provider reused on another, which is the property dialogram's block IR is
built for. Deferred tools work like dialogram's pauses. A tool marked
`requires_approval`, or one that raises `ApprovalRequired`, ends the run with
`DeferredToolRequests`. A new run resumes from `DeferredToolResults` plus the
stored conversation. Pydantic AI also ships typed structured output,
dependency injection through `RunContext`, OpenTelemetry instrumentation that
works with any OTLP backend, Pydantic Evals, MCP, AG-UI and Vercel AI event
streams, Pydantic Graph, and durable execution on eight engines: Temporal,
DBOS, Prefect, Restate, AWS Lambda, Absurd, pgtask and Airflow. dialogram adds
a resumable stream buffer with cursor resume, the three-channel envelope with
artifact handles, and the honesty guard with its A/B harness. None of these
appear on the pages I read. dialogram lacks structured output, durable-engine
integrations (its `Checkpointer` is the seam an engine would plug into, not
an integration), graph control flow and Pydantic AI's provider list.

### LangGraph and LangChain `create_agent`

LangChain's `create_agent` builds a tool-calling agent whose main extension
point is middleware. It uses LangGraph state underneath: the agents page links
to LangGraph's state docs and imports `InMemorySaver` from
`langgraph.checkpoint`. LangGraph's `interrupt()` suspends a node. It needs a
checkpointer and a thread id, and resumes with `Command(resume=...)`. The node
then runs again from the start, so code before the interrupt must be
idempotent. The built-in middleware covers human approval
(`HumanInTheLoopMiddleware`), model and tool retries, model fallback, model
and tool call limits, PII handling, summarisation, context editing and
subagents. Checkpointers persist graph state per thread, with `PostgresSaver`
recommended for production. Resumable streams come from the hosted side: the
Agent Server, part of LangSmith, resumes thread streams from the
`Last-Event-ID` header, while `join_stream` on a run does not buffer output
produced before joining. LangSmith also provides tracing (with an
OpenTelemetry integration) and offline, online and LLM-as-judge evaluation.
Both projects checkpoint at step level, pause the loop for approval, hook
model and tool calls, and resume streams by event id (LangGraph through
its hosted Agent Server). dialogram adds a
resumable transport you run yourself on Redis, a pause that resumes by
appending a `tool_result` instead of re-running a node, the three-channel
envelope and annotation-driven policy. dialogram lacks graphs, subagents, the
middleware catalogue, structured output (`response_format`), a hosted
deployment, and LangSmith's tracing and evaluation.

### Claude Agent SDK

The Claude Agent SDK embeds Claude Code in Python or TypeScript programs: the
SDK runs the Claude Code binary and gets its built-in tools, hooks, subagents,
MCP, permissions, sessions, skills and plugins. Anthropic's Commercial Terms
of Service govern its use. Its permission system has more layers than
dialogram's. Hooks run first, then deny rules, ask rules, the permission mode,
allow rules and finally a `canUseTool` callback. Approval waits inside the
loop: `canUseTool` holds execution until it returns. A `PreToolUse` hook can
instead return `defer`, so the process exits and resumes later from the
persisted session. Sessions are JSONL transcripts on local disk that can be
resumed or forked; a `SessionStore` adapter mirrors them for other hosts. The
CLI exports traces (beta), metrics and log events over OTLP. Both projects
gate tools with hooks, load `SKILL.md` skills, use MCP and emit OpenTelemetry.
dialogram adds an in-process Go library with no subprocess, a stored IR that
runs on two providers, a resumable SSE transport, a UI channel for tool
results, and pauses that end the turn by default, so no process has to stay
alive while a user decides. dialogram lacks built-in file, shell and web
tools, subagents, file checkpointing, permission modes and rule syntax,
plugins, and Claude Code's context management.

### Google ADK (including ADK Go)

Google's Agent Development Kit has SDKs for Python, TypeScript, Go, Java and
Kotlin (Apache-2.0). Google announced ADK Go 1.0 on 2026-03-31, with
OpenTelemetry tracing, a plugin system, human-in-the-loop confirmation for
tools flagged `RequireConfirmation`, YAML agent definitions and A2A. The
repository's module path is now `google.golang.org/adk/v2`. ADK is optimised
for Gemini. ADK Go's OpenAI package (`openaimodel`) is experimental, needs
ADK Go v2.1.0 or later, uses the Responses API by default and accepts text
only. I did not find a first-party Anthropic model package for ADK Go. The Go
repository has `session`, `memory`, `artifact`, `workflow`, `telemetry` and
`tool/mcptoolset` packages. The ADK docs describe Sequential, Parallel and
Loop workflow agents, graph workflows, evaluation, and deployment to Cloud
Run or GKE. Both are Go-native agent loops with tool confirmation as a
pause, MCP tools and OpenTelemetry. dialogram adds first-party Anthropic and
OpenAI-compatible adapters (both with images), a stored IR that resumes
across them, a resumable SSE buffer, the three-channel envelope, and a Go
eval package (the adk-go top-level listing has no eval directory; whether
ADK's evaluation tooling covers Go is not verified). dialogram lacks
multi-agent workflows, A2A, memory services, YAML agent definitions,
deployment tooling and Gemini support.

### Eino (CloudWeGo)

Eino is a Go framework (Apache-2.0) of components (ChatModel, Tool, Retriever,
ChatTemplate and others) that `compose` connects into graphs and workflows.
On top sits an ADK layer: `ChatModelAgent` runs a single ReAct agent, and
DeepAgent delegates to sub-agents. Any agent or tool can interrupt for human
input. The ADK Runner saves execution state to a `CheckPointStore`, a KV store
of string keys and byte values, and `ResumeWithParams` resumes it. With a
shared store this works across instances. Graph-level interrupts are static
breakpoints set at compile time. Callbacks provide hooks for logging, tracing
and metrics. eino-ext supplies chat models (OpenAI, Claude, Gemini, Ark,
Ollama), retrievers, indexers, embeddings, document loaders, callback handlers
such as Langfuse tracing, evaluators, and an MCP tool wrapper over
`mark3labs/mcp-go`. Both are Go, both interrupt and resume through a
pluggable store, and both have MCP tools and callback hooks. dialogram adds a
resumable stream transport, the three-channel envelope with artifact handles
and a gen_ui channel, annotation-driven policy and provenance fencing,
executor limits, prompt-cache breakpoints and the honesty guard with its A/B
harness. dialogram lacks graphs and workflows, multi-agent, retrieval
components and the breadth of eino-ext. Its checkpoints sit only at step
boundaries, not at arbitrary graph nodes.

### Genkit Go

Genkit (Google, Apache-2.0) has SDKs for TypeScript, Go, Python (beta) and
Dart (preview); its README calls Go production-ready. The Go docs list 16
model provider plugins, among them Anthropic, OpenAI, Gemini, AWS Bedrock,
Ollama and any OpenAI-compatible API. A tool that can pause is defined with
`DefineResumableTool` and returns `tool.Interrupt()`. The caller stores
`resp.History()` and resumes with `ai.WithResume`, the same caller-held
history shape as dialogram's pauses. Durable streaming (preview) persists
flow state in an in-memory or Firestore stream manager. A client that
reconnects with the `X-Genkit-Stream-Id` header gets the buffered stream
replayed; the page does not describe resuming from an offset. Genkit Go
creates spans on the global OpenTelemetry `TracerProvider`. Its evaluation
plugin has three reference-based evaluators (DeepEqual, Regex, JSONata) and a
CLI. The MCP plugin is a client over stdio, streamable HTTP or SSE, and can
also expose Genkit tools as an MCP server. dialogram adds cursor resume from
the client's last event id with a Redis store, the three-channel envelope,
annotation-driven policy, executor limits and the honesty guard. dialogram
lacks Genkit's provider list, flows and the developer UI, an MCP server mode
and SDKs in other languages.

### langchaingo

langchaingo (MIT) is the Go port of LangChain. Its repository has packages
for chains, agents, memory, prompts, output parsers, document loaders, text
splitters, embeddings and callbacks, 14 LLM provider integrations and 15
vector stores. It is pre-1.0 (v0.1.x on pkg.go.dev). Both are Go and share
the Anthropic and OpenAI providers. The two cover different ground.
langchaingo offers integrations; dialogram has no retrieval, vector stores,
document loaders or chains, and two providers. dialogram adds what
langchaingo's README and docs index do not mention: provider-neutral
persisted state with HITL resume, a resumable transport, a UI channel,
policy, checkpoints, MCP and evals. The two can be combined, since a
dialogram tool is a Go function and can call a langchaingo retriever.

## Protocols dialogram speaks or should speak

### AG-UI

dialogram speaks AG-UI through the `agui` package (see [AGUI.md](AGUI.md)).
The encoder maps dialogram's events onto `RUN_*`, `TEXT_MESSAGE_*`,
`TOOL_CALL_*` and `CUSTOM` events, sent as SSE `data:` frames.
`MessagesToTurns` lifts the client-held history into the block IR. AG-UI
defines no event cursor. Its events page points to `MESSAGES_SNAPSHOT` for
resynchronising after an interrupted connection, so an `agui` run cannot
resume and `sse` stays the resumable transport. A pause ends the run the
way AG-UI's interrupts specify (checked against `spec/1.0/schema.json` on
2026-10-24): a `MESSAGES_SNAPSHOT`, then `RUN_FINISHED` with an `outcome` of
type `interrupt`, and the next run's `resume` entries answer it. The
handler signs interrupt ids, so a client-held history cannot be edited into
running a tool the agent never paused on, and it refuses resumes that break
the spec's contract with `RUN_ERROR`. Approving with edited arguments is
not offered. dialogram emits no `STATE_*`, `STEP_*`, `REASONING_*`,
`ACTIVITY_*` or `SUBAGENT_*` events and has no protobuf encoding. AG-UI lists first-party
integrations including Pydantic AI, Google ADK (Python and JavaScript),
Mastra and LlamaIndex, a community integration for the Claude Agent SDK, and
the OpenAI Agent SDK as in progress.

### MCP

dialogram is an MCP client (`tool/mcp`, see [MCP.md](MCP.md)) over stdio and
streamable HTTP, built on the official Go SDK (v1.8.0), which negotiates
protocol version 2026-07-28 with servers that support it. The 2025-06-18
specification
defines four tool annotation hints with defaults `readOnlyHint: false`,
`destructiveHint: true`, `idempotentHint: false` and `openWorldHint: true`. It
says clients MUST treat annotations as untrusted unless they come from
trusted servers. dialogram maps declared hints with the spec's defaults, but
leaves a tool with no annotations block at all-false. `AssumeOpenWorld` is
the opt-in for the cautious reading; MCP.md explains why. Results from
servers registered `Untrusted` are fenced as data before the model reads
them. The spec also tells clients to implement timeouts and confirm sensitive
operations, which the per-call timeout and `PolicyInterceptor` cover. Gaps:
dialogram uses tools only, not prompts, resources, sampling or roots.
Elicitation is answered the 2026-07-28 way: the call's input request
becomes a pause the user answers, through `sse` or as an AG-UI interrupt,
and the call is retried with the answer. One elicitation per result is
answered, and a server on an older protocol that elicits in the middle of
an open call gets an error. It has no MCP server mode, no legacy SSE transport and no built-in OAuth
flow (a remote server gets fixed headers or a caller-supplied
`http.Client`), it does not follow `notifications/tools/list_changed`, and
image and audio parts become placeholders.

### OpenAI Responses API background mode

With `background: true`, a Responses API request runs asynchronously. The
client polls it or cancels it. With `stream: true` as well, a client that
drops can reconnect and resume after the last `sequence_number` it saw by
passing `starting_after`. That is the same cursor resume dialogram's `sse`
does, moved into the provider. Response data is kept for about ten minutes
to allow polling. Zero Data Retention projects run background requests with
`store=false`. dialogram's OpenAI adapter speaks Chat Completions only
(`/chat/completions`), so it cannot use background mode. A Responses adapter
could hand the long model call to OpenAI and resume it by sequence number.
The cost is state held by one provider, which the block IR exists to avoid.
Not implemented.

### Agent Skills (`SKILL.md`)

dialogram reads Agent Skills packages (see [SKILLS.md](SKILLS.md)). The
specification requires `name` (lowercase letters, digits and hyphens,
matching the directory name) and `description`, and defines optional
`license`, `compatibility`, `metadata` and a space-separated, experimental
`allowed-tools`. dialogram reads all six. It follows the three tiers of the
specification's client guide: names and descriptions in the stable prompt
as an `<available_skills>` block, the instructions through `load_skill`, and
bundled files through the same tool with a path, since the model has no file
system of its own. Loading is lenient where the guide recommends it (a name
that breaks the rules, an unknown field, an unquoted colon) and fails on a
skill that cannot load. `agent.ValidateSkills` applies the reference
validator's strict checks. Long instructions and files are paged rather than
cut. All 19 skills in github.com/anthropics/skills loaded in the 2026-10-24 check,
and every bundled text file could be read in full; the strict check flags one
description over the 1024-character limit. dialogram does not run bundled
scripts and does not enforce `allowed-tools`, which the specification leaves
to each client.
The Claude Agent SDK loads skills from
`.claude/`, and LangChain lists skills among its context-management
middleware.

## Capability matrix

"not verified" means I did not find it on the pages listed under Sources. It
does not mean the project lacks it.

| | dialogram | Vercel AI SDK | OpenAI Agents SDK | Pydantic AI | LangGraph | Claude Agent SDK | Google ADK (Go) | Eino | Genkit Go | langchaingo |
|---|---|---|---|---|---|---|---|---|---|---|
| Language | Go | TypeScript | Python; JS/TS SDK | Python | Python; LangGraph.js | Python, TypeScript | Go; also Python, TS, Java, Kotlin | Go | Go; also TS, Python, Dart | Go |
| Provider-neutral persisted conversation state | yes: block IR, 2 providers, replay-tested resume | partial: app persists UI messages; cross-provider not verified | partial: session items; cross-provider not verified | yes: model-independent messages | partial: checkpointed state; cross-provider not verified | not verified: Claude Code JSONL transcripts | not verified | not verified | partial: caller stores JSON history; cross-provider not verified | not verified |
| Resumable streaming transport | yes: SSE + memory/Redis, cursor resume | yes: `resume` + `resumable-stream` + Redis, app-built endpoints | not verified | not verified | via platform: Agent Server, `Last-Event-ID` | not verified | not verified | not verified | partial (preview): replay by stream id | not verified |
| UI channel for tool results | yes: `ui_payload`, `gen_ui` | yes: typed tool parts | not verified | via AG-UI / Vercel AI adapters | not verified | not verified | not verified | not verified | not verified | not verified |
| HITL / pause | yes: pause tools end the turn; AG-UI interrupts | yes: `toolApproval`, new call resumes | yes: interruptions, serialisable `RunState` | yes: deferred tools | yes: `interrupt()`, node re-runs | yes: in-loop callback or `defer` hook | yes: `RequireConfirmation` | yes: interrupt + resume | yes: resumable tools | not verified |
| Policy / interceptors | yes: interceptor chain, annotation policy | partial: approval map or function | yes: guardrails, approval callables | partial: approval per tool | yes: middleware | yes: hooks, rules, modes | yes: callbacks, plugins | partial: approval tool wrapper | not verified | not verified |
| Step checkpoints / durable execution | partial: per-step `Checkpointer`, no engine | not verified | partial: serialisable `RunState` | yes: 8 durable engines | yes: checkpointers | partial: sessions, file checkpoints | not verified | yes: `CheckPointStore` | not verified | not verified |
| Tool execution hardening | yes: timeout, panic recovery, output cap, concurrency bound; no sandbox | partial: abort signal, concurrency limit | partial: per-call timeout (async tools), error function | not verified | partial: retry, error, call-limit middleware | not verified | not verified | not verified | not verified | not verified |
| MCP tool source | yes: stdio, streamable HTTP | yes: HTTP, SSE, stdio | yes: + hosted tools | yes | not verified | yes | yes: `mcptoolset` | yes: eino-ext (`mcp-go`) | yes: client and server | not verified |
| Tracing | OpenTelemetry spans; Prometheus metrics | OpenTelemetry | yes: OpenAI backend by default | OpenTelemetry | via LangSmith (OTel integration) | OpenTelemetry (traces beta) | OpenTelemetry | callbacks (Langfuse handler) | OpenTelemetry | not verified |
| Built-in evals | yes: loop evals, grounding, judge, honesty A/B | not verified | via OpenAI platform | yes: Pydantic Evals | via LangSmith | not verified | listed in ADK docs; Go not verified | evaluators in eino-ext | yes: 3 evaluators + CLI | not verified |
| Multi-agent / orchestration | no | not verified | yes: handoffs, agents-as-tools | yes: Pydantic Graph | yes: graphs, subagents | yes: subagents | yes: workflow agents, A2A | yes: graphs, DeepAgent | not verified | partial: chains, agents |
| Licence | MIT | Apache-2.0 | MIT | MIT | MIT | Anthropic Commercial Terms | Apache-2.0 | Apache-2.0 | Apache-2.0 | MIT |

## Sources

Pages read on 2026-10-23 unless a later date is noted above or below.

dialogram: this repository (README.md, CHANGELOG.md, docs/*.md, `go doc`
for `agent`, `tool`, `tool/mcp`, `sse`, `streambuf`, `eval`, `agui`, and
the source of `agent/skills.go`, `agui/types.go`, `provider/openai/openai.go`,
`provider/anthropic/anthropic.go`).

Vercel AI SDK
- https://ai-sdk.dev/docs/introduction
- https://ai-sdk.dev/docs/agents/overview
- https://ai-sdk.dev/docs/ai-sdk-core/tools-and-tool-calling
- https://ai-sdk.dev/docs/ai-sdk-core/mcp-tools
- https://ai-sdk.dev/docs/ai-sdk-core/telemetry
- https://ai-sdk.dev/docs/ai-sdk-ui/chatbot-resume-streams
- https://ai-sdk.dev/docs/ai-sdk-ui/chatbot-message-persistence
- https://ai-sdk.dev/docs/ai-sdk-ui/generative-user-interfaces
- https://github.com/vercel/ai and https://raw.githubusercontent.com/vercel/ai/main/LICENSE

OpenAI Agents SDK
- https://openai.github.io/openai-agents-python/
- https://openai.github.io/openai-agents-python/human_in_the_loop/
- https://openai.github.io/openai-agents-python/models/
- https://openai.github.io/openai-agents-python/sessions/
- https://openai.github.io/openai-agents-python/tracing/
- https://openai.github.io/openai-agents-python/tools/
- https://openai.github.io/openai-agents-python/mcp/
- https://github.com/openai/openai-agents-python

Pydantic AI
- https://pydantic.dev/docs/ai/overview/
- https://pydantic.dev/docs/ai/core-concepts/message-history/
- https://pydantic.dev/docs/ai/tools-toolsets/deferred-tools/
- https://pydantic.dev/docs/ai/integrations/durable_execution/overview/
- https://pydantic.dev/docs/ai/integrations/ui/ag-ui/
- https://github.com/pydantic/pydantic-ai

LangGraph and LangChain
- https://docs.langchain.com/oss/python/langchain/agents
- https://docs.langchain.com/oss/python/langchain/middleware/built-in
- https://docs.langchain.com/oss/python/langgraph/interrupts
- https://docs.langchain.com/oss/python/langgraph/durable-execution (served the persistence page)
- https://docs.langchain.com/langsmith/streaming
- https://docs.langchain.com/langsmith/trace-with-opentelemetry
- https://docs.langchain.com/langsmith/evaluation
- https://github.com/langchain-ai/langgraph

Claude Agent SDK
- https://code.claude.com/docs/en/agent-sdk/overview
- https://code.claude.com/docs/en/agent-sdk/sessions
- https://code.claude.com/docs/en/agent-sdk/permissions
- https://code.claude.com/docs/en/agent-sdk/user-input
- https://code.claude.com/docs/en/agent-sdk/observability

Google ADK
- https://adk.dev/
- https://adk.dev/agents/models/openai/
- https://adk.dev/sessions/
- https://developers.googleblog.com/adk-go-10-arrives/
- https://github.com/google/adk-go and https://github.com/google/adk-go/tree/main/tool

Eino
- https://github.com/cloudwego/eino
- https://github.com/cloudwego/eino-ext
- https://github.com/cloudwego/eino-ext/tree/main/components/tool/mcp
- https://www.cloudwego.io/docs/eino/core_modules/eino_adk/agent_hitl/
- https://www.cloudwego.io/docs/eino/core_modules/chain_and_graph_orchestration/checkpoint_interrupt/
- https://pkg.go.dev/github.com/cloudwego/eino/schema

Genkit Go
- https://genkit.dev/docs/get-started/
- https://github.com/firebase/genkit
- https://genkit.dev/docs/go/interrupts/
- https://genkit.dev/docs/go/durable-streaming/
- https://genkit.dev/docs/go/model-context-protocol/
- https://genkit.dev/docs/go/local-observability/
- https://genkit.dev/docs/go/evaluation/
- https://genkit.dev/docs/go/integrations/model-providers/

langchaingo
- https://github.com/tmc/langchaingo
- https://github.com/tmc/langchaingo/tree/main/llms
- https://github.com/tmc/langchaingo/tree/main/vectorstores
- https://tmc.github.io/langchaingo/docs/
- https://pkg.go.dev/github.com/tmc/langchaingo

Protocols
- https://docs.ag-ui.com/introduction
- https://docs.ag-ui.com/concepts/events
- https://docs.ag-ui.com/concepts/interrupts
- https://modelcontextprotocol.io/specification/2025-06-18/server/tools
- https://modelcontextprotocol.io/specification/2025-06-18/schema
- https://developers.openai.com/api/docs/guides/background
- https://agentskills.io/specification
- https://agentskills.io/client-implementation/adding-skills-support (read on 2026-10-24)
- https://github.com/agentskills/agentskills/tree/main/skills-ref (validator and parser source, read on 2026-10-24)
