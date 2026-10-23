// Package replay serves recorded upstream responses to the provider
// adapters under test and records what each adapter sent. A test lists
// fixture files in the order the upstream would answer; request N receives
// fixture N. That drives the real loop — request building, SSE / JSON
// parsing, tool dispatch, the follow-up request — against hand-written
// copies of the real wire formats, with no network and no API key.
package replay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/internal/loop"
	"github.com/Cyprinus12138/dialogram/tool"
)

// Fixture is one recorded upstream response: a file, relative to the test
// package's directory, and the HTTP status it is served with.
type Fixture struct {
	Path   string
	Status int
}

// File is a 200 response read from path. The content type follows the
// extension: .sse is served as text/event-stream, anything else as
// application/json.
func File(path string) Fixture { return Fixture{Path: path, Status: http.StatusOK} }

// Status is File served with another HTTP status — an upstream error body.
func Status(code int, path string) Fixture { return Fixture{Path: path, Status: code} }

type recorded struct {
	header http.Header
	body   []byte
}

// Server replays fixtures in order and keeps every request it saw. A
// request past the last fixture fails the test and is answered with a 500,
// so a loop that makes one call too many cannot pass by accident.
type Server struct {
	// URL is the base URL to hand the provider under test.
	URL string

	t        testing.TB
	srv      *httptest.Server
	fixtures []Fixture
	bodies   [][]byte
	mu       sync.Mutex
	reqs     []recorded
}

// New loads every fixture up front (a missing file fails immediately) and
// starts the server. It is closed when the test ends.
func New(t testing.TB, fixtures ...Fixture) *Server {
	t.Helper()
	s := &Server{t: t, fixtures: fixtures}
	for _, f := range fixtures {
		b, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		s.bodies = append(s.bodies, b)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	idx := len(s.reqs)
	s.reqs = append(s.reqs, recorded{header: r.Header.Clone(), body: body})
	s.mu.Unlock()
	if idx >= len(s.fixtures) {
		s.t.Errorf("replay: request %d has no fixture (%d recorded)", idx, len(s.fixtures))
		http.Error(w, "no fixture", http.StatusInternalServerError)
		return
	}
	f := s.fixtures[idx]
	if strings.HasSuffix(f.Path, ".sse") {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(f.Status)
	_, _ = w.Write(s.bodies[idx])
}

// Count is the number of requests served so far.
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func (s *Server) req(i int) recorded {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.reqs) {
		s.t.Fatalf("replay: request %d was never made (%d seen)", i, len(s.reqs))
	}
	return s.reqs[i]
}

// Raw is the exact body of request i.
func (s *Server) Raw(i int) []byte { s.t.Helper(); return s.req(i).body }

// Header is the header set of request i.
func (s *Server) Header(i int) http.Header { s.t.Helper(); return s.req(i).header }

// Request is the decoded JSON body of request i. Walk it with Get.
func (s *Server) Request(i int) map[string]any {
	s.t.Helper()
	var m map[string]any
	if err := json.Unmarshal(s.Raw(i), &m); err != nil {
		s.t.Fatalf("replay: request %d is not a JSON object: %v", i, err)
	}
	return m
}

// Get walks a decoded JSON value: a string step indexes an object, an int
// step indexes an array. It returns nil as soon as a step does not apply,
// so a test compares against the expected leaf in one expression.
func Get(v any, path ...any) any {
	for _, step := range path {
		switch k := step.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			a, ok := v.([]any)
			if !ok || k < 0 || k >= len(a) {
				return nil
			}
			v = a[k]
		default:
			return nil
		}
	}
	return v
}

// Str is Get for a string leaf; a missing or non-string leaf is "".
func Str(v any, path ...any) string {
	s, _ := Get(v, path...).(string)
	return s
}

// Len is the length of the JSON array at path, or -1 when there is none.
func Len(v any, path ...any) int {
	a, ok := Get(v, path...).([]any)
	if !ok {
		return -1
	}
	return len(a)
}

// Decode parses raw JSON (a tool input, a tool result) into the same
// generic shape Request returns, so Get works on it too.
func Decode(t testing.TB, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("replay: not JSON: %v: %s", err, raw)
	}
	return v
}

// ShortBackoff shrinks the loop's retry backoff for the duration of the
// test so a transient-error scenario replays in milliseconds.
func ShortBackoff(t testing.TB) {
	prev := loop.BackoffBase
	loop.BackoffBase = time.Millisecond
	t.Cleanup(func() { loop.BackoffBase = prev })
}

// Events collects the stream events a ChatStream call emits, in order, so
// a test asserts on the whole sequence after the call returns.
type Events struct {
	Types       []string
	Text        string
	Turns       []agent.Turn
	ToolCalls   []agent.StreamEvent
	ToolResults []agent.StreamEvent
	Usage       *agent.StreamEvent
	Pause       *agent.StreamEvent
	Done        bool
}

// Emit is the agent.EmitFn to hand to ChatStream.
func (e *Events) Emit(ev agent.StreamEvent) {
	e.Types = append(e.Types, ev.Type)
	switch ev.Type {
	case agent.EventText:
		e.Text += ev.Delta
	case agent.EventTurn:
		if ev.Turn != nil {
			e.Turns = append(e.Turns, *ev.Turn)
		}
	case agent.EventToolCall:
		e.ToolCalls = append(e.ToolCalls, ev)
	case agent.EventToolResult:
		e.ToolResults = append(e.ToolResults, ev)
	case agent.EventUsage:
		u := ev
		e.Usage = &u
	case agent.EventWaitingForUser:
		p := ev
		e.Pause = &p
	case agent.EventDone:
		e.Done = true
	}
}

// Order is the comma-joined event type sequence, e.g. "text,turn,usage,done".
func (e *Events) Order() string { return strings.Join(e.Types, ",") }

// ---------- the fixture conversation ----------

const (
	// SystemPrompt is the stable system prompt of the replay profile.
	SystemPrompt = "You are a travel helper."
	// Memory is the volatile block passed as ChatInput.MemoryContext.
	Memory = "## Memory\n- prefers window seats\n"
	// PNG is a 1x1 PNG, base64-encoded, for the image scenario.
	PNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg=="
)

// Config is the agent configuration every scenario runs under: one profile
// carrying SystemPrompt, no tool restriction, the default step cap.
func Config() *agent.Config {
	return &agent.Config{
		Profiles:       map[string]agent.Profile{"assistant": {SystemPrompt: SystemPrompt}},
		DefaultProfile: "assistant",
	}
}

// Registry registers the tools the fixtures call, in the order their specs
// must appear on the wire: get_weather returns weather as a bare result
// (nil means {"high_c":27,"conditions":"sunny"}); search_flights returns
// an envelope whose runtime_state must never reach the wire; then the
// three built-in pause tools. get_weather deliberately finishes after
// search_flights, so a parallel turn proves result order follows the
// model's order rather than completion order.
func Registry(t testing.TB, weather any) *tool.Registry {
	t.Helper()
	if weather == nil {
		weather = map[string]any{"high_c": 27, "conditions": "sunny"}
	}
	reg := tool.NewRegistry()
	reg.Register(&tool.Tool{
		Name: "get_weather", Description: "Current weather for a city.",
		Schema: tool.Obj(map[string]any{"city": map[string]any{"type": "string"}}, "city"),
		Fn: func(context.Context, json.RawMessage) (any, error) {
			time.Sleep(20 * time.Millisecond)
			return weather, nil
		},
	})
	reg.Register(&tool.Tool{
		Name: "search_flights", Description: "Direct flights between two airports.",
		Schema: tool.Obj(map[string]any{"from": map[string]any{"type": "string"}, "to": map[string]any{"type": "string"}}, "from", "to"),
		Fn: func(context.Context, json.RawMessage) (any, error) {
			return &tool.Envelope{
				ModelObservation: map[string]any{"n": 2, "cheapest_eur": 89},
				RuntimeState: map[string]any{"rows": []any{
					map[string]any{"carrier": "SKYLINE", "fare": 89},
					map[string]any{"carrier": "IBERWING", "fare": 104},
				}},
			}, nil
		},
	})
	tool.RegisterHITL(reg)
	return reg
}
