package eval

import (
	"strings"
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
)

var testBudget = Budget{Name: "assistant", MaxSteps: 6, MaxOutputTokens: 500, MinCacheReadRatio: 0.70}

func assistantTurn(text string) agent.Turn {
	return agent.Turn{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockText, Text: text}}}
}
func toolTurn() agent.Turn {
	return agent.Turn{Role: agent.RoleTool, Blocks: []agent.Block{{Type: agent.BlockToolResult, ToolUseID: "x", ToolResult: []byte(`{}`)}}}
}

func TestCheckBudget(t *testing.T) {
	var turns []agent.Turn
	for i := 0; i < testBudget.MaxSteps+1; i++ {
		turns = append(turns, assistantTurn("x"), toolTurn())
	}
	if rep := CheckBudget(&agent.ChatOutput{Turns: turns, Reply: "ok"}, testBudget, false); rep.OK() || rep.Steps != 7 {
		t.Fatalf("expected step-cap violation, got %+v", rep)
	}
	long := strings.Repeat("word ", 1200)
	if rep := CheckBudget(&agent.ChatOutput{Turns: []agent.Turn{assistantTurn(long)}, Reply: long}, testBudget, false); rep.OK() {
		t.Fatalf("expected output-token violation, got ~%d tokens", rep.OutputTokens)
	}
	low := &agent.ChatOutput{Turns: []agent.Turn{assistantTurn("hi")}, Reply: "hi", CacheReadTokens: 100, CacheCreationTokens: 900}
	if rep := CheckBudget(low, testBudget, false); !rep.OK() {
		t.Fatalf("turn-1 must not enforce the cache floor: %+v", rep.Violations)
	}
	if rep := CheckBudget(low, testBudget, true); rep.OK() {
		t.Fatalf("follow-up must flag ratio %.2f", rep.CacheReadRatio)
	}
	healthy := &agent.ChatOutput{Turns: []agent.Turn{assistantTurn("a"), toolTurn(), assistantTurn("done")}, Reply: "done", CacheReadTokens: 800, CacheCreationTokens: 100}
	if rep := CheckBudget(healthy, testBudget, true); !rep.OK() {
		t.Fatalf("healthy follow-up should pass, got %+v", rep)
	}
	if rep := CheckBudget(&agent.ChatOutput{Turns: []agent.Turn{assistantTurn("hi")}, Reply: "hi"}, testBudget, true); !rep.OK() {
		t.Fatalf("zero cache activity must not flag a ratio violation: %+v", rep.Violations)
	}
}
