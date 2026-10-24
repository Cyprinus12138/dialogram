# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- `agent`: provider-neutral block IR (`Block`, `Turn`), `Provider` contract,
  `Agent` wrapper, `Profile`/`Config` with per-profile system prompt,
  tool allow-list and step cap, interceptor chain with
  `ApprovalListInterceptor`, built-in and custom pause detection
  (`DetectPause`, `Config.PauseTools`), opt-in Prometheus `Metrics`,
  `PromptFingerprint`.
- `agent` skills in the [Agent Skills](https://agentskills.io/specification)
  format. `LoadSkills` discovers `SKILL.md` packages (nested, symlinked, or
  under a shared root such as `.agents/skills`) and reads every field the
  specification defines. Loading is lenient where the specification's client
  guide recommends it and records `Skill.Diagnostics`; `ValidateSkills`
  applies the reference validator's strict checks. The catalog in the stable
  prompt is an `<available_skills>` block. `load_skill` takes an enum `name`,
  an optional `path` that reads one bundled file as text, and an `offset`
  for text longer than one 32 KiB page. Depends on `go.yaml.in/yaml/v3`.
- `agent.Guard`: configurable honesty guard with `Signature` detectors,
  per-kind grounding sets, restatement exemption, and `WrapEmit` /
  `GuardReply` for the streaming and non-streaming paths.
- `tool`: `Tool`, `Registry`, three-channel `Envelope`, `ArtifactStore`,
  schema helpers, built-in `request_user_input` / `request_user_choice` /
  `request_user_form`, `present_data`, `render_from_ref`, `now`.
- `provider/anthropic`, `provider/openai`: wire adapters over the shared
  loop, with prompt-cache breakpoints (Anthropic), parallel tool dispatch,
  mixed-turn pause deferral, step-cap synthesis and transient-error retries.
- `streambuf`: `Store` contract, `MemStore`, `RedisStore` with configurable
  key prefix and TTL.
- `sse`: resumable Server-Sent Events handler with detached producer, idle
  watchdog, cancel endpoint, programmatic `Launch`, gen_ui conversion and an
  `OnFinish` persistence hook.
- `eval`: grounding checker, LLM-as-judge scenarios with floors and an
  errored-vs-failed report, manifest/prompt pins, budgets, and the
  honesty-guard A/B runner with recorded-corpus replay.
- `examples/chatserver`: runnable reference deployment with a vanilla-JS
  client that resumes streams after a reload.
- `tool.Annotations` (`ReadOnly`, `Destructive`, `Idempotent`, `OpenWorld`,
  mirroring the MCP tool annotation hints) on `Tool.Annotations`. Server-side
  only: the wire tool specs and the eval manifest hash do not change when a
  tool is annotated. Every built-in tool is read-only and idempotent.
- `agent.PolicyInterceptor` / `agent.Policy`: annotation-driven approval and
  denial (`PauseDestructive`, `PauseOpenWorld`, `DenyDestructive`,
  `DenyUnknown`) with stable, human-readable reasons.
- `tool.Envelope.Provenance` with `ProvenanceTrusted` / `ProvenanceUntrusted`,
  `tool.UntrustedEnvelope` and `tool.UntrustedContentNote`;
  `ProjectModelObservation` fences an untrusted observation as
  `{"untrusted_content": …, "note": …}` on the wire without rewriting the
  persisted block.
- `docs/BLOCK_IR.md`: "Annotations, policy and provenance" — the trust
  boundary the loop owns and the sandbox it does not ship.
- `tool/mcp`: Model Context Protocol servers as tool sources. `Connect`
  dials a server over stdio (`Stdio`) or streamable HTTP
  (`StreamableHTTP`), `Tools` converts the server's tools to `tool.Tool`s
  with the input schema passed through, `Register` adds them to a
  `tool.Registry` under a per-server prefix with allow/deny lists,
  `Annotations` exposes the server's behaviour hints. Results map to the
  registry's shapes: structured content as-is, text joined into
  `{"text": …}`, binary parts as omitted placeholders, `isError` as a
  `MCP_TOOL_ERROR` soft failure, per-call timeouts as `UPSTREAM_TIMEOUT`.
  Wraps `github.com/modelcontextprotocol/go-sdk`.
- `tool/mcp`: `Tools` and `Register` map the server's annotation hints
  onto `tool.Annotations` so `agent.PolicyInterceptor` can act on them.
  Undeclared tools stay zero (dialogram's all-false default) rather than
  taking MCP's destructive/open-world defaults;
  `RegisterOptions.AssumeOpenWorld` marks them `OpenWorld` so
  `Policy.PauseOpenWorld` pauses every undeclared remote tool.
- `tool/mcp`: `RegisterOptions.Untrusted` wraps every successful result
  in `tool.UntrustedEnvelope` so `ProjectModelObservation` fences it as
  data; soft failures stay bare.
- `examples/mcptools`: connects to an MCP server, registers its tools and
  prints the Anthropic spec; optionally invokes one tool.
- `tool`: the execution seam — `Call`, `Executor`, `ConcurrencyLimiter` —
  and the default `LocalExecutor` (`NewLocalExecutor`,
  `LocalExecutorOptions`): a per-call timeout (`Tool.Timeout` overrides it
  per tool; `TOOL_TIMEOUT` soft failure), panic recovery (`TOOL_PANIC`
  soft failure, stack logged), a per-step concurrency bound, and a cap on
  the model-visible bytes of a result (`CapObservation`,
  `TruncatedObservation`; `ui_payload` and `runtime_state` are never
  capped). Wired through `Options.Executor` on both providers and
  `Deps.Executor` at the root; `Registry.Call` is unchanged.
- `agent`: step checkpoints — `Checkpoint`, `Checkpointer`,
  `MemCheckpointer`, `ChatInput.MessageID`, `DanglingToolUses`, and the
  recovery helpers `CloseDangling` / `Config.CloseDangling`
  (`RUN_INTERRUPTED` results for unanswered calls; pause tools stay
  pending). The loop saves a checkpoint after every assistant turn and
  every tool turn; wired through `Options.Checkpointer` on both providers
  and `Deps.Checkpointer` at the root. The example server uses
  `MemCheckpointer`; `docs/STREAMING.md` documents the recovery flow.
- Tool dispatch always produces one result per `tool_use`, including a
  call still queued when the context ends (`HARD_FAILURE`), so the tool
  turn stays well-formed on both wires.
- `agent.BlockImage`: image blocks in user turns (`ImageBlock`,
  `ImageURLBlock`, `TextBlock`, `UserTurnBlocks`, `IsImageMediaType`),
  translated to an Anthropic `image` block or an OpenAI `image_url` part
  and rejected with a clear error anywhere outside a user turn.
- Recorded wire-format fixtures under `provider/*/testdata` and the
  `provider/internal/replay` server: both adapters run end to end from
  hand-written upstream responses — text, tool loop, parallel tools,
  pause, soft failure, transient-error retry, cross-provider resume and
  image turns — with no network or API key.
- `agui`: AG-UI protocol transport — `Encoder` maps stream events to AG-UI
  events with well-formedness guarantees (every START closed, one terminal
  event, pauses emit their tool call then `RUN_FINISHED`),
  `MessagesToTurns` / `TurnsToMessages` round-trip the client-held history
  through the block IR, and `NewHandler` streams one run per request as
  `data:` frames. `examples/agui` serves a weather tool over it.
- OpenTelemetry tracing, following the GenAI semantic conventions. The
  `Agent` wrapper opens an `invoke_agent` run span per `Chat` / `ChatStream`
  (`agent.Options.TracerProvider`, `dialogram.Deps.TracerProvider`; nil uses
  the global provider, a no-op until an SDK is installed). Under it, both
  adapters emit a `chat {model}` span per upstream request (usage, cache
  counters, finish reason; retries are `retry` events on one span) and the
  loop emits an `execute_tool {name}` span per tool call with
  `dialogram.tool.outcome` (success, soft_failure, hard_failure, timeout,
  panic, denied). The run span carries `checkpoint`, `waiting_for_user`,
  `guard.redacted` and `done` events. Child spans use the run span's
  TracerProvider from the context. `sse` runs the detached producer under
  the launching request's span context, stamps `dialogram.message_id` on
  request spans and links a tailing request to the run's launch.
  `agent.InstrumentationName` is the scope name. Depends on
  `go.opentelemetry.io/otel` v1.46.0 (API only; the SDK is test-only).
  `docs/OBSERVABILITY.md` describes the metrics and the span model.
- `docs/COMPARISON.md`: dialogram next to the Vercel AI SDK, OpenAI Agents
  SDK, Pydantic AI, LangGraph, Claude Agent SDK, Google ADK, Eino, Genkit Go
  and langchaingo, and against AG-UI, MCP, OpenAI Responses background mode
  and Agent Skills: per-project notes, a capability matrix, the gaps stated
  plainly and sources checked on 2026-10-23. The README gains a short
  "How it compares" section.

- `agui` interrupts: a pause ends the run with `MESSAGES_SNAPSHOT` and a
  `RUN_FINISHED` whose `outcome` is an AG-UI interrupt (a reason per pause
  kind, the paused `toolCallId`, a `responseSchema`, `expiresAt`), and the
  `Handler` resumes from `RunAgentInput.resume`. Every entry is checked
  before anything runs; an approved call runs through `Options.Executor`
  and its result is reused on a replay; the paused call is answered with
  `TOOL_CALL_RESULT` and the turn's other calls with `NOT_RUN`. Interrupt
  ids are HMAC-signed with `Options.InterruptKey` over the thread and the
  call, and default to a 24-hour `expiresAt` (`Options.InterruptTTL`).
  Refusals are `RUN_ERROR` with `INTERRUPT_PENDING`, `RESUME_INVALID`,
  `RESUME_EXPIRED` or `HISTORY_INVALID`. `CUSTOM "waiting_for_user"` is
  still sent, and answering with a tool message still works.

- `agent.AnswerPause` resumes a paused conversation: it answers the paused
  call and every other unanswered call of its turn (`NOT_RUN`, via
  `agent.NotRunResultJSON`), so a turn that held a second pause, or tools
  beside one that needed approval, no longer leaves calls without results
  for the provider to reject. `agent.DismissedResultJSON` closes a pause
  the user left unanswered (a denial for an approval, `USER_CANCELLED`
  otherwise); `agent.ErrNotPending`, `tool.ErrorCodeNotRun` and
  `tool.ErrorCodeCancelled` go with them. Replay tests on both adapters pin
  the wire shape. `examples/chatserver` resumes with `AnswerPause`, closes a
  pause when a new message arrives instead of an answer, claims a pending
  interaction before acting on it so a double click cannot run an approved
  tool twice, runs approved calls through the agent's executor, and has
  end-to-end tests over its routes.

- `examples/mcpagent`: a terminal agent whose tools all come from one MCP
  server, over streamable HTTP or stdio. The server's instructions go into
  the system prompt; destructive, open-world, unannotated and `-approve`d
  calls ask `[y/N]` first, and an approved call runs through the agent's
  executor before the turn resumes with `AnswerPause`. docs/MCP.md walks
  through it against sg-property-engine.

- `tool.ClientView` drops `runtime_state` from a tool result. The `sse`
  handler's `tool_result` frames and the `agui` tool messages
  (`TOOL_CALL_RESULT`, the snapshot, `TurnsToMessages`, an approved call's
  result) now send that view, so the rows the docs call server-only no
  longer reach the client; `examples/chatserver` applies it to
  `GET /conversations/{id}`.
- `agent.Guard` checks the text of a pause the user reads: the input of the
  built-in human-in-the-loop tools and of the tools in the new
  `Guard.PauseTools` is redacted with the reply, in the persisted block and
  in the `waiting_for_user` event, and its numbers no longer count as
  grounded for the restatement exemption.
- `examples/chatserver`: Singapore property tools adapted from ProProp.
  `lookup_hdb_resale_comps` gives the model summary statistics and a
  three-row sample and keeps the full table behind an artifact handle;
  `psf_trend_series` returns the monthly median psf with a linear trend and
  draws it as a line chart; `calc_bsd` and `calc_absd` compute stamp duty
  on the IRAS rate tables; `propose_shortlist` is an application pause tool
  (`Config.PauseTools`) that takes only a project name: the card the user
  sees is built from the project table, and `/chat/respond` stores the
  resolved record when the user accepts. `GUARD=1` lists it in
  `Guard.PauseTools`. The market tools run on a generated demo
  sample and the projects are fictional.

### Changed (relative to the production code this was extracted from)
- Grounding is outcome-aware: a tool call whose result carries
  `error_code` / `soft_failure` no longer vouches for a figure
  (`agent.ResultIsSuccess`). This closes the largest leak class the
  production A/B measured.
- The restatement exemption applies to any number a tool produced with at
  least `Guard.MinRestatementDigits` digits (default 2), not only to
  three-digit amounts — the one observed false positive.
- Metrics are opt-in and registered on a caller-supplied registry under the
  `dialogram_agent_*` names; nothing auto-registers.
- Tool visibility is a per-profile allow-list plus a per-turn override,
  instead of per-tool mode restrictions on the registry.
- Neutral tool-use ids carry the `dg_` prefix.
- `tool.ProjectModelObservation` adds an envelope's `artifacts` manifest to
  the observation the model reads (after the fence for untrusted results),
  so a handle reaches `render_from_ref` without each tool copying it into
  `model_observation` by hand.
- Skills use the Agent Skills format. `name` is the identifier; `id`,
  `version` and the comma-separated `tool_allowlist` are gone (a version
  belongs in `metadata`, tool lists in `allowed-tools`). `load_skill` takes
  `name` instead of `id` and can read a skill's bundled files.
