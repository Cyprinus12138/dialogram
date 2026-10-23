// Package tool is the tool layer of dialogram: the Tool contract, the
// Registry the agent loop dispatches through, the three-channel result
// Envelope, the ArtifactStore that backs opaque data handles, and a handful
// of built-in tools (human-in-the-loop pauses, inline UI rendering, a clock).
//
// A Tool is a name, a description, a JSON Schema and a Go function. The
// Registry turns a set of Tools into the wire-level specs each provider
// expects and dispatches calls by name. Tools that return *Envelope split
// their output into what the model hears, what the user sees and what the
// server keeps — see envelope.go.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Tool is one callable capability exposed to the model.
//
// Fn may return either a *Envelope (preferred for any tool that produces
// non-trivial data) or any other JSON-serialisable value (the bare-result
// path). When Fn returns *Envelope:
//   - the entire envelope is serialised into the persisted tool_result so a
//     paused conversation resumes on any provider with full state;
//   - provider translators project only model_observation onto the wire;
//   - the HTTP layer emits ui_payload as a gen_ui stream event;
//   - runtime_state is stashed in the registry's ArtifactStore keyed by the
//     in-flight tool_use_id, so handles in model_observation can be resolved
//     later by render_from_ref.
type Tool struct {
	Name        string
	Description string
	// Label is an optional human-readable display name a UI may show in
	// place of Name (e.g. a translation key or a friendly title).
	Label  string
	Schema map[string]any
	Fn     func(ctx context.Context, input json.RawMessage) (any, error)
	// Annotations are server-side behavioural hints (read-only, destructive,
	// idempotent, open-world). They drive policy code such as
	// agent.PolicyInterceptor and are never sent to the model: AnthropicSpec
	// and OpenAISpec ignore them, so the wire tool list and the eval
	// manifest hash are unchanged by annotating a tool.
	Annotations Annotations `json:"annotations,omitzero"`
	// Timeout is an optional per-tool execution deadline enforced by
	// LocalExecutor. 0 inherits LocalExecutorOptions.Timeout. Set it on
	// tools whose natural latency differs from the default — a slow report
	// generator, or a cache lookup that should fail fast.
	Timeout time.Duration
}

// Annotations are behavioural hints about a tool, mirroring the MCP tool
// annotations field for field:
//
//   - ReadOnly    ↔ readOnlyHint:    the tool does not modify its environment.
//   - Destructive ↔ destructiveHint: the tool may perform destructive,
//     irreversible updates. Only meaningful when ReadOnly is false.
//   - Idempotent  ↔ idempotentHint:  repeating the call with the same
//     arguments has no additional effect. Only meaningful when ReadOnly is
//     false, but harmless to set on a read-only tool.
//   - OpenWorld   ↔ openWorldHint:   the tool talks to an open world of
//     external entities (the web, third-party APIs) whose content the
//     application does not control.
//
// One deliberate difference from MCP: every field here defaults to false.
// MCP defaults destructiveHint and openWorldHint to true, which is the safe
// direction for a hint a server may have forgotten to set, but in-process
// tools are authored alongside the policy that reads them, so an explicit
// opt-in reads better than a default that every read-only tool has to
// unset. Translate accordingly when bridging to or from MCP.
//
// Like their MCP counterparts these are hints, not guarantees: the loop
// does not verify them, and a policy built on them is only as honest as
// the tool that declared them. Annotations are not model-visible — they
// shape what the server lets a tool do, not what the model is told.
type Annotations struct {
	ReadOnly    bool `json:"read_only,omitempty"`
	Destructive bool `json:"destructive,omitempty"`
	Idempotent  bool `json:"idempotent,omitempty"`
	OpenWorld   bool `json:"open_world,omitempty"`
}

// Per-call context keys for state the registry threads into Fn so
// envelope-returning tools can address the artifact store under the
// in-flight tool_use_id without explicit wiring.
type ctxKey int

const (
	ctxKeyToolUseID     ctxKey = 1
	ctxKeyArtifactStore ctxKey = 2
)

// WithCallContext returns ctx annotated with the in-flight tool_use_id and
// the registry's ArtifactStore. The dispatcher in the agent loop calls this
// before invoking Registry.Call so tools (and the registry's own
// envelope-handling code) can address the store with a stable key.
func WithCallContext(ctx context.Context, toolUseID string, store ArtifactStore) context.Context {
	if toolUseID != "" {
		ctx = context.WithValue(ctx, ctxKeyToolUseID, toolUseID)
	}
	if store != nil {
		ctx = context.WithValue(ctx, ctxKeyArtifactStore, store)
	}
	return ctx
}

// ToolUseIDFromContext returns the neutral tool_use_id for the in-flight
// call, or "" when not running inside a registry-dispatched call.
func ToolUseIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyToolUseID).(string); ok {
		return v
	}
	return ""
}

// ArtifactStoreFromContext returns the registry's ArtifactStore, or nil.
func ArtifactStoreFromContext(ctx context.Context) ArtifactStore {
	if v, ok := ctx.Value(ctxKeyArtifactStore).(ArtifactStore); ok {
		return v
	}
	return nil
}

// Call runs the tool against a raw JSON input string and returns the
// serialised result. An empty input is treated as "{}".
func (t *Tool) Call(ctx context.Context, input string) (string, error) {
	raw := json.RawMessage(input)
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	started := time.Now()
	slog.Debug("tool call", "name", t.Name, "input", truncate(input, 500))
	out, err := t.Fn(ctx, raw)
	if err != nil {
		slog.Warn("tool call failed", "name", t.Name, "err", err,
			"duration_ms", time.Since(started).Milliseconds())
		return "", err
	}
	// Envelope path: stash runtime_state in the artifact store keyed by
	// tool_use_id so downstream tools can resolve handles without
	// rehydrating data into the model context. The marshalled envelope
	// still carries runtime_state — provider translators strip it on the
	// wire via ProjectModelObservation, but persistence keeps the full
	// shape so failover replay still has the data.
	if env, ok := out.(*Envelope); ok && env != nil {
		if store := ArtifactStoreFromContext(ctx); store != nil && env.RuntimeState != nil {
			if id := ToolUseIDFromContext(ctx); id != "" {
				kind := ""
				if len(env.Artifacts) > 0 {
					kind = env.Artifacts[0].Type
				}
				_ = store.Put(ctx, id, kind, env.RuntimeState)
			}
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	slog.Debug("tool call done", "name", t.Name, "bytes", len(b),
		"duration_ms", time.Since(started).Milliseconds(),
		"output_preview", truncate(string(b), 300))
	return string(b), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Registry holds the tools the agent may call, in registration order, plus
// the ArtifactStore that backs handle-based tool composition.
type Registry struct {
	tools     map[string]*Tool
	order     []string
	artifacts ArtifactStore
}

// NewRegistry builds an empty registry with an in-memory artifact store.
// Call SetArtifactStore to swap in a different backend before registering
// tools.
func NewRegistry() *Registry {
	return &Registry{
		tools:     map[string]*Tool{},
		artifacts: NewMemoryArtifactStore(0),
	}
}

// Artifacts returns the registry's artifact store.
func (r *Registry) Artifacts() ArtifactStore { return r.artifacts }

// SetArtifactStore replaces the registry's artifact store. nil is ignored.
func (r *Registry) SetArtifactStore(s ArtifactStore) {
	if s != nil {
		r.artifacts = s
	}
}

// Register adds a tool. Registering the same name twice panics — a
// duplicate is a wiring bug that must fail at startup, not at the first
// ambiguous dispatch.
func (r *Registry) Register(t *Tool) {
	if t == nil || t.Name == "" {
		panic("tool: Register called with a nil or unnamed tool")
	}
	if _, dup := r.tools[t.Name]; dup {
		panic("tool: duplicate tool: " + t.Name)
	}
	r.tools[t.Name] = t
	r.order = append(r.order, t.Name)
}

// Lookup returns the registered tool with the given name.
func (r *Registry) Lookup(name string) (*Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Call dispatches to the named tool. The dispatcher should annotate ctx
// with WithCallContext so envelope-returning tools can stash runtime state
// under the in-flight tool_use_id. If ctx already carries an ArtifactStore
// the registry honours it; otherwise it injects its own.
func (r *Registry) Call(ctx context.Context, name, input string) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	if ArtifactStoreFromContext(ctx) == nil {
		ctx = WithCallContext(ctx, ToolUseIDFromContext(ctx), r.artifacts)
	}
	return t.Call(ctx, input)
}

// Names returns the registered tool names in registration order.
func (r *Registry) Names() []string { return append([]string(nil), r.order...) }

// Len reports the number of registered tools.
func (r *Registry) Len() int { return len(r.order) }

// Label returns the display label for a tool, or "" when the tool is not
// registered or has no Label.
func (r *Registry) Label(name string) string {
	if t, ok := r.tools[name]; ok {
		return t.Label
	}
	return ""
}

// Visible returns the tools that survive an allow-list, in registration
// order. A nil allow-list means every registered tool; an empty non-nil
// list means none.
func (r *Registry) Visible(allow []string) []*Tool {
	set := stringSet(allow)
	out := make([]*Tool, 0, len(r.order))
	for _, n := range r.order {
		if set != nil && !set[n] {
			continue
		}
		out = append(out, r.tools[n])
	}
	return out
}

// AnthropicSpec returns every tool in the shape the Anthropic Messages API
// expects: {name, description, input_schema}.
func (r *Registry) AnthropicSpec() []map[string]any { return r.AnthropicSpecFor(nil) }

// AnthropicSpecFor is AnthropicSpec filtered by an allow-list (nil = all).
func (r *Registry) AnthropicSpecFor(allow []string) []map[string]any {
	ts := r.Visible(allow)
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, map[string]any{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": t.Schema,
		})
	}
	return out
}

// OpenAISpec returns every tool in the shape OpenAI-compatible
// chat/completions endpoints expect: {type:"function", function:{name,
// description, parameters}}.
func (r *Registry) OpenAISpec() []map[string]any { return r.OpenAISpecFor(nil) }

// OpenAISpecFor is OpenAISpec filtered by an allow-list (nil = all).
func (r *Registry) OpenAISpecFor(allow []string) []map[string]any {
	ts := r.Visible(allow)
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Schema,
			},
		})
	}
	return out
}

// stringSet returns nil for a nil slice (no filtering) and a lookup map
// otherwise. An empty non-nil slice yields an empty map (filters all).
func stringSet(xs []string) map[string]bool {
	if xs == nil {
		return nil
	}
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}
