# Resumable chat streaming

Server-side buffering for `/chat/stream` so replies survive transient client
disconnects (tab switches, Wi-Fi blips, mobile radio sleep). Each turn has a
stable `message_id`; the model call is a first-class server-side resource,
and the HTTP connection is disposable.

## Problem

SSE streams are bound to one TCP connection. If the browser drops it
mid-reply, two things are lost at once:

1. Tokens already emitted that the client had not rendered yet.
2. Tokens still to be generated — when the producer goroutine is tied to the
   request context, cancelling the client cancels the model call too.

## Goals

1. Tab switches do not kill replies. Backgrounding a tab must not waste the
   in-flight model call.
2. Partial replies are recoverable. A client that disconnects after N events
   reconnects and receives events N+1… without duplication.
3. The producer outlives the client. The model call and any tool use run to
   completion even if every client of that `message_id` disappears.
4. Optional Redis. Single-instance development works without external
   infrastructure; Redis unlocks multi-instance deploys and cross-process
   survival.
5. No new failure modes. A store error degrades to an explicit error frame,
   not a hang.

## Non-goals

- Surviving a server restart with the in-memory store (Redis is the answer
  when that matters).
- Ordering guarantees across different `message_id`s.
- Replaying finished streams indefinitely. Buffers expire (default 30 min
  after finishing).

## Architecture

```
┌──────────────┐  POST /chat/stream?message_id=X&since=N
│   browser    │─────────────────────────────────────────────┐
│              │                                             ▼
│  message_id  │                         ┌───────────────────────────────┐
│  since=<seq> │   SSE  id:<seq>         │          sse.Handler          │
│              │◄────── event:<type>─────┤  1. Start(X) — wins? → spawn  │
│              │        data:<json>      │  2. Tail(since) → forward     │
└──────────────┘                         │         │                     │
                                         │  ┌──────▼──────────────┐      │
                                         │  │ run(X): detached ctx│      │
                                         │  │  Producer(ctx, X,   │      │
                                         │  │   body, emit)       │      │
                                         │  │  for each event:    │      │
                                         │  │    Append(X, ev)    │      │
                                         │  │  Finish(X, status)  │      │
                                         │  └──────┬──────────────┘      │
                                         │  ┌──────▼──────────────┐      │
                                         │  │   streambuf.Store   │      │
                                         │  │  MemStore | Redis   │      │
                                         │  └─────────────────────┘      │
                                         └───────────────────────────────┘
```

Three things are decoupled that are usually one:

- **Producer** (the model call) — long-lived, expensive, runs in a detached
  context with its own timeout.
- **Transport** (one HTTP connection) — fragile, short-lived, disposable.
- **Client view** (browser state) — may come and go multiple times per
  producer run.

## Protocol

### Request

```
POST /chat/stream?message_id=<id>&since=<seq>
Content-Type: application/json

<application-defined body>     // required on first connect; empty / {} on resume
GET  /chat/stream?message_id=<id>&since=<seq>   // pure tail, EventSource-friendly
POST /chat/stream/cancel?message_id=<id>
```

- `message_id` — stable id chosen by the client per turn. The server mints
  one if absent (that stream cannot be resumed, since the client does not
  know the id — prefer client-chosen ids).
- `since` — last `id:` the client has seen; `0` on first connect. The
  `Last-Event-ID` header is honoured too.
- The body is whatever the application's `Producer` understands: a
  conversation id and a message, or a full history. An empty body (or `{}`,
  `null`) is a pure resume.

### Response

Standard SSE with the `id:` field acting as the server-side cursor:

```
id: 1
event: meta
data: {"type":"meta","message_id":"…"}

id: 2
event: text
data: {"type":"text","delta":"Here are "}

id: 3
event: tool_call
data: {"type":"tool_call","name":"get_weather","tool_use_id":"dg_…","input":{…}}

id: 4
event: gen_ui
data: {"type":"gen_ui","gen_ui":{"type":"bar_chart",…}}

: ping                           ← keepalive comment, no id

id: N
event: done
data: {"type":"done"}
```

- `event:` mirrors the embedded `"type"` so a client can switch on either.
- `id:` is monotonic per `message_id`.
- `: ping` comments keep connections open through proxies and carry no id.
- Terminal events: `done`, `error`, or `waiting_for_user` (a human-in-the-loop
  pause; the application resumes it later and starts a new producer, see
  `Handler.Launch`). The buffer's terminal status is set at the same time,
  so any tailing request returns after forwarding the terminal event.

## Store contract

```go
type Store interface {
    Start(ctx, msgID) (acquired bool, err error)     // atomic claim
    Append(ctx, msgID, data string) error            // assigns next seq
    Finish(ctx, msgID, status string) error          // terminal marker
    Tail(ctx, msgID, since int, timeout time.Duration) ([]Event, status string, err error)
}
```

**Ownership invariant.** `Start` is atomic: exactly one caller per `msgID`
ever gets `acquired == true`. That caller launches the producer. Every other
caller (including reconnects of the same client) falls through to `Tail`.

**Blocking semantics.** `Tail(since, timeout)` returns immediately if events
past `since` exist. Otherwise it blocks up to `timeout`; if none arrive it
returns an empty slice with empty status so the handler emits a keepalive
and loops.

### MemStore

A map guarded by a mutex; each entry holds the events, the status, waiter
channels and an expiry. `Append` and `Finish` close every waiter. A reaper
evicts finished entries past their TTL.

### RedisStore

| Key                      | Type   | Purpose                                        |
|--------------------------|--------|------------------------------------------------|
| `<prefix><id>:events`    | LIST   | `RPUSH` on append; `LRANGE since -1` for tail. |
| `<prefix><id>:meta`      | HASH   | `status` field — set by `Finish`.              |
| `<prefix><id>:owner`     | STRING | `SETNX` for atomic `Start`.                    |
| `<prefix><id>:notify`    | PUBSUB | One publish per append, wakes all tailers.     |

**Ordering trap avoided.** `Tail` subscribes to the notify channel *before*
calling `LLEN`. Otherwise an append landing between "nothing new" and
"subscribe" would be missed and the client would block the full timeout.

The default prefix is `dialogram:stream:`; set `RedisOptions.KeyPrefix` per
environment when several deployments share one Redis.

## Producer lifecycle

Three triggers bound a producer: an absolute timeout (default 5 min), an
idle watchdog that cancels when no event flows for 90 s (upstream connections
that hang without returning bytes), and the cancel endpoint, which marks the
stop as intentional so it lands as a clean `done` rather than an error.
Finalisation (the terminal event, `Finish`, the `OnFinish` hook) runs on a
fresh short-lived context so it lands even when the primary context has
already expired. A panicking producer still leaves a terminal error frame
behind.

`turn` events are filtered off the wire; they exist to let the server
accumulate the block IR to persist. `present_data` is converted into a
`gen_ui` frame, and any tool whose envelope carries `ui_payload` emits one
too.

## Client flow

```js
let since = 0; const messageId = crypto.randomUUID(); let body = {conversation_id, message};
for (let attempt = 0; attempt < 8; attempt++) {
  try {
    const res = await fetch(`/chat/stream?message_id=${messageId}&since=${since}`,
      { method: body ? "POST" : "GET", body: body && JSON.stringify(body) });
    // parse SSE frames: on `id:` update `since`; on done/error/waiting_for_user stop
    break;
  } catch { body = null; await sleep(Math.min(300 * 2 ** attempt, 3000)); }
}
```

Persist `{messageId, since}` in `localStorage` while a stream is in flight:
a page reload rejoins the producer instead of leaving the user staring at
an unanswered message. A `visibilitychange` listener that aborts the fetch
when the tab returns after a few seconds hidden forces an immediate
reconnect rather than waiting for the OS to notice a dead socket.

## Crash recovery: step checkpoints

The stream buffer holds the *events* of a turn; it does not hold the
conversation *state*. The block IR of the assistant and tool turns used to
reach the application only through `Options.OnFinish`, so a process that
died mid-turn — a deploy, an OOM kill, a lost node — left the client with
a replayable half-stream and the application with nothing to continue
from.

`agent.Checkpointer` closes that gap. When one is configured
(`dialogram.Deps.Checkpointer`, or `Options.Checkpointer` on either
provider), the loop calls `Save` at every step boundary:

1. after an assistant turn is finalised — `Pending` holds its `tool_use`
   blocks, so a crash during tool execution is recoverable;
2. after the matching tool turn is appended — `Pending` is empty.

Each `agent.Checkpoint` carries the `MessageID` (set `ChatInput.MessageID`
to the id the handler gives the `Producer`), the 1-based `Step`, the
`Turns` this run has produced so far, `Pending` and a timestamp. Only the
latest checkpoint per message id matters: `agent.MemCheckpointer` keeps
exactly that for tests and the example, and a production implementation
is one upsert keyed by message id. A `Save` error is logged and never
fails the run.

Recovery after a crash is the application's job, and it is short:

```go
cp, ok := checkpoints.Load(msgID)          // the last boundary reached
if ok {
    turns := cfg.CloseDangling(cp.Turns)   // pair any unanswered tool_use
    conv.Turns = append(conv.Turns, turns...)
    persist(conv)
    checkpoints.Delete(msgID)
}
```

`CloseDangling` appends a synthetic `tool_result` with
`error_code: "RUN_INTERRUPTED"` for every `tool_use` that never received a
result, so the conversation is well-formed for either provider on the next
request — both wire formats reject an unanswered call. Pause tools
(`request_user_input` and friends, plus `Config.PauseTools`) are left
pending: an unanswered question is state to resume, not a lost result.

The client meanwhile resumes from the buffer as usual. With Redis the
events survive the restart and the owner flag stops a duplicate producer;
the terminal frame never arrived, so the application should finish the
buffer (`Store.Append` an `error` or `done` frame, then `Store.Finish`)
once it has recovered the conversation, or launch a continuation turn from
it with `Handler.Launch` and let the client tail that.

Durable-execution engines (Temporal, DBOS and the like) plug in through
the same interface: a `Checkpointer` that records each boundary as a
workflow step gives the engine the state it replays from, with no change
to the loop.

## Failure matrix

| Failure                         | Recoverable? | Mechanism                                         |
|---------------------------------|--------------|---------------------------------------------------|
| Tab hidden then visible         | Yes          | `visibilitychange` + resume with `since`          |
| Client network drop             | Yes          | fetch throws → retry with same id, updated since  |
| Tab closed entirely             | Producer runs to completion; result held for TTL  |
| Provider error before any text  | Yes          | provider-level retry (429/5xx/network, ×2)        |
| Provider error after text       | No (partial) | error frame; partial text persisted by `OnFinish` |
| Upstream hangs silently         | Yes          | idle watchdog → error frame                       |
| Server restart (MemStore)       | No           | reply lost; user re-sends                         |
| Server restart (Redis)          | Partial      | buffer survives; owner flag blocks a duplicate producer; the checkpoint has the IR up to the last step boundary |
| Process dies mid-turn           | State: yes   | last `Checkpoint` + `CloseDangling`; the client sees no terminal frame until the app finishes the buffer |

## Trade-offs considered

**Client-only retry** can re-issue the prompt but loses tokens emitted
during the dead window and restarts the model call — unusable for tool
calls with side effects.

**`Last-Event-ID` alone** works only when the same server instance holds the
state; behind a load balancer without sticky sessions a reconnect lands
elsewhere. Redis solves this at the cost of a dependency, hence optional.

**WebSockets** trade SSE's simplicity for the same single-connection
fragility plus a heavier client and a harder proxy story.

**Server-chosen message ids** were rejected: the client would have to wait
for the first byte before it could retry, and failures before any response
bytes arrive would leave it without a handle.
