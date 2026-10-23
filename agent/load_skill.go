package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Cyprinus12138/dialogram/tool"
)

// LoadSkillToolName is the reserved tool that returns a packaged skill's
// full body by id.
const LoadSkillToolName = "load_skill"

// RegisterLoadSkill wires the load_skill tool into reg, backed by lib. It is
// a no-op when lib is nil or empty, so no load_skill appears on the wire
// when there is nothing to load.
//
// Tool contract:
//
//	input:  {"id": "<skill-id>"}
//	output (hit):  {"id": "...", "name": "...", "body": "..."}
//	output (miss): {"error_code": "UNKNOWN_SKILL", "error_message": "...", "retryable": false}
func RegisterLoadSkill(reg *tool.Registry, lib *SkillLibrary) {
	if reg == nil || lib == nil || lib.Len() == 0 {
		return
	}
	reg.Register(&tool.Tool{
		Name:  LoadSkillToolName,
		Label: "Load a skill",
		Description: "Load the full instructions for a skill advertised in the 'Available skills' section of the " +
			"system prompt. Call only when the user's request matches one of the advertised workflows.",
		Schema: tool.Obj(map[string]any{
			"id": tool.Str("Skill id from the 'Available skills' summaries."),
		}, "id"),
		Fn: func(_ context.Context, input json.RawMessage) (any, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return tool.Soft(tool.ErrorCodeInvalidInput, fmt.Sprintf("decode input: %v", err)), nil
			}
			if args.ID == "" {
				return tool.Soft(tool.ErrorCodeInvalidInput, "id is required"), nil
			}
			s, ok := lib.Get(args.ID)
			if !ok {
				return tool.Soft("UNKNOWN_SKILL", fmt.Sprintf("no skill with id %q", args.ID)), nil
			}
			return map[string]any{"id": s.ID, "name": s.Name, "body": s.Body}, nil
		},
		Annotations: tool.Annotations{ReadOnly: true, Idempotent: true},
	})
}
