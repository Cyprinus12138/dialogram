package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Cyprinus12138/dialogram/agent"
)

// LoadTranscript reads a recorded conversation stored as a JSON array of
// neutral Turn IR. Recorded transcripts are the Layer-2 corpus: the
// grounding checker runs over them in CI at zero cost.
func LoadTranscript(path string) ([]agent.Turn, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var turns []agent.Turn
	if err := json.Unmarshal(b, &turns); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return turns, nil
}

// LoadTranscripts loads every *.json transcript in dir, keyed by filename.
func LoadTranscripts(dir string) (map[string][]agent.Turn, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	out := make(map[string][]agent.Turn, len(matches))
	for _, m := range matches {
		turns, err := LoadTranscript(m)
		if err != nil {
			return nil, err
		}
		out[filepath.Base(m)] = turns
	}
	return out, nil
}
