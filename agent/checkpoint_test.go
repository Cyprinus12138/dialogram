package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Cyprinus12138/dialogram/tool"
)

func toolUse(id, name string) Block {
	return Block{Type: BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: json.RawMessage(`{}`)}
}

func toolResult(id, name string) Block {
	return Block{Type: BlockToolResult, ToolUseID: id, ToolName: name, ToolResult: json.RawMessage(`{"ok":true}`)}
}

func TestMemCheckpointer(t *testing.T) {
	m := NewMemCheckpointer()
	if _, ok := m.Load("m1"); ok {
		t.Fatal("empty checkpointer must miss")
	}
	first := Checkpoint{MessageID: "m1", Step: 1, Turns: []Turn{{Role: RoleAssistant, Blocks: []Block{toolUse("a", "x")}}}, Pending: []Block{toolUse("a", "x")}}
	if err := m.Save(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := Checkpoint{MessageID: "m1", Step: 1, Turns: append(first.Turns, Turn{Role: RoleTool, Blocks: []Block{toolResult("a", "x")}})}
	_ = m.Save(context.Background(), second)
	got, ok := m.Load("m1")
	if !ok || len(got.Turns) != 2 || len(got.Pending) != 0 {
		t.Fatalf("latest must win: ok=%v turns=%d pending=%d", ok, len(got.Turns), len(got.Pending))
	}
	// Load hands back copies.
	got.Turns = append(got.Turns, UserTurn("x"))
	if again, _ := m.Load("m1"); len(again.Turns) != 2 {
		t.Fatal("Load must not alias the stored slice")
	}
	m.Delete("m1")
	if _, ok := m.Load("m1"); ok {
		t.Fatal("Delete must forget the checkpoint")
	}
}

func TestDanglingToolUses(t *testing.T) {
	turns := []Turn{
		UserTurn("hi"),
		{Role: RoleAssistant, Blocks: []Block{toolUse("a", "x")}},
		{Role: RoleTool, Blocks: []Block{toolResult("a", "x")}},
		{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: "..."}, toolUse("b", "y"), toolUse("c", "z")}},
	}
	got := DanglingToolUses(turns)
	if len(got) != 2 || got[0].ToolUseID != "b" || got[1].ToolUseID != "c" {
		t.Fatalf("dangling = %+v", got)
	}
	if got := DanglingToolUses(turns[:3]); len(got) != 0 {
		t.Fatalf("fully paired history must have no dangling calls, got %+v", got)
	}
}

func TestCloseDangling(t *testing.T) {
	paired := []Turn{
		UserTurn("hi"),
		{Role: RoleAssistant, Blocks: []Block{toolUse("a", "x")}},
		{Role: RoleTool, Blocks: []Block{toolResult("a", "x")}},
		{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: "done"}}},
	}
	if got := CloseDangling(paired); len(got) != len(paired) || &got[0] != &paired[0] {
		t.Fatal("nothing dangling must return the input as is")
	}

	// A crash between the assistant checkpoint and the tool turn: every
	// regular call gets a RUN_INTERRUPTED result in a new tool turn.
	interrupted := append(paired[:3:3], Turn{Role: RoleAssistant, Blocks: []Block{toolUse("b", "y"), toolUse("c", "z")}})
	got := CloseDangling(interrupted)
	if len(got) != 5 || got[4].Role != RoleTool || len(got[4].Blocks) != 2 {
		t.Fatalf("expected a synthetic tool turn with two results, got %+v", got)
	}
	for i, id := range []string{"b", "c"} {
		b := got[4].Blocks[i]
		var sf tool.SoftFailure
		if b.ToolUseID != id || json.Unmarshal(b.ToolResult, &sf) != nil || sf.ErrorCode != tool.ErrorCodeRunInterrupted || !sf.Retryable {
			t.Fatalf("synthetic result %d wrong: %+v (%s)", i, b, b.ToolResult)
		}
	}
	if len(interrupted) != 4 || len(DanglingToolUses(got)) != 0 {
		t.Fatal("input must be untouched and the output fully paired")
	}

	// Built-in pause tools stay pending; a co-requested regular call is
	// closed.
	mixed := append(paired[:3:3], Turn{Role: RoleAssistant, Blocks: []Block{toolUse("p", tool.HITLInputToolName), toolUse("r", "regular")}})
	got = CloseDangling(mixed)
	if len(got) != 5 || len(got[4].Blocks) != 1 || got[4].Blocks[0].ToolUseID != "r" {
		t.Fatalf("pause must stay pending, regular closed: %+v", got)
	}
	pure := append(paired[:3:3], Turn{Role: RoleAssistant, Blocks: []Block{toolUse("p", tool.HITLChoiceToolName)}})
	if got := CloseDangling(pure); len(got) != 4 {
		t.Fatalf("a pure pause turn must be left alone, got %d turns", len(got))
	}

	// Custom pause tools are only known to the Config.
	custom := append(paired[:3:3], Turn{Role: RoleAssistant, Blocks: []Block{toolUse("p", "propose_change")}})
	if got := CloseDangling(custom); len(got) != 5 {
		t.Fatal("the package-level helper cannot know a custom pause tool")
	}
	cfg := &Config{PauseTools: []string{"propose_change"}}
	if got := cfg.CloseDangling(custom); len(got) != 4 {
		t.Fatalf("Config.CloseDangling must exempt PauseTools, got %d turns", len(got))
	}
	var nilCfg *Config
	if got := nilCfg.CloseDangling(custom); len(got) != 5 {
		t.Fatal("nil Config must behave like the package-level helper")
	}

	// A half-written tool turn is completed in place rather than followed
	// by a second tool turn.
	partial := []Turn{
		UserTurn("hi"),
		{Role: RoleAssistant, Blocks: []Block{toolUse("a", "x"), toolUse("b", "y")}},
		{Role: RoleTool, Blocks: []Block{toolResult("a", "x")}},
	}
	got = CloseDangling(partial)
	if len(got) != 3 || len(got[2].Blocks) != 2 || got[2].Blocks[1].ToolUseID != "b" || len(partial[2].Blocks) != 1 {
		t.Fatalf("partial tool turn must be completed without mutating the input: %+v", got)
	}
	if !strings.Contains(RunInterruptedResultJSON("y"), tool.ErrorCodeRunInterrupted) {
		t.Fatal("RunInterruptedResultJSON wrong")
	}
}
