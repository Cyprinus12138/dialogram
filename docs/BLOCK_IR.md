# The block IR and the three-channel envelope

## Why an IR

Every provider has its own message format and its own tool-call ids. If the
conversation is persisted in a provider's wire shape, two things become
hard:

- **Human-in-the-loop.** Pausing on a tool call and resuming later means
  appending a `tool_result` to a stored `tool_use` — easy if the pairing is
  provider-neutral, brittle if it depends on a `toolu_…` or `call_…` id that
  only means something inside one in-flight request.
- **Cross-provider failover.** A conversation started on one provider should
  be resumable on another after an outage. That is only possible if neither
  wire format is baked into the stored state.

dialogram stores a conversation as a sequence of `Turn`s, each a role-scoped
sequence of `Block`s:

```go
type Block struct {
    Type       BlockType       // "text" | "tool_use" | "tool_result"
    Text       string
    ToolUseID  string          // neutral id minted by the runtime ("dg_…")
    ToolName   string
    ToolInput  json.RawMessage
    ToolResult json.RawMessage // the full serialised tool result
}

type Turn struct {
    Role   string  // "user" | "assistant" | "tool"
    Blocks []Block
}
```

Each provider has a translator in both directions. Anthropic packs a tool
turn into one user message of `tool_result` blocks; OpenAI expands it into
one `role: tool` message per result. The provider's native id is discarded
at response-to-IR translation and never stored.

## The three-channel envelope

A tool that returns `*tool.Envelope` splits its output:

| Channel            | Who sees it     | Holds                                                      |
|--------------------|-----------------|------------------------------------------------------------|
| `model_observation`| the model       | summaries, key facts, counts, warnings, next actions       |
| `ui_payload`       | the user        | a render directive: component type + props                 |
| `runtime_state`    | the server      | raw rows, cursors, debug info                              |
| `artifacts`        | the model       | opaque handles (`id`, `type`, `description`) into `runtime_state` |

The whole envelope is persisted in `Block.ToolResult`, so a resume on any
provider still has the UI replay and the runtime state. Provider translators
call `tool.ProjectModelObservation` when building the wire request, so the
model only ever sees the first channel. The streaming layer calls
`tool.ExtractUIPayload` to emit a `gen_ui` frame the moment the tool returns.

This is what keeps a 200-row result out of the context window: the model
reads a ten-line observation, the user sees a table, and the server holds
the rows under the tool call's own id.

## Artifact handles

`Registry.Call` stashes `runtime_state` in the registry's `ArtifactStore`
under the in-flight `tool_use_id`. A tool advertises the handle in
`artifacts`; the model passes it to `render_from_ref`, which resolves it
server-side and emits a `ui_payload` — a table with any subset of columns,
or a bar chart from two columns — without the rows ever entering the model's
context. The default store is in-process with a TTL; plug in a shared one
(Redis keyed by the same ids) for multi-instance deployments.

## Pauses

Three built-in tools end the assistant turn and wait for the user:
`request_user_input` (free text), `request_user_choice` (options, optional
multi-select and "Other"), `request_user_form` (several questions at once).
Their `Fn` is never executed; the loop detects them before dispatch and
emits a `waiting_for_user` event carrying the kind, the `tool_use_id` and
the prompt or options.

Applications add their own pause tools through `Config.PauseTools`; these
surface as `PauseKindCustom` with the tool's name and input, and the
application decides what accepting them means.

An interceptor can pause any tool for approval (`InterceptPause`): the event
carries the tool's name and input so the application can dispatch it
verbatim on approve, or append a `DENIED` result on deny.

**Mixed turns.** When a model co-requests a pause alongside real tool calls,
the loop runs the real tools, attaches their results, and pairs each pause
block with a deterministic correction (`PAUSE_COREQUESTED_WITH_TOOLS`) that
tells the model to re-issue the pause on its own next turn. Both wire
protocols reject a follow-up request with an unpaired `tool_use`, so the
pairing is mandatory — and the real work is never dropped.

## Resuming

To resume a paused conversation, append a `tool` turn with one
`tool_result` block whose `ToolUseID` matches the pending pause, then run a
new turn with the extended history. The shape of the result body is a
contract between the application and its prompt; the example server uses
`{"user_response": …}`, `{"selected": […], "other_text": …}`,
`{"answers": {…}}`, and the dispatched tool's own output on approval.
