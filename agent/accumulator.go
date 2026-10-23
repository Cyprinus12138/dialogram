package agent

import (
	"encoding/json"
	"strings"
)

// Accumulator folds a stream of events into the state a server needs once
// the stream ends: the neutral Turn sequence to persist, whether the loop
// paused (and on what), and the final usage counters. Emission order is
// single-threaded (providers call emit serially), so no locking is needed.
type Accumulator struct {
	// Text is the concatenated text deltas — a diagnostic signal ("did we
	// emit anything before the stream broke?"), not the reply of record.
	Text strings.Builder
	// Turns are the assistant and tool turns in order.
	Turns []Turn
	// Paused is set by a waiting_for_user event; the Pause* fields carry
	// what the event said.
	Paused          bool
	PausedKind      string
	PausedToolUseID string
	PausedToolName  string
	PausedToolInput json.RawMessage
	PausedPrompt    string
	// Usage counters track the LAST seen usage event. Providers emit usage
	// cumulatively per turn, so the last value is the per-turn total.
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
}

// Observe folds one event into the accumulator.
func (a *Accumulator) Observe(ev StreamEvent) {
	switch ev.Type {
	case EventText:
		a.Text.WriteString(ev.Delta)
	case EventTurn:
		if ev.Turn != nil {
			a.Turns = append(a.Turns, *ev.Turn)
		}
	case EventUsage:
		if ev.InputTokens > 0 {
			a.InputTokens = ev.InputTokens
		}
		if ev.OutputTokens > 0 {
			a.OutputTokens = ev.OutputTokens
		}
		if ev.CacheReadTokens > 0 {
			a.CacheReadTokens = ev.CacheReadTokens
		}
		if ev.CacheCreationTokens > 0 {
			a.CacheCreationTokens = ev.CacheCreationTokens
		}
	case EventWaitingForUser:
		a.Paused = true
		a.PausedKind = ev.Kind
		a.PausedToolUseID = ev.ToolUseID
		a.PausedToolName = ev.Name
		a.PausedToolInput = ev.Input
		a.PausedPrompt = ev.Message
	}
}
