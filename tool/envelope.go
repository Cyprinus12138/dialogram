package tool

import "encoding/json"

// Envelope is the structured tool-result shape that splits a tool's output
// across three channels. Tools opt in by returning *Envelope from their Fn;
// tools that return any other type fall through to the bare-JSON path with
// no envelope handling.
//
// Channels:
//
//   - ModelObservation: the small, model-visible projection. Provider
//     translators send only this slice back to the model in subsequent
//     turns. It should contain summaries, key facts, counts, available
//     follow-up actions, warnings — only what the model needs to reason
//     about the next step. Never raw rows or rendered UI props.
//
//   - UIPayload: the inline-UI render directive. The HTTP streaming layer
//     emits it as a gen_ui event the moment the tool returns; it is never
//     appended to the model context.
//
//   - RuntimeState: server-only data. Persisted in the block IR for
//     resume/failover but never sent to the model and not emitted to the
//     frontend. Holds raw rows, pagination cursors, debug info. When a
//     handle scheme is in use (see ArtifactStore), the runtime state is
//     what backs the handles referenced in ModelObservation.
//
//   - Artifacts: optional manifest of opaque references the model may pass
//     to other tools (e.g. render_from_ref). Each entry carries a stable
//     id, type tag and human description; the bytes live in the
//     ArtifactStore keyed by id.
//
// Persistence note: the entire envelope is serialised into the tool_result
// block so a conversation paused on tool_result can resume on a different
// provider without losing UI replay or runtime state. Provider translators
// look for the envelope shape and project ModelObservation; if the shape is
// absent (bare persisted blobs), the raw JSON is forwarded as-is.
type Envelope struct {
	ModelObservation any           `json:"model_observation,omitempty"`
	UIPayload        *UIPayload    `json:"ui_payload,omitempty"`
	RuntimeState     any           `json:"runtime_state,omitempty"`
	Artifacts        []ArtifactRef `json:"artifacts,omitempty"`
}

// UIPayload is the out-of-band render directive consumed by the frontend.
// Component is a stable type name (e.g. "comparison_table", "line_chart");
// Data is the component-specific props and is forwarded verbatim.
// Interaction is reserved for click/handler metadata.
type UIPayload struct {
	Component   string         `json:"component"`
	Data        any            `json:"data"`
	Interaction map[string]any `json:"interaction,omitempty"`
}

// ArtifactRef points at runtime data stashed in the ArtifactStore. The model
// never sees the bytes — only this reference — and may pass the reference to
// other tools (notably render_from_ref) which dereference it server-side.
//
// Type is a canonical shape tag (ShapeTableRecord, ShapeTimeSeries, …) so
// consumers can dispatch without introspecting the data. Description is a
// human-readable hint surfaced in the model's observation so the model
// knows what each ref holds.
type ArtifactRef struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// Canonical shapes the built-in renderer and render_from_ref know how to
// handle. New tools should snap to one of these when possible.
const (
	ShapeTableRecord = "table_record"
	ShapeTimeSeries  = "time_series"
)

// TableRecord is the canonical shape for tabular results. Columns describes
// the schema; Rows is a list of cell arrays matching Columns by position.
type TableRecord struct {
	Columns []TableColumn `json:"columns"`
	Rows    [][]any       `json:"rows"`
}

// TableColumn describes one column in a TableRecord. Type is a hint for the
// renderer ("string", "number", "currency", "date") used for alignment and
// formatting.
type TableColumn struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Type  string `json:"type,omitempty"`
}

// envelopeWire is the JSON shape used to detect "is this an Envelope?"
// without fully decoding the payload.
type envelopeWire struct {
	ModelObservation json.RawMessage `json:"model_observation"`
	UIPayload        json.RawMessage `json:"ui_payload"`
	RuntimeState     json.RawMessage `json:"runtime_state"`
	Artifacts        json.RawMessage `json:"artifacts"`
}

// ProjectModelObservation extracts the model-visible slice from a persisted
// tool_result blob. If the blob is an Envelope, it returns the
// re-serialised model_observation field (or "null" when the field is
// absent). If the blob is bare JSON, it is returned unchanged.
//
// Provider translators call this when assembling tool_result blocks for the
// wire so the model never sees ui_payload or runtime_state, while the
// persisted IR keeps the full envelope for failover replay.
func ProjectModelObservation(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var w envelopeWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return raw
	}
	if len(w.ModelObservation) == 0 && len(w.UIPayload) == 0 &&
		len(w.RuntimeState) == 0 && len(w.Artifacts) == 0 {
		return raw
	}
	if len(w.ModelObservation) == 0 {
		return json.RawMessage("null")
	}
	return w.ModelObservation
}

// ExtractUIPayload returns the ui_payload slice from a persisted tool_result
// blob, or nil when the blob is not an envelope or has no UI payload. The
// HTTP layer uses this to emit gen_ui events generically — any tool whose
// envelope carries ui_payload triggers a render.
func ExtractUIPayload(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var w envelopeWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil
	}
	if len(w.UIPayload) == 0 {
		return nil
	}
	return w.UIPayload
}
