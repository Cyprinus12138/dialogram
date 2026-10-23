package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Cyprinus12138/dialogram/agent"
)

// Scenario is one golden conversation fixture, stored as block IR so it is
// provider-agnostic and replays deterministically. It carries the
// conversation up to the user message under test, the canned JSON each
// tool returns during replay, and the assertions to apply to the reply.
type Scenario struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	// Profile names the agent.Profile the scenario runs under.
	Profile string `json:"profile"`
	// InputTurns is the history fed to the loop, ending with the user turn
	// being answered.
	InputTurns []agent.Turn `json:"input_turns"`
	// ToolResults maps a tool name to the deterministic JSON body it returns
	// whenever the model calls it during this scenario.
	ToolResults map[string]json.RawMessage `json:"tool_results,omitempty"`
	// RecordedReply is an optional golden reply captured from a prior run.
	// When present, CI runs the deterministic assertions against it at zero
	// cost; the scheduled path re-runs the loop and judges a fresh reply.
	RecordedReply string `json:"recorded_reply,omitempty"`
	// RecordedToolResults are the tool_result bodies that accompanied
	// RecordedReply, used to ground it. When empty, the values of
	// ToolResults are used instead.
	RecordedToolResults []json.RawMessage `json:"recorded_tool_results,omitempty"`
	// Expect is the set of assertions to run over the produced turn.
	Expect Expectations `json:"expect"`
}

// GroundingResults returns the tool_result bodies to ground the recorded
// reply against.
func (s Scenario) GroundingResults() []string {
	if len(s.RecordedToolResults) > 0 {
		out := make([]string, len(s.RecordedToolResults))
		for i, r := range s.RecordedToolResults {
			out[i] = string(r)
		}
		return out
	}
	keys := make([]string, 0, len(s.ToolResults))
	for k := range s.ToolResults {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, string(s.ToolResults[k]))
	}
	return out
}

// Expectations bundles the deterministic (free, CI) and judged (scheduled,
// costs money) assertions for a scenario.
type Expectations struct {
	// MustGround requires the no-fabrication checker to pass over the reply.
	MustGround bool `json:"must_ground,omitempty"`
	// ReplyMustContain / ReplyMustNotContain are case-insensitive substring
	// assertions encoding behaviours the prompt mandates deterministically.
	ReplyMustContain    []string `json:"reply_must_contain,omitempty"`
	ReplyMustNotContain []string `json:"reply_must_not_contain,omitempty"`
	// ExpectLanguage asserts the reply's dominant script: "zh" (contains
	// Han characters) or "en" (does not). Empty disables the check.
	ExpectLanguage string `json:"expect_language,omitempty"`
	// Budget names the Budget (see CheckBudget) to assert; empty disables.
	Budget string `json:"budget,omitempty"`
	// RubricFloors is the minimum acceptable judge score (0..5) per
	// dimension, consulted only on the judged path.
	RubricFloors map[string]int `json:"rubric_floors,omitempty"`
}

// LoadScenario reads and validates a single scenario JSON file.
func LoadScenario(path string) (Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Scenario{}, err
	}
	var s Scenario
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Scenario{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if err := s.validate(); err != nil {
		return Scenario{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return s, nil
}

// LoadScenarios loads every *.json scenario in dir, sorted by filename.
func LoadScenarios(dir string) ([]Scenario, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	out := make([]Scenario, 0, len(matches))
	for _, m := range matches {
		s, err := LoadScenario(m)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (s Scenario) validate() error {
	if strings.TrimSpace(s.ID) == "" {
		return fmt.Errorf("missing id")
	}
	if len(s.InputTurns) == 0 {
		return fmt.Errorf("scenario %s: input_turns is empty", s.ID)
	}
	if last := s.InputTurns[len(s.InputTurns)-1]; last.Role != agent.RoleUser {
		return fmt.Errorf("scenario %s: input_turns must end with a user turn, got %q", s.ID, last.Role)
	}
	for dim := range s.Expect.RubricFloors {
		if !ValidDimension(dim) {
			return fmt.Errorf("scenario %s: unknown rubric dimension %q", s.ID, dim)
		}
	}
	return nil
}

// ValidDimension reports whether d is a known rubric dimension.
func ValidDimension(d string) bool {
	for _, k := range RubricDimensions {
		if k == d {
			return true
		}
	}
	return false
}

// LastUserText returns the text of the final user turn — the prompt under
// test.
func (s Scenario) LastUserText() string {
	for i := len(s.InputTurns) - 1; i >= 0; i-- {
		if s.InputTurns[i].Role != agent.RoleUser {
			continue
		}
		var b strings.Builder
		for _, blk := range s.InputTurns[i].Blocks {
			if blk.Type == agent.BlockText {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// CheckDeterministic runs every zero-cost assertion in Expect over a reply
// and its tool_results, returning the failures (empty == pass).
func (s Scenario) CheckDeterministic(reply string, toolResults []string) []string {
	var fails []string
	low := strings.ToLower(reply)
	if s.Expect.MustGround {
		if rep := CheckGrounding(reply, toolResults); !rep.OK() {
			fails = append(fails, fmt.Sprintf("ungrounded: %+v", rep.Violations))
		}
	}
	for _, want := range s.Expect.ReplyMustContain {
		if !strings.Contains(low, strings.ToLower(want)) {
			fails = append(fails, fmt.Sprintf("reply missing required substring %q", want))
		}
	}
	for _, banned := range s.Expect.ReplyMustNotContain {
		if strings.Contains(low, strings.ToLower(banned)) {
			fails = append(fails, fmt.Sprintf("reply contains banned substring %q", banned))
		}
	}
	if s.Expect.ExpectLanguage != "" {
		if got := DetectLanguage(reply); got != s.Expect.ExpectLanguage {
			fails = append(fails, fmt.Sprintf("language = %q, want %q", got, s.Expect.ExpectLanguage))
		}
	}
	return fails
}

// DetectLanguage returns "zh" when the string contains Han characters,
// otherwise "en". Deliberately coarse — enough to catch the
// Chinese-in/Chinese-out language-match rule.
func DetectLanguage(s string) string {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return "zh"
		}
	}
	return "en"
}
