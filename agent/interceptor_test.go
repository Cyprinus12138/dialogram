package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Cyprinus12138/dialogram/tool"
)

func annotatedTool(name string, a tool.Annotations) *tool.Tool {
	return &tool.Tool{
		Name:        name,
		Description: "fixture",
		Schema:      tool.Obj(nil),
		Fn:          func(context.Context, json.RawMessage) (any, error) { return map[string]any{"ok": true}, nil },
		Annotations: a,
	}
}

func policyRegistry() *tool.Registry {
	r := tool.NewRegistry()
	r.Register(annotatedTool("read_thing", tool.Annotations{ReadOnly: true, Idempotent: true}))
	r.Register(annotatedTool("delete_thing", tool.Annotations{Destructive: true}))
	r.Register(annotatedTool("fetch_url", tool.Annotations{OpenWorld: true}))
	r.Register(annotatedTool("post_webhook", tool.Annotations{Destructive: true, OpenWorld: true}))
	r.Register(annotatedTool("plain", tool.Annotations{}))
	return r
}

func TestPolicyInterceptor_DecisionTable(t *testing.T) {
	everything := Policy{PauseDestructive: true, PauseOpenWorld: true, DenyUnknown: true, DenyDestructive: true}
	cases := []struct {
		name   string
		policy Policy
		tool   string
		want   InterceptAction
		reason string
	}{
		{"zero policy allows destructive", Policy{}, "delete_thing", InterceptAllow, ""},
		{"zero policy allows open-world", Policy{}, "fetch_url", InterceptAllow, ""},
		{"zero policy allows unknown", Policy{}, "nope", InterceptAllow, ""},
		{"destructive pauses", Policy{PauseDestructive: true}, "delete_thing", InterceptPause,
			"delete_thing may make irreversible changes. Review the inputs and approve or deny."},
		{"open-world pauses", Policy{PauseOpenWorld: true}, "fetch_url", InterceptPause,
			"fetch_url reaches outside the application (open world). Review the inputs and approve or deny."},
		{"unknown denied", Policy{DenyUnknown: true}, "nope", InterceptDeny,
			"nope is not a registered tool and cannot be run."},
		{"deny destructive", Policy{DenyDestructive: true}, "delete_thing", InterceptDeny,
			"delete_thing is marked destructive and this deployment does not allow destructive tools."},
		{"deny beats pause", Policy{DenyDestructive: true, PauseDestructive: true}, "delete_thing", InterceptDeny,
			"delete_thing is marked destructive and this deployment does not allow destructive tools."},
		{"destructive and open-world: deny wins", Policy{DenyDestructive: true, PauseOpenWorld: true}, "post_webhook", InterceptDeny,
			"post_webhook is marked destructive and this deployment does not allow destructive tools."},
		{"destructive and open-world: destructive pause reason wins", Policy{PauseDestructive: true, PauseOpenWorld: true}, "post_webhook", InterceptPause,
			"post_webhook may make irreversible changes. Review the inputs and approve or deny."},
		{"open-world pause ignores destructive-only", Policy{PauseOpenWorld: true}, "delete_thing", InterceptAllow, ""},
		{"destructive pause ignores open-world-only", Policy{PauseDestructive: true}, "fetch_url", InterceptAllow, ""},
		{"read-only passes the strictest policy", everything, "read_thing", InterceptAllow, ""},
		{"unannotated passes the strictest policy", everything, "plain", InterceptAllow, ""},
	}
	reg := policyRegistry()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ic := NewPolicyInterceptor(reg, tc.policy)
			d := ic.InspectToolUse(context.Background(), Block{Type: BlockToolUse, ToolName: tc.tool})
			if d.Action != tc.want {
				t.Fatalf("action = %q, want %q (reason %q)", d.Action, tc.want, d.Reason)
			}
			if d.Reason != tc.reason {
				t.Fatalf("reason = %q, want %q", d.Reason, tc.reason)
			}
		})
	}
}

func TestPolicyInterceptor_NilRegistry(t *testing.T) {
	b := Block{Type: BlockToolUse, ToolName: "anything"}
	if d := NewPolicyInterceptor(nil, Policy{}).InspectToolUse(context.Background(), b); d.Action != InterceptAllow {
		t.Fatalf("nil registry with zero policy must allow, got %+v", d)
	}
	if d := NewPolicyInterceptor(nil, Policy{DenyUnknown: true}).InspectToolUse(context.Background(), b); d.Action != InterceptDeny {
		t.Fatalf("nil registry with DenyUnknown must deny, got %+v", d)
	}
}

// The reason strings are the observable contract: a denial reaches the
// model through DeniedResultJSON.
func TestPolicyInterceptor_DenialReachesTheModel(t *testing.T) {
	d := NewPolicyInterceptor(policyRegistry(), Policy{DenyUnknown: true}).
		InspectToolUse(context.Background(), Block{Type: BlockToolUse, ToolName: "nope"})
	var body map[string]any
	if err := json.Unmarshal([]byte(DeniedResultJSON(d.Reason)), &body); err != nil {
		t.Fatal(err)
	}
	if body["error_code"] != tool.ErrorCodeDenied || body["error_message"] != d.Reason {
		t.Fatalf("denied body wrong: %v", body)
	}
}

// Policy first, list second: a denial is final. The reverse order lets a
// user approval walk past the denial, which is why the doc comment on
// PolicyInterceptor says to put it first.
func TestPolicyInterceptor_ChainOrderWithApprovalList(t *testing.T) {
	reg := policyRegistry()
	policy := NewPolicyInterceptor(reg, Policy{DenyDestructive: true})
	list := NewApprovalListInterceptor([]string{"delete_thing", "plain"})
	del := Block{Type: BlockToolUse, ToolName: "delete_thing"}
	plain := Block{Type: BlockToolUse, ToolName: "plain"}

	if d := RunInterceptors(context.Background(), []Interceptor{policy, list}, del); d.Action != InterceptDeny {
		t.Fatalf("policy-first chain must deny a destructive tool, got %+v", d)
	}
	if d := RunInterceptors(context.Background(), []Interceptor{list, policy}, del); d.Action != InterceptPause {
		t.Fatalf("list-first chain masks the denial with a pause, got %+v", d)
	}
	if d := RunInterceptors(context.Background(), []Interceptor{policy, list}, plain); d.Action != InterceptPause || !strings.Contains(d.Reason, "plain") {
		t.Fatalf("a tool the policy allows must fall through to the list, got %+v", d)
	}
}

func TestLoadSkill_Annotations(t *testing.T) {
	reg := tool.NewRegistry()
	lib, err := NewSkillLibrary(&Skill{ID: "example", Name: "Example", Description: "placeholder"})
	if err != nil {
		t.Fatal(err)
	}
	RegisterLoadSkill(reg, lib)
	tl, ok := reg.Lookup(LoadSkillToolName)
	if !ok {
		t.Fatal("load_skill not registered")
	}
	if want := (tool.Annotations{ReadOnly: true, Idempotent: true}); tl.Annotations != want {
		t.Fatalf("load_skill annotations = %+v, want %+v", tl.Annotations, want)
	}
}
