// Package mcp connects a dialogram tool.Registry to Model Context Protocol
// servers. A Client dials one server over stdio or streamable HTTP, lists
// the server's tools and converts each into a tool.Tool whose Fn forwards
// tools/call over the session; Register adds them to a registry under a
// per-server prefix so two servers exposing the same tool name cannot
// collide.
//
// # Isolation
//
// An MCP tool runs out of process by definition: the server is a child
// process or a remote endpoint, never a Go function in this binary. That
// makes the server boundary the natural isolation boundary. Any sandboxing
// a risky tool needs — filesystem roots, network egress rules, credential
// scoping, seccomp — belongs inside the server, where the capability lives.
// This package does not attempt to sandbox the server; it forwards calls
// and maps results.
//
// # Trust
//
// A server's tool list, descriptions and results are data supplied by a
// peer. Descriptions reach the model as part of the tool spec, and results
// reach it as tool_result content, so a remote server is in a position to
// inject instructions. Treat results from servers you do not operate as
// untrusted content: register them with RegisterOptions.Untrusted, which
// marks every successful result ProvenanceUntrusted so the loop fences it
// as data rather than instructions. Soft failures (MCP_TOOL_ERROR,
// UPSTREAM_TIMEOUT) stay bare so the loop's error_code handling is
// unchanged.
//
// Servers also describe their tools with behaviour hints. Register maps
// them onto tool.Annotations so agent.PolicyInterceptor can pause or deny
// on them; see RegisterOptions for the mapping rules, in particular what
// an unannotated tool gets.
//
// # Result mapping
//
// A tools/call result becomes the Fn return value as follows, in order:
//
//   - IsError set: tool.Soft(ErrorCodeToolError, <joined text content>),
//     the registry's soft-failure shape, so the model stays on its
//     degraded-path instructions instead of seeing a hard failure.
//   - StructuredContent present: returned as-is (object, array or
//     primitive, per the server's output schema).
//   - Otherwise: {"text": <text parts joined by newlines>} plus, when any
//     non-text part is present, "attachments": a list of placeholders.
//     Image and audio parts become {"type","media_type","omitted":true};
//     the bytes never enter the model context. Embedded resources keep
//     their text when they carry text and are otherwise marked omitted;
//     resource links are forwarded as metadata (uri, name, media_type).
//
// With RegisterOptions.Untrusted every value above except a soft failure
// is wrapped in tool.UntrustedEnvelope, so tool.ProjectModelObservation
// fences it before the model reads it.
//
// A call that outruns Options.Timeout returns
// tool.Soft(tool.ErrorCodeUpstreamTimeout, …), which is retryable. A call
// whose parent context is cancelled returns the context error as a hard
// error. Transport and protocol errors (unknown tool, closed session) are
// hard errors.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Cyprinus12138/dialogram/tool"
)

const (
	// DefaultTimeout bounds one tools/call when Options.Timeout is zero.
	DefaultTimeout = 60 * time.Second

	// ErrorCodeToolError is the soft-failure error_code emitted when the
	// server answers a call with IsError set.
	ErrorCodeToolError = "MCP_TOOL_ERROR"

	// DefaultName labels a server whose Options.Name is empty and whose
	// initialize result carries no server name.
	DefaultName = "mcp"

	clientName    = "dialogram"
	clientVersion = "0.1.0"
)

// ErrClosed is returned by every method of a Client after Close.
var ErrClosed = errors.New("mcp: client is closed")

// Options configures a Client.
type Options struct {
	// Name labels the server in registered tool names (the default
	// RegisterOptions.Prefix is Name + "__") and in errors. Empty means the
	// name the server advertised during initialize, falling back to
	// DefaultName.
	Name string
	// Timeout bounds each tools/call. Zero means DefaultTimeout; a negative
	// value disables the per-call bound (the caller's context still applies).
	Timeout time.Duration
}

// Client is one initialised MCP session. Create it with Connect; it is safe
// for concurrent use.
type Client struct {
	name         string
	timeout      time.Duration
	instructions string
	session      *sdk.ClientSession

	mu     sync.Mutex
	tools  map[string]*sdk.Tool // as of the last Tools call, keyed by MCP name
	closed bool

	closeOnce sync.Once
	closeErr  error
}

// Connect dials the server described by t and completes the MCP initialize
// handshake. ctx bounds the handshake only; the session outlives it until
// Close.
func Connect(ctx context.Context, t Transport, o Options) (*Client, error) {
	if t == nil {
		return nil, errors.New("mcp: Connect called with a nil transport")
	}
	st, err := t.sdkTransport()
	if err != nil {
		return nil, err
	}
	cl := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: clientVersion}, nil)
	session, err := cl.Connect(ctx, st, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect: %w", err)
	}
	c := &Client{
		name:    o.Name,
		timeout: o.Timeout,
		session: session,
		tools:   map[string]*sdk.Tool{},
	}
	if init := session.InitializeResult(); init != nil {
		c.instructions = init.Instructions
		if c.name == "" && init.ServerInfo != nil {
			c.name = init.ServerInfo.Name
		}
	}
	if c.name == "" {
		c.name = DefaultName
	}
	if c.timeout == 0 {
		c.timeout = DefaultTimeout
	}
	return c, nil
}

// Name returns the server label (Options.Name or the advertised name).
func (c *Client) Name() string { return c.name }

// Instructions returns the usage instructions the server sent during
// initialize, or "". Servers intend this for the system prompt.
func (c *Client) Instructions() string { return c.instructions }

// Tools lists the server's tools (following pagination) and converts each
// into a tool.Tool: Name and Description pass through, Label is the
// server's display title when it sent one, Schema is the MCP inputSchema
// as a map (or tool.Obj(nil) when absent), Annotations carry the hints
// the server declared (see Register for the mapping; undeclared stays
// zero), and Fn forwards to tools/call. The returned tools are fresh
// values; renaming them (as Register does) does not affect the name sent
// on the wire.
//
// Each call re-lists, so a server that changes its tool set is picked up
// by calling Tools again. The listing also refreshes what Annotations
// reports.
func (c *Client) Tools(ctx context.Context) ([]*tool.Tool, error) {
	return c.list(ctx, false)
}

// list is Tools with the per-registration untrusted switch threaded into
// each Fn.
func (c *Client) list(ctx context.Context, untrusted bool) ([]*tool.Tool, error) {
	if c.isClosed() {
		return nil, ErrClosed
	}
	var listed []*sdk.Tool
	for t, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp: %s: list tools: %w", c.name, err)
		}
		if t == nil || t.Name == "" {
			continue
		}
		listed = append(listed, t)
	}
	byName := make(map[string]*sdk.Tool, len(listed))
	for _, t := range listed {
		byName[t.Name] = t
	}
	c.mu.Lock()
	c.tools = byName
	c.mu.Unlock()

	out := make([]*tool.Tool, 0, len(listed))
	for _, t := range listed {
		out = append(out, c.convert(t, untrusted))
	}
	return out, nil
}

// Hints are the MCP tool annotations as the server sent them, with the
// specification's defaults filled in for omitted fields (Destructive and
// OpenWorld default to true; ReadOnly and Idempotent default to false).
// Per the specification Destructive and Idempotent are only meaningful
// when ReadOnly is false. They are hints, not guarantees: a server can say
// anything about itself.
//
// Register maps Hints onto tool.Annotations so interceptors can act on
// them without importing this package; see RegisterOptions for the rules.
type Hints struct {
	// Title is the human-readable title from the annotations block.
	Title string
	// ReadOnly reports that the tool does not modify its environment.
	ReadOnly bool
	// Destructive reports that the tool may perform destructive updates.
	Destructive bool
	// Idempotent reports that repeating a call with the same arguments has
	// no additional effect.
	Idempotent bool
	// OpenWorld reports that the tool interacts with external entities
	// (web search) rather than a closed domain (a memory store).
	OpenWorld bool
	// Declared reports whether the server sent an annotations block at
	// all. When false every other field is a default.
	Declared bool
}

// Annotations returns the hints for the tool with the given MCP name (the
// name before any registration prefix), as of the most recent Tools call.
// ok is false when no tool of that name was listed.
func (c *Client) Annotations(name string) (h Hints, ok bool) {
	c.mu.Lock()
	t, ok := c.tools[name]
	c.mu.Unlock()
	if !ok {
		return Hints{}, false
	}
	return hintsOf(t.Annotations), true
}

// Close ends the session (and, for Stdio, the child process). It is
// idempotent and safe to call concurrently; every call returns the result
// of the first.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.closeErr = c.session.Close()
	})
	return c.closeErr
}

func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *Client) convert(t *sdk.Tool, untrusted bool) *tool.Tool {
	name := t.Name // wire name; the tool.Tool may be renamed by Register
	return &tool.Tool{
		Name:        name,
		Description: t.Description,
		Label:       displayTitle(t),
		Schema:      inputSchema(t.InputSchema),
		Annotations: annotationsFor(hintsOf(t.Annotations)),
		Fn: func(ctx context.Context, input json.RawMessage) (any, error) {
			return c.call(ctx, name, input, untrusted)
		},
	}
}

// call forwards one tools/call and maps the outcome; see the package doc.
// With untrusted set, a successful value is wrapped in
// tool.UntrustedEnvelope; soft failures are returned bare.
func (c *Client) call(parent context.Context, name string, input json.RawMessage, untrusted bool) (any, error) {
	if c.isClosed() {
		return nil, ErrClosed
	}
	args := bytes.TrimSpace(input)
	if len(args) == 0 || bytes.Equal(args, []byte("null")) {
		args = []byte("{}")
	}
	ctx := parent
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parent, c.timeout)
		defer cancel()
	}
	res, err := c.session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		switch {
		case parent.Err() != nil:
			return nil, parent.Err()
		case ctx.Err() != nil:
			return tool.Soft(tool.ErrorCodeUpstreamTimeout,
				fmt.Sprintf("%s: %s did not answer within %s", c.name, name, c.timeout)), nil
		default:
			return nil, fmt.Errorf("mcp: %s: call %s: %w", c.name, name, err)
		}
	}
	v, soft := mapResult(res)
	if untrusted && !soft {
		return tool.UntrustedEnvelope(v), nil
	}
	return v, nil
}

// displayTitle follows the specification's display-name precedence:
// title, then annotations.title; "" leaves Label unset so UIs fall back to
// the registered name.
func displayTitle(t *sdk.Tool) string {
	if t.Title != "" {
		return t.Title
	}
	if t.Annotations != nil {
		return t.Annotations.Title
	}
	return ""
}

// inputSchema passes the server's inputSchema through as a map. The SDK
// decodes it as map[string]any on the client side; anything else is
// round-tripped through JSON. Absent or empty becomes tool.Obj(nil), the
// empty object schema every provider accepts.
func inputSchema(v any) map[string]any {
	switch s := v.(type) {
	case nil:
		return tool.Obj(nil)
	case map[string]any:
		if len(s) == 0 {
			return tool.Obj(nil)
		}
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return tool.Obj(nil)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || len(m) == 0 {
		return tool.Obj(nil)
	}
	return m
}

// annotationsFor maps declared hints onto tool.Annotations. An undeclared
// tool gets the zero value — dialogram's all-false default, which
// agent.PolicyInterceptor treats as allowed — rather than MCP's
// destructive/open-world defaults, so a server that simply did not
// annotate is not paused or denied on every call (RegisterOptions.
// AssumeOpenWorld is the opt-in for the cautious reading). Within a
// declared block the MCP defaults already applied by hintsOf stand, except
// that Destructive is cleared on a read-only tool: the specification makes
// destructiveHint meaningful only when readOnlyHint is false.
func annotationsFor(h Hints) tool.Annotations {
	if !h.Declared {
		return tool.Annotations{}
	}
	a := tool.Annotations{
		ReadOnly:    h.ReadOnly,
		Destructive: h.Destructive,
		Idempotent:  h.Idempotent,
		OpenWorld:   h.OpenWorld,
	}
	if a.ReadOnly {
		a.Destructive = false
	}
	return a
}

func hintsOf(a *sdk.ToolAnnotations) Hints {
	h := Hints{Destructive: true, OpenWorld: true}
	if a == nil {
		return h
	}
	h.Declared = true
	h.Title = a.Title
	h.ReadOnly = a.ReadOnlyHint
	h.Idempotent = a.IdempotentHint
	if a.DestructiveHint != nil {
		h.Destructive = *a.DestructiveHint
	}
	if a.OpenWorldHint != nil {
		h.OpenWorld = *a.OpenWorldHint
	}
	return h
}
