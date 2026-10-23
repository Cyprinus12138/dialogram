package tool

import (
	"context"
	"encoding/json"
	"fmt"
)

// RenderFromRefToolName is the consumer side of the handle pattern.
const RenderFromRefToolName = "render_from_ref"

// RegisterRenderFromRef adds the render_from_ref tool. Data tools return
// small ArtifactRef handles in their model_observation; the model passes a
// handle to render_from_ref when it wants the rows displayed;
// render_from_ref resolves the handle against the ArtifactStore server-side
// and emits a ui_payload.
//
// The raw rows never enter the model context. Once a tool has stashed a
// TableRecord in the store, the model can ask for it to be rendered with any
// subset of columns and any supported component without pulling the data
// through its own context window.
func RegisterRenderFromRef(r *Registry) {
	r.Register(RenderFromRef())
}

// RenderFromRef returns the render_from_ref tool definition.
func RenderFromRef() *Tool {
	return &Tool{
		Name:  RenderFromRefToolName,
		Label: "Render saved data",
		Description: `Render a previously-captured data artifact directly in the chat UI without pulling its rows into your context.

When a data tool returns an artifact ref (look for "artifacts": [...] in its result), pass that ref id here and choose how to display it. The server resolves the ref and emits the visual component to the user.

Supported components:

comparison_table — full row-and-column table.
  Required: ref, component="comparison_table"
  Optional: title, columns (subset of column keys to include — defaults to all)

bar_chart        — horizontal bars from a single numeric column.
  Required: ref, component="bar_chart", x_column (label), y_column (numeric)
  Optional: title, x_label, y_label

Always prefer this over present_data when an upstream tool already returned an artifact ref — it saves tokens and avoids re-typing the rows.`,
		Schema: Obj(map[string]any{
			"ref":       Str("Artifact id from an earlier tool result's artifacts[] entry"),
			"component": Enum("UI component to render", ComponentComparisonTable, ComponentBarChart),
			"title":     Str("Optional section title shown above the component"),
			"columns": map[string]any{
				"type":        "array",
				"description": "Optional subset of column keys (from the artifact's columns) to display. Order is preserved. Omit to show all columns.",
				"items":       map[string]any{"type": "string"},
			},
			"x_column": Str("For bar_chart: the column key whose values become bar labels"),
			"y_column": Str("For bar_chart: the column key whose numeric values become bar heights"),
			"x_label":  Str("Optional x-axis label for bar_chart"),
			"y_label":  Str("Optional y-axis label for bar_chart"),
		}, "ref", "component"),
		Fn: func(ctx context.Context, input json.RawMessage) (any, error) {
			var args struct {
				Ref       string   `json:"ref"`
				Component string   `json:"component"`
				Title     string   `json:"title"`
				Columns   []string `json:"columns"`
				XColumn   string   `json:"x_column"`
				YColumn   string   `json:"y_column"`
				XLabel    string   `json:"x_label"`
				YLabel    string   `json:"y_label"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return SoftEnvelope(ErrorCodeInvalidInput, "could not parse render_from_ref arguments"), nil
			}
			if args.Ref == "" {
				return SoftEnvelope(ErrorCodeInvalidInput, "ref is required"), nil
			}
			store := ArtifactStoreFromContext(ctx)
			if store == nil {
				return SoftEnvelope("RUNTIME_UNAVAILABLE", "artifact store not configured"), nil
			}
			kind, value, ok := store.Get(ctx, args.Ref)
			if !ok {
				return SoftEnvelope("REF_NOT_FOUND", fmt.Sprintf("ref %q not found or expired", args.Ref)), nil
			}
			switch args.Component {
			case ComponentComparisonTable:
				return renderTable(kind, value, args.Title, args.Columns)
			case ComponentBarChart:
				return renderBar(kind, value, args.Title, args.XColumn, args.YColumn, args.XLabel, args.YLabel)
			default:
				return SoftEnvelope("UNSUPPORTED_COMPONENT", fmt.Sprintf("component %q not supported", args.Component)), nil
			}
		},
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
	}
}

func renderTable(kind string, value any, title string, columnsFilter []string) (*Envelope, error) {
	if kind != ShapeTableRecord {
		return SoftEnvelope("INCOMPATIBLE_SHAPE", fmt.Sprintf("ref is %q, expected %q", kind, ShapeTableRecord)), nil
	}
	tr, ok := CoerceTableRecord(value)
	if !ok {
		return SoftEnvelope("INCOMPATIBLE_SHAPE", "stored value is not a TableRecord"), nil
	}
	cols, idx := selectColumns(tr.Columns, columnsFilter)
	headerLabels := make([]string, len(cols))
	for i, c := range cols {
		headerLabels[i] = c.Label
	}
	stringRows := make([][]string, 0, len(tr.Rows))
	for _, r := range tr.Rows {
		row := make([]string, 0, len(idx))
		for _, i := range idx {
			if i >= 0 && i < len(r) {
				row = append(row, formatCell(r[i]))
			} else {
				row = append(row, "")
			}
		}
		stringRows = append(stringRows, row)
	}
	return &Envelope{
		ModelObservation: map[string]any{
			"rendered":  true,
			"component": ComponentComparisonTable,
			"row_count": len(stringRows),
			"col_count": len(cols),
		},
		UIPayload: &UIPayload{
			Component: ComponentComparisonTable,
			Data: map[string]any{
				"type":    ComponentComparisonTable,
				"title":   title,
				"columns": headerLabels,
				"rows":    stringRows,
			},
		},
	}, nil
}

func renderBar(kind string, value any, title, xColumn, yColumn, xLabel, yLabel string) (*Envelope, error) {
	if kind != ShapeTableRecord {
		return SoftEnvelope("INCOMPATIBLE_SHAPE", fmt.Sprintf("ref is %q, expected %q", kind, ShapeTableRecord)), nil
	}
	if xColumn == "" || yColumn == "" {
		return SoftEnvelope(ErrorCodeInvalidInput, "bar_chart requires x_column and y_column"), nil
	}
	tr, ok := CoerceTableRecord(value)
	if !ok {
		return SoftEnvelope("INCOMPATIBLE_SHAPE", "stored value is not a TableRecord"), nil
	}
	xi, yi := -1, -1
	for i, c := range tr.Columns {
		if c.Key == xColumn {
			xi = i
		}
		if c.Key == yColumn {
			yi = i
		}
	}
	if xi < 0 || yi < 0 {
		return SoftEnvelope("UNKNOWN_COLUMN", fmt.Sprintf("x_column=%q or y_column=%q not in artifact", xColumn, yColumn)), nil
	}
	type barPoint struct {
		Label string  `json:"label"`
		Value float64 `json:"value"`
	}
	pts := make([]barPoint, 0, len(tr.Rows))
	for _, r := range tr.Rows {
		if xi >= len(r) || yi >= len(r) {
			continue
		}
		v, _ := ParseFloat(r[yi])
		pts = append(pts, barPoint{Label: formatCell(r[xi]), Value: v})
	}
	return &Envelope{
		ModelObservation: map[string]any{
			"rendered":    true,
			"component":   ComponentBarChart,
			"point_count": len(pts),
		},
		UIPayload: &UIPayload{
			Component: ComponentBarChart,
			Data: map[string]any{
				"type":    ComponentBarChart,
				"title":   title,
				"x_label": xLabel,
				"y_label": yLabel,
				"data":    pts,
			},
		},
	}, nil
}

// CoerceTableRecord accepts either a *TableRecord or its JSON-decoded form
// (map[string]any from a generic any). The in-memory store keeps values as
// the original Go type; a shared store serialises through JSON, so both
// shapes round-trip.
func CoerceTableRecord(value any) (*TableRecord, bool) {
	switch v := value.(type) {
	case *TableRecord:
		return v, true
	case TableRecord:
		return &v, true
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var tr TableRecord
	if err := json.Unmarshal(b, &tr); err != nil {
		return nil, false
	}
	if len(tr.Columns) == 0 {
		return nil, false
	}
	return &tr, true
}

// selectColumns returns the filtered column slice and the parallel index
// list mapping each output column back to its source position. An empty
// filter means "all"; unknown keys are skipped silently so the model can be
// optimistic about column names without erroring.
func selectColumns(all []TableColumn, filter []string) ([]TableColumn, []int) {
	if len(filter) == 0 {
		idx := make([]int, len(all))
		for i := range all {
			idx[i] = i
		}
		return all, idx
	}
	keyToIdx := make(map[string]int, len(all))
	for i, c := range all {
		keyToIdx[c.Key] = i
	}
	cols := make([]TableColumn, 0, len(filter))
	idx := make([]int, 0, len(filter))
	for _, k := range filter {
		if i, ok := keyToIdx[k]; ok {
			cols = append(cols, all[i])
			idx = append(idx, i)
		}
	}
	return cols, idx
}

func formatCell(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// ParseFloat tolerates numbers and numeric strings.
func ParseFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		var f float64
		_, err := fmt.Sscanf(x, "%f", &f)
		return f, err == nil
	}
	return 0, false
}
