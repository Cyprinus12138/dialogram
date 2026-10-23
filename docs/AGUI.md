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
`forwardedProps` are passed to the `Runner` untouched.

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
| `waiting_for_user` | `TEXT_MESSAGE_END` (if open), the pause tool's `TOOL_CALL_START/ARGS/END` (unless already announced), `CUSTOM "waiting_for_user"`, `RUN_FINISHED` | see *Pauses* |
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

## Pauses and resuming

A human-in-the-loop pause ends the run in AG-UI terms: there is no
"waiting" state in the protocol, and the client owns the history. The
encoder therefore emits, in order:

1. `TOOL_CALL_START/ARGS/END` for the pause tool (`request_user_input`,
   `request_user_choice`, `request_user_form`, the intercepted tool of an
   approval, or a custom pause tool) — unless it was already announced, as
   an approval pause's tool is. This puts the tool call into the client's
   assistant message, where it needs to be for the resume to work.
2. `CUSTOM "waiting_for_user"` with value
   `{kind, tool_use_id, name, input, message, options, multiple}` — what to
   render: `kind` is one of `hitl`, `hitl_choice`, `hitl_form`, `approval`,
   `custom`; `message` is the prompt or reason; `options` / `multiple` are
   set for a choice.
3. `RUN_FINISHED`.

To resume, the client appends a tool message answering the call and starts
a new run with the full history:

```json
{"id":"…","role":"tool","toolCallId":"<tool_use_id>","content":"{\"selected\":[\"lis\"]}"}
```

`MessagesToTurns` turns that into the `tool_result` block the loop expects
after a pause, and the model continues. The result shapes are the ones
`examples/chatserver` builds in `/chat/respond`: `{"user_response": …}` for
`hitl`, `{"selected": […], "other_text": …}` for `hitl_choice`,
`{"answers": {…}}` for `hitl_form`; for an `approval` the application's
`Runner` should run the intercepted tool itself (or append
`agent.DeniedResultJSON`) before handing the history to the model —
the client only reports the decision.

## Limitations

- **No resume.** One run is one HTTP response. Use `sse` if you need it.
- **No server-side state.** The handler persists nothing; a `Runner` that
  wants to keep turns can wrap `emit` with an `agent.Accumulator`.
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
  how the reference client applies `parentMessageId` and `TOOL_CALL_RESULT`
- github.com/ag-ui-protocol/ag-ui `sdks/typescript/packages/core/src/generated/schemas.ts` —
  `TextPart` (`{type:"text", text}`), `UserMessage.content` as string or parts
