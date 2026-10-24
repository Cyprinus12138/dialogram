# AG-UI transport

Package `agui` serves a dialogram run over the [AG-UI protocol](https://docs.ag-ui.com),
the event vocabulary CopilotKit and other AG-UI clients speak. It is a second
transport next to `sse`, over the same `agent.Agent.ChatStream`.

## agui or sse?

| | `sse` | `agui` |
|---|---|---|
| Wire format | dialogram's own events, `event:` + `id:` lines | AG-UI events, `data:` lines only |
| Resumable | yes — producer is buffered server-side, client reconnects with `since` | no — AG-UI defines no cursor; a dropped connection loses the rest of the run |
| Producer lifetime | detached from the HTTP request | the request |
| Conversation state | the application persists turns (`OnFinish`) | the client holds the history and re-sends it every run |
| Human in the loop | `waiting_for_user`, answered through the application's own endpoint | an AG-UI interrupt on `RUN_FINISHED`, answered by `resume` on the next run |
| Frontend | yours | CopilotKit or any AG-UI client |

Use `sse` when you own the frontend and need streams that survive tab
switches and radio sleep. Use `agui` when you need an AG-UI compatible
frontend. Nothing stops you mounting both on one agent.

## Endpoint

```
POST /agui
Content-Type: application/json
Accept: text/event-stream

{"threadId":"t1","runId":"r1","state":{},"messages":[…],"tools":[],"context":[],"forwardedProps":{}}
```

The body is a `RunAgentInput`. `threadId` and `runId` are echoed on
`RUN_STARTED` / `RUN_FINISHED` (minted if the client left them blank).
`messages` is the whole conversation so far, including earlier runs'
assistant and tool messages; `state`, `tools`, `context` and
`forwardedProps` are passed to the `Runner` untouched. `resume` answers
the interrupt the previous run ended on; the handler applies it before the
`Runner` starts (see *Interrupts and resuming*).

```go
run := func(ctx context.Context, in agui.RunAgentInput, emit agent.EmitFn) error {
    turns, err := agui.MessagesToTurns(in.Messages)
    if err != nil {
        return err
    }
    return ag.ChatStream(ctx, agent.ChatInput{Turns: turns, MemoryContext: agui.SystemText(in.Messages)}, emit)
}
mux.Handle("/agui", agui.NewHandler(run, agui.Options{}))
```

The response is `text/event-stream`; every event is one frame of the form
`data: <json>\n\n`, flushed as it is produced. There are no `event:` or
`id:` lines — the event type is the `type` field of the JSON, spelled as
the AG-UI `EventType` enum spells it:

```
data: {"type":"RUN_STARTED","threadId":"t1","runId":"r1"}

data: {"type":"TEXT_MESSAGE_START","messageId":"3f…","role":"assistant"}

data: {"type":"TEXT_MESSAGE_CONTENT","messageId":"3f…","delta":"Here is "}

data: {"type":"TEXT_MESSAGE_END","messageId":"3f…"}

data: {"type":"TOOL_CALL_START","toolCallId":"dg_…","toolCallName":"get_weather","parentMessageId":"3f…"}

data: {"type":"TOOL_CALL_ARGS","toolCallId":"dg_…","delta":"{\"city\":\"Lisbon\"}"}

data: {"type":"TOOL_CALL_END","toolCallId":"dg_…"}

data: {"type":"TOOL_CALL_RESULT","messageId":"9a…","role":"tool","toolCallId":"dg_…","content":"{\"model_observation\":…}"}

data: {"type":"CUSTOM","name":"gen_ui","value":{"type":"bar_chart",…}}

data: {"type":"RUN_FINISHED","threadId":"t1","runId":"r1"}
```

`RUN_STARTED` is written before the `Runner` is called, so the client sees
the run open even when the first model byte is slow. A `Runner` error
after that becomes `RUN_ERROR` with `Options.FriendlyError`'s message (the
raw error is logged, not sent). A client disconnect cancels the `Runner`'s
context. `Options.RunTimeout` (default 5 min) bounds a run.

## Mapping

| dialogram event | AG-UI events | Notes |
|---|---|---|
| `meta` (first) | `RUN_STARTED` | `runId` = the supplied run id, else the meta's `message_id`, else minted. A meta that also carries `message` / `provider` / `model` adds a `CUSTOM "meta"` |
| `meta` (later) | `CUSTOM "meta"` | value `{message_id, message, provider, model}`, empty fields omitted |
| `text` | `TEXT_MESSAGE_START` on the first delta of a segment, `TEXT_MESSAGE_CONTENT` per delta | one assistant message id per contiguous text segment; empty deltas are dropped (the spec requires a non-empty delta) |
| `tool_call` | `TEXT_MESSAGE_END` (if a segment is open), `TOOL_CALL_START` + `TOOL_CALL_ARGS` + `TOOL_CALL_END` | one `TOOL_CALL_ARGS` carries the complete JSON input — dialogram announces whole inputs, never argument deltas |
| `tool_result` | `TOOL_CALL_RESULT` (+ `CUSTOM "gen_ui"`) | `content` = the tool output verbatim, `role` = `tool`, fresh `messageId`. A result whose envelope carries `ui_payload`, or a `present_data` result, adds a `CUSTOM "gen_ui"` with that payload — the same derivation `sse` does |
| `gen_ui` | `CUSTOM "gen_ui"` | `value` = the payload verbatim; deduplicated per tool-use id against the derived one |
| `usage` | `CUSTOM "usage"` | value `{input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, model?, provider?, breakdown?}` |
| `waiting_for_user` | `TEXT_MESSAGE_END` (if open), the pause tool's `TOOL_CALL_START/ARGS/END` (unless already announced), `CUSTOM "waiting_for_user"`, `MESSAGES_SNAPSHOT`, `RUN_FINISHED` with an interrupt outcome | see *Interrupts and resuming* |
| `done` | `TEXT_MESSAGE_END` (if open), `RUN_FINISHED` | |
| `error` | `TEXT_MESSAGE_END` (if open), `RUN_ERROR` | `message` = the event's message |
| `turn` | — | internal boundary event, never on the wire |

Well-formedness is guaranteed whatever the producer does: `RUN_STARTED`
comes first (any event opens the run if no meta did), every
`TEXT_MESSAGE_START` and `TOOL_CALL_START` is closed, a `TOOL_CALL_RESULT`
always follows a started call (one is synthesised when the producer never
announced it), exactly one terminal event is emitted and everything pushed
after it is dropped. `Encoder.Finish` closes a run whose producer returned
without a `done`.

### Message ids and `parentMessageId`

AG-UI clients build their message list from the stream, so the ids matter
for what the client sends back next run:

- Each contiguous text segment gets a fresh assistant `messageId`.
- Every tool call of an assistant turn carries the same `parentMessageId`:
  the id of the text segment that preceded it, or an id minted for the turn
  when it starts with a tool call. The reference client creates the
  assistant message under that id if it does not exist, so parallel tool
  calls land in **one** assistant message — which is what the OpenAI wire
  format requires when the history comes back.
- A tool result ends the assistant turn; the next text starts a new one.

## Messages ⇄ turns

`MessagesToTurns` lifts the client's history into the block IR;
`TurnsToMessages` renders turns as messages (ids are positional,
`msg_1`, `msg_2`, …, so the same history always renders the same ids).

| AG-UI message | Turn |
|---|---|
| `user` | user turn, one text block. Array content keeps the text parts (joined with newlines); image, audio, video and document parts are dropped |
| `assistant` | assistant turn: a text block when `content` is non-empty, then one `tool_use` per `toolCalls[]` entry (`function.arguments` parsed as JSON, empty = `{}`) |
| `tool` | `tool_result` block with the tool name recovered from the matching `toolCalls[]` entry; consecutive tool messages aggregate into one tool turn. `content` is kept verbatim when it is JSON, otherwise JSON-encoded as a string; an `error` with no content becomes the standard `HARD_FAILURE` object |
| `system`, `developer` | dropped — dialogram's system prompt comes from the Profile. `SystemText` joins them for `ChatInput.MemoryContext` if you want the client's instructions to reach the model |
| `activity`, `reasoning`, unknown | dropped |

Conversion fails (and the run ends with `RUN_ERROR`) on a tool call
without an id or name, on arguments that are not JSON, and on a tool
message whose `toolCallId` no earlier assistant message requested — a
provider would reject that history anyway.

## Interrupts and resuming

A pause ends the run with an AG-UI interrupt. The encoder emits, in order:

1. The pause tool's `TOOL_CALL_START/ARGS/END`, unless it was already
   announced, as an approval's tool is. This puts the call into the
   client's assistant message.
2. `CUSTOM "waiting_for_user"` with `{kind, tool_use_id, name, input,
   message, options, multiple}`, for clients that render dialogram's pause
   kinds themselves.
3. `MESSAGES_SNAPSHOT`: the client's messages exactly as it sent them, then
   the messages this run added. The spec requires a snapshot before a run
   ends on an interrupt; echoing the client's messages byte for byte means
   the snapshot never rewrites content dialogram does not model, such as
   image parts.
4. `RUN_FINISHED` with an interrupt outcome:

```json
{"type":"RUN_FINISHED","threadId":"t1","runId":"r1","outcome":{"type":"interrupt","interrupts":[{
  "id":"dgi1.ZGdfMw.approval.1792918800.p8b…",
  "reason":"tool_call",
  "message":"book_table may make irreversible changes. Review the inputs and approve or deny.",
  "toolCallId":"dg_3",
  "responseSchema":{"type":"object","properties":{"approved":{"type":"boolean"},"reason":{"type":"string"}},"required":["approved"],"additionalProperties":false},
  "expiresAt":"2026-10-25T09:00:00Z",
  "metadata":{"dialogram":{"kind":"approval","tool":"book_table","input":{"restaurant":"Ramiro","time":"20:00"}}}}]}}
```

| Pause | `reason` | `message` | `responseSchema`, and the tool result the answer becomes |
|---|---|---|---|
| `request_user_input` | `input_required` | the prompt | `{"user_response": string}`. The payload is the result. |
| `request_user_choice` | `input_required` | the prompt | `{"selected": [option values], "other_text"?: string}`, the options as titled enum values, one pick unless `multiple`. The payload is the result. |
| `request_user_form` | `input_required` | the title | `{"answers": {question id: answer}}`: a string for `text`, `{"value"}` for `single_choice`, `{"values"}` for `multi_choice`, with `other_text` where allowed. The payload is the result. |
| approval | `tool_call` | the interceptor's reason | `{"approved": boolean, "reason"?: string}`. Approving runs the call and its output is the result; declining gives `agent.DeniedResultJSON`. |
| custom pause tool | `dialogram:<tool>` | | none. The payload, any JSON, is the result. |

These are the result bodies `examples/chatserver` builds in `/chat/respond`,
so the model reads the same thing whichever transport the user is on.

### Resuming

Start a new run on the same thread with the snapshot's messages and a
resume entry for the interrupt:

```json
{"threadId":"t1","runId":"r2","messages":[…],
 "resume":[{"interruptId":"dgi1.…","status":"resolved","payload":{"approved":true}}]}
```

Before the `Runner` is called, the handler:

1. Checks every entry: a known status, an id it signed for this thread and
   for the call as the history shows it, not expired, and a payload that
   fits. Nothing runs until every entry passes.
2. Answers the paused call: with the payload, with the approved call's
   output through `Options.Executor`, or with a denial. Status `cancelled`
   gives a `USER_CANCELLED` soft failure, or a denial for an approval.
3. Answers the assistant message's other unanswered calls with a `NOT_RUN`
   soft failure, the body `agent.AnswerPause` writes on the `sse` path. The
   loop surfaces one pause at a time and runs nothing in a turn it pauses
   for approval, so those calls never ran. The model can make them again,
   and they then pass the interceptors as usual.
4. Emits `TOOL_CALL_RESULT` for each answer on the original `toolCallId`,
   without starting the call again, as the spec requires, and a
   `CUSTOM "gen_ui"` when an approved call's output carries `ui_payload`.
5. Puts the tool messages right after the assistant message and calls the
   `Runner` with that history.

The resumed run then ends like any other: `RUN_FINISHED`, or another
interrupt.

### The contract, and how the handler keeps it

| Spec rule | What the handler does |
|---|---|
| A resume uses the interrupted run's thread. | The id is signed over the thread. Another thread's id fails with `RESUME_INVALID`. |
| `interruptId` names an interrupt of the interrupted run. | The id is signed over the call's id, name and canonical arguments and the pause kind. A forged id, or a history whose call was edited, fails with `RESUME_INVALID`. |
| A resume answers every open interrupt. | A run raises one interrupt. A resume that leaves it open fails with `INTERRUPT_PENDING`. |
| Pending interrupts block new input. | A run without `resume` while the last tool-call message has unanswered calls fails with `INTERRUPT_PENDING`. |
| Replaying a resume is safe. | An entry whose call the history already answers is skipped. An approved call's output is kept for `InterruptTTL`, so a replay from a history without the result reuses it, and identical concurrent resumes share one execution. |
| The agent may validate the payload. | It does, against the shapes above. A mismatch fails with `RESUME_INVALID`, naming the field. |
| A resume after `expiresAt` is refused. | `InterruptTTL`, default 24 hours, sets `expiresAt`. A resolved resume after it fails with `RESUME_EXPIRED`; a cancelled one is accepted, so the thread can move on. |
| `RUN_ERROR` is the only error event. | Every refusal is `RUN_ERROR` with `code` `INTERRUPT_PENDING`, `RESUME_INVALID`, `RESUME_EXPIRED` or `HISTORY_INVALID` and a message saying what to fix. The `Runner` is not called. |

### Why interrupt ids are signed

The handler keeps no conversation state: the client's history is the
record. Approving means running a tool, and running a tool on the strength
of a client-sent history would let any client write itself an assistant
message calling `book_table` with arguments of its choosing and then
approve it. The signature binds the interrupt to a call this endpoint
paused on, so the history can be trusted for that one call. Give every
instance the same `Options.InterruptKey`; without one each `Handler` draws
a random key, and its interrupts end with the process.

An approved call runs on a context that ignores client disconnects,
bounded by `RunTimeout`. The user approved the effect, so it completes,
and its output is kept for a replay. That protection lives in the process:
a replay that reaches another instance, from a client that lost the
result, runs the call again. Make side-effecting tools idempotent where
that matters (`tool.Annotations.Idempotent` records it).

Editing the arguments before approving, `editedArgs` in the spec, is not
offered. The approval schema has no `editedArgs`, and a payload carrying
it is refused.

### The way that predates interrupts

A client that ignores the interrupt outcome can still read
`CUSTOM "waiting_for_user"`, append a tool message answering the call
itself, and start a new run without `resume`. The handler accepts that
history and answers the turn's other calls as not run. For an approval
answered this way the `Runner` has to run the tool itself, as before: the
client only reports the decision.

## Limitations

- **No stream resumption.** One run is one HTTP response, and a dropped
  connection loses the rest of it. Use `sse` if you need it. Interrupts
  are resumed, the AG-UI way.
- **No conversation state.** The handler persists nothing but the outputs
  of approved calls, in memory, for replays. A `Runner` that wants to keep
  turns can wrap `emit` with an `agent.Accumulator`.
- **One interrupt per run.** The loop surfaces one pause at a time, so an
  interrupt outcome always holds exactly one interrupt.
- **Client tools are not dispatched.** `RunAgentInput.tools` reaches the
  `Runner` untouched; dialogram only calls tools in its own Registry. A
  `Runner` can register them as pause tools if the frontend should run them.
- **No `STATE_SNAPSHOT` / `STATE_DELTA` / `STEP_*`.** dialogram has no
  shared-state or step concept. The `Event` type carries their fields so an
  application can write them with `agui.WriteFrame` itself.
- **SSE only.** The protobuf media type (`application/vnd.ag-ui.event+proto`)
  is not offered; the reference client defaults to SSE.
- **No timestamps.** `timestamp` is optional and left unset so the encoder's
  output is deterministic.
- **Multimodal content** is reduced to its text parts.

## Spec pages this was written against

- https://docs.ag-ui.com/concepts/events — lifecycle rules, START/CONTENT/END pattern
- https://docs.ag-ui.com/sdk/js/core/events — `EventType` spelling, every event's fields
- https://docs.ag-ui.com/sdk/js/core/types — `RunAgentInput`, `Message`, `ToolCall`, `Tool`, `Context`
- https://docs.ag-ui.com/sdk/js/client/http-agent — `POST` with `Content-Type: application/json`, `Accept: text/event-stream`
- https://docs.ag-ui.com/sdk/python/encoder/overview plus the encoder sources in
  github.com/ag-ui-protocol/ag-ui (`sdks/typescript/packages/encoder/src/encoder.ts`,
  `sdks/python/ag_ui/encoder/encoder.py`) — `data: <json>\n\n`, `text/event-stream`
- github.com/ag-ui-protocol/ag-ui `sdks/typescript/packages/client/src/apply/default.ts` —
  how the reference client applies `parentMessageId`, `TOOL_CALL_RESULT` and
  `MESSAGES_SNAPSHOT` (merged by message id, which is why the snapshot keeps the ids)
- https://docs.ag-ui.com/concepts/interrupts and github.com/ag-ui-protocol/ag-ui
  `spec/1.0/schema.json` (`RunFinishedOutcome`, `Interrupt`, `ResumeEntry`,
  `RunAgentInput.resume`), read on 2026-10-24
- github.com/ag-ui-protocol/ag-ui `sdks/typescript/packages/core/src/generated/schemas.ts` —
  `TextPart` (`{type:"text", text}`), `UserMessage.content` as string or parts
