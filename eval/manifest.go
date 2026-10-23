package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

// ToolVersion pins one tool's contract by a content hash of its wire spec
// (name + description + input schema). The version is derived from the
// contract itself so it can never silently drift the way a hand-maintained
// version string rots: a schema or description edit changes the hash, which
// is the signal the harness uses to attribute a score change to a tool
// change rather than a prompt change.
type ToolVersion struct {
	Name     string `json:"name"`
	SpecHash string `json:"spec_hash"`
}

// Manifest is the pinned fingerprint of everything that can move a scenario
// score deterministically: the system prompt and the tool contracts.
type Manifest struct {
	Profile      string        `json:"profile"`
	PromptHash   string        `json:"prompt_hash"`
	Tools        []ToolVersion `json:"tools"`
	ManifestHash string        `json:"manifest_hash"`
}

// BuildManifest computes the pinned manifest for a profile against the live
// registry. Tools are filtered to the profile's allow-list and sorted by
// name so the manifest is order-independent and reproducible. PromptHash
// equals profile.Fingerprint().
func BuildManifest(profile agent.Profile, reg *tool.Registry) Manifest {
	m := Manifest{Profile: profile.Name, PromptHash: profile.Fingerprint()}
	for _, spec := range reg.AnthropicSpecFor(profile.Tools) {
		name, _ := spec["name"].(string)
		m.Tools = append(m.Tools, ToolVersion{Name: name, SpecHash: hashSpec(spec)})
	}
	sort.Slice(m.Tools, func(i, j int) bool { return m.Tools[i].Name < m.Tools[j].Name })
	m.ManifestHash = m.computeManifestHash()
	return m
}

func (m Manifest) computeManifestHash() string {
	h := sha256.New()
	h.Write([]byte(m.PromptHash))
	for _, t := range m.Tools {
		h.Write([]byte{0})
		h.Write([]byte(t.Name))
		h.Write([]byte{0})
		h.Write([]byte(t.SpecHash))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashSpec canonicalises a tool spec to bytes and hashes it. json.Marshal
// of a map emits keys in sorted order, so the encoding is deterministic.
func hashSpec(spec map[string]any) string {
	b, err := json.Marshal(spec)
	if err != nil {
		return hashString("unmarshalable:" + err.Error())
	}
	return hashString(string(b))
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
