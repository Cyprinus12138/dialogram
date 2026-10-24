// Package agui serves a dialogram run over the AG-UI protocol
// (https://docs.ag-ui.com), the event vocabulary that CopilotKit and the
// other AG-UI clients consume.
//
// Three pieces:
//
//   - Encoder maps dialogram's agent.StreamEvent stream onto AG-UI events
//     and guarantees a well-formed run: RUN_STARTED first, every
//     TEXT_MESSAGE_START and TOOL_CALL_START closed, exactly one terminal
//     RUN_FINISHED or RUN_ERROR, nothing after it.
//   - MessagesToTurns / TurnsToMessages convert between AG-UI messages
//     (the history an AG-UI client holds and re-sends on every run) and
//     the block IR an agent.Agent consumes.
//   - NewHandler is the HTTP endpoint: POST a RunAgentInput, read back
//     text/event-stream frames of the form "data: <json>\n\n".
//
// Mapping (see docs/AGUI.md for the full table):
//
//	meta             → RUN_STARTED (first) / CUSTOM "meta" (later ones)
//	text             → TEXT_MESSAGE_START + TEXT_MESSAGE_CONTENT …
//	tool_call        → TOOL_CALL_START + TOOL_CALL_ARGS + TOOL_CALL_END
//	tool_result      → TOOL_CALL_RESULT (+ CUSTOM "gen_ui" for ui_payload)
//	gen_ui           → CUSTOM "gen_ui"
//	usage            → CUSTOM "usage"
//	waiting_for_user → CUSTOM "waiting_for_user" + MESSAGES_SNAPSHOT +
//	                   RUN_FINISHED{outcome: interrupt}
//	done             → RUN_FINISHED
//	error            → RUN_ERROR
//	turn             → dropped (internal boundary event)
//
// # When to use agui, when to use sse
//
// AG-UI has no resume cursor: a run is one HTTP response, and a client that
// loses the connection loses the rest of the run. Package sse buffers the
// producer server-side and lets a client reconnect with the last id it saw.
// Use sse when you own the frontend and need resumability; use agui when
// you need an AG-UI compatible frontend such as CopilotKit. Both sit on
// the same agent.Agent.ChatStream.
//
// # Interrupts
//
// A human-in-the-loop pause (waiting_for_user) ends the run with an AG-UI
// interrupt. The Encoder emits the pause's tool call so the client's
// history carries it, then CUSTOM "waiting_for_user" for clients that
// render dialogram's pauses themselves, then the MESSAGES_SNAPSHOT the spec
// requires, then RUN_FINISHED whose outcome holds one Interrupt: a reason
// per pause kind, the paused toolCallId, and a responseSchema for the
// answer.
//
// The client resumes with a new run whose RunAgentInput.resume answers the
// interrupt. The Handler checks every entry before acting, runs an approved
// call through Options.Executor, answers the paused call with
// TOOL_CALL_RESULT (without starting it again), answers the turn's other
// calls as not run, and only then calls the Runner. Interrupt ids are
// signed with Options.InterruptKey, so the client's history, which the
// handler otherwise trusts, cannot be edited into running a call the agent
// never paused on. A client that answers with its own tool message, the
// way that predates interrupts, still works.
//
// # Spec pages this package was written against
//
//   - https://docs.ag-ui.com/concepts/events — lifecycle rules and the
//     START/CONTENT/END pattern.
//   - https://docs.ag-ui.com/sdk/js/core/events — EventType enum spelling
//     and every event's field names and requiredness.
//   - https://docs.ag-ui.com/sdk/js/core/types — RunAgentInput, Message,
//     ToolCall, FunctionCall, Tool, Context.
//   - https://docs.ag-ui.com/sdk/js/client/http-agent — the client POSTs
//     RunAgentInput as JSON with Accept: text/event-stream.
//   - https://docs.ag-ui.com/sdk/python/encoder/overview and the SDK
//     encoder sources (sdks/typescript/packages/encoder/src/encoder.ts,
//     sdks/python/ag_ui/encoder/encoder.py in github.com/ag-ui-protocol/ag-ui)
//     — the SSE framing is "data: <json>\n\n" with content type
//     text/event-stream; no event: or id: lines.
//   - sdks/typescript/packages/client/src/apply/default.ts (same repo) —
//     how the reference client folds TOOL_CALL_START.parentMessageId,
//     TOOL_CALL_RESULT and MESSAGES_SNAPSHOT into its message list.
//   - https://docs.ag-ui.com/concepts/interrupts and spec/1.0/schema.json
//     (same repo) — the interrupt outcome, Interrupt, ResumeEntry and the
//     resume contract rules.
package agui
