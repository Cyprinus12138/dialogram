package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func softFailureOf(t *testing.T, out string) SoftFailure {
	t.Helper()
	var sf SoftFailure
	if err := json.Unmarshal([]byte(out), &sf); err != nil {
		t.Fatalf("result is not a soft failure: %v (%s)", err, out)
	}
	return sf
}

func TestNewLocalExecutor_Defaults(t *testing.T) {
	e := NewLocalExecutor(nil, LocalExecutorOptions{})
	if e.timeout != DefaultToolTimeout || e.maxObs != DefaultMaxObservationBytes || e.MaxConcurrency() != DefaultMaxConcurrency {
		t.Fatalf("defaults wrong: %+v", e)
	}
	// A nil registry is replaced by an empty one: unknown tool is a Go
	// error, exactly as Registry.Call reports it.
	if _, err := e.Execute(context.Background(), Call{Name: "nope"}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("unknown tool err = %v", err)
	}
	e = NewLocalExecutor(nil, LocalExecutorOptions{Timeout: -1, MaxObservationBytes: -1, MaxConcurrency: -1})
	if e.timeout >= 0 || e.maxObs >= 0 || e.MaxConcurrency() >= 0 {
		t.Fatalf("negative options must disable, got %+v", e)
	}
}

// A Go error from the tool passes through untouched so the loop stamps its
// usual HARD_FAILURE body; a successful bare result is byte-identical to
// Registry.Call.
func TestLocalExecutor_PassThrough(t *testing.T) {
	reg := NewRegistry()
	reg.Register(echoTool("echo"))
	reg.Register(&Tool{Name: "bad", Schema: Obj(nil), Fn: func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("kaboom")
	}})
	e := NewLocalExecutor(reg, LocalExecutorOptions{})
	want, _ := reg.Call(context.Background(), "echo", `{"msg":"hi"}`)
	got, err := e.Execute(context.Background(), Call{ToolUseID: "u", Name: "echo", Input: json.RawMessage(`{"msg":"hi"}`)})
	if err != nil || got != want {
		t.Fatalf("Execute = %q, %v; want %q", got, err, want)
	}
	if _, err := e.Execute(context.Background(), Call{Name: "bad"}); err == nil || err.Error() != "kaboom" {
		t.Fatalf("hard error must pass through, got %v", err)
	}
}

func TestLocalExecutor_Timeout(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&Tool{Name: "polite", Schema: Obj(nil), Fn: func(ctx context.Context, _ json.RawMessage) (any, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			return "late", nil
		}
	}})
	reg.Register(&Tool{Name: "deaf", Timeout: 20 * time.Millisecond, Schema: Obj(nil), Fn: func(context.Context, json.RawMessage) (any, error) {
		time.Sleep(300 * time.Millisecond)
		return "late", nil
	}})

	e := NewLocalExecutor(reg, LocalExecutorOptions{Timeout: 30 * time.Millisecond})
	start := time.Now()
	out, err := e.Execute(context.Background(), Call{ToolUseID: "u1", Name: "polite"})
	if err != nil {
		t.Fatalf("timeout must be a soft failure, got err %v", err)
	}
	if sf := softFailureOf(t, out); sf.ErrorCode != ErrorCodeToolTimeout || !strings.Contains(sf.ErrorMessage, "polite") {
		t.Fatalf("soft failure wrong: %+v", sf)
	}
	if time.Since(start) > time.Second {
		t.Fatal("executor waited for the tool instead of honouring the deadline")
	}

	// Tool.Timeout overrides a generous executor default, and a tool that
	// ignores its context is abandoned rather than awaited.
	e = NewLocalExecutor(reg, LocalExecutorOptions{Timeout: 5 * time.Second})
	start = time.Now()
	out, err = e.Execute(context.Background(), Call{ToolUseID: "u2", Name: "deaf"})
	if err != nil || softFailureOf(t, out).ErrorCode != ErrorCodeToolTimeout {
		t.Fatalf("per-tool timeout not applied: %q %v", out, err)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatal("executor awaited a tool that ignores ctx")
	}
}

func TestLocalExecutor_Panic(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&Tool{Name: "boom", Schema: Obj(nil), Fn: func(context.Context, json.RawMessage) (any, error) {
		panic("boom goes the tool")
	}})
	reg.Register(echoTool("echo"))
	e := NewLocalExecutor(reg, LocalExecutorOptions{})
	out, err := e.Execute(context.Background(), Call{ToolUseID: "u", Name: "boom"})
	if err != nil {
		t.Fatalf("panic must be a soft failure, got err %v", err)
	}
	if sf := softFailureOf(t, out); sf.ErrorCode != ErrorCodeToolPanic || !strings.Contains(sf.ErrorMessage, "boom goes the tool") {
		t.Fatalf("soft failure wrong: %+v", sf)
	}
	// The executor is still healthy afterwards.
	if out, err := e.Execute(context.Background(), Call{Name: "echo"}); err != nil || !strings.Contains(out, `"echoed":true`) {
		t.Fatalf("executor unusable after a panic: %q %v", out, err)
	}
}

func TestLocalExecutor_Cancellation(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&Tool{Name: "polite", Schema: Obj(nil), Fn: func(ctx context.Context, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	reg.Register(&Tool{Name: "deaf", Schema: Obj(nil), Fn: func(context.Context, json.RawMessage) (any, error) {
		time.Sleep(300 * time.Millisecond)
		return "late", nil
	}})
	e := NewLocalExecutor(reg, LocalExecutorOptions{Timeout: 5 * time.Second})
	for _, name := range []string{"polite", "deaf"} {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(20*time.Millisecond, cancel)
		start := time.Now()
		_, err := e.Execute(ctx, Call{ToolUseID: "u", Name: name})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: err = %v, want context.Canceled", name, err)
		}
		if time.Since(start) > 250*time.Millisecond {
			t.Fatalf("%s: cancellation did not propagate promptly", name)
		}
	}
}

func TestLocalExecutor_ObservationCapEnvelope(t *testing.T) {
	big := strings.Repeat("x", 5000)
	reg := NewRegistry()
	reg.Register(&Tool{Name: "env", Schema: Obj(nil), Fn: func(context.Context, json.RawMessage) (any, error) {
		return &Envelope{
			ModelObservation: map[string]any{"rows": big},
			UIPayload:        &UIPayload{Component: "table", Data: big},
			RuntimeState:     big,
			Artifacts:        []ArtifactRef{{ID: "a", Type: ShapeTableRecord}},
		}, nil
	}})
	e := NewLocalExecutor(reg, LocalExecutorOptions{MaxObservationBytes: 512})
	out, err := e.Execute(context.Background(), Call{ToolUseID: "u-env", Name: "env"})
	if err != nil {
		t.Fatal(err)
	}
	obs := ProjectModelObservation(json.RawMessage(out))
	if len(obs) > 512 {
		t.Fatalf("observation not capped: %d bytes", len(obs))
	}
	var tr TruncatedObservation
	if err := json.Unmarshal(obs, &tr); err != nil || !tr.Truncated || tr.OriginalBytes <= 512 || !strings.HasPrefix(tr.Preview, `{"rows":"xxx`) {
		t.Fatalf("marker wrong: %+v %v", tr, err)
	}
	if ui := ExtractUIPayload(json.RawMessage(out)); !strings.Contains(string(ui), big) {
		t.Fatal("ui_payload must not be capped")
	}
	var env Envelope
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.RuntimeState != big || len(env.Artifacts) != 1 {
		t.Fatalf("runtime_state / artifacts must pass through: %v", err)
	}
	// WithCallContext was applied: runtime_state is in the store under the
	// tool_use id, so render_from_ref can resolve it later.
	if _, v, ok := reg.Artifacts().Get(context.Background(), "u-env"); !ok || v != big {
		t.Fatal("runtime_state not stashed under the tool_use id")
	}
}

func TestLocalExecutor_ObservationCapBare(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&Tool{Name: "bare", Schema: Obj(nil), Fn: func(context.Context, json.RawMessage) (any, error) {
		return strings.Repeat("é", 3000), nil
	}})
	e := NewLocalExecutor(reg, LocalExecutorOptions{MaxObservationBytes: 200})
	out, err := e.Execute(context.Background(), Call{Name: "bare"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 200 {
		t.Fatalf("bare result not capped: %d bytes", len(out))
	}
	var tr TruncatedObservation
	if err := json.Unmarshal([]byte(out), &tr); err != nil || !tr.Truncated || tr.OriginalBytes != 6002 {
		t.Fatalf("marker wrong: %+v %v", tr, err)
	}
	if !utf8.ValidString(tr.Preview) || !strings.HasPrefix(tr.Preview, `"éé`) {
		t.Fatalf("preview must be a UTF-8 clean prefix of the JSON text: %q", tr.Preview)
	}
}

func TestCapObservation(t *testing.T) {
	small := `{"a":1}`
	if CapObservation(small, 100) != small || CapObservation(small, 0) != small || CapObservation(small, -1) != small {
		t.Fatal("small / uncapped input must be returned unchanged")
	}
	// An envelope whose observation fits is untouched even when the other
	// channels push the whole blob past the cap.
	env, _ := json.Marshal(&Envelope{ModelObservation: map[string]any{"n": 1}, RuntimeState: strings.Repeat("r", 1000)})
	if got := CapObservation(string(env), 256); got != string(env) {
		t.Fatalf("envelope with a small observation must be unchanged, got %s", got)
	}
	// Heavy escaping (quotes, HTML-escaped characters) must not push the
	// marker past the cap.
	noisy := `"` + strings.Repeat(`\"<&>`, 500) + `"`
	for _, max := range []int{80, 128, 300} {
		got := CapObservation(noisy, max)
		if len(got) > max || !json.Valid([]byte(got)) {
			t.Fatalf("max=%d: marker is %d bytes / valid=%v", max, len(got), json.Valid([]byte(got)))
		}
	}
	// A cap smaller than the marker frame still yields valid JSON.
	if got := CapObservation(noisy, 10); !json.Valid([]byte(got)) {
		t.Fatalf("tiny cap must still be valid JSON: %s", got)
	}
}
