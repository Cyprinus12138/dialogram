package openai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/provider/internal/replay"
	"github.com/Cyprinus12138/dialogram/tool"
)

// The trace scenarios drive whole runs through the replay fixtures with an
// in-memory span exporter and assert the span tree: one invoke_agent run
// span, a chat span per upstream request and an execute_tool span per
// tool call, both children of the run.

func newTracing(t *testing.T, opts ...sdktrace.TracerProviderOption) (*sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(append([]sdktrace.TracerProviderOption{sdktrace.WithSyncer(exp)}, opts...)...)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp, exp
}

// spanTree indexes the exported spans of one run.
type spanTree struct {
	t     *testing.T
	all   tracetest.SpanStubs
	run   tracetest.SpanStub
	chats []tracetest.SpanStub
	tools []tracetest.SpanStub
}

func collect(t *testing.T, exp *tracetest.InMemoryExporter) spanTree {
	t.Helper()
	st := spanTree{t: t, all: exp.GetSpans()}
	runs := 0
	for _, s := range st.all {
		if s.InstrumentationScope.Name != agent.InstrumentationName {
			t.Errorf("span %q scope = %q", s.Name, s.InstrumentationScope.Name)
		}
		switch str(s, "gen_ai.operation.name") {
		case "invoke_agent":
			st.run = s
			runs++
		case "chat":
			st.chats = append(st.chats, s)
		case "execute_tool":
			st.tools = append(st.tools, s)
		default:
			t.Errorf("unexpected span %q", s.Name)
		}
	}
	if runs != 1 {
		t.Fatalf("run spans = %d, want 1 (spans: %v)", runs, names(st.all))
	}
	// Exported on End: requests end in order, so chats are in step order.
	for _, s := range append(append([]tracetest.SpanStub{}, st.chats...), st.tools...) {
		if s.Parent.SpanID() != st.run.SpanContext.SpanID() || s.SpanContext.TraceID() != st.run.SpanContext.TraceID() {
			t.Errorf("span %q is not a child of the run span", s.Name)
		}
	}
	return st
}

func names(ss tracetest.SpanStubs) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}

func value(kvs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, kv := range kvs {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func str(s tracetest.SpanStub, key string) string {
	v, _ := value(s.Attributes, key)
	return v.AsString()
}

func num(s tracetest.SpanStub, key string) int64 {
	v, _ := value(s.Attributes, key)
	return v.AsInt64()
}

func has(s tracetest.SpanStub, key string) bool {
	_, ok := value(s.Attributes, key)
	return ok
}

func eventNames(s tracetest.SpanStub) string {
	var out []string
	for _, e := range s.Events {
		out = append(out, e.Name)
	}
	return strings.Join(out, ",")
}

func event(t *testing.T, s tracetest.SpanStub, name string) sdktrace.Event {
	t.Helper()
	for _, e := range s.Events {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("span %q has no %q event (events: %s)", s.Name, name, eventNames(s))
	return sdktrace.Event{}
}

func run(t *testing.T, stream bool, ag *agent.Agent, in agent.ChatInput) (*agent.ChatOutput, *replay.Events, error) {
	t.Helper()
	if !stream {
		out, err := ag.Chat(context.Background(), in)
		return out, nil, err
	}
	var ev replay.Events
	err := ag.ChatStream(context.Background(), in, ev.Emit)
	return nil, &ev, err
}

func fixture(stream bool, name string) replay.Fixture {
	if stream {
		return replay.File("testdata/" + name + ".sse")
	}
	return replay.File("testdata/" + name + ".json")
}

// One tool step and a final answer: the run span carries the identity
// attributes and the checkpoint and done events; each request is a chat
// span with its usage; the tool call is an execute_tool span.
func TestTrace_ToolCallRun(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "stream"}[stream], func(t *testing.T) {
			tp, exp := newTracing(t)
			srv := replay.New(t, fixture(stream, "tool_call"), fixture(stream, "tool_call_final"))
			p := New(Options{APIKey: "test-key", Model: "gpt-4o-mini", BaseURL: srv.URL, Registry: replay.Registry(t, nil),
				Config: replay.Config(), Checkpointer: agent.NewMemCheckpointer()})
			in := replayInput(askWeather)
			in.Profile, in.MessageID = "assistant", "m-trace"
			out, ev, err := run(t, stream, agent.New(p, agent.Options{TracerProvider: tp}), in)
			if err != nil {
				t.Fatal(err)
			}
			var toolUseID, toolOutput string
			if stream {
				toolUseID, toolOutput = ev.ToolResults[0].ToolUseID, ev.ToolResults[0].Output
			} else {
				toolUseID, toolOutput = out.Turns[0].Blocks[0].ToolUseID, out.ToolCalls[0].Output
			}

			st := collect(t, exp)
			r := st.run
			if r.Name != "invoke_agent assistant" || r.SpanKind != trace.SpanKindInternal || r.Parent.IsValid() ||
				str(r, "gen_ai.provider.name") != "openai" || str(r, "gen_ai.request.model") != "gpt-4o-mini" ||
				str(r, "gen_ai.agent.name") != "assistant" || str(r, "dialogram.profile") != "assistant" ||
				str(r, "dialogram.message_id") != "m-trace" || r.Status.Code != codes.Unset {
				t.Errorf("run span = %s %v %v status %v", r.Name, r.SpanKind, r.Attributes, r.Status)
			}
			// Step 1 after the assistant turn (one call pending) and after
			// the tool turn, step 2 after the final answer, then done.
			if got := eventNames(r); got != "checkpoint,checkpoint,checkpoint,done" {
				t.Fatalf("run events = %s", got)
			}
			for i, want := range []struct{ step, pending int64 }{{1, 1}, {1, 0}, {2, 0}} {
				e := r.Events[i]
				if s, _ := value(e.Attributes, "dialogram.step"); s.AsInt64() != want.step {
					t.Errorf("checkpoint %d step = %v", i, s.AsInt64())
				}
				if pd, _ := value(e.Attributes, "dialogram.checkpoint.pending"); pd.AsInt64() != want.pending {
					t.Errorf("checkpoint %d pending = %v", i, pd.AsInt64())
				}
			}

			if len(st.chats) != 2 {
				t.Fatalf("chat spans = %v", names(st.all))
			}
			for i, want := range []struct {
				finish             string
				step, in, out, hit int64
			}{{"tool_calls", 1, 310, 18, 0}, {"stop", 2, 352, 14, 256}} {
				c := st.chats[i]
				fr, _ := value(c.Attributes, "gen_ai.response.finish_reasons")
				if c.Name != "chat gpt-4o-mini" || c.SpanKind != trace.SpanKindClient || c.Status.Code != codes.Unset ||
					strings.Join(fr.AsStringSlice(), ",") != want.finish || num(c, "dialogram.step") != want.step ||
					num(c, "gen_ai.usage.input_tokens") != want.in || num(c, "gen_ai.usage.output_tokens") != want.out ||
					num(c, "gen_ai.usage.cache_read.input_tokens") != want.hit || has(c, "gen_ai.usage.cache_write.input_tokens") ||
					num(c, "dialogram.attempts") != 1 || str(c, "server.address") != "127.0.0.1" ||
					num(c, "gen_ai.request.max_tokens") != 4096 || has(c, "gen_ai.request.stream") != stream {
					t.Errorf("chat span %d = %s %v", i, c.Name, c.Attributes)
				}
				if want.hit == 0 && has(c, "gen_ai.usage.cache_read.input_tokens") {
					t.Errorf("a zero cache counter must be omitted: %v", c.Attributes)
				}
			}

			if len(st.tools) != 1 {
				t.Fatalf("tool spans = %v", names(st.all))
			}
			tl := st.tools[0]
			if tl.Name != "execute_tool get_weather" || tl.SpanKind != trace.SpanKindInternal ||
				str(tl, "gen_ai.tool.name") != "get_weather" || str(tl, "gen_ai.tool.call.id") != toolUseID ||
				!strings.HasPrefix(toolUseID, agent.ToolUseIDPrefix) || str(tl, "gen_ai.tool.type") != "function" ||
				str(tl, "dialogram.tool.outcome") != "success" || has(tl, "dialogram.tool.error_code") ||
				num(tl, "dialogram.tool.result_bytes") != int64(len(toolOutput)) || !has(tl, "dialogram.tool.truncated") ||
				tl.Status.Code != codes.Unset {
				t.Errorf("tool span = %s %v %v", tl.Name, tl.Attributes, tl.Status)
			}
			// The tool ran between the two requests.
			if tl.StartTime.Before(st.chats[0].EndTime) || tl.EndTime.After(st.chats[1].StartTime) {
				t.Error("the tool span must sit between the two chat spans")
			}
		})
	}
}

// Every tool outcome the loop distinguishes, through one tool step.
func TestTrace_ToolOutcomes(t *testing.T) {
	type tc struct {
		name      string
		fn        func(context.Context, json.RawMessage) (any, error)
		timeout   time.Duration
		maxObs    int
		deny      bool
		outcome   string
		code      string
		errStatus bool
		truncated bool
	}
	cases := []tc{
		{name: "soft failure", outcome: "soft_failure", code: tool.ErrorCodeNotFound,
			fn: func(context.Context, json.RawMessage) (any, error) {
				return tool.Soft(tool.ErrorCodeNotFound, "no weather station"), nil
			}},
		{name: "timeout", outcome: "timeout", code: tool.ErrorCodeToolTimeout, errStatus: true, timeout: 20 * time.Millisecond,
			fn: func(ctx context.Context, _ json.RawMessage) (any, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}},
		{name: "panic", outcome: "panic", code: tool.ErrorCodeToolPanic, errStatus: true,
			fn: func(context.Context, json.RawMessage) (any, error) { panic("boom") }},
		{name: "hard failure", outcome: "hard_failure", code: tool.ErrorCodeHardFailure, errStatus: true,
			fn: func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("station db down") }},
		{name: "denied", outcome: "denied", code: tool.ErrorCodeDenied, deny: true},
		{name: "truncated", outcome: "success", maxObs: 128, truncated: true,
			fn: func(context.Context, json.RawMessage) (any, error) {
				return map[string]any{"report": strings.Repeat("sunny ", 100)}, nil
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tp, exp := newTracing(t)
			ran := false
			reg := tool.NewRegistry()
			reg.Register(&tool.Tool{
				Name: "get_weather", Description: "Current weather for a city.", Timeout: c.timeout,
				Schema: tool.Obj(map[string]any{"city": map[string]any{"type": "string"}}, "city"),
				Fn: func(ctx context.Context, in json.RawMessage) (any, error) {
					ran = true
					return c.fn(ctx, in)
				},
			})
			opts := Options{APIKey: "k", Model: "gpt-4o-mini", Registry: reg, Config: replay.Config(),
				Executor: tool.NewLocalExecutor(reg, tool.LocalExecutorOptions{MaxObservationBytes: c.maxObs})}
			if c.deny {
				opts.Interceptors = []agent.Interceptor{denyAll{}}
			}
			srv := replay.New(t, replay.File("testdata/tool_call.json"), replay.File("testdata/soft_failure_final.json"))
			opts.BaseURL = srv.URL
			out, err := agent.New(New(opts), agent.Options{TracerProvider: tp}).Chat(context.Background(), replayInput(askWeather))
			if err != nil {
				t.Fatal(err)
			}
			if ran == c.deny {
				t.Fatalf("tool ran = %v with deny = %v", ran, c.deny)
			}
			st := collect(t, exp)
			if len(st.tools) != 1 || len(st.chats) != 2 {
				t.Fatalf("spans = %v", names(st.all))
			}
			tl := st.tools[0]
			if str(tl, "dialogram.tool.outcome") != c.outcome || str(tl, "dialogram.tool.error_code") != c.code ||
				num(tl, "dialogram.tool.result_bytes") != int64(len(out.ToolCalls[0].Output)) {
				t.Errorf("tool span attrs = %v", tl.Attributes)
			}
			if v, _ := value(tl.Attributes, "dialogram.tool.truncated"); v.AsBool() != c.truncated {
				t.Errorf("truncated = %v, want %v", v.AsBool(), c.truncated)
			}
			if c.errStatus {
				if tl.Status.Code != codes.Error || str(tl, "error.type") != c.code || tl.Status.Description == "" {
					t.Errorf("status = %v error.type = %q", tl.Status, str(tl, "error.type"))
				}
			} else if tl.Status.Code != codes.Unset || has(tl, "error.type") {
				t.Errorf("a %s is a result, not a span error: %v %v", c.outcome, tl.Status, tl.Attributes)
			}
			if c.name == "hard failure" && tl.Status.Description != "station db down" {
				t.Errorf("hard failure description = %q", tl.Status.Description)
			}
			// The model recovered; the run itself succeeded.
			if st.run.Status.Code != codes.Unset || eventNames(st.run) != "done" {
				t.Errorf("run status = %v events = %s", st.run.Status, eventNames(st.run))
			}
		})
	}
}

type denyAll struct{}

func (denyAll) InspectToolUse(context.Context, agent.Block) agent.InterceptDecision {
	return agent.InterceptDecision{Action: agent.InterceptDeny, Reason: "not allowed in tests"}
}

// A request that fails hard fails its chat span (HTTP status as error.type)
// and the run span, and the run records no done event.
func TestTrace_HardFailureRun(t *testing.T) {
	t.Run("chat", func(t *testing.T) {
		tp, exp := newTracing(t)
		srv := replay.New(t, replay.Status(429, "testdata/rate_limited_429.json"))
		ag := agent.New(replayProvider(t, srv, replay.Registry(t, nil)), agent.Options{TracerProvider: tp})
		if _, err := ag.Chat(context.Background(), replayInput("hi")); err == nil {
			t.Fatal("want an error")
		}
		st := collect(t, exp)
		if len(st.chats) != 1 {
			t.Fatalf("spans = %v", names(st.all))
		}
		c := st.chats[0]
		if c.Status.Code != codes.Error || !strings.Contains(c.Status.Description, "openai 429") ||
			str(c, "error.type") != "429" || num(c, "http.response.status_code") != 429 ||
			has(c, "gen_ai.usage.input_tokens") || num(c, "dialogram.attempts") != 1 {
			t.Errorf("chat span = %v %v", c.Status, c.Attributes)
		}
		r := st.run
		if r.Status.Code != codes.Error || !strings.Contains(r.Status.Description, "openai 429") ||
			str(r, "error.type") != "_OTHER" || eventNames(r) != "" {
			t.Errorf("run span = %v %v events %s", r.Status, r.Attributes, eventNames(r))
		}
	})
	t.Run("stream retries exhausted", func(t *testing.T) {
		replay.ShortBackoff(t)
		tp, exp := newTracing(t)
		f := replay.Status(429, "testdata/rate_limited_429.json")
		srv := replay.New(t, f, f, f)
		ag := agent.New(replayProvider(t, srv, replay.Registry(t, nil)), agent.Options{TracerProvider: tp})
		if err := ag.ChatStream(context.Background(), replayInput("hi"), func(agent.StreamEvent) {}); err == nil {
			t.Fatal("want an error")
		}
		st := collect(t, exp)
		if len(st.chats) != 1 {
			t.Fatalf("one span covers every attempt; spans = %v", names(st.all))
		}
		c := st.chats[0]
		if eventNames(c) != "retry,retry" || num(c, "dialogram.attempts") != 3 ||
			c.Status.Code != codes.Error || str(c, "error.type") != "429" {
			t.Errorf("chat span = %v %v events %s", c.Status, c.Attributes, eventNames(c))
		}
		if st.run.Status.Code != codes.Error {
			t.Errorf("run status = %v", st.run.Status)
		}
	})
}

// A 429 before any text is retried: the retry is an event on the request's
// span, which then completes normally.
func TestTrace_RetryEvent(t *testing.T) {
	replay.ShortBackoff(t)
	tp, exp := newTracing(t)
	srv := replay.New(t, replay.Status(429, "testdata/rate_limited_429.json"), replay.File("testdata/text_only.sse"))
	ag := agent.New(replayProvider(t, srv, replay.Registry(t, nil)), agent.Options{TracerProvider: tp})
	var ev replay.Events
	if err := ag.ChatStream(context.Background(), replayInput("hi"), ev.Emit); err != nil || !ev.Done {
		t.Fatalf("err = %v done = %v", err, ev.Done)
	}
	st := collect(t, exp)
	if len(st.chats) != 1 {
		t.Fatalf("spans = %v", names(st.all))
	}
	c := st.chats[0]
	retry := event(t, c, "retry")
	a, _ := value(retry.Attributes, "dialogram.attempt")
	et, _ := value(retry.Attributes, "error.type")
	hs, _ := value(retry.Attributes, "http.response.status_code")
	if eventNames(c) != "retry" || a.AsInt64() != 1 || et.AsString() != "429" || hs.AsInt64() != 429 {
		t.Errorf("retry event = %v", retry.Attributes)
	}
	if c.Status.Code != codes.Unset || num(c, "dialogram.attempts") != 2 || has(c, "error.type") ||
		num(c, "gen_ai.usage.input_tokens") != 25 || num(c, "gen_ai.usage.cache_read.input_tokens") != 1024 {
		t.Errorf("chat span = %v %v", c.Status, c.Attributes)
	}
	if eventNames(st.run) != "done" || st.run.Status.Code != codes.Unset {
		t.Errorf("run = %v %s", st.run.Status, eventNames(st.run))
	}
}

// A pause ends the run with a waiting_for_user event and no done.
func TestTrace_Pause(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "stream"}[stream], func(t *testing.T) {
			tp, exp := newTracing(t)
			srv := replay.New(t, fixture(stream, "pause_request_user_input"))
			ag := agent.New(replayProvider(t, srv, replay.Registry(t, nil)), agent.Options{TracerProvider: tp})
			if _, _, err := run(t, stream, ag, replayInput("book a flight")); err != nil {
				t.Fatal(err)
			}
			st := collect(t, exp)
			if len(st.tools) != 0 || len(st.chats) != 1 || eventNames(st.run) != "waiting_for_user" {
				t.Fatalf("spans = %v run events = %s", names(st.all), eventNames(st.run))
			}
			e := st.run.Events[0]
			k, _ := value(e.Attributes, "dialogram.pause.kind")
			n, _ := value(e.Attributes, "gen_ai.tool.name")
			id, _ := value(e.Attributes, "gen_ai.tool.call.id")
			if k.AsString() != agent.PauseKindHITL || n.AsString() != tool.HITLInputToolName || !strings.HasPrefix(id.AsString(), agent.ToolUseIDPrefix) {
				t.Errorf("waiting_for_user = %v", e.Attributes)
			}
		})
	}
}

// An interceptor asking for approval pauses before anything runs: no tool
// span, a waiting_for_user event of kind approval naming the tool.
func TestTrace_ApprovalPause(t *testing.T) {
	tp, exp := newTracing(t)
	srv := replay.New(t, replay.File("testdata/tool_call.json"))
	p := New(Options{APIKey: "k", Model: "gpt-4o-mini", BaseURL: srv.URL, Registry: replay.Registry(t, nil), Config: replay.Config(),
		Interceptors: []agent.Interceptor{agent.NewApprovalListInterceptor([]string{"get_weather"})}})
	if _, err := agent.New(p, agent.Options{TracerProvider: tp}).Chat(context.Background(), replayInput(askWeather)); err != nil {
		t.Fatal(err)
	}
	st := collect(t, exp)
	if len(st.tools) != 0 || eventNames(st.run) != "waiting_for_user" {
		t.Fatalf("spans = %v run events = %s", names(st.all), eventNames(st.run))
	}
	k, _ := value(st.run.Events[0].Attributes, "dialogram.pause.kind")
	n, _ := value(st.run.Events[0].Attributes, "gen_ai.tool.name")
	if k.AsString() != agent.PauseKindApproval || n.AsString() != "get_weather" {
		t.Errorf("waiting_for_user = %v", st.run.Events[0].Attributes)
	}
}

// Without an SDK the run works and nothing records; with an SDK that
// samples nothing, the run works and nothing is exported.
func TestTrace_Disabled(t *testing.T) {
	t.Run("no SDK installed", func(t *testing.T) {
		if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
			t.Skip("a global SDK provider is installed")
		}
		var recording, valid bool
		reg := tool.NewRegistry()
		reg.Register(&tool.Tool{
			Name: "get_weather", Description: "-", Schema: tool.Obj(map[string]any{"city": map[string]any{"type": "string"}}),
			Fn: func(ctx context.Context, _ json.RawMessage) (any, error) {
				s := trace.SpanFromContext(ctx)
				recording, valid = s.IsRecording(), s.SpanContext().IsValid()
				return map[string]any{"high_c": 27}, nil
			},
		})
		for _, stream := range []bool{false, true} {
			srv := replay.New(t, fixture(stream, "tool_call"), fixture(stream, "tool_call_final"))
			p := New(Options{APIKey: "k", Model: "gpt-4o-mini", BaseURL: srv.URL, Registry: reg, Config: replay.Config(),
				Checkpointer: agent.NewMemCheckpointer()})
			out, ev, err := run(t, stream, agent.New(p, agent.Options{}), replayInput(askWeather))
			if err != nil {
				t.Fatal(err)
			}
			if (stream && ev.Text != replyWeather) || (!stream && out.Reply != replyWeather) {
				t.Fatalf("stream=%v: run did not complete", stream)
			}
			if recording || valid {
				t.Errorf("stream=%v: the tool saw a live span (recording=%v valid=%v)", stream, recording, valid)
			}
		}
	})
	t.Run("never sampled", func(t *testing.T) {
		tp, exp := newTracing(t, sdktrace.WithSampler(sdktrace.NeverSample()))
		srv := replay.New(t, replay.File("testdata/tool_call.sse"), replay.File("testdata/tool_call_final.sse"))
		var ev replay.Events
		ag := agent.New(replayProvider(t, srv, replay.Registry(t, nil)), agent.Options{TracerProvider: tp})
		if err := ag.ChatStream(context.Background(), replayInput(askWeather), ev.Emit); err != nil || !ev.Done {
			t.Fatalf("err = %v done = %v", err, ev.Done)
		}
		if n := len(exp.GetSpans()); n != 0 {
			t.Errorf("exported %d spans, want 0", n)
		}
	})
}

// Without the Agent wrapper the loop's spans attach to whatever span the
// caller's context carries, from that span's TracerProvider — the pattern
// every child span relies on.
func TestTrace_ChildSpansFollowContext(t *testing.T) {
	tp, exp := newTracing(t)
	ctx, parent := tp.Tracer("app").Start(context.Background(), "handle request")
	srv := replay.New(t, replay.File("testdata/tool_call.json"), replay.File("testdata/tool_call_final.json"))
	if _, err := replayProvider(t, srv, replay.Registry(t, nil)).Chat(ctx, replayInput(askWeather)); err != nil {
		t.Fatal(err)
	}
	parent.End()
	var kids []string
	for _, s := range exp.GetSpans() {
		if s.Name == "handle request" {
			continue
		}
		if s.Parent.SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("span %q parent = %v", s.Name, s.Parent.SpanID())
		}
		kids = append(kids, s.Name)
	}
	if strings.Join(kids, ",") != "chat gpt-4o-mini,execute_tool get_weather,chat gpt-4o-mini" {
		t.Errorf("children = %v", kids)
	}
}
