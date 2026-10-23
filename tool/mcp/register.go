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

	// TODO: Untrusted bool — set Envelope.Provenance on every result from
	// this server once that field lands from the provenance branch, so
	// downstream consumers can tell server-supplied content from
	// in-process tool output.
}

// Register lists the client's tools, filters them by Allow and Deny,
// prefixes their names and adds them to reg. It returns the registered
// (prefixed) names in the server's order.
//
// Registration is all-or-nothing: if any prefixed name is already present
// in reg (or appears twice in the listing), Register returns an error and
// adds nothing, instead of letting tool.Registry.Register panic halfway.
func Register(ctx context.Context, reg *tool.Registry, c *Client, o RegisterOptions) ([]string, error) {
	if reg == nil || c == nil {
		return nil, errors.New("mcp: Register needs a registry and a client")
	}
	tools, err := c.Tools(ctx)
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
