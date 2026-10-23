package agent

import (
	"crypto/rand"
	"encoding/hex"
)

// ToolUseIDPrefix is the prefix of every neutral tool_use id this runtime
// mints.
const ToolUseIDPrefix = "dg_"

// NewToolUseID returns a provider-neutral tool_use identifier: the prefix
// followed by 24 random hex chars (96 bits of entropy).
//
// Why mint our own instead of passing the provider's native id through
// ("toolu_…" from Anthropic, "call_…" from OpenAI):
//
//   - Within a single provider request, the id is just an opaque string
//     used to match tool_use blocks to their tool_result replies. Providers
//     do not recognise ids across requests, so any internally consistent id
//     works.
//   - Across requests, minting our own id means the persisted state carries
//     no provider fingerprint. A conversation started on OpenAI can be
//     resumed on Anthropic (and vice versa) without an id-format mismatch
//     masquerading as a protocol error.
//   - If a provider changes its id format, stored history does not have to
//     be rewritten.
func NewToolUseID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return ToolUseIDPrefix + hex.EncodeToString(b[:])
}
