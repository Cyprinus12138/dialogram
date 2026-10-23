package agent

import (
	"context"
	"encoding/json"

	"github.com/Cyprinus12138/dialogram/tool"
)

// InterceptAction is the outcome an Interceptor returns for one tool_use.
type InterceptAction string

const (
	// InterceptAllow lets the tool dispatch proceed normally. Returned by
	// default when no interceptor cares about the tool.
	InterceptAllow InterceptAction = "allow"
	// InterceptDeny blocks the dispatch and feeds a synthetic tool_result
	// carrying Reason back to the model. The model sees the rejection the
	// same way it sees any soft tool failure and can recover or surface it.
	InterceptDeny InterceptAction = "deny"
	// InterceptPause suspends the turn with a waiting_for_user event of
	// PauseKindApproval. The application resumes it with the user's
	// decision — on approve it dispatches the stashed tool, on deny it
	// appends a DENIED tool_result.
	InterceptPause InterceptAction = "pause"
)

// InterceptDecision is the outcome of evaluating one tool_use against the
// interceptor chain. Reason is the human-readable explanation used by Deny
// (stuffed into the tool_result error body) and Pause (shown to the user as
// the pause prompt).
type InterceptDecision struct {
	Action InterceptAction
	Reason string
}

// Interceptor inspects a tool_use block before dispatch. Implementations
// should be side-effect-free except for logging — the decision they return
// is the only thing that shapes the loop's behaviour.
//
// Interceptors run in registration order; the first non-Allow decision wins
// and later interceptors do not run for that block. An empty chain always
// allows.
type Interceptor interface {
	InspectToolUse(ctx context.Context, b Block) InterceptDecision
}

// RunInterceptors evaluates chain on one tool_use, short-circuiting at the
// first non-Allow decision.
func RunInterceptors(ctx context.Context, chain []Interceptor, b Block) InterceptDecision {
	for _, i := range chain {
		d := i.InspectToolUse(ctx, b)
		if d.Action != InterceptAllow {
			return d
		}
	}
	return InterceptDecision{Action: InterceptAllow}
}

// DeniedResultJSON is the tool_result body the loop feeds back to the model
// when an interceptor (or the user, on an approval pause) denies a tool.
// The shape mirrors the soft-failure convention so the model's handling of
// error_code applies to denials without extra teaching.
func DeniedResultJSON(reason string) string {
	if reason == "" {
		reason = "An interceptor blocked this tool call."
	}
	b, _ := json.Marshal(map[string]any{
		"error_code":    tool.ErrorCodeDenied,
		"error_message": reason,
		"user_rejected": true,
	})
	return string(b)
}

// ApprovalListInterceptor pauses on any tool_use whose name is in names —
// the minimal "ask before running" policy. The intercepted tool's input is
// carried on the waiting_for_user event so the application can dispatch it
// verbatim on approve.
type ApprovalListInterceptor struct {
	names map[string]bool
}

// NewApprovalListInterceptor returns an interceptor pausing on any of
// names. Empty input returns an always-Allow interceptor so callers can wire
// it unconditionally.
func NewApprovalListInterceptor(names []string) *ApprovalListInterceptor {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		if n != "" {
			m[n] = true
		}
	}
	return &ApprovalListInterceptor{names: m}
}

// InspectToolUse returns InterceptPause when b's tool name is in the
// configured set, InterceptAllow otherwise.
func (a *ApprovalListInterceptor) InspectToolUse(_ context.Context, b Block) InterceptDecision {
	if len(a.names) == 0 || !a.names[b.ToolName] {
		return InterceptDecision{Action: InterceptAllow}
	}
	return InterceptDecision{
		Action: InterceptPause,
		Reason: b.ToolName + " requires your approval before running. Review the inputs and approve or deny.",
	}
}
