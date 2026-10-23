package anthropic

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/provider/internal/replay"
)

// Anthropic reports input_tokens without its cache counters; the GenAI
// conventions' gen_ai.usage.input_tokens includes them, so the chat span
// carries the sum, with each cache counter under its own name.
func TestTrace_UsageIncludesCacheTokens(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "stream"}[stream], func(t *testing.T) {
			exp := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
			f := replay.File("testdata/text_only.json")
			if stream {
				f = replay.File("testdata/text_only.sse")
			}
			srv := replay.New(t, f)
			ag := agent.New(replayProvider(t, srv, replay.Registry(t, nil)), agent.Options{TracerProvider: tp})
			var err error
			if stream {
				err = ag.ChatStream(context.Background(), replayInput("hi"), func(agent.StreamEvent) {})
			} else {
				_, err = ag.Chat(context.Background(), replayInput("hi"))
			}
			if err != nil {
				t.Fatal(err)
			}
			spans := exp.GetSpans()
			if len(spans) != 2 {
				t.Fatalf("spans = %d, want chat + run", len(spans))
			}
			c, r := spans[0], spans[1]
			get := func(kvs []attribute.KeyValue, k string) attribute.Value {
				for _, kv := range kvs {
					if string(kv.Key) == k {
						return kv.Value
					}
				}
				return attribute.Value{}
			}
			if c.Name != "chat claude-sonnet-4-6" || get(c.Attributes, "gen_ai.provider.name").AsString() != "anthropic" ||
				get(c.Attributes, "gen_ai.usage.input_tokens").AsInt64() != 25+2048+1024 ||
				get(c.Attributes, "gen_ai.usage.output_tokens").AsInt64() != 12 ||
				get(c.Attributes, "gen_ai.usage.cache_read.input_tokens").AsInt64() != 2048 ||
				get(c.Attributes, "gen_ai.usage.cache_write.input_tokens").AsInt64() != 1024 ||
				len(get(c.Attributes, "gen_ai.response.finish_reasons").AsStringSlice()) != 1 ||
				get(c.Attributes, "gen_ai.response.finish_reasons").AsStringSlice()[0] != "end_turn" {
				t.Errorf("chat span = %s %v", c.Name, c.Attributes)
			}
			if r.Name != "invoke_agent" || get(r.Attributes, "dialogram.profile").AsString() != "default" ||
				c.Parent.SpanID() != r.SpanContext.SpanID() {
				t.Errorf("run span = %s %v", r.Name, r.Attributes)
			}
		})
	}
}
