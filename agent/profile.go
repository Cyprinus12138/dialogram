package agent

import (
	"github.com/Cyprinus12138/dialogram/tool"
)

// DefaultMaxToolSteps caps how many tool_use rounds a single turn may run
// before the loop makes one tool-free synthesis call and returns.
const DefaultMaxToolSteps = 12

// DefaultFinalSynthesisNudge is appended as a one-off user message when the
// step cap is reached, so the model wraps up from the data it already has
// instead of dead-ending.
const DefaultFinalSynthesisNudge = "You've reached the tool-call budget for this turn, so no further tools are available. " +
	"Do not ask to run more tools. Using only the results you've already gathered, give the user your best, " +
	"complete answer now. Briefly note any gaps where you'd have looked further, but lead with the substantive findings."

// Profile is one chat surface: a system prompt, the tools it may see and
// the budget it runs under. Applications define as many as they have
// surfaces (a consumer assistant, an operator copilot, a report writer) and
// select one per turn via ChatInput.Profile.
type Profile struct {
	// Name is the key the profile is selected by. Filled from the map key
	// by Config.Resolve when empty.
	Name string
	// SystemPrompt is the cache-stable base prompt. Skills summaries are
	// appended to it; the volatile ChatInput.MemoryContext is sent as a
	// separate block.
	SystemPrompt string
	// Tools is the allow-list of tool names this profile may see. nil means
	// every registered tool; an empty non-nil slice means none.
	Tools []string
	// MaxToolSteps caps upstream completions per turn; <= 0 means
	// DefaultMaxToolSteps.
	MaxToolSteps int
	// FinalSynthesisNudge overrides DefaultFinalSynthesisNudge.
	FinalSynthesisNudge string
}

// MaxSteps returns the effective step cap.
func (p Profile) MaxSteps() int {
	if p.MaxToolSteps <= 0 {
		return DefaultMaxToolSteps
	}
	return p.MaxToolSteps
}

// Nudge returns the effective synthesis nudge.
func (p Profile) Nudge() string {
	if p.FinalSynthesisNudge == "" {
		return DefaultFinalSynthesisNudge
	}
	return p.FinalSynthesisNudge
}

// Fingerprint is the content hash of the stable system prompt — the unit of
// prompt copy whose change is a prompt regression. The eval harness pins it
// so a score move can be attributed to a prompt edit rather than a tool or
// data change.
func (p Profile) Fingerprint() string { return PromptFingerprint(p.SystemPrompt) }

// Config holds the profiles and the pause-tool policy shared by every turn.
// A nil *Config is valid and behaves like an empty one: a single unnamed
// profile with no system prompt and every tool visible.
type Config struct {
	// Profiles by name.
	Profiles map[string]Profile
	// DefaultProfile is used when ChatInput.Profile is empty or unknown.
	// When empty, the zero Profile is used.
	DefaultProfile string
	// PauseTools are application-defined tools that, like the built-in
	// human-in-the-loop tools, end the assistant turn and wait for the user
	// (e.g. "propose this change, let the user accept or skip"). The loop
	// never dispatches them; it emits a waiting_for_user event of
	// PauseKindCustom carrying the tool name and input.
	PauseTools []string
}

// Resolve returns the profile for name, falling back to DefaultProfile and
// then to the zero profile. Nil-safe.
func (c *Config) Resolve(name string) Profile {
	if c == nil || len(c.Profiles) == 0 {
		return Profile{Name: name}
	}
	if p, ok := c.Profiles[name]; ok {
		if p.Name == "" {
			p.Name = name
		}
		return p
	}
	if p, ok := c.Profiles[c.DefaultProfile]; ok {
		if p.Name == "" {
			p.Name = c.DefaultProfile
		}
		return p
	}
	return Profile{Name: name}
}

// IsPauseTool reports whether name ends the assistant turn by pausing for
// the user: one of the built-in HITL tools or a Config.PauseTools entry.
// Nil-safe.
func (c *Config) IsPauseTool(name string) bool {
	if tool.IsHITLTool(name) {
		return true
	}
	if c == nil {
		return false
	}
	for _, n := range c.PauseTools {
		if n == name {
			return true
		}
	}
	return false
}

// ToolAllow computes the per-turn tool allow-list: the profile's list
// intersected with ChatInput.Tools. nil means every registered tool.
func (c *Config) ToolAllow(in ChatInput) []string {
	p := c.Resolve(in.Profile)
	switch {
	case p.Tools == nil && in.Tools == nil:
		return nil
	case p.Tools == nil:
		return append([]string(nil), in.Tools...)
	case in.Tools == nil:
		return append([]string(nil), p.Tools...)
	}
	set := make(map[string]bool, len(p.Tools))
	for _, n := range p.Tools {
		set[n] = true
	}
	out := make([]string, 0, len(in.Tools))
	for _, n := range in.Tools {
		if set[n] {
			out = append(out, n)
		}
	}
	return out
}
