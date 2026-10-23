package sse

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/streambuf"
)

// traced serves h behind a middleware that opens a server span per request,
// the way otelhttp would.
func traced(t *testing.T, tp trace.TracerProvider, produce Producer, opts Options) *httptest.Server {
	t.Helper()
	store := streambuf.NewMemStore(time.Minute)
	t.Cleanup(func() { store.Close() })
	opts.KeepAlive = 20 * time.Millisecond
	h := NewHandler(store, produce, opts)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tp.Tracer("server").Start(r.Context(), r.Method, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		h.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func spanNamed(t *testing.T, exp *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	t.Helper()
	for _, s := range exp.GetSpans() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %q span", name)
	return tracetest.SpanStub{}
}

func messageID(s tracetest.SpanStub) string {
	for _, kv := range s.Attributes {
		if kv.Key == "dialogram.message_id" {
			return kv.Value.AsString()
		}
	}
	return ""
}

// The detached producer runs under the launching request's span context,
// so the run is a child of that request in one trace; a request that
// tails the running producer is linked to the launch; both request spans
// carry the message id.
func TestTrace_ProducerJoinsRequestTrace(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	started, release := make(chan struct{}), make(chan struct{})
	srv := traced(t, tp, func(ctx context.Context, _ string, _ []byte, emit agent.EmitFn) error {
		_, span := tp.Tracer("agent").Start(ctx, "run")
		defer span.End()
		emit(agent.StreamEvent{Type: agent.EventText, Delta: "hi"})
		close(started)
		<-release
		emit(agent.StreamEvent{Type: agent.EventDone})
		return nil
	}, Options{})

	type result struct {
		frames []frame
		err    error
	}
	launched := make(chan result, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/chat/stream?message_id=m1", "application/json", strings.NewReader(`{"message":"hi"}`))
		if err != nil {
			launched <- result{err: err}
			close(started)
			return
		}
		defer resp.Body.Close()
		launched <- result{frames: readFrames(t, bufio.NewReader(resp.Body))}
	}()
	<-started

	// Tail while the producer is still running; release it once the tail
	// has its first frame, so the link was made while it was in flight.
	resp, err := http.Get(srv.URL + "/chat/stream?message_id=m1")
	if err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(resp.Body)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("tail ended early: %v", err)
		}
		if strings.HasPrefix(line, "event: text") {
			break
		}
	}
	close(release)
	tailed := readFrames(t, rd)
	_ = resp.Body.Close()
	if res := <-launched; res.err != nil || types(res.frames) != "meta,text,done" || len(tailed) == 0 || tailed[len(tailed)-1].Event != "done" {
		t.Fatalf("launch err = %v frames = %s, tail frames = %s", res.err, types(res.frames), types(tailed))
	}

	// The server spans end after their handler returns.
	deadline := time.Now().Add(2 * time.Second)
	for len(exp.GetSpans()) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	launch, tail, run := spanNamed(t, exp, http.MethodPost), spanNamed(t, exp, http.MethodGet), spanNamed(t, exp, "run")
	if run.Parent.SpanID() != launch.SpanContext.SpanID() || run.SpanContext.TraceID() != launch.SpanContext.TraceID() {
		t.Errorf("run parent = %v, want the launching request %v", run.Parent.SpanID(), launch.SpanContext.SpanID())
	}
	if run.Parent.IsRemote() {
		t.Error("the request span lives in this process; it must be a local parent")
	}
	if len(tail.Links) != 1 || tail.Links[0].SpanContext.SpanID() != launch.SpanContext.SpanID() {
		t.Errorf("tail links = %+v", tail.Links)
	}
	if tail.SpanContext.TraceID() == launch.SpanContext.TraceID() {
		t.Error("the tail request is its own trace, linked rather than parented")
	}
	if messageID(launch) != "m1" || messageID(tail) != "m1" {
		t.Errorf("message ids = %q %q", messageID(launch), messageID(tail))
	}
}

// A span context the application's ProducerContext already set wins over
// the request's.
func TestTrace_ProducerContextSpanWins(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	own := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled,
	})
	srv := traced(t, tp, func(ctx context.Context, _ string, _ []byte, emit agent.EmitFn) error {
		_, span := tp.Tracer("agent").Start(ctx, "run")
		span.End()
		emit(agent.StreamEvent{Type: agent.EventDone})
		return nil
	}, Options{ProducerContext: func(*http.Request) context.Context {
		return trace.ContextWithSpanContext(context.Background(), own)
	}})
	if _, fs := post(t, srv.URL+"/chat/stream?message_id=m2", `{"message":"hi"}`); types(fs) != "meta,done" {
		t.Fatalf("frames = %s", types(fs))
	}
	if run := spanNamed(t, exp, "run"); run.Parent.SpanID() != own.SpanID() || run.SpanContext.TraceID() != own.TraceID() {
		t.Errorf("run parent = %v in trace %v", run.Parent.SpanID(), run.SpanContext.TraceID())
	}
}
