package agent

import (
	"context"
	"encoding/json"
	"fmt"

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

// Policy is the annotation-driven rule set PolicyInterceptor enforces. Every
// field is opt-in: the zero Policy allows everything, so the interceptor can
// be wired unconditionally and tightened per deployment.
type Policy struct {
	// PauseDestructive pauses for approval on any tool whose Annotations
	// mark it Destructive.
	PauseDestructive bool
	// PauseOpenWorld pauses for approval on any tool whose Annotations
	// mark it OpenWorld.
	PauseOpenWorld bool
	// DenyUnknown denies a tool_use that names a tool the registry does not
	// hold. Without it the miss surfaces later as a HARD_FAILURE from
	// Registry.Call; with it the rejection is an explicit DENIED decision
	// taken before anything is dispatched.
	DenyUnknown bool
	// DenyDestructive denies any tool annotated Destructive outright. It is
	// stronger than PauseDestructive and takes precedence over it: a
	// denial cannot be approved away.
	DenyDestructive bool
}

// Reason formats for PolicyInterceptor decisions. They end up in
// waiting_for_user events and in denied tool_result bodies, so they are
// part of the observable contract and must stay stable; the %s is always
// the tool name.
const (
	policyReasonUnknownTool     = "%s is not a registered tool and cannot be run."
	policyReasonDenyDestructive = "%s is marked destructive and this deployment does not allow destructive tools."
	policyReasonPauseDestruct   = "%s may make irreversible changes. Review the inputs and approve or deny."
	policyReasonPauseOpenWorld  = "%s reaches outside the application (open world). Review the inputs and approve or deny."
)

// PolicyInterceptor applies a Policy to every tool_use by looking the tool
// up in the registry and reading its tool.Annotations. Decisions, in order
// of precedence:
//
//  1. unknown tool          → Deny  (Policy.DenyUnknown), else Allow
//  2. Destructive           → Deny  (Policy.DenyDestructive)
//  3. Destructive           → Pause (Policy.PauseDestructive)
//  4. OpenWorld             → Pause (Policy.PauseOpenWorld)
//  5. otherwise             → Allow
//
// Annotations are evaluated as declared; a tool that is ReadOnly and
// Destructive at once is a tool bug, and the policy errs on the side of
// treating it as destructive.
//
// Combining with ApprovalListInterceptor: put the PolicyInterceptor FIRST.
// RunInterceptors stops at the first non-Allow decision, and an approval
// pause the user accepts is dispatched by the application without
// re-running the chain. If the list ran first, a tool the policy would deny
// could instead be paused by the list and then approved straight past the
// denial. With the policy first a denial is final, a policy pause still
// pauses, and anything the policy allows falls through to the list:
//
//	Interceptors: []agent.Interceptor{
//		agent.NewPolicyInterceptor(reg, agent.Policy{DenyUnknown: true, PauseDestructive: true}),
//		agent.NewApprovalListInterceptor([]string{"book_flight"}),
//	}
//
// Pause tools (the built-in HITL tools and Config.PauseTools) are split out
// of the assistant turn before the interceptor chain runs, so the policy
// never sees them; in particular DenyUnknown does not deny an application's
// unregistered custom pause tool.
type PolicyInterceptor struct {
	reg    *tool.Registry
	policy Policy
}

// NewPolicyInterceptor returns an interceptor enforcing p against the tools
// registered in reg. A nil reg makes every tool unknown.
func NewPolicyInterceptor(reg *tool.Registry, p Policy) *PolicyInterceptor {
	return &PolicyInterceptor{reg: reg, policy: p}
}

// InspectToolUse evaluates b against the policy. See PolicyInterceptor for
// the precedence order.
func (p *PolicyInterceptor) InspectToolUse(_ context.Context, b Block) InterceptDecision {
	var t *tool.Tool
	if p.reg != nil {
		t, _ = p.reg.Lookup(b.ToolName)
	}
	if t == nil {
		if p.policy.DenyUnknown {
			return InterceptDecision{Action: InterceptDeny, Reason: fmt.Sprintf(policyReasonUnknownTool, b.ToolName)}
		}
		return InterceptDecision{Action: InterceptAllow}
	}
	a := t.Annotations
	switch {
	case a.Destructive && p.policy.DenyDestructive:
		return InterceptDecision{Action: InterceptDeny, Reason: fmt.Sprintf(policyReasonDenyDestructive, b.ToolName)}
	case a.Destructive && p.policy.PauseDestructive:
		return InterceptDecision{Action: InterceptPause, Reason: fmt.Sprintf(policyReasonPauseDestruct, b.ToolName)}
	case a.OpenWorld && p.policy.PauseOpenWorld:
		return InterceptDecision{Action: InterceptPause, Reason: fmt.Sprintf(policyReasonPauseOpenWorld, b.ToolName)}
	}
	return InterceptDecision{Action: InterceptAllow}
}
