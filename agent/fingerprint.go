package agent

import (
	"crypto/sha256"
	"encoding/hex"
)

// PromptFingerprint returns a stable content hash (hex SHA-256) of a system
// prompt. Pin it in a golden test: an intentional prompt edit then fails the
// test until the golden is re-baselined — a conscious act that flags the
// nightly scenario scores for a fresh look.
func PromptFingerprint(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}
