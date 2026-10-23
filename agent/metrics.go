package agent

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the per-turn budget signals an Agent records: completed
// turns, tool steps per turn, output tokens per turn, and the prompt-cache
// read ratio. They mirror the budgets the eval package asserts, so an eval
// threshold breach predicts a real SLO regression rather than just failing a
// test in isolation.
//
// Metrics are opt-in: nothing is registered on any registry unless you
// construct a Metrics and hand it to agent.New. Labels are bounded (profile
// and provider are small fixed enums), so this is safe on a high-RPS path.
type Metrics struct {
	turnsTotal         *prometheus.CounterVec
	turnToolSteps      *prometheus.HistogramVec
	turnOutputTokens   *prometheus.HistogramVec
	turnCacheReadRatio *prometheus.HistogramVec
}

const (
	metricNamespace = "dialogram"
	metricSubsystem = "agent"
)

// NewMetrics builds the collectors and registers them on reg. A nil reg
// builds unregistered collectors (useful in tests). Registration errors
// (e.g. a second Metrics on the same registry) are returned.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		turnsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem, Name: "turns_total",
			Help: "Completed agent chat turns, by profile and provider.",
		}, []string{"profile", "provider"}),
		turnToolSteps: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem, Name: "turn_tool_steps",
			Help:    "Upstream completions (assistant turns) per chat turn — the step-cap budget signal.",
			Buckets: []float64{1, 2, 3, 4, 5, 6, 8, 10, 12},
		}, []string{"profile", "provider"}),
		turnOutputTokens: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem, Name: "turn_output_tokens",
			Help:    "Assistant reply length per chat turn — the output-token budget signal.",
			Buckets: []float64{50, 100, 200, 300, 400, 500, 750, 1000, 1500},
		}, []string{"profile", "provider"}),
		turnCacheReadRatio: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem, Name: "turn_cache_read_ratio",
			Help:    "cache_read / (cache_read + cache_creation) per chat turn. Only observed when the turn reported cache activity.",
			Buckets: prometheus.LinearBuckets(0, 0.1, 11),
		}, []string{"profile", "provider"}),
	}
	if reg != nil {
		for _, c := range []prometheus.Collector{m.turnsTotal, m.turnToolSteps, m.turnOutputTokens, m.turnCacheReadRatio} {
			if err := reg.Register(c); err != nil {
				return nil, err
			}
		}
	}
	return m, nil
}

// ObserveTurn records one completed chat turn's budget signals. Best-effort
// and side-effect-only; nil-safe. A turn with cacheRead+cacheCreation == 0
// reported no cache activity, so the ratio is left unobserved rather than
// recorded as a misleading 0.
func (m *Metrics) ObserveTurn(profile, provider string, toolSteps, outputTokens, cacheRead, cacheCreation int) {
	if m == nil {
		return
	}
	if profile == "" {
		profile = "default"
	}
	if provider == "" {
		provider = "unknown"
	}
	m.turnsTotal.WithLabelValues(profile, provider).Inc()
	m.turnToolSteps.WithLabelValues(profile, provider).Observe(float64(toolSteps))
	m.turnOutputTokens.WithLabelValues(profile, provider).Observe(float64(outputTokens))
	if denom := cacheRead + cacheCreation; denom > 0 {
		m.turnCacheReadRatio.WithLabelValues(profile, provider).Observe(float64(cacheRead) / float64(denom))
	}
}

// ToolSteps counts the upstream completions in this turn: one per assistant
// Turn. Interleaved tool turns are local execution, not model round-trips.
func (o *ChatOutput) ToolSteps() int {
	if o == nil {
		return 0
	}
	return CountAssistantTurns(o.Turns)
}

// CacheReadRatio is cache_read / (cache_read + cache_creation) for this
// turn, 0 when no cache activity was reported.
func (o *ChatOutput) CacheReadRatio() float64 {
	if o == nil {
		return 0
	}
	denom := o.CacheReadTokens + o.CacheCreationTokens
	if denom == 0 {
		return 0
	}
	return float64(o.CacheReadTokens) / float64(denom)
}

// CountAssistantTurns returns the number of assistant turns in a slice —
// the count of upstream completions in one chat turn.
func CountAssistantTurns(turns []Turn) int {
	n := 0
	for _, t := range turns {
		if t.Role == RoleAssistant {
			n++
		}
	}
	return n
}

// EstOutputTokens approximates a reply's length in tokens (~4 chars per
// token). Rough by design; the eval budgets and the metrics share this one
// definition so they agree.
func EstOutputTokens(reply string) int {
	if reply == "" {
		return 0
	}
	return (len(reply) + 3) / 4
}
