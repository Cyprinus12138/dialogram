# MCP tools

`tool/mcp` connects a `tool.Registry` to [Model Context Protocol](https://modelcontextprotocol.io)
servers. A `Client` dials one server, lists its tools and converts each into
a `tool.Tool` whose `Fn` forwards `tools/call` over the session. `Register`
adds them to a registry under a per-server prefix. From the agent loop's
point of view they are ordinary tools: the same specs, the same dispatch,
the same soft-failure vocabulary.

The package wraps the official Go SDK (`github.com/modelcontextprotocol/go-sdk`);
callers never import it.

## Connecting

```go
client, err := mcp.Connect(ctx, mcp.Stdio{
    Command: "npx",
    Args:    []string{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
}, mcp.Options{Name: "fs", Timeout: 30 * time.Second})
if err != nil { … }
defer client.Close()
```

Two transports are supported:

- `Stdio{Command, Args, Env, Dir}` starts the server as a child process.
  `Env` entries are appended to the parent environment; the child's stderr
  is forwarded so its logs stay visible. `Close` terminates the child.
- `StreamableHTTP{URL, Header, Client}` talks to a remote endpoint. `Header`
  is added to every request (bearer tokens, API keys).

`Options.Name` labels the server in tool names and errors; empty means the
name the server advertises during `initialize`. `Options.Timeout` bounds
each call (default 60s; negative disables it). `ctx` bounds only the
handshake — the session lives until `Close`, which is idempotent.

`client.Instructions()` returns the server's usage instructions, which
servers intend for the system prompt.

## Registering

```go
names, err := mcp.Register(ctx, reg, client, mcp.RegisterOptions{
    Allow: []string{"read_file", "list_directory"},
})
// names == []string{"fs__read_file", "fs__list_directory"}
```

- `Prefix` defaults to `<Name>__`, so two servers that both export
  `read_file` cannot collide; `NoPrefix` registers bare names.
- `Allow` / `Deny` match the server's names (before the prefix). `nil`
  `Allow` means every tool; an empty non-nil list means none — the same
  convention as `tool.Registry.Visible`.
- Registration is all-or-nothing: a name already present in the registry
  returns an error and adds nothing, instead of letting
  `Registry.Register` panic halfway through.

`client.Tools(ctx)` returns the converted tools without registering them,
for callers that want to wrap or inspect them first. Each call re-lists, so
a server that changes its tool set is picked up by listing again.

### Conversion

| MCP field | `tool.Tool` field |
|---|---|
| `name` | `Name` (prefixed by `Register`) |
| `description` | `Description` |
| `title`, else `annotations.title` | `Label` |
| `inputSchema` | `Schema`, passed through as a map; `tool.Obj(nil)` when absent |

The schema is not rewritten. `reg.AnthropicSpec()` and `reg.OpenAISpec()`
emit it as-is, so whatever the server declares is what the model sees.
Provider name limits (length, character set) apply to the prefixed name.

## Results

A `tools/call` result becomes the `Fn` return value in this order:

| Server result | `Fn` returns |
|---|---|
| `isError: true` | `tool.Soft("MCP_TOOL_ERROR", <text parts joined>)` |
| `structuredContent` present | the structured value as-is |
| text parts only | `{"text": "<parts joined by newline>"}` |
| mixed parts | `{"text": "…", "attachments": [ … ]}` |

Attachments are placeholders; bytes never enter the model context:

| Part | Placeholder |
|---|---|
| image | `{"type":"image","media_type":"image/png","omitted":true}` |
| audio | `{"type":"audio","media_type":"…","omitted":true}` |
| embedded text resource | `{"type":"resource","uri":"…","media_type":"…","text":"…"}` |
| embedded blob resource | `{"type":"resource","uri":"…","media_type":"…","omitted":true}` |
| resource link | `{"type":"resource_link","uri":"…","name":"…","media_type":"…"}` |

An embedded text resource keeps its text because that is what the server
meant the model to read; a blob is what the omission rule exists for.

### Errors and timeouts

- A call that outruns `Options.Timeout` returns
  `tool.Soft(tool.ErrorCodeUpstreamTimeout, …)` — retryable, and the shape
  the loop already uses for slow upstreams.
- A call whose parent context is cancelled returns the context error as a
  hard error; the turn is being torn down anyway.
- Transport and protocol errors (unknown tool, closed session) are hard
  errors, which the loop reports to the model as `HARD_FAILURE`.
- After `Close`, every method returns `mcp.ErrClosed`.

## Annotations

MCP servers may annotate tools with behaviour hints (`readOnlyHint`,
`destructiveHint`, `idempotentHint`, `openWorldHint`). `client.Annotations(name)`
returns them as `Hints` with the specification's defaults filled in
(`Destructive` and `OpenWorld` default to true) and `Declared` reporting
whether the server sent any. They are hints, not guarantees — a server can
say anything about itself — so use them to pick an interceptor (approval
list for destructive tools, read-only profiles), not to skip one.

A `tool.Annotations` type is landing on another branch; the TODO on
`Hints` is to map onto it after the merge so interceptors can act on the
hints without importing this package.

## Isolation and trust

An MCP tool runs out of process by definition: the server is a child
process or a remote endpoint, never a Go function in this binary. That
makes the server boundary the natural isolation boundary. Any sandboxing a
risky tool needs — filesystem roots, network egress rules, credential
scoping, seccomp — belongs inside the server, where the capability lives.
This loop does not sandbox the server; it forwards calls and maps results.

A server's tool list, descriptions and results are data supplied by a
peer. Descriptions reach the model as part of the tool spec and results
reach it as `tool_result` content, so a remote server is in a position to
inject instructions. Treat results from servers you do not operate as
untrusted content. A provenance flag on the result envelope
(`Envelope.Provenance`) is landing on another branch; `RegisterOptions`
will grow an `Untrusted` switch that sets it once the two merge.

## Example

```
go run ./examples/mcptools -- npx -y @modelcontextprotocol/server-filesystem /tmp
go run ./examples/mcptools -url http://localhost:3001/mcp -header "Authorization: Bearer …"
go run ./examples/mcptools -call filesystem__list_directory -args '{"path":"/tmp"}' -- npx -y @modelcontextprotocol/server-filesystem /tmp
```

`examples/mcptools` connects, registers the server's tools and prints the
registered names and the Anthropic spec; `-call` invokes one tool and
prints the mapped result.
