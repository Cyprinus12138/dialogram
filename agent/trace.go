package agent

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/Cyprinus12138/dialogram/internal/telemetry"
)

// InstrumentationName is the OpenTelemetry instrumentation scope every
// dialogram tracer uses: the module path. Filter or route dialogram's
// spans by it.
const InstrumentationName = telemetry.Scope

// startRun opens the run's root span: "invoke_agent {profile}" (plain
// "invoke_agent" when the input names no profile), INTERNAL, carrying the
// provider, model, profile and message id. The loop and the provider
// adapters start their model-call and tool spans from this span's
// TracerProvider, so they need no tracer of their own. See
// docs/OBSERVABILITY.md.
func (a *Agent) startRun(ctx context.Context, in ChatInput) (context.Context, trace.Span) {
	name := telemetry.OpInvokeAgent
	profile := in.Profile
	if profile != "" {
		name += " " + profile
	} else {
		profile = "default"
	}
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			telemetry.OperationName.String(telemetry.OpInvokeAgent),
			telemetry.ProviderName.String(a.provider.Name()),
			telemetry.RequestModel.String(a.provider.Model()),
			telemetry.Profile.String(profile),
		),
	}
	if in.Profile != "" {
		opts = append(opts, trace.WithAttributes(telemetry.AgentName.String(in.Profile)))
	}
	if in.MessageID != "" {
		opts = append(opts, trace.WithAttributes(telemetry.MessageID.String(in.MessageID)))
	}
	return a.tracer.Start(ctx, name, opts...)
}

// endRun ends the run span, failed when err is non-nil.
func endRun(span trace.Span, err error) {
	telemetry.Fail(span, 0, err)
	span.End()
}

// redactedEvent records one guard redaction on the run span.
func redactedEvent(span trace.Span, kind string, n int) {
	if n == 0 || !span.IsRecording() {
		return
	}
	span.AddEvent(telemetry.EventGuardRedacted, trace.WithAttributes(
		telemetry.GuardKind.String(kind), telemetry.GuardRedactions.Int(n)))
}
