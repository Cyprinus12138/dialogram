package agent

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Cyprinus12138/dialogram/tool"
)

// ErrNotPending is returned by AnswerPause for a tool call that is not in
// the conversation, or already has a result.
var ErrNotPending = errors.New("agent: the tool call is not pending")

// AnswerPause resumes a conversation that paused on the tool call
// toolUseID. It returns turns with that call answered by result, and with
// every other call of the same assistant turn that has no result yet
// answered by NotRunResultJSON. Run the next turn on what it returns.
//
// The other calls matter. The loop surfaces one pause at a time, and it
// runs nothing in a turn it pauses for approval, so a paused turn can hold
// calls besides the one the user is asked about: a second pause tool, or
// the other tools of a turn in which one needed approval. Both providers
// reject a history in which a tool call has no result, so answering only
// the paused call fails the next request. The model reads the not-run
// results and can make those calls again; they then pass the interceptors
// like any other call.
//
// The results go into the tool turn right after the paused assistant
// turn, completing one already there, so turns appended since then stay
// valid. turns is not modified. ErrNotPending wraps the error for a call
// that is unknown or already answered.
func AnswerPause(turns []Turn, toolUseID string, result json.RawMessage) ([]Turn, error) {
	answered := map[string]bool{}
	for _, t := range turns {
		for _, b := range t.Blocks {
			if b.Type == BlockToolResult {
				answered[b.ToolUseID] = true
			}
		}
	}
	owner := -1
	for i, t := range turns {
		if t.Role != RoleAssistant {
			continue
		}
		for _, b := range t.Blocks {
			if b.Type == BlockToolUse && b.ToolUseID == toolUseID {
				owner = i
			}
		}
	}
	if owner < 0 {
		return nil, fmt.Errorf("%w: no tool call %s in the conversation", ErrNotPending, toolUseID)
	}
	if answered[toolUseID] {
		return nil, fmt.Errorf("%w: tool call %s already has a result", ErrNotPending, toolUseID)
	}
	var blocks []Block
	for _, b := range turns[owner].Blocks {
		if b.Type != BlockToolUse || answered[b.ToolUseID] {
			continue
		}
		r := result
		if b.ToolUseID != toolUseID {
			r = json.RawMessage(NotRunResultJSON(b.ToolName))
		}
		blocks = append(blocks, Block{Type: BlockToolResult, ToolUseID: b.ToolUseID, ToolName: b.ToolName, ToolResult: r})
	}

	out := make([]Turn, 0, len(turns)+1)
	out = append(out, turns[:owner+1]...)
	rest := turns[owner+1:]
	if len(rest) > 0 && rest[0].Role == RoleTool {
		tt := rest[0]
		tt.Blocks = append(append([]Block(nil), tt.Blocks...), blocks...)
		out = append(out, tt)
		rest = rest[1:]
	} else {
		out = append(out, Turn{Role: RoleTool, Blocks: blocks})
	}
	return append(out, rest...), nil
}

// NotRunResultJSON is the tool_result body AnswerPause gives a call of the
// paused turn that never ran. It is a soft failure the model can act on by
// calling the tool again.
func NotRunResultJSON(toolName string) string {
	b, _ := json.Marshal(tool.Soft(tool.ErrorCodeNotRun, fmt.Sprintf(
		"%s was not run: it was requested in the same turn as a call that needed the user's input or approval. Call it again if it is still needed.", toolName)))
	return string(b)
}

// DismissedResultJSON is the tool_result body for a pause the user left
// without an answer: they dismissed it, or sent a new message instead.
// kind is the pause's kind (a PauseKind* constant). An approval reads as
// declined (DeniedResultJSON), so the model never takes the call for done;
// any other pause is a USER_CANCELLED soft failure. An empty reason gets a
// default.
func DismissedResultJSON(kind, reason string) string {
	if kind == PauseKindApproval {
		if reason == "" {
			reason = "The user dismissed the approval request without deciding."
		}
		return DeniedResultJSON(reason)
	}
	if reason == "" {
		reason = "The user dismissed this request without answering."
	}
	b, _ := json.Marshal(tool.Soft(tool.ErrorCodeCancelled, reason))
	return string(b)
}
