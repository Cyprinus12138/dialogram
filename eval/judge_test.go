package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
)

type fakeCompleter struct {
	reply     string
	gotSystem string
	gotUser   string
	err       error
}

func (f *fakeCompleter) Complete(_ context.Context, system, user string) (string, error) {
	f.gotSystem, f.gotUser = system, user
	return f.reply, f.err
}

func oneUserTurn(text string) []agent.Turn { return []agent.Turn{agent.UserTurn(text)} }

func TestBuildJudgePrompt(t *testing.T) {
	s := Scenario{Description: "test scenario", InputTurns: oneUserTurn("cheapest flight to porto?")}
	system, user := BuildJudgePrompt(s, "SKYLINE €58.", []string{`{"carrier":"SKYLINE"}`})
	if !strings.Contains(system, "faithfulness") {
		t.Error("judge system prompt must enumerate rubric dimensions")
	}
	for _, want := range []string{"cheapest flight to porto?", "SKYLINE €58.", `{"carrier":"SKYLINE"}`} {
		if !strings.Contains(user, want) {
			t.Errorf("judge user prompt missing %q", want)
		}
	}
	if _, u := BuildJudgePrompt(s, "x", nil); !strings.Contains(u, "(none)") {
		t.Error("no tool results must render (none)")
	}
}

func TestParseRubricScore(t *testing.T) {
	rs, err := ParseRubricScore(`{"scores":{"faithfulness":5,"conciseness":4},"rationale":"good"}`)
	if err != nil || rs.Scores[DimFaithfulness] != 5 || rs.Rationale != "good" {
		t.Fatalf("plain parse: %+v %v", rs, err)
	}
	rs, err = ParseRubricScore("Sure:\n```json\n{\"scores\":{\"faithfulness\":3}, \"rationale\":\"meh\"}\n```\nHope that helps!")
	if err != nil || rs.Scores[DimFaithfulness] != 3 {
		t.Fatalf("fenced parse: %+v %v", rs, err)
	}
	rs, _ = ParseRubricScore(`{"scores":{"faithfulness":9,"conciseness":-2}}`)
	if rs.Scores[DimFaithfulness] != 5 || rs.Scores[DimConciseness] != 0 {
		t.Fatalf("clamp wrong: %+v", rs.Scores)
	}
	if _, err := ParseRubricScore("no json here"); err == nil {
		t.Error("missing JSON must error")
	}
	if _, err := ParseRubricScore(`{"rationale":"no scores"}`); err == nil {
		t.Error("absent scores must error")
	}
	if rs.Min(DimFaithfulness, DimConciseness) != 0 || (RubricScore{}).Min("x") != -1 {
		t.Error("Min wrong")
	}
}

func TestRubricScore_FloorFailures(t *testing.T) {
	rs := RubricScore{Scores: map[string]int{DimFaithfulness: 5, DimToolSelection: 2}}
	fails := rs.FloorFailures(map[string]int{DimFaithfulness: 5, DimToolSelection: 4, DimConciseness: 3})
	if len(fails) != 2 {
		t.Fatalf("want 2 floor failures, got %d: %v", len(fails), fails)
	}
}

func TestLLMJudge(t *testing.T) {
	f := &fakeCompleter{reply: `{"scores":{"faithfulness":5,"honesty_on_empty":4,"language_match":5}}`}
	j := &LLMJudge{LLM: f}
	rs, err := j.Score(context.Background(), Scenario{InputTurns: oneUserTurn("预算 40")}, "你预算内 0 班...", []string{`{"n_in_budget":0}`})
	if err != nil || rs.Min(DimFaithfulness, DimHonestyOnEmpty, DimLanguageMatch) != 4 {
		t.Fatalf("score: %+v %v", rs, err)
	}
	if !strings.Contains(f.gotUser, "你预算内 0 班") || f.gotSystem != DefaultJudgeSystemPrompt {
		t.Error("completer did not receive the expected prompts")
	}
	custom := &LLMJudge{LLM: f, SystemPrompt: "CUSTOM"}
	_, _ = custom.Score(context.Background(), Scenario{InputTurns: oneUserTurn("x")}, "y", nil)
	if f.gotSystem != "CUSTOM" {
		t.Error("SystemPrompt override not applied")
	}
	if _, err := (&LLMJudge{}).Score(context.Background(), Scenario{}, "x", nil); err == nil {
		t.Error("nil LLM must error")
	}
}

func TestJudgeReport(t *testing.T) {
	var r JudgeReport
	r.Add(ScenarioResult{ID: "pass-1", Passed: true})
	r.Add(ScenarioResult{ID: "fail-1", FloorFailures: []string{"faithfulness = 2 < floor 4"}})
	r.Add(ScenarioResult{ID: "err-1", Errored: true, Error: "judge 429"})
	if r.Total != 3 || r.Passed != 1 || r.Failed != 1 || r.Errored != 1 {
		t.Fatalf("rollup = %d/%d/%d/%d", r.Total, r.Passed, r.Failed, r.Errored)
	}
	path := filepath.Join(t.TempDir(), "nested", "report.json")
	if err := r.WriteJSON(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var got JudgeReport
	if err := json.Unmarshal(b, &got); err != nil || got.Total != 3 || got.Results[0].ID != "pass-1" {
		t.Fatalf("round-trip mismatch: %+v %v", got, err)
	}
}
