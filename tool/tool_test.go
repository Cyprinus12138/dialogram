package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func echoTool(name string) *Tool {
	return &Tool{
		Name:        name,
		Description: "echoes input",
		Schema:      Obj(map[string]any{"msg": Str("message")}),
		Fn: func(ctx context.Context, input json.RawMessage) (any, error) {
			return map[string]any{"echoed": true}, nil
		},
	}
}

func TestRegistry_RegisterAndCall(t *testing.T) {
	reg := NewRegistry()
	reg.Register(echoTool("echo"))
	out, err := reg.Call(context.Background(), "echo", `{"msg":"hi"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, `"echoed":true`) {
		t.Errorf("unexpected output %s", out)
	}
}

func TestRegistry_UnknownTool(t *testing.T) {
	reg := NewRegistry()
	if _, err := reg.Call(context.Background(), "nonexistent", "{}"); err == nil {
		t.Errorf("expected error for unknown tool")
	}
}

func TestRegistry_DuplicatePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic for duplicate tool registration")
		}
	}()
	reg := NewRegistry()
	tool := echoTool("dup")
	reg.Register(tool)
	reg.Register(tool)
}

func TestRegistry_NamesOrder(t *testing.T) {
	reg := NewRegistry()
	for _, name := range []string{"c", "a", "b"} {
		reg.Register(echoTool(name))
	}
	names := reg.Names()
	want := []string{"c", "a", "b"}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("Names()[%d] = %q, want %q", i, names[i], w)
		}
	}
}

func TestRegistry_Specs(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&Tool{
		Name:        "my_tool",
		Description: "does something",
		Schema:      Obj(map[string]any{"x": Num("x value")}, "x"),
		Fn:          func(ctx context.Context, input json.RawMessage) (any, error) { return nil, nil },
	})
	reg.Register(echoTool("other"))

	a := reg.AnthropicSpec()
	if len(a) != 2 || a[0]["name"] != "my_tool" || a[0]["description"] != "does something" {
		t.Fatalf("anthropic spec = %v", a)
	}
	if _, ok := a[0]["input_schema"]; !ok {
		t.Errorf("missing input_schema key")
	}
	o := reg.OpenAISpec()
	fn, _ := o[0]["function"].(map[string]any)
	if o[0]["type"] != "function" || fn["name"] != "my_tool" {
		t.Fatalf("openai spec = %v", o)
	}
	if _, ok := fn["parameters"]; !ok {
		t.Errorf("missing parameters key")
	}

	// Allow-list narrows; empty non-nil list hides everything; nil = all.
	if got := reg.AnthropicSpecFor([]string{"other"}); len(got) != 1 || got[0]["name"] != "other" {
		t.Errorf("allow-list filter failed: %v", got)
	}
	if got := reg.OpenAISpecFor([]string{}); len(got) != 0 {
		t.Errorf("empty allow-list must hide every tool, got %d", len(got))
	}
	if got := reg.OpenAISpecFor(nil); len(got) != 2 {
		t.Errorf("nil allow-list must show every tool, got %d", len(got))
	}
}

func TestRegistry_Label(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&Tool{Name: "x", Label: "Ex", Schema: Obj(nil), Fn: shortCircuit})
	if reg.Label("x") != "Ex" || reg.Label("missing") != "" {
		t.Errorf("Label lookup wrong")
	}
}

func TestProjectModelObservation_Envelope(t *testing.T) {
	env := Envelope{
		ModelObservation: map[string]any{"summary": "ok", "count": 42},
		UIPayload: &UIPayload{
			Component: ComponentComparisonTable,
			Data:      map[string]any{"columns": []string{"a", "b"}, "rows": [][]any{{"x", "y"}}},
		},
		RuntimeState: map[string]any{"rows": []int{1, 2, 3}},
	}
	raw, _ := json.Marshal(env)
	got := string(ProjectModelObservation(raw))
	if !strings.Contains(got, `"summary":"ok"`) {
		t.Fatalf("model_observation lost: %s", got)
	}
	if strings.Contains(got, "ui_payload") || strings.Contains(got, "comparison_table") {
		t.Fatalf("ui_payload leaked into observation: %s", got)
	}
	if strings.Contains(got, "runtime_state") || strings.Contains(got, "rows") {
		t.Fatalf("runtime_state leaked into observation: %s", got)
	}
}

func TestProjectModelObservation_BarePassthrough(t *testing.T) {
	bare := json.RawMessage(`{"foo":"bar","count":3}`)
	if got := ProjectModelObservation(bare); string(got) != string(bare) {
		t.Fatalf("bare blob mutated: got %s, want %s", got, bare)
	}
	if got := ProjectModelObservation(nil); got != nil {
		t.Fatalf("nil must pass through")
	}
}

func TestProjectModelObservation_EmptyEnvelope(t *testing.T) {
	raw := json.RawMessage(`{"ui_payload":{"component":"x","data":null}}`)
	if got := string(ProjectModelObservation(raw)); got != "null" {
		t.Fatalf("expected null observation, got %s", got)
	}
}

func TestExtractUIPayload(t *testing.T) {
	env := Envelope{
		ModelObservation: map[string]any{"summary": "ok"},
		UIPayload:        &UIPayload{Component: ComponentBarChart, Data: map[string]any{"data": []any{}}},
	}
	raw, _ := json.Marshal(env)
	if got := ExtractUIPayload(raw); len(got) == 0 {
		t.Fatalf("expected ui_payload extracted, got nil")
	}
	if got := ExtractUIPayload(json.RawMessage(`{"foo":"bar"}`)); got != nil {
		t.Fatalf("expected nil for bare blob, got %s", got)
	}
}

func TestMemoryArtifactStore_PutGet(t *testing.T) {
	s := NewMemoryArtifactStore(time.Minute)
	tr := &TableRecord{Columns: []TableColumn{{Key: "a", Label: "A"}}, Rows: [][]any{{1}, {2}}}
	if err := s.Put(context.Background(), "id-1", ShapeTableRecord, tr); err != nil {
		t.Fatalf("put: %v", err)
	}
	kind, val, ok := s.Get(context.Background(), "id-1")
	if !ok || kind != ShapeTableRecord {
		t.Fatalf("get: ok=%v kind=%q", ok, kind)
	}
	if got, ok := val.(*TableRecord); !ok || len(got.Rows) != 2 {
		t.Fatalf("rows lost: %#v", val)
	}
}

func TestMemoryArtifactStore_TTL(t *testing.T) {
	s := NewMemoryArtifactStore(10 * time.Millisecond)
	_ = s.Put(context.Background(), "id-1", ShapeTableRecord, "v")
	time.Sleep(25 * time.Millisecond)
	if _, _, ok := s.Get(context.Background(), "id-1"); ok {
		t.Fatalf("expected expired entry to be gone")
	}
}

func TestRegistryCall_StashesRuntimeState(t *testing.T) {
	r := NewRegistry()
	tr := &TableRecord{Columns: []TableColumn{{Key: "k", Label: "K"}}, Rows: [][]any{{"v1"}}}
	r.Register(&Tool{
		Name:   "test_tool",
		Schema: Obj(nil),
		Fn: func(ctx context.Context, _ json.RawMessage) (any, error) {
			return &Envelope{
				ModelObservation: map[string]any{"ok": true},
				RuntimeState:     tr,
				Artifacts:        []ArtifactRef{{ID: ToolUseIDFromContext(ctx), Type: ShapeTableRecord}},
			}, nil
		},
	})
	ctx := WithCallContext(context.Background(), "dg_test_use_id", r.Artifacts())
	out, err := r.Call(ctx, "test_tool", `{}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(out, `"runtime_state"`) {
		t.Fatalf("expected envelope in wire output, got %s", out)
	}
	kind, val, ok := r.Artifacts().Get(ctx, "dg_test_use_id")
	if !ok || kind != ShapeTableRecord {
		t.Fatalf("artifact not stored: ok=%v kind=%q", ok, kind)
	}
	if got, ok := val.(*TableRecord); !ok || len(got.Rows) != 1 {
		t.Fatalf("artifact value lost: %#v", val)
	}
}

func TestRenderFromRef_TableProjectsColumns(t *testing.T) {
	r := NewRegistry()
	RegisterRenderFromRef(r)
	store := r.Artifacts()
	_ = store.Put(context.Background(), "ref-1", ShapeTableRecord, &TableRecord{
		Columns: []TableColumn{{Key: "date", Label: "Date"}, {Key: "city", Label: "City"}, {Key: "high_c", Label: "High"}},
		Rows:    [][]any{{"2026-04-01", "Lisbon", 21}, {"2026-04-02", "Lisbon", 23}},
	})
	ctx := WithCallContext(context.Background(), "dg_caller", store)
	out, err := r.Call(ctx, RenderFromRefToolName, `{"ref":"ref-1","component":"comparison_table","columns":["date","high_c"]}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if env.UIPayload == nil || env.UIPayload.Component != ComponentComparisonTable {
		t.Fatalf("ui_payload missing or wrong: %+v", env.UIPayload)
	}
	obs, _ := json.Marshal(env.ModelObservation)
	if strings.Contains(string(obs), "Lisbon") || strings.Contains(string(obs), "21") {
		t.Fatalf("row data leaked into model observation: %s", obs)
	}
	data, _ := json.Marshal(env.UIPayload.Data)
	if !strings.Contains(string(data), "23") {
		t.Fatalf("ui_payload missing projected rows: %s", data)
	}
	if strings.Contains(string(data), "Lisbon") || strings.Contains(string(data), "City") {
		t.Fatalf("filtered-out column leaked into ui_payload: %s", data)
	}
}

func TestRenderFromRef_BarChart(t *testing.T) {
	r := NewRegistry()
	RegisterRenderFromRef(r)
	_ = r.Artifacts().Put(context.Background(), "ref-2", ShapeTableRecord, map[string]any{
		"columns": []map[string]any{{"key": "day", "label": "Day"}, {"key": "high", "label": "High"}},
		"rows":    [][]any{{"Mon", "21"}, {"Tue", 24.5}},
	})
	ctx := WithCallContext(context.Background(), "dg_caller", r.Artifacts())
	out, err := r.Call(ctx, RenderFromRefToolName, `{"ref":"ref-2","component":"bar_chart","x_column":"day","y_column":"high"}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(out, `"point_count":2`) || !strings.Contains(out, `"value":24.5`) {
		t.Fatalf("bar chart projection wrong: %s", out)
	}
}

func TestRenderFromRef_SoftFailures(t *testing.T) {
	r := NewRegistry()
	RegisterRenderFromRef(r)
	ctx := WithCallContext(context.Background(), "dg_caller", r.Artifacts())
	out, err := r.Call(ctx, RenderFromRefToolName, `{"ref":"missing","component":"comparison_table"}`)
	if err != nil || !strings.Contains(out, "REF_NOT_FOUND") {
		t.Fatalf("expected REF_NOT_FOUND soft failure, got out=%s err=%v", out, err)
	}
	out, _ = r.Call(ctx, RenderFromRefToolName, `{"component":"comparison_table"}`)
	if !strings.Contains(out, ErrorCodeInvalidInput) {
		t.Fatalf("expected INVALID_INPUT, got %s", out)
	}
}

func TestHITL_ToolsShortCircuit(t *testing.T) {
	r := NewRegistry()
	RegisterHITL(r)
	for _, n := range HITLToolNames {
		if !IsHITLTool(n) {
			t.Errorf("%s must be a HITL tool", n)
		}
		if _, err := r.Call(context.Background(), n, `{}`); err != ErrHITLShortCircuit {
			t.Errorf("%s: expected short-circuit error, got %v", n, err)
		}
	}
	if IsHITLTool("get_weather") {
		t.Error("regular tool must not be a HITL tool")
	}
}

func TestPresentDataAndNow(t *testing.T) {
	r := NewRegistry()
	RegisterPresentData(r)
	RegisterNow(r, nil)
	out, err := r.Call(context.Background(), PresentDataToolName, `{"type":"metric_cards","cards":[{"label":"a","value":"1"}]}`)
	if err != nil || !strings.Contains(out, `"displayed":true`) {
		t.Fatalf("present_data: out=%s err=%v", out, err)
	}
	out, err = r.Call(context.Background(), NowToolName, `{}`)
	if err != nil || !strings.Contains(out, `"timezone":"UTC"`) {
		t.Fatalf("now: out=%s err=%v", out, err)
	}
}

func TestSoftHelpers(t *testing.T) {
	s := Soft(ErrorCodeUpstreamTimeout, "slow")
	if s["retryable"] != true {
		t.Error("upstream timeout must be retryable")
	}
	if Soft(ErrorCodeNotFound, "x")["retryable"] != false {
		t.Error("not found must not be retryable")
	}
	if ProjectModelObservation(mustJSON(SoftEnvelope("E", "m")))[0] != '{' {
		t.Error("soft envelope must project an object")
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
