package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func tracing(t *testing.T) (*sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp, exp
}

func attr(kvs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, kv := range kvs {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func onlySpan(t *testing.T, exp *tracetest.InMemoryExporter) tracetest.SpanStub {
	t.Helper()
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	return spans[0]
}

// A redaction by the guard is a guard.redacted event on the run span, on
// both paths, before done.
func TestRunSpan_GuardRedacted(t *testing.T) {
	const reply = "About $1,250."
	p := fakeProvider{
		out:    &ChatOutput{Reply: reply, Turns: []Turn{{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: reply}}}}},
		events: []StreamEvent{{Type: EventText, Delta: reply}, textTurn(reply), {Type: EventDone}},
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			tp, exp := tracing(t)
			a := New(p, Options{Guard: testGuard(), TracerProvider: tp})
			in := ChatInput{Profile: "assistant", MessageID: "m1"}
			var err error
			if stream {
				err = a.ChatStream(context.Background(), in, func(StreamEvent) {})
			} else {
				_, err = a.Chat(context.Background(), in)
			}
			if err != nil {
				t.Fatal(err)
			}
			s := onlySpan(t, exp)
			if s.Name != "invoke_agent assistant" || len(s.Events) != 2 ||
				s.Events[0].Name != "guard.redacted" || s.Events[1].Name != "done" {
				t.Fatalf("span = %s events = %+v", s.Name, s.Events)
			}
			kind, _ := attr(s.Events[0].Attributes, "dialogram.guard.kind")
			n, _ := attr(s.Events[0].Attributes, "dialogram.guard.redactions")
			if kind.AsString() != "money" || n.AsInt64() != 1 {
				t.Errorf("guard.redacted = %v", s.Events[0].Attributes)
			}
			if v, _ := attr(s.Attributes, "dialogram.message_id"); v.AsString() != "m1" {
				t.Errorf("attrs = %v", s.Attributes)
			}
		})
	}
}

type errProvider struct{ err error }

func (e errProvider) Name() string  { return "fake" }
func (e errProvider) Model() string { return "fake-1" }
func (e errProvider) Chat(context.Context, ChatInput) (*ChatOutput, error) {
	return nil, e.err
}
func (e errProvider) ChatStream(context.Context, ChatInput, EmitFn) error { return e.err }

// A failed run sets Error status with the message and a low-cardinality
// error.type; an unnamed profile names the span plain invoke_agent.
func TestRunSpan_Failure(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{errors.New("openai 500: upstream exploded"), "_OTHER"},
		{fmt.Errorf("openai stream request: %w", context.DeadlineExceeded), "timeout"},
		{context.Canceled, "canceled"},
	} {
		tp, exp := tracing(t)
		a := New(errProvider{c.err}, Options{TracerProvider: tp})
		if err := a.ChatStream(context.Background(), ChatInput{}, func(StreamEvent) {}); !errors.Is(err, c.err) {
			t.Fatalf("err = %v", err)
		}
		s := onlySpan(t, exp)
		et, _ := attr(s.Attributes, "error.type")
		prof, _ := attr(s.Attributes, "dialogram.profile")
		_, named := attr(s.Attributes, "gen_ai.agent.name")
		_, hasID := attr(s.Attributes, "dialogram.message_id")
		if s.Name != "invoke_agent" || s.Status.Code != codes.Error || s.Status.Description != c.err.Error() ||
			et.AsString() != c.want || prof.AsString() != "default" || named || hasID || len(s.Events) != 0 {
			t.Errorf("span = %s status %v attrs %v events %v", s.Name, s.Status, s.Attributes, s.Events)
		}
	}
}
