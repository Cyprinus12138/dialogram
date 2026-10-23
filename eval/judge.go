package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Rubric dimensions scored by the LLM-as-judge, each 0..5.
const (
	DimFaithfulness    = "faithfulness"     // claims are supported by tool_results
	DimCitationPresent = "citation_present" // source + window cited for figures
	DimConciseness     = "conciseness"      // within the response-length budget
	DimToolSelection   = "tool_selection"   // right tools, no needless fan-out
	DimHonestyOnEmpty  = "honesty_on_empty" // empty results handled honestly
	DimLanguageMatch   = "language_match"   // reply language matches the user
)

// RubricDimensions is the canonical ordered list of judged dimensions.
var RubricDimensions = []string{
	DimFaithfulness, DimCitationPresent, DimConciseness, DimToolSelection, DimHonestyOnEmpty, DimLanguageMatch,
}

// RubricScore is the judge's per-dimension scoring of one reply.
type RubricScore struct {
	Scores    map[string]int `json:"scores"`
	Rationale string         `json:"rationale"`
}

// Min returns the lowest score across dims, or -1 when none are present.
func (r RubricScore) Min(dims ...string) int {
	min := -1
	for _, d := range dims {
		v, ok := r.Scores[d]
		if !ok {
			continue
		}
		if min == -1 || v < min {
			min = v
		}
	}
	return min
}

// FloorFailures returns a message for each rubric floor the judged score is
// below (or the dimension was not scored at all).
func (r RubricScore) FloorFailures(floors map[string]int) []string {
	var fails []string
	keys := make([]string, 0, len(floors))
	for k := range floors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, dim := range keys {
		floor := floors[dim]
		got, ok := r.Scores[dim]
		if !ok {
			fails = append(fails, fmt.Sprintf("%s not scored (floor %d)", dim, floor))
			continue
		}
		if got < floor {
			fails = append(fails, fmt.Sprintf("%s = %d < floor %d", dim, got, floor))
		}
	}
	return fails
}

// ScenarioResult is one scenario's judged outcome in a report.
type ScenarioResult struct {
	ID            string         `json:"id"`
	Description   string         `json:"description,omitempty"`
	Scores        map[string]int `json:"scores,omitempty"`
	Rationale     string         `json:"rationale,omitempty"`
	FloorFailures []string       `json:"floor_failures,omitempty"`
	Passed        bool           `json:"passed"`
	// Errored marks a scenario the judge could not score at all — an infra
	// failure (upstream 429/5xx/timeout, malformed judge output), NOT an
	// eval regression. Counted in Errored, never Failed, so a judge outage
	// does not read as an agent-quality regression.
	Errored bool   `json:"errored,omitempty"`
	Error   string `json:"error,omitempty"`
}

// JudgeReport is the scored report a scheduled run writes to disk and
// uploads as an artifact. It carries the prompt fingerprint so a score
// baseline can be re-stamped deliberately when the prompt changes.
type JudgeReport struct {
	GeneratedAt       time.Time        `json:"generated_at"`
	Model             string           `json:"model"`
	PromptFingerprint string           `json:"prompt_fingerprint,omitempty"`
	Total             int              `json:"total"`
	Passed            int              `json:"passed"`
	Failed            int              `json:"failed"`
	Errored           int              `json:"errored"`
	Results           []ScenarioResult `json:"results"`
}

// Add appends a result and updates the rollup. An errored result counts
// only in Errored.
func (r *JudgeReport) Add(res ScenarioResult) {
	r.Results = append(r.Results, res)
	r.Total++
	switch {
	case res.Errored:
		r.Errored++
	case res.Passed:
		r.Passed++
	default:
		r.Failed++
	}
}

// WriteJSON writes the report as indented JSON, creating parent dirs.
func (r JudgeReport) WriteJSON(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Completer is the minimal model seam the judges depend on: one tool-free
// completion. Scheduled runs inject HTTPCompleter; unit tests inject a fake,
// so prompt building and score parsing are testable at zero cost.
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// Judge scores a produced reply against a scenario's rubric.
type Judge interface {
	Score(ctx context.Context, s Scenario, reply string, toolResults []string) (RubricScore, error)
}

// LLMJudge is the rubric judge backed by a Completer.
type LLMJudge struct {
	LLM Completer
	// SystemPrompt overrides DefaultJudgeSystemPrompt.
	SystemPrompt string
}

// Score builds the judge prompt, calls the model, and parses the rubric.
func (j *LLMJudge) Score(ctx context.Context, s Scenario, reply string, toolResults []string) (RubricScore, error) {
	if j.LLM == nil {
		return RubricScore{}, fmt.Errorf("LLMJudge.LLM is nil")
	}
	system, user := BuildJudgePrompt(s, reply, toolResults)
	if j.SystemPrompt != "" {
		system = j.SystemPrompt
	}
	out, err := j.LLM.Complete(ctx, system, user)
	if err != nil {
		return RubricScore{}, err
	}
	return ParseRubricScore(out)
}

// DefaultJudgeSystemPrompt instructs the judge model. It is strict about
// output shape so ParseRubricScore can consume it; the dimensions mirror the
// rules an honest tool-using assistant's prompt enforces.
const DefaultJudgeSystemPrompt = `You are a strict evaluator of a tool-using assistant's reply.
Score the reply on each dimension from 0 (terrible) to 5 (excellent):
- faithfulness: every named entity and figure in the reply appears in the provided tool_results. Fabrication scores 0.
- citation_present: figures cite their source and time window where the tool results supply one.
- conciseness: the reply respects a short-answer budget (~250 tokens typical, 500 hard ceiling). Padding scores low.
- tool_selection: the assistant chose the right tools and avoided needless repeated calls.
- honesty_on_empty: if the tools returned nothing useful, the reply says so plainly and offers a next step instead of inventing data.
- language_match: the reply is written in the same language the user used.
Respond with ONLY a JSON object of the form:
{"scores":{"faithfulness":0-5,"citation_present":0-5,"conciseness":0-5,"tool_selection":0-5,"honesty_on_empty":0-5,"language_match":0-5},"rationale":"one or two sentences"}
Do not include any text outside the JSON object.`

// BuildJudgePrompt assembles the (system, user) prompt pair. Pure and
// deterministic.
func BuildJudgePrompt(s Scenario, reply string, toolResults []string) (system, user string) {
	var b strings.Builder
	b.WriteString("# Scenario\n")
	if s.Description != "" {
		b.WriteString(s.Description)
		b.WriteString("\n\n")
	}
	b.WriteString("# User message\n")
	b.WriteString(s.LastUserText())
	b.WriteString("\n\n# Tool results the assistant received this turn\n")
	if len(toolResults) == 0 {
		b.WriteString("(none)\n")
	}
	for i, tr := range toolResults {
		fmt.Fprintf(&b, "## result %d\n%s\n", i+1, tr)
	}
	b.WriteString("\n# Assistant reply to evaluate\n")
	b.WriteString(reply)
	return DefaultJudgeSystemPrompt, b.String()
}

// ParseRubricScore extracts the rubric JSON from a judge completion. Models
// sometimes wrap JSON in prose or code fences, so the first balanced
// top-level object is scanned for before decoding. Scores are clamped to
// 0..5.
func ParseRubricScore(out string) (RubricScore, error) {
	obj := extractFirstJSONObject(out)
	if obj == "" {
		return RubricScore{}, fmt.Errorf("no JSON object in judge output: %q", truncateForErr(out))
	}
	var rs RubricScore
	if err := json.Unmarshal([]byte(obj), &rs); err != nil {
		return RubricScore{}, fmt.Errorf("parse judge JSON: %w (obj=%s)", err, truncateForErr(obj))
	}
	if rs.Scores == nil {
		return RubricScore{}, fmt.Errorf("judge output has no scores: %q", truncateForErr(out))
	}
	for k, v := range rs.Scores {
		if v < 0 {
			rs.Scores[k] = 0
		} else if v > 5 {
			rs.Scores[k] = 5
		}
	}
	return rs, nil
}

func extractFirstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func truncateForErr(s string) string {
	const n = 200
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
