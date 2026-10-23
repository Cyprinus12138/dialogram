package tool

import (
	"context"
	"encoding/json"
	"time"
)

// NowToolName is the clock tool.
const NowToolName = "now"

// RegisterNow adds a `now` tool that returns the current date and time in
// loc. A nil loc means UTC. Models should call this instead of guessing
// "today".
func RegisterNow(r *Registry, loc *time.Location) {
	r.Register(Now(loc))
}

// Now returns the clock tool definition for loc (nil = UTC).
func Now(loc *time.Location) *Tool {
	if loc == nil {
		loc = time.UTC
	}
	return &Tool{
		Name:        NowToolName,
		Label:       "Current time",
		Description: "Return the current date and time in the deployment's time zone (" + loc.String() + "). Use this whenever you need 'today' — do not guess dates.",
		Schema:      Obj(nil),
		Fn: func(ctx context.Context, input json.RawMessage) (any, error) {
			n := time.Now().In(loc)
			return map[string]any{
				"iso8601":  n.Format("2006-01-02T15:04:05-07:00"),
				"date":     n.Format("2006-01-02"),
				"weekday":  n.Weekday().String(),
				"timezone": loc.String(),
			}, nil
		},
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
	}
}
