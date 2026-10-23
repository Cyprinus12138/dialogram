package mcp

import (
	"errors"
	"net/http"
	"os"
	"os/exec"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Transport describes how to reach an MCP server. Stdio and StreamableHTTP
// cover the two transports the specification defines; callers never touch
// the SDK's transport types. A Transport value is single-use: pass a fresh
// one to each Connect.
type Transport interface {
	sdkTransport() (sdk.Transport, error)
}

// Stdio starts the server as a child process and speaks newline-delimited
// JSON-RPC over its stdin and stdout — the transport most local servers
// use, e.g. Command "npx", Args {"-y", "@modelcontextprotocol/server-filesystem", "/tmp"}.
//
// Env entries (KEY=VALUE) are appended to this process's environment, a
// later entry overriding an earlier one with the same key; nil inherits
// the environment unchanged. Dir is the child's working directory (empty
// inherits). The child's stderr is forwarded to this process's stderr so
// server logs stay visible. Close terminates the child.
type Stdio struct {
	Command string
	Args    []string
	Env     []string
	Dir     string
}

func (s Stdio) sdkTransport() (sdk.Transport, error) {
	if s.Command == "" {
		return nil, errors.New("mcp: Stdio.Command is required")
	}
	cmd := exec.Command(s.Command, s.Args...)
	cmd.Dir = s.Dir
	if len(s.Env) > 0 {
		cmd.Env = append(os.Environ(), s.Env...)
	}
	cmd.Stderr = os.Stderr
	return &sdk.CommandTransport{Command: cmd}, nil
}

// StreamableHTTP connects to a server exposing the streamable HTTP
// transport at URL. Header is added to every request (bearer tokens, API
// keys); Client is the HTTP client to use and defaults to a zero
// http.Client (no timeout — the transport holds a long-lived SSE stream).
type StreamableHTTP struct {
	URL    string
	Header http.Header
	Client *http.Client
}

func (s StreamableHTTP) sdkTransport() (sdk.Transport, error) {
	if s.URL == "" {
		return nil, errors.New("mcp: StreamableHTTP.URL is required")
	}
	hc := s.Client
	if hc == nil {
		hc = &http.Client{}
	}
	if len(s.Header) > 0 {
		base := hc.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		clone := *hc
		clone.Transport = headerRoundTripper{base: base, header: s.Header.Clone()}
		hc = &clone
	}
	return &sdk.StreamableClientTransport{Endpoint: s.URL, HTTPClient: hc}, nil
}

// headerRoundTripper sets fixed headers on every outgoing request. It
// clones the request first, as the RoundTripper contract requires.
type headerRoundTripper struct {
	base   http.RoundTripper
	header http.Header
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, vs := range h.header {
		req.Header[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
	return h.base.RoundTrip(req)
}

// rawTransport wraps an SDK transport (in-memory in tests).
type rawTransport struct{ t sdk.Transport }

func (r rawTransport) sdkTransport() (sdk.Transport, error) {
	if r.t == nil {
		return nil, errors.New("mcp: nil SDK transport")
	}
	return r.t, nil
}
