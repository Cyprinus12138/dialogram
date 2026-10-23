package loop

import "github.com/Cyprinus12138/dialogram/agent"

// MemorySeparator is the spacing between the stable system prompt and the
// per-turn volatile block. It is the model-visible join when the two are
// concatenated into one string (ComposeSystemPrompt) AND the leading
// spacing of the standalone volatile block on the Anthropic wire, so the
// model sees identical text either way.
const MemorySeparator = "\n\n"

// ComposeStableSystem returns the cache-stable portion of the system
// prompt: the profile's base prompt plus the skills summaries block. It
// deliberately EXCLUDES the per-turn MemoryContext.
//
// The volatile block drifts whenever application state changes. Anthropic
// caches by longest common prefix up to a cache_control breakpoint, so
// folding the volatile text into the cached system string would force
// cache creation of the entire prompt every turn it changes. Keeping it
// out lets the stable prefix stay cache-eligible turn to turn; callers emit
// the volatile block separately, un-cached.
//
// A nil/empty library collapses to the base prompt byte for byte.
func ComposeStableSystem(base string, lib *agent.SkillLibrary) string {
	summary := agent.SkillsSummary(lib)
	if summary == "" {
		return base
	}
	if base == "" {
		return summary
	}
	return base + "\n\n" + summary
}

// ComposeSystemPrompt returns the full system prompt as ONE string: the
// stable prefix followed by the volatile block. Empty inputs collapse
// cleanly. This is the canonical string for providers that carry the
// system prompt as a single message (OpenAI) and for token-breakdown
// estimation; the Anthropic provider splits it into two blocks instead.
func ComposeSystemPrompt(base string, lib *agent.SkillLibrary, memoryContext string) string {
	stable := ComposeStableSystem(base, lib)
	if memoryContext == "" {
		return stable
	}
	if stable == "" {
		return memoryContext
	}
	return stable + MemorySeparator + memoryContext
}
