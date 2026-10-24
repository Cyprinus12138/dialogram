package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Cyprinus12138/dialogram"
	"github.com/Cyprinus12138/dialogram/tool/mcp"
)

type noteIn struct {
	ID string `json:"id"`
}

type noteOut struct {
	ID     string `json:"id,omitempty"`
	Text   string `json:"text,omitempty"`
	Erased bool   `json:"erased,omitempty"`
}

// notesServer serves a small MCP server over streamable HTTP: a read-only
// lookup, a destructive erase that counts its calls, and a ping with no
// annotations at all.
func notesServer(t *testing.T, erased *atomic.Int32) string {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "notes", Version: "0"},
		&sdk.ServerOptions{Instructions: "Look a note up before erasing it."})
	closed, yes := false, true
	sdk.AddTool(srv, &sdk.Tool{Name: "lookup", Description: "Finds a note.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closed}},
		func(_ context.Context, _ *sdk.CallToolRequest, in noteIn) (*sdk.CallToolResult, noteOut, error) {
			return nil, noteOut{ID: in.ID, Text: "buy milk"}, nil
		})
	sdk.AddTool(srv, &sdk.Tool{Name: "erase", Description: "Erases a note.",
		Annotations: &sdk.ToolAnnotations{DestructiveHint: &yes, OpenWorldHint: &closed}},
		func(_ context.Context, _ *sdk.CallToolRequest, in noteIn) (*sdk.CallToolResult, noteOut, error) {
			erased.Add(1)
			return nil, noteOut{ID: in.ID, Erased: true}, nil
		})
	sdk.AddTool(srv, &sdk.Tool{Name: "ping", Description: "Pings the note service."},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, noteOut, error) {
			return nil, noteOut{Text: "pong"}, nil
		})
	hs := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil))
	t.Cleanup(hs.Close)
	return hs.URL
}

// upstream is a fake chat/completions endpoint: request n gets streams[n],
// and every request body is kept.
type upstream struct {
	mu      sync.Mutex
	streams []string
	bodies  []map[string]any
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	u.mu.Lock()
	n := len(u.bodies)
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()
	if n >= len(u.streams) {
		http.Error(w, "unexpected request", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, u.streams[n])
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies)
}

// messages returns the messages of upstream request i.
func (u *upstream) messages(t *testing.T, i int) []map[string]any {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if i >= len(u.bodies) {
		t.Fatalf("upstream saw %d requests, want request %d", len(u.bodies), i+1)
	}
	var out []map[string]any
	for _, m := range u.bodies[i]["messages"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

// result returns the tool message request i carries for the call to name.
// The loop replaces the model's call ids with neutral ones, so the lookup
// goes through the assistant message's tool_calls.
func (u *upstream) result(t *testing.T, i int, name string) string {
	t.Helper()
	names := map[any]string{}
	out := ""
	for _, m := range u.messages(t, i) {
		calls, _ := m["tool_calls"].([]any)
		for _, c := range calls {
			cm := c.(map[string]any)
			names[cm["id"]] = cm["function"].(map[string]any)["name"].(string)
		}
		if m["role"] == "tool" && names[m["tool_call_id"]] == name {
			out, _ = m["content"].(string)
		}
	}
	return out
}

func chunk(delta string) string {
	return fmt.Sprintf("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":null}]}\n\n", delta)
}

func finish(reason string) string {
	return fmt.Sprintf("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n\ndata: [DONE]\n\n", reason)
}

// call is one model step that requests a single tool.
func call(id, name, args string) string {
	a, _ := json.Marshal(args)
	return chunk(fmt.Sprintf(`{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]}`, id, name, a)) +
		finish("tool_calls")
}

// say is one model step that answers in text.
func say(text string) string {
	b, _ := json.Marshal(text)
	return chunk(fmt.Sprintf(`{"role":"assistant","content":%s}`, b)) + finish("stop")
}

// newTestSession wires a session to the notes server and to a fake model
// that replies with steps; stdin answers the approval prompts.
func newTestSession(t *testing.T, server string, o options, stdin string, steps ...string) (*session, *upstream, *strings.Builder) {
	t.Helper()
	u := &upstream{streams: steps}
	llm := httptest.NewServer(u)
	t.Cleanup(llm.Close)
	ctx := context.Background()
	c, err := mcp.Connect(ctx, mcp.StreamableHTTP{URL: server}, mcp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	out := &strings.Builder{}
	s, err := newSession(ctx, dialogram.Settings{Provider: dialogram.ProviderOpenAI, OpenAIAPIKey: "test", OpenAIBaseURL: llm.URL, OpenAIModel: "m"},
		c, o, strings.NewReader(stdin), out)
	if err != nil {
		t.Fatal(err)
	}
	return s, u, out
}

func TestReadOnlyToolsRunAndDestructiveOnesAsk(t *testing.T) {
	var erased atomic.Int32
	s, u, out := newTestSession(t, notesServer(t, &erased), options{}, "y\n",
		call("a", "notes__lookup", `{"id":"n1"}`),
		call("b", "notes__erase", `{"id":"n1"}`),
		say("Erased n1."),
	)
	if err := s.ask(context.Background(), "Erase my milk note."); err != nil {
		t.Fatal(err)
	}

	if sys := u.messages(t, 0)[0]; sys["role"] != "system" || !strings.Contains(sys["content"].(string), "Look a note up before erasing it.") {
		t.Errorf("the server's instructions are not in the system prompt: %v", sys)
	}
	if got := u.result(t, 1, "notes__lookup"); !strings.Contains(got, "buy milk") {
		t.Errorf("lookup result = %q", got)
	}
	if got := u.result(t, 2, "notes__erase"); !strings.Contains(got, `"erased":true`) {
		t.Errorf("erase result = %q", got)
	}
	if erased.Load() != 1 || u.count() != 3 {
		t.Errorf("erase ran %d times over %d model requests, want 1 over 3", erased.Load(), u.count())
	}
	if o := out.String(); !strings.Contains(o, "? run notes__erase") || strings.Contains(o, "? run notes__lookup") || !strings.Contains(o, "Erased n1.") {
		t.Errorf("only the destructive call should ask:\n%s", o)
	}
}

func TestDeclinedCallIsNotRun(t *testing.T) {
	var erased atomic.Int32
	s, u, _ := newTestSession(t, notesServer(t, &erased), options{}, "n\n",
		call("b", "notes__erase", `{"id":"n1"}`),
		say("Left it alone."),
	)
	if err := s.ask(context.Background(), "Erase n1."); err != nil {
		t.Fatal(err)
	}
	if erased.Load() != 0 {
		t.Errorf("a declined erase ran %d times", erased.Load())
	}
	if got := u.result(t, 1, "notes__erase"); !strings.Contains(got, "DENIED") || !strings.Contains(got, "user_rejected") {
		t.Errorf("the model should read a denial: %q", got)
	}
}

func TestUnannotatedAndListedToolsAsk(t *testing.T) {
	var erased atomic.Int32
	s, u, out := newTestSession(t, notesServer(t, &erased), options{approve: []string{"notes__lookup"}}, "y\ny\n",
		call("p", "notes__ping", `{}`),
		call("a", "notes__lookup", `{"id":"n1"}`),
		say("Pong, and the note says buy milk."),
	)
	if err := s.ask(context.Background(), "Ping, then read n1."); err != nil {
		t.Fatal(err)
	}
	if o := out.String(); !strings.Contains(o, "? run notes__ping") || !strings.Contains(o, "? run notes__lookup") {
		t.Errorf("an unannotated tool and a listed one should both ask:\n%s", o)
	}
	if got := u.result(t, 1, "notes__ping"); !strings.Contains(got, "pong") {
		t.Errorf("ping result = %q", got)
	}
	if got := u.result(t, 2, "notes__lookup"); !strings.Contains(got, "buy milk") {
		t.Errorf("lookup result = %q", got)
	}
}

func TestApproveNamesMustBeRegistered(t *testing.T) {
	var erased atomic.Int32
	c, err := mcp.Connect(context.Background(), mcp.StreamableHTTP{URL: notesServer(t, &erased)}, mcp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = newSession(context.Background(), dialogram.Settings{Provider: dialogram.ProviderOpenAI, OpenAIAPIKey: "test"},
		c, options{approve: []string{"lookup"}}, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), `"lookup"`) || !strings.Contains(err.Error(), "notes__lookup") {
		t.Errorf("an unprefixed -approve name should be refused with the registered names: %v", err)
	}
}
