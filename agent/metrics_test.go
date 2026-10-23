package agent

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestCountAssistantTurnsAndOutputHelpers(t *testing.T) {
	turns := []Turn{{Role: RoleAssistant}, {Role: RoleTool}, {Role: RoleAssistant}, {Role: RoleUser}}
	if got := CountAssistantTurns(turns); got != 2 {
		t.Fatalf("CountAssistantTurns = %d, want 2", got)
	}
	var nilOut *ChatOutput
	if nilOut.ToolSteps() != 0 || nilOut.CacheReadRatio() != 0 {
		t.Fatal("nil ChatOutput helpers must be zero-valued")
	}
	out := &ChatOutput{Turns: turns, CacheReadTokens: 700, CacheCreationTokens: 300}
	if out.ToolSteps() != 2 || out.CacheReadRatio() != 0.7 {
		t.Fatalf("helpers wrong: steps=%d ratio=%v", out.ToolSteps(), out.CacheReadRatio())
	}
	if (&ChatOutput{}).CacheReadRatio() != 0 {
		t.Fatal("no cache activity must give ratio 0, not NaN")
	}
	if EstOutputTokens("") != 0 || EstOutputTokens("abcd") != 1 || EstOutputTokens("abcde") != 2 {
		t.Fatal("EstOutputTokens must be ~4 chars/token, rounded up")
	}
}

func TestMetrics_RegisterAndObserve(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	if _, err := NewMetrics(reg); err == nil {
		t.Fatal("second registration on the same registry must fail")
	}
	var nilM *Metrics
	nilM.ObserveTurn("p", "x", 1, 1, 1, 1) // must not panic

	m.ObserveTurn("assistant", "openai", 3, 420, 700, 300)
	m.ObserveTurn("assistant", "openai", 1, 50, 0, 0)
	m.ObserveTurn("", "", 0, 0, 0, 0)

	if got := counterValue(t, m.turnsTotal.WithLabelValues("assistant", "openai")); got != 2 {
		t.Fatalf("turns_total = %v, want 2", got)
	}
	if got := histCount(t, m.turnToolSteps.WithLabelValues("assistant", "openai")); got != 2 {
		t.Fatalf("turn_tool_steps samples = %d, want 2", got)
	}
	// The cache-ratio histogram is observed ONLY for the cache-active turn.
	if got := histCount(t, m.turnCacheReadRatio.WithLabelValues("assistant", "openai")); got != 1 {
		t.Fatalf("cache_read_ratio samples = %d, want 1", got)
	}
	if got := counterValue(t, m.turnsTotal.WithLabelValues("default", "unknown")); got != 1 {
		t.Fatalf("empty labels must map to default/unknown, got %v", got)
	}
	// The collectors are exported through the registry under the dialogram namespace.
	fams, _ := reg.Gather()
	found := false
	for _, f := range fams {
		if f.GetName() == "dialogram_agent_turns_total" {
			found = true
		}
	}
	if !found {
		t.Fatal("dialogram_agent_turns_total not gathered from the registry")
	}
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var dm dto.Metric
	if err := c.Write(&dm); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	return dm.GetCounter().GetValue()
}

func histCount(t *testing.T, o prometheus.Observer) uint64 {
	t.Helper()
	m, ok := o.(prometheus.Metric)
	if !ok {
		t.Fatalf("observer %T is not a prometheus.Metric", o)
	}
	var dm dto.Metric
	if err := m.Write(&dm); err != nil {
		t.Fatalf("write histogram: %v", err)
	}
	return dm.GetHistogram().GetSampleCount()
}
