package tool

import (
	"context"
	"encoding/json"
)

// PresentDataToolName is the pass-through tool the model calls to render a
// structured component inline in the chat. The streaming layer converts its
// tool_result into a gen_ui event carrying the tool input verbatim.
const PresentDataToolName = "present_data"

// Built-in UI component types understood by the reference renderer.
const (
	ComponentMetricCards     = "metric_cards"
	ComponentBarChart        = "bar_chart"
	ComponentLineChart       = "line_chart"
	ComponentComparisonTable = "comparison_table"
)

// RegisterPresentData adds the present_data tool. It is a pure pass-through:
// the model calls it with a structured JSON payload describing a UI
// component (chart, table, metric cards) and it returns {"displayed": true}
// immediately. The HTTP streaming layer intercepts the tool_result event and
// emits a gen_ui event so the frontend renders the component inline, with
// no extra round-trip and no schema changes (the payload is persisted
// naturally as the tool input).
func RegisterPresentData(r *Registry) {
	r.Register(PresentData())
}

// PresentData returns the present_data tool definition.
func PresentData() *Tool {
	return &Tool{
		Name:  PresentDataToolName,
		Label: "Show a visual",
		Description: `Render a structured data visualisation directly in the chat UI. Use this whenever you have numbers, comparisons, or structured details that would be clearer as a visual component than as prose.

Available types — choose the best fit:

metric_cards  — row of KPI cards; ideal for a handful of headline numbers.
  Required: type, cards[].label, cards[].value
  Optional: title, cards[].sub (sub-label), cards[].highlight (accent colour, bool)

bar_chart     — horizontal bars; ideal for comparing a value across categories.
  Required: type, data[].label, data[].value (number)
  Optional: title, x_label, y_label

line_chart    — time-series trend line; ideal for a value over days/months/years.
  Required: type, data[].label, data[].value (number)
  Optional: title, x_label, y_label, series (multi-series: array of {name, data[].label, data[].value})

comparison_table — column-header table; ideal for side-by-side options.
  Required: type, columns (string array), rows (array of string arrays)
  Optional: title

Always call this tool AFTER gathering real data — never fabricate values. Follow up with text explanation if context is needed.`,
		Schema: Obj(map[string]any{
			"type":  Enum("UI component type", ComponentMetricCards, ComponentBarChart, ComponentLineChart, ComponentComparisonTable),
			"title": Str("Optional section title shown above the component"),

			"cards": map[string]any{
				"type":        "array",
				"description": "Cards to display (required for metric_cards)",
				"items": Obj(map[string]any{
					"label":     Str("Card label, e.g. 'High today'"),
					"value":     Str("Display value, e.g. '27 °C'"),
					"sub":       Str("Optional sub-label below the value"),
					"highlight": Bool("True to render in accent colour"),
				}, "label", "value"),
			},

			"data": map[string]any{
				"type":        "array",
				"description": "Data rows for bar_chart or single-series line_chart",
				"items": Obj(map[string]any{
					"label": Str("Category or x-axis label"),
					"value": Num("Numeric value"),
				}, "label", "value"),
			},
			"x_label": Str("X-axis label for bar_chart or line_chart"),
			"y_label": Str("Y-axis label for bar_chart or line_chart"),
			"series": map[string]any{
				"type":        "array",
				"description": "Named data series for multi-line line_chart; use instead of data[] when comparing multiple groups",
				"items": Obj(map[string]any{
					"name": Str("Series name"),
					"data": map[string]any{
						"type":        "array",
						"description": "Data points for this series",
						"items": Obj(map[string]any{
							"label": Str("X-axis label, e.g. '2026-01'"),
							"value": Num("Numeric value"),
						}, "label", "value"),
					},
				}, "name", "data"),
			},

			"columns": map[string]any{
				"type":        "array",
				"description": "Column headers for comparison_table",
				"items":       map[string]any{"type": "string"},
			},
			"rows": map[string]any{
				"type":        "array",
				"description": "Data rows for comparison_table — each row is a string array matching columns length",
				"items": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
				},
			},
		}, "type"),
		Fn: func(ctx context.Context, input json.RawMessage) (any, error) {
			return map[string]any{"displayed": true}, nil
		},
	}
}
