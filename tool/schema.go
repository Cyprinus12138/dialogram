package tool

// Error codes shared by the built-in tools and the agent loop. Tools are
// encouraged to reuse them so one model-facing vocabulary covers every
// failure the loop can produce.
const (
	ErrorCodeUpstreamTimeout = "UPSTREAM_TIMEOUT"
	ErrorCodeNotFound        = "NOT_FOUND"
	ErrorCodeInvalidInput    = "INVALID_INPUT"
	ErrorCodeDenied          = "DENIED"
	ErrorCodeHardFailure     = "HARD_FAILURE"
	// ErrorCodeToolTimeout is stamped by LocalExecutor when a tool exceeds
	// its per-call deadline (LocalExecutorOptions.Timeout / Tool.Timeout).
	ErrorCodeToolTimeout = "TOOL_TIMEOUT"
	// ErrorCodeToolPanic is stamped by LocalExecutor when a tool panics; the
	// stack is logged server-side and the model sees a soft failure.
	ErrorCodeToolPanic = "TOOL_PANIC"
	// ErrorCodeRunInterrupted is stamped by agent.CloseDangling on a
	// tool_use whose run died before its result was recorded.
	ErrorCodeRunInterrupted = "RUN_INTERRUPTED"
)

// Source is the provenance block a data tool attaches to its result so the
// model can cite where a figure came from.
type Source struct {
	Dataset     string `json:"dataset"`
	Publisher   string `json:"publisher,omitempty"`
	URL         string `json:"url,omitempty"`
	RetrievedAt string `json:"retrieved_at,omitempty"`
	Coverage    string `json:"coverage,omitempty"`
}

// SoftFailure is the structured "the tool ran but could not answer" shape.
// Returning it (rather than a Go error) keeps the model on its
// degraded-path instructions instead of surfacing a hard failure.
type SoftFailure struct {
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
	Retryable    bool   `json:"retryable"`
}

// Soft builds a bare soft-failure body. Retryable is set for
// ErrorCodeUpstreamTimeout only.
func Soft(code, msg string) map[string]any {
	return map[string]any{
		"error_code":    code,
		"error_message": msg,
		"retryable":     code == ErrorCodeUpstreamTimeout,
	}
}

// SoftEnvelope is Soft wrapped in an Envelope so envelope-returning tools
// can fail softly through model_observation.
func SoftEnvelope(code, msg string) *Envelope {
	return &Envelope{ModelObservation: map[string]any{
		"error_code":    code,
		"error_message": msg,
	}}
}

// ---------- JSON Schema helpers ----------

// Obj builds an object schema from a property map and the required keys.
func Obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	if props == nil {
		props = map[string]any{}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

// Str is a string property with a description.
func Str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

// Num is a number property with a description.
func Num(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }

// Int is an integer property with a description.
func Int(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

// Bool is a boolean property with a description.
func Bool(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// Enum is a string property constrained to the given values.
func Enum(desc string, vals ...string) map[string]any {
	s := make([]any, 0, len(vals))
	for _, v := range vals {
		s = append(s, v)
	}
	return map[string]any{"type": "string", "description": desc, "enum": s}
}

// Arr is an array property whose items match the given schema.
func Arr(items map[string]any, desc string) map[string]any {
	return map[string]any{"type": "array", "description": desc, "items": items}
}
