package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Cyprinus12138/dialogram/tool"
)

func TestInspectResult(t *testing.T) {
	pad := strings.Repeat("x", 400)
	big := tool.CapObservation(`{"rows":"`+pad+`"}`, 128)
	env := tool.CapObservation(`{"model_observation":{"rows":"`+pad+`"},"ui_payload":{"component":"table"}}`, 128)
	if !strings.Contains(env, `"ui_payload"`) {
		t.Fatalf("not capped as an envelope: %s", env)
	}
	for _, c := range []struct {
		name, out, code string
		truncated       bool
	}{
		{"bare success", `{"high_c":27}`, "", false},
		{"bare soft failure", `{"error_code":"NOT_FOUND","error_message":"x"}`, "NOT_FOUND", false},
		{"envelope soft failure", `{"model_observation":{"error_code":"UPSTREAM_TIMEOUT"}}`, "UPSTREAM_TIMEOUT", false},
		{"soft_failure flag", `{"soft_failure":true}`, "SOFT_FAILURE", false},
		{"soft_failure false", `{"soft_failure":false,"n":1}`, "", false},
		{"truncated bare", big, "", true},
		{"truncated envelope", env, "", true},
		{"not JSON", `sunny`, "", false},
		{"JSON array", `[1,2]`, "", false},
	} {
		code, truncated := inspectResult(c.out)
		if code != c.code || truncated != c.truncated {
			t.Errorf("%s: code = %q truncated = %v, want %q %v", c.name, code, truncated, c.code, c.truncated)
		}
	}
}

func TestClassifyError(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), ErrTimeout},
		{context.Canceled, ErrCanceled},
		{errors.New("openai decode: bad json"), ErrOther},
	} {
		if got := ClassifyError(c.err); got != c.want {
			t.Errorf("ClassifyError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
