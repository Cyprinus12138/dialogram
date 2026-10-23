package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Cyprinus12138/dialogram/tool"
)

// echoSchema is passed verbatim to the server and must come back through
// Tools() and reg.AnthropicSpec() unchanged.
var echoSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"msg": map[string]any{"type": "string", "description": "What to echo"},
	},
	"required": []any{"msg"},
}

func boolPtr(b bool) *bool { return &b }

// newTestServer builds an in-process MCP server with the tool shapes the
// package has to handle: structured output, text-only, isError, mixed
// content, annotations and a slow tool for the timeout path.
func newTestServer(t *testing.T) *sdk.Server {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "testsrv", Version: "0.0.1"},
		&sdk.ServerOptions{Instructions: "Use add for sums."})

	type addIn struct {
		A int `json:"a" jsonschema:"first addend"`
		B int `json:"b" jsonschema:"second addend"`
	}
	type addOut struct {
		Sum int `json:"sum"`
	}
	sdk.AddTool(srv, &sdk.Tool{Name: "add", Description: "Adds two integers."},
		func(_ context.Context, _ *sdk.CallToolRequest, in addIn) (*sdk.CallToolResult, addOut, error) {
			return nil, addOut{Sum: in.A + in.B}, nil
		})

	rawSchema, _ := json.Marshal(echoSchema)
	srv.AddTool(&sdk.Tool{Name: "echo", Description: "Echoes a message.", Title: "Echo it", InputSchema: json.RawMessage(rawSchema)},
		func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var in struct {
				Msg string `json:"msg"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &in)
			return &sdk.CallToolResult{Content: []sdk.Content{
				&sdk.TextContent{Text: "you said: " + in.Msg},
				&sdk.TextContent{Text: "(twice)"},
			}}, nil
		})

	srv.AddTool(&sdk.Tool{Name: "fail", Description: "Always fails.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "disk on fire"}}}, nil
		})

	srv.AddTool(&sdk.Tool{Name: "pic", Description: "Mixed content.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{
				&sdk.TextContent{Text: "here is a picture"},
				&sdk.ImageContent{Data: []byte{0x89, 'P', 'N', 'G'}, MIMEType: "image/png"},
				&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///notes.txt", MIMEType: "text/plain", Text: "hello"}},
				&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///blob.bin", MIMEType: "application/octet-stream", Blob: []byte{1, 2, 3}}},
				&sdk.ResourceLink{URI: "file:///other.txt", Name: "other", MIMEType: "text/plain"},
			}}, nil
		})

	srv.AddTool(&sdk.Tool{
		Name: "wipe", Description: "Destroys things.", InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: &sdk.ToolAnnotations{Title: "Wipe", DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(false), IdempotentHint: true},
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "wiped"}}}, nil
	})

	srv.AddTool(&sdk.Tool{
		Name: "peek", Description: "Reads things.", InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "peeked"}}}, nil
	})

	srv.AddTool(&sdk.Tool{Name: "slow", Description: "Sleeps.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			select {
			case <-time.After(2 * time.Second):
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "done"}}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	return srv
}

// connect wires a test server and a Client over in-memory transports.
func connect(t *testing.T, o Options) *Client {
	t.Helper()
	ctx := context.Background()
	srv := newTestServer(t)
	ct, st := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	c, err := Connect(ctx, rawTransport{ct}, o)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Close()
		_ = ss.Close()
	})
	return c
}

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not a JSON object: %s (%v)", s, err)
	}
	return m
}

func TestConnect_DefaultsFromServer(t *testing.T) {
	c := connect(t, Options{})
	if c.Name() != "testsrv" {
		t.Errorf("Name() = %q, want the advertised server name", c.Name())
	}
	if c.Instructions() != "Use add for sums." {
		t.Errorf("Instructions() = %q", c.Instructions())
	}
	if c.timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", c.timeout, DefaultTimeout)
	}
}

func TestConnect_Errors(t *testing.T) {
	if _, err := Connect(context.Background(), nil, Options{}); err == nil {
		t.Error("nil transport: want error")
	}
	if _, err := Connect(context.Background(), Stdio{}, Options{}); err == nil {
		t.Error("empty Stdio.Command: want error")
	}
	if _, err := Connect(context.Background(), StreamableHTTP{}, Options{}); err == nil {
		t.Error("empty StreamableHTTP.URL: want error")
	}
}

func TestTools_Conversion(t *testing.T) {
	c := connect(t, Options{Name: "demo"})
	tools, err := c.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	byName := map[string]*tool.Tool{}
	for _, tt := range tools {
		byName[tt.Name] = tt
	}
	var names []string
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	want := []string{"add", "echo", "fail", "peek", "pic", "slow", "wipe"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}

	echo := byName["echo"]
	if echo.Description != "Echoes a message." {
		t.Errorf("echo description = %q", echo.Description)
	}
	if echo.Label != "Echo it" {
		t.Errorf("echo label = %q, want the server title", echo.Label)
	}
	if !reflect.DeepEqual(echo.Schema, echoSchema) {
		t.Errorf("echo schema not passed through:\n got %v\nwant %v", echo.Schema, echoSchema)
	}
	if byName["wipe"].Label != "Wipe" {
		t.Errorf("wipe label = %q, want the annotations title", byName["wipe"].Label)
	}
	if byName["fail"].Label != "" {
		t.Errorf("fail label = %q, want empty", byName["fail"].Label)
	}

	add := byName["add"].Schema
	if add["type"] != "object" {
		t.Errorf("add schema type = %v", add["type"])
	}
	props, _ := add["properties"].(map[string]any)
	a, _ := props["a"].(map[string]any)
	if a["type"] != "integer" || a["description"] != "first addend" {
		t.Errorf("add schema property a = %v", a)
	}
	for _, tt := range tools {
		if tt.Fn == nil {
			t.Errorf("%s: nil Fn", tt.Name)
		}
	}
}

func TestInputSchema_Fallbacks(t *testing.T) {
	empty := tool.Obj(nil)
	if got := inputSchema(nil); !reflect.DeepEqual(got, empty) {
		t.Errorf("nil → %v", got)
	}
	if got := inputSchema(map[string]any{}); !reflect.DeepEqual(got, empty) {
		t.Errorf("empty map → %v", got)
	}
	raw := json.RawMessage(`{"type":"object","properties":{"x":{"type":"number"}}}`)
	got := inputSchema(raw)
	if got["type"] != "object" || got["properties"].(map[string]any)["x"].(map[string]any)["type"] != "number" {
		t.Errorf("raw message → %v", got)
	}
	if got := inputSchema(json.RawMessage(`not json`)); !reflect.DeepEqual(got, empty) {
		t.Errorf("garbage → %v", got)
	}
}

func TestRegister_PrefixAllowDeny(t *testing.T) {
	ctx := context.Background()
	c := connect(t, Options{Name: "demo"})

	reg := tool.NewRegistry()
	names, err := Register(ctx, reg, c, RegisterOptions{Allow: []string{"add", "echo", "fail"}, Deny: []string{"echo"}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	sort.Strings(names)
	if want := []string{"demo__add", "demo__fail"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if reg.Len() != 2 {
		t.Errorf("registry has %d tools, want 2", reg.Len())
	}
	if _, ok := reg.Lookup("demo__add"); !ok {
		t.Error("demo__add not registered")
	}

	// Custom prefix.
	reg2 := tool.NewRegistry()
	names, err = Register(ctx, reg2, c, RegisterOptions{Prefix: "x_", Allow: []string{"peek"}})
	if err != nil || !reflect.DeepEqual(names, []string{"x_peek"}) {
		t.Errorf("custom prefix: names=%v err=%v", names, err)
	}

	// NoPrefix wins over Prefix.
	reg3 := tool.NewRegistry()
	names, err = Register(ctx, reg3, c, RegisterOptions{Prefix: "x_", NoPrefix: true, Allow: []string{"peek"}})
	if err != nil || !reflect.DeepEqual(names, []string{"peek"}) {
		t.Errorf("NoPrefix: names=%v err=%v", names, err)
	}

	// Empty non-nil Allow registers nothing.
	reg4 := tool.NewRegistry()
	names, err = Register(ctx, reg4, c, RegisterOptions{Allow: []string{}})
	if err != nil || len(names) != 0 || reg4.Len() != 0 {
		t.Errorf("empty allow: names=%v err=%v len=%d", names, err, reg4.Len())
	}

	// Collision: all-or-nothing, no panic.
	before := reg.Len()
	if _, err := Register(ctx, reg, c, RegisterOptions{Allow: []string{"peek", "add"}}); err == nil {
		t.Error("re-registering demo__add: want error")
	}
	if reg.Len() != before {
		t.Errorf("collision registered %d tools, want none", reg.Len()-before)
	}

	// Nil arguments.
	if _, err := Register(ctx, nil, c, RegisterOptions{}); err == nil {
		t.Error("nil registry: want error")
	}
	if _, err := Register(ctx, reg, nil, RegisterOptions{}); err == nil {
		t.Error("nil client: want error")
	}
}

func TestCall_RoundTrip(t *testing.T) {
	ctx := context.Background()
	c := connect(t, Options{Name: "demo"})
	reg := tool.NewRegistry()
	if _, err := Register(ctx, reg, c, RegisterOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	t.Run("structured", func(t *testing.T) {
		out, err := reg.Call(ctx, "demo__add", `{"a":2,"b":3}`)
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if got := mustJSON(t, out); got["sum"] != float64(5) {
			t.Errorf("out = %s", out)
		}
	})

	t.Run("text", func(t *testing.T) {
		out, err := reg.Call(ctx, "demo__echo", `{"msg":"hi"}`)
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		got := mustJSON(t, out)
		if got["text"] != "you said: hi\n(twice)" {
			t.Errorf("text = %q", got["text"])
		}
		if _, has := got["attachments"]; has {
			t.Errorf("text-only result carries attachments: %s", out)
		}
	})

	t.Run("is_error", func(t *testing.T) {
		out, err := reg.Call(ctx, "demo__fail", `{}`)
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		got := mustJSON(t, out)
		if got["error_code"] != ErrorCodeToolError || got["error_message"] != "disk on fire" || got["retryable"] != false {
			t.Errorf("soft failure = %s", out)
		}
	})

	t.Run("mixed_content", func(t *testing.T) {
		out, err := reg.Call(ctx, "demo__pic", `{}`)
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if strings.Contains(out, "PNG") || strings.Contains(out, "AQID") {
			t.Errorf("binary leaked into the result: %s", out)
		}
		got := mustJSON(t, out)
		if got["text"] != "here is a picture" {
			t.Errorf("text = %q", got["text"])
		}
		att, _ := got["attachments"].([]any)
		if len(att) != 4 {
			t.Fatalf("attachments = %v, want 4", got["attachments"])
		}
		img := att[0].(map[string]any)
		if img["type"] != "image" || img["media_type"] != "image/png" || img["omitted"] != true {
			t.Errorf("image placeholder = %v", img)
		}
		txt := att[1].(map[string]any)
		if txt["type"] != "resource" || txt["uri"] != "file:///notes.txt" || txt["text"] != "hello" || txt["omitted"] != nil {
			t.Errorf("text resource = %v", txt)
		}
		blob := att[2].(map[string]any)
		if blob["type"] != "resource" || blob["uri"] != "file:///blob.bin" || blob["omitted"] != true || blob["text"] != nil {
			t.Errorf("blob resource = %v", blob)
		}
		link := att[3].(map[string]any)
		if link["type"] != "resource_link" || link["uri"] != "file:///other.txt" || link["name"] != "other" || link["media_type"] != "text/plain" {
			t.Errorf("resource link = %v", link)
		}
	})

	t.Run("empty_input", func(t *testing.T) {
		out, err := reg.Call(ctx, "demo__peek", "")
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if mustJSON(t, out)["text"] != "peeked" {
			t.Errorf("out = %s", out)
		}
		if _, err := c.call(ctx, "peek", json.RawMessage("null")); err != nil {
			t.Errorf("null input: %v", err)
		}
	})

	t.Run("unknown_tool_is_hard_error", func(t *testing.T) {
		if _, err := c.call(ctx, "nope", nil); err == nil {
			t.Error("unknown MCP tool: want hard error")
		}
	})
}

func TestCall_Timeout(t *testing.T) {
	ctx := context.Background()
	c := connect(t, Options{Name: "demo", Timeout: 50 * time.Millisecond})
	reg := tool.NewRegistry()
	if _, err := Register(ctx, reg, c, RegisterOptions{Allow: []string{"slow"}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	started := time.Now()
	out, err := reg.Call(ctx, "demo__slow", `{}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if el := time.Since(started); el > time.Second {
		t.Errorf("call took %v, timeout not honoured", el)
	}
	got := mustJSON(t, out)
	if got["error_code"] != tool.ErrorCodeUpstreamTimeout || got["retryable"] != true {
		t.Errorf("timeout result = %s", out)
	}
	if !strings.Contains(got["error_message"].(string), "slow") {
		t.Errorf("error_message = %q, want the tool name", got["error_message"])
	}
}

func TestCall_ParentCancelled(t *testing.T) {
	c := connect(t, Options{Name: "demo"})
	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := c.call(ctx, "slow", nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled as a hard error", err)
	}
}

func TestClose_Idempotent(t *testing.T) {
	ctx := context.Background()
	c := connect(t, Options{Name: "demo"})
	tools, err := c.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := c.Tools(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Tools after Close: %v, want ErrClosed", err)
	}
	if _, err := tools[0].Fn(ctx, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Fn after Close: %v, want ErrClosed", err)
	}
}

func TestAnthropicSpec_PassesSchemaThrough(t *testing.T) {
	ctx := context.Background()
	c := connect(t, Options{Name: "demo"})
	reg := tool.NewRegistry()
	reg.Register(&tool.Tool{Name: "local", Description: "in-process", Schema: tool.Obj(nil),
		Fn: func(context.Context, json.RawMessage) (any, error) { return nil, nil }})
	if _, err := Register(ctx, reg, c, RegisterOptions{Allow: []string{"echo"}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	spec := reg.AnthropicSpec()
	if len(spec) != 2 {
		t.Fatalf("spec has %d entries, want 2", len(spec))
	}
	entry := spec[1]
	if entry["name"] != "demo__echo" || entry["description"] != "Echoes a message." {
		t.Errorf("entry = %v", entry)
	}
	if !reflect.DeepEqual(entry["input_schema"], echoSchema) {
		t.Errorf("input_schema = %v, want %v", entry["input_schema"], echoSchema)
	}
	// And it survives marshalling the way the provider sends it.
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"What to echo"`) {
		t.Errorf("marshalled spec lost the schema: %s", b)
	}
	if o := reg.OpenAISpec(); !reflect.DeepEqual(o[1]["function"].(map[string]any)["parameters"], echoSchema) {
		t.Errorf("openai parameters = %v", o[1]["function"])
	}
}

func TestAnnotations(t *testing.T) {
	c := connect(t, Options{Name: "demo"})
	if _, ok := c.Annotations("wipe"); ok {
		t.Error("Annotations before Tools: want ok=false")
	}
	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	cases := []struct {
		name string
		want Hints
		ok   bool
	}{
		{"wipe", Hints{Title: "Wipe", Destructive: true, OpenWorld: false, Idempotent: true, Declared: true}, true},
		{"peek", Hints{ReadOnly: true, Destructive: true, OpenWorld: true, Declared: true}, true},
		{"add", Hints{Destructive: true, OpenWorld: true}, true},
		{"nope", Hints{}, false},
	}
	for _, tc := range cases {
		got, ok := c.Annotations(tc.name)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Annotations(%q) = %+v, %v; want %+v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMapResult_Edges(t *testing.T) {
	if got := mapResult(nil); !reflect.DeepEqual(got, map[string]any{"text": ""}) {
		t.Errorf("nil result → %v", got)
	}
	got := mapResult(&sdk.CallToolResult{IsError: true})
	if m := got.(map[string]any); m["error_code"] != ErrorCodeToolError || m["error_message"] == "" {
		t.Errorf("isError without text → %v", got)
	}
	// IsError wins over structured content.
	got = mapResult(&sdk.CallToolResult{IsError: true, StructuredContent: map[string]any{"x": 1},
		Content: []sdk.Content{&sdk.TextContent{Text: "bad"}}})
	if m := got.(map[string]any); m["error_message"] != "bad" {
		t.Errorf("isError with structured → %v", got)
	}
	// Structured primitives pass through.
	if got := mapResult(&sdk.CallToolResult{StructuredContent: float64(42)}); got != float64(42) {
		t.Errorf("structured primitive → %v", got)
	}
	// Audio and empty embedded resource.
	got = mapResult(&sdk.CallToolResult{Content: []sdk.Content{
		&sdk.AudioContent{Data: []byte{1}, MIMEType: "audio/wav"},
		&sdk.EmbeddedResource{},
	}})
	att := got.(map[string]any)["attachments"].([]map[string]any)
	if att[0]["type"] != "audio" || att[0]["omitted"] != true || att[1]["type"] != "resource" || att[1]["omitted"] != true {
		t.Errorf("audio/empty resource → %v", att)
	}
}

type captureRT struct{ got *http.Request }

func (c *captureRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c.got = req
	return &http.Response{StatusCode: 204, Body: http.NoBody, Request: req}, nil
}

func TestStreamableHTTP_Headers(t *testing.T) {
	base := &captureRT{}
	tr, err := StreamableHTTP{
		URL:    "http://example.invalid/mcp",
		Header: http.Header{"Authorization": {"Bearer t"}, "x-api-key": {"k"}},
		Client: &http.Client{Transport: base},
	}.sdkTransport()
	if err != nil {
		t.Fatalf("sdkTransport: %v", err)
	}
	st, ok := tr.(*sdk.StreamableClientTransport)
	if !ok || st.Endpoint != "http://example.invalid/mcp" {
		t.Fatalf("transport = %#v", tr)
	}
	req, _ := http.NewRequest(http.MethodPost, st.Endpoint, http.NoBody)
	req.Header.Set("Content-Type", "application/json")
	if _, err := st.HTTPClient.Transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if base.got.Header.Get("Authorization") != "Bearer t" || base.got.Header.Get("X-Api-Key") != "k" {
		t.Errorf("headers not injected: %v", base.got.Header)
	}
	if base.got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("original headers lost: %v", base.got.Header)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("RoundTrip mutated the caller's request")
	}
}

func TestStdio_Transport(t *testing.T) {
	tr, err := Stdio{Command: "true", Args: []string{"-x"}, Env: []string{"A=1"}, Dir: "/"}.sdkTransport()
	if err != nil {
		t.Fatalf("sdkTransport: %v", err)
	}
	ct, ok := tr.(*sdk.CommandTransport)
	if !ok {
		t.Fatalf("transport = %#v", tr)
	}
	if ct.Command.Dir != "/" || len(ct.Command.Args) != 2 || ct.Command.Args[1] != "-x" {
		t.Errorf("cmd = %v dir=%q", ct.Command.Args, ct.Command.Dir)
	}
	if ct.Command.Env == nil || ct.Command.Env[len(ct.Command.Env)-1] != "A=1" {
		t.Errorf("env not appended: %v", ct.Command.Env)
	}
	tr, _ = Stdio{Command: "true"}.sdkTransport()
	if tr.(*sdk.CommandTransport).Command.Env != nil {
		t.Error("nil Env should inherit (cmd.Env nil)")
	}
}
