package agent

import (
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
)

// Guard is the turn-level honesty guard.
//
// Prompt instruction alone does not stop a model from reciting a regulated
// figure from memory under pressure — it displaces the fabrication to
// whatever phrasing the prompt did not enumerate. Engineering has to own the
// trust line: on a turn that produced NO successful grounding tool call, an
// answer that nonetheless states a figure matching one of the Signatures is
// treated as fabricated and the span is replaced with an honest placeholder.
//
// Two lessons from measuring a first version of this guard in production
// are built in:
//
//   - Grounding means a SUCCESSFUL result, not a call. A tool whose output
//     carries error_code / soft_failure vouches for nothing; the model
//     stating a figure after a soft-failed lookup is the single largest
//     leak class, and a name-only check cannot see it (ResultIsSuccess).
//   - Restating a number a tool actually produced is honest. Every numeric
//     token of at least MinRestatementDigits digits found in a tool result
//     or tool input earlier in the conversation is exempt from redaction.
//
// The guard is pure and unit-testable; the policy of *where* it runs is
// Agent's (Agent.Chat redacts the reply and the persisted turns; ChatStream
// holds text deltas and re-emits them redacted at each assistant turn
// boundary). Install it through Options.Guard; a nil Guard means no
// guarding.
type Guard struct {
	// GroundingTools are the tools whose SUCCESSFUL result legitimately
	// backs a figure. A figure cited on a turn that called any of these
	// (successfully) is grounded and is NOT redacted. The safe failure
	// direction is over-redaction (visible), never a leak (silent), so an
	// unknown tool does not count.
	GroundingTools map[string]bool
	// KindGrounding optionally narrows grounding per signature kind: when a
	// kind has an entry here, only THOSE tools vouch for figures of that
	// kind (e.g. an "entity_fact" kind grounded only by a profile lookup,
	// not by any numeric tool). Kinds without an entry use GroundingTools.
	KindGrounding map[string]map[string]bool
	// Signatures are the regulated-figure detectors. Each should be
	// high-precision so the guard fires on a genuine fabricated figure and
	// stays quiet on ordinary prose. DefaultSignatures covers currency
	// amounts and percentages.
	Signatures []Signature
	// Placeholder replaces a redacted span when the matching Signature has
	// none of its own. Defaults to DefaultPlaceholder.
	Placeholder string
	// MinRestatementDigits is the minimum digit count for a number harvested
	// from tool results / inputs to count as "a figure a tool produced" for
	// the restatement exemption. Defaults to 2. Lower it to 1 to exempt every
	// number a tool ever emitted (at the cost of single-digit percentages
	// surviving on a no-tool turn); raise it to 3 to exempt only amounts.
	MinRestatementDigits int
	// Logger receives one Warn per redacted turn. nil uses slog.Default().
	Logger *slog.Logger
}

// Signature is one regulated-figure detector.
type Signature struct {
	// Kind labels the figure class ("money", "percent", …); reported on
	// Flag and used to look up KindGrounding.
	Kind string
	// Pattern matches the span to redact. Keep it tight: the whole match is
	// replaced by the placeholder.
	Pattern *regexp.Regexp
	// Placeholder overrides Guard.Placeholder for this signature.
	Placeholder string
}

// DefaultPlaceholder is the honest, first-person text that replaces an
// un-grounded figure. It offers the real path forward, so a redaction reads
// as the assistant declining to guess rather than as a censored blank.
const DefaultPlaceholder = "〔I don't have a verified figure for this — I'd need to look it up before quoting one〕"

var (
	// A currency amount: "$1,250", "€ 99.50", "USD 1,250", "1,250 USD".
	reMoney = regexp.MustCompile(`(?i)(?:[$€£¥₹]|\b(?:usd|eur|gbp|jpy|cny|aud|cad|inr|chf|hkd|sgd|krw)\b)\s?\d[\d,]*(?:\.\d+)?|\b\d[\d,]*(?:\.\d+)?\s?(?:usd|eur|gbp|jpy|cny|aud|cad|inr|chf|hkd|sgd|krw)\b`)
	// A percentage: "4%", "12.5 %".
	rePercent = regexp.MustCompile(`\b\d{1,3}(?:\.\d+)?\s?%`)
)

// DefaultSignatures returns the generic detector set: currency amounts and
// percentages. Applications add domain signatures (durations, rates,
// identifiers) on top.
func DefaultSignatures() []Signature {
	return []Signature{
		{Kind: "money", Pattern: reMoney},
		{Kind: "percent", Pattern: rePercent},
	}
}

// NewGuard builds a Guard with DefaultSignatures grounded by the given
// tool names.
func NewGuard(groundingTools ...string) *Guard {
	g := &Guard{GroundingTools: map[string]bool{}, Signatures: DefaultSignatures()}
	for _, n := range groundingTools {
		g.GroundingTools[n] = true
	}
	return g
}

func (g *Guard) placeholderFor(s Signature) string {
	if s.Placeholder != "" {
		return s.Placeholder
	}
	if g.Placeholder != "" {
		return g.Placeholder
	}
	return DefaultPlaceholder
}

func (g *Guard) minDigits() int {
	if g.MinRestatementDigits <= 0 {
		return 2
	}
	return g.MinRestatementDigits
}

func (g *Guard) logger() *slog.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return slog.Default()
}

// ResultIsSuccess reports whether a serialised tool result vouches for
// anything: a JSON object carrying a top-level error_code or soft_failure
// (directly or inside an envelope's model_observation) does not. Empty or
// non-JSON output is treated as success — the caller only knows the tool
// ran, which is the pre-outcome-aware behaviour.
func ResultIsSuccess(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '{' {
		return true
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		return true
	}
	if hasFailureKey(top) {
		return false
	}
	if obs, ok := top["model_observation"]; ok && len(obs) > 0 && obs[0] == '{' {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(obs, &inner); err == nil && hasFailureKey(inner) {
			return false
		}
	}
	return true
}

func hasFailureKey(m map[string]json.RawMessage) bool {
	if v, ok := m["error_code"]; ok && len(v) > 0 && string(v) != "null" && string(v) != `""` {
		return true
	}
	if v, ok := m["soft_failure"]; ok && len(v) > 0 && string(v) != "null" {
		return true
	}
	return false
}

// kind returns the first signature kind matched in text, or "".
func (g *Guard) kind(text string) string {
	for _, s := range g.Signatures {
		if s.Pattern != nil && s.Pattern.MatchString(text) {
			return s.Kind
		}
	}
	return ""
}

// grounded reports whether calls contain a successful grounding tool for
// kind. A call with empty Output is treated as successful (outcome unknown).
func (g *Guard) grounded(kind string, calls []ToolTrace) bool {
	set := g.GroundingTools
	if kg, ok := g.KindGrounding[kind]; ok {
		set = kg
	}
	for _, c := range calls {
		if set[c.Name] && ResultIsSuccess(c.Output) {
			return true
		}
	}
	return false
}

// Flag is the guard's decision: given the assistant text for a turn and the
// tool calls made this turn (with their outputs when known), it reports
// whether the text states a regulated figure with no successful grounding
// call behind it, and which signature kind fired first.
func (g *Guard) Flag(text string, calls []ToolTrace) (flagged bool, kind string) {
	if g == nil {
		return false, ""
	}
	kind = g.kind(text)
	if kind == "" {
		return false, ""
	}
	if g.grounded(kind, calls) {
		return false, ""
	}
	return true, kind
}

// Redaction records one rewritten span.
type Redaction struct {
	Kind string
	Span string
}

var numTokenRe = regexp.MustCompile(`\d[\d,]*`)

func normNum(s string) string { return strings.ReplaceAll(s, ",", "") }

// numbers extracts the comma-normalised numeric tokens of >= minDigits
// digits from s.
func numbers(s string, minDigits int) []string {
	var out []string
	for _, m := range numTokenRe.FindAllString(s, -1) {
		if n := normNum(m); len(n) >= minDigits {
			out = append(out, n)
		}
	}
	return out
}

// spanIsRestatement reports whether every qualifying number in the span
// already appeared in a tool result or input. A span with no qualifying
// number is never a restatement; it must be grounded on its own turn.
func (g *Guard) spanIsRestatement(span string, grounded map[string]bool) bool {
	nums := numbers(span, g.minDigits())
	if len(nums) == 0 {
		return false
	}
	for _, n := range nums {
		if !grounded[n] {
			return false
		}
	}
	return true
}

// CollectGroundedNumbers harvests every numeric token of >= minDigits digits
// from the tool results and tool inputs in turns — the numbers engineering
// vouched for, which the model may restate on a later no-tool turn.
func CollectGroundedNumbers(turns []Turn, minDigits int) map[string]bool {
	set := map[string]bool{}
	for _, t := range turns {
		harvestTurn(t, set, minDigits)
	}
	return set
}

func harvestTurn(t Turn, set map[string]bool, minDigits int) {
	for _, b := range t.Blocks {
		switch b.Type {
		case BlockToolResult:
			for _, n := range numbers(string(b.ToolResult), minDigits) {
				set[n] = true
			}
		case BlockToolUse:
			for _, n := range numbers(string(b.ToolInput), minDigits) {
				set[n] = true
			}
		}
	}
}

// Redact replaces every signature span in text with its placeholder, except
// spans that only restate a grounded number. It returns the rewritten text
// and the redactions made. Callers invoke it only on a turn Flag already
// flagged, so any non-restatement span is a number the model invented.
func (g *Guard) Redact(text string, grounded map[string]bool) (string, []Redaction) {
	var reds []Redaction
	for _, s := range g.Signatures {
		if s.Pattern == nil {
			continue
		}
		ph := g.placeholderFor(s)
		text = s.Pattern.ReplaceAllStringFunc(text, func(m string) string {
			if g.spanIsRestatement(m, grounded) {
				return m
			}
			reds = append(reds, Redaction{Kind: s.Kind, Span: m})
			return ph
		})
	}
	return text, reds
}

func assistantText(blocks []Block) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func toolUseTraces(blocks []Block) []ToolTrace {
	var out []ToolTrace
	for _, b := range blocks {
		if b.Type == BlockToolUse && b.ToolName != "" {
			out = append(out, ToolTrace{Name: b.ToolName, Input: b.ToolInput})
		}
	}
	return out
}

func toolResultTraces(blocks []Block) []ToolTrace {
	var out []ToolTrace
	for _, b := range blocks {
		if b.Type == BlockToolResult && b.ToolName != "" {
			out = append(out, ToolTrace{Name: b.ToolName, Output: string(b.ToolResult)})
		}
	}
	return out
}

// redactTurnTextBlocks rewrites each text block of an assistant turn in
// place so the PERSISTED block IR carries the same redaction the live stream
// shows — otherwise a page refresh would re-hydrate the un-redacted figure.
func (g *Guard) redactTurnTextBlocks(turn *Turn, grounded map[string]bool) []Redaction {
	var all []Redaction
	for i := range turn.Blocks {
		if turn.Blocks[i].Type != BlockText {
			continue
		}
		red, reds := g.Redact(turn.Blocks[i].Text, grounded)
		turn.Blocks[i].Text = red
		all = append(all, reds...)
	}
	return all
}

// GuardReply applies the guard to a non-streaming ChatOutput: numbers a
// tool produced earlier in history (the block-IR conversation fed as
// ChatInput.Turns) are seeded as grounded, then any un-grounded figure in
// out.Reply and out.Turns is redacted in place. Returns the redactions.
//
// Exported so an eval harness can compare a raw provider reply with the
// guarded reply from ONE upstream call — a paired before/after measurement.
func (g *Guard) GuardReply(history []Turn, out *ChatOutput) []Redaction {
	if g == nil || out == nil {
		return nil
	}
	grounded := CollectGroundedNumbers(history, g.minDigits())
	var calls []ToolTrace
	for _, tc := range out.ToolCalls {
		if tc.Name != "" {
			calls = append(calls, tc)
		}
		for _, n := range numbers(tc.Output, g.minDigits()) {
			grounded[n] = true
		}
		for _, n := range numbers(string(tc.Input), g.minDigits()) {
			grounded[n] = true
		}
	}
	flagged, kind := g.Flag(out.Reply, calls)
	if !flagged {
		return nil
	}
	red, reds := g.Redact(out.Reply, grounded)
	if len(reds) == 0 {
		return nil
	}
	out.Reply = red
	for i := range out.Turns {
		if out.Turns[i].Role == RoleAssistant {
			g.redactTurnTextBlocks(&out.Turns[i], grounded)
		}
	}
	g.logger().Warn("honesty guard redacted un-grounded figure", "kind", kind, "spans", len(reds))
	return reds
}

// WrapEmit wraps an EmitFn with the guard. It swallows raw text deltas and
// re-emits each assistant segment's text at that segment's turn boundary —
// where the full text and the tools used are both known — redacting any
// un-grounded figure first. The redaction is applied to BOTH the live text
// event and the persisted turn snapshot, keeping the streamed and
// re-hydrated views identical. Token-level streaming is traded for the
// guarantee; install a Guard only on surfaces where that trade is right.
func (g *Guard) WrapEmit(history []Turn, emit EmitFn) EmitFn {
	return g.wrapEmit(history, emit, nil)
}

// wrapEmit is WrapEmit with a hook that sees each turn's redactions; the
// Agent uses it to put a guard.redacted event on the run span.
func (g *Guard) wrapEmit(history []Turn, emit EmitFn, onRedact func([]Redaction)) EmitFn {
	if g == nil {
		return emit
	}
	grounded := CollectGroundedNumbers(history, g.minDigits())
	var callsSoFar []ToolTrace
	return func(ev StreamEvent) {
		switch ev.Type {
		case EventText:
			// Held: the assistant turn event below carries the authoritative
			// text and is the point at which grounding can be judged.
			return
		case EventTurn:
			if ev.Turn == nil {
				emit(ev)
				return
			}
			if ev.Turn.Role != RoleAssistant {
				// A tool turn: record outcomes and harvest its numbers so a
				// later step in this same call may restate them.
				callsSoFar = append(callsSoFar, toolResultTraces(ev.Turn.Blocks)...)
				harvestTurn(*ev.Turn, grounded, g.minDigits())
				emit(ev)
				return
			}
			// Same-turn tool_use blocks count as grounding for this turn's
			// text (their outcome is not yet known), and their inputs join
			// the grounded set.
			harvestTurn(*ev.Turn, grounded, g.minDigits())
			calls := append(append([]ToolTrace{}, callsSoFar...), toolUseTraces(ev.Turn.Blocks)...)
			text := assistantText(ev.Turn.Blocks)
			if flagged, kind := g.Flag(text, calls); flagged {
				if reds := g.redactTurnTextBlocks(ev.Turn, grounded); len(reds) > 0 {
					g.logger().Warn("honesty guard redacted un-grounded figure", "kind", kind, "spans", len(reds))
					text = assistantText(ev.Turn.Blocks)
					if onRedact != nil {
						onRedact(reds)
					}
				}
			}
			if text != "" {
				emit(StreamEvent{Type: EventText, Delta: text})
			}
			emit(ev)
		default:
			emit(ev)
		}
	}
}
