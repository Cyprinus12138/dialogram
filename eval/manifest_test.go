package eval

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

func fixtureTool(name, desc string, schema map[string]any) *tool.Tool {
	return &tool.Tool{Name: name, Description: desc, Schema: schema,
		Fn: func(context.Context, json.RawMessage) (any, error) { return map[string]any{"ok": true}, nil }}
}

func threeToolRegistry() *tool.Registry {
	r := tool.NewRegistry()
	r.Register(fixtureTool("alpha", "first", map[string]any{"type": "object"}))
	r.Register(fixtureTool("bravo", "second", map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}))
	r.Register(fixtureTool("charlie", "third", map[string]any{"type": "object"}))
	return r
}

func TestBuildManifest_Deterministic(t *testing.T) {
	p := agent.Profile{Name: "assistant", SystemPrompt: "PROMPT"}
	a := BuildManifest(p, threeToolRegistry())
	b := BuildManifest(p, threeToolRegistry())
	if a.ManifestHash != b.ManifestHash || len(a.Tools) != 3 || a.Tools[0].Name != "alpha" || a.Tools[2].Name != "charlie" {
		t.Fatalf("manifest not deterministic/sorted: %+v", a)
	}
	if a.PromptHash != p.Fingerprint() {
		t.Fatal("manifest prompt hash must equal the profile fingerprint")
	}
	// The profile allow-list narrows the pinned tools.
	narrow := BuildManifest(agent.Profile{Name: "n", Tools: []string{"bravo"}}, threeToolRegistry())
	if len(narrow.Tools) != 1 || narrow.Tools[0].Name != "bravo" {
		t.Fatalf("allow-list not applied: %+v", narrow.Tools)
	}
}

func TestBuildManifest_PromptChangeMovesOnlyPromptHash(t *testing.T) {
	reg := threeToolRegistry()
	a := BuildManifest(agent.Profile{SystemPrompt: "PROMPT-A"}, reg)
	b := BuildManifest(agent.Profile{SystemPrompt: "PROMPT-B"}, reg)
	if a.PromptHash == b.PromptHash || a.ManifestHash == b.ManifestHash {
		t.Fatal("prompt change must move the prompt and manifest hashes")
	}
	for i := range a.Tools {
		if a.Tools[i].SpecHash != b.Tools[i].SpecHash {
			t.Fatalf("tool %s spec hash moved on a prompt-only change", a.Tools[i].Name)
		}
	}
}

func TestBuildManifest_ToolChangeMovesToolAndManifestHash(t *testing.T) {
	p := agent.Profile{SystemPrompt: "PROMPT"}
	base := BuildManifest(p, threeToolRegistry())
	changed := tool.NewRegistry()
	changed.Register(fixtureTool("alpha", "first", map[string]any{"type": "object"}))
	changed.Register(fixtureTool("bravo", "second", map[string]any{"type": "object", "required": []string{"x"}, "properties": map[string]any{"x": map[string]any{"type": "string"}}}))
	changed.Register(fixtureTool("charlie", "third", map[string]any{"type": "object"}))
	got := BuildManifest(p, changed)
	if got.PromptHash != base.PromptHash || got.ManifestHash == base.ManifestHash {
		t.Fatal("tool change must move only the manifest hash, not the prompt hash")
	}
	byName := map[string]string{}
	for _, tv := range base.Tools {
		byName[tv.Name] = tv.SpecHash
	}
	for _, tv := range got.Tools {
		if (tv.Name == "bravo") == (tv.SpecHash == byName[tv.Name]) {
			t.Fatalf("%s spec hash moved incorrectly", tv.Name)
		}
	}
}

// builtinRegistry is the set of tools dialogram ships: their contracts are
// what the model sees, so a change to any of them is a visible event.
func builtinRegistry() *tool.Registry {
	r := tool.NewRegistry()
	tool.RegisterHITL(r)
	tool.RegisterPresentData(r)
	tool.RegisterRenderFromRef(r)
	tool.RegisterNow(r, time.UTC)
	lib, _ := agent.NewSkillLibrary(&agent.Skill{ID: "example", Name: "Example", Description: "placeholder"})
	agent.RegisterLoadSkill(r, lib)
	return r
}

const builtinManifestGolden = "testdata/manifest_builtin_tools.golden.json"

// TestManifest_GoldenBuiltinTools pins the content hash of every built-in
// tool contract. A change to a name / description / input_schema fails CI
// with the exact diff. Regenerate intentionally after a reviewed change:
//
//	UPDATE_GOLDEN=1 go test ./eval/ -run TestManifest_GoldenBuiltinTools
func TestManifest_GoldenBuiltinTools(t *testing.T) {
	m := BuildManifest(agent.Profile{Name: "builtin"}, builtinRegistry())
	got, _ := json.MarshalIndent(m.Tools, "", "  ")
	got = append(got, '\n')
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(builtinManifestGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s (%d tools)", builtinManifestGolden, len(m.Tools))
		return
	}
	want, err := os.ReadFile(builtinManifestGolden)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("built-in tool manifest changed — a tool contract (name/description/input_schema) moved.\nIf intentional, re-pin with UPDATE_GOLDEN=1 go test ./eval/ -run %s", t.Name())
	}
}
