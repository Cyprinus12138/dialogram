package eval

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

// GroundingViolationKind classifies a no-fabrication failure.
type GroundingViolationKind string

const (
	// ViolationFigure — a figure in the reply (currency amount,
	// comma-grouped number, sample size) does not appear in any tool_result
	// of the same turn.
	ViolationFigure GroundingViolationKind = "figure"
	// ViolationEntity — an uppercase entity token in the reply (a product,
	// place or carrier name the data layer spells in capitals) does not
	// appear in any tool_result of the same turn.
	ViolationEntity GroundingViolationKind = "entity"
)

// GroundingViolation is one ungrounded token found in a reply.
type GroundingViolation struct {
	Kind  GroundingViolationKind `json:"kind"`
	Value string                 `json:"value"` // token as it appeared
	Norm  string                 `json:"norm"`  // normalised form used for the lookup
}

// GroundingReport is the result of a no-fabrication check over one reply.
//
// The check is judge-free and deterministic. It is enabled by a prompt
// contract: every figure or named entity in a reply must appear verbatim
// in a tool_result of the same turn. The two token classes are extracted
// from the reply and asserted present in the concatenated tool_result text.
type GroundingReport struct {
	Figures    []string             `json:"figures"`
	Entities   []string             `json:"entities"`
	Violations []GroundingViolation `json:"violations"`
}

// OK reports whether the reply is fully grounded.
func (r GroundingReport) OK() bool { return len(r.Violations) == 0 }

// GroundingOptions tune the checker.
type GroundingOptions struct {
	// Stopwords are uppercase tokens that are never entity candidates
	// (acronyms, units, the product's own name). Merged with
	// DefaultStopwords.
	Stopwords []string
	// EntityCheck disables the entity class entirely when false.
	NoEntityCheck bool
}

// DefaultStopwords are uppercase tokens common in ordinary prose that must
// not be mistaken for entity names.
var DefaultStopwords = []string{
	"API", "LLM", "JSON", "HTTP", "HTTPS", "URL", "USD", "EUR", "GBP", "JPY", "CNY",
	"UTC", "GMT", "AM", "PM", "OK", "ID", "FAQ", "CEO", "CTO", "VIP", "ETA", "ASAP",
	"TL", "DR", "TLDR", "PDF", "CSV", "SQL", "AI", "IMO", "FYI",
}

var (
	figureMoneyRe  = regexp.MustCompile(`(?:[$€£¥₹]|\b(?:USD|EUR|GBP|JPY|CNY|AUD|CAD)\s?)\s?\d[\d,]*(?:\.\d+)?`)
	figureGroupRe  = regexp.MustCompile(`\d{1,3}(?:,\d{3})+(?:\.\d+)?`)
	figureSampleRe = regexp.MustCompile(`(?i)\bn\s*=\s*\d[\d,]*`)
	// rangeTailRe captures the second endpoint of a numeric range so both
	// halves of "$2,700–2,800" get grounded.
	rangeTailRe = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?\s*[–—-]\s*(\d[\d,]*(?:\.\d+)?)`)
	// entityRe matches runs of UPPERCASE words optionally joined by "@", "/",
	// "&" and digits — the canonical all-caps form many data sources use for
	// names. Title-case prose is deliberately not flagged.
	entityRe = regexp.MustCompile(`[A-Z][A-Z0-9]{2,}(?:\s*[@/&]?\s*[A-Z0-9][A-Z0-9]*)*`)
	// noiseTokenRe matches structural uppercase tokens: quarters, bare
	// numbers, short codes like D07.
	noiseTokenRe = regexp.MustCompile(`^(?:[A-Z]\d{1,2}|Q[1-4]|\d+)$`)
)

// CheckGrounding runs the no-fabrication check with default options.
func CheckGrounding(reply string, toolResults []string) GroundingReport {
	return CheckGroundingWith(reply, toolResults, GroundingOptions{})
}

// CheckGroundingWith runs the no-fabrication check: every figure and entity
// token in reply must appear in some entry of toolResults.
//
// toolResults are the raw tool_result bodies (envelopes or bare JSON) the
// model received in the SAME turn as the reply. Both sides are normalised
// the same way (strip currency symbols, commas and whitespace; lowercase)
// before the substring lookup, so "$2,700" in the reply matches "2700" in
// the data. Only model_observation is considered — a figure that exists
// solely in ui_payload or runtime_state never grounds a claim.
//
// Heuristic caveat: the figure haystack concatenates every digit run within
// a tool_result; a fabricated figure that spans the boundary between two
// adjacent grounded runs can substring-match. The checker is a screen, not a
// proof.
func CheckGroundingWith(reply string, toolResults []string, opts GroundingOptions) GroundingReport {
	var rep GroundingReport
	if strings.TrimSpace(reply) == "" {
		return rep
	}
	stop := map[string]bool{}
	for _, w := range DefaultStopwords {
		stop[w] = true
	}
	for _, w := range opts.Stopwords {
		stop[strings.ToUpper(w)] = true
	}

	var figureHay, textHay strings.Builder
	for _, tr := range toolResults {
		obs := string(tool.ProjectModelObservation(json.RawMessage(tr)))
		figureHay.WriteString(normalizeDigits(obs))
		figureHay.WriteString(" ")
		textHay.WriteString(normalizeText(obs))
		textHay.WriteString(" ")
	}
	figHay, txtHay := figureHay.String(), textHay.String()

	for _, tok := range extractFigures(reply) {
		norm := normalizeDigits(tok)
		rep.Figures = append(rep.Figures, tok)
		if norm == "" {
			continue
		}
		if !strings.Contains(figHay, norm) {
			rep.Violations = append(rep.Violations, GroundingViolation{Kind: ViolationFigure, Value: tok, Norm: norm})
		}
	}
	if !opts.NoEntityCheck {
		for _, tok := range extractEntityTokens(reply, stop) {
			norm := normalizeText(tok)
			rep.Entities = append(rep.Entities, tok)
			if norm == "" {
				continue
			}
			if !strings.Contains(txtHay, norm) {
				rep.Violations = append(rep.Violations, GroundingViolation{Kind: ViolationEntity, Value: tok, Norm: norm})
			}
		}
	}
	return rep
}

// CheckTranscript runs CheckGrounding over a completed conversation in the
// neutral Turn IR. "This turn" is everything since the last user turn: the
// final assistant text plus every tool_result produced while answering it.
func CheckTranscript(turns []agent.Turn) GroundingReport {
	reply, toolResults := LastTurnReplyAndResults(turns)
	return CheckGrounding(reply, toolResults)
}

// LastTurnReplyAndResults extracts the final assistant prose and the
// tool_result bodies emitted after the last user turn.
func LastTurnReplyAndResults(turns []agent.Turn) (reply string, toolResults []string) {
	start := 0
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == agent.RoleUser {
			start = i + 1
			break
		}
	}
	var b strings.Builder
	for _, t := range turns[start:] {
		switch t.Role {
		case agent.RoleAssistant:
			for _, blk := range t.Blocks {
				if blk.Type == agent.BlockText {
					b.WriteString(blk.Text)
					b.WriteString("\n")
				}
			}
		case agent.RoleTool:
			for _, blk := range t.Blocks {
				if blk.Type == agent.BlockToolResult && len(blk.ToolResult) > 0 {
					toolResults = append(toolResults, string(blk.ToolResult))
				}
			}
		}
	}
	return b.String(), toolResults
}

func extractFigures(reply string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(tok string) {
		tok = strings.TrimSpace(tok)
		if tok == "" || seen[tok] {
			return
		}
		seen[tok] = true
		out = append(out, tok)
	}
	for _, m := range figureMoneyRe.FindAllString(reply, -1) {
		add(m)
	}
	for _, m := range figureGroupRe.FindAllString(reply, -1) {
		add(m)
	}
	for _, m := range figureSampleRe.FindAllString(reply, -1) {
		add(m)
	}
	for _, m := range rangeTailRe.FindAllStringSubmatch(reply, -1) {
		add(m[1])
	}
	return out
}

func extractEntityTokens(reply string, stop map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range entityRe.FindAllString(reply, -1) {
		words := strings.FieldsFunc(strings.TrimSpace(m), func(r rune) bool {
			return r == ' ' || r == '\t' || r == '/' || r == '@' || r == '&'
		})
		var kept []string
		for _, w := range words {
			if stop[w] || noiseTokenRe.MatchString(w) {
				continue
			}
			kept = append(kept, w)
		}
		if len(kept) == 0 {
			continue
		}
		hasWordy := false
		for _, w := range kept {
			if len(w) >= 3 {
				hasWordy = true
				break
			}
		}
		if !hasWordy || (len(kept) == 1 && len(kept[0]) < 4) {
			continue
		}
		name := strings.Join(kept, " ")
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func normalizeDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || r == '.' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func normalizeText(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevSpace = false
		default:
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}
