package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/Cyprinus12138/dialogram/tool"
)

// Checkpoint is the block IR of one run at a step boundary. The loop hands
// one to the Checkpointer after every assistant turn is finalised (Pending
// = that turn's tool_use blocks, if any) and again after the matching tool
// turn is appended (Pending empty). An application that loses the process
// mid-turn can then pick the conversation up from the last boundary: the
// stream buffer has the events the client saw, the checkpoint has the
// state the model needs.
type Checkpoint struct {
	// MessageID is ChatInput.MessageID — the run this checkpoint belongs to.
	MessageID string `json:"message_id"`
	// Step is the 1-based step within this run (one step = one upstream
	// completion plus its tool turn). Both boundaries of a step carry the
	// same number; the step-cap synthesis turn is MaxSteps+1.
	Step int `json:"step"`
	// Turns are the assistant and tool turns this run has produced so far,
	// in order. The input history is not repeated — the application has it.
	Turns []Turn `json:"turns"`
	// Pending are the tool_use blocks whose results are not yet in Turns:
	// the just-finalised assistant turn's calls, or nothing at a tool-turn
	// boundary. A pause tool stays pending until the user answers.
	Pending []Block `json:"pending,omitempty"`
	// At is when the checkpoint was taken.
	At time.Time `json:"at"`
}

// Checkpointer persists a Checkpoint. Save runs synchronously on the
// producer goroutine at every step boundary, so it should be one quick
// write keyed by MessageID, the latest checkpoint replacing the previous.
// The loop logs a Save error and carries on — checkpointing is a
// durability aid, never a correctness gate for the run.
//
// This is also the seam for durable-execution engines (Temporal, DBOS and
// the like): a Checkpointer that records each boundary as a workflow step
// hands those engines exactly the state they replay from.
type Checkpointer interface {
	Save(ctx context.Context, cp Checkpoint) error
}

// MemCheckpointer keeps the latest checkpoint per message id in memory.
// Meant for tests and single-process examples; it does not survive a
// restart, which is the one failure checkpoints exist for — production
// wires a Checkpointer over its database.
type MemCheckpointer struct {
	mu     sync.Mutex
	latest map[string]Checkpoint
}

// NewMemCheckpointer builds an empty MemCheckpointer.
func NewMemCheckpointer() *MemCheckpointer {
	return &MemCheckpointer{latest: map[string]Checkpoint{}}
}

// Save replaces the checkpoint stored for cp.MessageID.
func (m *MemCheckpointer) Save(_ context.Context, cp Checkpoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latest[cp.MessageID] = cp
	return nil
}

// Load returns the latest checkpoint for messageID. The slices are copies;
// callers may append to them freely.
func (m *MemCheckpointer) Load(messageID string) (Checkpoint, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp, ok := m.latest[messageID]
	if !ok {
		return Checkpoint{}, false
	}
	cp.Turns = append([]Turn(nil), cp.Turns...)
	cp.Pending = append([]Block(nil), cp.Pending...)
	return cp, true
}

// Delete forgets messageID — call it once the run's turns are persisted
// through the normal path so a finished run leaves nothing behind.
func (m *MemCheckpointer) Delete(messageID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.latest, messageID)
}

// DanglingToolUses returns, in order, every tool_use block in turns that
// has no tool_result with the same ToolUseID anywhere in turns. In a
// well-formed conversation these can only sit in the last assistant turn;
// they are what a Checkpoint reports as Pending.
func DanglingToolUses(turns []Turn) []Block {
	resolved := map[string]bool{}
	for _, t := range turns {
		for _, b := range t.Blocks {
			if b.Type == BlockToolResult {
				resolved[b.ToolUseID] = true
			}
		}
	}
	var out []Block
	for _, t := range turns {
		if t.Role != RoleAssistant {
			continue
		}
		for _, b := range t.Blocks {
			if b.Type == BlockToolUse && !resolved[b.ToolUseID] {
				out = append(out, b)
			}
		}
	}
	return out
}

// CloseDangling makes a checkpointed conversation resumable on either
// provider after a crash: every tool_use that never received a tool_result
// is paired with a synthetic RUN_INTERRUPTED soft failure, so the next
// request does not carry an unanswered call (both wire formats reject
// that). The synthetic results are appended to the tool turn that follows
// the owning assistant turn, or a new tool turn is inserted when there is
// none. The input is not mutated; when nothing is dangling it is returned
// as is.
//
// The built-in human-in-the-loop tools are left pending: an unanswered
// request_user_input is a legitimate pause the application resolves with
// the user's answer, not a lost result. Use Config.CloseDangling to extend
// that exemption to Config.PauseTools.
func CloseDangling(turns []Turn) []Turn {
	return closeDangling(turns, tool.IsHITLTool)
}

// CloseDangling is CloseDangling with the config's pause tools (built-in
// and Config.PauseTools) exempt. Nil-safe.
func (c *Config) CloseDangling(turns []Turn) []Turn {
	return closeDangling(turns, c.IsPauseTool)
}

func closeDangling(turns []Turn, isPause func(name string) bool) []Turn {
	resolved := map[string]bool{}
	for _, t := range turns {
		for _, b := range t.Blocks {
			if b.Type == BlockToolResult {
				resolved[b.ToolUseID] = true
			}
		}
	}
	var out []Turn
	changed := false
	for i := 0; i < len(turns); i++ {
		t := turns[i]
		out = append(out, t)
		if t.Role != RoleAssistant {
			continue
		}
		var synth []Block
		for _, b := range t.Blocks {
			if b.Type != BlockToolUse || resolved[b.ToolUseID] || (isPause != nil && isPause(b.ToolName)) {
				continue
			}
			synth = append(synth, Block{
				Type:       BlockToolResult,
				ToolUseID:  b.ToolUseID,
				ToolName:   b.ToolName,
				ToolResult: json.RawMessage(RunInterruptedResultJSON(b.ToolName)),
			})
		}
		if len(synth) == 0 {
			continue
		}
		changed = true
		if i+1 < len(turns) && turns[i+1].Role == RoleTool {
			tt := turns[i+1]
			tt.Blocks = append(append([]Block(nil), tt.Blocks...), synth...)
			out = append(out, tt)
			i++
			continue
		}
		out = append(out, Turn{Role: RoleTool, Blocks: synth})
	}
	if !changed {
		return turns
	}
	return out
}

// RunInterruptedResultJSON is the tool_result body CloseDangling pairs with
// a tool_use whose run died before the result was recorded. Retryable is
// set: the tool never completed, so calling it again is the right move.
func RunInterruptedResultJSON(toolName string) string {
	b, _ := json.Marshal(map[string]any{
		"error_code":    tool.ErrorCodeRunInterrupted,
		"error_message": "The run was interrupted before " + toolName + " returned a result. Call it again if the result is still needed.",
		"retryable":     true,
	})
	return string(b)
}
