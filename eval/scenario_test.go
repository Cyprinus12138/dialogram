package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
)

const (
	scenarioDir   = "testdata/scenarios"
	transcriptDir = "testdata/transcripts"
)

// Every shipped scenario must load and validate; a malformed fixture fails
// the suite at zero cost.
func TestLoadScenarios_AllValid(t *testing.T) {
	scs, err := LoadScenarios(scenarioDir)
	if err != nil {
		t.Fatalf("load scenarios: %v", err)
	}
	if len(scs) < 3 {
		t.Fatalf("want at least 3 golden scenarios, got %d", len(scs))
	}
	seen := map[string]bool{}
	for _, s := range scs {
		if seen[s.ID] {
			t.Errorf("duplicate scenario id %q", s.ID)
		}
		seen[s.ID] = true
	}
	// Unknown fields and bad shapes are rejected.
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"id":"x","input_turns":[{"role":"assistant","blocks":[]}],"bogus":1}`), 0o644)
	if _, err := LoadScenario(bad); err == nil {
		t.Error("unknown field must fail")
	}
	_ = os.WriteFile(bad, []byte(`{"id":"x","input_turns":[{"role":"assistant","blocks":[]}]}`), 0o644)
	if _, err := LoadScenario(bad); err == nil {
		t.Error("history not ending in a user turn must fail")
	}
	_ = os.WriteFile(bad, []byte(`{"id":"x","input_turns":[{"role":"user","blocks":[]}],"expect":{"rubric_floors":{"nope":1}}}`), 0o644)
	if _, err := LoadScenario(bad); err == nil {
		t.Error("unknown rubric dimension must fail")
	}
}

// Every scenario that ships a recorded golden reply must pass its own
// deterministic assertions — the zero-cost regression gate.
func TestScenarios_RecordedRepliesPassDeterministic(t *testing.T) {
	scs, err := LoadScenarios(scenarioDir)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, s := range scs {
		if s.RecordedReply == "" {
			continue
		}
		checked++
		if fails := s.CheckDeterministic(s.RecordedReply, s.GroundingResults()); len(fails) > 0 {
			t.Errorf("scenario %s recorded reply failed:\n  - %s", s.ID, strings.Join(fails, "\n  - "))
		}
	}
	if checked == 0 {
		t.Fatal("no scenario shipped a recorded reply; CI has no deterministic teeth")
	}
}

func TestCheckDeterministic_Failures(t *testing.T) {
	s := Scenario{Expect: Expectations{MustGround: true, ReplyMustContain: []string{"widen"}, ReplyMustNotContain: []string{"roughly"}, ExpectLanguage: "zh"}}
	fails := s.CheckDeterministic("Roughly $9,999 is the fare.", []string{`{"fare":58}`})
	if len(fails) != 4 {
		t.Fatalf("want 4 failures (grounding, missing, banned, language), got %d: %v", len(fails), fails)
	}
}

// A known-good transcript is fully grounded; a known-bad one is flagged.
func TestTranscripts_GroundingCorpus(t *testing.T) {
	all, err := LoadTranscripts(transcriptDir)
	if err != nil {
		t.Fatal(err)
	}
	good := all["grounded_flight_brief.json"]
	if good == nil {
		t.Fatal("missing grounded_flight_brief.json")
	}
	if rep := CheckTranscript(good); !rep.OK() {
		t.Fatalf("grounded transcript flagged: %+v", rep.Violations)
	}
	bad := all["fabricated_fare.json"]
	if bad == nil {
		t.Fatal("missing fabricated_fare.json")
	}
	rep := CheckTranscript(bad)
	if rep.OK() {
		t.Fatalf("fabricated transcript not flagged; entities=%v figures=%v", rep.Entities, rep.Figures)
	}
	var gotEntity, gotFigure bool
	for _, v := range rep.Violations {
		if v.Kind == ViolationEntity && strings.Contains(v.Value, "AEROVISTA") {
			gotEntity = true
		}
		if v.Kind == ViolationFigure && v.Norm == "1150" {
			gotFigure = true
		}
	}
	if !gotEntity || !gotFigure {
		t.Fatalf("expected AEROVISTA and 1,150 violations, got %+v", rep.Violations)
	}
}

func TestDetectLanguageAndGroundingFallback(t *testing.T) {
	if DetectLanguage("你预算内 0 班") != "zh" || DetectLanguage("SKYLINE at €58") != "en" {
		t.Fatal("language detection wrong")
	}
	s, err := LoadScenario(filepath.Join(scenarioDir, "weather_grounded_zh.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.GroundingResults()) != 1 || s.LastUserText() == "" {
		t.Fatal("expected fallback to tool_results values and a user text")
	}
}

// Scheduled LLM-as-judge run. Gated behind RUN_LLM_EVALS so the default run
// stays free; judges every scenario's recorded reply against its rubric
// floors with an OpenAI-compatible judge (OPENAI_API_KEY / OPENAI_BASE_URL /
// EVAL_JUDGE_MODEL or OPENAI_MODEL) and writes EVAL_REPORT_PATH.
func TestNightly_JudgeScenarios(t *testing.T) {
	if os.Getenv("RUN_LLM_EVALS") == "" {
		t.Skip("set RUN_LLM_EVALS=1 to run the LLM-as-judge (costs money)")
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY not set; cannot run the judge")
	}
	model := firstEnv("EVAL_JUDGE_MODEL", "OPENAI_MODEL")
	judge := &LLMJudge{LLM: &HTTPCompleter{BaseURL: os.Getenv("OPENAI_BASE_URL"), APIKey: apiKey, Model: model,
		OmitTemperature: os.Getenv("EVAL_JUDGE_OMIT_TEMPERATURE") != ""}}
	scs, err := LoadScenarios(scenarioDir)
	if err != nil {
		t.Fatal(err)
	}
	report := JudgeReport{GeneratedAt: time.Now().UTC(), Model: model}
	for _, s := range scs {
		if len(s.Expect.RubricFloors) == 0 || s.RecordedReply == "" {
			continue
		}
		s := s
		t.Run(s.ID, func(t *testing.T) {
			rs, err := judge.Score(context.Background(), s, s.RecordedReply, s.GroundingResults())
			if err != nil {
				// A judge outage is infra, not a regression: record as
				// errored and skip rather than fail.
				report.Add(ScenarioResult{ID: s.ID, Description: s.Description, Errored: true, Error: err.Error()})
				t.Skipf("judge unavailable, scenario not scored: %v", err)
			}
			fails := rs.FloorFailures(s.Expect.RubricFloors)
			report.Add(ScenarioResult{ID: s.ID, Description: s.Description, Scores: rs.Scores, Rationale: rs.Rationale, FloorFailures: fails, Passed: len(fails) == 0})
			t.Logf("scores=%+v rationale=%q", rs.Scores, rs.Rationale)
			if len(fails) > 0 {
				t.Errorf("rubric floor failures: %s", strings.Join(fails, "; "))
			}
		})
	}
	if path := os.Getenv("EVAL_REPORT_PATH"); path != "" {
		if err := report.WriteJSON(path); err != nil {
			t.Errorf("write eval report: %v", err)
		}
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

var _ = agent.UserTurn
