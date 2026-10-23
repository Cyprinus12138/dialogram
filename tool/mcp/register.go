package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/Cyprinus12138/dialogram/tool"
)

// RegisterOptions controls how a server's tools land in a tool.Registry.
type RegisterOptions struct {
	// Prefix is prepended to every tool name. Empty means Client.Name() +
	// "__" (e.g. "filesystem__read_file"), which keeps two servers that
	// both export read_file apart. Set NoPrefix to register bare names.
	Prefix string
	// NoPrefix registers the server's names unchanged, ignoring Prefix.
	NoPrefix bool
	// Allow restricts registration to these MCP tool names (before the
	// prefix). nil means every tool; an empty non-nil list means none —
	// the same convention as tool.Registry.Visible.
	Allow []string
	// Deny skips these MCP tool names (before the prefix). Applied after
	// Allow.
	Deny []string
	// Untrusted wraps every successful result from this server in
	// tool.UntrustedEnvelope, so tool.ProjectModelObservation fences the
	// observation (as "untrusted_content" with tool.UntrustedContentNote)
	// before the model reads it. Soft failures (MCP_TOOL_ERROR,
	// UPSTREAM_TIMEOUT) stay bare so the loop's error_code handling is
	// unchanged. Set it for any server you do not operate.
	Untrusted bool
	// AssumeOpenWorld marks tools the server did not annotate as
	// OpenWorld, so a deployment running agent.PolicyInterceptor with
	// Policy.PauseOpenWorld pauses every undeclared remote tool. Tools
	// with a declared annotations block are never overridden.
	AssumeOpenWorld bool
}

// Register lists the client's tools, filters them by Allow and Deny,
// prefixes their names and adds them to reg. It returns the registered
// (prefixed) names in the server's order.
//
// Registration is all-or-nothing: if any prefixed name is already present
// in reg (or appears twice in the listing), Register returns an error and
// adds nothing, instead of letting tool.Registry.Register panic halfway.
//
// Each tool's Annotations are mapped from the hints the server declared
// (Client.Annotations): within a declared block, omitted hints take the
// MCP defaults (destructive and open-world true) and Destructive is
// cleared on a read-only tool, as the specification makes it meaningful
// only when readOnlyHint is false. A tool with no annotations block gets
// the zero tool.Annotations — dialogram's all-false default, which
// agent.PolicyInterceptor treats as allowed — rather than MCP's
// destructive/open-world defaults, so an unannotated server is not paused
// or denied on every call; AssumeOpenWorld opts those undeclared tools
// into OpenWorld.
func Register(ctx context.Context, reg *tool.Registry, c *Client, o RegisterOptions) ([]string, error) {
	if reg == nil || c == nil {
		return nil, errors.New("mcp: Register needs a registry and a client")
	}
	tools, err := c.list(ctx, o.Untrusted)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if !o.NoPrefix {
		prefix = o.Prefix
		if prefix == "" {
			prefix = c.Name() + "__"
		}
	}
	allow := stringSet(o.Allow)
	deny := stringSet(o.Deny)

	picked := make([]*tool.Tool, 0, len(tools))
	names := make([]string, 0, len(tools))
	seen := map[string]bool{}
	for _, t := range tools {
		if allow != nil && !allow[t.Name] {
			continue
		}
		if deny[t.Name] {
			continue
		}
		full := prefix + t.Name
		if _, dup := reg.Lookup(full); dup || seen[full] {
			return nil, fmt.Errorf("mcp: %s: tool %q is already registered", c.Name(), full)
		}
		seen[full] = true
		if o.AssumeOpenWorld {
			if h, ok := c.Annotations(t.Name); ok && !h.Declared {
				t.Annotations.OpenWorld = true
			}
		}
		t.Name = full
		picked = append(picked, t)
		names = append(names, full)
	}
	for _, t := range picked {
		reg.Register(t)
	}
	return names, nil
}

// stringSet mirrors tool.Registry's convention: nil for no filtering, a
// lookup map otherwise (an empty non-nil slice filters everything).
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
