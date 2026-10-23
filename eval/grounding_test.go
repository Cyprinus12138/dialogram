package eval

import (
	"encoding/json"
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
)

func TestCheckGrounding_GroundedReplyPasses(t *testing.T) {
	results := []string{`{"flights":[{"carrier":"SKYLINE","fare":89},{"carrier":"IBERWING","fare":104}],"n":9,"window":"searched 2026-05-08"}`}
	rep := CheckGrounding("SKYLINE at €89 and IBERWING at €104 (searched 2026-05-08 · n=9).", results)
	if !rep.OK() {
		t.Fatalf("expected grounded reply to pass, violations=%+v", rep.Violations)
	}
}

func TestCheckGrounding_FabricatedFigureAndEntityFail(t *testing.T) {
	results := []string{`{"flights":[{"carrier":"SKYLINE","fare":89}]}`}
	rep := CheckGrounding("SKYLINE is €89; AEROVISTA has one for about $1,150.", results)
	if rep.OK() {
		t.Fatalf("expected violations, got none; figures=%v entities=%v", rep.Figures, rep.Entities)
	}
	var gotFigure, gotEntity bool
	for _, v := range rep.Violations {
		if v.Kind == ViolationFigure && v.Norm == "1150" {
			gotFigure = true
		}
		if v.Kind == ViolationEntity && v.Value == "AEROVISTA" {
			gotEntity = true
		}
	}
	if !gotFigure || !gotEntity {
		t.Fatalf("missing expected violations: %+v", rep.Violations)
	}
}

func TestCheckGrounding_StopwordsRangesEnvelope(t *testing.T) {
	results := []string{`{"flights":[{"carrier":"SKYLINE","low":89,"high":104}],"codes":["Q1","D07"],"n":9}`}
	// Acronyms, quarter / short codes and citation prose must not be flagged.
	if rep := CheckGrounding("Per the API, Q1 fares: SKYLINE €89–104 (n=9). OK?", results); !rep.OK() {
		t.Fatalf("citation prose wrongly flagged: %+v", rep.Violations)
	}
	// Both range endpoints are checked.
	if rep := CheckGrounding("SKYLINE: €89–950.", results); rep.OK() {
		t.Fatal("ungrounded range endpoint must be flagged")
	}
	// Custom stopwords and the no-entity switch.
	opts := GroundingOptions{Stopwords: []string{"AEROVISTA"}}
	if rep := CheckGroundingWith("AEROVISTA flies there.", results, opts); !rep.OK() {
		t.Fatal("custom stopword must suppress the entity")
	}
	if rep := CheckGroundingWith("AEROVISTA flies there.", results, GroundingOptions{NoEntityCheck: true}); !rep.OK() {
		t.Fatal("NoEntityCheck must disable entity checking")
	}
	// Only model_observation grounds a claim.
	env := `{"model_observation":{"flights":[{"carrier":"SKYLINE"}]},"runtime_state":{"secret_fare":4321}}`
	if rep := CheckGrounding("SKYLINE is $4,321.", []string{env}); rep.OK() {
		t.Fatal("figure present only in runtime_state must not ground the claim")
	}
	if !CheckGrounding("   ", results).OK() {
		t.Fatal("empty reply must pass")
	}
}

func TestCheckTranscript_ScopesToLastTurn(t *testing.T) {
	turns := []agent.Turn{
		agent.UserTurn("flights to madrid"),
		{Role: agent.RoleTool, Blocks: []agent.Block{{Type: agent.BlockToolResult, ToolUseID: "a", ToolName: "search_flights", ToolResult: json.RawMessage(`{"flights":[{"carrier":"SKYLINE","fare":89}]}`)}}},
		{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockText, Text: "SKYLINE at €89."}}},
		agent.UserTurn("and porto?"),
		{Role: agent.RoleTool, Blocks: []agent.Block{{Type: agent.BlockToolResult, ToolUseID: "b", ToolName: "search_flights", ToolResult: json.RawMessage(`{"flights":[{"carrier":"IBERWING","fare":58}]}`)}}},
		{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockText, Text: "SKYLINE at €89 is still your best bet."}}},
	}
	if rep := CheckTranscript(turns); rep.OK() {
		t.Fatalf("cross-turn figure/entity must not be grounded; entities=%v figures=%v", rep.Entities, rep.Figures)
	}
	reply, results := LastTurnReplyAndResults(turns)
	if reply == "" || len(results) != 1 {
		t.Fatalf("scope wrong: reply=%q results=%d", reply, len(results))
	}
}
