package eval

import (
	"fmt"

	"github.com/Cyprinus12138/dialogram/agent"
)

// Budget is a per-profile cost/latency envelope asserted over a completed
// turn. Mirror your production SLOs here so an eval failure predicts a real
// regression, not just a test nit.
type Budget struct {
	Name string
	// MaxSteps caps the number of upstream completions in one turn.
	MaxSteps int
	// MaxOutputTokens caps the (estimated) assistant reply length.
	MaxOutputTokens int
	// MinCacheReadRatio is the floor on cache_read / (cache_read +
	// cache_creation) for follow-up turns; 0 disables the check.
	MinCacheReadRatio float64
}

// BudgetReport is the outcome of checking a turn against a Budget.
type BudgetReport struct {
	Name           string   `json:"name"`
	Steps          int      `json:"steps"`
	OutputTokens   int      `json:"output_tokens"`
	CacheReadRatio float64  `json:"cache_read_ratio"`
	Violations     []string `json:"violations"`
}

// OK reports whether the turn stayed within budget.
func (r BudgetReport) OK() bool { return len(r.Violations) == 0 }

// CheckBudget asserts a completed ChatOutput against a Budget. isFollowUp
// selects whether the cache-read floor applies (a first turn always pays
// cache creation, so the ratio floor is meaningless there). Steps, output
// tokens and the ratio are computed by the same agent helpers the
// production metrics use, so eval thresholds and SLOs share one definition.
func CheckBudget(out *agent.ChatOutput, b Budget, isFollowUp bool) BudgetReport {
	rep := BudgetReport{
		Name:           b.Name,
		Steps:          out.ToolSteps(),
		OutputTokens:   agent.EstOutputTokens(out.Reply),
		CacheReadRatio: out.CacheReadRatio(),
	}
	if b.MaxSteps > 0 && rep.Steps > b.MaxSteps {
		rep.Violations = append(rep.Violations, fmt.Sprintf("steps %d > max %d", rep.Steps, b.MaxSteps))
	}
	if b.MaxOutputTokens > 0 && rep.OutputTokens > b.MaxOutputTokens {
		rep.Violations = append(rep.Violations, fmt.Sprintf("output ~%d tokens > max %d", rep.OutputTokens, b.MaxOutputTokens))
	}
	if isFollowUp && b.MinCacheReadRatio > 0 {
		// Only assert when the turn actually exercised the prompt cache —
		// some proxies report no cache counters at all.
		if out.CacheReadTokens+out.CacheCreationTokens > 0 && rep.CacheReadRatio < b.MinCacheReadRatio {
			rep.Violations = append(rep.Violations, fmt.Sprintf("cache-read ratio %.2f < min %.2f", rep.CacheReadRatio, b.MinCacheReadRatio))
		}
	}
	return rep
}
